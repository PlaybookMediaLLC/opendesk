package tickets

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/deps"
	"github.com/useopendesk/opendesk/internal/domain"
	"github.com/useopendesk/opendesk/internal/helpdesk/hd"
)

// Scheduled job arguments. They carry no data; each run scans for due work.
type SLAScanArgs struct{}

func (SLAScanArgs) Kind() string { return "helpdesk.sla_scan" }

type SnoozeWakeArgs struct{}

func (SnoozeWakeArgs) Kind() string { return "helpdesk.snooze_wake" }

type AutoCloseArgs struct{}

func (AutoCloseArgs) Kind() string { return "helpdesk.auto_close" }

type PurgeAttachmentsArgs struct{}

func (PurgeAttachmentsArgs) Kind() string { return "helpdesk.purge_attachments" }

type AssignBacklogArgs struct{}

func (AssignBacklogArgs) Kind() string { return "helpdesk.assign_backlog" }

const scanBatch = 500

type ticketRef struct{ wid, tid uuid.UUID }

func collectRefs(rows pgx.Rows, err error) ([]ticketRef, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ticketRef, error) {
		var t ticketRef
		err := r.Scan(&t.wid, &t.tid)
		return t, err
	})
}

// SLAScanWorker logs an sla_breached activity and notifies the assignee once per kind.
type SLAScanWorker struct {
	river.WorkerDefaults[SLAScanArgs]
	D *deps.Deps
}

func (w *SLAScanWorker) Work(ctx context.Context, _ *river.Job[SLAScanArgs]) error {
	return ScanSLA(ctx, w.D)
}

// ScanSLA marks newly breached tickets: a reply target passed while the team has not
// replied in the cycle, or a resolution target passed while the clock runs.
func ScanSLA(ctx context.Context, d *deps.Deps) error {
	refs, err := collectRefs(d.DB.Pool.Query(ctx, `SELECT t.workspace_id, t.id FROM tickets t JOIN workspaces w ON w.id = t.workspace_id
		WHERE t.status IN ('open', 'pending', 'on_hold') AND t.deleted_at IS NULL AND t.sla_policy_id IS NOT NULL AND NOT t.spam AND `+ticketsOn+`
		AND ((`+domain.SLAReplyPendingSQL("t")+` AND t.first_response_due_at < now() AND t.sla_first_response_breached_at IS NULL)
			OR (t.resolution_due_at < now() AND t.sla_paused_at IS NULL AND t.sla_resolution_breached_at IS NULL))
		ORDER BY LEAST(t.first_response_due_at, t.resolution_due_at) LIMIT $1`, scanBatch))
	if err != nil {
		return err
	}
	for _, r := range refs {
		if err := d.DB.Tx(ctx, func(tx pgx.Tx) error { return breachTicket(ctx, tx, r.wid, r.tid) }); err != nil {
			return err
		}
	}
	return nil
}

func breachTicket(ctx context.Context, tx pgx.Tx, wid, tid uuid.UUID) error {
	stamped, err := stampBreaches(ctx, tx, wid, tid, false)
	if err != nil || !stamped {
		return err
	}
	return domain.PublishTicketUpdated(ctx, tx, wid, tid, []string{"sla"})
}

// stampBreaches records the breaches of the ticket's current cycle that are due by now and
// not recorded yet: an sla_breached activity for each, and, while the ticket is still being
// worked, the assignee's notification. solving: the ticket is being solved or closed in tx,
// so its breaches are recorded before the scan stops looking at it, and nobody is paged.
// It reports whether it recorded one.
func stampBreaches(ctx context.Context, tx pgx.Tx, wid, tid uuid.UUID, solving bool) (bool, error) {
	var frBreach, resBreach bool
	var assignee *uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE tickets t SET
		sla_first_response_breached_at = CASE WHEN fr THEN now() ELSE sla_first_response_breached_at END,
		sla_resolution_breached_at = CASE WHEN res THEN now() ELSE sla_resolution_breached_at END
		FROM (SELECT id,
			COALESCE(`+domain.SLAReplyPendingSQL("")+` AND first_response_due_at < now() AND sla_first_response_breached_at IS NULL, false) AS fr,
			COALESCE(resolution_due_at < now() AND sla_paused_at IS NULL AND sla_resolution_breached_at IS NULL, false) AS res
			FROM tickets WHERE workspace_id = $1 AND id = $2 AND ($3 OR status IN ('open', 'pending', 'on_hold'))
			AND deleted_at IS NULL AND sla_policy_id IS NOT NULL FOR UPDATE) s
		WHERE t.id = s.id AND (s.fr OR s.res)
		RETURNING s.fr, s.res, t.assignee_id`, wid, tid, solving).Scan(&frBreach, &resBreach, &assignee)
	if err != nil {
		if db.IsNotFound(err) {
			return false, nil // handled by another run, or no longer due
		}
		return false, err
	}
	for _, k := range []struct {
		on   bool
		kind string
	}{{frBreach, "first_response"}, {resBreach, "resolution"}} {
		if !k.on {
			continue
		}
		if _, err := domain.LogActivity(ctx, tx, wid, tid, "system", nil, "sla_breached", map[string]any{"kind": k.kind}); err != nil {
			return false, err
		}
		if assignee != nil && !solving {
			if err := hd.Notify(ctx, tx, wid, *assignee, hd.NotifySLABreached, &tid, map[string]any{"kind": k.kind}); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// SnoozeWakeWorker reopens tickets whose snooze has passed.
type SnoozeWakeWorker struct {
	river.WorkerDefaults[SnoozeWakeArgs]
	D *deps.Deps
}

func (w *SnoozeWakeWorker) Work(ctx context.Context, _ *river.Job[SnoozeWakeArgs]) error {
	return WakeSnoozed(ctx, w.D)
}

// WakeSnoozed sets status open and clears snoozed_until for every passed snooze.
func WakeSnoozed(ctx context.Context, d *deps.Deps) error {
	refs, err := collectRefs(d.DB.Pool.Query(ctx, `SELECT t.workspace_id, t.id FROM tickets t JOIN workspaces w ON w.id = t.workspace_id
		WHERE t.snoozed_until IS NOT NULL AND t.snoozed_until <= now() AND t.deleted_at IS NULL AND `+ticketsOn+` ORDER BY t.snoozed_until LIMIT $1`, scanBatch))
	if err != nil {
		return err
	}
	open := domain.StatusOpen
	for _, r := range refs {
		err := d.DB.Tx(ctx, func(tx pgx.Tx) error {
			var until *time.Time
			if err := tx.QueryRow(ctx, `SELECT snoozed_until FROM tickets WHERE workspace_id = $1 AND id = $2 FOR UPDATE`, r.wid, r.tid).Scan(&until); err != nil {
				return err
			}
			if until == nil || until.After(time.Now()) {
				return nil
			}
			_, err := applyPatch(ctx, tx, d, r.wid, r.tid, systemActor,
				Patch{Status: &open, Snooze: Opt[time.Time]{Set: true, Null: true}}, applyOpts{Enqueue: true})
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// AutoCloseWorker closes tickets solved longer than the workspace's auto-close delay.
type AutoCloseWorker struct {
	river.WorkerDefaults[AutoCloseArgs]
	D *deps.Deps
}

func (w *AutoCloseWorker) Work(ctx context.Context, _ *river.Job[AutoCloseArgs]) error {
	return AutoClose(ctx, w.D)
}

// ticketsOn leaves a workspace's tickets alone while it has Tickets off: no breaches, wakes or closes.
const ticketsOn = `COALESCE(w.settings->>'tickets_enabled', 'true') <> 'false'`

// autoCloseDays mirrors hd.Settings.AutoClose in SQL: a number in settings, default 7.
const autoCloseDays = `(CASE WHEN jsonb_typeof(w.settings->'auto_close_days') = 'number'
	THEN GREATEST(floor((w.settings->>'auto_close_days')::numeric), 0)::int ELSE 7 END)`

// AutoClose closes solved tickets past the delay (settings.auto_close_days, 0 = never).
func AutoClose(ctx context.Context, d *deps.Deps) error {
	refs, err := collectRefs(d.DB.Pool.Query(ctx, `SELECT t.workspace_id, t.id FROM tickets t JOIN workspaces w ON w.id = t.workspace_id
		WHERE t.status = 'solved' AND t.deleted_at IS NULL AND `+ticketsOn+` AND `+autoCloseDays+` > 0
		AND t.solved_at < now() - make_interval(days => `+autoCloseDays+`) ORDER BY t.solved_at LIMIT $1`, scanBatch))
	if err != nil {
		return err
	}
	closed := domain.StatusClosed
	for _, r := range refs {
		err := d.DB.Tx(ctx, func(tx pgx.Tx) error {
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM tickets WHERE workspace_id = $1 AND id = $2 FOR UPDATE`, r.wid, r.tid).Scan(&status); err != nil {
				return err
			}
			if status != domain.StatusSolved {
				return nil
			}
			_, err := applyPatch(ctx, tx, d, r.wid, r.tid, Actor{Type: "system", Meta: map[string]any{"via": "auto_close"}},
				Patch{Status: &closed}, applyOpts{Enqueue: true})
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// AssignBacklogWorker assigns the tickets auto-assignment left waiting.
type AssignBacklogWorker struct {
	river.WorkerDefaults[AssignBacklogArgs]
	D *deps.Deps
}

func (w *AssignBacklogWorker) Work(ctx context.Context, _ *river.Job[AssignBacklogArgs]) error {
	return AssignBacklog(ctx, w.D)
}

// AssignBacklog gives each open ticket that auto-assignment found nobody eligible for
// (assign_pending_since) to someone who is eligible now, oldest first, by the rule that
// applies to it (contracts/api.md "Assignment"). A pool with nobody eligible is not asked
// again in the same run.
func AssignBacklog(ctx context.Context, d *deps.Deps) error {
	rows, err := d.DB.Pool.Query(ctx, `SELECT t.workspace_id, t.id, COALESCE(t.team_id::text, '') || '/' || COALESCE(t.mailbox_id::text, '') || '/' || t.channel || '/' ||
			COALESCE((SELECT string_agg(ts.skill_id::text, ',' ORDER BY ts.skill_id) FROM ticket_skills ts WHERE ts.workspace_id = t.workspace_id AND ts.ticket_id = t.id), '')
		FROM tickets t JOIN workspaces w ON w.id = t.workspace_id
		WHERE t.assign_pending_since IS NOT NULL AND t.assignee_id IS NULL AND t.status = 'open' AND NOT t.spam
		AND t.deleted_at IS NULL AND t.merged_into_id IS NULL AND `+ticketsOn+`
		ORDER BY t.assign_pending_since LIMIT $1`, scanBatch)
	if err != nil {
		return err
	}
	type waiting struct {
		wid, tid uuid.UUID
		pool     string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (waiting, error) {
		var w waiting
		err := r.Scan(&w.wid, &w.tid, &w.pool)
		return w, err
	})
	if err != nil {
		return err
	}
	exhausted := map[string]bool{}
	for _, w := range list {
		key := w.wid.String() + "/" + w.pool
		if exhausted[key] {
			continue
		}
		var assigned bool
		err := d.DB.Tx(ctx, func(tx pgx.Tx) error {
			// The trigger's lock: a run on this ticket finishes before the backlog looks at it.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ticket_trigger:' || $1::text, 0))`, w.tid); err != nil {
				return err
			}
			if err := autoAssign(ctx, tx, d, w.wid, w.tid, ""); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT assignee_id IS NOT NULL FROM tickets WHERE workspace_id = $1 AND id = $2`, w.wid, w.tid).Scan(&assigned)
		})
		if db.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !assigned {
			exhausted[key] = true
		}
	}
	return nil
}

// PurgeAttachmentsWorker removes uploads that no message and no wiki page took within 24h.
type PurgeAttachmentsWorker struct {
	river.WorkerDefaults[PurgeAttachmentsArgs]
	D *deps.Deps
}

func (w *PurgeAttachmentsWorker) Work(ctx context.Context, _ *river.Job[PurgeAttachmentsArgs]) error {
	return PurgeAttachments(ctx, w.D)
}

// PurgeAttachments deletes stale unattached uploads from storage and the database. A wiki
// page's files (contracts/wiki.md "Files") are kept while the page is.
//
// Uploads held by a member's unsent reply draft stay while the draft does; drafts untouched
// for ReplyDraftRetention are deleted first, which frees their uploads.
func PurgeAttachments(ctx context.Context, d *deps.Deps) error {
	// A scheduled reply stays until it goes out, however far ahead it is.
	if _, err := d.DB.Pool.Exec(ctx, `DELETE FROM ticket_reply_drafts WHERE send_at IS NULL AND updated_at < now() - make_interval(secs => $1)`,
		ReplyDraftRetention.Seconds()); err != nil {
		return err
	}
	rows, err := d.DB.Pool.Query(ctx, `SELECT a.id, a.storage_key FROM attachments a WHERE a.message_id IS NULL AND a.wiki_page_id IS NULL
		AND a.created_at < now() - interval '24 hours'
		AND NOT EXISTS (SELECT 1 FROM ticket_reply_drafts rd WHERE rd.workspace_id = a.workspace_id AND a.id = ANY(rd.attachment_ids))
		ORDER BY a.created_at LIMIT $1`, scanBatch)
	if err != nil {
		return err
	}
	type att struct {
		id  uuid.UUID
		key string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (att, error) {
		var a att
		err := r.Scan(&a.id, &a.key)
		return a, err
	})
	if err != nil {
		return err
	}
	for _, a := range list {
		// Claim the row first so an upload attached meanwhile is never removed.
		tag, err := d.DB.Pool.Exec(ctx, `DELETE FROM attachments a WHERE a.id = $1 AND a.message_id IS NULL AND a.wiki_page_id IS NULL
			AND NOT EXISTS (SELECT 1 FROM ticket_reply_drafts rd WHERE rd.workspace_id = a.workspace_id AND a.id = ANY(rd.attachment_ids))`, a.id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if err := d.Storage.Delete(ctx, a.key); err != nil {
			d.Log.Warn("purge attachment object", "attachment_id", a.id, "err", err)
		}
	}
	return nil
}

// periodicJobs are the helpdesk schedules.
func periodicJobs() []*river.PeriodicJob {
	every := func(interval time.Duration, args river.JobArgs) *river.PeriodicJob {
		return river.NewPeriodicJob(river.PeriodicInterval(interval), func() (river.JobArgs, *river.InsertOpts) {
			return args, &river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByPeriod: interval}}
		}, &river.PeriodicJobOpts{RunOnStart: true, ID: args.Kind()})
	}
	return []*river.PeriodicJob{
		every(time.Minute, SLAScanArgs{}),
		every(time.Minute, SnoozeWakeArgs{}),
		every(15*time.Minute, AutoCloseArgs{}),
		every(time.Hour, PurgeAttachmentsArgs{}),
	}
}
