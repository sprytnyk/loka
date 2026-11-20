// Command loka shares files and directories with computers on the same local
// network.
package main

import (
	"bufio"
	"cmp"
	"context"
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
		return fail("forget needs the name of one device")
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

// configDir holds this device's identity and the devices it remembers.
var configDir = func() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("could not find the settings directory: %w", err)
	}
	return filepath.Join(dir, "loka"), nil
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
	t := transfer.New(transfer.Outgoing, o.to, items, size, transfer.Waiting)
	go func() { t.Finish(server.SendTo(t, entries, store, hostname(), o.to, o.peerPort)) }()
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
	for {
		v := t.View()
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

	answers := make(chan string)
	go func() {
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			answers <- scanner.Text()
		}
		close(answers)
	}()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	told := map[string]bool{} // refusals already printed
	for {
		select {
		case <-ctx.Done():
			return <-served
		case err := <-served:
			return err
		case <-ticker.C:
		}
		for _, t := range s.Transfers.All() {
			if v := t.View(); v.ForgetID != "" && !told[t.ID] {
				told[t.ID] = true
				fmt.Fprintf(stdout, "\nRefused a transfer: %s\nTo trust it, run: loka forget %q\n", v.Error, t.Peer)
			}
		}
		t := firstWaiting(s)
		if t == nil {
			continue
		}
		fmt.Fprintf(stdout, "\n%s wants to send: %s\n", sender(t), strings.Join(t.Items, ", "))
		if !answerOffer(ctx, s, t, answers, stdout) {
			return <-served
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
			fmt.Fprintln(stdout, "The sender cancelled.")
			return true
		}
		stop := context.AfterFunc(ctx, t.Stop)
		defer stop()
		follow(t, stdout)
		return ctx.Err() == nil
	}
}

// sender names who offers t, and warns when it isn't encrypted or comes
// from a device Loka hasn't seen before.
func sender(t *transfer.Transfer) string {
	switch {
	case t.Browser:
		return "A browser at " + t.Peer + " (not encrypted)"
	case t.NewDevice:
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
	remembered := map[string]bool{}
	if store, err := openTrust(); err == nil {
		devices, _ := store.Devices()
		for _, d := range devices {
			remembered[d.ID] = true
		}
	}
	for _, p := range peers {
		line := fmt.Sprintf("%s  %s:%d", p.Name, p.Host, p.Port)
		if remembered[p.ID] {
			line += "  (remembered)"
		}
		fmt.Fprintln(stdout, line)
	}
	return nil
}

// forgetCommand forgets every remembered device with this name, so a new
// certificate of theirs is trusted on the next transfer.
func forgetCommand(name string, stdout io.Writer) error {
	store, err := openTrust()
	if err != nil {
		return err
	}
	forgotten, err := store.Forget(func(d trust.Device) bool { return d.Name == name })
	if err != nil {
		return err
	}
	switch len(forgotten) {
	case 0:
		return fmt.Errorf("no remembered device is named %q", name)
	case 1:
		fmt.Fprintf(stdout, "Forgot %s.\n", name)
	default:
		fmt.Fprintf(stdout, "Forgot %d devices named %s.\n", len(forgotten), name)
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
	ln, port, err := listenOn(o.port)
	if err != nil {
		return err
	}
	s := server.New(hostname(), version)
	s.Transfers.Add(link)
	printLinks(stdout, addresses, port, "/download/"+link.Token)
	return s.Serve(ctx, ln)
}

// printLinks prints path at each address, each under its QR code when
// stdout is a terminal, so a phone can open it by camera.
func printLinks(stdout io.Writer, addresses []string, port int, path string) {
	f, ok := stdout.(*os.File)
	codes := ok && isTerminal(f)
	for _, address := range addresses {
		link := "http://" + net.JoinHostPort(address, strconv.Itoa(port)) + path
		if code, err := qr.Encode(link); codes && err == nil {
			fmt.Fprint(stdout, code.Text())
		}
		fmt.Fprintln(stdout, link)
	}
}
