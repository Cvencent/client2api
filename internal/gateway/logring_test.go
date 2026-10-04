package gateway

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLogRingKeepsOnlyNewestN pins the bound: the ring is a debugging tail,
// not an audit log, so old lines must be evicted rather than accumulated.
func TestLogRingKeepsOnlyNewestN(t *testing.T) {
	r := NewLogRing(5)
	for i := 0; i < 10; i++ {
		r.Add(fmt.Sprintf("line-%d", i))
	}

	got := r.Lines(0)
	if len(got) != 5 {
		t.Fatalf("retained %d lines, want 5: %q", len(got), got)
	}
	for i, line := range got {
		want := fmt.Sprintf("line-%d", i+5)
		if !strings.Contains(line, want) {
			t.Errorf("lines[%d] = %q, want it to contain %q", i, line, want)
		}
	}
	if r.Total() != 10 {
		t.Errorf("Total() = %d, want 10 (evicted lines still counted)", r.Total())
	}
	if r.Capacity() != 5 {
		t.Errorf("Capacity() = %d, want 5", r.Capacity())
	}
}

func TestLogRingLimitSelectsNewest(t *testing.T) {
	r := NewLogRing(10)
	for i := 0; i < 6; i++ {
		r.Add(fmt.Sprintf("line-%d", i))
	}

	got := r.Lines(2)
	if len(got) != 2 {
		t.Fatalf("Lines(2) returned %d lines: %q", len(got), got)
	}
	if !strings.Contains(got[0], "line-4") || !strings.Contains(got[1], "line-5") {
		t.Errorf("Lines(2) = %q, want the two newest", got)
	}
}

// TestLogRingRedactsSecrets is the load-bearing one: upstream error bodies
// routinely carry credentials, and these lines are served to a browser.
func TestLogRingRedactsSecrets(t *testing.T) {
	const (
		jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	)
	r := NewLogRing(10)
	r.Add("upstream said: Authorization: Bearer " + jwt)
	r.Add("config dump api_key=deadbeefdeadbeef")

	for _, line := range r.Lines(0) {
		if strings.Contains(line, jwt) {
			t.Errorf("stored line leaks the JWT: %q", line)
		}
		if strings.Contains(line, "deadbeefdeadbeef") {
			t.Errorf("stored line leaks the api key: %q", line)
		}
	}
	if lines := r.Lines(0); !strings.Contains(lines[0], "<redacted") {
		t.Errorf("first line was not redacted at all: %q", lines[0])
	}
}

func TestLogRingStampsAndFlattens(t *testing.T) {
	r := NewLogRing(10)
	r.Add("no timestamp here")
	r.Add("2026-01-02T15:04:05Z already stamped")
	r.Add("first half\nsecond half")

	lines := r.Lines(0)
	if len(lines) != 3 {
		t.Fatalf("stored %d lines, want 3: %q", len(lines), lines)
	}

	first := lines[0]
	if _, err := time.Parse(time.RFC3339, strings.Fields(first)[0]); err != nil {
		t.Errorf("line without a timestamp was not stamped: %q (%v)", first, err)
	}

	second := lines[1]
	if !strings.HasPrefix(second, "2026-01-02T15:04:05Z ") {
		t.Errorf("already-stamped line was re-stamped: %q", second)
	}
	if strings.Count(second, "2026-01-02T15:04:05Z") != 1 {
		t.Errorf("already-stamped line carries two timestamps: %q", second)
	}

	third := lines[2]
	if strings.ContainsAny(strings.TrimPrefix(third, third[:21]), "\r\n") {
		t.Errorf("embedded newline survived: %q", third)
	}
	if !strings.Contains(third, "first half second half") {
		t.Errorf("newline was not replaced with a space: %q", third)
	}
}

// TestLogRingReplacesStdlibStamp checks that a line written by a log.Logger
// (which already carries the standard library's "2006/01/02 15:04:05" prefix)
// ends up with exactly one timestamp — the ring's RFC3339 one.  Two clocks in
// one panel row read like a bug.
func TestLogRingReplacesStdlibStamp(t *testing.T) {
	r := NewLogRing(10)
	r.Add("2026/01/02 15:04:05 alias glm-5.3 -> zcode/GLM-5.3")
	r.Add("2026/01/02 15:04:05.123456 clients: 6 loaded, 0 skipped, registered=6")

	lines := r.Lines(0)
	if len(lines) != 2 {
		t.Fatalf("stored %d lines, want 2: %q", len(lines), lines)
	}
	for i, line := range lines {
		if strings.Contains(line, "2026/01/02 15:04:05") {
			t.Errorf("line %d kept the stdlib timestamp: %q", i, line)
		}
		if _, err := time.Parse(time.RFC3339, strings.Fields(line)[0]); err != nil {
			t.Errorf("line %d has no RFC3339 stamp: %q (%v)", i, line, err)
		}
	}
	if !strings.Contains(lines[0], "alias glm-5.3 -> zcode/GLM-5.3") {
		t.Errorf("message body was lost: %q", lines[0])
	}
	if !strings.Contains(lines[1], "clients: 6 loaded") {
		t.Errorf("microsecond variant left residue: %q", lines[1])
	}
}

// TestStripGoLogStamp only strips a real leading timestamp.
func TestStripGoLogStamp(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026/01/02 15:04:05 hello", "hello"},
		{"2026/01/02 15:04:05.123456 hello", "hello"},
		{"hello 2026/01/02 15:04:05", "hello 2026/01/02 15:04:05"},
		{"2026/01/02 15:04:05", "2026/01/02 15:04:05"},
		{"2026/99/02 15:04:05 not a date", "2026/99/02 15:04:05 not a date"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripGoLogStamp(c.in); got != c.want {
			t.Errorf("stripGoLogStamp(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLogRingWriteSplitsLines checks the io.Writer contract: a multi-line
// payload becomes several entries and Write reports the full length.
func TestLogRingWriteSplitsLines(t *testing.T) {
	r := NewLogRing(10)
	n, err := r.Write([]byte("alpha\nbeta\n\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("alpha\nbeta\n\n") {
		t.Errorf("Write returned %d, want %d", n, len("alpha\nbeta\n\n"))
	}
	lines := r.Lines(0)
	if len(lines) != 2 {
		t.Fatalf("stored %d lines, want 2 (blank lines skipped): %q", len(lines), lines)
	}
}

// TestLogRingConcurrent exercises the non-blocking path under -race.  Lines may
// be dropped, but nothing may race, panic, or deadlock.
func TestLogRingConcurrent(t *testing.T) {
	r := NewLogRing(64)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, _ = r.Write([]byte(fmt.Sprintf("goroutine %d line %d", g, i)))
			}
		}(g)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent writes deadlocked")
	}

	if got := len(r.Lines(0)); got > 64 {
		t.Errorf("retained %d lines, more than capacity", got)
	}
	if r.Total() == 0 {
		t.Error("no lines stored at all")
	}
}

func TestLogRingNilSafe(t *testing.T) {
	var r *LogRing
	if r.Total() != 0 || r.Dropped() != 0 {
		t.Error("nil ring must report zero counters")
	}
}

// TestClassifyLine pins the channel vocabulary.  The channels exist because a
// busy gateway's chat rows would otherwise flush every task result out of the
// ring, so a line landing in the wrong channel is the same as losing it.
func TestClassifyLine(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"chat row", "| #001 | zcode | glm-4.7 | ok |", ChChat},
		{"chat row behind the stdlib stamp", "2026/01/02 15:04:05 | #007 | kimi | ok |", ChChat},
		{"chat row behind an rfc3339 stamp", "2026-01-02T15:04:05Z | #007 | kimi | ok |", ChChat},
		{"checkin task", "checkin uid-cn-0001: credited", ChTask},
		{"checkin behind a stamp", "2026/01/02 15:04:05 checkin uid-cn-0001: credited", ChTask},
		{"panel task verb", "panel: 任务 一键签到 完成", ChTask},
		{"panel queue verb", "panel: 队列 3 项", ChTask},
		{"streak bonus", "streak-bonus uid-cn-0001: redeemed 2 tiers", ChTask},
		{"travel", "travel uid-cn-0001: departed", ChTask},
		{"startup line", "clients: 7 loaded, 0 skipped, registered=7", ChSys},
		{"error line", "chat: client zcode: upstream 502", ChSys},
		{"empty", "", ChSys},
		// A word that merely starts with a task prefix must not match: the
		// prefix table carries its own trailing space for exactly this reason.
		{"near miss", "checking something unrelated", ChSys},
	}
	for _, c := range cases {
		if got := classifyLine(c.in); got != c.want {
			t.Errorf("classifyLine(%q) [%s] = %q, want %q", c.in, c.name, got, c.want)
		}
	}
}

// TestLogRingEntriesCarryChannelAndTime checks the stored shape the panel
// filters on, and that the timestamp is the line's own rather than the moment
// of insertion.
func TestLogRingEntriesCarryChannelAndTime(t *testing.T) {
	r := NewLogRing(10)
	r.Add("2026-01-02T15:04:05Z | #001 | zcode | glm-4.7 | ok |")
	r.Add("checkin uid-cn-0001: credited")
	r.Add("clients: 7 loaded")

	got := r.Entries(0)
	if len(got) != 3 {
		t.Fatalf("stored %d entries, want 3", len(got))
	}
	wantCh := []string{ChChat, ChTask, ChSys}
	for i, e := range got {
		if e.Ch != wantCh[i] {
			t.Errorf("entries[%d].Ch = %q, want %q (text %q)", i, e.Ch, wantCh[i], e.Text)
		}
		if e.TS.IsZero() {
			t.Errorf("entries[%d].TS is zero", i)
		}
	}
	if got[0].TS.UTC().Format(time.RFC3339) != "2026-01-02T15:04:05Z" {
		t.Errorf("entries[0].TS = %s, want the line's own stamp", got[0].TS.UTC().Format(time.RFC3339))
	}
	if !strings.Contains(got[1].Text, "credited") {
		t.Errorf("entries[1].Text = %q, want the message body", got[1].Text)
	}
}

// TestLogRingLinesProjectsEntries keeps the two accessors from drifting: Lines
// is what the older callers read, Entries is what the panel filters.
func TestLogRingLinesProjectsEntries(t *testing.T) {
	r := NewLogRing(10)
	r.Add("checkin a")
	r.Add("| #001 | kimi | ok |")
	r.Add("clients: 1 loaded")

	entries := r.Entries(2)
	lines := r.Lines(2)
	if len(lines) != len(entries) {
		t.Fatalf("Lines(2) returned %d, Entries(2) returned %d", len(lines), len(entries))
	}
	for i := range entries {
		if lines[i] != entries[i].Text {
			t.Errorf("Lines(%d) = %q, Entries(%d).Text = %q", i, lines[i], i, entries[i].Text)
		}
	}
	if len(r.Lines(0)) != 3 {
		t.Errorf("Lines(0) = %d, want every retained entry", len(r.Lines(0)))
	}
}
