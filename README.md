# Loka

Share files and directories with computers and phones on your local network.
Loka is one program with nothing else to install. It runs in your browser,
finds other devices by itself, and needs no account, server or internet.

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

The page follows your system's light or dark setting and your browser's
language, English or Ukrainian. Buttons in the header switch both, and the
browser remembers your choice.

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

## From a phone

A phone doesn't need Loka. On the computer, select **Receive from a phone** and
scan the QR code with the phone's camera, or open the address in the phone's
browser. Choose files or a folder and send. The computer asks you to accept,
as with any transfer. iPhones and iPads can send files but not folders, because
their browsers can't choose a folder.

The first time, the phone offers to install Loka's certificate, with the steps
for Android and iPhone. Do it once, and from then on the page opens over HTTPS
without a warning. You can also continue without it: the connection is still
encrypted, but the browser warns that it can't check it's really your computer.

A computer with several networks shows an address and QR code for each. Use
the one on the same network as the phone.

## Download links

For someone without Loka, add what to share and select **Create link**. The
link works in any browser while you're sharing. Its **QR code** button shows a
code a phone can open by camera, one per address when the computer is on
several networks.

A link looks like `http://192.0.2.10:45873/#d=…`. Browsers never send the part
after `#`, so the page moves to HTTPS before the link's secret leaves the
device, the same way as for phones above.

A single file downloads as itself, and an interrupted download can resume.
Directories and several files download as a ZIP that Loka builds during the
download. Each download has the current contents. If a shared file or
directory is deleted or replaced, the link stops working.

## Command line

```sh
loka receive
loka devices
loka send --to 192.0.2.10 /path/to/file /path/to/directory
loka share /path/to/file
loka forget my-laptop
```

`send` takes the receiver's address. `receive` asks where to put each
transfer; leave the answer empty to decline. It also prints the address and QR
code a phone can send from. `share` prints a link and its QR code for each
local address, plus a `curl` command, and the link works until you press
Ctrl+C. `--address` limits it to one address. `devices` marks the devices Loka
remembers, and `forget` forgets one (see below).

The `curl` command uses `-k`, since curl doesn't know Loka's certificate. To
check it instead, download the certificate from the help on the page, convert
it with `openssl x509 -inform DER -in loka-ca.crt -out loka-ca.pem`, and give
curl `--cacert loka-ca.pem` in place of `-k`.

Run `loka -h` for all options and `loka --version` for the version. If the port
is busy, add `--port 0` to use a free one. Help and errors are in colour in a
terminal. Set `NO_COLOR=1` to turn that off.

## Safety

| Between | Encrypted |
| --- | --- |
| Two computers running Loka | Yes |
| A phone's browser and Loka | Yes |
| Download links | Yes |

The page on your own computer opens at `http://127.0.0.1`, which never leaves
it. The **?** button on the page explains the rest.

Each Loka makes a certificate on its first run and keeps it in your settings
folder (`~/.config/loka` on Linux, `~/Library/Application Support/loka` on
macOS, `%AppData%\loka` on Windows). The first transfer with another device is
trusted as it is, and the accept prompt marks that device as new. After that,
Loka remembers its certificate. If a remembered device shows a different one,
another device may be posing as it, and Loka refuses the transfer. To trust it
anyway, select **Forget this device** or run `loka forget NAME`. A reinstalled
Loka shows up as a new device instead.

Browsers can't remember a certificate that way, so each Loka also makes its own
certificate authority, kept in the same folder. Installed on a phone, it lets
the browser check it's talking to your computer. It can only vouch for
addresses on local networks, so even if someone copied it from your computer,
they couldn't use it to pose as a web site. Loka never installs it on the
computer itself.

Two steps still go over plain HTTP: the small page a phone opens first, which
moves it to HTTPS, and the download of the certificate. Someone on the network
could change either. To be sure, compare the certificate's fingerprint in the
phone's certificate details with the one in the page's help. Once it's
installed, you can open `https://` and the address directly. Anyone with a link
can download while you're sharing.

Loka 0.1 can't receive from this version, and this version refuses what Loka
0.1 sends, asking it to update. Update Loka on every device.

An interrupted transfer starts over. Loka skips symbolic links inside the
directories you choose.

## Compared with LocalSend and st

| | Loka | [LocalSend](https://localsend.org) | [st](https://github.com/chyok/st) |
| --- | --- | --- | --- |
| On computers | One program, used in the browser or the terminal | An app, or a separate command-line tool | One program, used in the terminal |
| On phones | Nothing, the browser | The app | Nothing, the browser |
| Between two installs | Encrypted | Encrypted by default | Not encrypted |
| From a phone's browser | You accept each upload | Not offered | Saved without asking |
| Download link for a browser | Yes | Yes | Yes |
| Finding devices | Multicast | Multicast, then scanning the network | Multicast |
| Account, server or internet | None | None | None |

## Planned

- Resume an interrupted transfer between devices instead of starting over.

## Development

Run `make check` for gofmt, go vet and the tests, and `make format` to format
the code. `make build` builds `dist/loka` for this computer. `make release`
builds the archives for every system into `dist/`, after checking that the
build for this computer runs.
