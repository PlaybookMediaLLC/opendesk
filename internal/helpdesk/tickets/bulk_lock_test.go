package tickets_test

import (
	"context"
	"testing"
	"time"

	"github.com/useopendesk/opendesk/internal/events"
)

// A bulk change locks all its tickets before it publishes anything, so an edit that holds
// one of those tickets and then publishes is never left waiting on the bulk change.
func TestBulkLocksTicketsBeforePublishing(t *testing.T) {
	e, ws := newEnv(t)
	a := createTicket(t, e, ws, map[string]any{"subject": "A"})
	b := createTicket(t, e, ws, map[string]any{"subject": "B"})
	ctx := context.Background()

	// An edit of b, as PATCH /v1/tickets/{b} runs it: the ticket's lock first.
	tx, err := e.D.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tickets WHERE workspace_id = $1 AND id = $2 FOR UPDATE`, ws.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	bulk := make(chan int, 1)
	go func() {
		r := e.Do("POST", "/v1/tickets/bulk", ws.Agent.As(ws), map[string]any{"ids": []string{a.ID.String(), b.ID.String()},
			"patch": map[string]any{"status": "pending"}})
		bulk <- r.Status
	}()
	// Give the bulk change time to reach b's lock.
	time.Sleep(200 * time.Millisecond)

	// The edit now publishes its event: the bulk change holds no event lock yet.
	published := make(chan error, 1)
	go func() {
		_, err := events.Publish(ctx, tx, events.Event{WorkspaceID: ws.ID, Type: "ticket.updated", TicketID: &b.ID, Data: map[string]any{"ticket_id": b.ID}})
		published <- err
	}()
	select {
	case err := <-published:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the edit waited on the bulk change for the workspace's events")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-bulk:
		if status != 200 {
			t.Fatalf("bulk status %d", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bulk change did not finish")
	}
	for _, id := range []string{a.ID.String(), b.ID.String()} {
		if got := e.Do("GET", "/v1/tickets/"+id, ws.Agent.As(ws), nil).Want(t, 200).Map(t)["status"]; got != "pending" {
			t.Fatalf("ticket %s status %v", id, got)
		}
	}
}
