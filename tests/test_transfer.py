import http.client
import io
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import zipfile
from contextlib import closing
from pathlib import Path
from unittest.mock import patch
from urllib.parse import quote, urlsplit

from loka import pickers
from loka.__main__ import (
    VERSION,
    Handler,
    Server,
    State,
    order_local_addresses,
    outgoing,
    request,
)
from loka.transfers import (
    MANIFEST_LENGTH,
    STREAM_CONTENT_TYPE,
    STREAM_END,
    STREAM_FORMAT,
    Source,
    publish,
    selection_members,
    stage_stream,
    stream_manifest,
    write_zip,
)


def transfer_body(records, contents, *, complete=True):
    metadata = json.dumps(records).encode("utf-8")
    marker = STREAM_END if complete else b""
    return MANIFEST_LENGTH.pack(len(metadata)) + metadata + contents + marker


class TransferTests(unittest.TestCase):
    def test_share_address_prefers_system_route_over_docker_bridge(self):
        with patch("loka.__main__.socket.socket") as socket_factory:
            socket_context = socket_factory.return_value.__enter__.return_value
            socket_context.getsockname.return_value = ("192.168.1.10", 12345)
            addresses = order_local_addresses({"172.17.0.1", "192.168.1.10"})
        self.assertEqual(addresses, ["192.168.1.10", "172.17.0.1"])

    def test_share_addresses_remain_available_without_a_default_route(self):
        with patch("loka.__main__.socket.socket", side_effect=OSError("no route")):
            addresses = order_local_addresses({"172.17.0.1", "192.168.1.10"})
        self.assertEqual(addresses, ["172.17.0.1", "192.168.1.10"])

    def test_share_address_prefers_lan_when_vpn_owns_default_route(self):
        vpn = "10.23.0.2"
        lan = "192.168.50.7"
        bridge = "172.17.0.1"
        with patch("loka.__main__.socket.socket") as socket_factory:
            socket_context = socket_factory.return_value.__enter__.return_value
            socket_context.getsockname.return_value = (vpn, 12345)
            self.assertEqual(
                order_local_addresses({lan}, {vpn, bridge}), [lan, vpn, bridge]
            )
            self.assertEqual(order_local_addresses(set(), {vpn, bridge}), [vpn, bridge])

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
        second_name = (
            "Set_ What’s next_.mp3" if os.name == "nt" else "Set: What’s next?.mp3"
        )
        (source / second_name).write_bytes(b"music")
        (source / "nested" / "empty").mkdir(parents=True)
        (source / "nested" / "data.bin").write_bytes(b"abc" * 120_000)
        (destination / "file.txt").write_text("old")
        sender = State(0)
        entry = sender.transfer(
            direction="outgoing",
            name="receiver",
            items=["file.txt", second_name],
            size=0,
        )
        thread = threading.Thread(
            target=outgoing,
            args=(
                sender,
                entry,
                [source / "file.txt", source / second_name, source / "nested"],
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
            self.assertEqual(list(Path(sender.temp.name).iterdir()), [])
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
            self.assertEqual((destination / second_name).read_bytes(), b"music")
            self.assertTrue((destination / "nested" / "empty").is_dir())
            self.assertEqual(
                (destination / "nested" / "data.bin").read_bytes(), b"abc" * 120_000
            )
        finally:
            entry["cancel"].set()
            thread.join(5)
            sender.temp.cleanup()

    def receive_bytes(self, body, length, destination, *, windows=None):
        with stage_stream(
            io.BytesIO(body),
            len(body),
            length,
            destination,
            lambda _: None,
            windows=windows,
        ) as (target, staged):
            publish(staged, target)

    def test_directory_transfer_preserves_empty_directories_and_existing_items(self):
        source = self.root / "source"
        destination = self.root / "destination"
        (source / "empty").mkdir(parents=True)
        (source / "file").write_text("new")
        (destination / "source").mkdir(parents=True)
        (destination / "source" / "old").write_text("old")
        members = selection_members([Source.selected(source)])
        manifest, length = stream_manifest(members)
        self.receive_bytes(manifest + b"new" + STREAM_END, length, destination)
        self.assertEqual((destination / "source" / "old").read_text(), "old")
        self.assertEqual((destination / "source (1)" / "file").read_text(), "new")
        self.assertTrue((destination / "source (1)" / "empty").is_dir())

    def test_receive_preserves_broken_symlinks_at_conflicting_names(self):
        destination = self.root / "destination"
        destination.mkdir()
        links = (destination / "report.txt", destination / "report (1).txt")
        missing_target = Path("missing.txt")
        try:
            for link in links:
                link.symlink_to(missing_target)
        except OSError as error:
            if getattr(error, "winerror", None) != 1314:
                raise
            self.skipTest("Creating symlinks requires extra privileges")

        body = transfer_body([["report.txt", 8]], b"incoming")
        self.receive_bytes(body, 8, destination)

        for link in links:
            with self.subTest(filename=link.name):
                self.assertEqual(link.readlink(), missing_target)
        self.assertEqual((destination / "report (2).txt").read_bytes(), b"incoming")

    def test_archive_keeps_distinct_items_with_the_same_name(self):
        first = self.root / "first"
        second = self.root / "second"
        third = self.root / "third"
        (first / "report.txt").mkdir(parents=True)
        (first / "report.txt" / "empty").mkdir()
        second.mkdir()
        (second / "report.txt").write_bytes(b"second")
        third.mkdir()
        (third / "report (1).txt").write_bytes(b"third")
        archive = self.root / "transfer.zip"

        sources = [
            Source.selected(path)
            for path in (
                first / "report.txt",
                second / "report.txt",
                third / "report (1).txt",
            )
        ]
        with zipfile.ZipFile(archive, "w") as transfer:
            write_zip(selection_members(sources), transfer)

        with zipfile.ZipFile(archive) as transfer:
            self.assertIn("report.txt/empty/", transfer.namelist())
            self.assertEqual(transfer.read("report (2).txt"), b"second")
            self.assertEqual(transfer.read("report (1).txt"), b"third")

    def test_upload_keeps_empty_directories_and_does_not_overwrite(self):
        self.start_server()
        headers = {
            "X-Loka-Key": self.state.ui_key,
            "X-Upload-Session": "test",
            "X-File-Path": "parent/empty",
            "X-Entry-Type": "directory",
        }
        result = request(
            "127.0.0.1", self.server.server_port, "POST", "/api/upload", b"", headers
        )
        staged = Path(result["path"])
        self.assertTrue((staged / "empty").is_dir())

        del headers["X-Entry-Type"]
        headers["X-File-Path"] = "parent/report.txt"
        request(
            "127.0.0.1",
            self.server.server_port,
            "POST",
            "/api/upload",
            b"first",
            headers,
        )
        with self.assertRaises(RuntimeError):
            request(
                "127.0.0.1",
                self.server.server_port,
                "POST",
                "/api/upload",
                b"second",
                headers,
            )
        self.assertEqual((staged / "report.txt").read_bytes(), b"first")

    def test_upload_can_share_1200_separate_unicode_files(self):
        self.start_server()
        port = self.server.server_port
        headers = {"X-Loka-Key": self.state.ui_key}
        expected_files = {}
        staged_paths = []
        for index in range(1200):
            name = f"Збірка – 音楽 – 🎵 e\u0301 100% #{index}.mp3"
            if index == 0 and os.name != "nt":
                name = "Set: What’s next?.mp3"
            content = str(index).encode()
            expected_files[name] = content
            staged = request(
                "127.0.0.1",
                port,
                "POST",
                "/api/upload",
                content,
                {
                    **headers,
                    "X-Upload-Session": str(index),
                    "X-File-Path": quote(name, safe=""),
                },
            )
            staged_paths.append(staged["path"])

        result = request(
            "127.0.0.1",
            port,
            "POST",
            "/api/share",
            json.dumps({"paths": staged_paths}, ensure_ascii=False).encode(),
            {**headers, "Content-Type": "application/json"},
        )
        entry = self.state.transfers[result["id"]]
        deadline = time.monotonic() + 5
        while entry["status"] == "preparing" and time.monotonic() < deadline:
            time.sleep(0.02)
        self.assertEqual(entry["status"], "shared", entry.get("error"))

        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
        with closing(connection):
            connection.request("GET", "/download/" + entry["token"])
            with connection.getresponse() as response:
                self.assertEqual(response.status, 200)
                archive = io.BytesIO(response.read())
        with zipfile.ZipFile(archive) as downloaded:
            names = downloaded.namelist()
            self.assertCountEqual(names, expected_files)
            for name in names:
                self.assertEqual(downloaded.read(name), expected_files[name], name)

    def test_progress_page_uses_utf8_with_windows_default_encoding(self):
        self.start_server()
        open_path = Path.open

        def open_with_cp1252(path, mode="r", buffering=-1, encoding=None, **kwargs):
            if "b" not in mode and encoding in (None, "locale"):
                encoding = "cp1252"
            return open_path(path, mode, buffering, encoding, **kwargs)

        port = self.server.server_port
        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
        with (
            patch.object(Path, "open", open_with_cp1252),
            closing(connection),
        ):
            connection.request("GET", "/")
            with connection.getresponse() as response:
                self.assertEqual(response.status, 200)
                self.assertEqual(response.headers.get_content_charset(), "utf-8")
                page = response.read().decode("utf-8")

        self.assertTrue("Preparing ${item.label}…" in page, "Upload status is garbled")
        self.assertTrue(
            " · ${transfer.progress}%" in page, "Transfer status is garbled"
        )
        self.assertIn(f'<span class="version">v{VERSION}</span>', page)
        self.assertEqual(page.count(">Choose files</button>"), 1)
        self.assertEqual(page.count(">Choose directory</button>"), 1)
        self.assertNotIn("Use browser picker", page)

    def test_stream_cannot_escape_destination(self):
        destination = self.root / "destination"
        destination.mkdir()
        with self.assertRaisesRegex(ValueError, "Invalid path"):
            self.receive_bytes(
                transfer_body([["../escape", 3]], b"bad"), 3, destination
            )
        self.assertFalse((self.root / "escape").exists())

    def test_stream_renames_only_incompatible_windows_names(self):
        destination = self.root / "destination"
        destination.mkdir()
        data = b"renamedunchangedreserved"
        records = [
            ["Set: live?", None],
            ["Set: live?/Track?.mp3", 7],
            ["Set: live?/Track_.mp3", 9],
            ["Set_ live_", None],
            ["Set_ live_/CON.txt", 8],
        ]
        self.receive_bytes(
            transfer_body(records, data), len(data), destination, windows=True
        )
        self.assertEqual(
            (destination / "Set_ live_" / "_CON.txt").read_bytes(), b"reserved"
        )
        renamed = destination / "Set_ live_ (1)"
        self.assertEqual((renamed / "Track_ (1).mp3").read_bytes(), b"renamed")
        self.assertEqual((renamed / "Track_.mp3").read_bytes(), b"unchanged")

    def test_decline_prevents_upload(self):
        self.start_server()
        offer = request(
            "127.0.0.1",
            self.server.server_port,
            "POST",
            "/peer/offer",
            json.dumps(
                {
                    "name": "sender",
                    "items": ["file"],
                    "size": 3,
                    "format": STREAM_FORMAT,
                }
            ),
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

    def test_incomplete_or_cancelled_stream_keeps_destination_clean(self):
        self.start_server()
        destination = self.root / "destination"
        destination.mkdir()
        port = self.server.server_port
        headers = {"Content-Type": "application/json", "X-Loka-Key": self.state.ui_key}
        metadata = transfer_body([["first", 3], ["second", 5]], b"", complete=False)
        for cancel in (False, True):
            with self.subTest(cancel=cancel):
                offer = request(
                    "127.0.0.1",
                    port,
                    "POST",
                    "/peer/offer",
                    json.dumps(
                        {
                            "name": "sender",
                            "items": ["first", "second"],
                            "size": 8,
                            "format": STREAM_FORMAT,
                        }
                    ),
                    {"Content-Type": "application/json"},
                )
                transfer_id = offer["id"]
                request(
                    "127.0.0.1",
                    port,
                    "POST",
                    "/api/accept",
                    json.dumps({"id": transfer_id, "destination": str(destination)}),
                    headers,
                )
                connection = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
                with closing(connection):
                    connection.putrequest("POST", f"/peer/upload/{transfer_id}")
                    connection.putheader("Content-Type", STREAM_CONTENT_TYPE)
                    connection.putheader(
                        "Content-Length", str(len(metadata) + 8 + len(STREAM_END))
                    )
                    connection.putheader("X-Transfer-Secret", offer["secret"])
                    partial = b"one!" if cancel else b"one!two?"
                    connection.endheaders(metadata + partial)
                    deadline = time.monotonic() + 5
                    while not list(destination.glob(".loka-*/first")):
                        self.assertLess(time.monotonic(), deadline, "No staged file")
                        time.sleep(0.01)
                    self.assertFalse((destination / "first").exists())
                    if cancel:
                        request(
                            "127.0.0.1",
                            port,
                            "POST",
                            "/api/cancel",
                            json.dumps({"id": transfer_id}),
                            headers,
                        )
                    connection.sock.shutdown(socket.SHUT_WR)
                    with connection.getresponse() as response:
                        self.assertEqual(response.status, 400)
                        response.read()
                entry = self.state.transfers[transfer_id]
                self.assertEqual(entry["status"], "cancelled" if cancel else "failed")
                self.assertEqual(list(destination.iterdir()), [])
                self.assertEqual(list(Path(self.state.temp.name).iterdir()), [])

    def test_directory_link_streams_a_fresh_zip(self):
        self.start_server()
        source = self.root / "shared"
        (source / "empty").mkdir(parents=True)
        file = source / "file.txt"
        file.write_bytes(b"before link")
        entry = self.state.transfer(direction="outgoing", name="Link", items=[], size=0)
        outgoing(self.state, entry, [source], None)
        self.assertEqual(entry["status"], "shared")

        connection = http.client.HTTPConnection(
            "127.0.0.1", self.server.server_port, timeout=5
        )
        with closing(connection):
            for content in (b"after link", b"next download"):
                file.write_bytes(content)
                connection.request("GET", f"/download/{entry['token']}")
                with connection.getresponse() as response:
                    self.assertEqual(response.status, 200)
                    self.assertEqual(response.getheader("Transfer-Encoding"), "chunked")
                    self.assertIsNone(response.getheader("Content-Length"))
                    with zipfile.ZipFile(io.BytesIO(response.read())) as archive:
                        self.assertIn("shared/empty/", archive.namelist())
                        self.assertEqual(archive.read("shared/file.txt"), content)
        self.assertEqual(list(Path(self.state.temp.name).iterdir()), [])

    def test_linux_destination_picker_uses_desktop_dialog(self):
        self.start_server()
        headers = {"Content-Type": "application/json", "X-Loka-Key": self.state.ui_key}
        destination = self.root / "destination "
        result = subprocess.CompletedProcess(
            [], 0, stdout=os.fsencode(destination) + b"\n", stderr=b""
        )
        with (
            patch.object(pickers.sys, "platform", "linux"),
            patch(
                "loka.pickers.shutil.which",
                side_effect=lambda name: name if name == "zenity" else None,
            ),
            patch("loka.pickers.subprocess.run", return_value=result) as run,
        ):
            response = request(
                "127.0.0.1",
                self.server.server_port,
                "POST",
                "/api/choose-destination",
                "{}",
                headers,
            )
        self.assertEqual(response["destination"], str(destination))
        self.assertEqual(run.call_args.args[0][0], "zenity")

    def test_native_file_picker_returns_local_paths_only_to_the_page(self):
        self.start_server()
        paths = [str(self.root / "résumé.txt"), str(self.root / "a second file.txt")]
        result = subprocess.CompletedProcess(
            [], 0, stdout=os.fsencode("\x1f".join(paths)) + b"\n", stderr=b""
        )
        port = self.server.server_port
        body = json.dumps({"kind": "files"})
        with (
            patch.object(pickers.sys, "platform", "linux"),
            patch("loka.pickers.shutil.which", return_value="zenity"),
            patch("loka.pickers.subprocess.run", return_value=result) as run,
        ):
            with self.assertRaisesRegex(RuntimeError, "Local access only"):
                request("127.0.0.1", port, "POST", "/api/choose-paths", body)
            run.assert_not_called()
            response = request(
                "127.0.0.1",
                port,
                "POST",
                "/api/choose-paths",
                body,
                headers={"X-Loka-Key": self.state.ui_key},
            )
        self.assertIn("--multiple", run.call_args.args[0])
        self.assertEqual(response, {"paths": paths})

    def test_single_file_link_reads_original_and_rejects_a_replacement(self):
        self.start_server()
        source = self.root / "shared.txt"
        source.write_bytes(b"original contents")
        entry = self.state.transfer(direction="outgoing", name="Link", items=[], size=0)
        outgoing(self.state, entry, [source], None)
        self.assertEqual(entry["status"], "shared")
        self.assertEqual(list(Path(self.state.temp.name).iterdir()), [])

        token = entry["token"]
        port = self.server.server_port

        def download():
            connection = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
            with closing(connection):
                connection.request("GET", f"/download/{token}")
                with connection.getresponse() as response:
                    return response.status, response.read()

        self.assertEqual(download(), (200, b"original contents"))
        source.write_bytes(b"updated contents")
        self.assertEqual(download(), (200, b"updated contents"))
        saved_source = source.rename(self.root / "original.txt")
        self.assertEqual(download()[0], 404)
        source.write_bytes(b"replacement contents")
        self.assertEqual(download()[0], 404)
        saved_source.replace(source)
        request(
            "127.0.0.1",
            port,
            "POST",
            "/api/unshare",
            json.dumps({"token": token}),
            headers={"X-Loka-Key": self.state.ui_key},
        )
        self.assertEqual(download()[0], 404)
        self.assertEqual(source.read_bytes(), b"updated contents")

    def test_cli_reports_occupied_port(self):
        source = self.root / "shared.txt"
        source.write_text("hello")
        with closing(socket.socket()) as listener:
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            port = listener.getsockname()[1]
            for command in ([], ["receive"], ["devices"], ["share", str(source)]):
                with self.subTest(command=command):
                    result = subprocess.run(
                        [sys.executable, "-m", "loka", "--port", str(port), *command],
                        capture_output=True,
                        check=False,
                        text=True,
                        timeout=5,
                    )
                    self.assertEqual(result.returncode, 1)
                    self.assertEqual(result.stdout, "")
                    self.assertIn(f"Port {port} is already in use", result.stderr)
                    self.assertIn("--port 0", result.stderr)
                    self.assertNotIn("Traceback", result.stderr)

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
