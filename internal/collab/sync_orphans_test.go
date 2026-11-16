package collab

import (
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/google/uuid"

	"github.com/useopendesk/opendesk/internal/ids"
	"github.com/useopendesk/opendesk/internal/jobs"
)

// Mail missed by one sync pass (contracts/collab.md "Sync"): it stays, with its
// conversation's comments, until later passes and the provider agree it is gone.

// imapAccount is a private IMAP account of Adam's on a fresh in-memory server holding INBOX
// and folders, and a client to that server standing in for the person's phone.
func imapAccount(t *testing.T, w *world, folders ...string) (*accountRow, *mailServer) {
	t.Helper()
	imapAddr := startIMAP(t)
	_, smtpAddr := startSMTP(t)
	ih, ip := hostPort(t, imapAddr)
	sh, sp := hostPort(t, smtpAddr)
	a := w.account(w.agent, ProviderIMAP, VisibilityPrivate, Audience{})
	cfg := accountConfig{IMAPHost: ih, IMAPPort: ip, IMAPTLS: "none", IMAPUsername: "user@acme.test",
		SMTPHost: sh, SMTPPort: sp, SMTPTLS: "none"}
	mustExec(t, w.d, `UPDATE collab_accounts SET config = $2 WHERE id = $1`, a.ID, cfg)
	srv := dialServer(t, imapAddr)
	for _, f := range folders {
		srv.create(f)
	}
	return w.reload(a), srv
}

// expunge removes a message from a folder for good, as another client would.
func (m *mailServer) expunge(folder, msgID string) {
	m.t.Helper()
	uids, _ := m.find(folder, msgID)
	if len(uids) == 0 {
		m.t.Fatalf("%s not in %s", msgID, folder)
	}
	if err := m.c.Store(imap.UIDSetNum(uids...), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		m.t.Fatal(err)
	}
	if err := m.c.Expunge().Close(); err != nil {
		m.t.Fatal(err)
	}
}

// comment adds a teammate's comment to a conversation.
func (w *world) comment(threadID uuid.UUID, body string) {
	w.t.Helper()
	mustExec(w.t, w.d, `INSERT INTO collab_comments (id, workspace_id, thread_id, author_id, body_html) VALUES ($1, $2, $3, $4, $5)`,
		ids.New(), w.wid, threadID, w.agent.UserID, "<p>"+body+"</p>")
}

func (w *world) comments(threadID uuid.UUID) int {
	w.t.Helper()
	return w.count(`SELECT count(*) FROM collab_comments WHERE thread_id = $1`, threadID)
}

// rewindFolder makes the next pass read a folder's every UID as new, so mail older than the
// first sync's window is stored (as a history import would).
func (w *world) rewindFolder(a *accountRow, remote string) {
	w.t.Helper()
	mustExec(w.t, w.d, `UPDATE collab_folders SET sync_state = sync_state || '{"u": 0}'::jsonb WHERE account_id = $1 AND remote_id = $2`, a.ID, remote)
}

func (w *world) orphaned(a *accountRow, remote string) bool {
	w.t.Helper()
	return w.count(`SELECT count(*) FROM collab_messages WHERE account_id = $1 AND remote_id = $2 AND orphaned_at IS NOT NULL`, a.ID, remote) > 0
}

func locatedIn(m *messageRow, folder string) bool {
	return slices.ContainsFunc(parseLocation(m.Location), func(l imapLoc) bool { return l.Folder == folder })
}

// TestIMAPGmailArchiveKeepsTheConversation: Gmail over IMAP archives by taking mail out of
// INBOX; it stays in All Mail, so the conversation and its comments stay. Mail deleted at
// Gmail goes after the grace, its conversation set aside with its comments until the same
// mail comes back.
func TestIMAPGmailArchiveKeepsTheConversation(t *testing.T) {
	w := newWorld(t)
	a, srv := imapAccount(t, w, "[Gmail]/All Mail")
	raw := testMail{from: "Frank <frank@example.com>", to: a.Email, subject: "Lease", msgID: "lease@example.com", body: "Sign here"}.raw()
	at := time.Now().Add(-time.Hour)
	srv.append("INBOX", raw, at)
	srv.append("[Gmail]/All Mail", raw, at)
	a = w.sync(a)
	all := w.folderByRemote(a, "[Gmail]/All Mail")
	if all.Role == nil || *all.Role != RoleAll {
		t.Fatalf("All Mail role: %v", all.Role)
	}
	m := w.messageByRemote(a, "lease@example.com")
	tid := m.ThreadID
	w.comment(tid, "Check the clause")

	// Archived in the Gmail app.
	srv.expunge("INBOX", "lease@example.com")
	a = w.sync(a)
	m = w.messageByRemote(a, "lease@example.com")
	if len(m.FolderIDs) != 0 || !locatedIn(m, "[Gmail]/All Mail") || w.orphaned(a, "lease@example.com") {
		t.Fatalf("archived at Gmail: folders %v, location %s", m.FolderIDs, m.Location)
	}
	w.ageOrphans(a)
	a = w.sync(a)
	if w.count(`SELECT count(*) FROM collab_messages WHERE account_id = $1`, a.ID) != 1 || w.comments(tid) != 1 {
		t.Fatal("archived mail or its comments went")
	}
	var page struct {
		Data []MailThread `json:"data"`
	}
	if st := doJSON(t, "GET", w.api(w.agent).URL+"/v1/collab/threads?box=all", nil, &page); st != 200 || len(page.Data) != 1 {
		t.Fatalf("archived conversation in All: %d %+v", st, page.Data)
	}

	// Deleted for good at Gmail: missed, then removed after the grace.
	srv.expunge("[Gmail]/All Mail", "lease@example.com")
	a = w.sync(a)
	if !w.orphaned(a, "lease@example.com") {
		t.Fatal("deleted mail is not an orphan")
	}
	w.ageOrphans(a)
	a = w.sync(a)
	if n := w.count(`SELECT count(*) FROM collab_messages WHERE account_id = $1`, a.ID); n != 0 {
		t.Fatalf("%d messages after the grace", n)
	}
	// The conversation is set aside with its comment, and listed nowhere.
	if n := w.count(`SELECT count(*) FROM collab_threads WHERE id = $1 AND emptied_at IS NOT NULL`, tid); n != 1 || w.comments(tid) != 1 {
		t.Fatal("the emptied conversation lost its comments")
	}
	if st := doJSON(t, "GET", w.api(w.agent).URL+"/v1/collab/threads?box=all", nil, &page); st != 200 || len(page.Data) != 0 {
		t.Fatalf("an emptied conversation is listed: %+v", page.Data)
	}
	if st := doJSON(t, "GET", w.api(w.agent).URL+"/v1/collab/threads/"+tid.String(), nil, nil); st != 404 {
		t.Fatalf("emptied conversation detail: %d", st)
	}

	// The same mail coming back joins it again.
	srv.append("INBOX", raw, at)
	a = w.sync(a)
	if m := w.messageByRemote(a, "lease@example.com"); m.ThreadID != tid {
		t.Fatal("returning mail started another conversation")
	}
	if n := w.count(`SELECT count(*) FROM collab_threads WHERE id = $1 AND emptied_at IS NULL AND message_count = 1`, tid); n != 1 {
		t.Fatal("the conversation did not come back")
	}
}

// TestIMAPUIDValidityResetKeepsOlderMail: a folder whose UIDVALIDITY changed keeps its mail,
// older than the first sync's window included, found again by Message-ID.
func TestIMAPUIDValidityResetKeepsOlderMail(t *testing.T) {
	w := newWorld(t)
	a, srv := imapAccount(t, w, "Projects")
	old := time.Now().Add(-90 * 24 * time.Hour)
	plan := testMail{from: "pm@example.com", to: a.Email, subject: "Plan", msgID: "plan@example.com", body: "The plan", date: old}.raw()
	srv.append("Projects", plan, old)
	a = w.sync(a)
	w.rewindFolder(a, "Projects")
	a = w.sync(a)
	m := w.messageByRemote(a, "plan@example.com")
	w.comment(m.ThreadID, "Keep this")

	// The server rebuilt the folder: the same mail under new UIDs and a new UIDVALIDITY.
	if err := srv.c.Delete("Projects").Wait(); err != nil {
		t.Fatal(err)
	}
	srv.create("Projects")
	srv.append("Projects", testMail{from: "pm@example.com", to: a.Email, subject: "Update", msgID: "update@example.com", body: "News"}.raw(), time.Now())
	srv.append("Projects", plan, old)
	a = w.sync(a)
	uids, _ := srv.find("Projects", "plan@example.com")
	sel, err := srv.c.Select("Projects", nil).Wait()
	if err != nil || len(uids) != 1 {
		t.Fatalf("server copy: %v %v", uids, err)
	}
	m = w.messageByRemote(a, "plan@example.com")
	locs := parseLocation(m.Location)
	if len(locs) != 1 || locs[0].Validity != sel.UIDValidity || locs[0].UID != uint32(uids[0]) || w.orphaned(a, "plan@example.com") {
		t.Fatalf("after the reset: %s (want validity %d uid %d)", m.Location, sel.UIDValidity, uids[0])
	}
	if w.comments(m.ThreadID) != 1 {
		t.Fatal("the comment went")
	}
}

// TestIMAPMoveToANewFolderKeepsOlderMail: older mail moved into a folder created after the
// connect is found there, though that folder's first sync reads only recent mail.
func TestIMAPMoveToANewFolderKeepsOlderMail(t *testing.T) {
	w := newWorld(t)
	a, srv := imapAccount(t, w)
	old := time.Now().Add(-400 * 24 * time.Hour)
	srv.append("INBOX", testMail{from: "acme@example.com", to: a.Email, subject: "Contract", msgID: "contract@example.com", body: "2025", date: old}.raw(), old)
	a = w.sync(a)
	w.rewindFolder(a, "INBOX")
	a = w.sync(a)
	tid := w.messageByRemote(a, "contract@example.com").ThreadID
	w.comment(tid, "Renewal due")

	srv.create("Clients")
	uids, _ := srv.find("INBOX", "contract@example.com")
	if _, err := srv.c.Move(imap.UIDSetNum(uids...), "Clients").Wait(); err != nil {
		t.Fatal(err)
	}
	a = w.sync(a)
	clients := w.folderByRemote(a, "Clients")
	m := w.messageByRemote(a, "contract@example.com")
	if len(m.FolderIDs) != 1 || m.FolderIDs[0] != clients.ID || !locatedIn(m, "Clients") || w.orphaned(a, "contract@example.com") {
		t.Fatalf("moved to a new folder: %v %s", m.FolderIDs, m.Location)
	}
	if m.ThreadID != tid || w.comments(tid) != 1 {
		t.Fatal("the conversation changed")
	}
}

// TestIMAPMoveDuringAPassKeepsTheMessage: a message moved to a folder the pass already read
// is found there instead of being removed.
func TestIMAPMoveDuringAPassKeepsTheMessage(t *testing.T) {
	w := newWorld(t)
	a, srv := imapAccount(t, w, "Archive", "Projects")
	srv.append("Projects", testMail{from: "pm@example.com", to: a.Email, subject: "Plan", msgID: "race@example.com", body: "Plan"}.raw(), time.Now().Add(-time.Hour))
	a = w.sync(a)
	tid := w.messageByRemote(a, "race@example.com").ThreadID
	w.comment(tid, "Watch this")

	uids, _ := srv.find("Projects", "race@example.com")
	if _, err := srv.c.Move(imap.UIDSetNum(uids...), "Archive").Wait(); err != nil {
		t.Fatal(err)
	}
	// Archive was read before the move: its cursor is already past the moved copy.
	moved, _ := srv.find("Archive", "race@example.com")
	mustExec(t, w.d, `UPDATE collab_folders SET sync_state = jsonb_set(sync_state, '{u}', to_jsonb($2::bigint)) WHERE account_id = $1 AND remote_id = 'Archive'`,
		a.ID, int64(moved[0]))
	a = w.sync(a)
	m := w.messageByRemote(a, "race@example.com")
	if !locatedIn(m, "Archive") || locatedIn(m, "Projects") || w.orphaned(a, "race@example.com") || w.comments(tid) != 1 {
		t.Fatalf("moved during the pass: %s", m.Location)
	}
}

// TestIMAPSentMailWithoutASentCopyStays: mail sent from opendesk that the server keeps no
// copy of (no Sent folder) is never an orphan.
func TestIMAPSentMailWithoutASentCopyStays(t *testing.T) {
	w := newWorld(t)
	a, _ := imapAccount(t, w)
	a = w.sync(a)
	if n := w.count(`SELECT count(*) FROM collab_folders WHERE account_id = $1 AND role = 'sent'`, a.ID); n != 0 {
		t.Fatal("the server has no Sent folder")
	}
	prov, err := w.s.provider(w.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	raw := testMail{from: a.Email, to: "frank@example.com", subject: "Hello", msgID: "out@acme.test", body: "Hi"}.raw()
	ref, err := prov.Send(w.ctx, raw, SendInfo{From: a.Email, Recipients: []string{"frank@example.com"}})
	prov.Close()
	if err != nil || len(parseLocation(ref.Location)) != 0 {
		t.Fatalf("send: %v %+v", err, ref)
	}
	if _, err := w.s.storeSent(w.ctx, a, raw, ref, w.agent.UserID, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		a = w.sync(a)
		w.ageOrphans(a)
	}
	a = w.sync(a)
	if n := w.count(`SELECT count(*) FROM collab_messages WHERE account_id = $1 AND remote_id = 'out@acme.test' AND orphaned_at IS NULL`, a.ID); n != 1 {
		t.Fatal("sent mail without a server copy went")
	}
}

// TestIMAPApplyChecksUIDValidity: an action never touches a UID read under another
// UIDVALIDITY (it may name another message now).
func TestIMAPApplyChecksUIDValidity(t *testing.T) {
	w := newWorld(t)
	a, srv := imapAccount(t, w, "Projects")
	one := testMail{from: "a@example.com", to: a.Email, subject: "One", msgID: "one@example.com", body: "One"}.raw()
	srv.append("Projects", one, time.Now().Add(-time.Hour))
	a = w.sync(a)
	tid := w.messageByRemote(a, "one@example.com").ThreadID

	if err := srv.c.Delete("Projects").Wait(); err != nil {
		t.Fatal(err)
	}
	srv.create("Projects")
	srv.append("Projects", testMail{from: "b@example.com", to: a.Email, subject: "Two", msgID: "two@example.com", body: "Two"}.raw(), time.Now())
	srv.append("Projects", one, time.Now().Add(-time.Hour))
	if err := w.s.applyAtProvider(w.ctx, a, jobs.CollabApplyArgs{WorkspaceID: a.WorkspaceID, AccountID: a.ID, ThreadIDs: []uuid.UUID{tid}, Action: ActionStar}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one@example.com", "two@example.com"} {
		if _, flags := srv.find("Projects", id); len(flags) != 1 || slices.Contains(flags[0], imap.FlagFlagged) {
			t.Fatalf("%s flagged through a stale UID: %v", id, flags)
		}
	}
}

// TestIMAPReplyBeforeItsParentJoinsOneConversation: a reply stored before the message it
// answers (a history import reads newest first) ends up in that message's conversation.
func TestIMAPReplyBeforeItsParentJoinsOneConversation(t *testing.T) {
	w := newWorld(t)
	a, srv := imapAccount(t, w)
	srv.append("INBOX", testMail{from: "c@example.com", to: a.Email, subject: "Re: Plan", msgID: "c@example.com", inReplyTo: "b@example.com", body: "C"}.raw(),
		time.Now().Add(-time.Hour))
	a = w.sync(a)
	w.comment(w.messageByRemote(a, "c@example.com").ThreadID, "About C")
	srv.append("INBOX", testMail{from: "b@example.com", to: a.Email, subject: "Re: Plan", msgID: "b@example.com", inReplyTo: "a@example.com",
		refs: "<a@example.com>", body: "B"}.raw(), time.Now().Add(-2*time.Hour))
	a = w.sync(a)
	b, c := w.messageByRemote(a, "b@example.com"), w.messageByRemote(a, "c@example.com")
	if b.ThreadID != c.ThreadID {
		t.Fatal("the reply stayed in a conversation of its own")
	}
	if n := w.count(`SELECT count(*) FROM collab_threads WHERE account_id = $1`, a.ID); n != 1 || w.comments(b.ThreadID) != 1 {
		t.Fatalf("%d conversations, comments %d", n, w.comments(b.ThreadID))
	}
}

// TestGraphOrphanFoundInAnotherFolder: a Microsoft message that left a folder, but that no
// other folder's delta lists (its round started over), is found where Microsoft says it is;
// one Microsoft no longer has is removed at once.
func TestGraphOrphanFoundInAnotherFolder(t *testing.T) {
	w := newWorld(t)
	g := newFakeGraph(t)
	w.s.graphBase = g.srv.URL
	a := w.account(w.agent, ProviderMicrosoft, VisibilityShared, Audience{Everyone: true})
	g.add("old1", "cv1", "f-inbox", true, testMail{from: "Frank <frank@example.com>", to: a.Email, subject: "Order", msgID: "o1@example.com", body: "Order"})
	a = w.sync(a)
	tid := w.messageByRemote(a, "old1").ThreadID
	w.comment(tid, "Chase this")

	g.mu.Lock()
	g.msgs["old1"].folder = "f-proj"
	g.changes["f-inbox"] = append(g.changes["f-inbox"], map[string]any{"id": "old1", "@removed": map[string]string{"reason": "changed"}})
	g.mu.Unlock()
	a = w.sync(a)
	proj := w.folderByRemote(a, "f-proj")
	if m := w.messageByRemote(a, "old1"); len(m.FolderIDs) != 1 || m.FolderIDs[0] != proj.ID || w.orphaned(a, "old1") {
		t.Fatalf("relocated: %v", m.FolderIDs)
	}

	g.remove("old1")
	a = w.sync(a)
	if n := w.count(`SELECT count(*) FROM collab_messages WHERE account_id = $1 AND remote_id = 'old1'`, a.ID); n != 0 {
		t.Fatal("a message Microsoft no longer has is kept")
	}
	if n := w.count(`SELECT count(*) FROM collab_threads WHERE id = $1 AND emptied_at IS NOT NULL`, tid); n != 1 || w.comments(tid) != 1 {
		t.Fatal("the conversation's comment went with its mail")
	}
}
