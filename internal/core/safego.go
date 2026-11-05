package core

import (
	"fmt"
	"os"
	"runtime/debug"
)

// goSafeFallback is where a recovered panic goes when the caller supplied no
// report function.  It is a variable only so a test can capture it.
//
// stderr alone is not a sink in the release build (issue #209): the installer
// and CI both link with -H=windowsgui, which starts the process with INVALID
// standard handles -- Fprint(os.Stderr) writes nowhere, so a recovered panic
// in one of the nil-report goroutines left zero evidence on disk.  main()
// injects the real logger through SetPanicSink; stderr stays in the list as
// one entry rather than the only one.
var goSafeFallback = func(msg string) { fmt.Fprint(os.Stderr, msg) }

// panicSink is the extra reporter main() installs at boot.
var panicSink func(string)

// SetPanicSink wires a logger into the nil-report fallback path (issue #209).
// Call it once at boot, after the log file exists.  The fallback still writes
// stderr first: from a console it is free, and from windowsgui it is a no-op
// that cannot break the sink below.
func SetPanicSink(sink func(string)) {
	panicSink = sink
}

// GoSafe runs fn on its own goroutine, turning a panic into a report instead of
// letting it take the whole process down.
//
// net/http recovers a panic only inside the handler goroutine net/http itself
// started.  Everything the gateway spawns around a request -- the SSE pumps,
// the pool writers, the batch workers -- and every long-lived loop (the usage
// flush, the affinity sweep, the scheduler) runs outside that protection, so a
// single panic in any of them kills the process and every request in flight
// with it.  Wrapping a goroutine in GoSafe makes the blast radius that one
// goroutine instead of the whole gateway.
//
// report may be nil, in which case the trace goes to stderr -- the same place
// an unrecovered panic would have printed it, minus the process exit.  report
// runs on the panicking goroutine, so it must not itself panic, and it is the
// caller's only chance to say which goroutine died.
//
// GoSafe is Recover on a new goroutine; use Recover directly when the panic
// must not cost the caller's own goroutine.
func GoSafe(what string, report func(string), fn func()) {
	go Recover(what, report, fn)
}

// Recover is GoSafe without the goroutine: it runs fn inline and turns a panic
// into a report, so the caller keeps its own goroutine -- and its own ordering.
//
// Reach for it when the panic must not cost the surrounding work.  A queue
// worker that calls vendor code inside its loop, for instance, has to survive a
// panic in that call: GoSafe would recover the panic but the worker itself would
// still be gone, leaving the queue permanently busy.  Recover keeps the loop
// running and the report still names what died.
func Recover(what string, report func(string), fn func()) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		msg := fmt.Sprintf("panic in %s: %v\n%s", what, r, debug.Stack())
		if report == nil {
			goSafeFallback(msg)
			if panicSink != nil {
				panicSink(msg)
			}
			return
		}
		report(msg)
	}()
	fn()
}
