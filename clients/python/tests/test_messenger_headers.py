import unittest
from unittest.mock import Mock, patch

from opennexus import OpenNexusClient


class MessengerHeaderContractTest(unittest.TestCase):
    def setUp(self):
        keys = OpenNexusClient.generate_keys()
        self.client = OpenNexusClient(
            identity_public_key=keys["public_key"],
            identity_private_key=keys["private_key"],
            messenger_url="https://api.opennexus.cc",
        )

    def test_post_message_uses_only_agent_id_header(self):
        with patch("opennexus.requests.post") as mock_post:
            mock_post.return_value = Mock(status_code=200)

            self.client._post_message("https://api.opennexus.cc", {"type": "data"})

            kwargs = mock_post.call_args.kwargs
            self.assertEqual(kwargs["headers"], {"X-Agent-ID": self.client.agent_id})

    def test_open_stream_uses_only_agent_id_header(self):
        with patch("opennexus.requests.get") as mock_get:
            mock_get.return_value = Mock(status_code=200)

            self.client._open_stream("https://api.opennexus.cc")

            kwargs = mock_get.call_args.kwargs
            self.assertEqual(kwargs["headers"], {"X-Agent-ID": self.client.agent_id})

    def test_presence_heartbeat_uses_only_agent_id_header(self):
        with patch("opennexus.requests.post") as mock_post:
            mock_post.return_value = Mock(status_code=200)

            self.client._send_presence_heartbeat()

            kwargs = mock_post.call_args.kwargs
            self.assertEqual(kwargs["headers"], {"X-Agent-ID": self.client.agent_id})


if __name__ == "__main__":
    unittest.main()
