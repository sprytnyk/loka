package qr

import (
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// samples cover every version and both count field sizes.
func samples() []string {
	texts := []string{
		"http://192.0.2.10:45873/",
		"http://192.168.100.200:45873/download/Qx3_Zk-8aLmN0pQrStUvWxYz",
		"Збірка – 音楽 – 🎵",
	}
	for n := 1; n <= 213; n += 7 {
		texts = append(texts, strings.Repeat("a", n))
	}
	return append(texts, strings.Repeat("z", 213))
}

func TestVersionsGrowWithText(t *testing.T) {
	for _, c := range []struct {
		length, version int
	}{{14, 1}, {15, 2}, {26, 2}, {27, 3}, {62, 4}, {63, 5}, {180, 9}, {181, 10}, {213, 10}} {
		code, err := Encode(strings.Repeat("x", c.length))
		if err != nil {
			t.Fatal(err)
		}
		if want := c.version*4 + 17; code.Size != want {
			t.Errorf("%d bytes: size %d, want %d", c.length, code.Size, want)
		}
	}
	if _, err := Encode(strings.Repeat("x", 214)); !errors.Is(err, ErrTooLong) {
		t.Fatalf("got %v, want ErrTooLong", err)
	}
}

// TestZbarReadsCodes decodes each code with zbarimg, an independent
// decoder, and compares the text. It is skipped where zbarimg is missing.
func TestZbarReadsCodes(t *testing.T) {
	zbar, err := exec.LookPath("zbarimg")
	if err != nil {
		t.Skip("zbarimg is not installed")
	}
	dir := t.TempDir()
	for i, text := range samples() {
		code, err := Encode(text)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "code.png")
		writePNG(t, file, code)
		out, err := exec.Command(zbar, "--quiet", "--raw", "-Sbinary", file).Output()
		if err != nil {
			t.Fatalf("sample %d (%d bytes): zbarimg failed: %v", i, len(text), err)
		}
		if got := string(out); got != text && got != text+"\n" {
			t.Fatalf("sample %d: got %q, want %q", i, got, text)
		}
	}
}

func writePNG(t *testing.T, file string, c *Code) {
	const scale = 4
	n := (c.Size + 2*quiet) * scale
	img := image.NewGray(image.Rect(0, 0, n, n))
	for y := range n {
		for x := range n {
			mx, my := x/scale-quiet, y/scale-quiet
			if mx >= 0 && my >= 0 && mx < c.Size && my < c.Size && c.Dark(mx, my) {
				img.SetGray(x, y, color.Gray{0})
			} else {
				img.SetGray(x, y, color.Gray{255})
			}
		}
	}
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestTextAndSVGDrawEveryModule(t *testing.T) {
	code, _ := Encode("http://192.0.2.10:45873/")
	n := code.Size + 2*quiet
	lines := strings.Split(strings.TrimSuffix(code.Text(), "\n"), "\n")
	if len(lines) != (n+1)/2 {
		t.Fatalf("got %d lines, want %d", len(lines), (n+1)/2)
	}
	for row, line := range lines {
		cells := []rune(line)
		if len(cells) != n {
			t.Fatalf("line of %d characters, want %d", len(cells), n)
		}
		// Read each character back as its two modules: a block is light.
		for x, cell := range cells {
			top := cell == '█' || cell == '▀'
			bottom := cell == '█' || cell == '▄'
			for i, light := range []bool{top, bottom} {
				mx, my := x-quiet, row*2+i-quiet
				if my >= code.Size+quiet {
					continue
				}
				want := mx < 0 || my < 0 || mx >= code.Size || my >= code.Size || !code.Dark(mx, my)
				if light != want {
					t.Fatalf("module %d,%d: light %v, want %v", mx, my, light, want)
				}
			}
		}
	}
	dark := 0
	for y := range code.Size {
		for x := range code.Size {
			if code.Dark(x, y) {
				dark++
			}
		}
	}
	if got := strings.Count(code.SVG(), "h1v1h-1z"); got != dark {
		t.Fatalf("SVG draws %d modules, want %d", got, dark)
	}
}
