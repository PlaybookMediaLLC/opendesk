// Package crypto holds secret handling: AES-GCM sealing, token generation and hashing.
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

// Box seals small secrets (mailbox credentials, webhook secrets) with AES-256-GCM. It seals
// with the current key and opens with the current key or any previous one, so the key can
// be replaced while values sealed with the one before it still open (CREDENTIALS_KEY and
// CREDENTIALS_KEY_PREVIOUS; internal/sealed re-encrypts them with the current key).
type Box struct {
	aead     cipher.AEAD
	previous []cipher.AEAD
}

// ErrUnknownKey is a sealed value that none of the box's keys opens.
var ErrUnknownKey = errors.New("crypto: sealed with none of the configured keys")

// NewBox returns a box that seals with key and also opens what previous keys sealed.
func NewBox(key []byte, previous ...[]byte) (*Box, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	b := &Box{aead: aead}
	for _, k := range previous {
		p, err := newAEAD(k)
		if err != nil {
			return nil, err
		}
		b.previous = append(b.previous, p)
	}
	return b, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("crypto: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal returns nonce || ciphertext, always under the current key.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open opens a value sealed with the current key or a previous one.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	plain, _, err := b.open(sealed)
	return plain, err
}

// Rotating reports whether the box has previous keys to move values off.
func (b *Box) Rotating() bool { return len(b.previous) > 0 }

// Reseal returns sealed under the current key when a previous key opened it, and nil when
// the current key already does. A value no key opens is ErrUnknownKey.
func (b *Box) Reseal(sealed []byte) ([]byte, error) {
	plain, current, err := b.open(sealed)
	if err != nil || current {
		return nil, err
	}
	return b.Seal(plain)
}

// open reports whether the current key was the one that opened sealed.
func (b *Box) open(sealed []byte) ([]byte, bool, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, false, errors.New("crypto: ciphertext too short")
	}
	if plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil); err == nil {
		return plain, true, nil
	}
	for _, p := range b.previous {
		if plain, err := p.Open(nil, sealed[:n], sealed[n:], nil); err == nil {
			return plain, false, nil
		}
	}
	return nil, false, ErrUnknownKey
}

// boundPrefix starts a value sealed for one row (SealFor): a format version that a value
// from Seal (nonce || ciphertext) starts with only by chance. Such a value fails the bound
// reading's authentication and is then opened as one from Seal.
var boundPrefix = []byte{0x00, 'o', 'd', 0x02}

// RowAAD names where a sealed value lives (its table, column and row key), so SealFor binds
// the value to that place: the same ciphertext copied to another row or column does not open.
func RowAAD(table, column, key string) []byte {
	return []byte(table + "\x00" + column + "\x00" + key)
}

// boundAAD is the associated data a bound value is sealed with: the format and the place.
func boundAAD(aad []byte) []byte {
	return append(append(make([]byte, 0, len(boundPrefix)+len(aad)), boundPrefix...), aad...)
}

// SealFor returns boundPrefix || nonce || ciphertext under the current key, with aad (RowAAD)
// authenticated alongside: only OpenFor with the same aad opens it.
func (b *Box) SealFor(aad, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	out := append(append(make([]byte, 0, len(boundPrefix)+len(nonce)+len(plaintext)+b.aead.Overhead()), boundPrefix...), nonce...)
	return b.aead.Seal(out, nonce, plaintext, boundAAD(aad)), nil
}

// OpenFor opens a value SealFor sealed with the same aad, under the current key or a
// previous one. A value Seal sealed (before the column was bound to its rows) opens too,
// until the key rotation seals it again for its row (ResealFor).
func (b *Box) OpenFor(aad, sealed []byte) ([]byte, error) {
	plain, _, _, err := b.openFor(aad, sealed)
	return plain, err
}

// ResealFor returns sealed as SealFor(aad) under the current key, or nil when it already is
// that. A value no key opens is ErrUnknownKey.
func (b *Box) ResealFor(aad, sealed []byte) ([]byte, error) {
	plain, current, bound, err := b.openFor(aad, sealed)
	if err != nil || (current && bound) {
		return nil, err
	}
	return b.SealFor(aad, plain)
}

// openFor reports whether the current key opened sealed and whether it was bound to aad.
func (b *Box) openFor(aad, sealed []byte) (plain []byte, current, bound bool, err error) {
	n := b.aead.NonceSize()
	if rest, ok := bytes.CutPrefix(sealed, boundPrefix); ok && len(rest) >= n {
		ad := boundAAD(aad)
		if plain, err := b.aead.Open(nil, rest[:n], rest[n:], ad); err == nil {
			return plain, true, true, nil
		}
		for _, p := range b.previous {
			if plain, err := p.Open(nil, rest[:n], rest[n:], ad); err == nil {
				return plain, false, true, nil
			}
		}
	}
	plain, current, err = b.open(sealed)
	return plain, current, false, err
}

// SealJSON encrypts v as JSON.
func (b *Box) SealJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b.Seal(raw)
}

// OpenJSON decrypts into v. A nil input leaves v untouched.
func (b *Box) OpenJSON(sealed []byte, v any) error {
	if len(sealed) == 0 {
		return nil
	}
	raw, err := b.Open(sealed)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// RandomToken returns n random bytes, base64url without padding.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomString returns a lowercase alphanumeric string of length n.
func RandomString(n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// HashToken is the stored form of an opaque credential.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// HMACHex returns hex(HMAC-SHA256(key, msg)).
func HMACHex(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// EqualHex compares two hex strings in constant time.
func EqualHex(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
