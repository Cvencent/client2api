package kimi

// decompress_test.go pins the transport the module added when it stopped lying
// about Accept-Encoding.  The CLI offers br/gzip/deflate, so the module must
// decode whichever the vendor chooses; a plain gzip-only client would hand
// compressed bytes to the SSE parser.

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

func mustGzip(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func mustZlib(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

func mustRawDeflate(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

func mustBrotli(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

// TestDecompressTransportDecodesEveryAdvertisedCoding checks each coding the CLI
// offers, including the raw-deflate variant some servers send in place of zlib.
func TestDecompressTransportDecodesEveryAdvertisedCoding(t *testing.T) {
	const payload = `{"ok":true,"data":"pong"}`
	cases := []struct {
		name     string
		encoding string
		body     []byte
	}{
		{"gzip", "gzip", mustGzip(t, payload)},
		{"zlib deflate", "deflate", mustZlib(t, payload)},
		{"raw deflate", "deflate", mustRawDeflate(t, payload)},
		{"brotli", "br", mustBrotli(t, payload)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Encoding": []string{tc.encoding}, "Content-Length": []string{"999"}},
					Body:          io.NopCloser(bytes.NewReader(tc.body)),
					ContentLength: 999,
				}, nil
			})
			client := withDecompression(&http.Client{Transport: base})
			resp, err := client.Get("http://example.test/x")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != payload {
				t.Fatalf("decoded body = %q, want %q", got, payload)
			}
			if ce := resp.Header.Get("Content-Encoding"); ce != "" {
				t.Fatalf("Content-Encoding survived decoding: %q", ce)
			}
			if cl := resp.Header.Get("Content-Length"); cl != "" {
				t.Fatalf("Content-Length survived decoding: %q", cl)
			}
		})
	}
}

// TestDecompressTransportPassesIdentityThrough keeps the wrapper from touching
// responses the vendor did not encode.
func TestDecompressTransportPassesIdentityThrough(t *testing.T) {
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("plain")),
		}, nil
	})
	client := withDecompression(&http.Client{Transport: base})
	resp, err := client.Get("http://example.test/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != "plain" {
		t.Fatalf("body = %q, want plain", got)
	}
}

// TestDecompressSurvivesARealServer does the same over httptest, so the wrapper
// is exercised with a genuine net/http response and a reused connection.
func TestDecompressSurvivesARealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mustGzip(t, "streamed"))
	}))
	defer srv.Close()

	c, _ := newClient(t, nil)
	c.deps.HTTPClient = srv.Client()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/models", nil)
	applyChatHeaders(req.Header, c.identity())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != "streamed" {
		t.Fatalf("body = %q, want streamed", got)
	}
}

func TestApplyIdentityHeadersAdvertisesTheCLIEncodings(t *testing.T) {
	var h http.Header = http.Header{}
	applyIdentityHeaders(h, deviceIdentity{}, true)
	if got := h.Get("Accept-Encoding"); got != "br, gzip, deflate" {
		t.Fatalf("Accept-Encoding = %q, want br, gzip, deflate", got)
	}
}

// roundTripFunc adapts a function to http.RoundTripper for the table-driven
// decoding tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
