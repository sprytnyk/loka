# Loka

Share files and directories with computers on the same local network. Loka is
a single program with nothing else to install.

![Loka showing selected files, nearby devices, and transfer progress](docs/loka.png)

## Install

Download the archive for your computer from
[Releases](https://github.com/sprytnyk/loka/releases), unpack it, and put
`loka` (`loka.exe` on Windows) in a directory on your `PATH`.

| Computer | Archive |
| --- | --- |
| Mac with Apple silicon | `loka_X.Y.Z_darwin_arm64.tar.gz` |
| Mac with Intel | `loka_X.Y.Z_darwin_amd64.tar.gz` |
| Linux | `loka_X.Y.Z_linux_amd64.tar.gz` |
| Windows | `loka_X.Y.Z_windows_amd64.zip` |

macOS refuses to open downloaded programs from unidentified developers. After
unpacking, run `xattr -d com.apple.quarantine loka` once.

To check a download, put `SHA256SUMS` and `SHA256SUMS.asc` from the release
next to it and run:

```sh
gpg --verify SHA256SUMS.asc SHA256SUMS && sha256sum --ignore-missing -c SHA256SUMS
```

With Go installed, you can instead run
`go install github.com/sprytnyk/loka@latest`.

## Start

Run `loka` on each computer. It opens Loka in your browser. Choose files or a
directory, then select the receiving device. The receiver chooses an existing
destination directory and accepts the transfer. Loka remembers the destination
until it stops. Several transfers can arrive at the same time.

**Choose files** and **Choose directory** open the desktop's file picker:
zenity or kdialog on Linux, Finder on macOS, and the usual dialog on Windows.
You can also drop files on the page or add them by path. Dropped files are
copied to a temporary directory first, because the browser does not tell Loka
where they are. Loka removes the copies when it stops.

If a device does not appear, enter its address and port. Some Wi-Fi networks
block connections between devices. On Windows, allow Loka through the firewall
when asked.

To share with someone who does not have Loka, choose the items, enter your
local address if needed, and select **Create link**. The link works until you
stop sharing or stop Loka. A single file downloads directly, and an
interrupted download can resume. Directories and multiple files download as a
ZIP archive. Each download has the current contents of what you shared. If a
shared file or directory is removed or replaced, the link stops working.

## Command line

```sh
loka receive
loka devices
loka send --to 192.0.2.10 /path/to/file /path/to/directory
loka share --address 192.0.2.10 /path/to/file
```

Run `loka --help` for port options and `loka --version` for the version. If
the port is already in use, run with `--port 0` to let Loka pick a free one. For
`share`, use this computer's local address in place of the example, or omit
`--address` to use a detected address. The link remains available until you
press Ctrl+C. When receiving, enter a destination directory or leave it empty
to decline.

Transfers are not encrypted. Anyone with a download link can use it while the
link is active. Use Loka only on a network you trust. Restart interrupted
transfers from the beginning. Loka does not transfer symbolic links, and leaves
them out of the directories you choose.

## Development

Run `make check` for gofmt, go vet and the tests, and `make format` to format
the code. `make build` builds `dist/loka` for this computer, and
`make release` builds the release archives for every system into `dist/`.
