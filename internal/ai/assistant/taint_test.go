package assistant

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/useopendesk/opendesk/internal/ai"
	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/integrations"
)

func TestOutsideText(t *testing.T) {
	clean := []turn{
		{Role: "user", Body: turnBody{Text: "What views do we have?", Context: &PageContext{Page: "inbox"}}},
		{Role: "assistant", Body: turnBody{Calls: []ai.ToolCall{{ID: "1", Name: "list_views"}, {ID: "2", Name: "add_note"}}}},
	}
	if outsideText(clean) {
		t.Fatal("a conversation of the team's own data counted as outside text")
	}
	for name, ts := range map[string][]turn{
		"open ticket": {{Role: "user", Body: turnBody{Text: "Summarize", Context: &PageContext{TicketID: uuid.NewString()}}}},
		"get_ticket":  {{Role: "assistant", Body: turnBody{Calls: []ai.ToolCall{{ID: "1", Name: "get_ticket"}}}}},
		"api_get":     {{Role: "assistant", Body: turnBody{Calls: []ai.ToolCall{{ID: "1", Name: "api_get"}}}}},
		"teammate":    {{Role: "assistant", Body: turnBody{Calls: []ai.ToolCall{{ID: "1", Name: "ask_teammate"}}}}},
		"connected":   {{Role: "assistant", Body: turnBody{Calls: []ai.ToolCall{{ID: "1", Name: "linear__list_issues"}}}}},
	} {
		if !outsideText(append(append([]turn{}, clean...), ts...)) {
			t.Errorf("%s did not count as outside text", name)
		}
	}
	rc := &runCtx{}
	read := externalTool(integrationsTool(true))
	if rc.needsApproval(read) {
		t.Fatal("a connected read asked for approval in a clean conversation")
	}
	rc.outside = true
	if !rc.needsApproval(read) || rc.needsApproval(&tool{Name: "search_kb"}) {
		t.Fatal("outside text: a connected read must be proposed, a built-in read still runs")
	}
}

func TestRunLeaseHoldsAcrossProcesses(t *testing.T) {
	e := newEnv(t, false)
	agent := e.WS.Agent.As(e.WS)
	sid := uuid.MustParse(e.session(agent))
	tok, err := e.S.begin(e.Ctx, e.WS.ID, sid)
	if err != nil {
		t.Fatal(err)
	}
	// Another process (another Service on the same database) finds it busy.
	other := New(e.D, e.S.ai, e.S.api)
	if _, err := other.begin(e.Ctx, e.WS.ID, sid); !errors.Is(err, errBusy) {
		t.Fatalf("second run: %v", err)
	}
	e.S.end(e.Ctx, e.WS.ID, sid, tok)
	tok2, err := other.begin(e.Ctx, e.WS.ID, sid)
	if err != nil {
		t.Fatalf("after end: %v", err)
	}
	// A lease left by a process that stopped lapses.
	e.Exec(`UPDATE assistant_sessions SET run_until = now() - interval '1 second' WHERE id = $1`, sid)
	tok3, err := e.S.begin(e.Ctx, e.WS.ID, sid)
	if err != nil {
		t.Fatalf("lapsed lease: %v", err)
	}
	// The run whose lease was taken stops at its next renewal; the one holding it renews.
	if err := other.renew(e.Ctx, &runCtx{p: &auth.Principal{WorkspaceID: e.WS.ID}, sid: sid, lease: tok2}); !errors.Is(err, errBusy) {
		t.Fatalf("renewing a taken lease: %v", err)
	}
	if err := e.S.renew(e.Ctx, &runCtx{p: &auth.Principal{WorkspaceID: e.WS.ID}, sid: sid, lease: tok3}); err != nil {
		t.Fatalf("renewing the held lease: %v", err)
	}
	e.S.end(e.Ctx, e.WS.ID, sid, tok3)
}

func integrationsTool(read bool) integrations.Tool {
	return integrations.Tool{Name: "linear__list_issues", Remote: "list_issues", Integration: "Linear", ReadOnly: read, OpenWorld: true}
}
