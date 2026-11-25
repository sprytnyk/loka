// Package qr makes QR codes for links. It covers only what links need: byte
// mode, medium error correction and versions 1 to 10, which hold up to 213
// bytes.
package qr

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Code is a QR code: Size rows of Size modules, true for dark.
type Code struct {
	Size    int
	modules []bool
	// function marks the finder, timing, alignment, format and version
	// modules, which masks leave alone.
	function []bool
}

// Dark reports whether the module at column x and row y is dark.
func (c *Code) Dark(x, y int) bool {
	return c.modules[y*c.Size+x]
}

// Per version, at medium error correction: the error correction codewords
// in each block and the number of blocks.
var (
	ecPerBlock = [...]int{10, 16, 26, 18, 24, 16, 18, 22, 22, 26}
	blocks     = [...]int{1, 1, 1, 2, 2, 4, 4, 4, 5, 5}
)

const maxVersion = len(ecPerBlock)

// ErrTooLong means the text does not fit in the largest version supported.
var ErrTooLong = errors.New("qr: text is too long")

// Encode makes the smallest QR code that holds text.
func Encode(text string) (*Code, error) {
	data := []byte(text)
	for version := 1; version <= maxVersion; version++ {
		if capacity := dataCodewords(version)*8 - 4 - countBits(version); len(data)*8 <= capacity {
			return build(version, data), nil
		}
	}
	return nil, ErrTooLong
}

// countBits is the size of the byte count field.
func countBits(version int) int {
	if version < 10 {
		return 8
	}
	return 16
}

// rawCodewords is how many codewords a version holds, data and error
// correction together: its modules less the function patterns, in bytes.
func rawCodewords(version int) int {
	modules := (16*version+128)*version + 64
	if version >= 2 {
		align := version/7 + 2
		modules -= (25*align-10)*align - 55
		if version >= 7 {
			modules -= 36
		}
	}
	return modules / 8
}

func dataCodewords(version int) int {
	return rawCodewords(version) - ecPerBlock[version-1]*blocks[version-1]
}

func build(version int, data []byte) *Code {
	size := version*4 + 17
	c := &Code{Size: size, modules: make([]bool, size*size), function: make([]bool, size*size)}
	c.drawFunctionPatterns(version)
	c.drawCodewords(addErrorCorrection(version, encodeData(version, data)))
	best, bestPenalty := 0, -1
	for mask := range 8 {
		c.applyMask(mask)
		c.drawFormat(mask)
		if p := c.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = mask, p
		}
		c.applyMask(mask) // masks undo themselves
	}
	c.applyMask(best)
	c.drawFormat(best)
	return c
}

// encodeData writes the mode, the count, the bytes, the terminator and the
// padding that fill the version's data codewords.
func encodeData(version int, data []byte) []byte {
	var bits bitBuffer
	bits.append(0b0100, 4) // byte mode
	bits.append(len(data), countBits(version))
	for _, b := range data {
		bits.append(int(b), 8)
	}
	capacity := dataCodewords(version) * 8
	bits.append(0, min(4, capacity-len(bits)))
	bits.append(0, (8-len(bits)%8)%8)
	for pad := 0xEC; len(bits) < capacity; pad ^= 0xEC ^ 0x11 {
		bits.append(pad, 8)
	}
	out := make([]byte, len(bits)/8)
	for i, bit := range bits {
		if bit {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out
}

type bitBuffer []bool

func (b *bitBuffer) append(value, n int) {
	for i := n - 1; i >= 0; i-- {
		*b = append(*b, value>>i&1 == 1)
	}
}

// addErrorCorrection splits the data into blocks, adds each block's error
// correction, and interleaves them.
func addErrorCorrection(version int, data []byte) []byte {
	n, ec := blocks[version-1], ecPerBlock[version-1]
	raw := rawCodewords(version)
	short := raw/n - ec // data codewords in a short block
	shortBlocks := n - raw%n
	divisor := rsDivisor(ec)
	var dataBlocks, ecBlocks [][]byte
	for i, k := 0, 0; i < n; i++ {
		length := short
		if i >= shortBlocks {
			length++
		}
		block := data[k : k+length]
		k += length
		dataBlocks = append(dataBlocks, block)
		ecBlocks = append(ecBlocks, rsRemainder(block, divisor))
	}
	var out []byte
	for i := range short + 1 {
		for _, block := range dataBlocks {
			if i < len(block) {
				out = append(out, block[i])
			}
		}
	}
	for i := range ec {
		for _, block := range ecBlocks {
			out = append(out, block[i])
		}
	}
	return out
}

// rsDivisor is the Reed-Solomon generator polynomial of a degree, highest
// term first and its leading 1 left out.
func rsDivisor(degree int) []byte {
	result := make([]byte, degree)
	result[degree-1] = 1
	root := byte(1)
	for range degree {
		for j := range result {
			result[j] = gfMultiply(result[j], root)
			if j+1 < degree {
				result[j] ^= result[j+1]
			}
		}
		root = gfMultiply(root, 2)
	}
	return result
}

func rsRemainder(data, divisor []byte) []byte {
	result := make([]byte, len(divisor))
	for _, b := range data {
		factor := b ^ result[0]
		copy(result, result[1:])
		result[len(result)-1] = 0
		for i, d := range divisor {
			result[i] ^= gfMultiply(d, factor)
		}
	}
	return result
}

// gfMultiply multiplies in GF(2^8) modulo x^8 + x^4 + x^3 + x^2 + 1.
func gfMultiply(x, y byte) byte {
	var z int
	for i := 7; i >= 0; i-- {
		z = z<<1 ^ (z>>7)*0x11D
		z ^= int(y>>i&1) * int(x)
	}
	return byte(z)
}

func (c *Code) set(x, y int, dark bool) {
	c.modules[y*c.Size+x] = dark
	c.function[y*c.Size+x] = true
}

func (c *Code) drawFunctionPatterns(version int) {
	for i := range c.Size {
		c.set(6, i, i%2 == 0)
		c.set(i, 6, i%2 == 0)
	}
	for _, center := range [][2]int{{3, 3}, {c.Size - 4, 3}, {3, c.Size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := center[0]+dx, center[1]+dy
				if x >= 0 && x < c.Size && y >= 0 && y < c.Size {
					d := max(abs(dx), abs(dy))
					c.set(x, y, d != 2 && d != 4)
				}
			}
		}
	}
	positions := alignmentPositions(version)
	last := len(positions) - 1
	for i, x := range positions {
		for j, y := range positions {
			if i == 0 && j == 0 || i == 0 && j == last || i == last && j == 0 {
				continue // a finder is there
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					c.set(x+dx, y+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	c.drawFormat(0) // reserves the format modules until the mask is known
	if version >= 7 {
		rem := version
		for range 12 {
			rem = rem<<1 ^ (rem>>11)*0x1F25
		}
		bits := version<<12 | rem
		for i := range 18 {
			dark := bits>>i&1 == 1
			a, b := c.Size-11+i%3, i/3
			c.set(a, b, dark)
			c.set(b, a, dark)
		}
	}
}

// alignmentPositions lists the centre coordinates of the alignment
// patterns, used as both rows and columns.
func alignmentPositions(version int) []int {
	if version == 1 {
		return nil
	}
	n := version/7 + 2
	step := (version*8 + n*3 + 5) / (n*4 - 4) * 2
	positions := []int{6}
	for pos := version*4 + 10; len(positions) < n; pos -= step {
		positions = slices.Insert(positions, 1, pos)
	}
	return positions
}

// drawFormat writes the error correction level (medium, 00) and the mask,
// twice.
func (c *Code) drawFormat(mask int) {
	data := mask // medium error correction is 00
	rem := data
	for range 10 {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 == 1 }
	for i := range 6 {
		c.set(8, i, bit(i))
	}
	c.set(8, 7, bit(6))
	c.set(8, 8, bit(7))
	c.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		c.set(14-i, 8, bit(i))
	}
	for i := range 8 {
		c.set(c.Size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		c.set(8, c.Size-15+i, bit(i))
	}
	c.set(8, c.Size-8, true) // always dark
}

// drawCodewords fills the modules left over, two columns at a time in a
// zigzag from the bottom right.
func (c *Code) drawCodewords(data []byte) {
	i := 0
	for right := c.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // skip the vertical timing pattern
		}
		upward := (right+1)&2 == 0
		for vert := range c.Size {
			y := vert
			if upward {
				y = c.Size - 1 - vert
			}
			for j := range 2 {
				module := y*c.Size + right - j
				if !c.function[module] && i < len(data)*8 {
					c.modules[module] = data[i/8]>>(7-i%8)&1 == 1
					i++
				}
			}
		}
	}
}

func (c *Code) applyMask(mask int) {
	for y := range c.Size {
		for x := range c.Size {
			var flip bool
			switch mask {
			case 0:
				flip = (x+y)%2 == 0
			case 1:
				flip = y%2 == 0
			case 2:
				flip = x%3 == 0
			case 3:
				flip = (x+y)%3 == 0
			case 4:
				flip = (x/3+y/2)%2 == 0
			case 5:
				flip = x*y%2+x*y%3 == 0
			case 6:
				flip = (x*y%2+x*y%3)%2 == 0
			case 7:
				flip = ((x+y)%2+x*y%3)%2 == 0
			}
			if flip && !c.function[y*c.Size+x] {
				c.modules[y*c.Size+x] = !c.modules[y*c.Size+x]
			}
		}
	}
}

// penalty scores how hard a code is to scan, by the four rules of the
// standard. The mask with the lowest score is used.
func (c *Code) penalty() int {
	score := 0
	line := func(get func(i int) bool) {
		run := 1
		for i := 1; i < c.Size; i++ {
			if get(i) == get(i-1) {
				run++
				continue
			}
			if run >= 5 {
				score += run - 2
			}
			run = 1
		}
		if run >= 5 {
			score += run - 2
		}
		// A finder-like 1:1:3:1:1 run with four light modules on one side.
		pattern := []bool{true, false, true, true, true, false, true}
		for i := 0; i+len(pattern) <= c.Size; i++ {
			match := true
			for k, want := range pattern {
				if get(i+k) != want {
					match = false
					break
				}
			}
			if match && (lightRun(get, i-4, i, c.Size) || lightRun(get, i+7, i+11, c.Size)) {
				score += 40
			}
		}
	}
	for y := range c.Size {
		line(func(x int) bool { return c.Dark(x, y) })
	}
	for x := range c.Size {
		line(func(y int) bool { return c.Dark(x, y) })
	}
	dark := 0
	for y := range c.Size {
		for x := range c.Size {
			isDark := c.Dark(x, y)
			if isDark {
				dark++
			}
			if x+1 < c.Size && y+1 < c.Size {
				if c.Dark(x+1, y) == isDark && c.Dark(x, y+1) == isDark && c.Dark(x+1, y+1) == isDark {
					score += 3
				}
			}
		}
	}
	total := c.Size * c.Size
	score += abs(dark*20-total*10) / total * 10
	return score
}

// lightRun reports whether the modules from start to end (exclusive) are
// light, counting those outside the code as light.
func lightRun(get func(int) bool, start, end, size int) bool {
	for i := start; i < end; i++ {
		if i >= 0 && i < size && get(i) {
			return false
		}
	}
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// quiet is the light border a scanner needs around the code, in modules.
const quiet = 4

// SVG draws the code as black modules on white, whatever the page's theme,
// since scanners need dark on light.
func (c *Code) SVG() string {
	n := c.Size + 2*quiet
	var path strings.Builder
	for y := range c.Size {
		for x := range c.Size {
			if c.Dark(x, y) {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	// Inline styles, so a page's own styles for SVG can't repaint the code.
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" style="fill:#fff;stroke:none"/><path style="fill:#000;stroke:none" d="%s"/></svg>`,
		n, n, n, n, path.String())
}

// Text draws the code for a terminal with half-block characters, two rows
// to a line. Light modules are drawn as blocks, so the code reads as dark
// on light in a terminal with light text on a dark background.
func (c *Code) Text() string {
	light := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return x < 0 || y < 0 || x >= c.Size || y >= c.Size || !c.Dark(x, y)
	}
	n := c.Size + 2*quiet
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		for x := range n {
			top, bottom := light(x, y), y+1 < n && light(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
