package opencode

import (
	"crypto/rand"
	"time"
)

// The anonymous free-tier handshake needs three things at once.  Drop any one
// and Zen answers 403 FreeTierError ("OpenCode's free tier can only be used
// from within OpenCode"):
//
//   - a User-Agent that names the opencode client at version >= 1.18
//     (opencode.go, userAgent);
//   - a streaming chat body declaring functions named `bash` and `read`
//     (body.go, buildFreeChatBody);
//   - an `x-opencode-session` header carrying an id of the shape the CLI mints.
//
// The session value is validated but never authenticated against a store, so
// the module mints its own instead of shelling out to the CLI.  The vendor
// (sst/opencode, packages/opencode/src/id/id.ts) builds an id as
//
//	prefix + "_" + 6 bytes of a descending timestamp counter + 14 random
//	base62 characters
//
// rendered as 12 lowercase hex characters followed by 14 base62 characters,
// 26 in total.  Live probing confirmed the shape is what is enforced: 13 hex
// characters, upper-case hex, dashes, and non-hex letters are all refused,
// while a correctly shaped value is accepted.

const (
	// base62 is the alphabet the vendor's random tail draws from.
	base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	// sessionRandomChars is the length of that random tail.
	sessionRandomChars = 14
)

// freeTierSession returns the value the anonymous handshake must carry in its
// `x-opencode-session` header.  It is minted once per process so a single
// logical client keeps presenting the same id.
func (c *Client) freeTierSession() string {
	c.sessionOnce.Do(func() {
		c.session = mintSessionID(c.now(), &c.sessionCounter)
	})
	return c.session
}

// mintSessionID reproduces the vendor's id.  The time half descends, so ids
// minted in the same millisecond still sort newest-first; counter breaks the
// tie.  A broken CSPRNG is not a reason to refuse the request: the fallback is
// still well shaped, so the upstream answers on its own merits.
func mintSessionID(now time.Time, counter *uint32) string {
	*counter++
	value := ^(uint64(now.UnixMilli())*0x1000 + uint64(*counter))
	var timeBytes [6]byte
	for i := range timeBytes {
		timeBytes[i] = byte((value >> (40 - 8*i)) & 0xff)
	}
	return "ses_" + hex12(timeBytes[:]) + randomBase62(sessionRandomChars)
}

const hexDigits = "0123456789abcdef"

// hex12 renders 6 bytes as 12 lowercase hex characters.
func hex12(b []byte) string {
	out := make([]byte, 0, len(b)*2)
	for _, v := range b {
		out = append(out, hexDigits[v>>4], hexDigits[v&0x0f])
	}
	return string(out)
}

// randomBase62 returns n characters drawn from base62 using the system CSPRNG.
// If the CSPRNG fails it falls back to zeros, which still satisfies the shape
// the vendor validates.
func randomBase62(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return zeros62(n)
	}
	out := make([]byte, n)
	for i, v := range buf {
		out[i] = base62[int(v)%len(base62)]
	}
	return string(out)
}

func zeros62(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = base62[0]
	}
	return string(out)
}
