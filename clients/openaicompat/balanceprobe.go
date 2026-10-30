package openaicompat

import "time"

// balanceProbeState is one relay's negative-cache entry for the "auto" balance
// protocol.
//
// A relay that exposes no billing surface answers all four probes with a 404 or
// with an HTML page where JSON was expected.  Without a memory of that, every
// five-minute sweep repeats four failed requests per platform and fills the log
// with errors that describe the relay's feature set rather than anything the
// operator can fix.  `until` is when the relay is worth asking again.
type balanceProbeState struct {
	until time.Time
}
