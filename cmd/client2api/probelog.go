package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// probeLogWindow is how long one balance-probe failure stays quiet after it has
// been logged.
//
// The soft sweep runs every five minutes, so an hour collapses twelve identical
// lines into one.  The window is deliberately shorter than a working session: a
// failure that clears is reported again within the hour, so the log still shows
// when a platform recovered rather than only that it was once broken.
const probeLogWindow = time.Hour

// probeLogDeduper collapses repeated identical balance-probe failures.
//
// WHY.  The live log had a platform answering "HTTP 403: User has been banned"
// 102 times in half a day, and four relays answering 404 to a billing probe
// that does not exist -- 400+ lines, all of them describing states that will
// not change without an operator.  A log whose majority is unchanging,
// unfixable lines is a log nobody reads, which is how the one line that did
// matter (a request-level 400) went unnoticed.
//
// The deduper is deliberately not a logger: it decides whether a line is worth
// emitting and how many repeats it stands for, and the caller keeps the
// existing log format.  It is safe for concurrent use because the scheduled and
// manual sweeps can overlap.
type probeLogDeduper struct {
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	seen map[string]*probeLogEntry
	// order preserves first-seen order so the summary at the end of a sweep is
	// stable and readable rather than map-ordered.
	order []string
}

type probeLogEntry struct {
	key        string
	lastLog    time.Time
	suppressed int
}

func newProbeLogDeduper(window time.Duration) *probeLogDeduper {
	return &probeLogDeduper{
		window: window,
		now:    time.Now,
		seen:   map[string]*probeLogEntry{},
	}
}

// shouldLog reports whether this failure is worth a line right now, and the
// text to print (with a suppressed-repeat suffix when the failure came back
// after being quiet).
//
// The identity of a failure is (platform, account, error text), so a relay that
// starts answering a different error is always reported: a change in the answer
// is news even when the line looks similar.
func (d *probeLogDeduper) shouldLog(client, account string, err error) (string, bool) {
	if d == nil || err == nil {
		return "", false
	}
	msg := probeErrorText(err)
	key := strings.Join([]string{client, account, msg}, "\x00")

	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	entry, ok := d.seen[key]
	if !ok {
		d.seen[key] = &probeLogEntry{key: key, lastLog: now}
		d.order = append(d.order, key)
		return msg, true
	}
	if now.Sub(entry.lastLog) < d.window {
		entry.suppressed++
		return "", false
	}
	entry.lastLog = now
	if entry.suppressed > 0 {
		// Say how many repeats were swallowed: an operator tuning a platform
		// needs to know whether a line stands for itself or for twenty.
		out := fmt.Sprintf("%s (repeated %d× in the previous %s)", msg, entry.suppressed+1, d.window)
		entry.suppressed = 0
		return out, true
	}
	return msg, true
}

// reportOnReturn summarises the failures that were silenced during this sweep.
//
// One compact line per platform is what keeps the log honest: the sweep is
// visible, the platforms are named, and the operator can see that probes are
// still running without reading forty identical errors.  A sweep with nothing
// silenced prints nothing, so a healthy system stays quiet.
func (d *probeLogDeduper) reportOnReturn(logger *log.Logger) {
	if d == nil || logger == nil {
		return
	}
	d.mu.Lock()
	byClient := map[string]int{}
	for _, key := range d.order {
		entry, ok := d.seen[key]
		if !ok || entry.suppressed == 0 {
			continue
		}
		name := strings.SplitN(key, "\x00", 2)[0]
		byClient[name] += entry.suppressed
		entry.suppressed = 0
	}
	d.mu.Unlock()
	if len(byClient) == 0 {
		return
	}
	names := make([]string, 0, len(byClient))
	for name := range byClient {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", name, byClient[name]))
	}
	logf(logger, "[balance sweep] %d repeat failure(s) suppressed this sweep: %s",
		totalProbeSuppressed(byClient), strings.Join(parts, ", "))
}

func totalProbeSuppressed(byClient map[string]int) int {
	total := 0
	for _, n := range byClient {
		total += n
	}
	return total
}

// probeErrorText renders a probe error for the log with the volatile parts
// removed, so a vendor request id does not make every line look like a new
// failure.
//
// Redaction runs first: a billing response can echo the credential, and this
// text is written to disk.
func probeErrorText(err error) string {
	msg := core.Redact(err.Error())
	msg = probeStableText(msg)
	if len(msg) > maxProbeLogText {
		msg = msg[:maxProbeLogText] + "…"
	}
	return msg
}

// maxProbeLogText bounds one probe line.  A 502 page from Cloudflare is a few
// hundred bytes of JSON; a full HTML error page is not, and it is the same page
// every time.
const maxProbeLogText = 240

// probeStableText strips the parts of a vendor error that change on every
// request while saying nothing.  A request id is the main offender: without
// this, a banned key's 102 identical lines would still be 102 distinct keys in
// the deduper and nothing would collapse.
func probeStableText(s string) string {
	if s == "" {
		return s
	}
	lower := strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(lower); {
		// Only the token is replaced; the field name stays, so the line still
		// reads as an error that carried a request id rather than losing the
		// word that explains what was removed.
		start, end, ok := probeRequestIDToken(lower, i)
		if !ok {
			i++
			continue
		}
		b.WriteString(s[:start])
		b.WriteString("<request-id>")
		s = s[end:]
		lower = lower[end:]
		i = 0
	}
	b.WriteString(s)
	return b.String()
}

// probeRequestIDToken reports where a request-id token starts and ends, given a
// scan position i in the lowercased text.
//
// The forms seen live: `"requestId":"ec3ce62f..."` (workbuddy), `request id:
// 202610290425053286251388268d9d6U5ljkcX1` (lingxiai), and a bare long token
// (New API relays).  The lingxiai id is hex-prefixed but alphanumeric overall,
// so the token class is wider than hexadecimal.
func probeRequestIDToken(lower string, i int) (int, int, bool) {
	for _, marker := range []string{"requestid\":\"", `"requestid": "`, "request id: ", "request_id="} {
		if !strings.HasPrefix(lower[i:], marker) {
			continue
		}
		start := i + len(marker)
		end := start
		for end < len(lower) && isIDTokenByte(lower[end]) {
			end++
		}
		if end > start {
			return start, end, true
		}
	}
	// A bare long token is an id too, but only when it is long enough to be
	// one: a short run is part of the message ("0x1f", "5xx").
	end := i
	for end < len(lower) && isIDTokenByte(lower[end]) {
		end++
	}
	if end-i >= 24 {
		return i, end, true
	}
	return 0, 0, false
}

func isIDTokenByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || c == '-'
}
