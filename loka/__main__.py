import argparse
import contextlib
import ctypes
import http.client
import http.server
import json
import os
import secrets
import shutil
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import uuid
import webbrowser
import zipfile
from pathlib import Path, PurePosixPath

PORT = 45873
GROUP = "239.255.45.87"
CHUNK = 1024 * 256
MAX_OFFER_ITEMS = 100


def request(host, port, method, path, data=None, headers=None, timeout=10):
    connection = http.client.HTTPConnection(host, port, timeout=timeout)
    connection.request(method, path, body=data, headers=headers or {})
    response = connection.getresponse()
    body = response.read()
    connection.close()
    if response.status >= 400:
        raise RuntimeError(body.decode("utf-8", "replace"))
    return json.loads(body) if body else None


def unique_path(path):
    if not path.exists():
        return path
    index = 1
    while True:
        candidate = path.with_name(f"{path.stem} ({index}){path.suffix}")
        if not candidate.exists():
            return candidate
        index += 1


def extract(archive, destination):
    """Extract each top-level item separately, never overwriting existing items."""
    destination = Path(destination).expanduser().resolve()
    if not destination.is_dir():
        raise ValueError("Destination must be an existing directory")
    with zipfile.ZipFile(archive) as archive_file:
        members = archive_file.infolist()
        roots = {}
        for info in members:
            path = PurePosixPath(info.filename)
            if (
                not path.parts
                or path.is_absolute()
                or any(p in (".", "..") for p in path.parts)
                or "\\" in info.filename
                or ":" in path.parts[0]
            ):
                raise ValueError("Invalid path in transfer")
            if (info.external_attr >> 16) & 0o170000 == 0o120000:
                raise ValueError("Symbolic links are not supported")
            roots.setdefault(path.parts[0], None)
        staging = Path(tempfile.mkdtemp(prefix=".loka-", dir=destination))
        try:
            for info in members:
                path = PurePosixPath(info.filename)
                target = staging.joinpath(*path.parts)
                if info.is_dir():
                    target.mkdir(parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(parents=True, exist_ok=True)
                    with archive_file.open(info) as source, target.open("wb") as output:
                        shutil.copyfileobj(source, output, CHUNK)
            for name in roots:
                source = staging / name
                source.rename(unique_path(destination / name))
        finally:
            shutil.rmtree(staging, ignore_errors=True)


def make_archive(paths, target, cancel=None):
    paths = [Path(path) for path in paths]
    original_names = {path.name for path in paths}
    used_names = set()
    with zipfile.ZipFile(
        target, "w", compression=zipfile.ZIP_STORED, allowZip64=True
    ) as archive_file:
        for path in paths:
            if path.is_symlink() or not path.exists():
                raise ValueError(f"Cannot share {path}: missing or symbolic link")
            name = path.name
            if name in used_names:
                index = 1
                while name in used_names or name in original_names:
                    name = f"{path.stem} ({index}){path.suffix}"
                    index += 1
            used_names.add(name)
            if path.is_dir():
                archive_file.writestr(name + "/", b"")
                for root, dirs, files in os.walk(path, followlinks=False):
                    root_path = Path(root)
                    dirs[:] = [d for d in dirs if not (root_path / d).is_symlink()]
                    for directory in dirs:
                        relative = (root_path / directory).relative_to(path).as_posix()
                        archive_file.writestr(f"{name}/{relative}/", b"")
                    for file in files:
                        child = root_path / file
                        if child.is_symlink():
                            raise ValueError("Symbolic links are not supported")
                        if cancel and cancel.is_set():
                            raise RuntimeError("Cancelled")
                        relative = child.relative_to(path).as_posix()
                        archive_file.write(child, f"{name}/{relative}")
            else:
                archive_file.write(path, name)


def choose_destination():
    if sys.platform.startswith("linux"):
        for executable, arguments in (
            (
                "zenity",
                ["--file-selection", "--directory", "--title=Choose destination"],
            ),
            ("kdialog", ["--getexistingdirectory"]),
        ):
            if not shutil.which(executable):
                continue
            try:
                result = subprocess.run(
                    [executable, *arguments],
                    capture_output=True,
                    text=True,
                    check=False,
                )
            except OSError:
                continue
            if result.returncode == 0:
                return result.stdout.strip()
            if result.returncode == 1:
                return ""

    result = subprocess.run(
        [
            sys.executable,
            "-c",
            (
                "import tkinter as tk; from tkinter import filedialog; "
                "root = tk.Tk(); root.withdraw(); "
                "print(filedialog.askdirectory()); root.destroy()"
            ),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    return result.stdout.strip()


class State:
    def __init__(self, port):
        self.id = uuid.uuid4().hex
        self.name = socket.gethostname()
        self.port = port
        self.ui_key = secrets.token_urlsafe(24)
        self.peers = {}
        self.transfers = {}
        self.shares = {}
        self.destination = ""
        self.discovery_error = ""
        self.discovery_send_error = ""
        self.lock = threading.RLock()
        self.stop = threading.Event()
        self.temp = tempfile.TemporaryDirectory(prefix="loka-")

    def transfer(self, **fields):
        entry = dict(
            id=uuid.uuid4().hex, progress=0, cancel=threading.Event(), **fields
        )
        entry.setdefault("status", "preparing")
        with self.lock:
            self.transfers[entry["id"]] = entry
        return entry

    def public(self, entry):
        return {
            key: value
            for key, value in entry.items()
            if key not in ("cancel", "archive", "secret")
        }

    def discover(self):
        threading.Thread(target=self._listen, daemon=True).start()
        threading.Thread(target=self._announce, daemon=True).start()

    def _listen(self):
        sock = None
        try:
            sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_UDP)
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind(("", PORT))
            interfaces = local_addresses() or ["0.0.0.0"]
            joined = 0
            for address in interfaces:
                try:
                    sock.setsockopt(
                        socket.IPPROTO_IP,
                        socket.IP_ADD_MEMBERSHIP,
                        socket.inet_aton(GROUP) + socket.inet_aton(address),
                    )
                    joined += 1
                except OSError:
                    continue
            if not joined:
                raise OSError("Could not join discovery on any network interface")
            sock.settimeout(1)
            while not self.stop.is_set():
                try:
                    data, address = sock.recvfrom(4096)
                    peer = json.loads(data)
                    if (
                        peer.get("service") == "loka"
                        and peer.get("id") != self.id
                        and isinstance(peer.get("port"), int)
                        and 0 < peer["port"] < 65536
                    ):
                        peer.update(host=address[0], seen=time.monotonic())
                        with self.lock:
                            self.peers[peer["id"]] = peer
                except (TimeoutError, ValueError, KeyError, TypeError):
                    continue
        except OSError as exc:
            self.discovery_error = (
                f"Discovery could not start: {exc}. "
                "Check your firewall and network connection."
            )
        finally:
            if sock:
                sock.close()

    def _announce(self):
        sock = None
        try:
            sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_UDP)
            sock.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_TTL, 1)
        except OSError as exc:
            if sock:
                sock.close()
            self.discovery_send_error = f"Could not start device announcements: {exc}"
            return
        payload = json.dumps(
            {"service": "loka", "id": self.id, "name": self.name, "port": self.port}
        ).encode()
        while not self.stop.is_set():
            sent = False
            for address in local_addresses() or ["0.0.0.0"]:
                try:
                    sock.setsockopt(
                        socket.IPPROTO_IP,
                        socket.IP_MULTICAST_IF,
                        socket.inet_aton(address),
                    )
                    sock.sendto(payload, (GROUP, PORT))
                    sent = True
                except OSError:
                    continue
            self.discovery_send_error = (
                ""
                if sent
                else "Could not announce this device. Check your firewall and network connection."
            )
            self.stop.wait(3)
        sock.close()


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "Loka/0.1"

    def log_message(self, *args):
        pass

    @property
    def state(self):
        return self.server.state

    def respond(self, status, value):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def body(self):
        length = int(self.headers.get("Content-Length", "0"))
        if length > 1024 * 1024:
            raise ValueError("Request too large")
        return json.loads(self.rfile.read(length) or b"{}")

    def local(self):
        return (
            self.client_address[0] in ("127.0.0.1", "::1")
            and self.headers.get("X-Loka-Key") == self.state.ui_key
        )

    def do_GET(self):
        parsed = urllib.parse.urlsplit(self.path)
        if parsed.path == "/" and self.client_address[0] in ("127.0.0.1", "::1"):
            data = (
                Path(__file__)
                .with_name("index.html")
                .read_text()
                .replace("__LOKA_KEY__", self.state.ui_key)
                .encode()
            )
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        elif parsed.path == "/api/state" and self.local():
            with self.state.lock:
                peers = [
                    p
                    for p in self.state.peers.values()
                    if time.monotonic() - p["seen"] < 12
                ]
                transfers = [
                    self.state.public(t) for t in self.state.transfers.values()
                ]
            self.respond(
                200,
                {
                    "name": self.state.name,
                    "peers": peers,
                    "transfers": transfers,
                    "addresses": local_addresses(),
                    "destination": self.state.destination,
                    "discovery_error": self.state.discovery_error
                    or self.state.discovery_send_error,
                },
            )
        elif parsed.path.startswith("/download/"):
            token = parsed.path.split("/")[-1]
            share = self.state.shares.get(token)
            if not share:
                return self.respond(404, {"error": "Link expired"})
            path = Path(share["archive"])
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header(
                "Content-Disposition",
                "attachment; filename*=UTF-8''"
                + urllib.parse.quote(share["filename"], safe=""),
            )
            self.send_header("Content-Length", str(path.stat().st_size))
            self.end_headers()
            with path.open("rb") as f:
                shutil.copyfileobj(f, self.wfile, CHUNK)
        elif parsed.path.startswith("/peer/status/"):
            transfer = self.state.transfers.get(parsed.path.split("/")[-1])
            if not transfer or self.headers.get("X-Transfer-Secret") != transfer.get(
                "secret"
            ):
                return self.respond(404, {"error": "Not found"})
            self.respond(200, {"status": transfer["status"]})
        else:
            self.respond(404, {"error": "Not found"})

    def do_POST(self):
        try:
            self.post()
        except (ValueError, KeyError, OSError, RuntimeError) as exc:
            self.respond(400, {"error": str(exc)})

    def post(self):
        path = urllib.parse.urlsplit(self.path).path
        if path == "/api/upload" and self.local():
            session = self.headers.get("X-Upload-Session", "")
            name = urllib.parse.unquote(self.headers.get("X-File-Path", ""))
            upload_path = PurePosixPath(name)
            parts = upload_path.parts
            if (
                not session.isalnum()
                or not parts
                or upload_path.is_absolute()
                or any(part in (".", "..") for part in parts)
                or "\\" in name
                or ":" in parts[0]
            ):
                raise ValueError("Invalid upload path")
            destination = Path(self.state.temp.name) / "uploads" / session
            target = destination.joinpath(*parts)
            if self.headers.get("X-Entry-Type") == "directory":
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                remaining = int(self.headers.get("Content-Length", "0"))
                with target.open("xb") as output:
                    while remaining:
                        chunk = self.rfile.read(min(CHUNK, remaining))
                        if not chunk:
                            raise ValueError("Upload interrupted")
                        output.write(chunk)
                        remaining -= len(chunk)
            return self.respond(200, {"path": str(destination / parts[0])})
        if path.startswith("/peer/cancel/"):
            entry = self.state.transfers.get(path.split("/")[-1])
            if not entry or self.headers.get("X-Transfer-Secret") != entry.get(
                "secret"
            ):
                return self.respond(404, {"error": "Not found"})
            entry["cancel"].set()
            if entry["status"] in ("waiting", "accepted"):
                entry["status"] = "cancelled"
            return self.respond(200, {"ok": True})
        if path == "/peer/offer":
            data = self.body()
            with self.state.lock:
                if any(
                    t["direction"] == "incoming"
                    and t["status"] in ("waiting", "accepted", "receiving")
                    for t in self.state.transfers.values()
                ):
                    return self.respond(409, {"error": "Receiver is busy"})
                entry = self.state.transfer(
                    direction="incoming",
                    name=str(data["name"])[:255],
                    items=data["items"][:MAX_OFFER_ITEMS],
                    size=int(data["size"]),
                    status="waiting",
                    secret=secrets.token_urlsafe(24),
                )
            return self.respond(200, {"id": entry["id"], "secret": entry["secret"]})
        if path.startswith("/peer/upload/"):
            entry = self.state.transfers.get(path.split("/")[-1])
            if not entry or self.headers.get("X-Transfer-Secret") != entry.get(
                "secret"
            ):
                return self.respond(404, {"error": "Not found"})
            with self.state.lock:
                if entry["status"] != "accepted":
                    return self.respond(409, {"error": "Transfer is not accepted"})
                entry["status"] = "receiving"
            size = int(self.headers.get("Content-Length", "0"))
            if size != entry["size"]:
                entry["status"] = "failed"
                return self.respond(400, {"error": "Transfer size does not match"})
            tmp = Path(self.state.temp.name) / (entry["id"] + ".zip")
            try:
                with tmp.open("wb") as f:
                    remaining = size
                    while remaining:
                        if entry["cancel"].is_set():
                            raise RuntimeError("Cancelled")
                        chunk = self.rfile.read(min(CHUNK, remaining))
                        if not chunk:
                            raise RuntimeError("Connection interrupted")
                        f.write(chunk)
                        remaining -= len(chunk)
                        entry["progress"] = (
                            int((size - remaining) * 100 / size) if size else 100
                        )
                extract(tmp, entry["destination"])
                entry["status"] = "complete"
            except Exception:
                entry["status"] = "cancelled" if entry["cancel"].is_set() else "failed"
                raise
            finally:
                tmp.unlink(missing_ok=True)
            return self.respond(200, {"status": entry["status"]})
        if not self.local():
            return self.respond(403, {"error": "Local access only"})
        data = self.body()
        if path == "/api/choose-destination":
            try:
                destination = choose_destination()
            except (OSError, subprocess.CalledProcessError) as exc:
                raise ValueError(
                    "Directory picker unavailable; enter a path instead"
                ) from exc
            return self.respond(200, {"destination": destination})
        if path == "/api/accept":
            entry = self.state.transfers[data["id"]]
            if entry["status"] != "waiting":
                raise ValueError("Transfer is no longer waiting")
            dest = Path(data["destination"]).expanduser().resolve()
            if not dest.is_dir():
                raise ValueError("Destination must be an existing directory")
            entry["destination"] = str(dest)
            self.state.destination = str(dest)
            entry["status"] = "accepted"
            return self.respond(200, {"ok": True})
        if path == "/api/cancel":
            entry = self.state.transfers[data["id"]]
            entry["cancel"].set()
            if entry["status"] in ("waiting", "accepted", "preparing"):
                entry["status"] = "cancelled"
            return self.respond(200, {"ok": True})
        if path in ("/api/send", "/api/share"):
            paths = data["paths"]
            if not isinstance(paths, list) or not paths:
                raise ValueError("Choose one or more paths")
            entry = self.state.transfer(
                direction="outgoing",
                name="Link" if path.endswith("share") else data["host"],
                items=[Path(p).name for p in paths],
                size=0,
            )
            thread = threading.Thread(
                target=outgoing,
                args=(
                    self.state,
                    entry,
                    paths,
                    data if path.endswith("send") else None,
                ),
                daemon=True,
            )
            thread.start()
            return self.respond(200, {"id": entry["id"]})
        if path == "/api/unshare":
            share = self.state.shares.pop(data["token"], None)
            if share:
                Path(share["archive"]).unlink(missing_ok=True)
                for entry in self.state.transfers.values():
                    if entry.get("token") == data["token"]:
                        entry["status"] = "stopped"
            return self.respond(200, {"ok": True})
        self.respond(404, {"error": "Not found"})


def outgoing(state, entry, paths, peer):
    archive = Path(state.temp.name) / (entry["id"] + ".zip")
    try:
        single_file = (
            peer is None
            and len(paths) == 1
            and Path(paths[0]).is_file()
            and not Path(paths[0]).is_symlink()
        )
        if single_file:
            shutil.copyfile(paths[0], archive)
        else:
            make_archive(paths, archive, entry["cancel"])
        if entry["cancel"].is_set():
            raise RuntimeError("Cancelled")
        entry["size"] = archive.stat().st_size
        if peer is None:
            token = secrets.token_urlsafe(24)
            state.shares[token] = {
                "archive": str(archive),
                "filename": Path(paths[0]).name if single_file else "loka-share.zip",
            }
            entry.update(status="shared", token=token, progress=100)
            return
        host, port = peer["host"], int(peer.get("port", PORT))
        offer_details = {
            "name": state.name,
            "items": entry["items"][:MAX_OFFER_ITEMS],
            "size": entry["size"],
        }
        offer = request(
            host,
            port,
            "POST",
            "/peer/offer",
            json.dumps(offer_details),
            {"Content-Type": "application/json"},
        )
        entry["status"] = "waiting"
        headers = {"X-Transfer-Secret": offer["secret"]}
        while not entry["cancel"].wait(1):
            status = request(
                host, port, "GET", "/peer/status/" + offer["id"], headers=headers
            )["status"]
            if status == "accepted":
                break
            if status != "waiting":
                raise RuntimeError("Receiver declined or cancelled")
        if entry["cancel"].is_set():
            with contextlib.suppress(Exception):
                request(
                    host, port, "POST", "/peer/cancel/" + offer["id"], headers=headers
                )
            raise RuntimeError("Cancelled")
        entry["status"] = "sending"
        connection = http.client.HTTPConnection(host, port, timeout=30)
        try:
            connection.putrequest("POST", "/peer/upload/" + offer["id"])
            connection.putheader("Content-Length", str(entry["size"]))
            connection.putheader("X-Transfer-Secret", offer["secret"])
            connection.endheaders()
            sent = 0
            with archive.open("rb") as f:
                while chunk := f.read(CHUNK):
                    if entry["cancel"].is_set():
                        raise RuntimeError("Cancelled")
                    connection.send(chunk)
                    sent += len(chunk)
                    entry["progress"] = int(sent * 100 / entry["size"])
            response = connection.getresponse()
            body = response.read()
            if response.status != 200:
                raise RuntimeError(body.decode("utf-8", "replace"))
        finally:
            connection.close()
        entry["status"] = "complete"
    except (
        OSError,
        ValueError,
        KeyError,
        TypeError,
        RuntimeError,
        http.client.HTTPException,
    ) as exc:
        entry["status"] = "cancelled" if entry["cancel"].is_set() else "failed"
        entry["error"] = str(exc)
    finally:
        if entry["status"] != "shared":
            archive.unlink(missing_ok=True)


def order_local_addresses(addresses):
    addresses = sorted(addresses)
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
            sock.connect(("192.0.2.1", 9))
            preferred = sock.getsockname()[0]
    except OSError:
        return addresses
    if preferred in addresses:
        addresses.remove(preferred)
        addresses.insert(0, preferred)
    return addresses


def local_addresses():
    """Find IPv4 addresses on local interfaces without relying on host DNS."""
    addresses = set()
    try:
        if sys.platform == "win32":
            from ctypes import wintypes

            class SocketAddress(ctypes.Structure):
                _fields_ = [("address", ctypes.c_void_p), ("length", ctypes.c_int)]

            class Unicast(ctypes.Structure):
                pass

            Unicast._fields_ = [
                ("alignment", ctypes.c_uint64),
                ("next", ctypes.POINTER(Unicast)),
                ("socket", SocketAddress),
            ]

            class Adapter(ctypes.Structure):
                pass

            Adapter._fields_ = [
                ("alignment", ctypes.c_uint64),
                ("next", ctypes.POINTER(Adapter)),
                ("name", ctypes.c_char_p),
                ("unicast", ctypes.POINTER(Unicast)),
            ]
            size = wintypes.ULONG(0)
            get_adapters = ctypes.windll.iphlpapi.GetAdaptersAddresses
            get_adapters(socket.AF_INET, 0, None, None, ctypes.byref(size))
            buffer = ctypes.create_string_buffer(size.value)
            if get_adapters(socket.AF_INET, 0, None, buffer, ctypes.byref(size)) != 0:
                return []
            adapter = ctypes.cast(buffer, ctypes.POINTER(Adapter))
            while adapter:
                unicast = adapter.contents.unicast
                while unicast:
                    sockaddr = unicast.contents.socket
                    if sockaddr.address and sockaddr.length >= 8:
                        raw = ctypes.string_at(sockaddr.address, sockaddr.length)
                        if int.from_bytes(raw[:2], sys.byteorder) == socket.AF_INET:
                            addresses.add(socket.inet_ntoa(raw[4:8]))
                    unicast = unicast.contents.next
                adapter = adapter.contents.next
        else:

            class Interface(ctypes.Structure):
                pass

            Interface._fields_ = [
                ("next", ctypes.POINTER(Interface)),
                ("name", ctypes.c_char_p),
                ("flags", ctypes.c_uint),
                ("address", ctypes.c_void_p),
                ("netmask", ctypes.c_void_p),
                ("union", ctypes.c_void_p),
                ("data", ctypes.c_void_p),
            ]
            first = ctypes.POINTER(Interface)()
            libc = ctypes.CDLL(None)
            if libc.getifaddrs(ctypes.byref(first)) != 0:
                return []
            try:
                current = first
                while current:
                    interface = current.contents
                    if (
                        interface.address
                        and interface.flags & 1
                        and not interface.flags & 8
                    ):
                        raw = ctypes.string_at(interface.address, 8)
                        family = (
                            raw[1]
                            if sys.platform == "darwin"
                            else int.from_bytes(raw[:2], sys.byteorder)
                        )
                        if family == socket.AF_INET:
                            addresses.add(socket.inet_ntoa(raw[4:8]))
                    current = interface.next
            finally:
                libc.freeifaddrs(first)
    except (AttributeError, OSError, ValueError):
        return []
    return order_local_addresses(
        address
        for address in addresses
        if not address.startswith("127.") and address != "0.0.0.0"
    )


def main():
    parser = argparse.ArgumentParser(description="Share files on your local network")
    parser.add_argument(
        "command",
        nargs="?",
        choices=["serve", "send", "receive", "devices", "share"],
        default="serve",
    )
    parser.add_argument("paths", nargs="*", help="files or directories to send")
    parser.add_argument("--to", help="receiver address for send")
    parser.add_argument("--address", help="local address to use in a share link")
    parser.add_argument("--peer-port", type=int, default=PORT, help="receiver port")
    parser.add_argument(
        "--port", type=int, default=PORT, help="listening port (default: %(default)s)"
    )
    parser.add_argument(
        "--no-browser", action="store_true", help="do not open the browser"
    )
    args = parser.parse_intermixed_args()
    if args.command == "share":
        if not args.paths:
            parser.error("share requires one or more paths")
        state = State(args.port)
        server = Server(("", args.port), Handler)
        server.state = state
        state.port = server.server_address[1]
        addresses = local_addresses()
        address = args.address or (addresses[0] if addresses else None)
        if not address:
            server.server_close()
            state.temp.cleanup()
            parser.error("could not detect a local address; supply --address")
        entry = state.transfer(direction="outgoing", name="Link", items=[], size=0)
        outgoing(state, entry, args.paths, None)
        if entry["status"] != "shared":
            print(entry.get("error", "Could not create link"), file=sys.stderr)
            server.server_close()
            state.temp.cleanup()
            return 1
        print(f"http://{address}:{state.port}/download/{entry['token']}", flush=True)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass
        finally:
            server.server_close()
            state.temp.cleanup()
        return 0
    if args.command == "send":
        if not args.paths or not args.to:
            parser.error("send requires --to ADDRESS and one or more paths")
        state = State(0)
        entry = state.transfer(
            direction="outgoing",
            name=args.to,
            items=[Path(p).name for p in args.paths],
            size=0,
        )
        thread = threading.Thread(
            target=outgoing,
            args=(state, entry, args.paths, {"host": args.to, "port": args.peer_port}),
        )
        thread.start()
        try:
            while thread.is_alive():
                print(
                    f"\r{entry['status']}: {entry['progress']}%   ", end="", flush=True
                )
                thread.join(0.5)
        except KeyboardInterrupt:
            entry["cancel"].set()
            thread.join()
        print(f"\r{entry['status']}: {entry['progress']}%   ")
        if entry.get("error"):
            print(entry["error"])
        state.temp.cleanup()
        return 0 if entry["status"] == "complete" else 1
    state = State(args.port)
    server = Server(("", args.port), Handler)
    server.state = state
    state.port = server.server_address[1]
    state.discover()
    url = f"http://127.0.0.1:{state.port}/"
    print(f"Loka is running at {url} (Ctrl+C to stop)", flush=True)
    if args.command == "devices":
        try:
            time.sleep(5)
            with state.lock:
                for peer in state.peers.values():
                    print(f"{peer['name']}  {peer['host']}:{peer['port']}")
        finally:
            state.stop.set()
            server.server_close()
            state.temp.cleanup()
        return 0
    if args.command == "receive":
        threading.Thread(target=server.serve_forever, daemon=True).start()
        print("Waiting for transfers. Press Ctrl+C to stop.")
        try:
            while True:
                waiting = next(
                    (
                        e
                        for e in list(state.transfers.values())
                        if e["direction"] == "incoming" and e["status"] == "waiting"
                    ),
                    None,
                )
                if waiting:
                    print(
                        f"\n{waiting['name']} wants to send: {', '.join(waiting['items'])}"
                    )
                    destination = input(
                        "Destination directory (empty to decline): "
                    ).strip()
                    if destination and Path(destination).expanduser().is_dir():
                        waiting["destination"] = str(
                            Path(destination).expanduser().resolve()
                        )
                        waiting["status"] = "accepted"
                        while waiting["status"] in ("accepted", "receiving"):
                            print(
                                f"\r{waiting['status']}: {waiting['progress']}%   ",
                                end="",
                                flush=True,
                            )
                            time.sleep(0.5)
                        print(f"\r{waiting['status']}: {waiting['progress']}%   ")
                    else:
                        waiting["status"] = "cancelled"
                time.sleep(0.5)
        except KeyboardInterrupt:
            server.shutdown()
            state.stop.set()
            server.server_close()
            state.temp.cleanup()
            return 0
    if not args.no_browser:
        webbrowser.open(url)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        state.stop.set()
        server.server_close()
        state.temp.cleanup()


if __name__ == "__main__":
    raise SystemExit(main())
