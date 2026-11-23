// Package qr: a small QR Code encoder (byte mode, error correction M,
// versions 1-20) for the Telegram sign-in link -- SVG for the web UI,
// half-block text for the terminal. No dependencies.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// blocks per version for level M: ec codewords per block, then
// (count, data codewords) for group 1 and group 2.
var eccM = [21][5]int{
	{},
	{10, 1, 16, 0, 0},
	{16, 1, 28, 0, 0},
	{26, 1, 44, 0, 0},
	{18, 2, 32, 0, 0},
	{24, 2, 43, 0, 0},
	{16, 4, 27, 0, 0},
	{18, 4, 31, 0, 0},
	{22, 2, 38, 2, 39},
	{22, 3, 36, 2, 37},
	{26, 4, 43, 1, 44},
	{30, 1, 50, 4, 51},
	{22, 6, 36, 2, 37},
	{22, 8, 37, 1, 38},
	{24, 4, 40, 5, 41},
	{24, 5, 41, 5, 42},
	{28, 7, 45, 3, 46},
	{28, 10, 46, 1, 47},
	{26, 9, 43, 4, 44},
	{26, 3, 44, 11, 45},
	{26, 3, 41, 13, 42},
}

var alignPos = [21][]int{
	{}, {}, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34},
	{6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}, {6, 30, 54}, {6, 32, 58}, {6, 34, 62},
	{6, 26, 46, 66}, {6, 26, 48, 70}, {6, 26, 50, 74}, {6, 30, 54, 78}, {6, 30, 56, 82}, {6, 30, 58, 86}, {6, 34, 62, 90},
}

// Code: Modules[y][x], true = dark.
type Code struct {
	Version int
	Size    int
	Mask    int
	Modules [][]bool
	isFn    [][]bool
}

func dataCapacity(v int) int {
	e := eccM[v]
	return e[1]*e[2] + e[3]*e[4]
}

// Encode picks the smallest version and the best mask.
func Encode(text string) (*Code, error) { return EncodeMask(text, -1, 0) }

// EncodeMask: mask -1 = best; version 0 = smallest that fits.
func EncodeMask(text string, mask, version int) (*Code, error) {
	data := []byte(text)
	ver := version
	if ver == 0 {
		for v := 1; v <= 20; v++ {
			cc := 8
			if v >= 10 {
				cc = 16
			}
			if 4+cc+8*len(data) <= dataCapacity(v)*8 {
				ver = v
				break
			}
		}
		if ver == 0 {
			return nil, errors.New("qr: text too long")
		}
	}
	cw, err := codewords(data, ver)
	if err != nil {
		return nil, err
	}
	c := newCode(ver)
	c.drawFunction()
	c.drawData(cw)
	if mask < 0 {
		best, bestScore := 0, -1
		for m := 0; m < 8; m++ {
			c.applyMask(m)
			c.drawFormat(m)
			s := c.penalty()
			if bestScore < 0 || s < bestScore {
				best, bestScore = m, s
			}
			c.applyMask(m) // undo (XOR)
		}
		mask = best
	}
	c.applyMask(mask)
	c.drawFormat(mask)
	c.Mask = mask
	c.isFn = nil
	return c, nil
}

// ---- data codewords ----

type bitBuf struct {
	b []byte
	n int
}

func (bb *bitBuf) put(v uint, bits int) {
	for i := bits - 1; i >= 0; i-- {
		if bb.n%8 == 0 {
			bb.b = append(bb.b, 0)
		}
		if (v>>uint(i))&1 == 1 {
			bb.b[bb.n/8] |= 0x80 >> uint(bb.n%8)
		}
		bb.n++
	}
}

func codewords(data []byte, ver int) ([]byte, error) {
	capBytes := dataCapacity(ver)
	var bb bitBuf
	bb.put(0b0100, 4)
	cc := 8
	if ver >= 10 {
		cc = 16
	}
	bb.put(uint(len(data)), cc)
	for _, d := range data {
		bb.put(uint(d), 8)
	}
	if bb.n > capBytes*8 {
		return nil, fmt.Errorf("qr: data does not fit version %d", ver)
	}
	term := capBytes*8 - bb.n
	if term > 4 {
		term = 4
	}
	bb.put(0, term)
	if bb.n%8 != 0 {
		bb.put(0, 8-bb.n%8)
	}
	for pad := byte(0xEC); len(bb.b) < capBytes; pad ^= 0xEC ^ 0x11 {
		bb.b = append(bb.b, pad)
	}
	// Split into blocks, RS each, interleave.
	e := eccM[ver]
	ecLen := e[0]
	var blocks [][]byte
	off := 0
	for g := 0; g < 2; g++ {
		cnt, size := e[1+2*g], e[2+2*g]
		for i := 0; i < cnt; i++ {
			blocks = append(blocks, bb.b[off:off+size])
			off += size
		}
	}
	gen := rsGenerator(ecLen)
	ecs := make([][]byte, len(blocks))
	maxData := 0
	for i, b := range blocks {
		ecs[i] = rsRemainder(b, gen)
		if len(b) > maxData {
			maxData = len(b)
		}
	}
	var out []byte
	for i := 0; i < maxData; i++ {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < ecLen; i++ {
		for _, ec := range ecs {
			out = append(out, ec[i])
		}
	}
	return out, nil
}

// ---- Reed-Solomon over GF(256), poly 0x11D ----

var gfExp [512]byte
var gfLog [256]byte

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// rsGenerator: coefficients of prod (x - a^i), i = 0..n-1, highest first
// without the leading 1.
func rsGenerator(n int) []byte {
	g := []byte{1}
	for i := 0; i < n; i++ {
		ng := make([]byte, len(g)+1)
		for j, c := range g {
			ng[j] ^= c
			ng[j+1] ^= gfMul(c, gfExp[i])
		}
		g = ng
	}
	return g[1:]
}

func rsRemainder(data, gen []byte) []byte {
	rem := make([]byte, len(gen))
	for _, d := range data {
		f := d ^ rem[0]
		copy(rem, rem[1:])
		rem[len(rem)-1] = 0
		for i, g := range gen {
			rem[i] ^= gfMul(g, f)
		}
	}
	return rem
}

// ---- matrix ----

func newCode(ver int) *Code {
	size := 17 + 4*ver
	c := &Code{Version: ver, Size: size}
	c.Modules = make([][]bool, size)
	c.isFn = make([][]bool, size)
	for i := range c.Modules {
		c.Modules[i] = make([]bool, size)
		c.isFn[i] = make([]bool, size)
	}
	return c
}

func (c *Code) set(x, y int, dark bool) {
	c.Modules[y][x] = dark
	c.isFn[y][x] = true
}

func (c *Code) drawFunction() {
	n := c.Size
	for i := 0; i < n; i++ {
		c.set(6, i, i%2 == 0)
		c.set(i, 6, i%2 == 0)
	}
	c.finder(3, 3)
	c.finder(n-4, 3)
	c.finder(3, n-4)
	pos := alignPos[c.Version]
	for i, a := range pos {
		for j, b := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					d := max(abs(dx), abs(dy))
					c.set(a+dx, b+dy, d != 1)
				}
			}
		}
	}
	c.drawFormat(0) // reserve
	if c.Version >= 7 {
		rem := c.Version
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
		}
		bits := c.Version<<12 | rem
		for i := 0; i < 18; i++ {
			bit := (bits>>uint(i))&1 == 1
			a := n - 11 + i%3
			b := i / 3
			c.set(a, b, bit)
			c.set(b, a, bit)
		}
	}
}

func (c *Code) finder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
				continue
			}
			d := max(abs(dx), abs(dy))
			c.set(x, y, d != 2 && d != 4)
		}
	}
}

// drawFormat: level M (bits 00) + mask, BCH(15,5), XOR 0x5412.
func (c *Code) drawFormat(mask int) {
	data := 0<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return (bits>>uint(i))&1 == 1 }
	n := c.Size
	for i := 0; i <= 5; i++ {
		c.set(8, i, bit(i))
	}
	c.set(8, 7, bit(6))
	c.set(8, 8, bit(7))
	c.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		c.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		c.set(n-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		c.set(8, n-15+i, bit(i))
	}
	c.set(8, n-8, true)
}

func (c *Code) drawData(cw []byte) {
	n := c.Size
	i := 0
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = n - 1 - vert
				}
				if !c.isFn[y][x] && i < len(cw)*8 {
					c.Modules[y][x] = (cw[i>>3]>>(7-uint(i&7)))&1 == 1
					i++
				}
			}
		}
	}
}

func (c *Code) applyMask(m int) {
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.isFn[y][x] {
				continue
			}
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv {
				c.Modules[y][x] = !c.Modules[y][x]
			}
		}
	}
}

func (c *Code) penalty() int {
	n := c.Size
	score := 0
	get := func(x, y int, row bool) bool {
		if row {
			return c.Modules[y][x]
		}
		return c.Modules[x][y]
	}
	for _, row := range []bool{true, false} {
		for a := 0; a < n; a++ {
			run := 1
			for b := 1; b < n; b++ {
				if get(b, a, row) == get(b-1, a, row) {
					run++
				} else {
					if run >= 5 {
						score += 3 + run - 5
					}
					run = 1
				}
			}
			if run >= 5 {
				score += 3 + run - 5
			}
			// finder-like 1011101 with 4 light on one side (outside = light)
			for b := -4; b < n; b++ {
				at := func(k int) bool {
					if k < 0 || k >= n {
						return false
					}
					return get(k, a, row)
				}
				pat := []bool{true, false, true, true, true, false, true}
				ok := true
				for k, p := range pat {
					if at(b+k) != p {
						ok = false
						break
					}
				}
				if !ok {
					continue
				}
				before, after := true, true
				for k := 1; k <= 4; k++ {
					if at(b - k) {
						before = false
					}
					if at(b + 6 + k) {
						after = false
					}
				}
				if before || after {
					score += 40
				}
			}
		}
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if c.Modules[y][x] {
				dark++
			}
			if x < n-1 && y < n-1 {
				v := c.Modules[y][x]
				if v == c.Modules[y][x+1] && v == c.Modules[y+1][x] && v == c.Modules[y+1][x+1] {
					score += 3
				}
			}
		}
	}
	total := n * n
	k := 0
	for dark*20 < (9-k)*total || dark*20 > (11+k)*total {
		k++
	}
	return score + k*10
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---- output ----

// SVG: crisp square modules, quiet zone 4, colours given (dark, light).
func (c *Code) SVG(dark, light string) string {
	const qz = 4
	n := c.Size + 2*qz
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="%s"/><path fill="%s" d="`, n, n, light, dark)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Modules[y][x] {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+qz, y+qz)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

// Terminal: two module rows per text line with half blocks; light
// modules are drawn (for a dark terminal), quiet zone 2.
func (c *Code) Terminal() string {
	const qz = 2
	n := c.Size + 2*qz
	light := func(x, y int) bool {
		x, y = x-qz, y-qz
		if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
			return true
		}
		return !c.Modules[y][x]
	}
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		for x := 0; x < n; x++ {
			top := light(x, y)
			bot := y+1 < n && light(x, y+1)
			switch {
			case top && bot:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bot:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
