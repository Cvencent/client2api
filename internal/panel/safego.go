package panel

import "client2api/internal/core"

// safeGo runs one panel worker under core.GoSafe.
//
// The panel's fan-outs -- one goroutine per account, per module, per queue
// worker -- all run outside net/http's own panic recovery, so a single vendor
// payload that panics a module would otherwise take the whole gateway down and
// drop every request in flight with it.  GoSafe keeps the blast radius to the
// one worker.
//
// A worker's own deferred cleanup (wg.Done, a semaphore release, a lock
// release) still runs during the unwind, so a recovered panic cannot wedge the
// wg.Wait that is collecting the results.
func (p *panel) safeGo(what string, fn func()) {
	core.GoSafe(what, p.panicReport(), fn)
}

// safeGoThen is safeGo for a worker whose crash leaves state behind that the
// panel would otherwise keep reporting as still in progress -- a task run stuck
// on "running" forever.  then runs after the trace has been recorded, and must
// not itself panic (see core.GoSafe).
func (p *panel) safeGoThen(what string, then func(string), fn func()) {
	report := p.panicReport()
	core.GoSafe(what, func(msg string) {
		if report != nil {
			report(msg)
		}
		then(msg)
	}, fn)
}

// panicReport records a recovered panic where an operator will look: the log
// ring the dashboard reads.  A nil return leaves core.GoSafe writing to stderr,
// which is what a panel built without instrumentation gets.
func (p *panel) panicReport() func(string) {
	if p.opts.Logs == nil {
		return nil
	}
	return func(msg string) { p.opts.Logs.Add(msg) }
}
