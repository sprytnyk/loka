# Loka

Share files and directories with computers on your local network. Loka is one
program with nothing else to install.

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

## Send and receive

Run `loka` on both computers. It opens in your browser.

1. Add what to send: choose files or a directory, drop them on the page, or
   enter a path.
2. Select the receiving device. If it doesn't show up, enter its address and
   port.
3. On the receiver, choose an existing directory and accept.

Several transfers can arrive at once, and each shows its speed. Loka remembers
the last destination until it stops.

The pickers use zenity or kdialog on Linux, Finder on macOS, and the usual
dialog on Windows. Without one, drop files or enter paths instead.

Dropped files are copied first, because the browser doesn't tell Loka where
they are. Loka deletes the copies when it stops. Chosen files and paths are
read where they are.

Incoming files go to a hidden directory inside the destination and move into
place when the transfer finishes. Nothing is overwritten, and a failed transfer
leaves nothing behind.

If the computers can't see each other, the Wi-Fi network may block connections
between devices. On Windows, allow Loka through the firewall when asked.

## Download links

For someone without Loka, add what to share and select **Create link**. Enter
your local address if Loka can't find it. The link works in any browser while
you're sharing.

A single file downloads as itself, and an interrupted download can resume.
Directories and several files download as a ZIP that Loka builds during the
download. Each download has the current contents. If a shared file or
directory is deleted or replaced, the link stops working.

## Command line

```sh
loka receive
loka devices
loka send --to 192.0.2.10 /path/to/file /path/to/directory
loka share --address 192.0.2.10 /path/to/file
```

`send` takes the receiver's address. `receive` asks where to put each
transfer; leave the answer empty to decline. `share` prints a link that works
until you press Ctrl+C. Without `--address`, it uses this computer's address.

Run `loka -h` for all options and `loka --version` for the version. If the port
is busy, add `--port 0` to use a free one. Help and errors are in colour in a
terminal. Set `NO_COLOR=1` to turn that off.

## Safety

Transfers aren't encrypted, so use Loka only on networks you trust. Anyone with
a link can download while you're sharing. An interrupted transfer between
devices starts over. Loka skips symbolic links inside the directories you
choose.

## Development

Run `make check` for gofmt, go vet and the tests, and `make format` to format
the code. `make build` builds `dist/loka` for this computer. `make release`
builds the archives for every system into `dist/`, after checking that the
build for this computer runs.
