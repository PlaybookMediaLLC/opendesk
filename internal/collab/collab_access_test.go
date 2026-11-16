package collab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/useopendesk/opendesk/internal/email"
	"github.com/useopendesk/opendesk/internal/httpx"
	"github.com/useopendesk/opendesk/internal/ids"
)

// TestAPIPrivateConversationOwnerOnly: someone who sees one conversation of a private account
// reads it and comments; only the account's owner acts on it, replies from it, shares it,
// assigns someone new to it or mentions someone who does not see it. A shared account's
// conversations keep their rules.
func TestAPIPrivateConversationOwnerOnly(t *testing.T) {
	w := newWorld(t)
	fake := &fakeProvider{}
	w.useFake(fake)
	a := w.account(w.agent, ProviderGoogle, VisibilityPrivate, Audience{})
	tid, mid := w.seedThread(a, "Offer letter", w.folder(a, RoleInbox))
	adam, bea := w.api(w.agent), w.api(w.agent2)
	if st := doJSON(t, "PUT", adam.URL+"/v1/collab/threads/"+tid.String()+"/shares",
		map[string]any{"shares": Audience{UserIDs: []uuid.UUID{w.agent2.UserID}}}, nil); st != 200 {
		t.Fatalf("owner shares: %d", st)
	}
	var d MailThreadDetail
	if st := doJSON(t, "GET", bea.URL+"/v1/collab/threads/"+tid.String(), nil, &d); st != 200 || !d.CanAct || d.CanUseMailbox ||
		d.Account.OwnerID == nil || *d.Account.OwnerID != w.agent.UserID {
		t.Fatalf("Bea's detail: %d can_act %v can_use_mailbox %v owner %v", st, d.CanAct, d.CanUseMailbox, d.Account.OwnerID)
	}
	if st := doJSON(t, "GET", adam.URL+"/v1/collab/threads/"+tid.String(), nil, &d); st != 200 || !d.CanUseMailbox {
		t.Fatalf("Adam's detail: %d %v", st, d.CanUseMailbox)
	}
	refused := func(what string, st int, e apiError) {
		t.Helper()
		if st != 403 || e.Code != "owner_only" {
			t.Fatalf("%s by Bea: %d %+v", what, st, e)
		}
	}
	var e apiError
	for _, action := range []string{ActionArchive, ActionRead, ActionTrash} {
		e = apiError{}
		refused(action, doJSON(t, "POST", bea.URL+"/v1/collab/threads/actions", map[string]any{"thread_ids": []uuid.UUID{tid}, "action": action}, &e), e)
	}
	e = apiError{}
	refused("share", doJSON(t, "PUT", bea.URL+"/v1/collab/threads/"+tid.String()+"/shares",
		map[string]any{"shares": Audience{Everyone: true}}, &e), e)
	e = apiError{}
	refused("assign someone new", doJSON(t, "PUT", bea.URL+"/v1/collab/threads/"+tid.String()+"/assignees",
		map[string]any{"user_ids": []uuid.UUID{w.owner.UserID}}, &e), e)
	if st := doJSON(t, "PUT", bea.URL+"/v1/collab/threads/"+tid.String()+"/assignees",
		map[string]any{"user_ids": []uuid.UUID{w.agent2.UserID}}, nil); st != 200 {
		t.Fatalf("Bea takes it on: %d", st)
	}
	mention := fmt.Sprintf(`<p>Can you look, <span data-type="mention" data-id="%s" class="mention">@Olive</span>?</p>`, w.owner.UserID)
	e = apiError{}
	refused("mention someone new", doJSON(t, "POST", bea.URL+"/v1/collab/threads/"+tid.String()+"/comments",
		map[string]any{"body_html": mention, "mentions": []uuid.UUID{w.owner.UserID}}, &e), e)
	if n := w.count(`SELECT count(*) FROM collab_shares WHERE thread_id = $1 AND user_id = $2`, tid, w.owner.UserID); n != 0 {
		t.Fatal("a refused mention shared the conversation")
	}
	if st := doJSON(t, "POST", bea.URL+"/v1/collab/threads/"+tid.String()+"/comments", map[string]any{"body_html": "<p>Looks fine</p>"}, nil); st != 200 {
		t.Fatalf("Bea comments: %d", st)
	}
	e = apiError{}
	refused("reply", doJSON(t, "POST", bea.URL+"/v1/collab/messages", map[string]any{"account_id": a.ID, "mode": "reply", "message_id": mid,
		"to": []string{"from@example.com"}, "subject": "", "body_html": "<p>Accepted</p>"}, &e), e)
	e = apiError{}
	refused("draft", doJSON(t, "POST", bea.URL+"/v1/collab/drafts", map[string]any{"account_id": a.ID, "mode": "reply", "message_id": mid,
		"to": []string{}, "subject": "", "body_html": ""}, &e), e)
	if len(fake.sent) != 0 || len(fake.applied) != 0 {
		t.Fatalf("Bea reached the provider: %d sent, %d applied", len(fake.sent), len(fake.applied))
	}
	var page httpx.Page[MailThread]
	if st := doJSON(t, "GET", bea.URL+"/v1/collab/threads?box=shared", nil, &page); st != 200 || len(page.Data) != 1 || !page.Data[0].PrivateAccount {
		t.Fatalf("Bea's shared box: %d %+v", st, page.Data)
	}
	// The owner does all of it.
	if st := doJSON(t, "POST", adam.URL+"/v1/collab/threads/"+tid.String()+"/comments",
		map[string]any{"body_html": mention, "mentions": []uuid.UUID{w.owner.UserID}}, nil); st != 200 {
		t.Fatalf("Adam mentions Olive: %d", st)
	}

	// A shared account keeps its rules: whoever sees a conversation acts on it and shares it.
	s := w.account(w.agent, ProviderGoogle, VisibilityShared, Audience{UserIDs: []uuid.UUID{w.owner.UserID}})
	sid, _ := w.seedThread(s, "Team mail", w.folder(s, RoleInbox))
	if st := doJSON(t, "PUT", adam.URL+"/v1/collab/threads/"+sid.String()+"/shares",
		map[string]any{"shares": Audience{UserIDs: []uuid.UUID{w.agent2.UserID}}}, nil); st != 200 {
		t.Fatalf("share a shared account's conversation: %d", st)
	}
	if st := doJSON(t, "GET", bea.URL+"/v1/collab/threads/"+sid.String(), nil, &d); st != 200 || !d.CanUseMailbox {
		t.Fatalf("shared account conversation: %d %v", st, d.CanUseMailbox)
	}
	if st := doJSON(t, "POST", bea.URL+"/v1/collab/threads/actions", map[string]any{"thread_ids": []uuid.UUID{sid}, "action": ActionArchive}, nil); st != 200 {
		t.Fatalf("Bea archives a shared account's conversation: %d", st)
	}
	if st := doJSON(t, "PUT", bea.URL+"/v1/collab/threads/"+sid.String()+"/shares",
		map[string]any{"shares": Audience{UserIDs: []uuid.UUID{w.agent2.UserID, w.owner.UserID}}}, nil); st != 200 {
		t.Fatalf("Bea shares a shared account's conversation: %d", st)
	}
}

// TestChangedEventsNameOnlyWhatEachReceiverSees: one batch touching several conversations of
// a private account tells someone who sees one of them about that one alone.
func TestChangedEventsNameOnlyWhatEachReceiverSees(t *testing.T) {
	w := newWorld(t)
	a := w.account(w.agent, ProviderGoogle, VisibilityPrivate, Audience{})
	inbox := w.folder(a, RoleInbox)
	shared, _ := w.seedThread(a, "Shared", inbox)
	private, _ := w.seedThread(a, "Private", inbox)
	mustExec(t, w.d, `INSERT INTO collab_shares (id, workspace_id, thread_id, user_id, shared_by) VALUES ($1, $2, $3, $4, $5)`,
		ids.New(), w.wid, shared, w.agent2.UserID, w.agent.UserID)
	seq := w.count(`SELECT event_seq FROM workspaces WHERE id = $1`, w.wid)
	if err := w.d.DB.Tx(w.ctx, func(tx pgx.Tx) error {
		return w.s.publishChanged(w.ctx, tx, w.wid, &a.ID, []uuid.UUID{shared, private}, true)
	}); err != nil {
		t.Fatal(err)
	}
	events := func(user uuid.UUID, where string, args ...any) int {
		t.Helper()
		return w.count(`SELECT count(*) FROM events WHERE workspace_id = $1 AND seq > $2 AND type = 'collab.changed' AND $3 = ANY(user_ids)`+where,
			append([]any{w.wid, seq, user}, args...)...)
	}
	if n := events(w.agent2.UserID, ""); n != 1 {
		t.Fatalf("Bea got %d events, want 1", n)
	}
	if n := events(w.agent2.UserID, ` AND data->'thread_ids' = $4::jsonb AND data->>'folders' = 'false'`, fmt.Sprintf(`["%s"]`, shared)); n != 1 {
		t.Fatal("Bea's event names more than her conversation")
	}
	if n := events(w.agent.UserID, ` AND data->'thread_ids' ? $4 AND data->'thread_ids' ? $5`, shared.String(), private.String()); n != 1 {
		t.Fatal("the owner is not told of both conversations in one event")
	}
}

// TestAttachmentTypeFromItsBytes: a file is served as the type its bytes have (an active type
// as plain text), and a program is listed but never served.
func TestAttachmentTypeFromItsBytes(t *testing.T) {
	w := newWorld(t)
	a := w.account(w.agent, ProviderGoogle, VisibilityPrivate, Audience{})
	_, mid := w.seedThread(a, "Files", w.folder(a, RoleInbox))
	raw, err := email.BuildMIME(&email.OutMessage{From: mail.Address{Address: "from@example.com"}, To: []string{a.Email}, Subject: "Files",
		MessageID: "<typed@example.com>", HTML: "<p>hi</p>", Text: "hi", Attachments: []email.OutAttachment{
			{Filename: "invoice.pdf", ContentType: "application/pdf", Data: []byte("<html><script>alert(1)</script></html>")},
			{Filename: "setup.exe", ContentType: "application/pdf", Data: []byte("MZ\x90\x00 a program")},
		}}, false)
	if err != nil {
		t.Fatal(err)
	}
	key := "collab/" + w.wid.String() + "/" + a.ID.String() + "/" + mid.String() + ".eml"
	if err := w.d.Storage.Put(w.ctx, key, bytes.NewReader(raw), int64(len(raw)), "message/rfc822"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, w.d, `UPDATE collab_messages SET raw_key = $2, attachments = $3 WHERE id = $1`, mid, key, []storedAttachment{
		{Index: 0, Filename: "invoice.pdf", ContentType: "application/pdf", Size: 37},
		{Index: 1, Filename: "setup.exe", ContentType: "application/pdf", Size: 13}})
	srv := w.api(nil)
	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if resp := get(w.s.attachmentPath(mid, 0)); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("HTML declared as a PDF: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp := get(w.s.attachmentPath(mid, 1)); resp.StatusCode != 403 {
		t.Fatalf("a program was served: %d", resp.StatusCode)
	}

	// Stored mail records what its files are.
	id, err := w.s.storeSent(w.ctx, a, raw, SentRef{RemoteID: "typed-1"}, w.agent.UserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var atts []storedAttachment
	if err := w.d.DB.Pool.QueryRow(w.ctx, `SELECT attachments FROM collab_messages WHERE id = $1`, id).Scan(&atts); err != nil {
		t.Fatal(err)
	}
	if len(atts) != 2 || atts[0].ContentType != "text/plain" || atts[0].Blocked || !atts[1].Blocked {
		t.Fatalf("stored files: %+v", atts)
	}
}

// postSend sends with an idempotency key, as the composer does.
func postSend(t *testing.T, srv string, key string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv+"/v1/collab/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return resp.StatusCode
}

// TestAPISendIdempotencyKey: the same key answers the message it sent instead of sending it
// again, waits while that send's outcome is unknown, and is given back when the provider
// refused the send.
func TestAPISendIdempotencyKey(t *testing.T) {
	w := newWorld(t)
	fake := &fakeProvider{}
	w.useFake(fake)
	a := w.account(w.agent, ProviderGoogle, VisibilityPrivate, Audience{})
	srv := w.api(w.agent).URL
	body := map[string]any{"account_id": a.ID, "mode": "new", "to": []string{"x@example.com"}, "subject": "Hi", "body_html": "<p>Hi</p>"}
	var m1, m2 MailMessage
	if st := postSend(t, srv, "k1", body, &m1); st != 200 || len(fake.sent) != 1 {
		t.Fatalf("first send: %d, %d sent", st, len(fake.sent))
	}
	if st := postSend(t, srv, "k1", body, &m2); st != 200 || m2.ID != m1.ID || len(fake.sent) != 1 {
		t.Fatalf("same key: %d %v, %d sent", st, m2.ID, len(fake.sent))
	}
	// A send whose message was not stored holds its key for a while.
	mustExec(t, w.d, `INSERT INTO collab_send_keys (workspace_id, user_id, key, account_id) VALUES ($1, $2, 'k2', $3)`, w.wid, w.agent.UserID, a.ID)
	var e apiError
	if st := postSend(t, srv, "k2", body, &e); st != 409 || e.Code != "send_in_progress" || len(fake.sent) != 1 {
		t.Fatalf("held key: %d %+v, %d sent", st, e, len(fake.sent))
	}
	mustExec(t, w.d, `UPDATE collab_send_keys SET created_at = now() - interval '1 hour' WHERE key = 'k2' AND user_id = $1`, w.agent.UserID)
	if st := postSend(t, srv, "k2", body, nil); st != 200 || len(fake.sent) != 2 {
		t.Fatalf("after the hold: %d, %d sent", st, len(fake.sent))
	}
	// Refused for good: nothing went out, the key is free.
	fake.err = &providerError{msg: "the recipient address was refused", permanent: true}
	if st := postSend(t, srv, "k3", body, nil); st != 502 {
		t.Fatalf("refused: %d", st)
	}
	fake.err = nil
	if st := postSend(t, srv, "k3", body, nil); st != 200 || len(fake.sent) != 3 {
		t.Fatalf("after a refusal: %d, %d sent", st, len(fake.sent))
	}
}

// TestAPIMicrosoftMessageTooLarge: a Microsoft message past what Graph takes in one request
// is refused before anything is sent.
func TestAPIMicrosoftMessageTooLarge(t *testing.T) {
	w := newWorld(t)
	fake := &fakeProvider{}
	w.useFake(fake)
	a := w.account(w.agent, ProviderMicrosoft, VisibilityPrivate, Audience{})
	_, mid := w.seedThread(a, "Big file", w.folder(a, RoleInbox))
	raw, err := email.BuildMIME(&email.OutMessage{From: mail.Address{Address: "from@example.com"}, To: []string{a.Email}, Subject: "Big file",
		MessageID: "<big@example.com>", HTML: "<p>hi</p>", Text: "hi", Attachments: []email.OutAttachment{
			{Filename: "scan.pdf", ContentType: "application/pdf", Data: append([]byte("%PDF-1.4 "), bytes.Repeat([]byte("A"), 3_300_000)...)},
		}}, false)
	if err != nil {
		t.Fatal(err)
	}
	key := "collab/" + w.wid.String() + "/" + a.ID.String() + "/" + mid.String() + ".eml"
	if err := w.d.Storage.Put(w.ctx, key, bytes.NewReader(raw), int64(len(raw)), "message/rfc822"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, w.d, `UPDATE collab_messages SET raw_key = $2, attachments = $3 WHERE id = $1`, mid, key, []storedAttachment{
		{Index: 0, Filename: "scan.pdf", ContentType: "application/pdf", Size: 3_300_009}})
	var e apiError
	st := doJSON(t, "POST", w.api(w.agent).URL+"/v1/collab/messages", map[string]any{"account_id": a.ID, "mode": "forward", "message_id": mid,
		"to": []string{"x@example.com"}, "subject": "", "body_html": "<p>FYI</p>", "forward_attachments": true}, &e)
	if st != 422 || e.Code != "message_too_large" || len(fake.sent) != 0 {
		t.Fatalf("too large: %d %+v, %d sent", st, e, len(fake.sent))
	}
}
