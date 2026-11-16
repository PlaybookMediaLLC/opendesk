package collab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/email"
	"github.com/useopendesk/opendesk/internal/httpx"
	"github.com/useopendesk/opendesk/internal/jobs"
)

// Actions on conversations (contracts/collab.md "Actions"): applied at once in opendesk
// (applyLocal), then at the provider by collab.apply, then a sync brings the provider's
// state back, which also undoes an action the provider refused for good.
//
// Which messages an action touches:
//   - read, unstar: every message; unread, star: the newest message of each conversation.
//   - Gmail: labels change on every message (archive removes INBOX; move adds the label and
//     removes INBOX; trash is threads.trash).
//   - Microsoft and IMAP: moving actions move every message that is in a folder other
//     than Sent and Drafts; mail in Sent and Drafts stays there.

var (
	errNoFolder      = httpx.E(422, "validation_error", "Choose a folder of this account.")
	errNoTrashFolder = httpx.E(422, "folder_missing", "This account has no folder for this. Create it at the provider first.")
)

// movingActions take messages out of their folders.
var movingActions = []string{ActionArchive, ActionInbox, ActionTrash, ActionSpam, ActionNotSpam, ActionMove}

// actionMessage is a stored message of a conversation an action applies to.
type actionMessage struct {
	*storedMsg
	threadRemoteID string
	sentAt         time.Time
}

// actionMessages loads the account's messages of the conversations, oldest first.
func actionMessages(ctx context.Context, q db.Querier, a *accountRow, threadIDs []uuid.UUID) ([]actionMessage, error) {
	rows, err := q.Query(ctx, `SELECT `+storedColumns+`, t.remote_id FROM collab_messages m
		JOIN collab_threads t ON t.id = m.thread_id
		WHERE m.account_id = $1 AND m.workspace_id = $2 AND m.thread_id = ANY($3) ORDER BY m.thread_id, m.sent_at, m.id`,
		a.ID, a.WorkspaceID, threadIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (actionMessage, error) {
		var m storedMsg
		var out actionMessage
		err := r.Scan(&m.ID, &m.ThreadID, &m.RemoteID, &m.MessageID, &m.Unread, &m.Starred, &m.Draft, &m.RawKey, &m.Location,
			&m.FolderIDs, &m.SentAt, &out.threadRemoteID)
		if m.FolderIDs == nil {
			m.FolderIDs = []uuid.UUID{}
		}
		out.sentAt = m.SentAt
		out.storedMsg = &m
		return out, err
	})
}

// newestOf picks the newest message of each conversation (list is oldest first per thread).
func newestOf(list []actionMessage) []actionMessage {
	last := map[uuid.UUID]int{}
	for i, m := range list {
		last[m.ThreadID] = i
	}
	out := []actionMessage{}
	for i, m := range list {
		if last[m.ThreadID] == i {
			out = append(out, m)
		}
	}
	return out
}

// normalizeAction turns a move to a special folder into that folder's action.
func normalizeAction(action string, folder *folderRow) string {
	if action != ActionMove || folder == nil || folder.Role == nil {
		return action
	}
	switch *folder.Role {
	case RoleInbox:
		return ActionInbox
	case RoleTrash:
		return ActionTrash
	case RoleSpam:
		return ActionSpam
	case RoleArchive:
		return ActionArchive
	}
	return action
}

// applyLocal applies an action to the account's conversations in opendesk (messages'
// flags and folders, then each thread's state), in tx, and queues it at the provider
// (jobs.CollabApplyArgs, after commit). folder is the target of move, label and unlabel.
// It refuses label and unlabel on a provider without labels (errLabels). It publishes
// nothing; the caller publishes collab.changed.
func (s *service) applyLocal(ctx context.Context, tx pgx.Tx, a *accountRow, threadIDs []uuid.UUID, action string, folder *folderRow) error {
	current, err := loadAccountForUpdate(ctx, tx, a.ID)
	if err != nil {
		return err
	}
	a = current
	if action == ActionDone || action == ActionNeedsAction {
		// Any other handling state ends a snooze, so a conversation marked done is not woken later.
		_, err := tx.Exec(ctx, `UPDATE collab_threads SET needs_action = $3, snoozed_until = NULL, updated_at = now()
			WHERE workspace_id = $1 AND id = ANY($2)`, a.WorkspaceID, threadIDs, action == ActionNeedsAction)
		return err
	}
	if (action == ActionLabel || action == ActionUnlabel) && a.Provider == ProviderIMAP {
		return errLabels
	}
	if action == ActionMove || action == ActionLabel || action == ActionUnlabel {
		if folder == nil || folder.AccountID != a.ID || folder.WorkspaceID != a.WorkspaceID {
			return errNoFolder
		}
		if (action == ActionLabel || action == ActionUnlabel) && folder.Kind != KindLabel {
			return errLabels
		}
		if action == ActionMove && a.Provider == ProviderMicrosoft && folder.Kind == KindLabel {
			return errMoveToCategory
		}
	} else {
		folder = nil
	}
	action = normalizeAction(action, folder)
	if action != ActionMove && action != ActionLabel && action != ActionUnlabel {
		folder = nil
	}
	p, err := s.newPass(ctx, tx, a)
	if err != nil {
		return err
	}
	msgs, err := actionMessages(ctx, tx, a, threadIDs)
	if err != nil {
		return err
	}
	var target *folderRow
	if slices.Contains(movingActions, action) {
		if target, err = s.localTarget(ctx, tx, p, action, folder); err != nil {
			return err
		}
	}
	change := func(m actionMessage, fn func(st *state)) error {
		st := state{Unread: m.Unread, Starred: m.Starred, Draft: m.Draft, FolderIDs: slices.Clone(m.FolderIDs)}
		fn(&st)
		return p.update(ctx, tx, m.storedMsg, st)
	}
	roleID := func(role string) uuid.UUID {
		if f := p.roleFolder(role); f != nil {
			return f.ID
		}
		return uuid.Nil
	}
	gmail := a.Provider == ProviderGoogle
	labels := map[uuid.UUID]bool{}
	for _, f := range p.folders {
		if f.Kind == KindLabel {
			labels[f.ID] = true
		}
	}
	picked := pick(action, msgs)
	for _, m := range picked {
		err := change(m, func(st *state) {
			switch action {
			case ActionRead:
				st.Unread = false
			case ActionUnread:
				st.Unread = true
			case ActionStar:
				st.Starred = true
				if gmail {
					st.FolderIDs = addID(st.FolderIDs, roleID(RoleStarred))
				}
			case ActionUnstar:
				st.Starred = false
				if gmail {
					st.FolderIDs = removeID(st.FolderIDs, roleID(RoleStarred))
				}
			case ActionLabel:
				st.FolderIDs = addID(st.FolderIDs, folder.ID)
			case ActionUnlabel:
				st.FolderIDs = removeID(st.FolderIDs, folder.ID)
			default:
				if gmail {
					st.FolderIDs = gmailMove(st.FolderIDs, action, target, roleID)
				} else {
					st.FolderIDs = folderMove(st.FolderIDs, target.ID, func(id uuid.UUID) bool {
						return id == roleID(RoleSent) || id == roleID(RoleDrafts) || labels[id]
					})
				}
			}
		})
		if err != nil {
			return err
		}
	}
	if err := recomputeThreads(ctx, tx, a, threadIDs); err != nil {
		return err
	}
	if err := projectGmailCategories(ctx, tx, a, threadIDs); err != nil {
		return err
	}
	if s.d.Jobs == nil || a.Config.Demo || len(picked) == 0 {
		return nil // a demo account's mail is only here: nothing to make at a provider
	}
	// The job is pinned to the messages changed here: mail that arrives before it runs keeps
	// its own state at the provider.
	mids := make([]uuid.UUID, 0, len(picked))
	for _, m := range picked {
		mids = append(mids, m.ID)
	}
	at := time.Now()
	args := jobs.CollabApplyArgs{WorkspaceID: a.WorkspaceID, AccountID: a.ID, ThreadIDs: threadIDs, Action: action, MessageIDs: mids, At: &at}
	if folder != nil {
		args.FolderID = &folder.ID
	}
	_, err = s.d.Jobs.InsertTx(ctx, tx, args, &river.InsertOpts{Queue: jobs.QueueEmail, MaxAttempts: 8})
	return err
}

// pick is the messages an action changes.
func pick(action string, msgs []actionMessage) []actionMessage {
	if action == ActionUnread || action == ActionStar {
		return newestOf(msgs)
	}
	return msgs
}

// localTarget is the folder a moving action takes messages to. An IMAP account without
// an Archive folder gets one (created at the server by the job).
func (s *service) localTarget(ctx context.Context, tx pgx.Tx, p *pass, action string, folder *folderRow) (*folderRow, error) {
	if action == ActionMove {
		return folder, nil
	}
	role := map[string]string{ActionArchive: RoleArchive, ActionInbox: RoleInbox, ActionNotSpam: RoleInbox,
		ActionTrash: RoleTrash, ActionSpam: RoleSpam}[action]
	if f := p.roleFolder(role); f != nil {
		return f, nil
	}
	switch {
	case p.a.Provider == ProviderGoogle && role == RoleArchive:
		return nil, nil // Gmail archives by removing INBOX
	case p.a.Provider == ProviderIMAP && role == RoleArchive:
		f, err := s.upsertFolder(ctx, tx, p.a, RemoteFolder{RemoteID: "Archive", Name: "Archive", Path: "Archive", Role: RoleArchive, Kind: KindFolder})
		if err != nil {
			return nil, err
		}
		p.folders[f.RemoteID] = f
		p.foldersChanged = true
		return f, nil
	}
	return nil, errNoTrashFolder
}

// gmailMove is a Gmail message's labels after a moving action.
func gmailMove(folders []uuid.UUID, action string, target *folderRow, roleID func(string) uuid.UUID) []uuid.UUID {
	inbox, trash, spam := roleID(RoleInbox), roleID(RoleTrash), roleID(RoleSpam)
	switch action {
	case ActionArchive:
		return removeID(folders, inbox)
	case ActionInbox:
		return addID(removeID(removeID(folders, trash), spam), inbox)
	case ActionTrash:
		return addID(removeID(removeID(folders, inbox), spam), trash)
	case ActionSpam:
		return addID(removeID(folders, inbox), spam)
	case ActionNotSpam:
		return addID(removeID(folders, spam), inbox)
	case ActionMove:
		if target == nil {
			return folders
		}
		return addID(removeID(folders, inbox), target.ID)
	}
	return folders
}

// folderMove is a Microsoft or IMAP message's folders after it moves to target: its
// copies in Sent and Drafts and its labels (Outlook categories) stay; a message only
// there does not move.
func folderMove(folders []uuid.UUID, target uuid.UUID, stays func(uuid.UUID) bool) []uuid.UUID {
	out := []uuid.UUID{}
	moves := false
	for _, f := range folders {
		if stays(f) {
			out = append(out, f)
		} else {
			moves = true
		}
	}
	if !moves {
		return folders
	}
	return addID(out, target)
}

func addID(list []uuid.UUID, id uuid.UUID) []uuid.UUID {
	if id == uuid.Nil || slices.Contains(list, id) {
		return list
	}
	return append(list, id)
}

func removeID(list []uuid.UUID, id uuid.UUID) []uuid.UUID {
	return slices.DeleteFunc(list, func(x uuid.UUID) bool { return x == id })
}

// applyRemote makes a queued action at the provider. A refusal for good is logged and the
// messages it was to change are read again from the provider (readBack): a change the
// provider refused leaves nothing in its history or delta for the sync to bring back.
// Passing failures are retried by River. Either way a sync follows.
func (s *service) applyRemote(ctx context.Context, job *river.Job[jobs.CollabApplyArgs]) error {
	args := job.Args
	if args.CategoryIntentID != nil || args.Action == ActionCategorize {
		return s.deliverCategoryIntent(ctx, job)
	}
	a, err := loadAccount(ctx, s.d.DB.Pool, args.WorkspaceID, args.AccountID)
	if db.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.pauseIfInactive(ctx, a.WorkspaceID); err != nil {
		return err
	}
	if a.Reconnect {
		return nil
	}
	err = s.applyAtProvider(ctx, a, args)
	if err == nil || isPermanent(err) || job.Attempt >= job.MaxAttempts {
		if err != nil {
			s.log.Info("collab action refused by the provider", "account_id", a.ID, "action", args.Action, "reason", safeError(err))
			if isReconnect(err) {
				_ = s.recordSyncError(ctx, a, err)
			} else if rerr := s.readBack(ctx, a, args); rerr != nil {
				s.log.Info("collab state not read back after a refused action", "account_id", a.ID, "reason", safeError(rerr))
			}
		}
		return s.queueSync(ctx, s.d.DB.Pool, a.ID)
	}
	return err
}

// jobMessages loads the stored messages a queued action applies to: those it was pinned
// to, else (a job queued before pinning) every message of its conversations.
func jobMessages(ctx context.Context, q db.Querier, a *accountRow, args jobs.CollabApplyArgs) ([]actionMessage, error) {
	msgs, err := actionMessages(ctx, q, a, args.ThreadIDs)
	if err != nil || len(args.MessageIDs) == 0 {
		return msgs, err
	}
	pinned := make(map[uuid.UUID]bool, len(args.MessageIDs))
	for _, id := range args.MessageIDs {
		pinned[id] = true
	}
	return slices.DeleteFunc(msgs, func(m actionMessage) bool { return !pinned[m.ID] }), nil
}

func (s *service) applyAtProvider(ctx context.Context, a *accountRow, args jobs.CollabApplyArgs) error {
	msgs, err := jobMessages(ctx, s.d.DB.Pool, a, args)
	if err != nil || len(msgs) == 0 {
		return err
	}
	folders, err := loadFolders(ctx, s.d.DB.Pool, a.ID)
	if err != nil {
		return err
	}
	op := ApplyOp{Action: args.Action, Special: map[string]RemoteFolder{}}
	if args.At != nil {
		op.Before = *args.At
	}
	var target *folderRow
	for _, f := range folders {
		if f.Role != nil {
			if _, ok := op.Special[*f.Role]; !ok {
				op.Special[*f.Role] = f.remote()
			}
		}
		if args.FolderID != nil && f.ID == *args.FolderID {
			target = f
		}
	}
	if args.FolderID != nil {
		if target == nil {
			return nil // the folder is gone
		}
		r := target.remote()
		op.Target = &r
	}
	chosen := pick(args.Action, msgs)
	if slices.Contains(movingActions, args.Action) && a.Provider != ProviderGoogle {
		// The messages applyLocal moved are now in the target folder.
		dest := op.Target
		if dest == nil {
			if t, err := moveTarget(op); err == nil {
				dest = &t
			}
		}
		chosen = slices.DeleteFunc(slices.Clone(msgs), func(m actionMessage) bool {
			if dest == nil {
				return true
			}
			for _, f := range folders {
				if f.RemoteID == dest.RemoteID && slices.Contains(m.FolderIDs, f.ID) {
					return false
				}
			}
			return true
		})
	}
	for _, m := range chosen {
		op.Messages = append(op.Messages, m.ref(m.threadRemoteID))
	}
	if len(op.Messages) == 0 {
		return nil
	}
	prov, err := s.provider(ctx, a)
	if err != nil {
		return err
	}
	defer prov.Close()
	return prov.Apply(ctx, op)
}

// readBackMax bounds the messages one refused action reads again.
const readBackMax = 500

// readBack reads the messages a refused action was to change from the provider and writes
// what it has: labels or flags, and folders (contracts/collab.md "Actions"). A message the
// provider no longer has is removed (Gmail, Microsoft) or loses that location (IMAP).
func (s *service) readBack(ctx context.Context, a *accountRow, args jobs.CollabApplyArgs) error {
	msgs, err := jobMessages(ctx, s.d.DB.Pool, a, args)
	if err != nil || len(msgs) == 0 {
		return err
	}
	// A sent message stored under an id of opendesk's own ("sent:") has none at the provider.
	msgs = slices.DeleteFunc(msgs, func(m actionMessage) bool { return strings.HasPrefix(m.RemoteID, "sent:") })
	if len(msgs) > readBackMax {
		msgs = msgs[len(msgs)-readBackMax:]
	}
	if len(msgs) == 0 {
		return nil
	}
	prov, err := s.provider(ctx, a)
	if err != nil {
		return err
	}
	defer prov.Close()
	p, err := s.newPass(ctx, s.d.DB.Pool, a)
	if err != nil {
		return err
	}
	switch pr := prov.(type) {
	case *gmailProvider:
		return s.readBackGmail(ctx, p, pr, msgs)
	case *graphProvider:
		return s.readBackGraph(ctx, p, pr, msgs)
	case *imapProvider:
		return s.readBackIMAP(ctx, p, pr, msgs)
	}
	return nil
}

func (s *service) readBackGmail(ctx context.Context, p *pass, g *gmailProvider, msgs []actionMessage) error {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.RemoteID)
	}
	res, errs := fetchEach(ctx, gmailFetchers, ids, g.fork, func(ctx context.Context, g *gmailProvider, id string) (*gmailMessage, error) {
		var m gmailMessage
		err := g.do(ctx, http.MethodGet, "/messages/"+url.PathEscape(id)+"?format=minimal", nil, &m)
		return &m, err
	})
	return p.batch(ctx, func(tx pgx.Tx) error {
		for i, m := range msgs {
			switch {
			case isNotFound(errs[i]):
				if err := p.remove(ctx, tx, m.storedMsg); err != nil {
					return err
				}
			case errs[i] != nil || res[i] == nil:
			default:
				if err := p.update(ctx, tx, m.storedMsg, p.gmailState(res[i].LabelIDs)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *service) readBackGraph(ctx context.Context, p *pass, g *graphProvider, msgs []actionMessage) error {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.RemoteID)
	}
	res, errs := fetchEach(ctx, graphFetchers, ids, g.fork, func(ctx context.Context, g *graphProvider, id string) (*graphMessage, error) {
		var m graphMessage
		err := g.do(ctx, http.MethodGet, "/me/messages/"+url.PathEscape(id)+"?$select="+graphDeltaSelect, nil, &m)
		return &m, err
	})
	return p.batch(ctx, func(tx pgx.Tx) error {
		for i, m := range msgs {
			switch {
			case isNotFound(errs[i]):
				if err := p.remove(ctx, tx, m.storedMsg); err != nil {
					return err
				}
				continue
			case errs[i] != nil || res[i] == nil:
				continue
			}
			gm := res[i]
			st := state{Unread: !gm.IsRead, Starred: gm.Flag.FlagStatus == "flagged", Draft: gm.IsDraft, FolderIDs: []uuid.UUID{}}
			if f, ok := p.folders[gm.ParentFolderID]; ok && f.Kind == KindFolder {
				st.FolderIDs = append(st.FolderIDs, f.ID)
			} else {
				// A folder opendesk does not keep: the message keeps the folders it has.
				for _, id := range m.FolderIDs {
					if f := p.folderByID(id); f != nil && f.Kind == KindFolder {
						st.FolderIDs = append(st.FolderIDs, id)
					}
				}
			}
			cats, err := p.categoryIDs(ctx, tx, gm.Categories)
			if err != nil {
				return err
			}
			st.FolderIDs = append(st.FolderIDs, cats...)
			if err := p.update(ctx, tx, m.storedMsg, st); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *service) readBackIMAP(ctx context.Context, p *pass, ip *imapProvider, msgs []actionMessage) error {
	c, err := ip.session(ctx)
	if err != nil {
		return err
	}
	byFolder := map[string][]imapLoc{}
	order := []string{}
	for _, m := range msgs {
		for _, l := range parseLocation(m.Location) {
			if _, ok := byFolder[l.Folder]; !ok {
				order = append(order, l.Folder)
			}
			byFolder[l.Folder] = append(byFolder[l.Folder], l)
		}
	}
	checked := map[string]bool{}
	present := map[imapLoc][]imap.Flag{}
	for _, f := range order {
		sel, err := c.Select(f, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			if isNonExistent(err) {
				checked[f] = true
				continue
			}
			return opError("opening a folder", err)
		}
		checked[f] = true
		uids := []uint32{}
		for _, l := range byFolder[f] {
			if l.Validity == sel.UIDValidity {
				uids = append(uids, l.UID)
			}
		}
		if len(uids) == 0 {
			continue
		}
		got, err := c.Fetch(uidSet(uids), &imap.FetchOptions{UID: true, Flags: true}).Collect()
		if err != nil {
			return opError("reading flags", err)
		}
		for _, x := range got {
			present[imapLoc{Folder: f, Validity: sel.UIDValidity, UID: uint32(x.UID)}] = x.Flags
		}
	}
	return p.batch(ctx, func(tx pgx.Tx) error {
		for _, m := range msgs {
			keep := []imapLoc{}
			var flags []imap.Flag
			read := false
			for _, l := range parseLocation(m.Location) {
				if !checked[l.Folder] {
					keep = append(keep, l)
					continue
				}
				if fl, ok := present[l]; ok {
					keep, flags, read = append(keep, l), fl, true
				}
			}
			var err error
			if read {
				err = p.update(ctx, tx, m.storedMsg, p.imapState(flags, keep))
			} else {
				err = p.setLocs(ctx, tx, m.storedMsg, keep)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (m actionMessage) ref(threadRemoteID string) MessageRef {
	return MessageRef{RemoteID: m.RemoteID, ThreadRemoteID: threadRemoteID, Location: m.Location}
}

// storeSent stores a message the account just sent (raw is what was sent, Bcc included)
// in its conversation (threadID, else found from its headers, else new) and in Sent, with
// sent_by, and publishes collab.changed. It returns the stored message's id.
func (s *service) storeSent(ctx context.Context, a *accountRow, raw []byte, ref SentRef, sentBy uuid.UUID, threadID *uuid.UUID) (uuid.UUID, error) {
	p, err := s.newPass(ctx, s.d.DB.Pool, a)
	if err != nil {
		return uuid.Nil, err
	}
	remote := ref.RemoteID
	if remote == "" {
		pm, err := email.ParseMail(raw)
		msgID := ""
		if err == nil {
			msgID = pm.MessageID
		}
		remote = imapRemoteID(msgID, raw)
		if a.Provider != ProviderIMAP {
			remote = "sent:" + remote
		}
	}
	folders := p.folderIDs(ref.FolderRemoteIDs)
	if len(folders) == 0 {
		if f := p.roleFolder(RoleSent); f != nil {
			folders = []uuid.UUID{f.ID}
		}
	}
	in := &newMessage{RemoteID: remote, ThreadRemoteID: ref.ThreadRemoteID, Raw: raw, Date: time.Now(),
		State: state{FolderIDs: folders, Location: sentLocation(a, ref)}, SentBy: &sentBy, ThreadID: threadID}
	var id uuid.UUID
	err = p.batch(ctx, func(tx pgx.Tx) error {
		var err error
		if id, err = p.insert(ctx, tx, in); err != nil || id == uuid.Nil {
			return err
		}
		// A reply sent instead of an AI draft of the conversation replaces it ("AI draft outcomes").
		return settleSent(ctx, tx, nil, id, sentBy)
	})
	if err != nil {
		return uuid.Nil, err
	}
	if id == uuid.Nil {
		return uuid.Nil, httpx.E(422, "validation_error", "The message could not be read back to store it.")
	}
	return id, nil
}

// sentLocation is where a message the account just sent is at the provider: the copy's own
// location, or for IMAP mail the server kept no copy of, localLocation.
func sentLocation(a *accountRow, ref SentRef) json.RawMessage {
	if a.Provider == ProviderIMAP && len(parseLocation(ref.Location)) == 0 {
		return localLocation
	}
	return ref.Location
}

// loadRaw reads a stored message's raw source: from storage (raw_key), else the provider.
func (s *service) loadRaw(ctx context.Context, a *accountRow, m *messageRow, threadRemoteID string) ([]byte, error) {
	if m.RawKey != "" {
		rc, err := s.d.Storage.Get(ctx, m.RawKey)
		if err == nil {
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, maxRawBytes+1))
		}
	}
	prov, err := s.provider(ctx, a)
	if err != nil {
		return nil, err
	}
	defer prov.Close()
	return prov.FetchRaw(ctx, m.ref(threadRemoteID))
}
