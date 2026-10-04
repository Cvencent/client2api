package raccoon

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Dependency-free QR encoder.
//
// Scope is deliberately the minimum this module needs: BYTE mode, error
// correction level M, versions 1-10 (213 bytes of payload), rendered as an
// inline SVG.  Nothing else is supported, and content that does not fit is an
// error rather than a silently unreadable code.
//
// The algorithm follows ISO/IEC 18004; the structure matches the reference
// implementation this module was ported from (src/raccoon-qr.ts).  The
// reference and this port are cross-checked by the fixed matrices in
// qr_test.go, which were produced by the reference encoder.
// ---------------------------------------------------------------------------

// qrDataCodewords is the number of DATA codewords for error correction level M
// at each version (index 0 unused).
var qrDataCodewords = [11]int{0, 16, 28, 44, 64, 86, 108, 124, 154, 182, 216}

// qrBlockGroup is one run of equal-sized blocks in a version's block layout.
type qrBlockGroup struct {
	count     int
	dataWords int
}

// qrVersionSpec is the Reed-Solomon layout for one version at level M.
type qrVersionSpec struct {
	ecPerBlock int
	groups     []qrBlockGroup
}

var qrSpecsM = [11]qrVersionSpec{
	{},
	{ecPerBlock: 10, groups: []qrBlockGroup{{1, 16}}},
	{ecPerBlock: 16, groups: []qrBlockGroup{{1, 28}}},
	{ecPerBlock: 26, groups: []qrBlockGroup{{1, 44}}},
	{ecPerBlock: 18, groups: []qrBlockGroup{{2, 32}}},
	{ecPerBlock: 24, groups: []qrBlockGroup{{2, 43}}},
	{ecPerBlock: 16, groups: []qrBlockGroup{{4, 27}}},
	{ecPerBlock: 18, groups: []qrBlockGroup{{4, 31}}},
	{ecPerBlock: 22, groups: []qrBlockGroup{{2, 38}, {2, 39}}},
	{ecPerBlock: 22, groups: []qrBlockGroup{{3, 36}, {2, 37}}},
	{ecPerBlock: 26, groups: []qrBlockGroup{{4, 43}, {1, 44}}},
}

// qrMatrix is a finished QR code.
type qrMatrix struct {
	Size    int
	Modules [][]bool
}

// --- GF(256) arithmetic (primitive polynomial 0x11D) ----------------------

var qrGFExp [512]byte
var qrGFLog [256]byte

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		qrGFExp[i] = byte(x)
		qrGFLog[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		qrGFExp[i] = qrGFExp[i-255]
	}
}

func qrGFMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return qrGFExp[int(qrGFLog[a])+int(qrGFLog[b])]
}

// qrPolyMul multiplies two polynomials with the highest power first.
func qrPolyMul(a, b []byte) []byte {
	out := make([]byte, len(a)+len(b)-1)
	for i, av := range a {
		for j, bv := range b {
			out[i+j] ^= qrGFMul(av, bv)
		}
	}
	return out
}

// qrRSGenerator builds the degree-n Reed-Solomon generator polynomial.
func qrRSGenerator(n int) []byte {
	poly := []byte{1}
	for i := 0; i < n; i++ {
		poly = qrPolyMul(poly, []byte{1, qrGFExp[i]})
	}
	return poly
}

// qrRSEncode returns the n error-correction codewords for one block.
func qrRSEncode(data []byte, n int) []byte {
	gen := qrRSGenerator(n)
	buf := make([]byte, len(data)+n)
	copy(buf, data)
	for i := 0; i < len(data); i++ {
		coef := buf[i]
		if coef == 0 {
			continue
		}
		for j, g := range gen {
			buf[i+j] ^= qrGFMul(g, coef)
		}
	}
	return buf[len(data):]
}

// --- data encoding --------------------------------------------------------

// qrPickVersion returns the smallest version that fits n bytes, or 0.
func qrPickVersion(n int) int {
	for v := 1; v <= 10; v++ {
		capacity := qrDataCodewords[v] * 8
		overhead := 4
		if v <= 9 {
			overhead += 8
		} else {
			overhead += 16
		}
		if overhead+n*8 <= capacity {
			return v
		}
	}
	return 0
}

// qrBuildCodewords encodes bytes into the interleaved data+ECC codeword
// sequence for a version.
func qrBuildCodewords(data []byte, version int) ([]byte, error) {
	spec := qrSpecsM[version]
	if spec.ecPerBlock == 0 {
		return nil, fmt.Errorf("raccoon: unsupported QR version %d", version)
	}
	totalData := qrDataCodewords[version]
	capacityBits := totalData * 8

	bits := make([]bool, 0, capacityBits)
	pushBits := func(value, length int) {
		for i := length - 1; i >= 0; i-- {
			bits = append(bits, (value>>uint(i))&1 == 1)
		}
	}
	pushBits(0b0100, 4) // byte mode
	if version <= 9 {
		pushBits(len(data), 8)
	} else {
		pushBits(len(data), 16)
	}
	for _, b := range data {
		pushBits(int(b), 8)
	}
	terminator := 4
	if remaining := capacityBits - len(bits); remaining < terminator {
		terminator = remaining
	}
	pushBits(0, terminator)
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	pad := [2]int{0xEC, 0x11}
	for i := 0; len(bits) < capacityBits; i++ {
		pushBits(pad[i%2], 8)
	}

	// Pack the bit stream into data codewords.
	dataCodewords := make([]byte, 0, totalData)
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for j := 0; j < 8; j++ {
			b <<= 1
			if bits[i+j] {
				b |= 1
			}
		}
		dataCodewords = append(dataCodewords, b)
	}

	// Split into blocks and compute each block's ECC.
	var dataBlocks, ecBlocks [][]byte
	offset := 0
	for _, g := range spec.groups {
		for b := 0; b < g.count; b++ {
			block := append([]byte(nil), dataCodewords[offset:offset+g.dataWords]...)
			offset += g.dataWords
			dataBlocks = append(dataBlocks, block)
			ecBlocks = append(ecBlocks, qrRSEncode(block, spec.ecPerBlock))
		}
	}

	// Interleave: every block's first data codeword, then second, ...; then
	// every block's ECC codewords.
	maxData := 0
	for _, b := range dataBlocks {
		if len(b) > maxData {
			maxData = len(b)
		}
	}
	out := make([]byte, 0, totalData+spec.ecPerBlock*len(dataBlocks))
	for i := 0; i < maxData; i++ {
		for _, b := range dataBlocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < spec.ecPerBlock; i++ {
		for _, b := range ecBlocks {
			out = append(out, b[i])
		}
	}
	return out, nil
}

// --- matrix construction --------------------------------------------------

// qrAlignmentPositions returns the alignment-pattern centre coordinates.
func qrAlignmentPositions(version, size int) []int {
	if version == 1 {
		return nil
	}
	numAlign := version/7 + 2
	step := ((version*4 + 4) + (numAlign*2 - 2) - 1) / (numAlign*2 - 2) * 2
	result := []int{6}
	for pos := size - 7; len(result) < numAlign; pos -= step {
		result = append(result[:1], append([]int{pos}, result[1:]...)...)
	}
	return result
}

func qrDrawFinderPattern(modules, isFunction [][]bool, size, cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || x >= size || y < 0 || y >= size {
				continue
			}
			dist := absInt(dx)
			if absInt(dy) > dist {
				dist = absInt(dy)
			}
			dark := dist != 2 && dist != 4
			modules[y][x] = dark
			isFunction[y][x] = true
		}
	}
}

func qrDrawAlignmentPattern(modules, isFunction [][]bool, cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			d := absInt(dx)
			if absInt(dy) > d {
				d = absInt(dy)
			}
			dark := d != 1
			modules[cy+dy][cx+dx] = dark
			isFunction[cy+dy][cx+dx] = true
		}
	}
}

// qrDrawFormatBits writes both copies of the 15-bit format information.
func qrDrawFormatBits(modules, isFunction [][]bool, size, mask int) {
	data := (0b00 << 3) | mask // error correction level M is 0b00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := ((data << 10) | rem) ^ 0x5412

	set := func(x, y int, dark bool) {
		modules[y][x] = dark
		isFunction[y][x] = true
	}
	bit := func(i int) bool { return (bits>>uint(i))&1 == 1 }

	for i := 0; i <= 5; i++ {
		set(8, i, bit(i))
	}
	set(8, 7, bit(6))
	set(8, 8, bit(7))
	set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		set(size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		set(8, size-7+(i-8), bit(i))
	}
	// The fixed dark module; it carries no format bit.
	set(8, size-8, true)
}

// qrDrawFunctionPatterns lays down timing, finders, alignment, the format
// reservation and (for versions 7+) the version information.
func qrDrawFunctionPatterns(modules, isFunction [][]bool, size, version int) {
	for i := 0; i < size; i++ {
		dark := i%2 == 0
		modules[6][i] = dark
		isFunction[6][i] = true
		modules[i][6] = dark
		isFunction[i][6] = true
	}
	qrDrawFinderPattern(modules, isFunction, size, 3, 3)
	qrDrawFinderPattern(modules, isFunction, size, size-4, 3)
	qrDrawFinderPattern(modules, isFunction, size, 3, size-4)

	positions := qrAlignmentPositions(version, size)
	n := len(positions)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			isCorner := (i == 0 && j == 0) || (i == 0 && j == n-1) || (i == n-1 && j == 0)
			if isCorner {
				continue
			}
			qrDrawAlignmentPattern(modules, isFunction, positions[i], positions[j])
		}
	}

	qrDrawFormatBits(modules, isFunction, size, 0)

	if version >= 7 {
		rem := version
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
		}
		bits := (version << 12) | rem
		for i := 0; i < 18; i++ {
			dark := (bits>>uint(i))&1 == 1
			a := size - 11 + (i % 3)
			b := i / 3
			modules[b][a] = dark
			isFunction[b][a] = true
			modules[a][b] = dark
			isFunction[a][b] = true
		}
	}
}

// qrDrawCodewords fills the data modules in the standard zigzag order.
func qrDrawCodewords(modules, isFunction [][]bool, size int, codewords []byte) {
	i := 0
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := ((right + 1) & 2) == 0
				y := vert
				if upward {
					y = size - 1 - vert
				}
				if !isFunction[y][x] && i < len(codewords)*8 {
					b := codewords[i>>3]
					modules[y][x] = (b>>uint(7-(i&7)))&1 == 1
					i++
				}
			}
		}
	}
}

func qrApplyMask(modules, isFunction [][]bool, size, mask int) {
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if isFunction[y][x] {
				continue
			}
			var invert bool
			switch mask {
			case 0:
				invert = (x+y)%2 == 0
			case 1:
				invert = y%2 == 0
			case 2:
				invert = x%3 == 0
			case 3:
				invert = (x+y)%3 == 0
			case 4:
				invert = (y/2+x/3)%2 == 0
			case 5:
				invert = (x*y)%2+(x*y)%3 == 0
			case 6:
				invert = ((x*y)%2+(x*y)%3)%2 == 0
			default:
				invert = ((x+y)%2+(x*y)%3)%2 == 0
			}
			if invert {
				modules[y][x] = !modules[y][x]
			}
		}
	}
}

// qrPenaltyForLine scores one row or column under rules 1 and 3.
func qrPenaltyForLine(line []bool, n1, n3 int) int {
	result := 0
	runLength := 1
	for i := 1; i < len(line); i++ {
		if line[i] == line[i-1] {
			runLength++
		} else {
			if runLength >= 5 {
				result += n1 + (runLength - 5)
			}
			runLength = 1
		}
	}
	if runLength >= 5 {
		result += n1 + (runLength - 5)
	}

	patternA := [11]bool{true, false, true, true, true, false, true, false, false, false, false}
	patternB := [11]bool{false, false, false, false, true, false, true, true, true, false, true}
	for i := 0; i+11 <= len(line); i++ {
		matchA, matchB := true, true
		for j := 0; j < 11; j++ {
			if line[i+j] != patternA[j] {
				matchA = false
			}
			if line[i+j] != patternB[j] {
				matchB = false
			}
			if !matchA && !matchB {
				break
			}
		}
		if matchA {
			result += n3
		}
		if matchB {
			result += n3
		}
	}
	return result
}

func qrComputePenalty(modules [][]bool, size int) int {
	const n1, n2, n3, n4 = 3, 3, 40, 10
	result := 0
	for y := 0; y < size; y++ {
		result += qrPenaltyForLine(modules[y], n1, n3)
	}
	col := make([]bool, size)
	for x := 0; x < size; x++ {
		for y := 0; y < size; y++ {
			col[y] = modules[y][x]
		}
		result += qrPenaltyForLine(col, n1, n3)
	}
	for y := 0; y < size-1; y++ {
		for x := 0; x < size-1; x++ {
			c := modules[y][x]
			if c == modules[y][x+1] && c == modules[y+1][x] && c == modules[y+1][x+1] {
				result += n2
			}
		}
	}
	dark := 0
	for _, row := range modules {
		for _, cell := range row {
			if cell {
				dark++
			}
		}
	}
	total := size * size
	diff := dark*20 - total*10
	if diff < 0 {
		diff = -diff
	}
	k := (diff+total-1)/total - 1
	if k > 0 {
		result += k * n4
	}
	return result
}

// qrBuildMatrix encodes text and returns the finished module matrix.
func qrBuildMatrix(text string) (*qrMatrix, error) {
	bytes := []byte(text)
	version := qrPickVersion(len(bytes))
	if version == 0 {
		return nil, fmt.Errorf("raccoon: QR content is too long (%d bytes, limit 213)", len(bytes))
	}
	size := version*4 + 17
	modules := make([][]bool, size)
	isFunction := make([][]bool, size)
	for i := range modules {
		modules[i] = make([]bool, size)
		isFunction[i] = make([]bool, size)
	}

	qrDrawFunctionPatterns(modules, isFunction, size, version)
	codewords, err := qrBuildCodewords(bytes, version)
	if err != nil {
		return nil, err
	}
	qrDrawCodewords(modules, isFunction, size, codewords)

	bestMask := 0
	bestPenalty := -1
	for mask := 0; mask < 8; mask++ {
		qrApplyMask(modules, isFunction, size, mask)
		qrDrawFormatBits(modules, isFunction, size, mask)
		penalty := qrComputePenalty(modules, size)
		if bestPenalty < 0 || penalty < bestPenalty {
			bestPenalty = penalty
			bestMask = mask
		}
		qrApplyMask(modules, isFunction, size, mask) // XOR is its own inverse
	}
	qrApplyMask(modules, isFunction, size, bestMask)
	qrDrawFormatBits(modules, isFunction, size, bestMask)

	return &qrMatrix{Size: size, Modules: modules}, nil
}

// renderQRSVG renders text as an inline SVG.  px is the rendered width and
// height in CSS pixels; the quiet zone is always 4 modules.
func renderQRSVG(text string, px int) (string, error) {
	m, err := qrBuildMatrix(text)
	if err != nil {
		return "", err
	}
	if px <= 0 {
		px = 158
	}
	const margin = 4
	dim := m.Size + margin*2

	var path strings.Builder
	for y := 0; y < m.Size; y++ {
		for x := 0; x < m.Size; x++ {
			if !m.Modules[y][x] {
				continue
			}
			fmt.Fprintf(&path, "M%d,%dh1v1h-1z", x+margin, y+margin)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img">`, px, px, dim, dim)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, dim, dim)
	fmt.Fprintf(&b, `<path d="%s" fill="#000000"/>`, path.String())
	b.WriteString(`</svg>`)
	return b.String(), nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
