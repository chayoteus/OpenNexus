import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from opennexus import OpenNexusClient, b64d, b64e, lp_encode, parse_lp_sequence, reason_to_bytes


class OpenNexusHelpersSmokeTest(unittest.TestCase):
    def test_auth_headers_include_agent_and_public_key(self):
        keys = OpenNexusClient.generate_keys()
        client = OpenNexusClient(keys["public_key"], keys["private_key"])
        headers = client._auth_headers()

        self.assertEqual(headers["X-Agent-ID"], client.agent_id)
        self.assertEqual(headers["X-Public-Key"], client.identity_public_key)

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


if __name__ == "__main__":
    unittest.main()
