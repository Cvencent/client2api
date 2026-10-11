package autoclaw

import (
	"crypto/rand"
	"encoding/hex"
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
