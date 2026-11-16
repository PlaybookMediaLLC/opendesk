package collab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/email"
	"github.com/useopendesk/opendesk/internal/httpx"
)

// Sending (contracts/collab.md "POST /v1/collab/messages").
const (
	ModeNew      = "new"
	ModeReply    = "reply"
	ModeReplyAll = "reply_all"
	ModeForward  = "forward"
)

// sendPerMinute bounds the messages one person sends per minute.
const sendPerMinute = 60

// maxSendHTML bounds a sent body.
const maxSendHTML = 2 << 20

type sendInput struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" doc:"The same key from the same person within a day answers the message it already sent instead of sending it again"`
	Body           struct {
		AccountID          uuid.UUID   `json:"account_id"`
		Mode               string      `json:"mode" enum:"new,reply,reply_all,forward"`
		MessageID          *uuid.UUID  `json:"message_id,omitempty" doc:"The message replied to or forwarded; required unless new"`
		To                 []string    `json:"to" maxItems:"100"`
		Cc                 []string    `json:"cc,omitempty" maxItems:"100"`
		Bcc                []string    `json:"bcc,omitempty" maxItems:"100"`
		Subject            string      `json:"subject" maxLength:"998"`
		BodyHTML           string      `json:"body_html" maxLength:"2097152"`
		AttachmentIDs      []uuid.UUID `json:"attachment_ids,omitempty" maxItems:"50" doc:"POST /v1/attachments uploads of the caller"`
		ForwardAttachments bool        `json:"forward_attachments,omitempty" doc:"Forward: attach the original's files"`
		FromEmail          string      `json:"from_email,omitempty" maxLength:"254" doc:"One of the account's usable addresses; left out: a reply's goes from the address it answers, new mail from the default"`
	}
}

func errSend(detail string) error { return httpx.E(502, "send_failed", detail) }

// sendKeyHold is how long a send that did not store its message holds its idempotency key.
const sendKeyHold = 10 * time.Minute

// errSendPending answers a send whose idempotency key is held by a send not yet stored: it
// may be at the provider already, so it is not sent again.
var errSendPending = httpx.E(409, "send_in_progress", "This message is being sent or may have been sent already. Check Sent before sending it again.")

func (s *service) registerSendRoutes(api huma.API) {
	collabOp(api, huma.Operation{OperationID: "send-collab-message", Method: http.MethodPost, Path: "/v1/collab/messages",
		Summary: "Send mail from an account"},
		func(ctx context.Context, in *sendInput) (*struct{ Body MailMessage }, error) {
			p, err := s.require(ctx, auth.RoleAgent)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			if !s.d.Limiter.Allow("collab-send:"+p.UserID.String(), sendPerMinute, time.Minute) {
				return nil, httpx.RateLimited()
			}
			out, err := s.send(ctx, p, in)
			if err != nil {
				return nil, err
			}
			return &struct{ Body MailMessage }{out}, nil
		})
}

func (s *service) send(ctx context.Context, p *auth.Principal, in *sendInput) (MailMessage, error) {
	b := in.Body
	t, err := s.composeTarget(ctx, p, b.AccountID, b.Mode, b.MessageID)
	if err != nil {
		return MailMessage{}, err
	}
	out, err := s.compose(ctx, p, t, composeIn{Mode: b.Mode, To: b.To, Cc: b.Cc, Bcc: b.Bcc, Subject: b.Subject, BodyHTML: b.BodyHTML,
		AttachmentIDs: b.AttachmentIDs, ForwardAttachments: b.ForwardAttachments, FromEmail: b.FromEmail, NeedRecipients: true})
	if err != nil {
		return MailMessage{}, err
	}
	a := t.a

	// An idempotency key holds the send from here: a retry with the same key answers the
	// stored message, or waits for it, and is never sent twice. A send the provider refused
	// gives its key back.
	key := strings.TrimSpace(in.IdempotencyKey)
	release := func() {}
	if key != "" {
		done, err := s.claimSendKey(ctx, p, a, key)
		if err != nil || done != nil {
			if done != nil {
				return s.messageByID(ctx, p, a, *done)
			}
			return MailMessage{}, err
		}
		release = func() {
			_, _ = s.d.DB.Pool.Exec(context.WithoutCancel(ctx), `DELETE FROM collab_send_keys WHERE workspace_id = $1 AND user_id = $2 AND key = $3
				AND message_id IS NULL`, p.WorkspaceID, p.UserID, key)
		}
	}

	// Sent through the provider within the request: nothing is stored when it refuses.
	prov, err := s.provider(ctx, a)
	if err != nil {
		release()
		return MailMessage{}, errSend(safeError(err))
	}
	defer prov.Close()
	info := SendInfo{From: out.From, Recipients: out.Recipients}
	var threadID *uuid.UUID
	if t.thread != nil && b.Mode != ModeForward {
		info.ThreadRemoteID = t.thread.RemoteID
		threadID = &t.thread.ID
	}
	sctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	ref, err := prov.Send(sctx, out.Raw, info)
	if err != nil {
		s.log.Warn("collab send refused", "account_id", a.ID, "err", err)
		var pe *providerError
		if isPermanent(err) || errors.As(err, &pe) && pe.limited {
			release() // refused (for good, or by a rate limit before it ran): nothing went out
		}
		return MailMessage{}, errSend(safeError(err))
	}
	mid, err := s.storeSent(ctx, a, out.Raw, ref, p.UserID, threadID)
	if err != nil {
		return MailMessage{}, httpx.Wrap(err)
	}
	if key != "" {
		if _, err := s.d.DB.Pool.Exec(context.WithoutCancel(ctx), `UPDATE collab_send_keys SET message_id = $4 WHERE workspace_id = $1 AND user_id = $2 AND key = $3`,
			p.WorkspaceID, p.UserID, key, mid); err != nil {
			s.log.Warn("collab send key not recorded", "account_id", a.ID, "err", err)
		}
	}
	return s.messageByID(ctx, p, a, mid)
}

// claimSendKey takes a send's idempotency key for the caller. It answers the stored message
// when the key already sent one from this account, errSendPending when its send is not
// stored yet (or the key was used for another account), else nil, nil: the key is the
// caller's to send with.
func (s *service) claimSendKey(ctx context.Context, p *auth.Principal, a *accountRow, key string) (*uuid.UUID, error) {
	pool := s.d.DB.Pool
	tag, err := pool.Exec(ctx, `INSERT INTO collab_send_keys (workspace_id, user_id, key, account_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, p.WorkspaceID, p.UserID, key, a.ID)
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	// A send that ended without storing its message long ago gives its key up: whoever
	// retries has been told to check Sent for sendKeyHold.
	tag, err = pool.Exec(ctx, `UPDATE collab_send_keys SET account_id = $4, created_at = now() WHERE workspace_id = $1 AND user_id = $2 AND key = $3
		AND message_id IS NULL AND created_at < now() - make_interval(secs => $5)`, p.WorkspaceID, p.UserID, key, a.ID, sendKeyHold.Seconds())
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var account uuid.UUID
	var mid *uuid.UUID
	err = pool.QueryRow(ctx, `SELECT account_id, message_id FROM collab_send_keys WHERE workspace_id = $1 AND user_id = $2 AND key = $3`,
		p.WorkspaceID, p.UserID, key).Scan(&account, &mid)
	if err != nil && !db.IsNotFound(err) {
		return nil, httpx.Wrap(err)
	}
	if err == nil && mid != nil && account == a.ID {
		return mid, nil
	}
	return nil, errSendPending
}

// messageByID loads one stored message of the account as the API shows it.
func (s *service) messageByID(ctx context.Context, p *auth.Principal, a *accountRow, id uuid.UUID) (MailMessage, error) {
	pool := s.d.DB.Pool
	m, err := scanMessage(pool.QueryRow(ctx, `SELECT `+messageColumns+` FROM collab_messages m WHERE m.workspace_id = $1 AND m.id = $2`,
		p.WorkspaceID, id))
	if err != nil {
		return MailMessage{}, httpx.Wrap(err)
	}
	list, err := s.messagesOut(ctx, pool, a, []*messageRow{m})
	if err != nil {
		return MailMessage{}, httpx.Wrap(err)
	}
	return list[0], nil
}

// uploadedFiles reads the caller's uploads. Inline images the HTML shows by their upload
// URL are sent as cid: parts so they keep showing in the recipient's mail client.
func (s *service) uploadedFiles(ctx context.Context, wid uuid.UUID, attIDs []uuid.UUID, body string) ([]email.OutAttachment, string, error) {
	if len(attIDs) == 0 {
		return nil, body, nil
	}
	rows, err := s.d.DB.Pool.Query(ctx, `SELECT id, filename, content_type, storage_key, size_bytes, inline FROM attachments
		WHERE workspace_id = $1 AND id = ANY($2) ORDER BY created_at, id`, wid, attIDs)
	if err != nil {
		return nil, "", httpx.Wrap(err)
	}
	type upload struct {
		id       uuid.UUID
		filename string
		ct, key  string
		size     int64
		inline   bool
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (upload, error) {
		var x upload
		return x, r.Scan(&x.id, &x.filename, &x.ct, &x.key, &x.size, &x.inline)
	})
	if err != nil {
		return nil, "", httpx.Wrap(err)
	}
	var total int64
	for _, u := range list {
		total += u.size
	}
	if total > maxSendBytes {
		return nil, "", httpx.E(422, "attachments_too_large", "Attach at most 25 MB of files.")
	}
	referenced := map[uuid.UUID]string{}
	body = rewriteImages(body, func(src string) string {
		for _, u := range list {
			if strings.Contains(src, "/v1/attachments/"+u.id.String()) {
				referenced[u.id] = u.id.String() + "@opendesk"
				return referenced[u.id]
			}
		}
		return ""
	})
	out := make([]email.OutAttachment, 0, len(list))
	for _, u := range list {
		rc, err := s.d.Storage.Get(ctx, u.key)
		if err != nil {
			return nil, "", httpx.Wrap(err)
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxSendBytes+1))
		rc.Close()
		if err != nil {
			return nil, "", httpx.Wrap(err)
		}
		out = append(out, email.OutAttachment{Filename: u.filename, ContentType: u.ct, Data: data, ContentID: referenced[u.id]})
	}
	return out, body, nil
}

// collabImageLink is a signed link to a stored message's file (attachmentURL).
var collabImageLink = regexp.MustCompile(`/v1/collab/attachments/([0-9a-f-]{36})/(\d{1,4})\?`)

// rewriteImages points each <img> whose src cidFor names at "cid:<content id>", for
// files sent with the message.
func rewriteImages(body string, cidFor func(src string) string) string {
	doc := email.ParseHTML(body)
	if doc == nil {
		return body
	}
	changed := false
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.DataAtom == atom.Img {
			for i, at := range n.Attr {
				if strings.EqualFold(at.Key, "src") {
					if cid := cidFor(at.Val); cid != "" {
						n.Attr[i].Val, changed = "cid:"+cid, true
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if !changed {
		return body
	}
	return email.RenderHTML(doc)
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// clipRunes shortens s to n runes.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
