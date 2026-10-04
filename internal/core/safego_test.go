package core

import (
	"strings"
	"testing"
	"time"
)

// TestGoSafeReportsAPanicInsteadOfDying pins the whole point of the helper: a
// panicking goroutine must hand its value and stack to the report function
// rather than bringing the process down.
func TestGoSafeReportsAPanicInsteadOfDying(t *testing.T) {
	got := make(chan string, 1)
	GoSafe("the widget pump", func(msg string) { got <- msg }, func() {
		panic("boom")
	})

	select {
	case msg := <-got:
		if !strings.Contains(msg, "boom") {
			t.Errorf("report = %q, want it to carry the panic value", msg)
		}
		if !strings.Contains(msg, "the widget pump") {
			t.Errorf("report = %q, want it to name the goroutine", msg)
		}
		if !strings.Contains(msg, "goroutine") {
			t.Errorf("report = %q, want it to carry a stack trace", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a panic was swallowed without reaching the report function")
	}
}

// TestGoSafeRunsTheFunction proves the wrapper is transparent on the happy
// path: fn still runs, and report is never called.
func TestGoSafeRunsTheFunction(t *testing.T) {
	ran := make(chan struct{})
	reported := make(chan string, 1)
	GoSafe("no-op", func(msg string) { reported <- msg }, func() { close(ran) })

	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("GoSafe never ran fn")
	}
	select {
	case msg := <-reported:
		t.Errorf("report was called for a clean run: %q", msg)
	default:
	}
}

// TestGoSafeWithoutAReportFallsBackToStderr covers the nil-report path, which
// every caller that has no logger uses.  A panic there must still be visible
// rather than silently dropped.
func TestGoSafeWithoutAReportFallsBackToStderr(t *testing.T) {
	old := goSafeFallback
	defer func() { goSafeFallback = old }()

	got := make(chan string, 1)
	goSafeFallback = func(msg string) { got <- msg }

	GoSafe("no logger here", nil, func() { panic("dropped?") })

	select {
	case msg := <-got:
		if !strings.Contains(msg, "dropped?") {
			t.Errorf("fallback = %q, want it to carry the panic value", msg)
		}
		if !strings.Contains(msg, "no logger here") {
			t.Errorf("fallback = %q, want it to name the goroutine", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a panic with no report function was dropped silently")
	}
}

// TestRecoverKeepsTheCallersGoroutine is the difference between Recover and
// GoSafe: after the panic is reported, the code that follows the call still
// runs on the same goroutine.  A queue worker relies on exactly that -- if the
// panic unwound out of the loop the worker would be gone and the queue would
// stay busy forever.
func TestRecoverKeepsTheCallersGoroutine(t *testing.T) {
	var reported string
	Recover("the sweep", func(msg string) { reported = msg }, func() { panic("vendor code") })

	if !strings.Contains(reported, "vendor code") {
		t.Errorf("report = %q, want it to carry the panic value", reported)
	}
	if !strings.Contains(reported, "the sweep") {
		t.Errorf("report = %q, want it to name the work", reported)
	}
	// Reaching this line at all is the proof: GoSafe would have moved fn to
	// another goroutine, and an unrecovered panic would have unwound past here.
}

// TestRecoverOnTheHappyPathNeverCallsTheReport is the transparency half.
func TestRecoverOnTheHappyPathNeverCallsTheReport(t *testing.T) {
	ran := false
	Recover("no-op", func(msg string) { t.Errorf("report was called for a clean run: %q", msg) }, func() { ran = true })
	if !ran {
		t.Fatal("Recover never ran fn")
	}
}

// TestRecoverWithoutAReportFallsBackToStderr covers the nil-report path.
func TestRecoverWithoutAReportFallsBackToStderr(t *testing.T) {
	old := goSafeFallback
	defer func() { goSafeFallback = old }()

	var got string
	goSafeFallback = func(msg string) { got = msg }

	Recover("no logger here", nil, func() { panic("dropped?") })

	if !strings.Contains(got, "dropped?") {
		t.Errorf("fallback = %q, want it to carry the panic value", got)
	}
}

// TestRecoverRunsFnsOwnDefersBeforeTheReport pins the ordering GoSafe's callers
// depend on: fn's deferred cleanup runs during the unwind, and only then does
// the report fire.
func TestRecoverRunsFnsOwnDefersBeforeTheReport(t *testing.T) {
	var order []string
	Recover("ordered", func(msg string) { order = append(order, "report") }, func() {
		defer func() { order = append(order, "defer") }()
		panic("boom")
	})

	if len(order) != 2 || order[0] != "defer" || order[1] != "report" {
		t.Fatalf("order = %v, want [defer report]", order)
	}
}

// TestGoSafeKeepsTheProcessAliveAfterAPanic is the end-to-end claim: after a
// recovered panic, the test binary is still running and other goroutines still
// make progress.
func TestGoSafeKeepsTheProcessAliveAfterAPanic(t *testing.T) {
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		GoSafe("doomed", nil, func() {
			defer func() { done <- struct{}{} }()
			panic("each of these used to be a process exit")
		})
	}
	deadline := time.After(10 * time.Second)
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-deadline:
			t.Fatalf("only %d of 4 goroutines got past their panic", i)
		}
	}
}
