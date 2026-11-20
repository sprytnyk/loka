package transfer

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// bigSize is past 8 GiB, the most a plain tar header can hold, and past
// 4 GiB, the most a ZIP can hold without its 64-bit records.
const bigSize = 8<<30 + 3

// bigFile makes a file of bigSize that takes no space on disk: zeros, then
// "end".
func bigFile(t *testing.T) string {
	t.Helper()
	if os.Getenv("LOKA_LARGE_TESTS") == "" {
		t.Skip("moves more than 8 GiB and writes it to disk: run make test-large")
	}
	path := filepath.Join(t.TempDir(), "big")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("end"), bigSize-3); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSendingAFileOver8GiB sends a file too big for a plain tar header and
// checks it arrives whole.
func TestSendingAFileOver8GiB(t *testing.T) {
	entries, _, size, err := Collect([]string{bigFile(t)})
	if err != nil {
		t.Fatal(err)
	}
	if size != bigSize {
		t.Fatalf("collected %d bytes", size)
	}
	r, w := io.Pipe()
	var sent, received atomic.Int64
	go func() { w.CloseWithError(WriteTar(context.Background(), w, entries, &sent)) }()
	destination := t.TempDir()
	if err := Receive(context.Background(), r, destination, size, &received, identity); err != nil {
		t.Fatal(err)
	}
	if sent.Load() != bigSize || received.Load() != bigSize {
		t.Fatalf("counted %d sent and %d received", sent.Load(), received.Load())
	}
	f, err := os.Open(filepath.Join(destination, "big"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	end := make([]byte, 3)
	if info, err := f.Stat(); err != nil || info.Size() != bigSize {
		t.Fatalf("received %v, %v", info.Size(), err)
	}
	if _, err := f.ReadAt(end, bigSize-3); err != nil || string(end) != "end" {
		t.Fatalf("the file ends with %q, %v", end, err)
	}
}

// TestZipOfAFileOver8GiB builds the download of a file too big for a ZIP
// without 64-bit records, and checks a ZIP reader opens it whole.
func TestZipOfAFileOver8GiB(t *testing.T) {
	path := bigFile(t)
	entries, _, _, err := Collect([]string{filepath.Dir(path)})
	if err != nil {
		t.Fatal(err)
	}
	var out zeroRuns
	if err := WriteZip(context.Background(), &out, entries); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(&out, out.size)
	if err != nil {
		t.Fatal(err)
	}
	var big *zip.File
	for _, f := range zr.File {
		if filepath.Base(f.Name) == "big" {
			big = f
		}
	}
	if big == nil || big.UncompressedSize64 != bigSize {
		t.Fatalf("the ZIP holds %+v", zr.File)
	}
	rc, err := big.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var tail lastBytes
	// Reading to the end also checks the CRC.
	if n, err := io.Copy(&tail, rc); err != nil || n != bigSize || string(tail.b) != "end" {
		t.Fatalf("read %d bytes ending %q, %v", n, tail.b, err)
	}
}

// zeroRuns keeps what is written to it, but writes of only zeros as
// their count, so an archive of a big empty file fits in memory.
type zeroRuns struct {
	runs []run
	size int64
}

type run struct {
	at    int64
	zeros int64  // a run of zeros, or
	data  []byte // these bytes
}

func (z *zeroRuns) Write(b []byte) (int, error) {
	if n := int64(len(b)); allZeros(b) {
		if last := len(z.runs) - 1; last >= 0 && z.runs[last].data == nil {
			z.runs[last].zeros += n
		} else {
			z.runs = append(z.runs, run{at: z.size, zeros: n})
		}
	} else {
		z.runs = append(z.runs, run{at: z.size, data: bytes.Clone(b)})
	}
	z.size += int64(len(b))
	return len(b), nil
}

var zeros [64 << 10]byte

func allZeros(b []byte) bool {
	for len(b) > 0 {
		n := min(len(b), len(zeros))
		if !bytes.Equal(b[:n], zeros[:n]) {
			return false
		}
		b = b[n:]
	}
	return true
}

func (z *zeroRuns) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for _, r := range z.runs {
		length := r.zeros + int64(len(r.data))
		if n == len(p) || off+int64(n) >= r.at+length {
			continue
		}
		from := off + int64(n) - r.at
		want := min(int64(len(p)-n), length-from)
		if r.data != nil {
			copy(p[n:], r.data[from:from+want])
		} else {
			clear(p[n : n+int(want)])
		}
		n += int(want)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// lastBytes keeps the last three bytes written to it.
type lastBytes struct{ b []byte }

func (l *lastBytes) Write(p []byte) (int, error) {
	l.b = append(l.b, p[max(len(p)-3, 0):]...)
	l.b = l.b[max(len(l.b)-3, 0):]
	return len(p), nil
}
