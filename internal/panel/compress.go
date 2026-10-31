package panel

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// The panel's two biggest payloads -- the shell HTML and the two vendored
// libraries -- are text that gzip shrinks by ~74%.  Browsers always offer
// Accept-Encoding: gzip, and Go's net/http never compresses on its own, so
// every page load used to move the full 1.96 MB (issue #14).
//
// The vendored files are compiled into the binary, so they are compressed once
// at startup and reused; the shell is compressed on demand (it is rewritten by
// the build, and per-request compression of ~1.2 MB is still cheap compared to
// shipping it raw).

var (
	vendorGzOnce sync.Once
	threeGZ      []byte
	motionGZ     []byte
)

// gzipVendorOnce pre-compresses the vendored libraries into memory.
func gzipVendorOnce() {
	vendorGzOnce.Do(func() {
		threeGZ = gzipBytes(threeJS)
		motionGZ = gzipBytes(motionJS)
	})
}

// gzipBytes compresses b once, at the default level.
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// acceptsGzip reports whether the client offered gzip in Accept-Encoding.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.HasPrefix(strings.TrimSpace(strings.ToLower(part)), "gzip") {
			return true
		}
	}
	return false
}

// writeGzip sends body to the client, gzipped when the client asked for it.
// The Content-Encoding and Vary headers are only set on the compressed path;
// callers set Content-Type and cache headers themselves.
func writeGzip(w http.ResponseWriter, r *http.Request, body []byte) {
	if !acceptsGzip(r) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
		return
	}
	gz := gzipBytes(body)
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
	_, _ = w.Write(gz)
}

// etagMatches reports whether an If-None-Match header endorses etag.  The
// shell only ever sends one etag, so a plain substring check over the comma
// list covers both the exact form and the W/ weak-prefix form browsers may
// send back.  "*" also matches: the client says it has some copy and is
// willing to revalidate, and a 304 is the cheapest honest answer.
func etagMatches(ifNoneMatch, etag string) bool {
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, part := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimSpace(part) == etag {
			return true
		}
		if strings.TrimSpace(part) == "W/"+etag {
			return true
		}
	}
	return false
}
