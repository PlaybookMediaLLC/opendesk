package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/useopendesk/opendesk/internal/crypto"
	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/ids"
	"github.com/useopendesk/opendesk/internal/natsx"
)

// relayEnv is a relay on a real Postgres (OPENDESK_TEST_DATABASE_URL) and an embedded NATS.
type relayEnv struct {
	t     *testing.T
	db    *db.DB
	nc    *natsx.Conn
	relay *Relay
}

var migrateOnce sync.Once

func newRelayEnv(t *testing.T) *relayEnv {
	t.Helper()
	url := os.Getenv("OPENDESK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENDESK_TEST_DATABASE_URL to run database tests")
	}
	ctx := context.Background()
	var merr error
	migrateOnce.Do(func() { merr = db.Migrate(ctx, url) })
	if merr != nil {
		t.Fatalf("migrate: %v", merr)
	}
	database, err := db.Open(ctx, url, url)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	nc, err := natsx.Connect(natsx.Options{StoreDir: t.TempDir(), Name: "events-test"}, log)
	if err != nil {
		t.Fatal(err)
	}
	jc, err := river.NewClient[pgx.Tx](riverpgxv5.New(database.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		nc.Close()
		database.Close()
	})
	return &relayEnv{t: t, db: database, nc: nc, relay: &Relay{Pool: database.Pool, NATS: nc.Conn, Jobs: jc, Log: log}}
}

func (e *relayEnv) workspace() uuid.UUID {
	e.t.Helper()
	wid := ids.New()
	slug := "relay-" + crypto.RandomString(10)
	if _, err := e.db.Pool.Exec(context.Background(), `INSERT INTO workspaces (id, name, slug, widget_key) VALUES ($1, 'Relay', $2, $3)`,
		wid, slug, "wk_"+crypto.RandomString(12)); err != nil {
		e.t.Fatal(err)
	}
	return wid
}

// subscribe returns the envelopes NATS delivers for workspace wid.
func (e *relayEnv) subscribe(wid uuid.UUID) *nats.Subscription {
	e.t.Helper()
	sub, err := e.nc.SubscribeSync(SubjectPrefix + wid.String())
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.nc.Flush(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = sub.Unsubscribe() })
	return sub
}

func (e *relayEnv) pending(wid uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE workspace_id = $1 AND published_at IS NULL`, wid).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// drain flushes until nothing is left to publish, or until every event of wid is published.
func (e *relayEnv) drain(wid uuid.UUID) {
	e.t.Helper()
	for i := 0; i < 10_000; i++ {
		n, err := e.relay.flush(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 || e.pending(wid) == 0 {
			return
		}
	}
	e.t.Fatal("relay did not drain")
}

func received(t *testing.T, sub *nats.Subscription, n int) []Envelope {
	t.Helper()
	out := make([]Envelope, 0, n)
	for len(out) < n {
		msg, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("after %d of %d envelopes: %v", len(out), n, err)
		}
		var env Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			t.Fatal(err)
		}
		out = append(out, env)
	}
	if msg, err := sub.NextMsg(100 * time.Millisecond); err == nil {
		t.Fatalf("unexpected extra envelope %s", msg.Data)
	}
	return out
}

func assertSeqOrder(t *testing.T, got []Envelope, from int64) {
	t.Helper()
	for i, env := range got {
		if env.Seq != from+int64(i) {
			seqs := make([]int64, len(got))
			for j, g := range got {
				seqs[j] = g.Seq
			}
			t.Fatalf("published seqs %v, want %d.. in order", seqs, from)
		}
	}
}

// insertEvent writes an outbox row directly, with the given creation time.
func (e *relayEnv) insertEvent(wid uuid.UUID, seq int64, at time.Time) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO events (workspace_id, seq, id, type, visibility, topics, data, created_at)
		VALUES ($1, $2, $3, 'test.event', 'agents', $4, '{}', $5)`, wid, seq, ids.New(), []string{WorkspaceTopic(wid)}, at); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE workspaces SET event_seq = GREATEST(event_seq, $2) WHERE id = $1`, wid, seq); err != nil {
		e.t.Fatal(err)
	}
}

// A workspace's events go out in seq order, whatever order their creation times are in.
func TestRelayPublishesBySeqNotTime(t *testing.T) {
	e := newRelayEnv(t)
	wid := e.workspace()
	sub := e.subscribe(wid)
	t0 := time.Now().Add(-time.Minute)
	e.insertEvent(wid, 1, t0)
	e.insertEvent(wid, 2, t0.Add(50*time.Millisecond))
	e.insertEvent(wid, 3, t0.Add(20*time.Millisecond))
	e.insertEvent(wid, 4, t0.Add(-time.Second))
	e.drain(wid)
	assertSeqOrder(t, received(t, sub, 4), 1)
	if n := e.pending(wid); n != 0 {
		t.Fatalf("%d events left unpublished", n)
	}
}

// Two transactions publishing into one workspace at once: the one that waited for the
// workspace's seq gets the later seq, and the relay publishes in seq order.
func TestRelayOrdersConcurrentPublishers(t *testing.T) {
	e := newRelayEnv(t)
	wid := e.workspace()
	sub := e.subscribe(wid)
	ctx := context.Background()

	a, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Rollback(ctx) }()
	if _, err := Publish(ctx, a, Event{WorkspaceID: wid, Type: "test.a1", Data: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- e.db.Tx(ctx, func(tx pgx.Tx) error {
			_, err := Publish(ctx, tx, Event{WorkspaceID: wid, Type: "test.b", Data: map[string]any{}})
			return err
		})
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := Publish(ctx, a, Event{WorkspaceID: wid, Type: "test.a2", Data: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.drain(wid)
	got := received(t, sub, 3)
	assertSeqOrder(t, got, 1)
	if got[0].Type != "test.a1" || got[1].Type != "test.a2" || got[2].Type != "test.b" {
		t.Fatalf("published %s, %s, %s", got[0].Type, got[1].Type, got[2].Type)
	}
	if got[2].OccurredAt.Before(got[1].OccurredAt) {
		t.Fatalf("occurred_at %v of seq 3 is before %v of seq 2", got[2].OccurredAt, got[1].OccurredAt)
	}
}

// While one relay holds a workspace, another relay publishes none of its events; once
// released, they all go out in order, more than one flush's worth included.
func TestRelayClaimsAWorkspaceForOneRelay(t *testing.T) {
	e := newRelayEnv(t)
	wid := e.workspace()
	sub := e.subscribe(wid)
	ctx := context.Background()
	total := int64(relayPerWorkspace + 7)
	t0 := time.Now().Add(-time.Hour)
	for seq := int64(1); seq <= total; seq++ {
		e.insertEvent(wid, seq, t0.Add(time.Duration(total-seq)*time.Millisecond))
	}

	holder, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var held bool
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1::int, hashtext($2::text))`, relayLockClass, wid.String()).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("workspace already claimed")
	}
	for {
		n, err := e.relay.flush(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if n := e.pending(wid); n != int(total) {
		t.Fatalf("%d of %d events pending while another relay holds the workspace", n, total)
	}
	if msg, err := sub.NextMsg(100 * time.Millisecond); err == nil {
		t.Fatalf("published %s while another relay holds the workspace", msg.Data)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	e.drain(wid)
	assertSeqOrder(t, received(t, sub, int(total)), 1)
}
