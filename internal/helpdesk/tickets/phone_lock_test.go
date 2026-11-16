package tickets_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/helpdesk/hd"
	"github.com/useopendesk/opendesk/internal/helpdesk/tickets"
	"github.com/useopendesk/opendesk/internal/jobs"
)

// The model answers a created trigger's ai_decide question outside the trigger
// transaction: a call-ended run on the same ticket goes ahead meanwhile, and the created
// run then merges the ticket, call and all, into the earlier one.
func TestCallEndedConcurrentCreatedMerge(t *testing.T) {
	e, ws := newEnv(t)
	mb := addMailbox(t, e, ws)
	dst := customerMail(t, e, ws, mb, "caller@example.test", "Call", "earlier")
	src := customerMail(t, e, ws, mb, "caller@example.test", "Call", "later")
	call := uuid.New()
	e.Exec(`INSERT INTO phone_calls (id,workspace_id,provider,external_call_id,ticket_id,direction,stage) VALUES ($1,$2,'aircall',$1::uuid::text,$3,'inbound','ended')`, call, ws.ID, src)
	addAutomation(t, e, ws, jobs.TriggerTicketCreated, 0, []map[string]string{{"field": "ai_decide", "op": "gt", "value": "0.5", "question": "Merge?"}}, []map[string]any{mergeAction("7", false)})
	addAutomation(t, e, ws, jobs.TriggerCallEnded, 0, []map[string]string{}, []map[string]string{{"type": "set_status", "value": "open"}})
	entered, release := make(chan struct{}), make(chan struct{})
	prev := hd.GetAIHooks()
	defer hd.SetAIHooks(prev)
	var once sync.Once
	hd.SetAIHooks(&hd.AIHooks{Decide: func(ctx context.Context, _ db.Querier, wid, tid uuid.UUID, q []string) (map[string]float64, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return map[string]float64{"Merge?": 1}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	ctx, cancel := context.WithTimeout(e.Ctx, 15*time.Second)
	defer cancel()
	created := make(chan error, 1)
	go func() {
		created <- tickets.RunTrigger(ctx, e.D, jobs.TicketTriggerArgs{WorkspaceID: ws.ID, TicketID: src, Trigger: jobs.TriggerTicketCreated})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// No trigger lock is held while the model answers: the call-ended run finishes first.
	ended := make(chan error, 1)
	go func() {
		ended <- tickets.RunTrigger(ctx, e.D, jobs.TicketTriggerArgs{WorkspaceID: ws.ID, TicketID: src, CallID: &call, Trigger: jobs.TriggerCallEnded})
	}()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("call ended: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("call-ended waited for the model answering the created trigger")
	}
	close(release)
	if err := <-created; err != nil {
		t.Errorf("created merge: %v", err)
	}
	var owner uuid.UUID
	e.Scalar(`SELECT ticket_id FROM phone_calls WHERE id=$1`, &owner, call)
	s := mergedInto(t, e, src)
	if owner != dst || s.Into == nil || *s.Into != dst || s.Status != "closed" {
		t.Fatalf("owner=%s source=%+v want destination=%s and source closed", owner, s, dst)
	}
}
