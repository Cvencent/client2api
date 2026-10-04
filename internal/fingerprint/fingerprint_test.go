package fingerprint

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// helloLog records the ClientHello every handshake offered, which is the only
// way to prove from a test that uTLS is driving the handshake and not
// crypto/tls wearing a different name.
type helloLog struct {
	mu     sync.Mutex
	hellos []*tls.ClientHelloInfo
}

func (h *helloLog) add(chi *tls.ClientHelloInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hellos = append(h.hellos, chi)
}

func (h *helloLog) last(t *testing.T) *tls.ClientHelloInfo {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.hellos) == 0 {
		t.Fatal("the server recorded no client hello")
	}
	return h.hellos[len(h.hellos)-1]
}

// tlsServer starts a local HTTPS server that records the client hello and
// answers with the protocol it ended up speaking.
func tlsServer(t *testing.T, enableHTTP2 bool) (*httptest.Server, *helloLog) {
	t.Helper()
	log := &helloLog{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Proto)
	}))
	srv.EnableHTTP2 = enableHTTP2
	srv.TLS = &tls.Config{
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			log.add(chi)
			return nil, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, log
}

func newClient(t *testing.T, o Options) *http.Client {
	t.Helper()
	o.InsecureSkipVerify = true // the test server's certificate is self-signed
	c, err := New(o)
	if err != nil {
		t.Fatalf("New(%+v): %v", o, err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func get(t *testing.T, c *http.Client, rawURL string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

// hasGREASE reports whether a cipher suite list contains a GREASE value, that
// is one of 0x0a0a, 0x1a1a, … 0xfafa. Browsers send one; crypto/tls never does,
// so it is a cheap and unambiguous "who built this hello" probe.
func hasGREASE(vals []uint16) bool {
	for _, v := range vals {
		if v&0x0f0f == 0x0a0a {
			return true
		}
	}
	return false
}

func TestProfilesListTheOffSwitchAndTheBrowsers(t *testing.T) {
	got := Profiles()
	for _, want := range []string{
		"", "golang", "chrome", "chrome-133", "chrome-131", "chrome-120",
		"chrome-106", "firefox", "firefox-120", "safari", "edge", "ios",
		"android", "randomized",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("Profiles() is missing %q, got %v", want, got)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("Profiles() must be sorted so two error messages read the same: %v", got)
	}
	if !slices.Contains(got, "") {
		t.Error(`Profiles() must list "" so an operator can see the off switch exists`)
	}
}

func TestAnUnknownProfileIsRefusedAndNamesTheKnownOnes(t *testing.T) {
	_, err := New(Options{Profile: "netscape"})
	if !errors.Is(err, ErrUnknownProfile) {
		t.Fatalf("want ErrUnknownProfile, got %v", err)
	}
	if !strings.Contains(err.Error(), "chrome") {
		t.Errorf("the error must list the known profiles so a typo is fixable: %v", err)
	}
}

func TestTheEmptyProfileKeepsTheStockHandshake(t *testing.T) {
	c, err := New(Options{Profile: ProfileNone})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("ProfileNone must keep the stdlib transport, got %T", c.Transport)
	}
	if tr.DialTLSContext != nil {
		t.Error("ProfileNone must not install a uTLS dialer")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ProfileNone must leave HTTP/2 on, exactly as the stock client does")
	}

	// ProfileGolang is the same handshake chosen out loud rather than by
	// omission, so it resolves to the same transport shape.
	g, err := New(Options{Profile: ProfileGolang})
	if err != nil {
		t.Fatalf("New(ProfileGolang): %v", err)
	}
	if _, ok := g.Transport.(*http.Transport); !ok {
		t.Fatalf("ProfileGolang must resolve to the stdlib transport, got %T", g.Transport)
	}
}

func TestABrowserProfileCarriesGREASEAndGoDoesNot(t *testing.T) {
	srv, log := tlsServer(t, true)

	chrome := newClient(t, Options{Profile: ProfileChrome})
	get(t, chrome, srv.URL)
	chromeHello := log.last(t)
	if !hasGREASE(chromeHello.CipherSuites) {
		t.Errorf("the chrome profile must offer a GREASE cipher suite, got %v", chromeHello.CipherSuites)
	}

	golang := newClient(t, Options{Profile: ProfileGolang})
	get(t, golang, srv.URL)
	goHello := log.last(t)
	if hasGREASE(goHello.CipherSuites) {
		t.Errorf("crypto/tls must not offer GREASE, got %v", goHello.CipherSuites)
	}

	if len(chromeHello.CipherSuites) == len(goHello.CipherSuites) {
		t.Errorf("the impersonated hello must not be a rename of Go's: both sent %d cipher suites",
			len(chromeHello.CipherSuites))
	}
}

func TestTheProtocolChoiceShowsUpInALPN(t *testing.T) {
	srv, log := tlsServer(t, true)

	h1 := newClient(t, Options{Profile: ProfileChrome, Protocol: ProtocolH1})
	resp, body := get(t, h1, srv.URL)
	if resp.Proto != "HTTP/1.1" || body != "HTTP/1.1" {
		t.Errorf("ProtocolH1 must speak HTTP/1.1, got %s (body %q)", resp.Proto, body)
	}
	if protos := log.last(t).SupportedProtos; !slices.Equal(protos, []string{"http/1.1"}) {
		t.Errorf("ProtocolH1 must offer http/1.1 alone, got %v", protos)
	}

	h2 := newClient(t, Options{Profile: ProfileChrome, Protocol: ProtocolH2})
	resp, body = get(t, h2, srv.URL)
	if resp.Proto != "HTTP/2.0" || body != "HTTP/2.0" {
		t.Errorf("ProtocolH2 must speak HTTP/2, got %s (body %q)", resp.Proto, body)
	}
	if protos := log.last(t).SupportedProtos; !slices.Contains(protos, "h2") {
		t.Errorf("ProtocolH2 must offer h2, got %v", protos)
	}
}

func TestAutoSpeaksHTTP2WhenTheServerOffersIt(t *testing.T) {
	srv, _ := tlsServer(t, true)
	c := newClient(t, Options{Profile: ProfileChrome, Protocol: ProtocolAuto})
	resp, _ := get(t, c, srv.URL)
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("ProtocolAuto against an h2 server must speak h2, got %s", resp.Proto)
	}
}

func TestAutoDowngradesAndRemembersWhenTheServerHasNoHTTP2(t *testing.T) {
	srv, _ := tlsServer(t, false) // the server's ALPN list is http/1.1 only
	c := newClient(t, Options{Profile: ProfileChrome, Protocol: ProtocolAuto})

	resp, body := get(t, c, srv.URL)
	if resp.Proto != "HTTP/1.1" || body != "HTTP/1.1" {
		t.Fatalf("an h2-less server must be served over HTTP/1.1, got %s (body %q)", resp.Proto, body)
	}

	sw, ok := c.Transport.(*switching)
	if !ok {
		t.Fatalf("ProtocolAuto must install the downgrading transport, got %T", c.Transport)
	}
	if host := mustHost(t, srv.URL); !sw.b.refusedHTTP2(host) {
		t.Errorf("the refusal must be recorded for %s so the next request skips h2", host)
	}

	// The second request must not pay for the failed h2 attempt again.
	resp, body = get(t, c, srv.URL)
	if resp.Proto != "HTTP/1.1" || body != "HTTP/1.1" {
		t.Errorf("the second request must also be HTTP/1.1, got %s (body %q)", resp.Proto, body)
	}
}

func TestAProxyForcesHTTP1(t *testing.T) {
	for _, proto := range []Protocol{ProtocolAuto, ProtocolH2} {
		c, err := New(Options{Profile: ProfileChrome, Protocol: proto, Proxy: "http://127.0.0.1:3128"})
		if err != nil {
			t.Fatalf("New(Protocol=%q, proxy): %v", proto, err)
		}
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Protocol=%q with a proxy must use the HTTP/1.1 transport, got %T", proto, c.Transport)
		}
		req := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}
		got, err := tr.Proxy(req)
		if err != nil {
			t.Fatalf("Protocol=%q: the proxy function errored: %v", proto, err)
		}
		if got == nil || got.Host != "127.0.0.1:3128" {
			t.Errorf("Protocol=%q: the proxy was dropped, got %v", proto, got)
		}
		if tr.ForceAttemptHTTP2 {
			t.Errorf("Protocol=%q with a proxy must not attempt HTTP/2", proto)
		}
	}
}

func TestABadProxySurfacesAtRequestTime(t *testing.T) {
	c, err := New(Options{Profile: ProfileNone, Proxy: "://not a url"})
	if err != nil {
		t.Fatalf("New must accept the proxy string and let the caller validate it: %v", err)
	}
	tr := c.Transport.(*http.Transport)
	if _, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}); err == nil {
		t.Error("a malformed proxy must fail loudly rather than silently connecting direct")
	}
}

func TestDisableCompressionKeepsGoFromAddingItsOwnHeader(t *testing.T) {
	var seen string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Accept-Encoding")
	}))
	srv.StartTLS()
	t.Cleanup(srv.Close)

	c := newClient(t, Options{Profile: ProfileChrome, DisableCompression: true})
	if _, err := c.Get(srv.URL); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if seen != "" {
		t.Errorf("DisableCompression must leave Accept-Encoding to the module, got %q", seen)
	}
}

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Hostname()
}
