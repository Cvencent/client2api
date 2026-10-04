package gateway

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"client2api/internal/core"
	"client2api/internal/logfmt"
)

// ---------------------------------------------------------------------------
// per-request console row
// ---------------------------------------------------------------------------
//
// One line per accepted chat request, so that an operator watching the console
// can tell what the service is doing without turning on debug logging:
//
//	| #007 | 14:03:11 | workbuddy/glm-5.2   | stream | 200 | 5f4a1c2e(work) | TTFB=812ms | tok=486 | 61.4tok/s | total=7.9s |
//
// The row is written on the way out of the request, whatever the outcome, and
// it is deliberately lossy: it is a monitoring aid, not a record.  The durable
// record is the usage store.

// chatSeq numbers the rows within this process, so that a line in a scrollback
// can be referred to unambiguously.
var chatSeq atomic.Int64

// chatLogEnabled turns the rows off.  It exists for the test binary, which
// flips it so that a failing assertion is not buried under rows; a running
// service never changes it.
var chatLogEnabled = true

// chatLogOut is where the rows go.  main swaps in a writer that mirrors them
// into the panel's ring buffer, which leaves stdout behaving exactly as before.
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput redirects the rows.  It is called once during start-up,
// before the listener accepts anything, so it needs no locking; a nil writer is
// ignored rather than panicking on the first request.
func SetChatLogOutput(w io.Writer) {
	if w == nil {
		return
	}
	chatLogOut = w
}

// Column widths, in display columns rather than bytes.  The model column has to
// fit the longest qualified name the registry can produce, otherwise a name
// like "workbuddy/deepseek-v4-flash" would push every column after it out of
// alignment.
const (
	chatModelWidth = 26
	chatAcctWidth  = 22
	chatTTFBWidth  = 8
	chatTokWidth   = 6
	chatRateWidth  = 11
)

// chatStat accumulates what the row reports about one request.
//
// toks is -1 until an upstream usage block is seen, which is how "the upstream
// did not say" stays distinguishable from "the upstream said zero".
type chatStat struct {
	start  time.Time
	model  string
	mode   string
	uid    string
	nick   string
	ttfb   time.Duration
	toks   int
	status int
	logged bool
}

// newChatStat starts the clock for a request.  model is what the caller asked
// for; the qualified name replaces it once the registry has resolved it.
func newChatStat(now time.Time, model string, stream bool) *chatStat {
	return &chatStat{
		start: now,
		model: strings.TrimSpace(model),
		mode:  chatMode(stream),
		toks:  -1,
	}
}

// chatMode names the two response shapes the gateway can produce.
func chatMode(stream bool) string {
	if stream {
		return "stream"
	}
	return "sync"
}

// noteFirstFrame records the time to first byte.  Only the first frame counts:
// later frames would turn TTFB into a moving number.
func (s *chatStat) noteFirstFrame(at time.Time) {
	if s == nil || s.ttfb > 0 {
		return
	}
	if d := at.Sub(s.start); d > 0 {
		s.ttfb = d
	}
}

// noteUsage records the token count the upstream reported.  Completion tokens
// are preferred because they are what the throughput figure is about; a usage
// block that only carries totals still yields a number.
func (s *chatStat) noteUsage(u *core.Usage) {
	if s == nil || u == nil {
		return
	}
	switch {
	case u.CompletionTokens > 0:
		s.toks = u.CompletionTokens
	case u.TotalTokens > 0:
		s.toks = u.TotalTokens
	default:
		s.toks = 0
	}
}

// done writes the row exactly once.  Every path out of a request calls it
// through a defer, and several of them can run, so the guard is load-bearing.
func (s *chatStat) done() {
	if s == nil || s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks)
}

// logChatRow formats and emits one row.
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)

	// An empty name becomes a dash before padding: padding alone would render
	// the field as blank, which reads as a formatting fault rather than as
	// "the request never named a model".
	if strings.TrimSpace(model) == "" {
		model = "-"
	}
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)

	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1ftok/s", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0tok/s"
		}
	}

	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}

	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %s | total=%.1fs |\n",
		seq, time.Now().Format("15:04:05"), model, mode, status, acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth), logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(tokpsField, chatRateWidth), total.Seconds())
}
