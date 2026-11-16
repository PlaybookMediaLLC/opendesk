package collab

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/events"
	"github.com/useopendesk/opendesk/internal/httpx"
)

// Event types (contracts/collab.md "Events"). They carry ids only and are scoped to the
// people who see what changed.
const (
	EventChanged  = "collab.changed"
	EventAccounts = "collab.accounts"
	// EventSidebar tells a member that their sidebar preferences of the workspace changed.
	EventSidebar = "collab.sidebar"
)

// publishChanged tells who sees the account and the conversations that they changed, in tx.
// accountID may be nil for conversations of several accounts (each thread's account is
// looked up); folders marks a change of folders or counts. Every event names only what its
// receivers see: the account's audience is told of all of its conversations, and someone who
// sees one conversation on its own (shared or assigned) is told of that one alone. A change
// to a single conversation is one event for everyone who sees it.
func (s *service) publishChanged(ctx context.Context, tx pgx.Tx, wid uuid.UUID, accountID *uuid.UUID, threadIDs []uuid.UUID, folders bool) error {
	byAccount := map[uuid.UUID][]uuid.UUID{}
	order := []uuid.UUID{}
	add := func(acc uuid.UUID, t *uuid.UUID) {
		if _, ok := byAccount[acc]; !ok {
			order = append(order, acc)
			byAccount[acc] = []uuid.UUID{}
		}
		if t != nil {
			byAccount[acc] = appendNew(byAccount[acc], *t)
		}
	}
	found := map[uuid.UUID]bool{}
	if len(threadIDs) > 0 {
		rows, err := tx.Query(ctx, `SELECT id, account_id FROM collab_threads WHERE workspace_id = $1 AND id = ANY($2)`, wid, threadIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, acc uuid.UUID
			if err := rows.Scan(&id, &acc); err != nil {
				rows.Close()
				return err
			}
			if accountID == nil || acc == *accountID {
				found[id] = true
				add(acc, &id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if accountID != nil {
		add(*accountID, nil)
		// A conversation deleted in this transaction was the account's.
		for _, t := range threadIDs {
			if !found[t] {
				add(*accountID, &t)
			}
		}
	}
	extra, err := threadAudiences(ctx, tx, wid, threadIDs)
	if err != nil {
		return err
	}
	publish := func(users []uuid.UUID, acc uuid.UUID, threads []uuid.UUID, folders bool) error {
		_, err := events.Publish(ctx, tx, events.Event{WorkspaceID: wid, Type: EventChanged, Users: users,
			Data: map[string]any{"account_id": acc, "thread_ids": threads, "folders": folders}})
		return err
	}
	for _, acc := range order {
		threads := byAccount[acc]
		list, err := accountAudience(ctx, tx, wid, acc)
		if err != nil {
			return err
		}
		users := appendNew([]uuid.UUID{}, list...)
		if len(threads) == 1 {
			if err := publish(appendNew(users, extra[threads[0]]...), acc, threads, folders); err != nil {
				return err
			}
			continue
		}
		if err := publish(users, acc, threads, folders); err != nil {
			return err
		}
		for _, t := range threads {
			rest := slices.DeleteFunc(slices.Clone(extra[t]), func(u uuid.UUID) bool { return slices.Contains(users, u) })
			if len(rest) == 0 {
				continue
			}
			if err := publish(rest, acc, []uuid.UUID{t}, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// threadAudiences is who sees each conversation on its own: its shares and assignees.
func threadAudiences(ctx context.Context, q db.Querier, wid uuid.UUID, threadIDs []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	out := map[uuid.UUID][]uuid.UUID{}
	if len(threadIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT ts.thread_id, m.user_id FROM collab_shares ts
			JOIN members m ON m.workspace_id = ts.workspace_id AND (ts.everyone OR m.user_id = ts.user_id
				OR m.user_id IN (SELECT tm.user_id FROM team_members tm WHERE tm.team_id = ts.team_id))
			WHERE ts.workspace_id = $1 AND ts.thread_id = ANY($2)
		UNION
		SELECT ca.thread_id, ca.user_id FROM collab_assignees ca JOIN collab_threads t ON t.id = ca.thread_id
			WHERE t.workspace_id = $1 AND ca.thread_id = ANY($2)`, wid, threadIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t, u uuid.UUID
		if err := rows.Scan(&t, &u); err != nil {
			return nil, err
		}
		out[t] = appendNew(out[t], u)
	}
	return out, rows.Err()
}

// publishSharedThreadsChanged refreshes account preferences in conversations shared on
// their own. Each event names one conversation and reaches only its audience.
func (s *service) publishSharedThreadsChanged(ctx context.Context, tx pgx.Tx, wid, accountID uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT t.id FROM collab_threads t
		WHERE t.workspace_id = $1 AND t.account_id = $2 AND (
			EXISTS (SELECT 1 FROM collab_shares sh WHERE sh.thread_id = t.id)
			OR EXISTS (SELECT 1 FROM collab_assignees ca WHERE ca.thread_id = t.id))`, wid, accountID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.publishChanged(ctx, tx, wid, &accountID, []uuid.UUID{id}, false); err != nil {
			return err
		}
	}
	return nil
}

// publishAccounts tells who sees an account that it changed, plus extra people (the
// audience before a change of sharing), in tx.
func (s *service) publishAccounts(ctx context.Context, tx pgx.Tx, wid, accountID uuid.UUID, extra []uuid.UUID) error {
	users, err := accountAudience(ctx, tx, wid, accountID)
	if err != nil {
		return err
	}
	users = appendNew(users, extra...)
	_, err = events.Publish(ctx, tx, events.Event{WorkspaceID: wid, Type: EventAccounts, Users: users,
		Data: map[string]any{"account_id": accountID}})
	return err
}

// publishAccountsTo tells only these people that an account changed, in tx.
func (s *service) publishAccountsTo(ctx context.Context, tx pgx.Tx, wid, accountID uuid.UUID, users []uuid.UUID) error {
	_, err := events.Publish(ctx, tx, events.Event{WorkspaceID: wid, Type: EventAccounts, Users: users,
		Data: map[string]any{"account_id": accountID}})
	return err
}

func appendNew(list []uuid.UUID, add ...uuid.UUID) []uuid.UUID {
	for _, id := range add {
		if id != uuid.Nil && !slices.Contains(list, id) {
			list = append(list, id)
		}
	}
	return list
}

// Errors with a fixed answer.
var (
	errAudienceTooLarge = httpx.E(422, "validation_error", "Share with at most 50 teams and 200 people.")
	errUnknownTeam      = httpx.E(422, "validation_error", "A team you chose is not in this workspace.")
	errUnknownMember    = httpx.E(422, "validation_error", "A person you chose is not in this workspace.")
	// Invitations (GH#447): on accounts only.
	errInvitedThread  = httpx.E(422, "validation_error", "Share a conversation with people once they have joined.")
	errLabels         = httpx.E(422, "labels_unsupported", "IMAP accounts have no labels. Move the conversation to a folder instead.")
	errMoveToCategory = httpx.E(422, "validation_error", "An Outlook category is a label: add it with Label, or move to a folder.")
	errFolderColor    = httpx.E(422, "validation_error", "Only Gmail labels and Outlook categories have colours.")
	errCategoryRename = httpx.E(422, "rename_unsupported", "Outlook categories cannot be renamed. Create a new one instead.")
	errSpecialFolder  = httpx.E(422, "special_folder", "This folder belongs to the mail provider and cannot be changed.")
	errIMAPOff        = httpx.E(422, "imap_unavailable", "IMAP accounts are not available on this instance.")
	errConflict       = httpx.E(409, "account_conflict", "This address is already connected in this workspace.")
)

// errReconnect means the provider refused the stored grant or password: connect it again.
var errReconnect = errors.New("the account connection was refused. Connect it again")

// errProviderUnavailable is a passing provider failure; the job tries again.
var errProviderUnavailable = errors.New("the mail provider is not responding. We will try again")
