"""Smoke-test and archive the executable built for this platform."""

import gzip
import hashlib
import platform
import re
import subprocess
import tarfile
import zipfile
from pathlib import Path
from tempfile import TemporaryDirectory


def write_archive(archive, binary, readme):
    files = [(binary, 0o755), (readme, 0o644)]
    if archive.suffix == ".zip":
        with zipfile.ZipFile(archive, "w") as bundle:
            for source, mode in files:
                entry = zipfile.ZipInfo(source.name)
                entry.create_system = 3
                entry.external_attr = (0o100000 | mode) << 16
                bundle.writestr(entry, source.read_bytes(), zipfile.ZIP_DEFLATED)
    else:
        with (
            archive.open("wb") as output,
            gzip.GzipFile(
                filename="", fileobj=output, mode="wb", mtime=0
            ) as compressed,
            tarfile.open(fileobj=compressed, mode="w|") as bundle,
        ):
            for source, mode in files:
                entry = tarfile.TarInfo(source.name)
                entry.size = source.stat().st_size
                entry.mode = mode
                with source.open("rb") as contents:
                    bundle.addfile(entry, contents)


def main():
    root = Path(__file__).resolve().parent.parent
    system = platform.system().lower()
    machine = platform.machine().lower()
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(machine, machine)
    windows = system == "windows"
    binary = root / "dist" / ("loka.exe" if windows else "loka")
    executable_version = subprocess.check_output(
        [str(binary), "--version"], cwd=binary.parent, text=True
    ).strip()
    match = re.fullmatch(r"loka ([0-9][A-Za-z0-9.+]*)", executable_version)
    if not match:
        raise SystemExit("Could not read the Loka version from the executable.")
    version = match[1]
    help_text = subprocess.check_output(
        [str(binary), "-h"], cwd=binary.parent, text=True
    )
    if "--version" not in help_text or "--no-browser" not in help_text:
        raise SystemExit("Loka's command-line help is incomplete.")

    destination = root / "dist" / "release" / version
    destination.mkdir(parents=True, exist_ok=True)
    extension = "zip" if windows else "tar.gz"
    archive = destination / f"loka_{version}_{system}_{arch}.{extension}"
    checksums = destination / "SHA256SUMS"
    with TemporaryDirectory(dir=destination) as scratch:
        staging = Path(scratch)
        candidate = staging / archive.name
        write_archive(candidate, binary, root / "README.md")
        candidate.replace(archive)

        archives = sorted(
            (*destination.glob("loka_*.tar.gz"), *destination.glob("loka_*.zip"))
        )
        staged_checksums = staging / checksums.name
        with staged_checksums.open("w", encoding="ascii", newline="\n") as output:
            for path in archives:
                digest = hashlib.sha256(path.read_bytes()).hexdigest()
                output.write(f"{digest}  {path.name}\n")
        staged_checksums.replace(checksums)
    print(archive.relative_to(root))
    print(checksums.relative_to(root))


if __name__ == "__main__":
    main()
