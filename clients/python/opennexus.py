"""
OpenNexus Agent Client
OpenNexus Protocol 0.1.0 (Draft) reference client implementation.
Usage:
    python opennexus.py generate-keys
    python opennexus.py send --to <peer_public_key> --messenger-url <url> --message "Hello"
    python opennexus.py stream
    python opennexus.py interactive
    python opennexus.py   # defaults to interactive mode
"""

import argparse
import base64
import hashlib
import json
import os
import struct
import sys
import time
import signal
import threading
from typing import Dict, Optional, Tuple

import requests
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ed25519, x25519
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

DEFAULT_MESSENGER = os.environ.get(
    "MESSENGER_URL",
    "https://api.opennexus.cc",
)
PROTOCOL_VERSION = "0.1.0"

MSG_TYPE_HELLO = "hello"
MSG_TYPE_HELLO_ACK = "hello_ack"
MSG_TYPE_DATA = "data"
MSG_TYPE_RESET = "reset"

TAG_HELLO = b"OpenNexus-v0-hello"
TAG_HELLO_ACK = b"OpenNexus-v0-hello-ack"
TAG_DATA = b"OpenNexus-v0-data"
TAG_SESSION = b"OpenNexus-v0-session"
TAG_RESET = b"OpenNexus-v0-reset"
TAG_RESET_SIGNED = b"OpenNexus-v0-reset-signed"

RESET_REASON_DECRYPTION_FAILED = 1
RESET_REASON_SESSION_MISMATCH = 2
RESET_REASON_UNKNOWN = 255


def b64e(data: bytes) -> str:
    return base64.b64encode(data).decode()


def b64d(value: str) -> bytes:
    return base64.b64decode(value)


def sha256(data: bytes) -> bytes:
    return hashlib.sha256(data).digest()


def lp_encode(data: bytes) -> bytes:
    if len(data) > 0xFFFF:
        raise ValueError("field too large for uint16 length prefix")
    return struct.pack(">H", len(data)) + data


def parse_lp_sequence(data: bytes) -> list:
    idx = 0
    out = []
    while idx < len(data):
        if idx + 2 > len(data):
            raise ValueError("invalid length-prefixed sequence")
        n = int.from_bytes(data[idx : idx + 2], "big")
        idx += 2
        if idx + n > len(data):
            raise ValueError("invalid length-prefixed sequence")
        out.append(data[idx : idx + n])
        idx += n
    return out


def reason_to_bytes(reason_enum: int) -> bytes:
    if reason_enum < 0 or reason_enum > 0xFFFF:
        raise ValueError("reason_enum out of range")
    return reason_enum.to_bytes(2, "big")


class OpenNexusClient:
    """OpenNexus Protocol 0.1.0 (Draft) client."""

    def __init__(self, identity_public_key=None, identity_private_key=None, messenger_url=None):
        self.identity_public_key = identity_public_key
        self.identity_private_key = identity_private_key
        self.messenger_url = messenger_url or DEFAULT_MESSENGER

        self.identity_public_key_bytes = b64d(self.identity_public_key)
        self.identity_private_key_bytes = b64d(self.identity_private_key)
        self.agent_id_bytes = sha256(self.identity_public_key_bytes)
        self.agent_id = b64e(self.agent_id_bytes)

        self.sessions: Dict[str, dict] = {}
        self.peer_messenger_urls: Dict[str, str] = {}

        safe_id = self.agent_id_bytes[:8].hex()
        self.session_keys_file = f"session_keys_{safe_id}.json"
        self._load_session_keys()

    def _load_session_keys(self):
        self.sessions = {}
        self.peer_messenger_urls = {}

        if not os.path.exists(self.session_keys_file):
            return

        try:
            with open(self.session_keys_file, "r") as f:
                data = json.load(f)
        except Exception:
            return

        if isinstance(data.get("peer_messenger_urls"), dict):
            self.peer_messenger_urls = data["peer_messenger_urls"]

        sessions = data.get("sessions", {})
        if not isinstance(sessions, dict):
            return

        for peer_agent_id, raw in sessions.items():
            try:
                session = {
                    "session_id": b64d(raw["session_id"]),
                    "tx_key": b64d(raw["tx_key"]),
                    "rx_key": b64d(raw["rx_key"]),
                    "tx_counter": int(raw.get("tx_counter", 0)),
                    "rx_counter": int(raw.get("rx_counter", -1)),
                    "peer_public_key": raw.get("peer_public_key", ""),
                    "local_ephemeral_public_key": raw.get("local_ephemeral_public_key", ""),
                    "peer_ephemeral_public_key": raw.get("peer_ephemeral_public_key", ""),
                }
                if len(session["session_id"]) != 32:
                    continue
                if len(session["tx_key"]) != 32 or len(session["rx_key"]) != 32:
                    continue
                self.sessions[peer_agent_id] = session
            except Exception:
                continue

    def _save_session_keys(self):
        payload = {
            "sessions": {
                peer_agent_id: {
                    "session_id": b64e(state["session_id"]),
                    "tx_key": b64e(state["tx_key"]),
                    "rx_key": b64e(state["rx_key"]),
                    "tx_counter": state["tx_counter"],
                    "rx_counter": state["rx_counter"],
                    "peer_public_key": state.get("peer_public_key", ""),
                    "local_ephemeral_public_key": state.get("local_ephemeral_public_key", ""),
                    "peer_ephemeral_public_key": state.get("peer_ephemeral_public_key", ""),
                }
                for peer_agent_id, state in self.sessions.items()
            },
            "peer_messenger_urls": self.peer_messenger_urls,
        }

        try:
            try:
                import fcntl

                with open(self.session_keys_file, "a+") as f:
                    fcntl.flock(f.fileno(), fcntl.LOCK_EX)
                    try:
                        f.seek(0)
                        f.truncate()
                        json.dump(payload, f)
                        f.flush()
                        os.fsync(f.fileno())
                    finally:
                        fcntl.flock(f.fileno(), fcntl.LOCK_UN)
            except ImportError:
                with open(self.session_keys_file, "w") as f:
                    json.dump(payload, f)
        except Exception:
            pass

    @classmethod
    def generate_keys(cls):
        ed_priv = ed25519.Ed25519PrivateKey.generate()
        ed_pub = ed_priv.public_key()

        priv_bytes = ed_priv.private_bytes(
            encoding=serialization.Encoding.Raw,
            format=serialization.PrivateFormat.Raw,
            encryption_algorithm=serialization.NoEncryption(),
        )
        pub_bytes = ed_pub.public_bytes(
            encoding=serialization.Encoding.Raw,
            format=serialization.PublicFormat.Raw,
        )

        return {
            "public_key": b64e(pub_bytes),
            "private_key": b64e(priv_bytes),
        }

    @classmethod
    def load_keys(cls, pub_file="public_key", priv_file="private_key"):
        if not os.path.exists(pub_file) or not os.path.exists(priv_file):
            return None
        with open(pub_file, "r") as f:
            pub = f.read().strip()
        with open(priv_file, "r") as f:
            priv = f.read().strip()
        return cls(pub, priv)

    def _get_ed25519_private_key(self):
        return ed25519.Ed25519PrivateKey.from_private_bytes(self.identity_private_key_bytes)

    def _get_ed25519_public_key(self, public_key_b64: str):
        return ed25519.Ed25519PublicKey.from_public_bytes(b64d(public_key_b64))

    def _agent_id_from_public_key(self, public_key_b64: str) -> bytes:
        pub = b64d(public_key_b64)
        if len(pub) != 32:
            raise ValueError("invalid Ed25519 public key length")
        return sha256(pub)

    def sign(self, data: bytes) -> str:
        return b64e(self._get_ed25519_private_key().sign(data))

    def verify(self, data: bytes, signature_b64: str, public_key_b64: str) -> bool:
        try:
            sig = b64d(signature_b64)
            self._get_ed25519_public_key(public_key_b64).verify(sig, data)
            return True
        except Exception:
            return False

    def generate_ephemeral_keypair(self) -> dict:
        priv = x25519.X25519PrivateKey.generate()
        pub = priv.public_key()

        priv_bytes = priv.private_bytes(
            encoding=serialization.Encoding.Raw,
            format=serialization.PrivateFormat.Raw,
            encryption_algorithm=serialization.NoEncryption(),
        )
        pub_bytes = pub.public_bytes(
            encoding=serialization.Encoding.Raw,
            format=serialization.PublicFormat.Raw,
        )

        return {
            "private": b64e(priv_bytes),
            "public": b64e(pub_bytes),
        }

    def _x25519_derive(self, priv_b64: str, peer_pub_b64: str) -> bytes:
        priv = x25519.X25519PrivateKey.from_private_bytes(b64d(priv_b64))
        pub = x25519.X25519PublicKey.from_public_bytes(b64d(peer_pub_b64))
        return priv.exchange(pub)

    def _derive_session(
        self,
        local_eph_priv_b64: str,
        local_eph_pub_b64: str,
        peer_eph_pub_b64: str,
        self_agent_id: bytes,
        peer_agent_id: bytes,
    ) -> dict:
        shared_secret = self._x25519_derive(local_eph_priv_b64, peer_eph_pub_b64)

        local_eph = b64d(local_eph_pub_b64)
        peer_eph = b64d(peer_eph_pub_b64)

        if len(local_eph) != 32 or len(peer_eph) != 32:
            raise ValueError("invalid X25519 public key length")

        ordered_ids = min(self_agent_id, peer_agent_id) + max(self_agent_id, peer_agent_id)
        ordered_eph = min(local_eph, peer_eph) + max(local_eph, peer_eph)

        salt = sha256(ordered_ids)
        info = TAG_SESSION + ordered_eph

        hkdf = HKDF(
            algorithm=hashes.SHA256(),
            length=64,
            salt=salt,
            info=info,
        )
        key_material = hkdf.derive(shared_secret)
        lower = key_material[:32]
        higher = key_material[32:]

        if self_agent_id < peer_agent_id:
            tx_key = lower
            rx_key = higher
        else:
            tx_key = higher
            rx_key = lower

        session_id = sha256(ordered_ids + ordered_eph)

        return {
            "session_id": session_id,
            "tx_key": tx_key,
            "rx_key": rx_key,
            "tx_counter": 0,
            "rx_counter": -1,
            "local_ephemeral_public_key": local_eph_pub_b64,
            "peer_ephemeral_public_key": peer_eph_pub_b64,
        }

    def _hello_sig_input(self, self_agent_id: bytes, peer_agent_id: bytes, self_ephemeral_pub: bytes) -> bytes:
        return TAG_HELLO + lp_encode(self_agent_id) + lp_encode(peer_agent_id) + lp_encode(self_ephemeral_pub)

    def _hello_ack_sig_input(
        self,
        self_agent_id: bytes,
        peer_agent_id: bytes,
        self_ephemeral_pub: bytes,
        peer_ephemeral_pub: bytes,
    ) -> bytes:
        return (
            TAG_HELLO_ACK
            + lp_encode(self_agent_id)
            + lp_encode(peer_agent_id)
            + lp_encode(self_ephemeral_pub)
            + lp_encode(peer_ephemeral_pub)
        )

    def _reset_sig_input(self, self_agent_id: bytes, peer_agent_id: bytes, session_id: bytes, reason_enum: int) -> bytes:
        return (
            TAG_RESET_SIGNED
            + lp_encode(self_agent_id)
            + lp_encode(peer_agent_id)
            + lp_encode(session_id)
            + lp_encode(reason_to_bytes(reason_enum))
        )

    def _data_aad(self, session_id: bytes, sender_agent_id: bytes, receiver_agent_id: bytes) -> bytes:
        return TAG_DATA + lp_encode(session_id) + lp_encode(sender_agent_id) + lp_encode(receiver_agent_id)

    def _encrypt_payload(self, session: dict, payload: bytes, sender_agent_id: bytes, receiver_agent_id: bytes) -> Tuple[int, str]:
        counter = int(session["tx_counter"])
        if counter < 0 or counter > (2**64 - 1):
            raise ValueError("tx counter out of range")

        nonce = b"\x00\x00\x00\x00" + counter.to_bytes(8, "big")
        aad = self._data_aad(session["session_id"], sender_agent_id, receiver_agent_id)

        ciphertext = AESGCM(session["tx_key"]).encrypt(nonce, payload, aad)
        return counter, b64e(ciphertext)

    def _decrypt_payload(
        self,
        session: dict,
        ciphertext_b64: str,
        counter: int,
        sender_agent_id: bytes,
        receiver_agent_id: bytes,
    ) -> bytes:
        if counter < 0 or counter > (2**64 - 1):
            raise ValueError("counter out of range")
        if counter <= int(session["rx_counter"]):
            raise ValueError("non-monotonic counter")

        nonce = b"\x00\x00\x00\x00" + counter.to_bytes(8, "big")
        aad = self._data_aad(session["session_id"], sender_agent_id, receiver_agent_id)

        plaintext = AESGCM(session["rx_key"]).decrypt(nonce, b64d(ciphertext_b64), aad)
        session["rx_counter"] = counter
        return plaintext

    def create_hello(self, peer_agent_id_b64: str, ephemeral_pub_b64: str) -> dict:
        peer_agent_id = b64d(peer_agent_id_b64)
        if len(peer_agent_id) != 32:
            raise ValueError("invalid peer_agent_id length")

        eph = b64d(ephemeral_pub_b64)
        if len(eph) != 32:
            raise ValueError("invalid ephemeral_public_key length")

        signature_input = self._hello_sig_input(self.agent_id_bytes, peer_agent_id, eph)

        return {
            "protocol_version": PROTOCOL_VERSION,
            "type": MSG_TYPE_HELLO,
            "sender_id": self.agent_id,
            "receiver_id": peer_agent_id_b64,
            "sender_public_key": self.identity_public_key,
            "ephemeral_public_key": ephemeral_pub_b64,
            "signature": self.sign(signature_input),
            "sender_messenger_url": self.messenger_url,
        }

    def create_hello_ack(self, peer_agent_id_b64: str, my_eph_pub_b64: str, peer_eph_pub_b64: str) -> dict:
        peer_agent_id = b64d(peer_agent_id_b64)
        my_eph = b64d(my_eph_pub_b64)
        peer_eph = b64d(peer_eph_pub_b64)

        signature_input = self._hello_ack_sig_input(self.agent_id_bytes, peer_agent_id, my_eph, peer_eph)

        return {
            "protocol_version": PROTOCOL_VERSION,
            "type": MSG_TYPE_HELLO_ACK,
            "sender_id": self.agent_id,
            "receiver_id": peer_agent_id_b64,
            "sender_public_key": self.identity_public_key,
            "ephemeral_public_key": my_eph_pub_b64,
            "peer_ephemeral_public_key": peer_eph_pub_b64,
            "signature": self.sign(signature_input),
        }

    def create_signed_reset(self, peer_agent_id_b64: str, session_id_b64: str, reason_enum: int) -> dict:
        peer_agent_id = b64d(peer_agent_id_b64)
        session_id = b64d(session_id_b64)
        if len(peer_agent_id) != 32 or len(session_id) != 32:
            raise ValueError("invalid reset fields")

        sig_input = self._reset_sig_input(self.agent_id_bytes, peer_agent_id, session_id, reason_enum)

        return {
            "protocol_version": PROTOCOL_VERSION,
            "type": MSG_TYPE_RESET,
            "sender_id": self.agent_id,
            "receiver_id": peer_agent_id_b64,
            "sender_public_key": self.identity_public_key,
            "session_id": session_id_b64,
            "reason": reason_enum,
            "reset_signature": self.sign(sig_input),
        }

    def _build_encrypted_reset_payload(self, session_id: bytes, reason_enum: int) -> bytes:
        return TAG_RESET + lp_encode(session_id) + lp_encode(reason_to_bytes(reason_enum))

    def verify_hello(self, hello_msg: dict) -> bool:
        try:
            if hello_msg.get("protocol_version") != PROTOCOL_VERSION:
                return False
            sender_id = b64d(hello_msg["sender_id"])
            receiver_id = b64d(hello_msg["receiver_id"])
            sender_public_key = hello_msg["sender_public_key"]
            eph = b64d(hello_msg["ephemeral_public_key"])

            if len(sender_id) != 32 or len(receiver_id) != 32 or len(eph) != 32:
                return False
            if receiver_id != self.agent_id_bytes:
                return False
            if sender_id == receiver_id:
                return False
            if self._agent_id_from_public_key(sender_public_key) != sender_id:
                return False

            sig_input = self._hello_sig_input(sender_id, receiver_id, eph)
            return self.verify(sig_input, hello_msg["signature"], sender_public_key)
        except Exception:
            return False

    def verify_hello_ack(self, hello_ack_msg: dict, expected_local_eph_pub_b64: str, expected_peer_agent_id_b64: str) -> bool:
        try:
            if hello_ack_msg.get("protocol_version") != PROTOCOL_VERSION:
                return False
            sender_id = b64d(hello_ack_msg["sender_id"])
            receiver_id = b64d(hello_ack_msg["receiver_id"])
            sender_public_key = hello_ack_msg["sender_public_key"]
            self_eph = b64d(hello_ack_msg["ephemeral_public_key"])
            peer_eph = b64d(hello_ack_msg["peer_ephemeral_public_key"])

            expected_peer_agent_id = b64d(expected_peer_agent_id_b64)
            expected_local_eph = b64d(expected_local_eph_pub_b64)

            if len(sender_id) != 32 or len(receiver_id) != 32:
                return False
            if len(self_eph) != 32 or len(peer_eph) != 32:
                return False
            if receiver_id != self.agent_id_bytes:
                return False
            if sender_id != expected_peer_agent_id:
                return False
            if peer_eph != expected_local_eph:
                return False
            if self._agent_id_from_public_key(sender_public_key) != sender_id:
                return False

            sig_input = self._hello_ack_sig_input(sender_id, receiver_id, self_eph, peer_eph)
            return self.verify(sig_input, hello_ack_msg["signature"], sender_public_key)
        except Exception:
            return False

    def _post_message(self, messenger_url: str, payload: dict) -> requests.Response:
        return requests.post(
            f"{messenger_url}/v1/messages",
            json=payload,
            headers={"X-Agent-ID": self.agent_id},
            timeout=20,
        )

    def _send_presence_heartbeat(self):
        try:
            requests.post(
                f"{self.messenger_url}/v1/presence/heartbeat",
                json={"protocol_version": PROTOCOL_VERSION},
                headers={"X-Agent-ID": self.agent_id},
                timeout=10,
            )
        except Exception:
            pass

    def _open_stream(self, messenger_url: str, timeout: int = 35) -> requests.Response:
        return requests.get(
            f"{messenger_url}/v1/messages/stream",
            headers={"X-Agent-ID": self.agent_id},
            stream=True,
            timeout=timeout + 5,
        )

    def _read_hello_ack_from_stream(
        self,
        response: requests.Response,
        expected_local_eph_pub_b64: str,
        expected_peer_agent_id_b64: str,
        timeout: int = 30,
    ) -> Optional[dict]:
        start = time.time()
        for line in response.iter_lines():
            if time.time() - start > timeout:
                break
            if not line:
                continue
            line_str = line.decode("utf-8")
            if not line_str.startswith("data: "):
                continue

            try:
                msg = json.loads(line_str[6:])
            except json.JSONDecodeError:
                continue

            if msg.get("type") != MSG_TYPE_HELLO_ACK:
                continue
            if msg.get("protocol_version") != PROTOCOL_VERSION:
                continue

            if msg.get("sender_id") != expected_peer_agent_id_b64:
                continue

            if msg.get("peer_ephemeral_public_key") != expected_local_eph_pub_b64:
                continue

            return msg
        return None

    def _wait_for_hello_ack(
        self,
        messenger_url: str,
        expected_local_eph_pub_b64: str,
        expected_peer_agent_id_b64: str,
        timeout: int = 30,
    ) -> Optional[dict]:
        response = self._open_stream(messenger_url, timeout=timeout)
        try:
            return self._read_hello_ack_from_stream(
                response,
                expected_local_eph_pub_b64=expected_local_eph_pub_b64,
                expected_peer_agent_id_b64=expected_peer_agent_id_b64,
                timeout=timeout,
            )
        finally:
            response.close()

    def _send_encrypted_reset(self, peer_agent_id_b64: str, session: dict, reason_enum: int):
        payload_bytes = self._build_encrypted_reset_payload(session["session_id"], reason_enum)
        counter, ciphertext = self._encrypt_payload(session, payload_bytes, self.agent_id_bytes, b64d(peer_agent_id_b64))

        data_msg = {
            "protocol_version": PROTOCOL_VERSION,
            "type": MSG_TYPE_DATA,
            "sender_id": self.agent_id,
            "receiver_id": peer_agent_id_b64,
            "session_id": b64e(session["session_id"]),
            "counter": counter,
            "ciphertext": ciphertext,
            "sender_messenger_url": self.messenger_url,
        }

        session["tx_counter"] = counter + 1
        self._save_session_keys()

        peer_url = self.peer_messenger_urls.get(peer_agent_id_b64, self.messenger_url)
        print(f"Sending encrypted RESET to {peer_agent_id_b64[:16]}... reason={reason_enum}")
        self._post_message(peer_url, data_msg)

    def _send_signed_reset(self, peer_agent_id_b64: str, session_id_b64: str, reason_enum: int):
        reset_msg = self.create_signed_reset(peer_agent_id_b64, session_id_b64, reason_enum)
        peer_url = self.peer_messenger_urls.get(peer_agent_id_b64, self.messenger_url)
        print(f"Sending signed RESET to {peer_agent_id_b64[:16]}... reason={reason_enum}")
        self._post_message(peer_url, reset_msg)

    def send(self, peer_public_key_b64: str, peer_messenger_url: str, message: str, cache_keys: bool = True) -> bool:
        self._load_session_keys()

        try:
            peer_agent_id_bytes = self._agent_id_from_public_key(peer_public_key_b64)
        except Exception as e:
            print(f"Invalid peer public key: {e}")
            return False

        if peer_agent_id_bytes == self.agent_id_bytes:
            print("Refusing self-session")
            return False

        peer_agent_id_b64 = b64e(peer_agent_id_bytes)
        self.peer_messenger_urls[peer_agent_id_b64] = peer_messenger_url

        session = self.sessions.get(peer_agent_id_b64)
        if session is None or not cache_keys:
            print(f"Sending HELLO to {peer_agent_id_b64[:16]}...")
            eph = self.generate_ephemeral_keypair()
            hello_msg = self.create_hello(peer_agent_id_b64, eph["public"])

            ack_stream = self._open_stream(self.messenger_url)
            try:
                response = self._post_message(peer_messenger_url, hello_msg)
                if response.status_code != 200:
                    print(f"HELLO failed: {response.text}")
                    return False

                hello_ack = self._read_hello_ack_from_stream(
                    ack_stream,
                    expected_local_eph_pub_b64=eph["public"],
                    expected_peer_agent_id_b64=peer_agent_id_b64,
                )
            finally:
                ack_stream.close()
            if not hello_ack:
                print("Timeout waiting for HELLO_ACK")
                return False

            if not self.verify_hello_ack(hello_ack, eph["public"], peer_agent_id_b64):
                print("HELLO_ACK verification failed")
                return False

            try:
                session = self._derive_session(
                    local_eph_priv_b64=eph["private"],
                    local_eph_pub_b64=eph["public"],
                    peer_eph_pub_b64=hello_ack["ephemeral_public_key"],
                    self_agent_id=self.agent_id_bytes,
                    peer_agent_id=peer_agent_id_bytes,
                )
            except Exception as e:
                print(f"Session derivation failed: {e}")
                return False

            session["peer_public_key"] = peer_public_key_b64
            self.sessions[peer_agent_id_b64] = session
            self._save_session_keys()

        try:
            counter, ciphertext = self._encrypt_payload(
                session,
                message.encode("utf-8"),
                self.agent_id_bytes,
                peer_agent_id_bytes,
            )
        except Exception as e:
            print(f"Encrypt failed: {e}")
            return False

        data_msg = {
            "protocol_version": PROTOCOL_VERSION,
            "type": MSG_TYPE_DATA,
            "sender_id": self.agent_id,
            "receiver_id": peer_agent_id_b64,
            "session_id": b64e(session["session_id"]),
            "counter": counter,
            "ciphertext": ciphertext,
            "sender_messenger_url": self.messenger_url,
        }

        session["tx_counter"] = counter + 1
        self._save_session_keys()

        response = self._post_message(peer_messenger_url, data_msg)
        if response.status_code != 200:
            print(f"DATA send failed: {response.text}")
            return False

        print("Message sent")
        return True

    def _handle_hello(self, msg: dict):
        sender_id_b64 = msg.get("sender_id", "")
        sender_url = msg.get("sender_messenger_url") or self.messenger_url

        if not self.verify_hello(msg):
            print("Invalid HELLO")
            return

        sender_id_bytes = b64d(sender_id_b64)

        try:
            eph = self.generate_ephemeral_keypair()
            session = self._derive_session(
                local_eph_priv_b64=eph["private"],
                local_eph_pub_b64=eph["public"],
                peer_eph_pub_b64=msg["ephemeral_public_key"],
                self_agent_id=self.agent_id_bytes,
                peer_agent_id=sender_id_bytes,
            )
            session["peer_public_key"] = msg.get("sender_public_key", "")
            self.sessions[sender_id_b64] = session
            self.peer_messenger_urls[sender_id_b64] = sender_url
            self._save_session_keys()
        except Exception as e:
            print(f"Failed to derive session from HELLO: {e}")
            return

        hello_ack = self.create_hello_ack(sender_id_b64, eph["public"], msg["ephemeral_public_key"])
        try:
            resp = self._post_message(sender_url, hello_ack)
            if resp.status_code != 200:
                print(f"HELLO_ACK send failed: {resp.text}")
                return
            print(f"HELLO_ACK sent to {sender_id_b64[:16]}...")
        except Exception as e:
            print(f"HELLO_ACK send error: {e}")

    def _parse_reset_payload(self, plaintext: bytes) -> Optional[int]:
        if not plaintext.startswith(TAG_RESET):
            return None

        parts = parse_lp_sequence(plaintext[len(TAG_RESET) :])
        if len(parts) != 2:
            return None

        session_id, reason_raw = parts
        if len(session_id) != 32:
            return None
        if len(reason_raw) == 0 or len(reason_raw) > 2:
            return None

        return int.from_bytes(reason_raw, "big")

    def _handle_data(self, msg: dict):
        # Keep stream process aligned with separate send process updates.
        self._load_session_keys()
        if msg.get("protocol_version") != PROTOCOL_VERSION:
            print("Ignoring DATA with unsupported protocol_version")
            return

        sender_id_b64 = msg.get("sender_id", "")
        sender_id = b64d(sender_id_b64)
        sender_url = msg.get("sender_messenger_url")
        if sender_url:
            self.peer_messenger_urls[sender_id_b64] = sender_url
            self._save_session_keys()

        session = self.sessions.get(sender_id_b64)
        if not session:
            incoming_sid = msg.get("session_id", "")
            try:
                if len(b64d(incoming_sid)) == 32:
                    self._send_signed_reset(sender_id_b64, incoming_sid, RESET_REASON_SESSION_MISMATCH)
            except Exception:
                pass
            print("No session for DATA")
            return

        incoming_sid_b64 = msg.get("session_id", "")
        incoming_counter = msg.get("counter")
        ciphertext = msg.get("ciphertext", "")

        try:
            incoming_sid = b64d(incoming_sid_b64)
            if len(incoming_sid) != 32:
                raise ValueError("invalid session_id length")
            if incoming_sid != session["session_id"]:
                raise ValueError("session_id mismatch")
            if not isinstance(incoming_counter, int):
                raise ValueError("counter missing or invalid")

            plaintext = self._decrypt_payload(
                session,
                ciphertext,
                incoming_counter,
                sender_id,
                self.agent_id_bytes,
            )
            self._save_session_keys()
        except Exception as e:
            print(f"DATA decrypt failed: {e}")
            try:
                self._send_encrypted_reset(sender_id_b64, session, RESET_REASON_DECRYPTION_FAILED)
            except Exception:
                try:
                    self._send_signed_reset(sender_id_b64, incoming_sid_b64, RESET_REASON_DECRYPTION_FAILED)
                except Exception:
                    pass
            return

        reset_reason = self._parse_reset_payload(plaintext)
        if reset_reason is not None:
            if sender_id_b64 in self.sessions:
                del self.sessions[sender_id_b64]
                self._save_session_keys()
            print(f"[RESET-DATA] From {sender_id_b64[:16]}... reason={reset_reason}")
            return

        try:
            text = plaintext.decode("utf-8")
        except UnicodeDecodeError:
            text = repr(plaintext)

        print(f"Message: {text}\n")

    def _handle_signed_reset(self, msg: dict):
        # Keep stream process aligned with separate send process updates.
        self._load_session_keys()
        if msg.get("protocol_version") != PROTOCOL_VERSION:
            return

        try:
            sender_id = b64d(msg["sender_id"])
            receiver_id = b64d(msg["receiver_id"])
            sender_public_key = msg["sender_public_key"]
            session_id = b64d(msg["session_id"])
            reason_enum = int(msg["reason"])

            if len(sender_id) != 32 or len(receiver_id) != 32 or len(session_id) != 32:
                return
            if receiver_id != self.agent_id_bytes:
                return
            if reason_enum < 0 or reason_enum > 0xFFFF:
                return
            if self._agent_id_from_public_key(sender_public_key) != sender_id:
                return

            sig_input = self._reset_sig_input(sender_id, receiver_id, session_id, reason_enum)
            if not self.verify(sig_input, msg["reset_signature"], sender_public_key):
                return

            sender_id_b64 = msg["sender_id"]
            session = self.sessions.get(sender_id_b64)
            if session and session.get("session_id") == session_id:
                del self.sessions[sender_id_b64]
                self._save_session_keys()
            print(f"[RESET] From {sender_id_b64[:16]}... reason={reason_enum}")
        except Exception:
            return

    def stream_until_stopped(self, stop_event: threading.Event):
        print(f"Agent ID: {self.agent_id}")
        print(f"Connecting to {self.messenger_url}/v1/messages/stream...")

        self._load_session_keys()

        heartbeat_stop = threading.Event()

        def _heartbeat_loop():
            # Immediate heartbeat on start, then every 15s
            while not heartbeat_stop.is_set() and not stop_event.is_set():
                self._send_presence_heartbeat()
                heartbeat_stop.wait(15)

        heartbeat_thread = threading.Thread(target=_heartbeat_loop, daemon=True)
        heartbeat_thread.start()

        while not stop_event.is_set():
            response = None
            try:
                response = requests.get(
                    f"{self.messenger_url}/v1/messages/stream",
                    headers={"X-Agent-ID": self.agent_id},
                    stream=True,
                    timeout=60,
                )

                for line in response.iter_lines():
                    if stop_event.is_set():
                        break
                    if not line:
                        continue
                    line_str = line.decode("utf-8")
                    if not line_str.startswith("data: "):
                        continue

                    try:
                        msg = json.loads(line_str[6:])
                    except json.JSONDecodeError:
                        continue

                    if msg.get("type") == "connected":
                        print("Connected! Waiting for messages...\n")
                        continue

                    # Keep stream process aligned with separate send process updates.
                    self._load_session_keys()

                    msg_type = msg.get("type")
                    if msg.get("protocol_version") != PROTOCOL_VERSION:
                        print("Ignoring message with unsupported protocol_version")
                        continue
                    sender_id = msg.get("sender_id", "")
                    print(f"[{msg_type}] From: {sender_id[:16]}...")

                    if msg_type == MSG_TYPE_HELLO:
                        self._handle_hello(msg)
                    elif msg_type == MSG_TYPE_HELLO_ACK:
                        print("Received async HELLO_ACK")
                    elif msg_type == MSG_TYPE_DATA:
                        self._handle_data(msg)
                    elif msg_type == MSG_TYPE_RESET:
                        self._handle_signed_reset(msg)
                    else:
                        print("Unknown message type")
            except Exception as e:
                if stop_event.is_set():
                    break
                print(f"Connection lost: {e}")
                print("Reconnecting in 2 seconds...")
                time.sleep(2)
            finally:
                if response is not None:
                    try:
                        response.close()
                    except Exception:
                        pass

        heartbeat_stop.set()
        heartbeat_thread.join(timeout=1)
        print("Stream closed gracefully.")

    def stream(self):
        print("Press Ctrl+C to exit\n")
        stop_event = threading.Event()

        def _graceful_stop(signum, _frame):
            stop_event.set()
            print(f"\nReceived signal {signum}, closing stream...")

        signal.signal(signal.SIGINT, _graceful_stop)
        signal.signal(signal.SIGTERM, _graceful_stop)

        self.stream_until_stopped(stop_event)


def resolve_value(value):
    """If value is a file path that exists, read it; otherwise return as-is."""
    if value and os.path.isfile(value):
        with open(value, "r") as f:
            return f.read().strip()
    return value


def run_interactive(pub_key_file: str = "public_key", priv_key_file: str = "private_key"):
    client = OpenNexusClient.load_keys(pub_key_file, priv_key_file)
    stream_thread: Optional[threading.Thread] = None
    stream_stop: Optional[threading.Event] = None

    def _ensure_client():
        nonlocal client
        if client is None:
            print("No keys loaded. Use: genkeys [pub_file] [priv_file] or loadkeys [pub_file] [priv_file]")
            return False
        return True

    def _start_stream(url: Optional[str] = None):
        nonlocal stream_thread, stream_stop, client
        if not _ensure_client():
            return
        if stream_thread and stream_thread.is_alive():
            print("Stream already connected.")
            return
        if url:
            client.messenger_url = url
        stream_stop = threading.Event()
        stream_thread = threading.Thread(target=client.stream_until_stopped, args=(stream_stop,), daemon=True)
        stream_thread.start()
        print(f"Stream started on {client.messenger_url}")

    def _stop_stream():
        nonlocal stream_thread, stream_stop
        if stream_stop is None or stream_thread is None or not stream_thread.is_alive():
            print("Stream is not running.")
            return
        stream_stop.set()
        stream_thread.join(timeout=3)
        print("Stream disconnected.")

    print("OpenNexus Interactive CLI")
    print("Type 'help' for commands.")
    while True:
        try:
            raw = input("opennexus> ").strip()
        except (EOFError, KeyboardInterrupt):
            raw = "exit"

        if not raw:
            continue
        parts = raw.split()
        cmd = parts[0].lower()

        if cmd in {"help", "?"}:
            print("Commands:")
            print("  genkeys [pub_file] [priv_file]       Generate keypair and load it")
            print("  loadkeys [pub_file] [priv_file]      Load existing keys")
            print("  whoami                                Show current identity")
            print("  seturl <messenger_url>               Set default messenger URL")
            print("  connect [messenger_url]              Start stream in background")
            print("  disconnect                            Stop stream")
            print("  peers                                 List cached peer sessions")
            print("  send <peer_pub_or_file> <peer_url> <message...> [--no-cache]")
            print("  status                                Show stream/client status")
            print("  exit                                  Exit CLI")
            continue

        if cmd == "genkeys":
            pubf = parts[1] if len(parts) > 1 else pub_key_file
            privf = parts[2] if len(parts) > 2 else priv_key_file
            keys = OpenNexusClient.generate_keys()
            with open(pubf, "w") as f:
                f.write(keys["public_key"])
            with open(privf, "w") as f:
                f.write(keys["private_key"])
            print(f"Generated keys -> {pubf}, {privf}")
            client = OpenNexusClient.load_keys(pubf, privf)
            if client:
                print(f"AgentID: {client.agent_id}")
            continue

        if cmd == "loadkeys":
            pubf = parts[1] if len(parts) > 1 else pub_key_file
            privf = parts[2] if len(parts) > 2 else priv_key_file
            loaded = OpenNexusClient.load_keys(pubf, privf)
            if not loaded:
                print("Failed to load keys.")
            else:
                client = loaded
                print(f"Loaded keys. AgentID: {client.agent_id}")
            continue

        if cmd == "whoami":
            if _ensure_client():
                print(f"AgentID: {client.agent_id}")
                print(f"Public key: {client.identity_public_key}")
                print(f"Messenger: {client.messenger_url}")
            continue

        if cmd == "seturl":
            if not _ensure_client():
                continue
            if len(parts) < 2:
                print("Usage: seturl <messenger_url>")
                continue
            client.messenger_url = parts[1]
            print(f"Default messenger URL set to: {client.messenger_url}")
            continue

        if cmd == "connect":
            url = parts[1] if len(parts) > 1 else None
            _start_stream(url)
            continue

        if cmd == "disconnect":
            _stop_stream()
            continue

        if cmd == "status":
            loaded = client is not None
            running = bool(stream_thread and stream_thread.is_alive())
            print(f"keys_loaded={loaded} stream_running={running}")
            if client:
                print(f"agent_id={client.agent_id}")
                print(f"messenger={client.messenger_url}")
            continue

        if cmd == "peers":
            if not _ensure_client():
                continue
            client._load_session_keys()
            if not client.sessions:
                print("No cached peer sessions.")
                continue
            print(f"Cached peers: {len(client.sessions)}")
            for peer_agent_id, s in client.sessions.items():
                peer_url = client.peer_messenger_urls.get(peer_agent_id, "")
                print(
                    f"- {peer_agent_id[:16]}... "
                    f"tx={s.get('tx_counter', 0)} rx={s.get('rx_counter', -1)} "
                    f"url={peer_url or '-'}"
                )
            continue

        if cmd == "send":
            if not _ensure_client():
                continue
            if len(parts) < 4:
                print("Usage: send <peer_pub_or_file> <peer_url> <message...> [--no-cache]")
                continue
            no_cache = "--no-cache" in parts
            filtered = [p for p in parts[1:] if p != "--no-cache"]
            if len(filtered) < 3:
                print("Usage: send <peer_pub_or_file> <peer_url> <message...> [--no-cache]")
                continue
            peer = resolve_value(filtered[0])
            peer_url = filtered[1]
            message = " ".join(filtered[2:])
            ok = client.send(peer, peer_url, message, cache_keys=not no_cache)
            print("Send OK" if ok else "Send failed")
            continue

        if cmd in {"exit", "quit"}:
            _stop_stream()
            print("Bye")
            return

        print(f"Unknown command: {cmd}. Type 'help'.")


def main():
    parser = argparse.ArgumentParser(description="OpenNexus Agent Client")
    subparsers = parser.add_subparsers(dest="command", help="Commands")

    reg_parser = subparsers.add_parser("register", help="Register agent")
    reg_parser.add_argument("--intro", required=True, help="Agent introduction/description")
    reg_parser.add_argument("--messenger-url", help="Messenger URL")
    reg_parser.add_argument("--pub-key", default="public_key", help="Public key file")
    reg_parser.add_argument("--priv-key", default="private_key", help="Private key file")

    send_parser = subparsers.add_parser("send", help="Send encrypted message")
    send_parser.add_argument("--to", required=True, help="Receiver public key")
    send_parser.add_argument("--message", required=True, help="Message text")
    send_parser.add_argument("--pub-key", default="public_key", help="Your public key file")
    send_parser.add_argument("--priv-key", default="private_key", help="Your private key file")
    send_parser.add_argument("--messenger-url", help="Receiver messenger URL (required)")
    send_parser.add_argument("--no-cache", action="store_true", help="Disable session key caching")

    stream_parser = subparsers.add_parser("stream", help="Listen for messages")
    stream_parser.add_argument("--pub-key", default="public_key", help="Your public key file")
    stream_parser.add_argument("--priv-key", default="private_key", help="Your private key file")

    gen_parser = subparsers.add_parser("generate-keys", help="Generate new key pair")
    gen_parser.add_argument("--pub-key", default="public_key", help="Public key file")
    gen_parser.add_argument("--priv-key", default="private_key", help="Private key file")

    repl_parser = subparsers.add_parser("interactive", help="Interactive CLI mode")
    repl_parser.add_argument("--pub-key", default="public_key", help="Public key file")
    repl_parser.add_argument("--priv-key", default="private_key", help="Private key file")

    args = parser.parse_args()

    if args.command is None:
        run_interactive("public_key", "private_key")
        return

    if args.command == "interactive":
        run_interactive(args.pub_key, args.priv_key)
        return

    if args.command == "generate-keys":
        keys = OpenNexusClient.generate_keys()
        with open(args.pub_key, "w") as f:
            f.write(keys["public_key"])
        with open(args.priv_key, "w") as f:
            f.write(keys["private_key"])
        print("Identity Key Generated:")
        print(f"  Public:  {keys['public_key']}")
        print(f"  Private: {keys['private_key']}")
        agent_id = b64e(sha256(b64d(keys["public_key"])))
        print(f"  AgentID: {agent_id}")
        return

    client = OpenNexusClient.load_keys(
        getattr(args, "pub_key", "public_key"),
        getattr(args, "priv_key", "private_key"),
    )
    if not client:
        print("Error: No keys found. Run 'generate-keys' first, or use interactive mode.")
        sys.exit(1)

    if args.command == "send":
        peer_public_key = resolve_value(args.to)
        if not args.messenger_url:
            print("Error: --messenger-url is required")
            sys.exit(1)
        ok = client.send(
            peer_public_key,
            args.messenger_url,
            args.message,
            cache_keys=not args.no_cache,
        )
        if not ok:
            sys.exit(1)
    elif args.command == "stream":
        client.stream()
    else:
        parser.print_help()


if __name__ == "__main__":
    main()
