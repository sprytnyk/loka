import re
import subprocess
import sys
from importlib.metadata import PackageNotFoundError, version
from pathlib import Path

VERSION_FILE = "loka-version.txt"


def build_version(root, tag=""):
    if tag:
        match = re.fullmatch(r"v([0-9]+\.[0-9]+\.[0-9]+)", tag)
        if not match:
            raise ValueError(f"Invalid release tag {tag!r}. Expected vX.Y.Z.")
        return match[1]

    description = "unknown"
    try:
        result = subprocess.run(
            ["git", "describe", "--tags", "--always", "--dirty"],
            cwd=root,
            capture_output=True,
            encoding="utf-8",
            errors="replace",
            check=True,
        )
    except (OSError, subprocess.CalledProcessError):
        pass
    else:
        description = (
            re.sub(r"[^A-Za-z0-9.]+", ".", result.stdout.strip()).strip(".")
            or description
        )
    return f"0.0.0.dev0+{description}"


def current_version():
    if getattr(sys, "frozen", False):
        return Path(sys._MEIPASS, VERSION_FILE).read_text(encoding="utf-8").strip()

    root = Path(__file__).resolve().parent.parent
    if (root / ".git").exists() and (root / "loka.spec").is_file():
        return build_version(root)
    try:
        return version("loka-share")
    except PackageNotFoundError:
        return "0.0.0.dev0+unknown"
