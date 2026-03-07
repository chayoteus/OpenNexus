import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from opennexus import OpenNexusClient, b64d, b64e, lp_encode, parse_lp_sequence, reason_to_bytes


class OpenNexusHelpersSmokeTest(unittest.TestCase):
    def test_base64_roundtrip(self):
        raw = b"OpenNexus"
        self.assertEqual(b64d(b64e(raw)), raw)

    def test_length_prefixed_sequence_roundtrip(self):
        parts = [b"a", b"bc", b"def"]
        encoded = b"".join(lp_encode(p) for p in parts)
        self.assertEqual(parse_lp_sequence(encoded), parts)

    def test_reason_to_bytes_bounds(self):
        self.assertEqual(reason_to_bytes(0), b"\x00\x00")
        self.assertEqual(reason_to_bytes(65535), b"\xff\xff")

        with self.assertRaises(ValueError):
            reason_to_bytes(-1)

        with self.assertRaises(ValueError):
            reason_to_bytes(65536)


class OpenNexusClientRecoverySmokeTest(unittest.TestCase):
    def test_handle_data_caches_sender_messenger_url_without_session(self):
        keys = OpenNexusClient.generate_keys()
        client = OpenNexusClient(
            identity_public_key=keys["public_key"],
            identity_private_key=keys["private_key"],
            messenger_url="http://127.0.0.1:18090",
        )

        with tempfile.TemporaryDirectory() as tmpdir:
            client.session_keys_file = os.path.join(tmpdir, "session_keys_test.json")
            peer_keys = OpenNexusClient.generate_keys()
            peer_id = b64e(client._agent_id_from_public_key(peer_keys["public_key"]))
            session_id = b64e(b"\x11" * 32)
            calls = []
            client._send_signed_reset = lambda peer, sid, reason: calls.append((peer, sid, reason))

            client._handle_data(
                {
                    "protocol_version": "0.1.0",
                    "type": "data",
                    "sender_id": peer_id,
                    "session_id": session_id,
                    "sender_messenger_url": "http://127.0.0.1:18091",
                }
            )

            self.assertEqual(client.peer_messenger_urls.get(peer_id), "http://127.0.0.1:18091")
            self.assertEqual(len(calls), 1)


if __name__ == "__main__":
    unittest.main()
