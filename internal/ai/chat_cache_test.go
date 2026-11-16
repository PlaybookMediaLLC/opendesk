package ai

import "testing"

// TestToolLoopTurnEndsTheCachedPrefix: mid-loop, the newest user turn holds only tool
// results, and its last result carries the cache breakpoint.
func TestToolLoopTurnEndsTheCachedPrefix(t *testing.T) {
	a := NewAnthropic("k", "")
	req := ChatRequest{MaxTokens: 100, Turns: []ChatTurn{
		{Role: "user", Text: "Look it up"},
		{Role: "assistant", Calls: []ToolCall{{ID: "c1", Name: "x"}, {ID: "c2", Name: "y"}}},
		{Role: "user", Results: []ToolResult{{CallID: "c1", Content: "one"}, {CallID: "c2", Content: "two"}}},
	}}
	p, _ := a.chatParams(req)
	last := p.Messages[len(p.Messages)-1].Content
	if len(last) != 2 || last[0].OfToolResult == nil || last[1].OfToolResult == nil {
		t.Fatalf("last turn %+v", last)
	}
	if last[1].OfToolResult.CacheControl.Type == "" || last[0].OfToolResult.CacheControl.Type != "" {
		t.Fatalf("cache breakpoint not on the last result")
	}
	// The first user turn, no longer the newest, carries none.
	if first := p.Messages[0].Content; first[0].OfText == nil || first[0].OfText.CacheControl.Type != "" {
		t.Fatal("an older turn kept a breakpoint")
	}
}
