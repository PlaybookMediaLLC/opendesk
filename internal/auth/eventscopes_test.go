package auth

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestCanReadEvent(t *testing.T) {
	key := func(scopes ...string) *Principal { return &Principal{Kind: KindAPIKey, Scopes: scopes} }
	cases := []struct {
		p    *Principal
		typ  string
		want bool
	}{
		{&Principal{Kind: KindUser}, "contact.updated", true},
		{&Principal{Kind: KindUser}, "workspace.updated", true},
		{key(ScopeTicketsRead), "ticket.updated", true},
		{key(ScopeTicketsRead), "message.created", true},
		{key(ScopeTicketsRead), "contact.updated", false},
		{key(ScopeTicketsRead), "member.updated", false},
		{key(ScopeTicketsRead), "workspace.updated", false},
		{key(ScopeTicketsRead), "kb_article.created", false},
		{key(ScopeTicketsRead), "tag.created", true},
		{key(ScopeContactsRead), "contact.updated", true},
		{key(ScopeContactsRead), "company.deleted", true},
		{key(ScopeContactsRead), "ticket.created", false},
		{key(ScopeKBRead), "kb_article.updated", true},
		{key(ScopeWikiRead), "wiki_page.moved", true},
		{key(ScopeSettingsRead), "workspace.updated", true},
		{key(ScopeSettingsRead), "some_new_resource.created", true},
		{key(ScopeSettingsRead), "ticket.created", false},
		{key(ScopeRealtime), "ticket.created", false},
		{key(ScopeRealtime), "event.hidden", true},
		{key(ScopeTicketsRead), "some_new_resource.created", false},
		// A person acting through an OAuth grant is bound by its scopes too.
		{&Principal{Kind: KindUser, GrantID: uuid.New(), Scopes: []string{ScopeTicketsRead}}, "contact.created", false},
		{&Principal{Kind: KindUser, GrantID: uuid.New(), Scopes: []string{ScopeTicketsRead}}, "ticket.created", true},
	}
	for _, c := range cases {
		if got := c.p.CanReadEvent(c.typ); got != c.want {
			t.Errorf("%s %v reading %s: %v, want %v", c.p.Kind, c.p.Scopes, c.typ, got, c.want)
		}
	}
}

func TestEventFilterMatchesCanReadEvent(t *testing.T) {
	if (&Principal{Kind: KindUser}).EventFilter() != nil {
		t.Fatal("a person signed in has no events claim")
	}
	types := []string{"ticket.created", "contact.updated", "tag.merged", "kb_article.created", "wiki_comment.changed",
		"changelog_entry.updated", "member.added", "workspace.updated", "brand.created", "future_thing.created"}
	for _, scopes := range [][]string{
		{ScopeRealtime}, {ScopeTicketsRead}, {ScopeContactsRead, ScopeRealtime}, {ScopeSettingsRead},
		{ScopeSettingsRead, ScopeTicketsRead}, {ScopeKBRead, ScopeWikiRead, ScopeChangelogRead},
	} {
		p := &Principal{Kind: KindAPIKey, Scopes: scopes}
		f := p.EventFilter()
		if f == nil || f.Resources == nil {
			t.Fatalf("%v: no events claim", scopes)
		}
		for _, typ := range types {
			listed := slices.Contains(f.Resources, EventResource(typ))
			if got := listed == f.Only; got != p.CanReadEvent(typ) {
				t.Errorf("%v: claim says %v for %s, CanReadEvent %v", scopes, got, typ, p.CanReadEvent(typ))
			}
		}
	}
}
