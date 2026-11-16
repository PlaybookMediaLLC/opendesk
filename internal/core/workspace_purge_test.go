package core

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/useopendesk/opendesk/internal/jobs"
)

// A large workspace is closed in the request and removed by the purge job in batches.
func TestLargeWorkspaceIsPurgedInTheBackground(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.signup("purge")
	wid := uuid.MustParse(owner.newWorkspace("Purge Co"))
	ctx := context.Background()
	if r := owner.do("POST", "/v1/teams", map[string]any{"name": "Support"}); r.Status != 201 {
		t.Fatalf("team: %d %s", r.Status, r.Raw)
	}

	// Every workspace counts as large here.
	prev := syncDeleteRows
	syncDeleteRows = 0
	t.Cleanup(func() { syncDeleteRows = prev })

	var members []uuid.UUID
	if err := e.d.DB.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		members, _, err = e.s.deleteWorkspace(ctx, tx, wid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("members to disconnect: %v", members)
	}
	var deleting, suspended bool
	var memberRows, queued int
	if err := e.d.DB.Pool.QueryRow(ctx, `SELECT deleting_at IS NOT NULL, suspended_at IS NOT NULL,
		(SELECT count(*) FROM members WHERE workspace_id = $1),
		(SELECT count(*) FROM river_job WHERE kind = 'core.workspace_purge' AND args->>'workspace_id' = $2)
		FROM workspaces WHERE id = $1`, wid, wid.String()).Scan(&deleting, &suspended, &memberRows, &queued); err != nil {
		t.Fatal(err)
	}
	if !deleting || !suspended || memberRows != 0 || queued != 1 {
		t.Fatalf("closed workspace: deleting %v suspended %v members %d queued %d", deleting, suspended, memberRows, queued)
	}
	// It is gone from its owner's list and its routes at once.
	if r := owner.do("GET", "/v1/workspace", nil); r.Status/100 == 2 {
		t.Fatalf("closed workspace still answers: %d %s", r.Status, r.Raw)
	}

	// A purge job for a workspace whose deletion was not asked for changes nothing.
	other, _ := e.signup("keep")
	kept := uuid.MustParse(other.newWorkspace("Kept Co"))
	worker := &workspacePurgeWorker{s: e.s}
	if err := worker.Work(ctx, &river.Job[jobs.WorkspacePurgeArgs]{Args: jobs.WorkspacePurgeArgs{WorkspaceID: kept}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.d.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE id = $1`, kept).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a workspace without deleting_at was purged: %d %v", n, err)
	}

	if err := worker.Work(ctx, &river.Job[jobs.WorkspacePurgeArgs]{Args: jobs.WorkspacePurgeArgs{WorkspaceID: wid}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM workspaces WHERE id = $1`,
		`SELECT count(*) FROM events WHERE workspace_id = $1`,
		`SELECT count(*) FROM audit_logs WHERE workspace_id = $1`,
		`SELECT count(*) FROM teams WHERE workspace_id = $1`,
	} {
		if err := e.d.DB.Pool.QueryRow(ctx, q, wid).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s after the purge: %d %v", q, n, err)
		}
	}
	// A repeat of the job finds nothing to do.
	if err := worker.Work(ctx, &river.Job[jobs.WorkspacePurgeArgs]{Args: jobs.WorkspacePurgeArgs{WorkspaceID: wid}}); err != nil {
		t.Fatal(err)
	}
}
