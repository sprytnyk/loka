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
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sprytnyk/loka/internal/desktop"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: loka [command] [options] [paths...]

Share files and directories with computers on the same local network.

commands:
  serve     open the page in your browser (the default)
  send      send paths to another device: loka send --to ADDRESS PATH...
  receive   wait for transfers and answer them in the terminal
  devices   list devices running Loka nearby
  share     print a download link for paths: loka share PATH...

options:
`

var commands = []string{"serve", "send", "receive", "devices", "share"}

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
	o, code := parse(args, stderr)
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
	}
	if err != nil {
		fmt.Fprintln(stderr, "loka:", err)
		return 1
	}
	return 0
}

// parse reads the command line. Options may come before or after paths.
// It returns a code of 0 or more when Loka should exit with it; an empty
// command means print the version.
func parse(args []string, stderr io.Writer) (options, int) {
	var o options
	fs := flag.NewFlagSet("loka", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.to, "to", "", "receiver address for send")
	fs.StringVar(&o.address, "address", "", "local address to put in a share link (default: detected)")
	fs.IntVar(&o.port, "port", defaultPort, "port to listen on, 0 for any free port")
	fs.IntVar(&o.peerPort, "peer-port", defaultPort, "receiver port for send")
	fs.BoolVar(&o.noBrowser, "no-browser", false, "do not open the browser")
	showVersion := fs.Bool("version", false, "print the version and exit")

	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return o, 0
			}
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
	named := len(positional) > 0 && slices.Contains(commands, positional[0])
	if named {
		o.command, positional = positional[0], positional[1:]
	}
	o.paths = positional
	fail := func(format string, a ...any) (options, int) {
		fmt.Fprintf(stderr, "loka: "+format+"\n", a...)
		fs.Usage()
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
	case !named && len(o.paths) > 0:
		return fail("unknown command %q", o.paths[0])
	case o.command != "send" && o.command != "share" && len(o.paths) > 0:
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
	if addressInUse(err) {
		return nil, 0, fmt.Errorf("port %d is already in use. Stop the other program, or run with --port 0 to pick a free port", port)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("could not listen on port %d: %w", port, err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// serveUntil serves handler on ln until ctx is done.
func serveUntil(ctx context.Context, ln net.Listener, handler http.Handler) error {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
		srv.Close()
	})
	defer stop()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startDevice runs the server with discovery, as serve and receive do.
func startDevice(ctx context.Context, port int) (*server, net.Listener, int, error) {
	ln, port, err := listenOn(port)
	if err != nil {
		return nil, nil, 0, err
	}
	s := newServer(hostname(), version)
	s.devices = newDiscovery(announcement{ID: randomToken(16), Name: s.name, Port: port})
	go s.devices.listen(ctx)
	go s.devices.announce(ctx)
	return s, ln, port, nil
}

func serve(ctx context.Context, o options, stdout io.Writer) error {
	s, ln, port, err := startDevice(ctx, o.port)
	if err != nil {
		return err
	}
	defer s.close()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	fmt.Fprintf(stdout, "Loka %s is running at %s (Ctrl+C to stop)\n", version, url)
	if !o.noBrowser {
		if err := desktop.OpenBrowser(url); err != nil {
			fmt.Fprintf(stdout, "Open %s in your browser.\n", url)
		}
	}
	return serveUntil(ctx, ln, s.handler())
}

func sendCommand(ctx context.Context, o options, stdout io.Writer) error {
	entries, items, size, err := collect(o.paths)
	if err != nil {
		return err
	}
	t := newTransfer(outgoing, o.to, items, size, statusWaiting)
	go func() { t.finish(sendTo(t, entries, hostname(), o.to, o.peerPort)) }()
	stop := context.AfterFunc(ctx, t.stop)
	defer stop()
	v := follow(t, stdout)
	if v.Status != statusComplete {
		return errors.New(cmp.Or(v.Error, string(v.Status)))
	}
	return nil
}

// follow prints a transfer's progress until it ends.
func follow(t *Transfer, stdout io.Writer) transferView {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		v := t.view()
		label := fmt.Sprintf("%s: %d%%", v.Status, v.Progress)
		if v.SpeedMbps != nil {
			label += fmt.Sprintf(" at %.1f Mbps", *v.SpeedMbps)
		}
		fmt.Fprintf(stdout, "\r%-40s", label) // pads over a longer earlier line
		if v.Status.final() {
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
	defer s.close()
	served := make(chan error, 1)
	go func() { served <- serveUntil(ctx, ln, s.handler()) }()
	fmt.Fprintf(stdout, "Loka %s is waiting for transfers on port %d. Press Ctrl+C to stop.\n", version, port)

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
	for {
		select {
		case <-ctx.Done():
			return <-served
		case err := <-served:
			return err
		case <-ticker.C:
		}
		t := firstWaiting(s)
		if t == nil {
			continue
		}
		fmt.Fprintf(stdout, "\n%s wants to send: %s\n", t.Peer, strings.Join(t.Items, ", "))
		for {
			fmt.Fprint(stdout, "Destination directory (empty to decline): ")
			var answer string
			var ok bool
			select {
			case <-ctx.Done():
				t.stop()
				return <-served
			case answer, ok = <-answers:
			}
			if !ok || strings.TrimSpace(answer) == "" {
				t.stop()
				fmt.Fprintln(stdout, "Declined.")
				break
			}
			destination, err := existingDir(strings.TrimSpace(answer))
			if err != nil {
				fmt.Fprintln(stdout, err)
				continue
			}
			if !t.accept(destination) {
				fmt.Fprintln(stdout, "The sender cancelled.")
				break
			}
			s.destination.Store(&destination)
			follow(t, stdout)
			break
		}
	}
}

func firstWaiting(s *server) *Transfer {
	for _, t := range s.transfers.all() {
		if t.Direction == incoming && t.Status() == statusWaiting {
			return t
		}
	}
	return nil
}

func devicesCommand(ctx context.Context, stdout io.Writer) error {
	d := newDiscovery(announcement{ID: randomToken(16)})
	ctx, cancel := context.WithTimeout(ctx, 2*announceEvery)
	defer cancel()
	d.listen(ctx)
	peers, problem := d.list()
	if problem != "" {
		return errors.New(problem)
	}
	if len(peers) == 0 {
		fmt.Fprintln(stdout, "No devices found.")
	}
	for _, p := range peers {
		fmt.Fprintf(stdout, "%s  %s:%d\n", p.Name, p.Host, p.Port)
	}
	return nil
}

func shareCommand(ctx context.Context, o options, stdout io.Writer) error {
	address := o.address
	if address == "" {
		addresses := localAddresses()
		if len(addresses) == 0 {
			return errors.New("could not find a local address; give one with --address")
		}
		address = addresses[0]
	}
	link, err := newLink(o.paths)
	if err != nil {
		return err
	}
	ln, port, err := listenOn(o.port)
	if err != nil {
		return err
	}
	s := newServer(hostname(), version)
	s.transfers.add(link)
	fmt.Fprintf(stdout, "http://%s/download/%s\n", net.JoinHostPort(address, strconv.Itoa(port)), link.token)
	return serveUntil(ctx, ln, s.handler())
}
