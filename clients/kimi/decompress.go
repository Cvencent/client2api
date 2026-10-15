package kimi

// decompress.go adds the one transport behaviour the CLI's Accept-Encoding
// forces on us.  The official CLI sends
//
//	accept-encoding: br, gzip, deflate
//
// on every vendor call.  net/http does NOT decompress a response for you once
// you set Accept-Encoding yourself -- it only auto-handles the gzip it
// implicitly advertised.  So the moment this module stopped lying about its
// Accept-Encoding, it took on the job of decoding whatever the vendor returns,
// or every chat would receive compressed bytes where it expects JSON/SSE.

import (
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

// decompressTransport wraps the chosen client's transport so a response
// carrying Content-Encoding: br | gzip | deflate is decoded before any caller
// reads it.  It is applied around whichever client httpClient() selected, so
// vendorClient, Deps.HTTPClient and the default all get the same treatment.
type decompressTransport struct {
	base http.RoundTripper
}

func (t decompressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		return resp, nil
	}
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if encoding == "" || encoding == "identity" {
		return resp, nil
	}
	body, derr := decodeBody(resp.Body, encoding)
	if derr != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("kimi: decoding %s response: %w", encoding, derr)
	}
	resp.Body = body
	// The caller must not see the stale headers: the body is now plaintext and
	// its length is unknown.
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

// decodeBody picks the decoder named by the response's Content-Encoding.  For
// "deflate" it accepts both the zlib framing the HTTP spec mandates and the
// raw deflate stream some servers send instead; the CLI's Node client accepts
// both, so refusing one would be a regression against the real client.
func decodeBody(body io.ReadCloser, encoding string) (io.ReadCloser, error) {
	switch encoding {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, err
		}
		return &compoundReadCloser{Reader: zr, closers: []io.Closer{zr, body}}, nil
	case "br":
		return &compoundReadCloser{Reader: brotli.NewReader(body), closers: []io.Closer{body}}, nil
	case "deflate":
		// Try zlib first.  zlib.NewReader consumes a couple of bytes, so a
		// failed attempt has to be replayed through a buffering wrapper before
		// the raw-deflate fallback can read the same bytes.
		br := newBufferedReader(body)
		zr, err := zlib.NewReader(br)
		if err == nil {
			return &compoundReadCloser{Reader: zr, closers: []io.Closer{zr, body}}, nil
		}
		br.rewind()
		return &compoundReadCloser{
			Reader:  flate.NewReader(br),
			closers: []io.Closer{body},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", encoding)
	}
}

// compoundReadCloser closes the decoder(s) and then the underlying body exactly
// once, which is what keeps a streamed connection reusable.
type compoundReadCloser struct {
	io.Reader
	closers []io.Closer
	closed  bool
}

func (c *compoundReadCloser) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	var firstErr error
	for _, closer := range c.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// bufferedReader replays every byte it has already handed out.  decodeBody
// needs it because zlib.NewReader consumes the stream header before it can
// decide whether the framing is zlib or raw deflate; the raw fallback must see
// the same bytes from the beginning again.
type bufferedReader struct {
	src    io.Reader
	buf    []byte
	offset int
}

func newBufferedReader(src io.Reader) *bufferedReader {
	return &bufferedReader{src: src}
}

func (b *bufferedReader) Read(p []byte) (int, error) {
	if b.offset < len(b.buf) {
		n := copy(p, b.buf[b.offset:])
		b.offset += n
		return n, nil
	}
	n, err := b.src.Read(p)
	if n > 0 {
		b.buf = append(b.buf, p[:n]...)
		b.offset += n
	}
	return n, err
}

// rewind resets the read cursor to the start so the raw-deflate decoder reads
// the header bytes zlib already consumed.
func (b *bufferedReader) rewind() { b.offset = 0 }
