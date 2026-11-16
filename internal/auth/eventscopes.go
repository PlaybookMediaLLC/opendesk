package auth

import (
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// eventScopes is the one table of which read scopes let a scoped credential (an API key, or
// a person acting through an OAuth grant) receive an event: an event's resource, its type up
// to the first dot, maps to the scopes its REST reads accept, any one of which is enough
// (contracts/events.md "Who receives an event"). A resource not listed needs settings:read.
// The replay (GET /v1/events) and the gateway (the ws token's events claim) both read it.
var eventScopes = map[string][]string{
	"ticket":             {ScopeTicketsRead},
	"message":            {ScopeTicketsRead},
	"activity":           {ScopeTicketsRead},
	"thread":             {ScopeTicketsRead},
	"ticket_draft":       {ScopeTicketsRead},
	"reply_draft":        {ScopeTicketsRead},
	"ticket_link":        {ScopeTicketsRead},
	"ticket_panel":       {ScopeTicketsRead},
	"ai_suggested_reply": {ScopeTicketsRead},
	"ai_agent_review":    {ScopeTicketsRead},
	"view":               {ScopeTicketsRead},
	"saved_reply":        {ScopeTicketsRead},
	"notification":       {ScopeTicketsRead},
	"contact":            {ScopeContactsRead},
	"company":            {ScopeContactsRead},
	"tag":                {ScopeSettingsRead, ScopeTicketsRead, ScopeContactsRead},
	"tag_group":          {ScopeSettingsRead, ScopeTicketsRead, ScopeContactsRead},
	"custom_field":       {ScopeSettingsRead, ScopeTicketsRead, ScopeContactsRead},
	"kb_article":         {ScopeKBRead},
	"kb_collection":      {ScopeKBRead},
	"changelog_entry":    {ScopeChangelogRead},
	"changelog_category": {ScopeChangelogRead},
	"wiki_page":          {ScopeWikiRead},
	"wiki_comment":       {ScopeWikiRead},
}

// hiddenEventType is the stand-in every reader receives in place of an event it may not
// read (events.HiddenType); it carries nothing.
const hiddenEventType = "event.hidden"

// EventResource is an event type's resource: the type up to its first dot.
func EventResource(typ string) string {
	if i := strings.IndexByte(typ, '.'); i >= 0 {
		return typ[:i]
	}
	return typ
}

// EventScopes returns the read scopes, any one of which lets a scoped credential receive an
// event of type typ.
func EventScopes(typ string) []string {
	if s, ok := eventScopes[EventResource(typ)]; ok {
		return s
	}
	return []string{ScopeSettingsRead}
}

// ScopedByGrant reports a caller whose scopes bound what it reads: an API key, or a person
// acting through an OAuth grant.
func (p *Principal) ScopedByGrant() bool {
	return p.Kind == KindAPIKey || (p.Kind == KindUser && p.GrantID != uuid.Nil)
}

// CanReadEvent reports whether the caller may receive an event of type typ in full. A person
// signed in receives every event the workspace's other rules give them (private teams,
// people-scoped events); a scoped credential only those its scopes read (EventScopes).
func (p *Principal) CanReadEvent(typ string) bool {
	if !p.ScopedByGrant() || typ == hiddenEventType {
		return true
	}
	return slices.ContainsFunc(EventScopes(typ), p.HasScope)
}

// EventFilter is a ws token's events claim (contracts/tokens.md): with Only, the socket
// receives in full only events of the listed resources; without it, every event except
// those. Any other event reaches it as the event.hidden stand-in.
type EventFilter struct {
	Only      bool     `json:"only"`
	Resources []string `json:"resources"`
}

// EventFilter returns the events claim for the caller's ws token, or nil when the caller
// receives every event (a person signed in).
func (p *Principal) EventFilter() *EventFilter {
	if !p.ScopedByGrant() {
		return nil
	}
	f := &EventFilter{Only: !p.HasScope(ScopeSettingsRead), Resources: []string{}}
	for resource, scopes := range eventScopes {
		readable := slices.ContainsFunc(scopes, p.HasScope)
		// Only: list what it reads. Otherwise (it reads everything unlisted): list what it does not.
		if readable == f.Only {
			f.Resources = append(f.Resources, resource)
		}
	}
	sort.Strings(f.Resources)
	return f
}
