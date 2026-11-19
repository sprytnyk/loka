# Build with: make build
import os
import sys
from pathlib import Path
from tempfile import TemporaryDirectory

root = Path(SPECPATH)
sys.path.insert(0, str(root))

from loka.version import VERSION_FILE, build_version

with TemporaryDirectory(prefix="loka-build-") as scratch:
    version_file = Path(scratch) / VERSION_FILE
    version = build_version(root, os.environ.get("TAG", ""))
    version_file.write_text(version, encoding="utf-8")

    a = Analysis(
        [str(root / "loka/__main__.py")],
        pathex=[str(root)],
        datas=[(str(root / "loka/index.html"), "."), (str(version_file), ".")],
    )
    pyz = PYZ(a.pure)

    exe = EXE(
        pyz,
        a.scripts,
        a.binaries,
        a.datas,
        [],
        name="loka",
        upx=False,
        console=True,
    )
