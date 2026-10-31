package main

import (
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// newCaptureLogger returns a *log.Logger that appends whole lines to dst.
func newCaptureLogger(dst *[]string) *log.Logger {
	return log.New(writerFunc(func(p []byte) (int, error) {
		*dst = append(*dst, strings.TrimRight(string(p), "\n"))
		return len(p), nil
	}), "", 0)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// The live log had one platform answering "User has been banned" 102 times in
// half a day.  The sweep is what produces that drumbeat, so the sweep is what
// has to collapse it.
func TestProbeLogDeduperCollapsesRepeatedFailures(t *testing.T) {
	d := newProbeLogDeduper(time.Hour)
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	err := errors.New("HTTP 403: User has been banned")

	out, log := d.shouldLog("bcsn", "key-1", err)
	if !log {
		t.Fatal("the first failure must be logged")
	}
	if !strings.Contains(out, "banned") {
		t.Fatalf("first line = %q, want the vendor's message", out)
	}

	// Eleven more sweeps inside the window: all silent.
	for i := 0; i < 11; i++ {
		now = now.Add(5 * time.Minute)
		if out, log := d.shouldLog("bcsn", "key-1", err); log {
			t.Fatalf("sweep %d logged %q, want it suppressed", i+2, out)
		}
	}

	// Past the window the failure returns with its repeat count, so an operator
	// can tell it stood for twelve sweeps rather than one.
	now = now.Add(time.Hour)
	out, log = d.shouldLog("bcsn", "key-1", err)
	if !log {
		t.Fatal("a failure must be reported again once the window lapses")
	}
	if !strings.Contains(out, "12×") {
		t.Fatalf("line = %q, want it to name the 12 suppressed repeats", out)
	}
	if !strings.Contains(out, "repeated") {
		t.Fatalf("line = %q, want a repeat marker", out)
	}
}

// A different error on the same account is news even inside the window: the
// state changed, and hiding that would make the deduper lie.
func TestProbeLogDeduperReportsAChangedError(t *testing.T) {
	d := newProbeLogDeduper(time.Hour)
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }

	if _, log := d.shouldLog("relay", "key-1", errors.New("HTTP 404: not found")); !log {
		t.Fatal("first failure must be logged")
	}
	now = now.Add(time.Minute)
	out, log := d.shouldLog("relay", "key-1", errors.New("HTTP 401: invalid api key"))
	if !log {
		t.Fatal("a changed error must be logged inside the window")
	}
	if !strings.Contains(out, "invalid api key") {
		t.Fatalf("line = %q, want the new error", out)
	}
}

// The same error on two different accounts is two facts, not one repeat: one
// dead key is an operator errand, two is a broken platform.
func TestProbeLogDeduperKeepsAccountsApart(t *testing.T) {
	d := newProbeLogDeduper(time.Hour)
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	err := errors.New("HTTP 401: invalid api key")

	if _, log := d.shouldLog("relay", "key-1", err); !log {
		t.Fatal("the first account must be logged")
	}
	if _, log := d.shouldLog("relay", "key-2", err); !log {
		t.Fatal("a second account failing the same way is a separate fact")
	}
}

// A vendor request id changes on every request and says nothing, so it must not
// defeat the deduper: without this, a banned key's identical lines would all
// look distinct.
func TestProbeStableTextRemovesRequestIDs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "workbuddy requestId field",
			in:   `{"code":11129,"msg":"invalid function call parameters","requestId":"ec3ce62f0be9ae6f4f3b901ff32e4d31"}`,
			want: `{"code":11129,"msg":"invalid function call parameters","requestId":"<request-id>"}`,
		},
		{
			name: "lingxiai request id prose",
			in:   "No permission to access group Gemini (request id: 202610290425053286251388268d9d6U5ljkcX1)",
			want: "No permission to access group Gemini (request id: <request-id>)",
		},
		{
			name: "a bare long hex token",
			in:   "HTTP 403: User has been banned 202610290244119832392218268d9d6GNxF70CZ",
			want: "HTTP 403: User has been banned <request-id>",
		},
		{
			name: "a short hex fragment is left alone",
			in:   "offset 0x1f is out of range",
			want: "offset 0x1f is out of range",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeStableText(tc.in); got != tc.want {
				t.Fatalf("probeStableText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The sweep still has to be visible: a run that silenced twenty lines says so
// once, per platform, instead of saying nothing.
func TestProbeLogDeduperSummarisesSilencedRepeats(t *testing.T) {
	d := newProbeLogDeduper(time.Hour)
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	err := errors.New("HTTP 404: not found")

	var lines []string
	logger := newCaptureLogger(&lines)
	for i := 0; i < 6; i++ {
		d.shouldLog("relay", "key-1", err)
	}
	d.reportOnReturn(logger)
	if len(lines) != 1 {
		t.Fatalf("summary lines = %d (%v), want exactly one", len(lines), lines)
	}
	if !strings.Contains(lines[0], "relay×5") {
		t.Fatalf("summary = %q, want it to name the five suppressed repeats", lines[0])
	}

	// A second report inside the same window has nothing new to say.
	lines = nil
	d.reportOnReturn(logger)
	if len(lines) != 0 {
		t.Fatalf("second report = %v, want silence", lines)
	}
}

// probeLogDeduper must survive concurrent sweeps: the scheduled and the manual
// one can overlap.
func TestProbeLogDeduperIsSafeForConcurrentSweeps(t *testing.T) {
	d := newProbeLogDeduper(time.Hour)
	err := errors.New("HTTP 404: not found")
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				d.shouldLog("relay", "key-1", err)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// The deduper the sweeps actually share must survive from one sweep to the
// next: a fresh map per call made the one-hour window fictional, and a dead
// credential printed the same 401 every five minutes for days (issue #7).
func TestSharedProbeLogDeduperSurvivesAcrossSweeps(t *testing.T) {
	// Not a fresh instance on purpose: the singleton is the subject here.
	d := sharedProbeLogDeduper()

	// Freeze time so the test does not depend on the wall clock.
	orig := d.now
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	t.Cleanup(func() { d.now = orig })

	err := errors.New("HTTP 401: The API Key appears to be invalid or may have expired")
	if _, log := d.shouldLog("kimi", "key-1", err); !log {
		t.Fatal("the first failure must be logged")
	}
	// The next sweep, five minutes later: the same failure through the same
	// shared instance must be silent now that the map survives.
	now = now.Add(5 * time.Minute)
	if _, log := d.shouldLog("kimi", "key-1", err); log {
		t.Fatal("the shared deduper forgot the failure between sweeps: the drumbeat is back")
	}
}
