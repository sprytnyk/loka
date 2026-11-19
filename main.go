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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sprytnyk/loka/internal/desktop"
	"github.com/sprytnyk/loka/internal/network"
	"github.com/sprytnyk/loka/internal/server"
	"github.com/sprytnyk/loka/internal/transfer"
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
	fs.StringVar(&o.address, "address", "", "local address to put in a share link (default: detected)")
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
	if network.AddressInUse(err) {
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
func startDevice(ctx context.Context, port int) (*server.Server, net.Listener, int, error) {
	ln, port, err := listenOn(port)
	if err != nil {
		return nil, nil, 0, err
	}
	name := hostname()
	s := server.New(name, version)
	s.Devices = network.New(network.Announcement{ID: transfer.RandomToken(16), Name: name, Port: port})
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
	return serveUntil(ctx, ln, s.Handler())
}

func sendCommand(ctx context.Context, o options, stdout io.Writer) error {
	entries, items, size, release, err := transfer.CollectHeld(o.paths)
	if err != nil {
		return err
	}
	t := transfer.New(transfer.Outgoing, o.to, items, size, transfer.Waiting)
	go func() {
		defer release()
		t.Finish(server.SendTo(t, entries, hostname(), o.to, o.peerPort))
	}()
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
	go func() { served <- serveUntil(ctx, ln, s.Handler()) }()
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
	for _, p := range peers {
		fmt.Fprintf(stdout, "%s  %s:%d\n", p.Name, p.Host, p.Port)
	}
	return nil
}

func shareCommand(ctx context.Context, o options, stdout io.Writer) error {
	address := o.address
	if address == "" {
		addresses := network.LocalAddresses()
		if len(addresses) == 0 {
			return errors.New("could not find a local address; give one with --address")
		}
		address = addresses[0]
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
	fmt.Fprintf(stdout, "http://%s/download/%s\n", net.JoinHostPort(address, strconv.Itoa(port)), link.Token)
	return serveUntil(ctx, ln, s.Handler())
}
