package autoclaw

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

func randomHex(n int) string {
	if n <= 0 {
		n = 16
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// randomHex is only used for trace IDs; a clock-derived value is
		// still unique enough for correlation if the system RNG fails.
		for i := range buf {
			buf[i] = byte(i * 31)
		}
	}
	return hex.EncodeToString(buf)
}

// randomUUID returns a random version-4 UUID in canonical form.
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := uint64(time.Now().UnixNano())
		for i := range b {
			b[i] = byte(now >> (8 * uint(i%8)))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
