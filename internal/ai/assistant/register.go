// Package assistant is Ollie, opendesk's assistant: a tool-using conversation on the
// configured AI provider, streamed over SSE, that reads the workspace and proposes
// changes the user approves one by one (contracts/ai.md, Assistant). Tools run through
// the public API in process, as the calling user, so they carry exactly that user's
// role, workspace and checks.
package assistant

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/useopendesk/opendesk/internal/ai"
	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/deps"
	"github.com/useopendesk/opendesk/internal/httpx"
)

// approvalTTL is how long a proposed change waits for a decision.
const approvalTTL = 15 * time.Minute

// maxSteps bounds the model turns of one run.
const maxSteps = 8

var (
	errNotPending = httpx.E(http.StatusConflict, "approval_not_pending", "This change is no longer waiting for approval.")
	errBusy       = httpx.E(http.StatusConflict, "assistant_busy", "Ollie is still working on this conversation.")
	errOff        = httpx.E(http.StatusForbidden, "assistant_off", "The assistant is turned off for this workspace.")
	errBadToken   = httpx.E(http.StatusForbidden, "approval_invalid", "This approval is not valid for you.")
)

// Service runs assistant conversations.
type Service struct {
	d     *deps.Deps
	ai    *ai.Service
	api   http.Handler
	tools map[string]*tool
	order []*tool
	// spec is the API's OpenAPI document, which the API tools search and match.
	spec    *huma.OpenAPI
	opsOnce sync.Once
	apiOps  []apiOp
	apiIDF  map[string]float64
	apiAvg  float64
}

// New builds the service. api is the public API handler the tools call.
func New(d *deps.Deps, svc *ai.Service, api http.Handler) *Service {
	s := &Service{d: d, ai: svc, api: api, tools: map[string]*tool{}}
	for _, t := range append(append(append(readTools(), writeTools()...), apiTools()...), peerTools()...) {
		s.tools[t.Name] = t
		s.order = append(s.order, t)
	}
	return s
}

// Register adds the assistant routes.
func Register(d *deps.Deps, api huma.API, _ chi.Router) {
	New(d, ai.For(d), api.Adapter()).register(api)
}

// RegisterWorkers, Periodic and Background: the assistant has no jobs.
func RegisterWorkers(*deps.Deps, *river.Workers) {}
func Periodic(*deps.Deps) []*river.PeriodicJob   { return nil }
func Background(context.Context, *deps.Deps)     {}

// runLease is how long a run holds its conversation without renewing the claim; each model
// turn renews it.
const runLease = 10 * time.Minute

// begin claims a conversation for one run, across every API process: only one run per
// conversation at a time (errBusy otherwise). The claim is a lease on the session row
// (run_token, run_until); one left by a process that stopped lapses after runLease.
func (s *Service) begin(ctx context.Context, wid, sid uuid.UUID) (uuid.UUID, error) {
	tok := uuid.New()
	tag, err := s.d.DB.Pool.Exec(ctx, `UPDATE assistant_sessions SET run_token = $3, run_until = now() + $4::int * interval '1 second'
		WHERE workspace_id = $1 AND id = $2 AND (run_until IS NULL OR run_until < now())`, wid, sid, tok, int(runLease/time.Second))
	if err != nil {
		return uuid.Nil, httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return uuid.Nil, errBusy
	}
	return tok, nil
}

// renew extends the run's lease; a run whose lease another run took stops (errBusy).
func (s *Service) renew(ctx context.Context, rc *runCtx) error {
	if rc.lease == uuid.Nil {
		return nil
	}
	tag, err := s.d.DB.Pool.Exec(ctx, `UPDATE assistant_sessions SET run_until = now() + $4::int * interval '1 second'
		WHERE workspace_id = $1 AND id = $2 AND run_token = $3`, rc.p.WorkspaceID, rc.sid, rc.lease, int(runLease/time.Second))
	if err != nil {
		return httpx.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return errBusy
	}
	return nil
}

// end gives the conversation back, even when the request went away.
func (s *Service) end(ctx context.Context, wid, sid, tok uuid.UUID) {
	if _, err := s.d.DB.Pool.Exec(context.WithoutCancel(ctx), `UPDATE assistant_sessions SET run_token = NULL, run_until = NULL
		WHERE workspace_id = $1 AND id = $2 AND run_token = $3`, wid, sid, tok); err != nil {
		s.d.Log.Warn("assistant run lease", "err", err)
	}
}

// require checks the caller may use the assistant: a signed-in member (API keys do not
// use it) of a workspace with AI and the assistant switched on.
func (s *Service) require(ctx context.Context) (*auth.Principal, error) {
	p, err := auth.RequireWorkspace(ctx, auth.RoleViewer, "")
	if err != nil {
		return nil, httpx.Wrap(err)
	}
	if p.Kind != auth.KindUser {
		return nil, httpx.Forbidden()
	}
	st, err := s.ai.WorkspaceAI(ctx, p.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !st.Assistant {
		return nil, errOff
	}
	return p, nil
}

type sessionList struct {
	Data       []Session `json:"data"`
	NextCursor *string   `json:"next_cursor"`
}

// Transcript is GET /v1/assistant/sessions/{id}.
type Transcript struct {
	Session  Session   `json:"session"`
	Items    []Item    `json:"items"`
	Approval *Approval `json:"approval"`
}

func (s *Service) register(api huma.API) {
	s.spec = api.OpenAPI()
	tags := []string{"Assistant"}

	huma.Register(api, huma.Operation{OperationID: "assistant-list-sessions", Method: http.MethodGet, Path: "/v1/assistant/sessions",
		Summary: "List conversations", Description: "The caller's own conversations in this workspace, most recent first.", Tags: tags},
		func(ctx context.Context, in *struct {
			Limit int `query:"limit" minimum:"1" maximum:"100" default:"50"`
		}) (*struct{ Body sessionList }, error) {
			p, err := s.require(ctx)
			if err != nil {
				return nil, err
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 50
			}
			rows, err := s.d.DB.Pool.Query(ctx, `SELECT id, title, created_at, updated_at FROM assistant_sessions
				WHERE workspace_id = $1 AND user_id = $2 ORDER BY updated_at DESC, id DESC LIMIT $3`, p.WorkspaceID, p.UserID, limit)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Session, error) {
				var x Session
				err := r.Scan(&x.ID, &x.Title, &x.CreatedAt, &x.UpdatedAt)
				return x, err
			})
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			if out == nil {
				out = []Session{}
			}
			return &struct{ Body sessionList }{sessionList{Data: out}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "assistant-create-session", Method: http.MethodPost, Path: "/v1/assistant/sessions",
		Summary: "Start a conversation", Tags: tags, DefaultStatus: http.StatusCreated},
		func(ctx context.Context, in *struct {
			Body struct {
				Title string `json:"title,omitempty" maxLength:"200"`
			}
		}) (*struct{ Body Session }, error) {
			p, err := s.require(ctx)
			if err != nil {
				return nil, err
			}
			sess, err := createSession(ctx, s.d.DB.Pool, p.WorkspaceID, p.UserID, in.Body.Title)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			return &struct{ Body Session }{sess}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "assistant-get-session", Method: http.MethodGet, Path: "/v1/assistant/sessions/{id}",
		Summary: "Open a conversation", Description: "The transcript, and the change waiting for approval, if any.", Tags: tags},
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body Transcript }, error) {
			p, err := s.require(ctx)
			if err != nil {
				return nil, err
			}
			tr, err := s.transcript(ctx, p, in.ID)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			return &struct{ Body Transcript }{*tr}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "assistant-rename-session", Method: http.MethodPatch, Path: "/v1/assistant/sessions/{id}",
		Summary: "Rename a conversation", Tags: tags},
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Title string `json:"title" minLength:"1" maxLength:"200"`
			}
		}) (*struct{ Body Session }, error) {
			p, err := s.require(ctx)
			if err != nil {
				return nil, err
			}
			title := cleanTitle(in.Body.Title)
			if title == "" {
				return nil, httpx.Validation("title is required")
			}
			var out Session
			err = s.d.DB.Pool.QueryRow(ctx, `UPDATE assistant_sessions SET title = $4, updated_at = updated_at
				WHERE workspace_id = $1 AND user_id = $2 AND id = $3 RETURNING id, title, created_at, updated_at`,
				p.WorkspaceID, p.UserID, in.ID, title).Scan(&out.ID, &out.Title, &out.CreatedAt, &out.UpdatedAt)
			if err != nil {
				if isNotFound(err) {
					return nil, httpx.NotFound("Conversation")
				}
				return nil, httpx.Wrap(err)
			}
			return &struct{ Body Session }{out}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "assistant-delete-session", Method: http.MethodDelete, Path: "/v1/assistant/sessions/{id}",
		Summary: "Delete a conversation", Description: "Removes the conversation, its turns and its proposals.", Tags: tags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{}, error) {
			p, err := s.require(ctx)
			if err != nil {
				return nil, err
			}
			tag, err := s.d.DB.Pool.Exec(ctx, `DELETE FROM assistant_sessions WHERE workspace_id = $1 AND user_id = $2 AND id = $3`,
				p.WorkspaceID, p.UserID, in.ID)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			if tag.RowsAffected() == 0 {
				return nil, httpx.NotFound("Conversation")
			}
			return nil, nil
		})

	s.registerPeer(api)

	streamDoc := " Send `Accept: text/event-stream` to stream events (session, text, tool, approval, done, error); without it, the response is the collected result."

	huma.Register(api, huma.Operation{OperationID: "assistant-send-message", Method: http.MethodPost, Path: "/v1/assistant/sessions/{id}/messages",
		Summary: "Send a message", Description: "Runs the assistant on a new message with the page context. A change it proposes waits for approval and ends the run." + streamDoc,
		Tags: tags, Middlewares: huma.Middlewares{s.sse(s.streamMessage)}},
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body AssistantMessageInput
		}) (*struct{ Body RunResult }, error) {
			col := &collector{}
			if err := s.sendMessage(ctx, in.ID, in.Body, col.emit); err != nil {
				return nil, err
			}
			return &struct{ Body RunResult }{col.result()}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "assistant-decide-action", Method: http.MethodPost, Path: "/v1/assistant/actions/{id}/decision",
		Summary: "Approve or deny a proposed change", Description: "Takes the approval token from the approval event. An approved change runs once, " +
			"as the user, then the assistant continues." + streamDoc,
		Tags: tags, Middlewares: huma.Middlewares{s.sse(s.streamDecision)}},
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body DecisionInput
		}) (*struct{ Body RunResult }, error) {
			col := &collector{}
			if err := s.decide(ctx, in.ID, in.Body, col.emit); err != nil {
				return nil, err
			}
			return &struct{ Body RunResult }{col.result()}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "assistant-tips", Method: http.MethodGet, Path: "/v1/assistant/tips",
		Summary: "Things worth doing now", Description: "Cheap counts (no AI) the panel can offer as a tip: SLA breaches, old unassigned tickets.", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct {
			CacheControl string `header:"Cache-Control"`
			Body         TipList
		}, error) {
			p, err := s.require(ctx)
			if err != nil {
				return nil, err
			}
			if !s.d.Limiter.Allow("assistant-tips:"+p.UserID.String(), 30, time.Minute) {
				return nil, httpx.E(http.StatusTooManyRequests, "rate_limited", "Too many requests. Try again in a minute.")
			}
			tips, err := s.tips(ctx, p)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			return &struct {
				CacheControl string `header:"Cache-Control"`
				Body         TipList
			}{"private, max-age=60", TipList{Tips: tips}}, nil
		})
}
