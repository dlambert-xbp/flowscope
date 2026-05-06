"""
SNMPv3 passphrase encryption for FlowScope.

Wraps AES-256-GCM with a master key derived (HKDF-SHA256) from the
FLOWSCOPE_SNMP_KEY environment variable. The master key never lands on
disk; only ciphertext goes to SQLite.

Threat model (from CLAUDE.md "Working agreement" + Phase 2 design):
  - The FlowScope dashboard has no per-row auth on credential reads, so
    even with FLOWSCOPE_AUTH_TOKEN set we MUST NOT return v3 passphrases
    in API responses. Encryption-at-rest + redaction-on-GET are the two
    layered controls.
  - If FLOWSCOPE_SNMP_KEY is unset on a host that already has stored v3
    profiles, decryption fails loudly: the poller logs an error and
    skips those profiles rather than reading garbage.
  - HKDF salt is fixed (per-deployment) so the same master key produces
    the same derived key on every restart — required since we re-decrypt
    existing rows on each boot.

Stored blob format: [12-byte nonce][AES-GCM ciphertext + 16-byte tag].
"""

import os

from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF


ENV_KEY    = "FLOWSCOPE_SNMP_KEY"
HKDF_SALT  = b"flowscope:snmp:v1"
HKDF_INFO  = b"snmp profile passphrases"
NONCE_LEN  = 12


class CryptoNotConfigured(Exception):
    """FLOWSCOPE_SNMP_KEY is unset or empty."""


class CryptoBadInput(Exception):
    """Stored blob is malformed (wrong length, tag mismatch, etc.)."""


_master_key_cache = None


def _master_key():
    """Derive the AES-256 key from the env var. Cached because HKDF is
    deterministic for a given input and we need the same key on every call."""
    global _master_key_cache
    if _master_key_cache is not None:
        return _master_key_cache
    raw = os.environ.get(ENV_KEY, "")
    if not raw:
        raise CryptoNotConfigured(f"{ENV_KEY} is not set")
    _master_key_cache = HKDF(
        algorithm=hashes.SHA256(),
        length=32,
        salt=HKDF_SALT,
        info=HKDF_INFO,
    ).derive(raw.encode("utf-8"))
    return _master_key_cache


def is_configured():
    return bool(os.environ.get(ENV_KEY, ""))


def encrypt(plaintext):
    """Encrypt a string passphrase. Pass-through for None/empty."""
    if plaintext is None or plaintext == "":
        return None
    key = _master_key()
    nonce = os.urandom(NONCE_LEN)
    ct = AESGCM(key).encrypt(nonce, plaintext.encode("utf-8"), None)
    return nonce + ct


def decrypt(blob):
    """Decrypt a stored blob back to a string. Pass-through for None.
    Raises CryptoBadInput if the blob is malformed (wrong length, tag fails)."""
    if blob is None:
        return None
    if len(blob) < NONCE_LEN + 16:
        raise CryptoBadInput("blob too short to be valid AES-GCM ciphertext")
    key = _master_key()
    nonce, ct = blob[:NONCE_LEN], blob[NONCE_LEN:]
    try:
        return AESGCM(key).decrypt(nonce, ct, None).decode("utf-8")
    except Exception as e:
        raise CryptoBadInput(f"decrypt failed: {type(e).__name__}") from e
