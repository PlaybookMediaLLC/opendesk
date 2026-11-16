package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestBoxOpensWithPreviousKeys(t *testing.T) {
	a, _ := NewBox(key('a'))
	c, _ := NewBox(key('c'))
	sealedA, err := a.Seal([]byte("secret-a"))
	if err != nil {
		t.Fatal(err)
	}
	sealedC, _ := c.Seal([]byte("secret-c"))

	b, err := NewBox(key('b'), key('c'), key('a'))
	if err != nil {
		t.Fatal(err)
	}
	if !b.Rotating() || a.Rotating() {
		t.Fatal("Rotating is whether previous keys are set")
	}
	for in, want := range map[string][]byte{"secret-a": sealedA, "secret-c": sealedC} {
		got, err := b.Open(want)
		if err != nil || string(got) != in {
			t.Fatalf("open %s: %q %v", in, got, err)
		}
	}
	// New values are sealed with the current key only.
	sealedB, _ := b.Seal([]byte("secret-b"))
	if _, err := a.Open(sealedB); err == nil {
		t.Fatal("a previous key opened a new value")
	}
	if only, _ := NewBox(key('b')); func() bool { p, err := only.Open(sealedB); return err != nil || string(p) != "secret-b" }() {
		t.Fatal("the current key alone does not open a new value")
	}

	// Reseal: nil for current values, the current key for previous ones.
	if next, err := b.Reseal(sealedB); err != nil || next != nil {
		t.Fatalf("reseal current: %v %v", next, err)
	}
	next, err := b.Reseal(sealedA)
	if err != nil || next == nil {
		t.Fatalf("reseal previous: %v", err)
	}
	only, _ := NewBox(key('b'))
	if p, err := only.Open(next); err != nil || string(p) != "secret-a" {
		t.Fatalf("resealed value with the current key alone: %q %v", p, err)
	}

	// A value no key opens is ErrUnknownKey, and so is Open's answer.
	d, _ := NewBox(key('d'))
	sealedD, _ := d.Seal([]byte("secret-d"))
	if _, err := b.Reseal(sealedD); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("reseal unknown: %v", err)
	}
	if _, err := b.Open(sealedD); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("open unknown: %v", err)
	}
	if _, err := NewBox(key('b'), []byte("short")); err == nil {
		t.Fatal("a short previous key was accepted")
	}
}

func TestSealForBindsTheValueToItsRow(t *testing.T) {
	b, _ := NewBox(key('a'))
	here := RowAAD("webhooks", "secret_enc", "0192f0c4-0000-7000-8000-000000000001")
	there := RowAAD("webhooks", "secret_enc", "0192f0c4-0000-7000-8000-000000000002")
	sealed, err := b.SealFor(here, []byte("whsec_x"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.OpenFor(here, sealed); err != nil || string(got) != "whsec_x" {
		t.Fatalf("open in its row: %q %v", got, err)
	}
	for name, aad := range map[string][]byte{
		"another row":    there,
		"another column": RowAAD("webhooks", "headers_enc", "0192f0c4-0000-7000-8000-000000000001"),
		"no place":       nil,
	} {
		if _, err := b.OpenFor(aad, sealed); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("opened in %s: %v", name, err)
		}
	}
	if _, err := b.Open(sealed); err == nil {
		t.Error("a bound value opened without its place")
	}
	// A value sealed before its column was bound still opens, wherever it is read.
	legacy, _ := b.Seal([]byte("old"))
	if got, err := b.OpenFor(here, legacy); err != nil || string(got) != "old" {
		t.Fatalf("legacy value: %q %v", got, err)
	}
}

func TestResealForMovesValuesToTheBoundFormatAndCurrentKey(t *testing.T) {
	old, _ := NewBox(key('a'))
	aad := RowAAD("webhooks", "secret_enc", "row-1")
	legacyOld, _ := old.Seal([]byte("one"))
	boundOld, _ := old.SealFor(aad, []byte("two"))

	b, _ := NewBox(key('b'), key('a'))
	legacyCurrent, _ := b.Seal([]byte("three"))
	boundCurrent, _ := b.SealFor(aad, []byte("four"))

	if next, err := b.ResealFor(aad, boundCurrent); err != nil || next != nil {
		t.Fatalf("a bound value under the current key is left alone: %v %v", next, err)
	}
	for want, sealed := range map[string][]byte{"one": legacyOld, "two": boundOld, "three": legacyCurrent} {
		next, err := b.ResealFor(aad, sealed)
		if err != nil || next == nil {
			t.Fatalf("%s: %v %v", want, next, err)
		}
		if !bytes.HasPrefix(next, boundPrefix) {
			t.Fatalf("%s not resealed in the bound format", want)
		}
		only, _ := NewBox(key('b'))
		if got, err := only.OpenFor(aad, next); err != nil || string(got) != want {
			t.Fatalf("%s after reseal: %q %v", want, got, err)
		}
		if _, err := only.OpenFor(RowAAD("webhooks", "secret_enc", "row-2"), next); err == nil {
			t.Fatalf("%s opened in another row after reseal", want)
		}
	}
	if _, err := b.ResealFor(aad, []byte("short")); err == nil {
		t.Fatal("garbage resealed")
	}
}
