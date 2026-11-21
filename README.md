# Loka

Share files and directories with computers and phones on your local network.
Files go straight from one device to the other, encrypted, and never pass
through a server or the internet.

Loka is one program with nothing else to install, used in the browser or the
terminal. It finds other devices by itself, and phones need only a browser. It
needs no account and sends nothing outside your network.

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

> [!NOTE]
> macOS refuses to open downloaded programs from unidentified developers. After
> unpacking, run `xattr -d com.apple.quarantine loka` once.

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
3. On the receiver, choose an existing directory and accept within a minute.
   Both computers show a pairing code. The first time, check it's the same
   on both.

Several transfers can arrive at once, and each shows its speed. Loka remembers
the last destination until it stops.

The page follows your system's light or dark setting and your browser's
language, English or Ukrainian. Buttons in the header switch both, and the
browser remembers your choice.

The pickers use zenity or kdialog on Linux, Finder on macOS, and the
Windows file dialog. Without one, drop files or enter paths instead.

Loka copies dropped files first, because the browser doesn't tell it where they
are, and deletes the copies when it stops. It reads chosen files and paths
where they are.

Incoming files go to a hidden directory inside the destination and move into
place when the transfer finishes. Nothing is overwritten, and a failed transfer
leaves nothing behind.

> [!TIP]
> If the computers can't see each other, the Wi-Fi network may block
> connections between devices. On Windows, allow Loka through the firewall when
> asked.

## From a phone

A phone doesn't need Loka. On the computer, select **Receive from a phone** and
scan the QR code with the phone's camera, or open the address in the phone's
browser. Choose files or a directory and send. The computer asks you to accept,
as with any transfer. iPhones and iPads can send files but not directories,
because their browsers can't choose a directory.

The first time, the phone offers to install Loka's certificate, with the steps
for Android and iPhone. Do it once, and from then on the page opens over HTTPS
without a warning. You can also continue without it: the connection is still
encrypted, but the browser warns that it can't check it's really your computer.

A computer with several networks shows an address and QR code for each,
including a Tailscale address. Use the one on the same network as the phone.
Other VPN addresses only show up when there is no local network.

## Download links

For someone without Loka, add what to share and select **Create link**. The
link works in any browser while you're sharing. Its **QR code** button shows a
code a phone can open by camera, one per address when the computer is on
several networks.

A link looks like `http://192.0.2.10:45873/#d=…`. Browsers never send the part
after `#`, so the page moves to HTTPS before the link's secret leaves the
device, the same way as for phones above. Each link also has an `https://`
form, and the QR code window has a **The phone has Loka's certificate** box
that switches the codes to it.

> [!WARNING]
> An `http://` link opens a small page over plain HTTP first. Someone on the
> network who can change traffic could swap that page and steal the link. Once
> a phone has Loka's certificate, use the `https://` link instead. The same
> goes for the address in **Receive from a phone**.

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

`send` takes the receiver's address. `receive` asks where to put each transfer;
leave the answer empty to decline. It also prints the address and QR code a
phone can send from, its `https://` form and the certificate's fingerprint.
`share` prints a link and its QR code for each local address, the same links
over `https://`, the certificate's fingerprint and a `curl` command. The link
works until you press Ctrl+C. `--address` limits it to one address. `devices`
lists nearby devices and marks a device as remembered only when it shows the
certificate Loka knows for it. `forget` forgets a device.

curl doesn't know Loka's certificate, so the command turns that check off with
`-k`. Instead, `--pinnedpubkey` makes curl check the key Loka uses in this run.
If another device answers, curl stops before it sends the link.

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
it. The **?** button on the page explains the rest. [PROTOCOL.md](PROTOCOL.md)
describes how devices talk, step by step.

Each Loka makes a certificate on its first run and keeps it in your settings
directory: `~/.config/sprytnyk.dev/Loka` on Linux,
`~/Library/Application Support/sprytnyk.dev/Loka` on macOS and
`%AppData%\sprytnyk.dev\Loka` on Windows. Loka trusts the first transfer with
another device as it is, and marks that device as new on both sides.

Both devices show a six-digit pairing code, made from both certificates and a
random number from each side.

> [!IMPORTANT]
> The first time two devices talk, check that both show the same pairing code.
> If another device sits between them and passes the transfer on, the codes
> differ.

After the first transfer, Loka remembers the other device's certificate. If a
remembered device shows a different one, another device is probably posing as
it, and Loka refuses the transfer. A reinstalled Loka shows up as a new device
instead, so there is nothing to forget.

Browsers can't remember a certificate that way, so each Loka also makes its own
certificate authority, kept in the same directory. Installed on a phone, it lets
the browser check it's talking to your computer. It can only vouch for
addresses on local networks, so even if someone copied it from your computer,
they couldn't use it to pose as a web site. Loka never installs it on the
computer itself.

Two steps still go over plain HTTP: the small page an `http://` address opens
first, which moves the phone to HTTPS, and the download of the certificate.
Someone on the network could change either. Once the phone has the
certificate, open `https://` addresses and links, which skip the plain page.

> [!IMPORTANT]
> Check the certificate before you trust it. Compare the SHA-256 fingerprint in
> the phone's certificate details with the one on the computer: in the **?**
> help on Loka's page, or printed by `loka receive` and `loka share`. Don't
> trust a fingerprint shown on the phone, since someone who can swap the
> certificate can swap that page too.

> [!CAUTION]
> Anyone with a link can download while you're sharing. When you're done,
> select **Stop sharing**, or press Ctrl+C after `loka share`.

An interrupted transfer starts over. If no bytes move for a minute, for
example because a laptop went to sleep, the transfer stops. Loka skips
symbolic links inside the directories you choose.

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
the code. `make test-large` also sends a file over 8 GiB, which needs that
much free disk space. `make build` builds `dist/loka` for this computer.
`make release` builds the archives for every system into `dist/`, after
checking that the build for this computer runs.
