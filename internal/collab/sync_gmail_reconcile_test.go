package collab

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/useopendesk/opendesk/internal/jobs"
)

// lastApplyArgs is the collab.apply job applyLocal queued last for the account.
func (w *world) lastApplyArgs(a *accountRow) jobs.CollabApplyArgs {
	w.t.Helper()
	var raw []byte
	if err := w.d.DB.Pool.QueryRow(w.ctx, `SELECT args FROM river_job WHERE kind = 'collab.apply' AND args->>'account_id' = $1
		ORDER BY id DESC LIMIT 1`, a.ID.String()).Scan(&raw); err != nil {
		w.t.Fatal(err)
	}
	var args jobs.CollabApplyArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		w.t.Fatal(err)
	}
	return args
}

// runApply runs a collab.apply job as River would, on its first attempt.
func (w *world) runApply(args jobs.CollabApplyArgs) {
	w.t.Helper()
	if err := w.s.applyRemote(w.ctx, &river.Job[jobs.CollabApplyArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 8}, Args: args}); err != nil {
		w.t.Fatalf("applyRemote: %v", err)
	}
}

// TestGmailExpiredHistoryReconcilesStoredMail: when Gmail no longer has the history since
// the last sync, every stored message is read again, so mail deleted or archived meanwhile
// is brought in step, whatever the account's category engine.
func TestGmailExpiredHistoryReconcilesStoredMail(t *testing.T) {
	w := newWorld(t)
	g := newFakeGmail(t)
	w.useGmail(g)
	a := w.account(w.agent, ProviderGoogle, VisibilityPrivate, Audience{})
	g.add("m1", "t1", []string{"INBOX"}, testMail{from: "a@example.com", to: a.Email, subject: "Gone", msgID: "gone@example.com", body: "Gone"})
	g.add("m2", "t2", []string{"INBOX"}, testMail{from: "b@example.com", to: a.Email, subject: "Kept", msgID: "kept@example.com", body: "Kept"})
	a = w.sync(a)
	inbox := w.folderByRemote(a, "INBOX")
	if !sliceHas(w.messageByRemote(a, "m2").FolderIDs, inbox.ID) {
		t.Fatal("m2 not in the inbox")
	}

	// While the history lapsed: m1 deleted, m2 archived, and Gmail keeps no record of either.
	g.mu.Lock()
	delete(g.msgs, "m1")
	g.msgs["m2"].labels = []string{}
	g.history += 10
	g.expireBefore = g.history
	g.mu.Unlock()
	for i := 0; i < 5; i++ {
		a = w.sync(a)
		if i > 0 && w.count(`SELECT count(*) FROM collab_accounts WHERE id = $1 AND category_refresh IS NOT NULL`, a.ID) == 0 {
			break
		}
	}
	if n := w.count(`SELECT count(*) FROM collab_messages WHERE account_id = $1 AND remote_id = 'm1'`, a.ID); n != 0 {
		t.Fatal("a message deleted while the history lapsed is kept")
	}
	if m := w.messageByRemote(a, "m2"); sliceHas(m.FolderIDs, inbox.ID) {
		t.Fatalf("an archived message keeps INBOX: %v", m.FolderIDs)
	}
	if n := w.count(`SELECT count(*) FROM collab_accounts WHERE id = $1 AND category_refresh IS NOT NULL`, a.ID); n != 0 || historyOf(a) != strconv.Itoa(g.history) {
		t.Fatalf("the sweep did not finish: history %s", historyOf(a))
	}
}

// TestGmailActionsPinnedToTheMessagesActedOn: a queued action changes the messages it was
// taken on, never mail that arrived before it ran; an archive also takes the conversation's
// older messages opendesk does not store.
func TestGmailActionsPinnedToTheMessagesActedOn(t *testing.T) {
	w := newWorld(t)
	g := newFakeGmail(t)
	w.useGmail(g)
	a := w.account(w.agent, ProviderGoogle, VisibilityPrivate, Audience{})
	g.add("m0", "t1", []string{"INBOX"}, testMail{from: "frank@example.com", to: a.Email, subject: "Hi", msgID: "h0@example.com", body: "Long ago",
		date: time.Now().Add(-90 * 24 * time.Hour)})
	g.add("m1", "t1", []string{"INBOX", "UNREAD"}, testMail{from: "frank@example.com", to: a.Email, subject: "Hi", msgID: "h1@example.com",
		inReplyTo: "h0@example.com", body: "Hi", date: time.Now().Add(-time.Hour)})
	a = w.sync(a)
	if n := w.count(`SELECT count(*) FROM collab_messages WHERE account_id = $1 AND remote_id = 'm0'`, a.ID); n != 0 {
		t.Fatal("mail older than the first sync was imported")
	}
	t1 := w.threadByRemote(a, "t1")
	if err := w.d.DB.Tx(w.ctx, func(tx pgx.Tx) error { return w.s.applyLocal(w.ctx, tx, a, []uuid.UUID{t1.ID}, ActionArchive, nil) }); err != nil {
		t.Fatal(err)
	}
	args := w.lastApplyArgs(a)
	if len(args.MessageIDs) != 1 || args.At == nil {
		t.Fatalf("job not pinned: %+v", args)
	}

	// The customer answers before the job runs.
	g.add("m2", "t1", []string{"INBOX", "UNREAD"}, testMail{from: "frank@example.com", to: a.Email, subject: "Re: Hi", msgID: "h2@example.com",
		inReplyTo: "h1@example.com", body: "One more thing", date: time.Now().Add(time.Minute)})
	g.record("messagesAdded", "m2")
	a = w.sync(a)
	if m2 := w.messageByRemote(a, "m2"); m2.ThreadID != t1.ID {
		t.Fatal("the answer is in another conversation")
	}
	w.runApply(args)
	last := g.modify[len(g.modify)-1]
	if fmt.Sprint(last["removeLabelIds"]) != "[INBOX]" || fmt.Sprint(last["ids"]) != "[m1 m0]" {
		t.Fatalf("archive at Gmail: %v", last)
	}
}

// TestGmailRefusedActionReadsBack: an action Gmail refuses for good leaves no history, so
// the messages it was to change are read again and keep Gmail's labels.
func TestGmailRefusedActionReadsBack(t *testing.T) {
	w := newWorld(t)
	g := newFakeGmail(t)
	w.useGmail(g)
	a := w.account(w.agent, ProviderGoogle, VisibilityPrivate, Audience{})
	g.add("m1", "t1", []string{"INBOX"}, testMail{from: "frank@example.com", to: a.Email, subject: "Hi", msgID: "r1@example.com", body: "Hi"})
	a = w.sync(a)
	inbox := w.folderByRemote(a, "INBOX")
	t1 := w.threadByRemote(a, "t1")
	if err := w.d.DB.Tx(w.ctx, func(tx pgx.Tx) error { return w.s.applyLocal(w.ctx, tx, a, []uuid.UUID{t1.ID}, ActionArchive, nil) }); err != nil {
		t.Fatal(err)
	}
	if sliceHas(w.messageByRemote(a, "m1").FolderIDs, inbox.ID) {
		t.Fatal("not archived in opendesk")
	}
	g.mu.Lock()
	g.modifyStatus = 400
	g.mu.Unlock()
	w.runApply(w.lastApplyArgs(a))
	if m := w.messageByRemote(a, "m1"); !sliceHas(m.FolderIDs, inbox.ID) {
		t.Fatalf("a refused archive was not read back: %v", m.FolderIDs)
	}
	if !slices.Contains(w.threadByRemote(a, "t1").FolderIDs, inbox.ID) {
		t.Fatal("the conversation is not back in the inbox")
	}
}
