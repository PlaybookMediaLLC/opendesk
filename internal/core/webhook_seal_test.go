package core

import (
	"testing"

	"github.com/useopendesk/opendesk/internal/ids"
)

// A webhook's sealed secret and headers open only in the row they were sealed for; values
// sealed before the columns were bound to their rows still open.
func TestWebhookSealsAreBoundToTheirRow(t *testing.T) {
	e := newEnv(t)
	a, b := ids.New(), ids.New()
	sealed, err := e.s.sealWebhookSecret(a, "whsec_test")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.openWebhookSecret(a, sealed); err != nil || string(got) != "whsec_test" {
		t.Fatalf("own row: %q %v", got, err)
	}
	if _, err := e.s.openWebhookSecret(b, sealed); err == nil {
		t.Fatal("a secret opened in another webhook's row")
	}
	headers, err := e.s.sealWebhookHeaders(a, map[string]string{"Authorization": "Bearer x"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.openWebhookHeaders(a, headers); err != nil || got["Authorization"] != "Bearer x" {
		t.Fatalf("own headers: %v %v", got, err)
	}
	if _, err := e.s.openWebhookHeaders(b, headers); err == nil {
		t.Fatal("headers opened in another webhook's row")
	}
	// The secret's ciphertext is not the headers' either.
	if _, err := e.s.openWebhookHeaders(a, sealed); err == nil {
		t.Fatal("a secret opened as the same row's headers")
	}
	legacy, _ := e.d.Box.Seal([]byte("whsec_old"))
	if got, err := e.s.openWebhookSecret(b, legacy); err != nil || string(got) != "whsec_old" {
		t.Fatalf("legacy secret: %q %v", got, err)
	}
}
