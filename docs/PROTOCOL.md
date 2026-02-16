# OpenNexus Secure Transport Protocol 0.1.0 (Draft Specification)

---

## 1. Scope

OpenNexus Protocol defines a hardened baseline end-to-end encrypted transport
for always-online, session-oriented AI agents across untrusted relays.

OpenNexus Protocol provides:

- End-to-end confidentiality against relays
- Mutual authentication
- Session-level forward secrecy
- Deterministic encoding (no JSON-in-crypto)
- Key separation (directional nonce safety)
- UKS protection
- Handshake freshness protection
- Oracle-safe reset behavior

OpenNexus Protocol is NOT a full messaging system (no ratchet, no offline prekeys, no groups).

All wire messages MUST include:

    protocol_version = "0.1.0"

Receivers MUST reject unsupported protocol versions.

---

## 2. Cryptographic Primitives

- Identity keys: Ed25519
- Key exchange: X25519
- KDF: HKDF-SHA256
- AEAD: AES-256-GCM (96-bit nonce)
- Hash: SHA-256

All crypto MUST use well-vetted libraries.

---

## 3. Agent Identity

agent_id = SHA-256(agent_public_key)  (exactly 32 bytes)

agent_id is treated as raw bytes for all cryptographic inputs.

---

## 4. Deterministic Encoding

encode(x) = uint16_be(len(x)) || x

All signature inputs and AEAD AAD MUST use deterministic length-prefixed encoding.
JSON MUST NOT be used inside signatures or AEAD inputs.

---

## 5. Session Eligibility

Agents MUST NOT establish secure sessions with themselves.

If self_agent_id == peer_agent_id, the connection MUST be immediately rejected.

This prevents undefined role behavior and avoids nonce-collision regressions.

---

## 6. Role & Write Key Semantics

Role is NOT tied to connection direction.

Key derivation produces two write keys:

    lower_id_write_key   = first 32 bytes
    higher_id_write_key  = next 32 bytes

Rule:

If agent_id_A < agent_id_B (lexicographic byte order):

    agent_A uses lower_id_write_key for encryption
    agent_B uses higher_id_write_key for encryption

Both sides MUST compute this independently.

---

## 7. Handshake

### 7.1 Hello (Initiating message)

Signature input:

    b"OpenNexus-v0-hello" ||
    encode(self_agent_id) ||
    encode(peer_agent_id) ||
    encode(self_ephemeral_public_key)

Signature = Ed25519.sign(signature_input)

Receiver MUST verify:

- agent_id == SHA-256(agent_public_key)
- signature validity
- peer_agent_id matches expected identity
- self_agent_id != peer_agent_id (reject self-session)

---

### 7.2 HelloAck (Freshness Protected)

Signature input:

    b"OpenNexus-v0-hello-ack" ||
    encode(self_agent_id) ||
    encode(peer_agent_id) ||
    encode(self_ephemeral_public_key) ||
    encode(peer_ephemeral_public_key)

Receiver MUST verify that peer_ephemeral_public_key matches the locally generated
ephemeral key for this handshake attempt. This prevents HelloAck replay blackholes.

---

## 8. Session Key Derivation (Transcript-Bound KDF)

shared_secret = X25519(ephemeral_priv, peer_ephemeral_pub)

ordered_ids = min(agent_id_A, agent_id_B) || max(agent_id_A, agent_id_B)

ordered_eph = min(ephemeral_pub_A, ephemeral_pub_B) || max(ephemeral_pub_A, ephemeral_pub_B)

salt = SHA-256(ordered_ids)

info = b"OpenNexus-v0-session" || ordered_eph

key_material = HKDF(
    input = shared_secret,
    salt = salt,
    info = info,
    length = 64 bytes
)

lower_id_write_key  = first 32 bytes
higher_id_write_key = next 32 bytes

session_id = SHA-256(ordered_ids || ordered_eph)

session_id MUST be exactly 32 bytes and MUST be validated on every received message.

---

## 9. Encrypted Data Transport

### 9.1 Nonce Format

AES-GCM nonce (96-bit):

    nonce = uint32_be(0) || uint64_be(counter)

Counter:

- Starts at 0 per direction
- MUST strictly increase
- Overflow MUST trigger rekey before wraparound

### 9.2 AAD

aad =
    b"OpenNexus-v0-data" ||
    encode(session_id) ||
    encode(sender_agent_id) ||
    encode(receiver_agent_id)

### 9.3 Encryption

ciphertext = AEAD_Encrypt(
    key = write_key,
    nonce = nonce,
    plaintext = payload,
    aad = aad
)

Message MUST be rejected if session_id mismatch occurs.

---

## 10. Reset Mechanism (Oracle-Safe)

Preferred path (if session key exists):

    reset_plaintext =
        b"OpenNexus-v0-reset" ||
        encode(session_id) ||
        encode(reason_enum)

    send as encrypted AEAD message

Fallback path (only if session key is lost):

- session_id MUST be 32 bytes
- reason_enum MUST be a predefined small integer
- peer_agent_id MUST be provided and MUST be 32 bytes

reset_sig_input =
    b"OpenNexus-v0-reset-signed" ||
    encode(self_agent_id) ||
    encode(peer_agent_id) ||
    encode(session_id) ||
    encode(reason_enum)

reset_signature = Ed25519.sign(reset_sig_input)

Receiver MUST verify structure, lengths, and signature before accepting.

---

## 11. Security Guarantees

OpenNexus Protocol provides:

- End-to-end confidentiality against relays
- Mutual authentication
- Forward secrecy per session
- Deterministic encoding (interoperable)
- Directional nonce safety via key separation
- UKS protection via peer binding
- Freshness protection against HelloAck replay
- Oracle-safe reset behavior
- Transcript-bound KDF (IDs + ephemerals)

---

Protocol: opennexus-secure-transport
Version: 0.1.0
Status: Draft
