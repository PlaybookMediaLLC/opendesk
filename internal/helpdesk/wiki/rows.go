package wiki

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/deps"
	"github.com/useopendesk/opendesk/internal/domain"
	"github.com/useopendesk/opendesk/internal/helpdesk/hd"
	"github.com/useopendesk/opendesk/internal/httpx"
)

// DbRow is a database's row as its views read it: the page without its body, its values,
// who made and last edited it, its cover and how its text starts (gallery cards).
type DbRow struct {
	WikiNode
	Props     map[string]any  `json:"props"`
	Cover     string          `json:"cover"`
	CoverY    int             `json:"cover_y"`
	Excerpt   string          `json:"excerpt" doc:"The first 280 characters of its text"`
	CreatedAt time.Time       `json:"created_at"`
	Creator   *domain.UserRef `json:"created_by"`
	Editor    *domain.UserRef `json:"updated_by"`
}

// DbRows is a page of a database's rows, and its templates.
type DbRows struct {
	Data       []DbRow    `json:"data"`
	NextCursor *string    `json:"next_cursor" doc:"The next page's cursor; null on the last page"`
	Templates  []WikiNode `json:"templates" doc:"The database's own templates: a new row starts as a copy of one"`
}

// rowsCursor is where a page of rows ends, in their order.
type rowsCursor struct {
	Position int       `json:"p"`
	ID       uuid.UUID `json:"id"`
}

// excerptSource is how much of a row's stored body its excerpt is read from.
const excerptSource = 4096

func registerRows(d *deps.Deps, api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "list-wiki-database-rows", Method: http.MethodGet, Path: "/v1/wiki/databases/{id}/rows",
		Summary: "List a wiki database's rows", Tags: tags,
		Description: "The database's rows (its pages in the tree, in their order) with their values, a page at a time: follow next_cursor for the rest. Filters, sorts and groups are the views' to apply."},
		func(ctx context.Context, in *struct {
			ID     string `path:"id"`
			Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"200" doc:"Rows per page"`
			Cursor string `query:"cursor" doc:"Opaque cursor from next_cursor"`
		}) (*struct{ Body DbRows }, error) {
			p, err := auth.RequireWorkspace(ctx, auth.RoleViewer, auth.ScopeWikiRead)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			id, err := hd.ParseID(in.ID, "Database")
			if err != nil {
				return nil, err
			}
			v, err := viewerFor(ctx, d, p)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			if err := mustSee(ctx, d.DB.Pool, v, p.WorkspaceID, id); err != nil {
				return nil, httpx.Wrap(err)
			}
			var kind string
			if err := d.DB.Pool.QueryRow(ctx, `SELECT kind FROM wiki_pages WHERE workspace_id = $1 AND id = $2`, p.WorkspaceID, id).Scan(&kind); err != nil {
				return nil, httpx.Wrap(err)
			}
			if kind != "database" {
				return nil, httpx.NotFound("Database")
			}
			var after rowsCursor
			if err := httpx.DecodeCursor(in.Cursor, &after); err != nil {
				return nil, err
			}
			limit := in.Limit
			if limit < 1 || limit > 500 {
				limit = 200
			}
			rows, err := d.DB.Pool.Query(ctx, `SELECT `+nodeCols+`, p.props, p.cover, p.cover_y,
					left(p.body_html, `+strconv.Itoa(excerptSource)+`), p.created_at,
					c.id, c.name, c.email, c.avatar_url, u.id, u.name, u.email, u.avatar_url
				FROM wiki_pages p LEFT JOIN users c ON c.id = p.created_by LEFT JOIN users u ON u.id = p.updated_by
				WHERE p.workspace_id = $1 AND p.parent_id = $2 AND p.deleted_at IS NULL AND NOT p.template
				AND ($3 OR (p.position, p.id) > ($4, $5))
				ORDER BY p.position, p.id LIMIT $6`, p.WorkspaceID, id, in.Cursor == "", after.Position, after.ID, limit+1)
			if err != nil {
				return nil, httpx.Wrap(err)
			}
			out := DbRows{Data: []DbRow{}}
			defer rows.Close()
			for rows.Next() {
				var r DbRow
				var c, u userCols
				if err := rows.Scan(&r.ID, &r.ParentID, &r.Title, &r.Icon, &r.Position, &r.Version, &r.UpdatedAt, &r.CreatedBy, &r.Tags,
					&r.Private, &r.TeamID, &r.Journal, &r.DailyDate, &r.Template, &r.Verified, &r.Kind, &r.Area,
					&r.Props, &r.Cover, &r.CoverY, &r.Excerpt, &r.CreatedAt,
					&c.id, &c.name, &c.email, &c.avatar, &u.id, &u.name, &u.email, &u.avatar); err != nil {
					return nil, httpx.Wrap(err)
				}
				// How its text starts, read from the start of its body; a ticket mention is its number.
				r.Excerpt = hd.Excerpt(hd.HTMLToText(hd.TicketMentionsByNumber(r.Excerpt)), 280)
				r.Creator, r.Editor = c.ref(), u.ref()
				if r.Props == nil {
					r.Props = map[string]any{}
				}
				out.Data = append(out.Data, r)
			}
			if err := rows.Err(); err != nil {
				return nil, httpx.Wrap(err)
			}
			if len(out.Data) > limit {
				last := out.Data[limit-1]
				out.Data = out.Data[:limit]
				c := httpx.EncodeCursor(rowsCursor{Position: last.Position, ID: last.ID})
				out.NextCursor = &c
			}
			if out.Templates, err = collectNodes(d.DB.Pool.Query(ctx, nodeSelect+` WHERE p.workspace_id = $1 AND p.parent_id = $2 AND p.template AND p.deleted_at IS NULL
				ORDER BY p.position, p.id`, p.WorkspaceID, id)); err != nil {
				return nil, httpx.Wrap(err)
			}
			return &struct{ Body DbRows }{out}, nil
		})
}
