package tickets_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/helpdesk/hd"
	"github.com/useopendesk/opendesk/internal/helpdesk/hdtest"
	"github.com/useopendesk/opendesk/internal/helpdesk/tickets"
	"github.com/useopendesk/opendesk/internal/jobs"
)

func near(a, b time.Time) bool {
	d := a.Sub(b)
	return d > -2*time.Minute && d < 2*time.Minute
}

func scan(t *testing.T, e *hdtest.Env) {
	t.Helper()
	if err := tickets.ScanSLA(e.Ctx, e.D); err != nil {
		t.Fatal(err)
	}
}

// A solved ticket the customer opens again starts a new SLA cycle from their message: a
// reply target and a resolution target from then, nothing of the last cycle's deadlines, and
// no breach of them.
func TestSLAReopenStartsNewCycle(t *testing.T) {
	e, ws := newEnv(t)
	addSLA(t, e, ws, nil, 30, 120, 0, true)
	mb := addMailbox(t, e, ws)
	tk := createTicket(t, e, ws, map[string]any{"channel": "email", "mailbox_id": mb.String(), "kind": "note",
		"contact": map[string]any{"email": "customer@example.com"}})
	runTriggers(t, e, ws.ID)
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Agent.As(ws), map[string]any{"status": "solved"}).Want(t, 200)
	runTriggers(t, e, ws.ID)
	// Days later, long past the first cycle's deadlines.
	e.Exec(`UPDATE tickets SET first_responded_at = now() - interval '3 days', first_response_due_at = now() - interval '3 days',
		resolution_due_at = now() - interval '2 days', solved_at = now() - interval '2 days' WHERE id = $1`, tk.ID)

	m := inbound(t, e, ws, tk.ID, "Thanks, one more question")
	runTriggers(t, e, ws.ID)
	scan(t, e)
	got := getTicket(t, e, ws, tk.ID)
	if got.Status != "open" || got.SLA == nil || got.SLA.Breached || !got.SLA.ReplyPending {
		t.Fatalf("reopened sla %+v", got.SLA)
	}
	if got.SLA.StartedAt == nil || !near(*got.SLA.StartedAt, m.CreatedAt) ||
		!near(*got.SLA.FirstResponseDueAt, m.CreatedAt.Add(30*time.Minute)) || !near(*got.SLA.ResolutionDueAt, m.CreatedAt.Add(120*time.Minute)) {
		t.Fatalf("new cycle targets %+v, message at %s", got.SLA, m.CreatedAt)
	}
	if n := countRows(t, e, `SELECT count(*) FROM activities WHERE ticket_id = $1 AND type = 'sla_breached'`, tk.ID); n != 0 {
		t.Fatalf("a reopen breached the last cycle's targets (%d)", n)
	}

	// The reply target of the new cycle is met by the team's next reply...
	e.Do("POST", "/v1/tickets/"+tk.ID.String()+"/messages", ws.Agent.As(ws), map[string]any{"kind": "note", "body_html": "<p>Internal</p>"}).Want(t, 201)
	if !getTicket(t, e, ws, tk.ID).SLA.ReplyPending {
		t.Fatal("a note met the reply target")
	}
	e.Exec(`UPDATE tickets SET sla_responded_at = now() WHERE id = $1`, tk.ID) // as a public reply records it
	if getTicket(t, e, ws, tk.ID).SLA.ReplyPending {
		t.Fatal("reply still pending after the team replied")
	}

	// ...and missed when it passes first.
	e.Exec(`UPDATE tickets SET sla_responded_at = NULL, first_response_due_at = now() - interval '1 minute' WHERE id = $1`, tk.ID)
	scan(t, e)
	if n := countRows(t, e, `SELECT count(*) FROM activities WHERE ticket_id = $1 AND type = 'sla_breached' AND data->>'kind' = 'first_response'`, tk.ID); n != 1 {
		t.Fatalf("reply target breach of the new cycle %d", n)
	}

	// A person reopening a solved ticket starts the resolution target only.
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Agent.As(ws), map[string]any{"status": "solved"}).Want(t, 200)
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Agent.As(ws), map[string]any{"status": "open"}).Want(t, 200)
	got = getTicket(t, e, ws, tk.ID)
	if got.SLA == nil || got.SLA.ReplyPending || got.SLA.Breached || !near(*got.SLA.ResolutionDueAt, time.Now().Add(120*time.Minute)) {
		t.Fatalf("agent reopen sla %+v", got.SLA)
	}
}

// The resolution clock stands still while the ticket is pending or on hold, and the time it
// waited moves the resolution target when it runs again, whoever reopens it.
func TestSLAPausesWhilePending(t *testing.T) {
	e, ws := newEnv(t)
	addSLA(t, e, ws, nil, 30, 120, 0, true)
	mb := addMailbox(t, e, ws)
	tk := createTicket(t, e, ws, map[string]any{"channel": "email", "mailbox_id": mb.String(), "kind": "note",
		"contact": map[string]any{"email": "customer@example.com"}})
	runTriggers(t, e, ws.ID)
	e.Exec(`UPDATE tickets SET first_responded_at = now() WHERE id = $1`, tk.ID)

	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Agent.As(ws), map[string]any{"status": "pending"}).Want(t, 200)
	got := getTicket(t, e, ws, tk.ID)
	if got.SLA == nil || got.SLA.PausedAt == nil {
		t.Fatalf("pending did not pause %+v", got.SLA)
	}
	// Waiting a day on the customer: its target passed meanwhile, which is no breach.
	e.Exec(`UPDATE tickets SET sla_paused_at = now() - interval '60 minutes', resolution_due_at = now() - interval '10 minutes' WHERE id = $1`, tk.ID)
	scan(t, e)
	if n := countRows(t, e, `SELECT count(*) FROM activities WHERE ticket_id = $1 AND type = 'sla_breached'`, tk.ID); n != 0 {
		t.Fatalf("breached while paused (%d)", n)
	}
	if getTicket(t, e, ws, tk.ID).SLA.Breached {
		t.Fatal("shown as breached while paused")
	}

	// An agent opens it again: the target moves by the hour it waited.
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Agent.As(ws), map[string]any{"status": "open"}).Want(t, 200)
	got = getTicket(t, e, ws, tk.ID)
	if got.SLA.PausedAt != nil || !near(*got.SLA.ResolutionDueAt, time.Now().Add(50*time.Minute)) || got.SLA.Breached {
		t.Fatalf("resumed sla %+v", got.SLA)
	}
	var paused int
	e.Scalar(`SELECT sla_paused_minutes FROM tickets WHERE id = $1`, &paused, tk.ID)
	if paused < 59 || paused > 61 {
		t.Fatalf("paused minutes %d", paused)
	}

	// On hold, then the customer's reply opens it: the trigger runs the clock again.
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Agent.As(ws), map[string]any{"status": "on_hold"}).Want(t, 200)
	before := *getTicket(t, e, ws, tk.ID).SLA.ResolutionDueAt
	e.Exec(`UPDATE tickets SET sla_paused_at = now() - interval '30 minutes' WHERE id = $1`, tk.ID)
	inbound(t, e, ws, tk.ID, "Here are the logs")
	runTriggers(t, e, ws.ID)
	got = getTicket(t, e, ws, tk.ID)
	if got.Status != "open" || got.SLA.PausedAt != nil || !near(*got.SLA.ResolutionDueAt, before.Add(30*time.Minute)) {
		t.Fatalf("resumed by the customer %+v (was due %s)", got.SLA, before)
	}

	// A priority change recomputes from the cycle's start plus the time it stood still.
	e.Exec(`UPDATE tickets SET sla_paused_minutes = 90 WHERE id = $1`, tk.ID)
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Agent.As(ws), map[string]any{"priority": "high"}).Want(t, 200)
	runTriggers(t, e, ws.ID)
	got = getTicket(t, e, ws, tk.ID)
	if !near(*got.SLA.ResolutionDueAt, got.CreatedAt.Add(210*time.Minute)) {
		t.Fatalf("recomputed resolution %s, created %s", got.SLA.ResolutionDueAt, got.CreatedAt)
	}
}

// A ticket solved after its target passed is breached at once, before the scan reaches it,
// without paging anyone; the breach stays on the solved ticket and through a priority change.
func TestSLABreachRecordedAtSolve(t *testing.T) {
	e, ws := newEnv(t)
	addSLA(t, e, ws, nil, 30, 120, 0, true)
	tk := createTicket(t, e, ws, map[string]any{"kind": "note"})
	runTriggers(t, e, ws.ID)
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Admin.As(ws), map[string]any{"assignee_id": ws.Agent.ID.String()}).Want(t, 200)
	runTriggers(t, e, ws.ID)
	e.Exec(`UPDATE tickets SET resolution_due_at = now() - interval '1 minute', first_responded_at = now() WHERE id = $1`, tk.ID)

	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Admin.As(ws), map[string]any{"status": "solved"}).Want(t, 200)
	if n := countRows(t, e, `SELECT count(*) FROM activities WHERE ticket_id = $1 AND type = 'sla_breached' AND data->>'kind' = 'resolution'`, tk.ID); n != 1 {
		t.Fatalf("breach at solve %d", n)
	}
	if n := countRows(t, e, `SELECT count(*) FROM notifications WHERE ticket_id = $1 AND type = 'sla_breached'`, tk.ID); n != 0 {
		t.Fatalf("a solve paged the assignee (%d)", n)
	}
	got := getTicket(t, e, ws, tk.ID)
	if got.SLA == nil || !got.SLA.Breached || got.SLA.ResolutionBreachedAt == nil {
		t.Fatalf("solved ticket sla %+v", got.SLA)
	}
	scan(t, e)
	if n := countRows(t, e, `SELECT count(*) FROM activities WHERE ticket_id = $1 AND type = 'sla_breached'`, tk.ID); n != 1 {
		t.Fatalf("breach recorded twice (%d)", n)
	}
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), ws.Admin.As(ws), map[string]any{"priority": "low"}).Want(t, 200)
	runTriggers(t, e, ws.ID)
	if got := getTicket(t, e, ws, tk.ID); !got.SLA.Breached {
		t.Fatalf("a priority change took the breach back %+v", got.SLA)
	}
	// A ticket solved within its targets records nothing.
	ok := createTicket(t, e, ws, map[string]any{"kind": "note"})
	runTriggers(t, e, ws.ID)
	e.Do("PATCH", "/v1/tickets/"+ok.ID.String(), ws.Admin.As(ws), map[string]any{"status": "solved"}).Want(t, 200)
	if got := getTicket(t, e, ws, ok.ID); got.SLA == nil || got.SLA.Breached {
		t.Fatalf("met ticket %+v", got.SLA)
	}
}

// ticket_updated automations run when a field of their conditions changed, not on every
// edit; changed holds for the field the update changed.
func TestTicketUpdatedAutomationsRunOnTheirFields(t *testing.T) {
	e, ws := newEnv(t)
	solved := addAutomation(t, e, ws, jobs.TriggerTicketUpdated, 0,
		[]map[string]string{{"field": "status", "op": "equals", "value": "solved"}},
		[]map[string]string{{"type": "add_note", "value": "<p>Solved</p>"}})
	moved := addAutomation(t, e, ws, jobs.TriggerTicketUpdated, 1,
		[]map[string]string{{"field": "assignee_id", "op": "changed", "value": ""}},
		[]map[string]string{{"type": "add_tag", "value": "reassigned"}})
	runs := func(id uuid.UUID) int {
		return countRows(t, e, `SELECT run_count FROM automations WHERE id = $1`, id)
	}
	tk := createTicket(t, e, ws, map[string]any{"kind": "note"})
	runTriggers(t, e, ws.ID)
	agent := ws.Agent.As(ws)
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), agent, map[string]any{"status": "solved"}).Want(t, 200)
	runTriggers(t, e, ws.ID)
	for _, b := range []map[string]any{{"priority": "high"}, {"severity": "sev2"}, {"subject": "Renamed"}} {
		e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), agent, b).Want(t, 200)
		runTriggers(t, e, ws.ID)
	}
	if runs(solved) != 1 || runs(moved) != 0 {
		t.Fatalf("runs solved=%d moved=%d", runs(solved), runs(moved))
	}
	if n := countRows(t, e, `SELECT count(*) FROM messages WHERE ticket_id = $1 AND author_type = 'system' AND kind = 'note'`, tk.ID); n != 1 {
		t.Fatalf("notes %d", n)
	}
	e.Do("PATCH", "/v1/tickets/"+tk.ID.String(), agent, map[string]any{"assignee_id": ws.Admin.ID.String()}).Want(t, 200)
	runTriggers(t, e, ws.ID)
	if runs(moved) != 1 || runs(solved) != 1 {
		t.Fatalf("after reassigning: solved=%d moved=%d", runs(solved), runs(moved))
	}

	// Saving: a ticket_updated automation needs a condition on a field an update changes, and
	// changed is for ticket_updated only.
	admin := ws.Admin.As(ws)
	act := []map[string]any{{"type": "add_tag", "value": "x"}}
	e.Do("POST", "/v1/automations", admin, map[string]any{"name": "Chats", "trigger": "ticket_updated",
		"conditions": []map[string]any{{"field": "channel", "op": "equals", "value": "chat"}}, "actions": act}).Want(t, 422)
	e.Do("POST", "/v1/automations", admin, map[string]any{"name": "New", "trigger": "ticket_created",
		"conditions": []map[string]any{{"field": "priority", "op": "changed"}}, "actions": act}).Want(t, 422)
	e.Do("POST", "/v1/automations", admin, map[string]any{"name": "Contact", "trigger": "ticket_updated",
		"conditions": []map[string]any{{"field": "body", "op": "changed"}}, "actions": act}).Want(t, 422)
	var a struct {
		Conditions []map[string]any `json:"conditions"`
	}
	e.Do("POST", "/v1/automations", admin, map[string]any{"name": "Chats raised", "trigger": "ticket_updated",
		"conditions": []map[string]any{{"field": "channel", "op": "equals", "value": "chat"}, {"field": "priority", "op": "changed", "value": "ignored"}},
		"actions":    act}).Want(t, 201).JSON(t, &a)
	if len(a.Conditions) != 2 || a.Conditions[1]["op"] != "changed" || a.Conditions[1]["value"] != "" {
		t.Fatalf("stored conditions %+v", a.Conditions)
	}
	e.Do("POST", "/v1/automations", admin, map[string]any{"name": "Every update", "trigger": "ticket_updated",
		"conditions": []map[string]any{}, "actions": act}).Want(t, 201)
}

// Automation replies in a chat count toward the same per-ticket cap as by email.
func TestAutomationChatRepliesCapped(t *testing.T) {
	e, ws := newEnv(t)
	tk := createTicket(t, e, ws, map[string]any{"kind": "note", "contact": map[string]any{"email": "visitor@example.com", "name": "Vi"}})
	runTriggers(t, e, ws.ID)
	e.Exec(`UPDATE tickets SET channel = 'chat' WHERE id = $1`, tk.ID)
	addAutomation(t, e, ws, jobs.TriggerCustomerReplied, 0, []map[string]string{},
		[]map[string]string{{"type": "send_reply", "value": "<p>We will be right with you.</p>"}})
	for i := 0; i < 5; i++ {
		inbound(t, e, ws, tk.ID, "Hello?")
		runTriggers(t, e, ws.ID)
	}
	if n := countRows(t, e, `SELECT count(*) FROM messages WHERE ticket_id = $1 AND author_type = 'system' AND kind = 'reply'`, tk.ID); n != 3 {
		t.Fatalf("chat automation replies %d, want 3", n)
	}
}

// ai_decide and ai_triage ask the model outside the trigger transaction: neither the ticket
// row nor the trigger lock is held while it answers, and each is asked once per run.
func TestAutomationModelCallsHoldNoLock(t *testing.T) {
	e, ws := newEnv(t)
	addAutomation(t, e, ws, jobs.TriggerTicketCreated, 0,
		[]map[string]string{{"field": "ai_decide", "op": "gt", "value": "0.5", "question": "Is it urgent?"}},
		[]map[string]string{{"type": "ai_triage", "value": ""}, {"type": "add_tag", "value": "decided"}})
	chained := addAutomation(t, e, ws, jobs.TriggerTicketUpdated, 0,
		[]map[string]string{{"field": "priority", "op": "equals", "value": "urgent"}},
		[]map[string]string{{"type": "add_tag", "value": "triaged-urgent"}})
	tk := createTicket(t, e, ws, map[string]any{"kind": "note"})

	free := func(ctx context.Context, what string) {
		// Another connection takes the ticket row and the trigger lock at once.
		err := e.D.DB.Tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
				return err
			}
			var got bool
			if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('ticket_trigger:' || $1::text, 0))`, tk.ID).Scan(&got); err != nil {
				return err
			}
			if !got {
				t.Errorf("%s: the trigger lock was held while the model answered", what)
			}
			_, err := tx.Exec(ctx, `SELECT 1 FROM tickets WHERE id = $1 FOR UPDATE NOWAIT`, tk.ID)
			return err
		})
		if err != nil {
			t.Errorf("%s: the ticket was locked while the model answered: %v", what, err)
		}
	}
	decides, prepares := 0, 0
	prev := hd.GetAIHooks()
	defer hd.SetAIHooks(prev)
	hd.SetAIHooks(&hd.AIHooks{
		Decide: func(ctx context.Context, _ db.Querier, wid, tid uuid.UUID, q []string) (map[string]float64, error) {
			decides++
			free(ctx, "ai_decide")
			return map[string]float64{"Is it urgent?": 0.9}, nil
		},
		TriagePrepare: func(ctx context.Context, _ db.Querier, wid, tid uuid.UUID) (any, error) {
			prepares++
			free(ctx, "ai_triage")
			return "urgent", nil
		},
		TriageApply: func(ctx context.Context, tx pgx.Tx, wid, tid uuid.UUID, result any) ([]string, error) {
			_, err := tx.Exec(ctx, `UPDATE tickets SET priority = $2 WHERE id = $1`, tid, result)
			return []string{"priority"}, err
		},
	})
	runTriggers(t, e, ws.ID)
	got := getTicket(t, e, ws, tk.ID)
	if got.Priority != "urgent" || len(got.Tags) != 2 {
		t.Fatalf("ticket %s %+v", got.Priority, got.Tags)
	}
	if decides != 1 || prepares != 1 {
		t.Fatalf("model asked decide=%d triage=%d times", decides, prepares)
	}
	if n := countRows(t, e, `SELECT run_count FROM automations WHERE id = $1`, chained); n != 1 {
		t.Fatalf("chained runs %d", n)
	}
}

// A ticket auto-assignment found nobody for is assigned once someone is eligible, by the
// minute job; one assigned meanwhile, or whose rule is none, is left alone.
func TestAssignBacklog(t *testing.T) {
	e, ws := newEnv(t)
	e.SetSettings(ws, map[string]any{"auto_assign": "round_robin"})
	e.Exec(`UPDATE members SET availability = 'offline' WHERE workspace_id = $1`, ws.ID)
	waiting := createTicket(t, e, ws, map[string]any{"kind": "note"})
	taken := createTicket(t, e, ws, map[string]any{"kind": "note"})
	runTriggers(t, e, ws.ID)
	var pending bool
	e.Scalar(`SELECT assign_pending_since IS NOT NULL FROM tickets WHERE id = $1`, &pending, waiting.ID)
	if !pending || getTicket(t, e, ws, waiting.ID).Assignee != nil {
		t.Fatal("not waiting for an eligible member")
	}
	if err := tickets.AssignBacklog(e.Ctx, e.D); err != nil {
		t.Fatal(err)
	}
	if getTicket(t, e, ws, waiting.ID).Assignee != nil {
		t.Fatal("assigned with nobody eligible")
	}
	e.Do("PATCH", "/v1/tickets/"+taken.ID.String(), ws.Admin.As(ws), map[string]any{"assignee_id": ws.Owner.ID.String()}).Want(t, 200)
	e.Scalar(`SELECT assign_pending_since IS NOT NULL FROM tickets WHERE id = $1`, &pending, taken.ID)
	if pending {
		t.Fatal("an assignment left the ticket waiting")
	}

	// The agent comes online: the waiting ticket is theirs.
	e.Exec(`UPDATE members SET availability = 'online' WHERE workspace_id = $1 AND user_id = $2`, ws.ID, ws.Agent.ID)
	if err := tickets.AssignBacklog(e.Ctx, e.D); err != nil {
		t.Fatal(err)
	}
	if a := getTicket(t, e, ws, waiting.ID).Assignee; a == nil || a.ID != ws.Agent.ID {
		t.Fatalf("backlog assignee %+v", a)
	}
	if a := getTicket(t, e, ws, taken.ID).Assignee; a == nil || a.ID != ws.Owner.ID {
		t.Fatalf("an assigned ticket changed hands %+v", a)
	}

	// With the rule turned to none, a waiting ticket stops waiting.
	e.Exec(`UPDATE members SET availability = 'offline' WHERE workspace_id = $1`, ws.ID)
	later := createTicket(t, e, ws, map[string]any{"kind": "note"})
	runTriggers(t, e, ws.ID)
	e.SetSettings(ws, map[string]any{"auto_assign": "none"})
	e.Exec(`UPDATE members SET availability = 'online' WHERE workspace_id = $1`, ws.ID)
	if err := tickets.AssignBacklog(e.Ctx, e.D); err != nil {
		t.Fatal(err)
	}
	e.Scalar(`SELECT assign_pending_since IS NOT NULL FROM tickets WHERE id = $1`, &pending, later.ID)
	if pending || getTicket(t, e, ws, later.ID).Assignee != nil {
		t.Fatal("a ticket under rule none was assigned or kept waiting")
	}
}
