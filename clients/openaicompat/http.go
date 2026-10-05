package openaicompat

import "net/http"

// httpRequest is the config layer's handle on an HTTP request.  It is a plain
// alias so config.go can set headers without importing net/http itself.
type httpRequest = http.Request
