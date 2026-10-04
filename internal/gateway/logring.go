package gateway

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"client2api/internal/core"
)

// DefaultLogCapacity is how many recent log lines the gateway keeps in memory
// for the panel.  It is a debugging aid for the dashboard, not an audit log:
// older lines are dropped, and nothing is written to disk.
const DefaultLogCapacity = 2000

// Log channels.  Everything the process logs goes into one stream, and a busy
// gateway is dominated by chat traffic: a single conversation can push a
// scheduled task's result out of the ring before an operator ever looks.  The
// channel is what lets the panel show one kind at a time.
const (
	// ChChat marks a per-request chat table row.
	ChChat = "chat"
	// ChTask marks a scheduled or panel-triggered task.
	ChTask = "task"
	// ChSys marks everything else: startup, config, upstream errors.
	ChSys = "sys"
)

// LogEntry is one retained line with the channel it belongs to and the moment
// it was stored.
type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// taskPrefixes are the leading words of lines emitted by the scheduler and the
// task centre.  They are matched against the message with any leading
// timestamp removed.
var taskPrefixes = []string{
	"school ", "streak-bonus ", "travel ", "blackcat ", "lottery ",
	"checkin ", "activity ", "keepalive ", "balance ", "user-resource ",
	"panel: 任务", "panel: 一键", "panel: checkin", "panel: 手动",
	"panel: 队列", "panel: 券码",
}

// classifyLine sorts a stored line into a channel.
//
// A chat row is recognised by its leading "| #", the shape internal/gateway's
// logChatRow writes; task lines are recognised by the vocabulary the scheduler
// and the panel use.  Everything else is system output, which is the right
// default: an unrecognised line is more useful in the unfiltered bucket than
// misfiled under a task.
func classifyLine(line string) string {
	if s := stripGoLogStamp(line); s != line {
		line = s
	} else if _, ok := leadingStamp(line); ok {
		_, line, _ = strings.Cut(line, " ")
	}
	if strings.HasPrefix(line, "| #") {
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// LogRing is a bounded, concurrency-safe ring of recent log lines.
//
// It is an io.Writer so that cmd/client2api can tee a log.Logger into it: the
// operator still sees the log on stdout, and the panel gets the tail of the
// same stream.  Two properties matter:
//
//   - Write never blocks.  A chat request must never stall because the
//     dashboard is slow to drain the buffer, so a contended buffer drops the
//     line (and counts it) instead of waiting for the lock.
//   - Nothing is stored unredacted.  Upstream error bodies routinely carry
//     credentials, and this buffer is served to a browser, so every line goes
//     through core.Redact before it is kept.
type LogRing struct {
	capacity int

	mu    sync.Mutex
	buf   []LogEntry // ring storage, exactly capacity long
	start int        // index of the oldest retained entry
	count int        // number of retained entries (<= capacity)

	total   atomic.Int64
	dropped atomic.Int64
}

// NewLogRing returns a ring holding at most capacity lines.  A non-positive
// capacity selects DefaultLogCapacity.
func NewLogRing(capacity int) *LogRing {
	if capacity <= 0 {
		capacity = DefaultLogCapacity
	}
	return &LogRing{capacity: capacity, buf: make([]LogEntry, capacity)}
}

// Write implements io.Writer.  A single Write may carry several physical lines
// (a multi-line message, or a logger flushing a batch); each one is stored
// separately.  It always reports success, because a dropped debug line must
// never turn into a logging error.
func (r *LogRing) Write(p []byte) (int, error) {
	r.store(string(p))
	return len(p), nil
}

// store splits a payload into physical lines and keeps each non-empty one.
func (r *LogRing) store(payload string) {
	for _, raw := range strings.Split(payload, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		r.Add(line)
	}
}

// Add stores one line, stamping it with an RFC3339 timestamp when it does not
// already start with one and replacing embedded newlines with spaces so that
// one entry is always one physical line.
//
// It reports false when the buffer was busy and the line was dropped; callers
// are free to ignore that, and the count is available through Dropped.
func (r *LogRing) Add(line string) bool {
	text := core.Redact(stampLine(flatten(line)))
	ts, ok := leadingStamp(text)
	if !ok {
		// stampLine guarantees a leading stamp, so this is unreachable for the
		// text it returns; keeping the fallback means a future change to
		// stampLine cannot produce entries with a zero time.
		ts = time.Now().UTC()
	}
	entry := LogEntry{TS: ts, Ch: classifyLine(text), Text: text}
	if !r.mu.TryLock() {
		// Deliberately no waiting: dropping a line beats stalling a request.
		r.dropped.Add(1)
		return false
	}
	defer r.mu.Unlock()

	if r.count == r.capacity {
		// Full: overwrite the oldest entry and advance the window.
		r.buf[r.start] = entry
		r.start = (r.start + 1) % r.capacity
	} else {
		r.buf[(r.start+r.count)%r.capacity] = entry
		r.count++
	}
	r.total.Add(1)
	return true
}

// Entries returns up to limit retained entries, oldest first and newest last.
// A non-positive limit returns everything retained.
func (r *LogRing) Entries(limit int) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := r.count
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]LogEntry, 0, n)
	for i := r.count - n; i < r.count; i++ {
		out = append(out, r.buf[(r.start+i)%r.capacity])
	}
	return out
}

// Lines returns just the text of up to limit retained lines, oldest first.
func (r *LogRing) Lines(limit int) []string {
	entries := r.Entries(limit)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Text)
	}
	return out
}

// Capacity is the maximum number of lines retained.
func (r *LogRing) Capacity() int { return r.capacity }

// Total is the number of lines the ring has stored since the process started,
// including lines already overwritten.
func (r *LogRing) Total() int64 {
	if r == nil {
		return 0
	}
	return r.total.Load()
}

// Dropped is the number of lines discarded because the buffer was busy.
func (r *LogRing) Dropped() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// flatten collapses a line onto a single physical line.
func flatten(line string) string {
	if !strings.ContainsAny(line, "\r\n") {
		return line
	}
	line = strings.ReplaceAll(line, "\r\n", " ")
	line = strings.ReplaceAll(line, "\n", " ")
	return strings.ReplaceAll(line, "\r", " ")
}

// goLogStamps are the date/time layouts the standard library's log package
// writes when LstdFlags (and Lmicroseconds) are set.  The ring is normally fed
// by a log.Logger, so a raw line already begins with one.
var goLogStamps = []string{"2006/01/02 15:04:05.000000", "2006/01/02 15:04:05"}

// stripGoLogStamp removes a leading standard-library log timestamp.  Without
// this the panel row would show two clocks — the logger's local one and the
// ring's RFC3339 one — which reads like a bug even though both are correct.
func stripGoLogStamp(line string) string {
	for _, layout := range goLogStamps {
		n := len(layout)
		if len(line) > n && line[n] == ' ' {
			if _, err := time.Parse(layout, line[:n]); err == nil {
				return line[n+1:]
			}
		}
	}
	return line
}

// stampLine replaces a leading standard-library log timestamp with an RFC3339
// one, and adds a timestamp when the line carries none.
func stampLine(line string) string {
	line = stripGoLogStamp(line)
	if hasLeadingTimestamp(line) {
		return line
	}
	return time.Now().UTC().Format(time.RFC3339) + " " + line
}

// leadingStamp parses the RFC3339 timestamp stampLine puts at the front of an
// entry, reporting whether the line has one.
func leadingStamp(line string) (time.Time, bool) {
	first, _, _ := strings.Cut(line, " ")
	if len(first) < 20 {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, first); err == nil {
		return t, true
	}
	t, err := time.Parse(time.RFC3339Nano, first)
	return t, err == nil
}

// hasLeadingTimestamp reports whether the first whitespace-delimited token of
// line is an RFC3339 timestamp.
func hasLeadingTimestamp(line string) bool {
	_, ok := leadingStamp(line)
	return ok
}
