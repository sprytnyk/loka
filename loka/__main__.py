import argparse
import contextlib
import ctypes
import errno
import http.client
import http.server
import json
import os
import secrets
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import uuid
import zipfile
from pathlib import Path, PurePosixPath

from loka.pickers import choose_paths, open_browser
from loka.transfers import (
    CHUNK,
    MANIFEST_LENGTH,
    STREAM_CONTENT_TYPE,
    STREAM_END,
    STREAM_FORMAT,
    Source,
    copy_bytes,
    publish,
    selection_members,
    stage_stream,
    stream_manifest,
    write_zip,
)
from loka.version import current_version

PORT = 45873
GROUP = "239.255.45.87"
MAX_OFFER_ITEMS = 100
VERSION = current_version()


def request(host, port, method, path, data=None, headers=None, timeout=10):
    with contextlib.closing(
        http.client.HTTPConnection(host, port, timeout=timeout)
    ) as connection:
        connection.request(method, path, body=data, headers=headers or {})
        response = connection.getresponse()
        body = response.read()
    if response.status >= 400:
        raise RuntimeError(body.decode("utf-8", "replace"))
    return json.loads(body) if body else None


def transfer_progress(entry):
    total = entry["size"]
    copied = 0

    def advance(size):
        nonlocal copied
        if entry["cancel"].is_set():
            raise RuntimeError("Cancelled")
        copied += size
        if total:
            entry["progress"] = copied * 100 // total

    return advance


class ZipResponse:
    def __init__(self, output, *, chunked):
        self.output = output
        self.chunked = chunked
        self.failed = False

    def write(self, data):
        if data and not self.failed:
            if self.chunked:
                self.output.write(f"{len(data):x}\r\n".encode("ascii") + data + b"\r\n")
            else:
                self.output.write(data)
        return len(data)

    def flush(self):
        self.output.flush()

    def finish(self):
        if self.chunked:
            self.output.write(b"0\r\n\r\n")


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
            if key not in ("cancel", "secret")
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
            page = Path(__file__).with_name("index.html").read_text(encoding="utf-8")
            data = (
                page.replace("__LOKA_KEY__", self.state.ui_key)
                .replace("__LOKA_VERSION__", VERSION)
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
            with self.state.lock:
                sources = self.state.shares.get(token)
            if not sources:
                return self.respond(404, {"error": "Link expired"})
            self.download(sources)
        elif parsed.path.startswith("/peer/status/"):
            transfer = self.state.transfers.get(parsed.path.split("/")[-1])
            if not transfer or self.headers.get("X-Transfer-Secret") != transfer.get(
                "secret"
            ):
                return self.respond(404, {"error": "Not found"})
            self.respond(200, {"status": transfer["status"]})
        else:
            self.respond(404, {"error": "Not found"})

    def download(self, sources):
        direct = len(sources) == 1 and not sources[0].directory
        with contextlib.ExitStack() as opened:
            try:
                if direct:
                    file, size = opened.enter_context(sources[0].open())
                    filename = sources[0].path.name
                else:
                    members = selection_members(sources)
                    filename = "loka-share.zip"
            except (OSError, ValueError):
                return self.respond(
                    404, {"error": "Shared items are no longer available"}
                )

            chunked = not direct and self.request_version == "HTTP/1.1"
            if chunked:
                self.protocol_version = "HTTP/1.1"
            self.close_connection = True
            self.send_response(200)
            self.send_header("Connection", "close")
            self.send_header(
                "Content-Type",
                "application/octet-stream" if direct else "application/zip",
            )
            self.send_header(
                "Content-Disposition",
                "attachment; filename*=UTF-8''"
                + urllib.parse.quote(filename, safe="", errors="surrogateescape"),
            )
            if direct:
                self.send_header("Content-Length", str(size))
            elif chunked:
                self.send_header("Transfer-Encoding", "chunked")
            try:
                self.end_headers()
                if direct:
                    copy_bytes(file, self.wfile.write, size)
                    return
                stream = ZipResponse(self.wfile, chunked=chunked)
                with zipfile.ZipFile(
                    stream, "w", compression=zipfile.ZIP_STORED
                ) as archive:
                    try:
                        write_zip(members, archive)
                    except BaseException:
                        # An interrupted HTTP/1.0 response must also lack a ZIP footer.
                        stream.failed = True
                        raise
                stream.finish()
            except (OSError, ValueError, RuntimeError):
                # Closing early leaves an incomplete body that the client can detect.
                return

    def do_POST(self):
        try:
            self.post()
        except (ValueError, KeyError, OSError, RuntimeError) as exc:
            self.respond(400, {"error": str(exc)})

    def _receive_transfer(self, entry):
        with self.state.lock:
            if entry["status"] != "accepted":
                return self.respond(409, {"error": "Transfer is not accepted"})
            entry["status"] = "receiving"
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if (
                self.headers.get("Content-Type") != STREAM_CONTENT_TYPE
                or self.headers.get("Transfer-Encoding")
                or length <= MANIFEST_LENGTH.size + entry["size"] + len(STREAM_END)
            ):
                raise ValueError("Invalid streaming transfer headers")
            self.connection.settimeout(30)
            advance = transfer_progress(entry)
            with (
                stage_stream(
                    self.rfile, length, entry["size"], entry["destination"], advance
                ) as (destination, staged),
                self.state.lock,
            ):
                advance(0)
                publish(staged, destination)
                entry.update(status="complete", progress=100)
        except Exception:
            with self.state.lock:
                entry["status"] = "cancelled" if entry["cancel"].is_set() else "failed"
            raise
        return self.respond(200, {"status": entry["status"]})

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
                or (os.name == "nt" and any(":" in part for part in parts))
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
            with self.state.lock:
                entry["cancel"].set()
                if entry["status"] in ("waiting", "accepted"):
                    entry["status"] = "cancelled"
            return self.respond(200, {"ok": True})
        if path == "/peer/offer":
            data = self.body()
            if not isinstance(data, dict) or data.get("format") != STREAM_FORMAT:
                raise ValueError("Update both Loka devices to use streaming transfers")
            size = data.get("size")
            items = data.get("items")
            if (
                type(size) is not int
                or size < 0
                or not isinstance(items, list)
                or not items
                or not all(isinstance(item, str) for item in items)
            ):
                raise ValueError("Invalid transfer offer")
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
                    items=items[:MAX_OFFER_ITEMS],
                    size=size,
                    status="waiting",
                    secret=secrets.token_urlsafe(24),
                )
            return self.respond(
                200,
                {"id": entry["id"], "secret": entry["secret"], "format": STREAM_FORMAT},
            )
        if path.startswith("/peer/upload/"):
            entry = self.state.transfers.get(path.split("/")[-1])
            if not entry or self.headers.get("X-Transfer-Secret") != entry.get(
                "secret"
            ):
                return self.respond(404, {"error": "Not found"})
            return self._receive_transfer(entry)
        if not self.local():
            return self.respond(403, {"error": "Local access only"})
        data = self.body()
        if path in ("/api/choose-paths", "/api/choose-destination"):
            destination = path == "/api/choose-destination"
            if destination:
                kind = "destination"
            else:
                kind = data.get("kind") if isinstance(data, dict) else None
                if kind not in ("files", "directory"):
                    raise ValueError("Choose files or a directory")
            try:
                paths = choose_paths(kind)
            except (OSError, ValueError, subprocess.CalledProcessError) as exc:
                fallback = (
                    "enter a path"
                    if destination
                    else "drop files or a directory here, or enter a path"
                )
                raise ValueError(
                    f"Desktop picker unavailable. Please {fallback}."
                ) from exc
            if destination:
                return self.respond(200, {"destination": paths[0] if paths else ""})
            return self.respond(200, {"paths": paths})
        if path == "/api/accept":
            dest = Path(data["destination"]).expanduser().resolve()
            with self.state.lock:
                entry = self.state.transfers[data["id"]]
                if entry["direction"] != "incoming" or entry["status"] != "waiting":
                    raise ValueError("Transfer is no longer waiting")
                if not dest.is_dir():
                    raise ValueError("Destination must be an existing directory")
                entry["destination"] = str(dest)
                self.state.destination = str(dest)
                entry["status"] = "accepted"
            return self.respond(200, {"ok": True})
        if path == "/api/cancel":
            with self.state.lock:
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
            token = data["token"]
            with self.state.lock:
                share = self.state.shares.pop(token, None)
                if share:
                    for entry in self.state.transfers.values():
                        if entry.get("token") == token:
                            entry["status"] = "stopped"
                            break
            return self.respond(200, {"ok": True})
        self.respond(404, {"error": "Not found"})


def outgoing(state, entry, paths, peer):
    offer = None
    try:
        if entry["cancel"].is_set():
            raise RuntimeError("Cancelled")
        sources = tuple(Source.selected(path) for path in paths)
        if peer is None:
            token = secrets.token_urlsafe(24)
            with state.lock:
                if entry["cancel"].is_set():
                    raise RuntimeError("Cancelled")
                state.shares[token] = sources
                if len(sources) == 1 and not sources[0].directory:
                    entry["size"] = sources[0].info.st_size
                entry.update(status="shared", token=token, progress=100)
            return

        members = selection_members(sources, entry["cancel"])
        manifest, size = stream_manifest(members)
        entry["size"] = size
        advance = transfer_progress(entry)
        advance(0)
        host, port = peer["host"], int(peer.get("port", PORT))
        offer_details = {
            "name": state.name,
            "items": entry["items"][:MAX_OFFER_ITEMS],
            "size": size,
            "format": STREAM_FORMAT,
        }
        offer = request(
            host,
            port,
            "POST",
            "/peer/offer",
            json.dumps(offer_details),
            {"Content-Type": "application/json"},
        )
        if not isinstance(offer, dict) or offer.get("format") != STREAM_FORMAT:
            raise RuntimeError("Update both Loka devices to use streaming transfers")
        headers = {"X-Transfer-Secret": offer["secret"]}
        entry["status"] = "waiting"
        while not entry["cancel"].wait(1):
            status = request(
                host, port, "GET", "/peer/status/" + offer["id"], headers=headers
            )["status"]
            if status == "accepted":
                break
            if status != "waiting":
                raise RuntimeError("Receiver declined or cancelled")

        advance(0)
        entry["status"] = "sending"
        with contextlib.closing(
            http.client.HTTPConnection(host, port, timeout=30)
        ) as connection:
            connection.putrequest("POST", "/peer/upload/" + offer["id"])
            connection.putheader("Content-Type", STREAM_CONTENT_TYPE)
            connection.putheader(
                "Content-Length", str(size + len(manifest) + len(STREAM_END))
            )
            connection.putheader("X-Transfer-Secret", offer["secret"])
            connection.endheaders()
            connection.send(manifest)
            for _, source in members:
                advance(0)
                if source.directory:
                    continue
                with source.open() as (file, length):
                    if length != source.info.st_size:
                        raise ValueError(f"Source file changed: {source.path}")
                    copy_bytes(file, connection.send, length, advance)
            advance(0)
            connection.send(STREAM_END)
            response = connection.getresponse()
            body = response.read()
            if response.status != 200:
                raise RuntimeError(body.decode("utf-8", "replace"))
        entry.update(status="complete", progress=100)
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
        if isinstance(offer, dict):
            with contextlib.suppress(
                KeyError, OSError, RuntimeError, TypeError, http.client.HTTPException
            ):
                request(
                    host,
                    port,
                    "POST",
                    "/peer/cancel/" + offer["id"],
                    headers={"X-Transfer-Secret": offer["secret"]},
                    timeout=2,
                )


def order_local_addresses(lan_addresses, other_addresses=()):
    lan_addresses = set(lan_addresses)
    addresses = lan_addresses.union(other_addresses)
    route_address = None
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
            sock.connect(("192.0.2.1", 9))
            route_address = sock.getsockname()[0]
    except OSError:
        pass
    return sorted(
        (
            address
            for address in addresses
            if not address.startswith("127.") and address != "0.0.0.0"
        ),
        key=lambda address: (
            address not in lan_addresses,
            address != route_address,
            address,
        ),
    )


def _windows_ipv4_addresses():
    from ctypes import wintypes

    addresses = []

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
        ("anycast", ctypes.c_void_p),
        ("multicast", ctypes.c_void_p),
        ("dns_server", ctypes.c_void_p),
        ("dns_suffix", ctypes.c_wchar_p),
        ("description", ctypes.c_wchar_p),
        ("friendly_name", ctypes.c_wchar_p),
        ("physical_address", ctypes.c_ubyte * 8),
        ("physical_address_length", wintypes.ULONG),
        ("flags", wintypes.ULONG),
        ("mtu", wintypes.ULONG),
        ("if_type", wintypes.ULONG),
    ]
    size = wintypes.ULONG(0)
    get_adapters = ctypes.windll.iphlpapi.GetAdaptersAddresses
    get_adapters(socket.AF_INET, 0, None, None, ctypes.byref(size))
    buffer = ctypes.create_string_buffer(size.value)
    if get_adapters(socket.AF_INET, 0, None, buffer, ctypes.byref(size)) != 0:
        return None
    adapter = ctypes.cast(buffer, ctypes.POINTER(Adapter))
    while adapter:
        interface = adapter.contents
        description = (interface.description or "").lower()
        vpn = any(
            name in description
            for name in ("openvpn", "tap-windows", "wireguard", "wintun")
        )
        # IF_TYPE_ETHERNET_CSMACD and IF_TYPE_IEEE80211.
        on_lan = interface.if_type in (6, 71) and not vpn
        unicast = interface.unicast
        while unicast:
            sockaddr = unicast.contents.socket
            if sockaddr.address and sockaddr.length >= 8:
                raw = ctypes.string_at(sockaddr.address, sockaddr.length)
                if int.from_bytes(raw[:2], sys.byteorder) == socket.AF_INET:
                    addresses.append((socket.inet_ntoa(raw[4:8]), on_lan))
            unicast = unicast.contents.next
        adapter = interface.next
    return addresses


def _unix_ipv4_addresses():
    iff_up = 0x1
    iff_loopback = 0x8
    iff_point_to_point = 0x10
    addresses = []

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
        return None
    try:
        current = first
        while current:
            interface = current.contents
            current = interface.next
            flags = interface.flags
            if not interface.address or not (flags & iff_up) or flags & iff_loopback:
                continue
            raw = ctypes.string_at(interface.address, 8)
            family = (
                raw[1]
                if sys.platform == "darwin"
                else int.from_bytes(raw[:2], sys.byteorder)
            )
            if family != socket.AF_INET:
                continue
            point_to_point = flags & iff_point_to_point
            virtual_bridge = interface.name.startswith(
                (b"docker", b"br-", b"virbr", b"veth")
            )
            on_lan = not (point_to_point or virtual_bridge)
            addresses.append((socket.inet_ntoa(raw[4:8]), on_lan))
    finally:
        libc.freeifaddrs(first)
    return addresses


def local_addresses():
    """Find IPv4 addresses on local interfaces without relying on host DNS."""
    read_addresses = (
        _windows_ipv4_addresses if sys.platform == "win32" else _unix_ipv4_addresses
    )
    try:
        addresses = read_addresses()
    except (AttributeError, OSError, ValueError):
        return []
    if addresses is None:
        return []

    lan_addresses = set()
    other_addresses = set()
    for address, on_lan in addresses:
        (lan_addresses if on_lan else other_addresses).add(address)
    return order_local_addresses(lan_addresses, other_addresses)


def main():
    parser = argparse.ArgumentParser(description="Share files on your local network")
    parser.add_argument("--version", action="version", version=f"loka {VERSION}")
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
        "--port",
        type=int,
        default=PORT,
        help="listening port, use 0 for a free port (default: %(default)s)",
    )
    parser.add_argument(
        "--no-browser", action="store_true", help="do not open the browser"
    )
    args = parser.parse_intermixed_args()
    if args.command == "share" and not args.paths:
        parser.error("share requires one or more paths")
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
    if not 0 <= args.port <= 65535:
        parser.error("--port must be between 0 and 65535")
    try:
        server = Server(("", args.port), Handler)
    except OSError as exc:
        if exc.errno in (
            errno.EADDRINUSE,
            getattr(errno, "WSAEADDRINUSE", errno.EADDRINUSE),
        ):
            print(
                f"Port {args.port} is already in use. Stop the other program or run "
                "with --port 0 to pick a free port.",
                file=sys.stderr,
            )
        else:
            print(
                f"Could not listen on port {args.port}. {exc.strerror or exc}",
                file=sys.stderr,
            )
        return 1
    state = State(server.server_port)
    server.state = state
    if args.command == "share":
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
    state.discover()
    url = f"http://127.0.0.1:{state.port}/"
    print(f"Loka {VERSION} is running at {url} (Ctrl+C to stop)", flush=True)
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
                    target = (
                        Path(destination).expanduser().resolve()
                        if destination
                        else None
                    )
                    with state.lock:
                        if waiting["status"] != "waiting":
                            continue
                        if target is None or not target.is_dir():
                            waiting["status"] = "cancelled"
                            continue
                        waiting["destination"] = str(target)
                        waiting["status"] = "accepted"
                    while waiting["status"] in ("accepted", "receiving"):
                        print(
                            f"\r{waiting['status']}: {waiting['progress']}%   ",
                            end="",
                            flush=True,
                        )
                        time.sleep(0.5)
                    print(f"\r{waiting['status']}: {waiting['progress']}%   ")
                time.sleep(0.5)
        except KeyboardInterrupt:
            server.shutdown()
            state.stop.set()
            server.server_close()
            state.temp.cleanup()
            return 0
    if not args.no_browser:
        open_browser(url)
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
