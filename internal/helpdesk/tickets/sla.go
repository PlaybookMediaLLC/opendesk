package tickets

// SLA targets (contracts/api.md "SLA"). A ticket's policy sets two targets for its current
// cycle: a reply (first_response_due_at) and a resolution (resolution_due_at). The first
// cycle starts when the ticket is created; a solved ticket that opens again starts a new
// one then. While the ticket is pending or on hold the resolution clock stands still, and
// the time it stood still moves the resolution target when it runs again. A breach is
// recorded once per target and cycle, by the minute scan or when the ticket is solved.

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/domain"
	"github.com/useopendesk/opendesk/internal/helpdesk/hd"
)

// applySLA sets the ticket's policy and targets and publishes ticket.updated when it wrote.
func applySLA(ctx context.Context, tx pgx.Tx, wid, tid uuid.UUID) error {
	changed, err := computeSLA(ctx, tx, wid, tid)
	if err != nil || !changed {
		return err
	}
	return domain.PublishTicketUpdated(ctx, tx, wid, tid, []string{"sla"})
}

// computeSLA picks the first enabled policy, by position, for the ticket's priority (or any
// priority) and its team (or any team), and sets the targets from the start of the current
// cycle: the reply target from there, the resolution target from there plus the minutes the
// clock stood still in this cycle. Business hours are the team's own, else the workspace's.
// A breach already recorded in the cycle stays recorded. It reports whether it wrote.
func computeSLA(ctx context.Context, tx pgx.Tx, wid, tid uuid.UUID) (bool, error) {
	var priority string
	var start time.Time
	var paused int
	var current, team *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT priority, COALESCE(sla_started_at, created_at), sla_paused_minutes, sla_policy_id, team_id FROM tickets
		WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`, wid, tid).Scan(&priority, &start, &paused, &current, &team)
	if db.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var policyID *uuid.UUID
	var frMin, resMin int
	var inHours bool
	err = tx.QueryRow(ctx, `SELECT id, first_response_minutes, resolution_minutes, business_hours FROM sla_policies
		WHERE workspace_id = $1 AND enabled AND (priority = $2 OR priority IS NULL) AND (team_id = $3 OR team_id IS NULL)
		ORDER BY position, (priority IS NULL), (team_id IS NULL), created_at LIMIT 1`, wid, priority, team).Scan(&policyID, &frMin, &resMin, &inHours)
	if err != nil && !db.IsNotFound(err) {
		return false, err
	}
	if policyID == nil && current == nil {
		return false, nil
	}
	if policyID == nil {
		_, err = tx.Exec(ctx, `UPDATE tickets SET sla_policy_id = NULL, first_response_due_at = NULL, resolution_due_at = NULL,
			sla_first_response_breached_at = NULL, sla_resolution_breached_at = NULL, sla_paused_at = NULL, sla_paused_minutes = 0, updated_at = now()
			WHERE workspace_id = $1 AND id = $2`, wid, tid)
		return err == nil, err
	}
	bh, loc := hd.BusinessHours{}, time.UTC
	if inHours {
		if bh, loc, err = hd.HoursFor(ctx, tx, wid, team); err != nil {
			return false, err
		}
	}
	fr, res := bh.AddBusinessMinutes(start, frMin, loc), bh.AddBusinessMinutes(start, resMin+max(paused, 0), loc)
	_, err = tx.Exec(ctx, `UPDATE tickets SET sla_policy_id = $3, first_response_due_at = $4, resolution_due_at = $5, updated_at = now()
		WHERE workspace_id = $1 AND id = $2`, wid, tid, *policyID, fr, res)
	return err == nil, err
}

// startSLACycle starts a new cycle at at for a ticket that opened again after it was solved,
// and sets its targets. replied: the team needs no reply in the new cycle (a person reopened
// the ticket); a customer's message that reopens it starts the reply target from itself.
func startSLACycle(ctx context.Context, tx pgx.Tx, wid, tid uuid.UUID, at time.Time, replied bool) error {
	if err := domain.ResetSLACycle(ctx, tx, wid, tid, at, replied); err != nil {
		return err
	}
	_, err := computeSLA(ctx, tx, wid, tid)
	return err
}

// slaClock stops the resolution clock of a pending or on hold ticket at at, and runs it again
// for a ticket that is neither any more: the open minutes it stood still (business minutes when
// the policy counts business hours) are added to the cycle's paused minutes and move the
// resolution target. It reports whether it wrote.
func slaClock(ctx context.Context, tx pgx.Tx, wid, tid uuid.UUID, at time.Time) (bool, error) {
	var status string
	var policy, team *uuid.UUID
	var pausedAt, resDue *time.Time
	err := tx.QueryRow(ctx, `SELECT status, sla_policy_id, team_id, sla_paused_at, resolution_due_at FROM tickets
		WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`, wid, tid).Scan(&status, &policy, &team, &pausedAt, &resDue)
	if db.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	paused := status == domain.StatusPending || status == domain.StatusOnHold
	switch {
	case paused && pausedAt == nil && policy != nil:
		_, err := tx.Exec(ctx, `UPDATE tickets SET sla_paused_at = $3 WHERE workspace_id = $1 AND id = $2`, wid, tid, at)
		return err == nil, err
	case !paused && pausedAt != nil:
		mins, due := 0, resDue
		if policy != nil {
			var inHours bool
			if err := tx.QueryRow(ctx, `SELECT business_hours FROM sla_policies WHERE workspace_id = $1 AND id = $2`, wid, *policy).
				Scan(&inHours); err != nil && !db.IsNotFound(err) {
				return false, err
			}
			bh, loc := hd.BusinessHours{}, time.UTC
			if inHours {
				if bh, loc, err = hd.HoursFor(ctx, tx, wid, team); err != nil {
					return false, err
				}
			}
			mins = bh.BusinessMinutesBetween(*pausedAt, at, loc)
			if due != nil && mins > 0 {
				d := bh.AddBusinessMinutes(*due, mins, loc)
				due = &d
			}
		}
		_, err := tx.Exec(ctx, `UPDATE tickets SET sla_paused_at = NULL, sla_paused_minutes = sla_paused_minutes + $3, resolution_due_at = $4
			WHERE workspace_id = $1 AND id = $2`, wid, tid, mins, due)
		return err == nil, err
	}
	return false, nil
}

// slaStatusChanged keeps the ticket's SLA in step with a status change made in tx: a solved
// or closed ticket that opens again starts a new cycle; pending and on hold stop the
// resolution clock and any other status runs it; solving records the breaches the scan has
// not reached yet. replied is startSLACycle's.
func slaStatusChanged(ctx context.Context, tx pgx.Tx, wid, tid uuid.UUID, from, to string, replied bool) error {
	now := time.Now().UTC()
	if closing(from) && !closing(to) {
		if err := startSLACycle(ctx, tx, wid, tid, now, replied); err != nil {
			return err
		}
	}
	if _, err := slaClock(ctx, tx, wid, tid, now); err != nil {
		return err
	}
	if closing(to) && !closing(from) {
		if _, err := stampBreaches(ctx, tx, wid, tid, true); err != nil {
			return err
		}
	}
	return nil
}
