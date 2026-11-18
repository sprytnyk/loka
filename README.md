# Loka

Share files and directories with computers on the same local network. Loka
requires Python 3.10 or later and has no runtime dependencies.

## Start

Run `uv run python -m loka` on each computer. The command opens the browser
interface. Choose files or a directory, then select the receiving device. The
receiver accepts the transfer and enters an existing destination directory.

If a device does not appear, enter its address and port. Some Wi-Fi networks
prevent devices from communicating with each other.

To share with someone who does not have Loka, choose the items, enter your
local address if needed, and select **Create link**. The link works until you
stop sharing or stop Loka. A single file downloads directly. Directories and
multiple files download as a ZIP archive.

## Command line

```sh
uv run python -m loka receive
uv run python -m loka devices
uv run python -m loka send --to 192.0.2.10 /path/to/file /path/to/directory
uv run python -m loka share --address 192.0.2.10 /path/to/file
```

Run `python -m loka --help` for port options. For `share`, use this computer's
local address in place of the example, or omit `--address` to use a detected
address. The link remains available until you press Ctrl+C. When receiving,
enter a destination directory or leave it empty to decline.

Transfers are not encrypted. Anyone with a download link can use it while the
link is active. Use Loka only on a network you trust. Restart interrupted
transfers from the beginning. Loka does not transfer symbolic links.

## Development

Run `make check` for Ruff and pytest, or `make format` to format the Python code.
