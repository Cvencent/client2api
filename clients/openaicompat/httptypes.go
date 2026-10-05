package openaicompat

import "net/http"

// httpResponseAlias is the concrete type the stream holds.  The alias keeps
// the import surface of the other files small.
type httpResponseAlias = http.Response
