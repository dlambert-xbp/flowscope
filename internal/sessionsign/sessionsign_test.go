package sessionsign

import (
	"errors"
	"testing"
	"time"
)

func newSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := New("0123456789abcdef-pad-pad-pad-pad")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNewRejectsShortKey(t *testing.T) {
	if _, err := New("short"); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestRoundTrip(t *testing.T) {
	s := newSigner(t)
	in := Payload{
		ID:      "id-1",
		Subject: "user-42",
		Email:   "user@example.com",
		Scope:   "admin",
		Expires: time.Now().Add(time.Hour).Truncate(time.Second),
	}
	tok := s.Sign(in)
	out, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Subject != in.Subject || out.Email != in.Email || out.Scope != in.Scope || out.ID != in.ID {
		t.Errorf("payload mismatch: got %+v want %+v", out, in)
	}
	if !out.Expires.Equal(in.Expires) {
		t.Errorf("expires mismatch: got %v want %v", out.Expires, in.Expires)
	}
}

func TestExpiredReturnsErrExpired(t *testing.T) {
	s := newSigner(t)
	tok := s.Sign(Payload{
		Subject: "u",
		Scope:   "read",
		Expires: time.Now().Add(-time.Minute),
	})
	_, err := s.Verify(tok)
	if !errors.Is(err, ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
}

func TestTamperedReturnsErrInvalid(t *testing.T) {
	s := newSigner(t)
	tok := s.Sign(Payload{Subject: "u", Scope: "read", Expires: time.Now().Add(time.Hour)})
	// Flip a byte in the middle. RawURLEncoding alphabet — pick a
	// safe character swap.
	idx := len(tok) / 2
	mutated := tok[:idx] + string(rotate(tok[idx])) + tok[idx+1:]
	if mutated == tok {
		t.Fatalf("rotate produced same string; got %q", mutated)
	}
	_, err := s.Verify(mutated)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestWrongKeyReturnsErrInvalid(t *testing.T) {
	a := newSigner(t)
	b, err := New("zyxwvutsrqponml-pad-pad-pad-pad!")
	if err != nil {
		t.Fatalf("New(b): %v", err)
	}
	tok := a.Sign(Payload{Subject: "u", Scope: "read", Expires: time.Now().Add(time.Hour)})
	_, err = b.Verify(tok)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestEmptyReturnsErrInvalid(t *testing.T) {
	s := newSigner(t)
	if _, err := s.Verify(""); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// rotate returns a different ASCII byte from b, preserving the
// alphabet that base64 RawURLEncoding uses.
func rotate(b byte) byte {
	if b == 'A' {
		return 'B'
	}
	return 'A'
}
