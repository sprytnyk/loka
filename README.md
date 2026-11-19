# Loka

Share files and directories over your local network. To run from source, use
Python 3.10 or later.

![Loka showing selected files, nearby devices, and transfer progress](docs/loka.png)

## Start

Run `loka` on both computers, or `uv run python -m loka` from a checkout. Loka
opens in your browser.

Drop files or a directory onto the page, then choose the receiving device. If
it does not appear, enter its address and port. Some Wi-Fi networks block
connections between devices. On the receiver, choose an existing destination
directory and accept the transfer. Loka remembers your destination until it
stops.

When you send or share dragged files, Loka keeps a temporary copy until it
stops. **Choose files**, **Choose directory**, and **Add by path** let Loka read
the originals.
The desktop picker uses Zenity or KDialog on Linux, AppleScript on macOS, and
Windows PowerShell on Windows. You can type the destination directory when
receiving a transfer.

Loka stages incoming files in the destination directory and moves them into
place when the transfer finishes. Update Loka on both devices to use this
transfer format.

## Download links

Choose what to share and select **Create link**. Enter your local address if
Loka cannot find it. The recipient can open the link in a browser while you're
sharing.

Loka sends a single file as-is. It builds a ZIP as you download a directory or
multiple files. Each download reads the current contents. If a selected file
or directory goes missing or gets replaced, the link stops working.

## Command line

```sh
uv run python -m loka receive
uv run python -m loka devices
uv run python -m loka send --to 192.0.2.10 /path/to/file /path/to/directory
uv run python -m loka share --address 192.0.2.10 /path/to/file
```

Use your receiver's address with `send`. For `share`, use your own address or
omit `--address` to let Loka choose it. Press Ctrl+C to stop sharing. Leave the
destination empty to decline an incoming transfer.

Run `loka -h` for all options. If the listening port is busy, use `--port 0` to
pick a free port.

Use Loka on networks you trust. Transfers are unencrypted, and anyone with the
link can download while you're sharing. Restart interrupted transfers from the
beginning. Choose the original files and directories in place of symbolic
links.

## Development

Run `make check` for Ruff and pytest, or `make format` to format the Python code.

Run `make build` on the operating system you want to build for. The executable
is `dist/loka` on macOS and Linux, or `dist/loka.exe` on Windows. Add it to your
`PATH` to run `loka` from any directory. `loka --version` shows the same version
as the startup line and browser page.

Without Make, run
`uv run --locked --group build pyinstaller loka.spec --clean --noconfirm`.

Run `make release TAG=vX.Y.Z` to build an archive and `SHA256SUMS` in
`dist/release/X.Y.Z/`. Use `TAG` for the release version. Leave it out to build a
development version from `git describe`. The release script checks `--version`
and `-h` before it creates the archive.

Without Make, set `TAG` when you run PyInstaller, then run
`uv run --no-sync python scripts/package_release.py`.
