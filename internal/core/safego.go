package core

import (
	"fmt"
	"os"
	"runtime/debug"
)

// goSafeFallback is where a recovered panic goes when the caller supplied no
// report function.  It is a variable only so a test can capture it.
var goSafeFallback = func(msg string) { fmt.Fprint(os.Stderr, msg) }

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
			return
		}
		report(msg)
	}()
	fn()
}
