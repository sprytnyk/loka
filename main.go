// Command loka shares files and directories with computers and phones on the
// same local network.
package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sprytnyk/loka/internal/desktop"
	"github.com/sprytnyk/loka/internal/network"
	"github.com/sprytnyk/loka/internal/qr"
	"github.com/sprytnyk/loka/internal/server"
	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/trust"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// options are the parsed command line.
type options struct {
	command     string
	paths       []string
	to, address string
	port        int
	peerPort    int
	noBrowser   bool
}

// run runs Loka with the given arguments and returns the exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, code := parse(args, stdout, stderr)
	if code >= 0 {
		return code
	}
	if o.command == "" {
		fmt.Fprintln(stdout, "loka", version)
		return 0
	}
	var err error
	switch o.command {
	case "serve":
		err = serve(ctx, o, stdout)
	case "send":
		err = sendCommand(ctx, o, stdout)
	case "receive":
		err = receiveCommand(ctx, o, stdin, stdout)
	case "devices":
		err = devicesCommand(ctx, stdout)
	case "share":
		err = shareCommand(ctx, o, stdout)
	case "forget":
		err = forgetCommand(o.paths[0], stdout)
	}
	if err != nil {
		printError(stderr, err.Error())
		return 1
	}
	return 0
}

// parse reads the command line. Options may come before or after paths.
// Help goes to stdout and errors to stderr. It returns a code of 0 or more
// when Loka should exit with it; an empty command means print the version.
func parse(args []string, stdout, stderr io.Writer) (options, int) {
	var o options
	fs := flag.NewFlagSet("loka", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // help and errors are printed below
	fs.StringVar(&o.to, "to", "", "receiver address for send")
	fs.StringVar(&o.address, "address", "", "local address to put in a share link (default: every local address)")
	fs.IntVar(&o.port, "port", network.DefaultPort, "port to listen on, 0 for any free port")
	fs.IntVar(&o.peerPort, "peer-port", network.DefaultPort, "receiver port for send")
	fs.BoolVar(&o.noBrowser, "no-browser", false, "do not open the browser")
	showVersion := fs.Bool("version", false, "print the version and exit")

	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				printHelp(stdout, fs)
				return o, 0
			}
			printUsageError(stderr, err.Error())
			return o, 2
		}
		rest := fs.Args()
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			positional = append(positional, rest...)
			break
		}
		if len(rest) == 0 {
			break
		}
		positional, args = append(positional, rest[0]), rest[1:]
	}
	if *showVersion {
		return o, -1
	}

	o.command = "serve"
	named := len(positional) > 0 && isCommand(positional[0])
	if named {
		o.command, positional = positional[0], positional[1:]
	}
	o.paths = positional
	fail := func(format string, a ...any) (options, int) {
		printUsageError(stderr, fmt.Sprintf(format, a...))
		return o, 2
	}
	switch {
	case o.port < 0 || o.port > 65535:
		return fail("--port must be between 0 and 65535")
	case o.peerPort < 1 || o.peerPort > 65535:
		return fail("--peer-port must be between 1 and 65535")
	case o.command == "send" && (o.to == "" || len(o.paths) == 0):
		return fail("send needs --to ADDRESS and one or more paths")
	case o.command == "share" && len(o.paths) == 0:
		return fail("share needs one or more paths")
	case o.command == "forget" && len(o.paths) != 1:
		return fail("forget needs the name or ID of one device")
	case !named && len(o.paths) > 0:
		return fail("unknown command %q", o.paths[0])
	case o.command != "send" && o.command != "share" && o.command != "forget" && len(o.paths) > 0:
		return fail("%s takes no paths", o.command)
	}
	return o, -1
}

// hostname names this device for other devices.
func hostname() string {
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "Loka"
}

// listenOn opens the listening port, with a hint when it is taken. Port 0
// picks a free one.
func listenOn(port int) (net.Listener, int, error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if network.AddressInUse(err) {
		return nil, 0, fmt.Errorf("port %d is already in use. Stop the other program, or run with --port 0 to pick a free port", port)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("could not listen on port %d: %w", port, err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// configDir holds this device's identity and the devices it remembers,
// beside the settings of other sprytnyk.dev apps.
var configDir = func() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("could not find the settings directory: %w", err)
	}
	return filepath.Join(dir, "sprytnyk.dev", "Loka"), nil
}

// openTrust reads this device's identity, making one the first time.
func openTrust() (*trust.Store, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	return trust.Open(dir)
}

// startDevice runs the server with discovery, as serve and receive do.
func startDevice(ctx context.Context, port int) (*server.Server, net.Listener, int, error) {
	store, err := openTrust()
	if err != nil {
		return nil, nil, 0, err
	}
	ln, port, err := listenOn(port)
	if err != nil {
		return nil, nil, 0, err
	}
	name := hostname()
	s := server.New(name, version)
	s.Trust = store
	s.Devices = network.New(network.Announcement{ID: store.ID, Name: name, Port: port})
	go s.Devices.Listen(ctx)
	go s.Devices.Announce(ctx)
	return s, ln, port, nil
}

func serve(ctx context.Context, o options, stdout io.Writer) error {
	s, ln, port, err := startDevice(ctx, o.port)
	if err != nil {
		return err
	}
	defer s.Close()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	fmt.Fprintf(stdout, "Loka %s is running at %s (Ctrl+C to stop)\n", version, url)
	if !o.noBrowser {
		if err := desktop.OpenBrowser(url); err != nil {
			fmt.Fprintf(stdout, "Open %s in your browser.\n", url)
		}
	}
	return s.Serve(ctx, ln)
}

func sendCommand(ctx context.Context, o options, stdout io.Writer) error {
	entries, items, size, err := transfer.Collect(o.paths)
	if err != nil {
		return err
	}
	store, err := openTrust()
	if err != nil {
		return err
	}
	t := transfer.New(transfer.Outgoing, o.to, items, size, transfer.Pairing)
	go func() { t.Finish(server.SendTo(t, entries, store, hostname(), o.to, o.peerPort, "")) }()
	stop := context.AfterFunc(ctx, t.Stop)
	defer stop()
	v := follow(t, stdout)
	if v.Status != transfer.Complete {
		return errors.New(cmp.Or(v.Error, string(v.Status)))
	}
	return nil
}

// follow prints a transfer's progress until it ends.
func follow(t *transfer.Transfer, stdout io.Writer) transfer.View {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	warned, coded := false, false
	for {
		v := t.View()
		// An incoming offer already said both when it was shown.
		if t.Direction == transfer.Outgoing && v.NewDevice && !warned {
			warned = true
			fmt.Fprintf(stdout, "\r%s is a new device, not seen before. Make sure it's the one you meant.\n", v.Name)
		}
		if t.Direction == transfer.Outgoing && v.Code != "" && !coded {
			coded = true
			fmt.Fprintf(stdout, "\rPairing code %s. Check that the receiver shows the same.\n", v.Code)
		}
		label := fmt.Sprintf("%s: %d%%", v.Status, v.Progress)
		if v.SpeedMbps != nil {
			label += fmt.Sprintf(" at %.1f Mbps", *v.SpeedMbps)
		}
		fmt.Fprintf(stdout, "\r%-40s", label) // pads over a longer earlier line
		if v.Status.Final() {
			fmt.Fprintln(stdout)
			return v
		}
		<-ticker.C
	}
}

func receiveCommand(ctx context.Context, o options, stdin io.Reader, stdout io.Writer) error {
	s, ln, port, err := startDevice(ctx, o.port)
	if err != nil {
		return err
	}
	defer s.Close()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln) }()
	fmt.Fprintf(stdout, "Loka %s is waiting for transfers on port %d (Ctrl+C to stop)\n", version, port)
	if addresses := network.LocalAddresses(); len(addresses) > 0 {
		fmt.Fprintln(stdout, "To send from a phone, open this in its browser:")
		printLinks(stdout, s.Trust, addresses, port, "/")
	}

	answers := make(chan string)
	go func() {
		r := bufio.NewReader(stdin)
		for {
			line, err := readLine(r)
			if err != nil {
				close(answers)
				return
			}
			answers <- line
		}
	}()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	told := map[string]bool{} // refusals and warnings already printed
	for {
		select {
		case <-ctx.Done():
			return <-served
		case err := <-served:
			return err
		case <-ticker.C:
		}
		for _, t := range s.Transfers.All() {
			// Offers from Lokas that failed before the person saw them: a
			// refused device, or a pairing that didn't finish.
			if t.Direction == transfer.Incoming && t.DeviceID != "" && t.Destination() == "" && !told[t.ID] {
				if v := t.View(); v.Status == transfer.Failed {
					told[t.ID] = true
					fmt.Fprintf(stdout, "\nRefused a transfer from %s: %s\n", v.Name, v.Error)
				}
			}
		}
		if w := s.Warning(); w != "" && !told[w] {
			told[w] = true
			fmt.Fprintf(stdout, "\n%s\n", w)
		}
		t := firstWaiting(s)
		if t == nil {
			continue
		}
		told[t.ID] = true // answerOffer says how it ends
		skipStaleAnswer(answers)
		fmt.Fprintf(stdout, "\n%s wants to send: %s\n", sender(t), strings.Join(t.Items, ", "))
		if v := t.View(); v.Code != "" {
			fmt.Fprintf(stdout, "Pairing code %s. Check that the sender shows the same.\n", v.Code)
		}
		if !answerOffer(ctx, s, t, answers, stdout) {
			return <-served
		}
	}
}

// maxAnswer is how much of a line readLine keeps, well past the longest
// path a system allows.
const maxAnswer = 64 << 10

// readLine reads a line of any length, keeping at most maxAnswer bytes of
// it. A longer line is then an answer like any other, which fails as a
// directory, rather than the end of all answers.
func readLine(r *bufio.Reader) (string, error) {
	var line []byte
	for {
		part, more, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		line = append(line, part[:min(len(part), maxAnswer-len(line))]...)
		if !more {
			return string(line), nil
		}
	}
}

// answerOffer asks for a destination until one exists or the person
// declines, then follows the transfer. It returns false when ctx ends,
// which also stops the transfer.
func answerOffer(ctx context.Context, s *server.Server, t *transfer.Transfer, answers <-chan string, stdout io.Writer) bool {
	for {
		fmt.Fprint(stdout, "Destination directory (empty to decline): ")
		var answer string
		var ok bool
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.Context().Done():
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, offerEnded(t))
			return true
		case answer, ok = <-answers:
		}
		answer = strings.TrimSpace(answer)
		if !ok || answer == "" {
			t.Stop()
			fmt.Fprintln(stdout, "Declined.")
			return true
		}
		destination, err := server.ExistingDir(answer)
		if err != nil {
			fmt.Fprintln(stdout, err)
			continue
		}
		if !s.Accept(t, destination) {
			fmt.Fprintln(stdout, offerEnded(t))
			return true
		}
		stop := context.AfterFunc(ctx, t.Stop)
		defer stop()
		follow(t, stdout)
		return ctx.Err() == nil
	}
}

// skipStaleAnswer drops a line typed while no offer was asked about, such
// as just after one ended, so it can't answer the next offer before the
// person sees it. A closed channel stays closed.
func skipStaleAnswer(answers <-chan string) {
	select {
	case <-answers:
	default:
	}
}

// offerEnded says why an offer ended before it was accepted.
func offerEnded(t *transfer.Transfer) string {
	if err := t.View().Error; err != "" {
		return err + "."
	}
	return "The sender cancelled."
}

// sender names who offers t, and warns when it comes from a device Loka
// hasn't seen before.
func sender(t *transfer.Transfer) string {
	switch {
	case t.Browser:
		return "A browser at " + t.Peer
	case t.NewDevice():
		return t.Peer + " (new device, not seen before)"
	}
	return t.Peer
}

func firstWaiting(s *server.Server) *transfer.Transfer {
	for _, t := range s.Transfers.All() {
		if t.Direction == transfer.Incoming && t.Status() == transfer.Waiting {
			return t
		}
	}
	return nil
}

func devicesCommand(ctx context.Context, stdout io.Writer) error {
	d := network.New(network.Announcement{ID: transfer.RandomToken(16)})
	ctx, cancel := context.WithTimeout(ctx, 2*network.AnnounceEvery)
	defer cancel()
	d.Listen(ctx)
	peers, problem := d.List()
	if problem != "" {
		return errors.New(problem)
	}
	if len(peers) == 0 {
		fmt.Fprintln(stdout, "No devices found.")
	}
	labels := make([]string, len(peers))
	if store, err := openTrust(); err == nil {
		remembered := map[string]bool{}
		devices, _ := store.Devices()
		for _, d := range devices {
			remembered[d.ID] = true
		}
		var wg sync.WaitGroup
		for i, p := range peers {
			if remembered[p.ID] {
				wg.Go(func() { labels[i] = rememberedLabel(store, p) })
			}
		}
		wg.Wait()
	}
	for i, p := range peers {
		fmt.Fprintf(stdout, "%s  %s:%d%s\n", p.Name, p.Host, p.Port, labels[i])
	}
	return nil
}

// rememberedLabel connects to a device that announces a remembered ID, as
// anyone can, and labels it by the certificate it shows.
func rememberedLabel(store *trust.Store, p network.Peer) string {
	var standing trust.Standing
	var shown string
	config := store.ClientConfig(func(id, fingerprint string) error {
		shown = id
		var err error
		standing, _, err = store.Check(id, fingerprint)
		return err
	})
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 2 * time.Second}, Config: config}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
	if err != nil {
		return "  (remembered ID, could not check it)"
	}
	conn.Close()
	if shown == p.ID && standing == trust.Known {
		return "  (remembered)"
	}
	return "  (not the remembered device: another device may be posing as it)"
}

// forgetCommand forgets the remembered device with this ID, or every one
// with this name, so a new certificate of theirs is trusted on the next
// transfer.
func forgetCommand(nameOrID string, stdout io.Writer) error {
	store, err := openTrust()
	if err != nil {
		return err
	}
	forgotten, err := store.Forget(func(d trust.Device) bool { return d.ID == nameOrID || d.Name == nameOrID })
	if err != nil {
		return err
	}
	switch len(forgotten) {
	case 0:
		return fmt.Errorf("no remembered device has the name or ID %q", nameOrID)
	case 1:
		fmt.Fprintf(stdout, "Forgot %s.\n", forgotten[0].Name)
	default:
		fmt.Fprintf(stdout, "Forgot %d devices named %s.\n", len(forgotten), nameOrID)
	}
	return nil
}

func shareCommand(ctx context.Context, o options, stdout io.Writer) error {
	addresses := network.LocalAddresses()
	if o.address != "" {
		addresses = []string{o.address}
	}
	if len(addresses) == 0 {
		return errors.New("could not find a local address; give one with --address")
	}
	link, err := transfer.NewLink(o.paths)
	if err != nil {
		return err
	}
	store, err := openTrust()
	if err != nil {
		return err
	}
	ln, port, err := listenOn(o.port)
	if err != nil {
		return err
	}
	s := server.New(hostname(), version)
	s.Trust = store
	s.OnlyLinks = true
	s.Transfers.Add(link)
	fmt.Fprintln(stdout, "Open this on a phone or in a browser:")
	printLinks(stdout, store, addresses, port, "/#d="+link.Token)
	fmt.Fprintln(stdout, "With curl:")
	for _, address := range addresses {
		fmt.Fprintf(stdout, "curl -kOJ %s\n", hostURL("https", address, port, "/download/"+link.Token))
	}
	return s.Serve(ctx, ln)
}

// hostURL is the URL of path on this device at address.
func hostURL(scheme, address string, port int, path string) string {
	return scheme + "://" + net.JoinHostPort(address, strconv.Itoa(port)) + path
}

// printLinks prints path at each address, each under its QR code when
// stdout is a terminal, so a phone can open it by camera. The links are
// plain HTTP: the page there moves the browser to HTTPS, or offers to
// install Loka's certificate first. It warns about the addresses where
// installing the certificate won't help.
func printLinks(stdout io.Writer, store *trust.Store, addresses []string, port int, path string) {
	f, ok := stdout.(*os.File)
	codes := ok && isTerminal(f)
	for _, address := range addresses {
		link := hostURL("http", address, port, path)
		if code, err := qr.Encode(link); codes && err == nil {
			fmt.Fprint(stdout, code.Text())
		}
		fmt.Fprintln(stdout, link)
	}
	for _, problem := range store.AddressProblems(addresses) {
		fmt.Fprintln(stdout, problem)
	}
}
