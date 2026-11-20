package transfer

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A transfer between devices is a tar stream: one header per directory or
// file, in the order Collect listed them, then the file's bytes. The
// receiver unpacks it as it arrives, so neither side keeps a copy.

// WriteTar writes entries to w as a transfer stream, adding the bytes of
// file content it sends to done.
func WriteTar(ctx context.Context, w io.Writer, entries []Entry, done *atomic.Int64) error {
	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, ModTime: e.info.ModTime(), Typeflag: tar.TypeReg, Mode: 0o644, Size: e.size}
		if e.dir {
			hdr.Name += "/"
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !e.dir {
			if err := copyFile(ctx, tw, e, done); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// errReplaced means a file changed or was replaced after it was chosen.
var errReplaced = errors.New("changed or was replaced after it was chosen")

// errInUse means another program has a file open for writing, so it can't
// be read without the risk of an edit while it is sent.
var errInUse = errors.New("is open for writing in another program. Close it there and try again")

// copyFile sends one file, which must still be the file Collect saw,
// unchanged, at the size Collect saw. It checks again after copying, so an
// edit made while the file was read can't pass as a whole file.
func copyFile(ctx context.Context, w io.Writer, e Entry, done *atomic.Int64) error {
	f, info, err := openChosen(e.src, e.info)
	if err != nil {
		return err
	}
	defer f.Close()
	if !unchanged(e.info, info) {
		return fmt.Errorf("%s %w", e.src, errReplaced)
	}
	_, err = io.CopyN(w, progressReader{ctx, f, done}, e.size)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err != nil || moreToRead(f) {
		return fmt.Errorf("%s changed while it was being sent", e.src)
	}
	if info, err := f.Stat(); err != nil || !unchanged(e.info, info) {
		return fmt.Errorf("%s changed while it was being sent", e.src)
	}
	return nil
}

func moreToRead(r io.Reader) bool {
	n, _ := r.Read(make([]byte, 1))
	return n > 0
}

// progressReader stops when ctx is done and adds what it reads to done.
type progressReader struct {
	ctx  context.Context
	r    io.Reader
	done *atomic.Int64
}

func (p progressReader) Read(b []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := p.r.Read(b)
	p.done.Add(int64(n))
	return n, err
}

// Receive unpacks a transfer stream of size bytes of file content into
// destination. It unpacks into a hidden staging directory first, then moves
// each top-level item into destination under a name nothing else uses. So
// nothing that exists is overwritten, and a failed transfer leaves nothing
// behind. nameFor adapts each name to this system.
func Receive(ctx context.Context, r io.Reader, destination string, size int64, done *atomic.Int64, nameFor func(string) string) error {
	staging, err := os.MkdirTemp(destination, ".loka-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	u := unpacker{root: staging, localName: nameFor, staged: map[string]string{}}
	tr := tar.NewReader(r)
	var roots []string
	var received int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return transferError(ctx, err)
		}
		parts, err := SplitName(hdr.Name)
		if err != nil {
			return err
		}
		if _, seen := u.staged[parts[0]]; !seen {
			roots = append(roots, parts[0])
		}
		target, err := u.path(parts)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if received += hdr.Size; hdr.Size < 0 || received > size {
				return errors.New("The sender sent more than it offered")
			}
			if err := writeFile(ctx, target, tr, done); err != nil {
				return transferError(ctx, err)
			}
		default:
			return fmt.Errorf("%s: only files and directories can be received", hdr.Name)
		}
	}
	if received != size {
		return errors.New("The transfer ended early")
	}
	// A cancel that arrives with the last bytes still wins.
	if err := ctx.Err(); err != nil {
		return err
	}
	return publish(u.staged, roots, destination)
}

// publish moves each staged top-level item into destination. When one
// cannot move, it moves back the ones already moved, so a failed transfer
// still leaves nothing behind.
func publish(staged map[string]string, roots []string, destination string) error {
	var moved [][2]string
	for _, root := range roots {
		from := staged[root]
		to, err := moveNoReplace(from, destination, filepath.Base(from))
		if err != nil {
			for _, m := range slices.Backward(moved) {
				os.Rename(m[1], m[0])
			}
			return err
		}
		moved = append(moved, [2]string{from, to})
	}
	return nil
}

// StallTimeout is how long a transfer may move no bytes before it counts
// as interrupted.
var StallTimeout = time.Minute

// WatchStall calls stalled once if done stays the same for StallTimeout.
// The returned stop ends the watch.
func WatchStall(done *atomic.Int64, stalled func()) (stop func()) {
	timeout, quit := StallTimeout, make(chan struct{})
	go func() {
		ticker := time.NewTicker(timeout / 10)
		defer ticker.Stop()
		last, since := done.Load(), time.Now()
		for {
			select {
			case <-quit:
				return
			case now := <-ticker.C:
				if n := done.Load(); n != last {
					last, since = n, now
				} else if now.Sub(since) >= timeout {
					stalled()
					return
				}
			}
		}
	}()
	return sync.OnceFunc(func() { close(quit) })
}

// ErrInterrupted means the connection ended in the middle of a transfer.
var ErrInterrupted = errors.New("The connection was interrupted")

// transferError keeps a cancelled context as the reason, since reads after a
// cancel fail in ways that hide it. A stream cut short or a connection
// reset reads as interrupted, rather than as socket details.
func transferError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if Interrupted(err) {
		return ErrInterrupted
	}
	return err
}

// Interrupted reports whether err comes from a connection that broke
// after it was made: cut short, reset, or closed.
func Interrupted(err error) bool {
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op != "dial" {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed)
}

func writeFile(ctx context.Context, target string, r io.Reader, done *atomic.Int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, progressReader{ctx, r, done}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// SplitName checks a path from a transfer stream and splits it into names.
// It refuses anything that could reach outside the destination.
func SplitName(name string) ([]string, error) {
	trimmed := strings.TrimSuffix(name, "/")
	parts := strings.Split(trimmed, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return nil, fmt.Errorf("Invalid path in transfer: %q", name)
		}
	}
	return parts, nil
}

// unpacker maps each path in a stream to a path inside root. Each directory
// or file gets its local name the first time it appears, numbered when that
// name is taken, and keeps it for the rest of the stream.
type unpacker struct {
	root      string
	localName func(string) string
	staged    map[string]string // stream path -> path inside root
}

func (u unpacker) path(parts []string) (string, error) {
	parent := u.root
	for i := range parts {
		key := strings.Join(parts[:i+1], "/")
		staged, ok := u.staged[key]
		if !ok {
			var err error
			if staged, err = freePath(parent, u.localName(parts[i])); err != nil {
				return "", err
			}
			u.staged[key] = staged
		}
		parent = staged
	}
	return parent, nil
}

// freePath returns dir/name, numbered when something already has that name.
// A broken symbolic link counts as something. A name the file system
// refuses, such as one that is too long, is an error.
func freePath(dir, name string) (string, error) {
	for i := 0; ; i++ {
		path := filepath.Join(dir, numbered(name, i))
		_, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
	}
}

// moveNoReplace moves from into dir as name, or as a numbered name when
// something already has that name, and returns where it went.
func moveNoReplace(from, dir, name string) (string, error) {
	for i := 0; ; i++ {
		to := filepath.Join(dir, numbered(name, i))
		err := renameNoReplace(from, to)
		if !errors.Is(err, fs.ErrExist) {
			return to, err
		}
	}
}

// renameIfFree is the fallback for file systems that cannot refuse to
// replace in one step. Another process could slip in between the check and
// the rename.
func renameIfFree(from, to string) error {
	if _, err := os.Lstat(to); !errors.Is(err, fs.ErrNotExist) {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: fs.ErrExist}
	}
	return os.Rename(from, to)
}

// LocalName adapts a received name to this system.
func LocalName(name string) string {
	if runtime.GOOS == "windows" {
		return windowsName(name)
	}
	return name
}
