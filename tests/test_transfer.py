import http.client
import json
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import zipfile
from pathlib import Path
from unittest.mock import patch

from loka.__main__ import (
    Handler,
    Server,
    State,
    extract,
    make_archive,
    outgoing,
    request,
)


class TransferTests(unittest.TestCase):
    def test_discovery_reports_socket_failure(self):
        state = State(0)
        try:
            with patch(
                "loka.__main__.socket.socket", side_effect=PermissionError("blocked")
            ):
                state._listen()
            self.assertIn("blocked", state.discovery_error)
        finally:
            state.temp.cleanup()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.state = None
        self.server = None

    def start_server(self):
        self.state = State(0)
        try:
            self.server = Server(("127.0.0.1", 0), Handler)
        except PermissionError:
            self.state.temp.cleanup()
            self.state = None
            self.skipTest("Local sockets are blocked")
        self.server.state = self.state
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        if self.server:
            self.server.shutdown()
            self.server.server_close()
            self.thread.join()
        if self.state:
            self.state.temp.cleanup()
        self.temp.cleanup()

    def test_send_after_receiver_accepts_and_preserve_existing_file(self):
        self.start_server()
        source = self.root / "source"
        destination = self.root / "destination"
        source.mkdir()
        destination.mkdir()
        (source / "file.txt").write_text("new")
        (destination / "file.txt").write_text("old")
        sender = State(0)
        entry = sender.transfer(
            direction="outgoing", name="receiver", items=["file.txt"], size=0
        )
        thread = threading.Thread(
            target=outgoing,
            args=(
                sender,
                entry,
                [str(source / "file.txt")],
                {"host": "127.0.0.1", "port": self.server.server_port},
            ),
        )
        thread.start()
        try:
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                incoming = list(self.state.transfers.values())
                if incoming:
                    break
                time.sleep(0.02)
            self.assertTrue(incoming)
            headers = {
                "Content-Type": "application/json",
                "X-Loka-Key": self.state.ui_key,
            }
            request(
                "127.0.0.1",
                self.server.server_port,
                "POST",
                "/api/accept",
                json.dumps({"id": incoming[0]["id"], "destination": str(destination)}),
                headers,
            )
            thread.join(5)
            self.assertFalse(thread.is_alive())
            self.assertEqual(entry["status"], "complete", entry.get("error"))
            self.assertEqual((destination / "file.txt").read_text(), "old")
            self.assertEqual((destination / "file (1).txt").read_text(), "new")
        finally:
            entry["cancel"].set()
            thread.join(5)
            sender.temp.cleanup()

    def test_directory_archive_preserves_empty_directories_and_existing_items(self):
        source = self.root / "source"
        destination = self.root / "destination"
        (source / "empty").mkdir(parents=True)
        (source / "file").write_text("new")
        (destination / "source").mkdir(parents=True)
        (destination / "source" / "old").write_text("old")
        archive = self.root / "transfer.zip"
        make_archive([source], archive)
        extract(archive, destination)
        self.assertEqual((destination / "source" / "old").read_text(), "old")
        self.assertEqual((destination / "source (1)" / "file").read_text(), "new")
        self.assertTrue((destination / "source (1)" / "empty").is_dir())

    def test_archive_cannot_escape_destination(self):
        archive = self.root / "bad.zip"
        with zipfile.ZipFile(archive, "w") as z:
            z.writestr("../escape", "bad")
        with self.assertRaises(ValueError):
            extract(archive, self.root)
        self.assertFalse((self.root.parent / "escape").exists())

    def test_decline_prevents_upload(self):
        self.start_server()
        offer = request(
            "127.0.0.1",
            self.server.server_port,
            "POST",
            "/peer/offer",
            json.dumps({"name": "sender", "items": ["file"], "size": 3}),
            {"Content-Type": "application/json"},
        )
        headers = {"Content-Type": "application/json", "X-Loka-Key": self.state.ui_key}
        request(
            "127.0.0.1",
            self.server.server_port,
            "POST",
            "/api/cancel",
            json.dumps({"id": offer["id"]}),
            headers,
        )
        status = request(
            "127.0.0.1",
            self.server.server_port,
            "GET",
            "/peer/status/" + offer["id"],
            headers={"X-Transfer-Secret": offer["secret"]},
        )
        self.assertEqual(status["status"], "cancelled")

    def test_linux_destination_picker_uses_desktop_dialog(self):
        self.start_server()
        headers = {"Content-Type": "application/json", "X-Loka-Key": self.state.ui_key}
        result = subprocess.CompletedProcess([], 0, stdout="/tmp/example\n", stderr="")
        with (
            patch("loka.__main__.sys.platform", "linux"),
            patch(
                "loka.__main__.shutil.which",
                side_effect=lambda name: name if name == "zenity" else None,
            ),
            patch("loka.__main__.subprocess.run", return_value=result) as run,
        ):
            response = request(
                "127.0.0.1",
                self.server.server_port,
                "POST",
                "/api/choose-destination",
                "{}",
                headers,
            )
        self.assertEqual(response["destination"], "/tmp/example")
        self.assertEqual(run.call_args.args[0][0], "zenity")

    def test_cli_share_serves_file_until_stopped(self):
        source = self.root / "shared.txt"
        source.write_text("hello")
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "loka",
                "share",
                "--address",
                "127.0.0.1",
                "--port",
                "0",
                str(source),
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        try:
            line = process.stdout.readline().strip()
            self.assertTrue(
                line.startswith("http://127.0.0.1:"), line or process.stderr.read()
            )
            from urllib.parse import urlsplit

            url = urlsplit(line)
            connection = http.client.HTTPConnection(url.hostname, url.port, timeout=5)
            connection.request("GET", url.path)
            response = connection.getresponse()
            self.assertEqual(response.status, 200)
            self.assertEqual(response.read(), b"hello")
            connection.close()
        finally:
            process.terminate()
            process.communicate(timeout=5)


if __name__ == "__main__":
    unittest.main()
