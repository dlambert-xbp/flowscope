package snmpx

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// Per VISION.md §4.2: SNMP v3 passphrases are encrypted at rest with
// AES-256-GCM. The 32-byte AEAD key is derived via HKDF-SHA256 from
// FLOWSCOPE_SNMP_KEY (the master key, sourced from Azure Key Vault
// in production).
//
// Ciphertext layout on the wire (and as stored in ClickHouse):
//
//	base64( salt[16] | nonce[12] | ciphertext | tag[16] )
//
// The salt is per-record so two encryptions of the same plaintext
// produce different blobs. The nonce is per-record too (random),
// which is what AES-GCM requires for confidentiality + integrity.
//
// Empty plaintext encrypts to an empty string ("") so we don't store
// blobs for fields the operator left blank (e.g. v2c targets have
// no v3 passphrases). Decrypt of "" returns "" too.

// Crypter encrypts and decrypts secrets using a master key. The
// master is loaded once at startup from FLOWSCOPE_SNMP_KEY; the
// per-record AEAD key is derived via HKDF on each call.
type Crypter struct {
	master []byte
}

// NewCrypter validates the master key and returns a ready-to-use
// Crypter. The master must be at least 16 bytes (128 bits of
// entropy); production deployments should use 32+ bytes from
// `openssl rand -base64 32` or equivalent.
func NewCrypter(masterKey string) (*Crypter, error) {
	if len(masterKey) < 16 {
		return nil, fmt.Errorf("snmpx: master key must be ≥ 16 bytes (got %d)", len(masterKey))
	}
	return &Crypter{master: []byte(masterKey)}, nil
}

// Encrypt seals plaintext under a fresh random salt + nonce. Returns
// "" for "" so the caller does not need a special case for empty
// fields.
func (c *Crypter) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("snmpx: read salt: %w", err)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("snmpx: read nonce: %w", err)
	}
	key := hkdfSHA256(c.master, salt, []byte("flowscope.snmp.v1"), 32)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)

	out := make([]byte, 0, len(salt)+len(nonce)+len(ct))
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt opens a blob previously produced by Encrypt with the same
// master key. Returns ErrInvalid for malformed input, including
// blobs encrypted under a different master.
func (c *Crypter) Decrypt(blob string) (string, error) {
	if blob == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", fmt.Errorf("snmpx: base64: %w", err)
	}
	if len(raw) < 16+12+16 { // salt + nonce + tag
		return "", ErrInvalid
	}
	salt := raw[:16]
	nonce := raw[16:28]
	ct := raw[28:]

	key := hkdfSHA256(c.master, salt, []byte("flowscope.snmp.v1"), 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", ErrInvalid
	}
	return string(pt), nil
}

// ErrInvalid is returned by Decrypt when the blob is malformed,
// truncated, or decrypts under the wrong master key.
var ErrInvalid = errors.New("snmpx: invalid ciphertext or wrong master key")

// hkdfSHA256 implements RFC 5869 in stdlib (no x/crypto dependency).
// Used here only to expand the master key by per-record salt; it is
// the standard pairing for AES-GCM at-rest encryption.
func hkdfSHA256(secret, salt, info []byte, outLen int) []byte {
	// Extract step: PRK = HMAC-SHA256(salt, secret)
	mac := hmac.New(sha256.New, salt)
	mac.Write(secret)
	prk := mac.Sum(nil)

	// Expand step: T(i) = HMAC-SHA256(PRK, T(i-1) || info || byte(i))
	out := make([]byte, 0, outLen)
	var prev []byte
	for i := byte(1); len(out) < outLen; i++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(prev)
		mac.Write(info)
		mac.Write([]byte{i})
		prev = mac.Sum(nil)
		out = append(out, prev...)
	}
	return out[:outLen]
}
