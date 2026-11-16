package ai

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/useopendesk/opendesk/internal/ai/decide"
	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/domain"
	"github.com/useopendesk/opendesk/internal/helpdesk/hd"
	"github.com/useopendesk/opendesk/internal/jobs"
)

// TriageTicket is POST /v1/tickets/{id}/ai/triage. With apply, the result is written
// (suggested tags are created) and the ticket_updated trigger runs, as for any edit.
func (s *Service) TriageTicket(ctx context.Context, p *auth.Principal, rawID string, apply bool) (*Triage, error) {
	if !s.canTriage() {
		return nil, errDisabled
	}
	tid, err := resolveVisibleTicket(ctx, s.d.DB.Pool, p, rawID)
	if err != nil {
		return nil, err
	}
	t, c, err := s.triage(ctx, s.d.DB.Pool, p.WorkspaceID, p.ActorUserID(), tid)
	if err != nil {
		return nil, err
	}
	if apply {
		err := s.d.DB.Tx(ctx, func(tx pgx.Tx) error {
			changes, err := applyTriage(ctx, tx, p.WorkspaceID, tid, t, true, "ai_triage")
			if err != nil {
				return err
			}
			_, err = s.d.Jobs.InsertTx(ctx, tx, jobs.TicketTriggerArgs{WorkspaceID: p.WorkspaceID, TicketID: tid,
				Trigger: jobs.TriggerTicketUpdated, ActorUserID: p.ActorUserID(), Changes: changes}, nil)
			return err
		})
		if err != nil {
			return nil, err
		}
		t.Applied = true
	}
	c.charge(ctx)
	return t, nil
}

// skippable errors make automatic AI quietly do nothing.
func skippable(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBusy) || errors.Is(err, ErrRefused) ||
		errors.Is(err, ErrBadOutput) || errors.Is(err, errDisabled) || errors.Is(err, errOff) ||
		errors.Is(err, context.DeadlineExceeded) || isNoBalance(err)
}

// autoTriage is the hd.AIHooks.AutoTriage hook: triage a new ticket when the
// workspace turned it on, once.
func (s *Service) autoTriage(ctx context.Context, wid, tid uuid.UUID) error {
	if !s.canTriage() {
		return nil
	}
	st, err := hd.LoadSettings(ctx, s.d.DB.Pool, wid)
	if err != nil {
		return err
	}
	if a := st.AI(); !a.Enabled || !a.AutoTriage {
		return nil
	}
	t, err := domain.LoadTicket(ctx, s.d.DB.Pool, wid, tid)
	if err != nil {
		return err
	}
	if _, done := t.Custom["ai_triaged_at"]; done || t.Spam {
		return nil
	}
	res, c, err := s.triage(ctx, s.d.DB.Pool, wid, nil, tid)
	if err != nil {
		if isNoBalance(err) {
			return nil
		}
		return err
	}
	if err := s.d.DB.Tx(ctx, func(tx pgx.Tx) error {
		_, err := applyTriage(ctx, tx, wid, tid, res, false, "auto_triage")
		return err
	}); err != nil {
		return err
	}
	c.charge(ctx)
	return nil
}

// triageResult is a triage the ai_triage automation action computed, waiting to be applied.
type triageResult struct{ t *Triage }

// triagePrepare is the hd.AIHooks.TriagePrepare hook for the ai_triage automation action: it
// asks the model through q (the pool), outside the trigger's transaction, and charges the
// call. A skipped triage is (nil, nil).
func (s *Service) triagePrepare(ctx context.Context, q db.Querier, wid, tid uuid.UUID) (any, error) {
	if !s.canTriage() {
		return nil, nil
	}
	res, c, err := s.triage(ctx, q, wid, nil, tid)
	if err != nil {
		if skippable(err) || isNotFound(err) {
			s.d.Log.Info("ai_triage skipped", "ticket_id", tid, "reason", errReason(err))
			return nil, nil
		}
		return nil, err
	}
	c.charge(ctx)
	return triageResult{t: res}, nil
}

// triageApply is the hd.AIHooks.TriageApply hook: it writes a triagePrepare result through
// the trigger's transaction, on a copy, so a retried transaction applies the same result.
func (s *Service) triageApply(ctx context.Context, tx pgx.Tx, wid, tid uuid.UUID, result any) ([]string, error) {
	r, ok := result.(triageResult)
	if !ok || r.t == nil {
		return nil, nil
	}
	return applyTriage(ctx, tx, wid, tid, cloneTriage(r.t), false, "automation")
}

// cloneTriage copies a triage result deep enough that applyTriage, which records what it
// applied and suggested on it, leaves the original as the model returned it.
func cloneTriage(t *Triage) *Triage {
	c := *t
	c.Tags = slices.Clone(t.Tags)
	c.SuggestedTags = slices.Clone(t.SuggestedTags)
	if t.Suggestions != nil {
		c.Suggestions = maps.Clone(t.Suggestions)
	}
	if t.tagField != nil {
		c.tagField = maps.Clone(t.tagField)
	}
	if t.Decisions.Fields != nil {
		c.Decisions.Fields = make(map[string]*Decision, len(t.Decisions.Fields))
		for k, d := range t.Decisions.Fields {
			if d != nil {
				dc := *d
				d = &dc
			}
			c.Decisions.Fields[k] = d
		}
	}
	return &c
}

// decideConditions is the hd.AIHooks.Decide hook for ai_decide automation conditions:
// every question in one decision call about the ticket, read through q (the pool),
// outside the trigger's transaction. A skipped call answers nothing, so those conditions do not hold.
func (s *Service) decideConditions(ctx context.Context, q db.Querier, wid, tid uuid.UUID, questions []string) (map[string]float64, error) {
	if len(questions) == 0 {
		return nil, nil
	}
	if _, err := s.workspaceOn(ctx, wid); err != nil {
		return nil, nil
	}
	if dr, err := s.decisionsFor(ctx, wid, nil); err != nil || dr.dc == nil {
		return nil, err
	}
	tc, err := s.loadTicketCtx(ctx, q, wid, tid, true)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	qs := make(map[string]decide.Question, len(questions))
	for i, q := range questions {
		qs["q"+strconv.Itoa(i)] = decide.Noul(map[string]string{"question": q, "about": "the ticket in `ticket`", "rule": dataRule}, "", "")
	}
	c := s.newCall(wid, nil, &tid, "decide")
	res, err := c.decide(ctx, decide.Request{State: decisionState(tc), Questions: qs})
	if err != nil {
		if skippable(err) {
			s.d.Log.Info("ai_decide skipped", "ticket_id", tid, "reason", errReason(err))
			return nil, nil
		}
		return nil, err
	}
	c.charge(ctx)
	out := make(map[string]float64, len(questions))
	for i, q := range questions {
		if a, ok := res.Answers["q"+strconv.Itoa(i)]; ok {
			out[q] = a.Noul
		}
	}
	return out, nil
}

// errReason is a log-safe label for an AI error.
func errReason(err error) string {
	switch {
	case isNoBalance(err):
		return "no_balance"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrRefused):
		return "refused"
	case errors.Is(err, ErrBadOutput):
		return "bad_output"
	case errors.Is(err, errOff), errors.Is(err, errDisabled):
		return "off"
	}
	return "unavailable"
}

// SummarizeWorker runs ai.summarize jobs (auto-summarize).
type SummarizeWorker struct {
	river.WorkerDefaults[jobs.AISummarizeArgs]
	S *Service
}

func (w *SummarizeWorker) Work(ctx context.Context, job *river.Job[jobs.AISummarizeArgs]) error {
	s, a := w.S, job.Args
	if !s.Enabled() {
		return nil
	}
	st, err := hd.LoadSettings(ctx, s.d.DB.Pool, a.WorkspaceID)
	if err != nil {
		return err
	}
	if ai := st.AI(); !ai.Enabled || !ai.AutoSummarize {
		return nil
	}
	t, err := domain.LoadTicket(ctx, s.d.DB.Pool, a.WorkspaceID, a.TicketID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if t.Spam || t.MessageCount < autoSummarizeMin || storedSummary(t.Custom).MessageCount >= t.MessageCount {
		return nil
	}
	_, err = s.Summarize(ctx, a.WorkspaceID, nil, a.TicketID)
	if err != nil && skippable(err) {
		if errors.Is(err, ErrBusy) || errors.Is(err, ErrUnavailable) {
			return err // retried with backoff by River
		}
		return nil
	}
	return err
}
