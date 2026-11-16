package email

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/useopendesk/opendesk/internal/crypto"
	"github.com/useopendesk/opendesk/internal/domain"
)

// receiveDirect takes in mail as the built-in SMTP receiver does: straight from a sending
// server, so no authentication result in it is believed.
func (w *world) receiveDirect(m *mailboxRow, raw string) msgRow {
	w.t.Helper()
	raw = strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\n", "\r\n")
	id, _, err := w.s.ingest(w.ctx, m, []byte(raw), w.forwardAddr(m, ""), nil, viaDirect)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.s.processInbound(w.ctx, w.wid, id); err != nil {
		w.t.Fatal(err)
	}
	return w.messageFor(id)
}

// TestDirectMailMatchedByTicketNumberIsUnverified: mail the built-in receiver took straight
// from a sending server, matched to a ticket by its number alone and the contact's From
// address, is flagged and changes nothing about the ticket; mail answering one of the
// ticket's own messages is not.
func TestDirectMailMatchedByTicketNumberIsUnverified(t *testing.T) {
	w := newWorld(t)
	m := w.mailbox(InboundForwarding, OutboundSystem, func(m *mailboxRow, _ *credentials) { m.AIAnswers = true })
	from := customer()
	tk, ours := firstTicket(t, w, m, from)
	mustExec(t, w.d, `UPDATE tickets SET status = 'solved', solved_at = now() WHERE id = $1`, tk.ID)
	triggers := w.jobCount("helpdesk.ticket_trigger", "", "")

	forged := "From: " + from + "\r\nTo: " + m.Email + "\r\nSubject: Re: Printer [#" + strconv.FormatInt(tk.Number, 10) +
		"]\r\nMessage-ID: <fz" + crypto.RandomString(8) + "@spoof.test>\r\n\r\nWire the refund to this account.\r\n"
	msg := w.receiveDirect(m, forged)
	if msg.TicketID != tk.ID {
		t.Fatal("the message should stay on the ticket its tag names")
	}
	var authFailed bool
	if err := w.d.DB.Pool.QueryRow(w.ctx, `SELECT auth_failed FROM messages WHERE id = $1`, msg.ID).Scan(&authFailed); err != nil {
		t.Fatal(err)
	}
	if !authFailed {
		t.Fatal("a ticket-number match from the built-in receiver is not flagged")
	}
	if got := w.ticket(tk.ID); got.Status != domain.StatusSolved || got.Spam {
		t.Fatalf("ticket status %s spam %v: only the message is flagged", got.Status, got.Spam)
	}
	if n := w.jobCount("helpdesk.ticket_trigger", "", ""); n != triggers {
		t.Fatal("automations queued for an unverified sender")
	}
	if n := w.jobCount("ai.email_answer", "", ""); n != 0 {
		t.Fatal("Ollie queued for an unverified sender")
	}

	// A reply naming one of the ticket's own messages is matched by more than a number.
	reply := "From: " + from + "\r\nTo: " + m.Email + "\r\nSubject: Re: Printer\r\nIn-Reply-To: " + ours +
		"\r\nMessage-ID: <ok" + crypto.RandomString(8) + "@mail.customer.test>\r\n\r\nThanks, that worked.\r\n"
	msg = w.receiveDirect(m, reply)
	if err := w.d.DB.Pool.QueryRow(w.ctx, `SELECT auth_failed FROM messages WHERE id = $1`, msg.ID).Scan(&authFailed); err != nil {
		t.Fatal(err)
	}
	if msg.TicketID != tk.ID || authFailed {
		t.Fatalf("a threaded reply: ticket %v flagged %v", msg.TicketID, authFailed)
	}
}

// TestForgedBounceNamingAnotherRecipient: a delivery report counts only for a message sent to
// a recipient it names.
func TestForgedBounceNamingAnotherRecipient(t *testing.T) {
	w := newWorld(t)
	m := w.mailbox(InboundForwarding, OutboundSystem, nil)
	from := customer()
	_, ours := firstTicket(t, w, m, from)
	mustExec(t, w.d, `UPDATE messages SET to_emails = ARRAY[$2::text] WHERE email_message_id = $1`, ours, from)
	status := func() string {
		var s string
		if err := w.d.DB.Pool.QueryRow(w.ctx, `SELECT delivery_status FROM messages WHERE email_message_id = $1`, ours).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// The fixture's report names nobody@customer.test, who the reply never went to.
	w.receive(m, fixture(t, "bounce.eml", "{{OUR}}", ours, "{{ID}}", crypto.RandomString(8)))
	if status() != "sent" {
		t.Fatal("a report naming another recipient marked the reply bounced")
	}
	w.receive(m, strings.ReplaceAll(fixture(t, "bounce.eml", "{{OUR}}", ours, "{{ID}}", crypto.RandomString(8)), "nobody@customer.test", from))
	if status() != "bounced" {
		t.Fatal("a report naming the reply's recipient was not recorded")
	}
}

// TestRestartWindowStopsAtANewMailboxStart: a mailbox that reads new mail only never reads
// back past its creation when the provider drops its cursor.
func TestRestartWindowStopsAtANewMailboxStart(t *testing.T) {
	now := time.Now()
	m := &mailboxRow{CreatedAt: now.Add(-2 * time.Hour)}
	if got := restartWindow(m, now); got != initialWindow {
		t.Fatalf("recent: %s", got)
	}
	m.Config.SyncStart = SyncStartNew
	if got := restartWindow(m, now); got != 2*time.Hour+restartSlack {
		t.Fatalf("new only: %s", got)
	}
	m.CreatedAt = now.Add(-30 * 24 * time.Hour)
	if got := restartWindow(m, now); got != initialWindow {
		t.Fatalf("an older mailbox: %s", got)
	}
}
