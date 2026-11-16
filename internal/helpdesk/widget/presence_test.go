package widget_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/useopendesk/opendesk/internal/crypto"
	"github.com/useopendesk/opendesk/internal/domain"
	"github.com/useopendesk/opendesk/internal/helpdesk/hdtest"
	"github.com/useopendesk/opendesk/internal/helpdesk/tickets"
	"github.com/useopendesk/opendesk/internal/helpdesk/widget"
)

func TestVisitorPresence(t *testing.T) {
	e := hdtest.New(t, widget.Register, tickets.Register)
	ws := e.NewWorkspace()
	s := start(t, e, map[string]any{"key": ws.WidgetKey, "email": "here@example.com"})
	va := hdtest.Auth{Visitor: s.VisitorToken}
	var created struct {
		Conversation struct {
			ID uuid.UUID `json:"id"`
		} `json:"conversation"`
	}
	e.Do("POST", "/v1/widget/conversations", va, map[string]any{"body": "Hello"}).Want(t, 201).JSON(t, &created)
	path := "/v1/tickets/" + created.Conversation.ID.String() + "/visitor-presence"
	agent := ws.Agent.As(ws)

	var p domain.VisitorPresence
	// The address an anonymous visitor typed is unconfirmed: no reply is emailed to it.
	e.Do("GET", path, agent, nil).Want(t, 200).JSON(t, &p)
	if !p.Chat || !p.Present || p.EmailFollowUp {
		t.Fatalf("unconfirmed address: %+v", p)
	}
	e.Exec(`UPDATE contacts SET email_unconfirmed = false WHERE id = $1`, s.Contact.ID) // as a member's save confirms it
	e.Do("GET", path, agent, nil).Want(t, 200).JSON(t, &p)
	if !p.Chat || !p.Present || p.LastSeenAt == nil || p.AwayAfterSeconds != 120 || !p.EmailFollowUp {
		t.Fatalf("on the page: %+v", p)
	}

	// Quiet for longer than the follow-up's threshold: away, and a reply would be emailed.
	e.Exec(`UPDATE widget_sessions SET last_seen_at = now() - interval '3 minutes' WHERE contact_id = $1`, s.Contact.ID)
	e.Do("GET", path, agent, nil).Want(t, 200).JSON(t, &p)
	if !p.Chat || p.Present || p.LastSeenAt == nil || time.Since(*p.LastSeenAt) < 2*time.Minute || !p.EmailFollowUp {
		t.Fatalf("away: %+v", p)
	}
	// Back: any widget request.
	e.Do("GET", "/v1/widget/conversations/"+created.Conversation.ID.String()+"/messages", va, nil).Want(t, 200)
	e.Do("GET", path, agent, nil).Want(t, 200).JSON(t, &p)
	if !p.Present {
		t.Fatalf("back: %+v", p)
	}

	// Without an email, nothing is emailed; with the widget's follow-up off neither.
	e.Exec(`UPDATE contacts SET email = NULL WHERE id = $1`, s.Contact.ID)
	e.Do("GET", path, agent, nil).Want(t, 200).JSON(t, &p)
	if p.EmailFollowUp {
		t.Fatalf("no email: %+v", p)
	}

	// Not a chat: chat false. Others: 404, 401; a viewer reads it.
	var email uuid.UUID
	e.Scalar(`INSERT INTO tickets (workspace_id, number, subject, channel, status, brand_id)
		VALUES ($1, 99001, 'Mail', 'email', 'open', $2) RETURNING id`, &email, ws.ID, ws.BrandID)
	e.Do("GET", "/v1/tickets/"+email.String()+"/visitor-presence", agent, nil).Want(t, 200).JSON(t, &p)
	if p.Chat || p.Present || p.LastSeenAt != nil {
		t.Fatalf("email ticket: %+v", p)
	}
	e.Do("GET", "/v1/tickets/"+uuid.NewString()+"/visitor-presence", agent, nil).Want(t, 404)
	e.Do("GET", path, hdtest.Auth{}, nil).Want(t, 401)
	e.Do("GET", path, ws.Viewer.As(ws), nil).Want(t, 200)

	// Coming back tells the agents on each open chat, with its brand.
	evs, err := widget.PresenceEvents(e.Ctx, e.D, ws.ID, s.Contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].TicketID != created.Conversation.ID || evs[0].ContactID != s.Contact.ID || evs[0].Type != "customer.presence" ||
		evs[0].BrandID == nil || *evs[0].BrandID != ws.BrandID || evs[0].TeamID != nil {
		t.Fatalf("events %+v", evs)
	}
	e.Exec(`UPDATE tickets SET status = 'solved' WHERE id = $1`, created.Conversation.ID)
	if evs, err = widget.PresenceEvents(e.Ctx, e.D, ws.ID, s.Contact.ID); err != nil || len(evs) != 0 {
		t.Fatalf("solved chat: %+v %v", evs, err)
	}
}

func TestVisitorComeback(t *testing.T) {
	c := uuid.New()
	now := time.Now()
	if !widget.Comeback(c, now) {
		t.Fatal("first request is a coming back")
	}
	if widget.Comeback(c, now.Add(time.Minute)) {
		t.Fatal("a request a minute later is not")
	}
	if !widget.Comeback(c, now.Add(time.Minute+domain.ChatAwayAfter)) {
		t.Fatal("a request after the away threshold is")
	}
}

// Chat replies are emailed only to a confirmed address: a verified visitor's, never one an
// anonymous visitor typed, until mail comes from it.
func TestChatFollowUpNeedsConfirmedEmail(t *testing.T) {
	e := hdtest.New(t, widget.Register, tickets.Register)
	ws := e.NewWorkspace()
	sealed, err := e.D.Box.Seal([]byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	e.Exec(`UPDATE widgets SET secret_enc = $2 WHERE id = $1`, ws.WidgetID, sealed)
	chats := map[string]uuid.UUID{}
	followUp := func(s session) bool {
		t.Helper()
		id, ok := chats[s.VisitorToken]
		if !ok {
			var created struct {
				Conversation struct {
					ID uuid.UUID `json:"id"`
				} `json:"conversation"`
			}
			e.Do("POST", "/v1/widget/conversations", hdtest.Auth{Visitor: s.VisitorToken}, map[string]any{"body": "Hello"}).Want(t, 201).JSON(t, &created)
			id = created.Conversation.ID
			chats[s.VisitorToken] = id
		}
		e.Exec(`UPDATE widget_sessions SET last_seen_at = now() - interval '3 minutes' WHERE contact_id = $1`, s.Contact.ID)
		var p domain.VisitorPresence
		e.Do("GET", "/v1/tickets/"+id.String()+"/visitor-presence", ws.Agent.As(ws), nil).Want(t, 200).JSON(t, &p)
		return p.EmailFollowUp
	}
	anon := start(t, e, map[string]any{"key": ws.WidgetKey, "email": "someone@corp.example"})
	if anon.Contact.Email == nil || *anon.Contact.Email != "someone@corp.example" || followUp(anon) {
		t.Fatalf("anonymous visitor's typed address: %+v", anon.Contact)
	}
	// Mail from the address confirms it.
	err = e.D.DB.Tx(e.Ctx, func(tx pgx.Tx) error {
		_, _, err := domain.UpsertContactByEmail(e.Ctx, tx, ws.ID, "someone@corp.example", "Someone")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !followUp(anon) {
		t.Fatal("an address mail came from is still unconfirmed")
	}
	// An address given later in an anonymous session is unconfirmed too.
	later := start(t, e, map[string]any{"key": ws.WidgetKey})
	start(t, e, map[string]any{"key": ws.WidgetKey, "visitor_token": later.VisitorToken, "email": "later@corp.example"})
	if followUp(later) {
		t.Fatal("an address typed later redirects chat replies")
	}
	verified := start(t, e, map[string]any{"key": ws.WidgetKey, "user_id": "u-7", "user_hash": crypto.HMACHex([]byte("s3cret"), "u-7"),
		"email": "member@corp.example"})
	if !followUp(verified) {
		t.Fatal("a verified visitor's address is not emailed")
	}
}
