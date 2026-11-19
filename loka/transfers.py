import contextlib
import json
import ntpath
import os
import stat
import struct
import tempfile
import zipfile
from dataclasses import dataclass
from pathlib import Path, PureWindowsPath

CHUNK = 256 * 1024
STREAM_FORMAT = "loka-stream-v1"
STREAM_CONTENT_TYPE = "application/vnd.loka.stream"
STREAM_END = b"\0"
MAX_MANIFEST_BYTES = 16 * 1024 * 1024
MANIFEST_LENGTH = struct.Struct("!I")
WINDOWS_NAME_REPLACEMENTS = str.maketrans(
    {char: "_" for char in '<>:"|?*' + "".join(map(chr, range(32)))}
)


@dataclass(frozen=True)
class Source:
    path: Path
    info: os.stat_result

    @classmethod
    def selected(cls, path):
        path = Path(path).absolute()
        info = path.lstat()
        if not (stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode)):
            raise ValueError(f"Cannot share {path}: choose a regular file or directory")
        return cls(path, info)

    @property
    def directory(self):
        return stat.S_ISDIR(self.info.st_mode)

    def verify(self):
        current = self.path.lstat()
        if not os.path.samestat(self.info, current) or stat.S_IFMT(
            current.st_mode
        ) != stat.S_IFMT(self.info.st_mode):
            raise FileNotFoundError(self.path)

    @contextlib.contextmanager
    def open(self):
        self.verify()
        flags = os.O_RDONLY | getattr(os, "O_BINARY", 0)
        flags |= getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
        with os.fdopen(os.open(self.path, flags), "rb") as file:
            current = os.fstat(file.fileno())
            if not stat.S_ISREG(current.st_mode) or not os.path.samestat(
                self.info, current
            ):
                raise FileNotFoundError(self.path)
            yield file, current.st_size


def _numbered_name(name, index):
    path = Path(name)
    return f"{path.stem} ({index}){path.suffix}"


def selection_members(sources, cancel=None):
    original_names = {source.path.name for source in sources}
    used_names = set()
    members = []
    for source in sources:
        name = source.path.name
        if not name:
            raise ValueError("Choose items inside the root directory")
        if name in used_names:
            index = 1
            while name in used_names or name in original_names:
                name = _numbered_name(source.path.name, index)
                index += 1
        used_names.add(name)
        pending = [(name, source)]
        while pending:
            if cancel is not None and cancel.is_set():
                raise RuntimeError("Cancelled")
            member_name, member = pending.pop()
            member.verify()
            members.append((member_name, member))
            if member.directory:
                with os.scandir(member.path) as children:
                    for child in children:
                        if child.is_symlink() and child.is_dir():
                            continue
                        selected = Source.selected(child.path)
                        pending.append((f"{member_name}/{child.name}", selected))
                member.verify()
    return members


def write_zip(members, archive):
    for name, source in members:
        source.verify()
        info = zipfile.ZipInfo.from_file(source.path, name, strict_timestamps=False)
        if source.directory:
            archive.writestr(info, b"")
        else:
            with source.open() as (file, size):
                info.file_size = size
                with archive.open(info, "w") as output:
                    copy_bytes(file, output.write, size)


def _parts(name):
    if not isinstance(name, str) or "\\" in name or "\0" in name:
        raise ValueError("Invalid path in transfer")
    parts = tuple(name.split("/"))
    if any(part in ("", ".", "..") for part in parts):
        raise ValueError("Invalid path in transfer")
    return parts


def stream_manifest(members):
    records = []
    size = 0
    for name, source in members:
        _parts(name)
        length = None if source.directory else source.info.st_size
        records.append([name, length])
        size += length or 0
    manifest = json.dumps(records, separators=(",", ":")).encode("ascii")
    if len(manifest) > MAX_MANIFEST_BYTES:
        raise ValueError("Too many paths for one transfer")
    return MANIFEST_LENGTH.pack(len(manifest)) + manifest, size


def copy_bytes(source, write, length, advance=None):
    while length:
        if advance is not None:
            advance(0)
        block = source.read(min(length, CHUNK))
        if not block:
            raise ValueError("Transfer interrupted or source file changed")
        write(block)
        length -= len(block)
        if advance is not None:
            advance(len(block))


def _windows_name(name):
    name = name.translate(WINDOWS_NAME_REPLACEMENTS).rstrip(" .") or "_"
    is_reserved = (
        ntpath.isreserved(name)
        if hasattr(ntpath, "isreserved")
        else PureWindowsPath(name).is_reserved()
    )
    return "_" + name if is_reserved else name


def _target_paths(staging, records, windows):
    if not windows:
        return {parts: staging.joinpath(*parts) for parts in records}

    targets = {(): staging}
    used = set()
    # Reserve names Windows can use as-is before assigning conflicting names.
    order = sorted(
        records, key=lambda parts: tuple(_windows_name(part) != part for part in parts)
    )
    for parts in order:
        parent = targets[parts[:-1]]
        name = _windows_name(parts[-1])
        target = parent / name
        index = 1
        while str(target).casefold() in used:
            target = parent / _numbered_name(name, index)
            index += 1
        used.add(str(target).casefold())
        targets[parts] = target
    return targets


def _read_manifest(source, content_length, size):
    header = source.read(MANIFEST_LENGTH.size)
    if len(header) != MANIFEST_LENGTH.size:
        raise ValueError("Transfer interrupted before the file list")
    (manifest_size,) = MANIFEST_LENGTH.unpack(header)
    if not 0 < manifest_size <= MAX_MANIFEST_BYTES:
        raise ValueError("Invalid transfer file list size")
    if content_length != MANIFEST_LENGTH.size + manifest_size + size + len(STREAM_END):
        raise ValueError("Transfer size does not match the offer")
    manifest = source.read(manifest_size)
    if len(manifest) != manifest_size:
        raise ValueError("Transfer interrupted while reading the file list")
    try:
        entries = json.loads(manifest)
    except RecursionError as error:
        raise ValueError("Invalid transfer file list") from error
    if not isinstance(entries, list) or not entries:
        raise ValueError("Invalid transfer file list")
    records = {}
    total = 0
    for entry in entries:
        if not isinstance(entry, list) or len(entry) != 2:
            raise ValueError("Invalid entry in transfer file list")
        name, file_size = entry
        parts = _parts(name)
        if parts in records:
            raise ValueError("Duplicate path in transfer")
        if file_size is not None and (type(file_size) is not int or file_size < 0):
            raise ValueError("Invalid file size in transfer")
        records[parts] = file_size
        total += file_size or 0
    if total != size or any(
        len(parts) > 1
        and (parts[:-1] not in records or records[parts[:-1]] is not None)
        for parts in records
    ):
        raise ValueError("Invalid transfer file list")
    return records


@contextlib.contextmanager
def stage_stream(source, content_length, size, destination, advance, *, windows=None):
    records = _read_manifest(source, content_length, size)
    destination = Path(destination).expanduser().resolve()
    if not destination.is_dir():
        raise ValueError("Destination must be an existing directory")
    if windows is None:
        windows = os.name == "nt"
    advance(0)
    with tempfile.TemporaryDirectory(prefix=".loka-", dir=destination) as directory:
        staging = Path(directory)
        targets = _target_paths(staging, records, windows)
        for parts, length in records.items():
            advance(0)
            target = targets[parts]
            if length is None:
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with target.open("xb") as output:
                    copy_bytes(source, output.write, length, advance)
        if source.read(len(STREAM_END)) != STREAM_END:
            raise ValueError("Transfer interrupted before completion")
        yield destination, [targets[parts] for parts in records if len(parts) == 1]


def _rename_to_available_name(source, destination):
    candidate = destination / source.name
    index = 1
    while True:
        is_directory = source.is_dir()
        try:
            if os.name == "nt":
                source.rename(candidate)
                return candidate

            # On Unix, reserve the name before rename can replace it.
            if is_directory:
                candidate.mkdir()
            else:
                candidate.touch(exist_ok=False)
            try:
                source.rename(candidate)
            except OSError:
                if is_directory:
                    candidate.rmdir()
                else:
                    candidate.unlink()
                raise
            return candidate
        except FileExistsError:
            candidate = destination / _numbered_name(source.name, index)
            index += 1


def publish(staged, destination):
    moved = []
    try:
        for source in staged:
            target = _rename_to_available_name(source, destination)
            moved.append((source, target))
    except OSError:
        for source, target in reversed(moved):
            with contextlib.suppress(OSError):
                target.rename(source)
        raise
