package core

import (
	"strings"
	"testing"
)

// TestSignupClosed: with SIGNUP_ENABLED off, an instance that has accounts takes a new one
// only through an invitation, and its sign-in options say sign-up is closed. (The instance's
// very first account registers anyway; the shared test database always has accounts.)
func TestSignupClosed(t *testing.T) {
	e := newEnv(t)
	owner, _ := e.signup("owner")
	owner.newWorkspace("Closed Co")
	e.d.Cfg.SignupEnabled = false
	t.Cleanup(func() { e.d.Cfg.SignupEnabled = true })

	if r := e.client().do("GET", "/v1/auth/config", nil); r.Status != 200 || r.Body["signup_enabled"] != false {
		t.Fatalf("config: %d %s", r.Status, r.Raw)
	}
	if first, err := firstAccount(t.Context(), e.d.DB.Pool); err != nil || first {
		t.Fatalf("firstAccount with accounts: %v %v", first, err)
	}
	r := e.client().do("POST", "/v1/auth/register", map[string]any{"email": uniqueEmail(t, "stranger"), "password": goodPassword, "name": "Stranger"})
	if r.Status != 403 || r.str("code") != "signup_disabled" {
		t.Fatalf("register: %d %s", r.Status, r.Raw)
	}

	// An invitation still opens it.
	email := uniqueEmail(t, "invited")
	if r := owner.do("POST", "/v1/invitations", map[string]any{"email": email, "role": "agent"}); r.Status != 201 {
		t.Fatalf("invite: %d %s", r.Status, r.Raw)
	}
	_, text := e.lastMail(email)
	tok := tokenFromText(text, "/invite?token=")
	if tok == "" || !strings.Contains(text, "/invite?token=") {
		t.Fatalf("no invitation link in %q", text)
	}
	r = e.client().do("POST", "/v1/auth/register", map[string]any{"email": email, "password": goodPassword, "name": "Invited", "invite_token": tok})
	if r.Status != 200 {
		t.Fatalf("invited register: %d %s", r.Status, r.Raw)
	}
}
