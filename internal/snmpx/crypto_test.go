package snmpx

import (
	"strings"
	"testing"
)

func TestCrypter_RoundTrip(t *testing.T) {
	c, err := NewCrypter("a-very-long-master-key-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{"", "public", "snmpv3-passphrase", strings.Repeat("x", 200)}
	for _, plain := range cases {
		blob, err := c.Encrypt(plain)
		if err != nil {
			t.Fatalf("encrypt %q: %v", plain, err)
		}
		got, err := c.Decrypt(blob)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if got != plain {
			t.Errorf("round-trip mismatch: got %q, want %q", got, plain)
		}
	}
}

func TestCrypter_DifferentBlobsForSamePlaintext(t *testing.T) {
	c, _ := NewCrypter("a-very-long-master-key-for-tests")
	a, _ := c.Encrypt("public")
	b, _ := c.Encrypt("public")
	if a == b {
		t.Error("encrypting the same plaintext twice produced identical blobs (salt or nonce reuse)")
	}
}

func TestCrypter_WrongMasterFails(t *testing.T) {
	a, _ := NewCrypter("master-A-must-be-long-enough")
	b, _ := NewCrypter("master-B-must-be-long-enough")
	blob, _ := a.Encrypt("secret")
	if _, err := b.Decrypt(blob); err == nil {
		t.Error("decrypt with wrong master should fail")
	}
}

func TestCrypter_MalformedBlobFails(t *testing.T) {
	c, _ := NewCrypter("master-key-must-be-long-enough")
	if _, err := c.Decrypt("!!not-base64!!"); err == nil {
		t.Error("decrypt of non-base64 should fail")
	}
	if _, err := c.Decrypt("c2hvcnQ="); err != ErrInvalid {
		t.Errorf("decrypt of short blob = %v, want ErrInvalid", err)
	}
}

func TestCrypter_RejectsShortMaster(t *testing.T) {
	if _, err := NewCrypter("short"); err == nil {
		t.Error("short master key should be rejected")
	}
}

func TestCrypter_EmptyRoundTrip(t *testing.T) {
	c, _ := NewCrypter("a-very-long-master-key-for-tests")
	blob, _ := c.Encrypt("")
	if blob != "" {
		t.Errorf("empty plaintext encrypted to %q, want ''", blob)
	}
	got, _ := c.Decrypt("")
	if got != "" {
		t.Errorf("empty blob decrypted to %q", got)
	}
}
