// Package fingerprint builds net/http clients whose TLS handshake is a copy of
// a real browser's rather than Go's crypto/tls, so a vendor that fingerprints
// the transport layer sees the client it expects to see.
//
// It is a leaf package: it imports the standard library and uTLS, and it never
// imports internal/core or a client module. A module opts in by calling New and
// keeps the stock transport otherwise, so adding this package cannot change the
// behaviour of a module that does not use it.
//
// # What is impersonated
//
// The whole ClientHello: cipher suites and their order, the extension set and
// its order, the GREASE values, the supported curves, the signature algorithms
// and the ALPN list. That is the material JA3 and JA4 are computed from, and it
// is what turns "Go-http-client" into "Chrome" at the transport layer.
//
// # What is not
//
// HTTP/2 frames. When a client ends up speaking h2, the frames are written by
// golang.org/x/net/http2, so the SETTINGS payload and the pseudo-header order
// stay Go-shaped even though the ClientHello does not. Impersonating those as
// well needs fhttp, whose Header type is not net/http.Header and which would
// force every client module to rewrite its request construction and its tests —
// a worse trade than the one this package makes. Say so in a module's README
// rather than pretending the h2 layer is clean.
//
// # Choosing a protocol
//
// Browsers offer h2 and http/1.1 and take whichever the server picks, which is
// what ProtocolAuto does. ProtocolH1 offers http/1.1 only, which is what a
// module wants when Go's h2 stack is itself the thing being detected; note that
// dropping h2 from the ALPN list is visible, so it is a deliberate trade and
// not a free one.
//
// # The node profile
//
// ProfileNode is not a browser. It is the Node.js runtime bundled into a vendor
// CLI, captured on the wire as a raw ClientHello. uTLS ships no parrot for it,
// so this package builds the spec by hand from the capture: 52 cipher suites in
// their captured order, twelve extensions in their captured order, ALPN
// http/1.1 alone, and no GREASE anywhere. It is h1-first on purpose, so
// ProtocolH1 reproduces the capture byte for byte at the JA3 layer; ProtocolAuto
// and ProtocolH2 widen the ALPN list to offer h2, which is a visible edit to the
// hello rather than a transparent one.
//
// The residual difference is the same one every profile here carries: when h2
// is negotiated, the frames are golang.org/x/net/http2's, so the SETTINGS
// payload and the pseudo-header order stay Go-shaped. Only the TLS ClientHello
// is impersonated.
//
// # The trae profile
//
// ProfileTrae is the desktop Trae application, captured on api.trae.com.cn and
// identical from both the CN and the SOLO build. It is the only profile here
// that cannot speak TLS 1.3: the capture offers no supported_versions
// extension, so the hello is TLS 1.2 from the first byte to the last, and
// handshakeALPN pins uTLS's MaxVersion to match rather than letting the stack
// plan a 1.3 handshake the hello never advertised. It is also the smallest
// hello in the table — eighteen cipher suites, seven extensions, three curves
// and one point format — with no GREASE and no ALPN at all, so ProtocolH1
// reproduces the capture exactly at the JA3 layer.
package fingerprint

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Profile names the client hello to imitate.
type Profile string

// ErrUnknownProfile is returned by New for a profile that is not in the table.
var ErrUnknownProfile = errors.New("fingerprint: unknown profile")

// The supported profiles. ProfileNone is the zero value on purpose: an unset
// config key must keep the stock handshake instead of silently impersonating
// someone.
const (
	ProfileNone Profile = ""
	// ProfileGolang is Go's own hello. It exists so a module can choose "look
	// like nobody in particular" deliberately, and so a test can prove the
	// difference between the two.
	ProfileGolang Profile = "golang"

	// ProfileNode is the hand-built hello of a vendor CLI's bundled Node.js
	// runtime: no GREASE, http/1.1-only ALPN, and the cipher and extension
	// order of the capture. See the package comment.
	ProfileNode Profile = "node"

	// ProfileTrae is the hand-built hello of the Trae desktop applications.
	// Unlike every parrot in the table it is TLS 1.2 only: the capture of
	// api.trae.com.cn carries no supported_versions extension and no ALPN, and
	// both Trae clients sent it unchanged.
	ProfileTrae Profile = "trae"

	ProfileChrome     Profile = "chrome"
	ProfileChrome133  Profile = "chrome-133"
	ProfileChrome131  Profile = "chrome-131"
	ProfileChrome120  Profile = "chrome-120"
	ProfileChrome106  Profile = "chrome-106"
	ProfileFirefox    Profile = "firefox"
	ProfileFirefox120 Profile = "firefox-120"
	ProfileSafari     Profile = "safari"
	ProfileEdge       Profile = "edge"
	ProfileIOS        Profile = "ios"
	ProfileAndroid    Profile = "android"
	ProfileRandomized Profile = "randomized"
)

var hellos = map[Profile]utls.ClientHelloID{
	ProfileGolang: utls.HelloGolang,
	// ProfileNode and ProfileTrae are hand-built rather than loaded from a uTLS
	// parrot, so their id is the sentinel that tells specForDial to build them
	// from the capture. They are in this table so Profiles() lists them and New
	// accepts them like any other name.
	ProfileNode:       utls.HelloCustom,
	ProfileTrae:       utls.HelloCustom,
	ProfileChrome:     utls.HelloChrome_Auto,
	ProfileChrome133:  utls.HelloChrome_133,
	ProfileChrome131:  utls.HelloChrome_131,
	ProfileChrome120:  utls.HelloChrome_120,
	ProfileChrome106:  utls.HelloChrome_106_Shuffle,
	ProfileFirefox:    utls.HelloFirefox_Auto,
	ProfileFirefox120: utls.HelloFirefox_120,
	ProfileSafari:     utls.HelloSafari_Auto,
	ProfileEdge:       utls.HelloEdge_Auto,
	ProfileIOS:        utls.HelloIOS_Auto,
	ProfileAndroid:    utls.HelloAndroid_11_OkHttp,
	ProfileRandomized: utls.HelloRandomized,
}

// Profiles lists the accepted profile names, sorted, for config validation and
// for error messages. ProfileNone is listed as "" so an operator can see that
// the off switch exists.
func Profiles() []string {
	out := make([]string, 0, len(hellos)+1)
	out = append(out, string(ProfileNone))
	for name := range hellos {
		out = append(out, string(name))
	}
	sort.Strings(out)
	return out
}

// Protocol names the application-layer protocol to negotiate.
type Protocol string

const (
	// ProtocolAuto offers the profile's own ALPN list — for Chrome that is
	// h2 followed by http/1.1 — and falls back to HTTP/1.1 when the server
	// refuses h2. It is the zero value, so it is also the default.
	ProtocolAuto Protocol = ""
	// ProtocolH2 requires h2. A server that will not negotiate it is an error
	// rather than a silent downgrade.
	ProtocolH2 Protocol = "h2"
	// ProtocolH1 offers http/1.1 only and never speaks h2.
	ProtocolH1 Protocol = "http/1.1"
)

// Options configures a fingerprinted client.
type Options struct {
	// Profile selects the hello. The empty value builds the stock client.
	Profile Profile
	// Protocol selects the application protocol. Empty means ProtocolAuto.
	Protocol Protocol

	// Timeout is the client's overall timeout. Zero means no timeout, which is
	// what a streaming SSE call needs.
	Timeout time.Duration
	// DialTimeout bounds the TCP connect. Zero means 15s.
	DialTimeout time.Duration
	// ResponseHeaderTimeout bounds the wait for the response headers. Zero
	// means unbounded, which is again what streaming needs.
	ResponseHeaderTimeout time.Duration

	// Proxy is the proxy URL to route through, an empty string for a direct
	// connection. A proxy forces HTTP/1.1: x/net/http2 cannot tunnel through
	// one, and silently ignoring a configured proxy would be worse than
	// downgrading.
	Proxy string

	// InsecureSkipVerify turns off certificate verification. It exists for
	// tests against a local self-signed server; a production module must not
	// set it.
	InsecureSkipVerify bool

	// DisableCompression keeps net/http from adding its own
	// "Accept-Encoding: gzip". Set it when the module sends a browser-shaped
	// Accept-Encoding itself, because a duplicated header is its own tell.
	DisableCompression bool

	// Logf, when set, receives one line per h2 downgrade.
	Logf func(format string, args ...any)
}

func (o Options) dialTimeout() time.Duration {
	if o.DialTimeout > 0 {
		return o.DialTimeout
	}
	return 15 * time.Second
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// New builds the client. An empty Profile yields the stock net/http client, so
// a module can wire its config key straight through and an unset value keeps
// today's behaviour exactly.
func New(o Options) (*http.Client, error) {
	proto := o.effectiveProtocol()

	// Go's own hello is crypto/tls, so these two profiles are the same path:
	// looking like nobody, once by not deciding and once by deciding to.
	if o.Profile == ProfileNone || o.Profile == ProfileGolang {
		return &http.Client{Timeout: o.Timeout, Transport: stockTransport(o, proto)}, nil
	}

	hello, ok := hellos[o.Profile]
	if !ok {
		return nil, fmt.Errorf("%w: %q (known profiles: %s)",
			ErrUnknownProfile, string(o.Profile), strings.Join(Profiles(), ", "))
	}
	// Fail at construction rather than on the first request, so a profile that
	// uTLS cannot build is a startup error.
	if _, err := specForDial(o.Profile, hello, proto == ProtocolH1); err != nil {
		return nil, fmt.Errorf("fingerprint: profile %q: %w", string(o.Profile), err)
	}

	b := &builder{opts: o, hello: hello}

	h1 := b.http1Transport()
	if proto == ProtocolH1 {
		return &http.Client{Timeout: o.Timeout, Transport: h1}, nil
	}

	h2 := b.http2Transport()
	if proto == ProtocolH2 {
		return &http.Client{Timeout: o.Timeout, Transport: h2}, nil
	}
	return &http.Client{Timeout: o.Timeout, Transport: &switching{h2: h2, h1: h1, b: b, logf: o.logf}}, nil
}

// effectiveProtocol applies the default and the proxy rule once, so the rest of
// New reads a resolved value instead of testing three fields.
func (o Options) effectiveProtocol() Protocol {
	if o.Proxy != "" && o.Protocol != ProtocolH1 {
		o.logf("fingerprint: profile %q: a proxy forces HTTP/1.1 (x/net/http2 cannot tunnel)", string(o.Profile))
		return ProtocolH1
	}
	if o.Protocol == ProtocolH1 || o.Protocol == ProtocolH2 {
		return o.Protocol
	}
	return ProtocolAuto
}

// stockTransport is the plain net/http transport, options applied but no
// handshake impersonation. It is what New returns for ProfileNone and
// ProfileGolang.
func stockTransport(o Options, proto Protocol) *http.Transport {
	t := &http.Transport{
		Proxy:               proxyFunc(o.Proxy),
		DialContext:         (&net.Dialer{Timeout: o.dialTimeout(), KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   proto != ProtocolH1,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		DisableCompression:  o.DisableCompression,
	}
	if proto == ProtocolH1 || o.InsecureSkipVerify {
		t.TLSClientConfig = &tls.Config{ //nolint:gosec // opt-in, docs say tests only
			InsecureSkipVerify: o.InsecureSkipVerify,
		}
		if proto == ProtocolH1 {
			t.TLSClientConfig.NextProtos = []string{"http/1.1"}
		}
	}
	if o.ResponseHeaderTimeout > 0 {
		t.ResponseHeaderTimeout = o.ResponseHeaderTimeout
	}
	return t
}

func proxyFunc(raw string) func(*http.Request) (*url.URL, error) {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Callers validate the proxy before reaching here; returning a proxy
		// function that always fails keeps the mistake visible at request time
		// instead of pretending the connection is direct.
		return func(*http.Request) (*url.URL, error) {
			return nil, fmt.Errorf("fingerprint: bad proxy %q: %w", raw, err)
		}
	}
	return http.ProxyURL(u)
}

// specForDial builds a spec for one handshake, and must not be cached: uTLS's
// ApplyPreset rewrites the GREASE values, computes the padding length and fills
// in the key share in place, so a spec reused for a second dial still carries
// the first dial's state and the server answers it with "bad record MAC". A
// fresh spec per connection is the price of correctness, and connection pooling
// means it is paid once per TCP connection rather than once per request.
//
// The hand-built profiles are selected by name and not by hello id: they share
// the HelloCustom sentinel because uTLS has no parrot matching either capture,
// and UTLSIdToSpec resolves parrots only. A fresh spec is returned every call,
// which is what the caller requires.
func specForDial(p Profile, hello utls.ClientHelloID, http1Only bool) (*utls.ClientHelloSpec, error) {
	switch p {
	case ProfileNode:
		return nodeSpec(http1Only), nil
	case ProfileTrae:
		return traeSpec(http1Only), nil
	}
	spec, err := utls.UTLSIdToSpec(hello)
	if err != nil {
		return nil, err
	}
	if http1Only {
		setALPN(&spec, []string{"http/1.1"})
	}
	return &spec, nil
}

// setALPN replaces the spec's ALPN list so the caller controls what the server
// may pick. Narrowing it is visible to a server that compares the ALPN
// extension against the rest of the hello; widening it (offering h2 where the
// captured hello did not) is visible in the same way, which is the price of
// speaking h2 over an h1-first profile.
func setALPN(spec *utls.ClientHelloSpec, alpn []string) {
	for i, ext := range spec.Extensions {
		if _, ok := ext.(*utls.ALPNExtension); ok {
			spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: alpn}
			return
		}
	}
	spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: alpn})
}

// nodeCipherSuites is the 52-suite list from the capture, in order. It is
// written in decimal so it can be diffed line by line against the JA3 string
// it has to reproduce.
var nodeCipherSuites = []uint16{
	4866, 4867, 4865, // TLS 1.3: AES-256-GCM-SHA384, CHACHA20, AES-128-GCM-SHA256
	49199, 49195, 49200, 49196, // ECDHE-ECDSA/RSA AES-GCM and AES-256-GCM
	158, 49191, 103, 49192, 107, // ECDHE-RSA/ECDSA CHACHA20-POLY1305
	163, 159, // DHE-RSA AES-CCM
	52393, 52392, 52394, // ECDHE-ECDSA/RSA ARIA
	49325, 49311, 49245, 49249, 49239, 49235,
	162, 49324, 49310, 49244, 49248, 49238, 49234,
	49188, 106, 49187, 64, // ECDHE-ECDSA / ECDHE-RSA CBC SHA
	49162, 49172, 57, 56, 49161, 49171, 51, 50,
	157, 49309, 49233, 156, 49308, 49232,
	61, 60, 53, 47,
}

// nodeCurves is supported_groups from the capture. 4588 is X25519MLKEM768 and
// 30 is x25519kyber768draft00, neither of which is GREASE. uTLS names 30 as
// 0x6399 (a later draft of the same group), so the captured 30 goes in as a
// literal rather than through the uTLS constant.
var nodeCurves = []utls.CurveID{
	4588, // X25519MLKEM768
	29,   // X25519
	23,   // secp256r1
	30,   // x25519kyber768draft00
	24,   // secp384r1
	25,   // secp521r1
	256,  // ffdhe2048
	257,  // ffdhe3072
}

// nodePointFormats is ec_point_formats from the capture.
var nodePointFormats = []uint8{0, 1, 2}

// nodeSignatureAlgorithms is the one field JA3 does not cover: the capture does
// not record the body of extension 13, so this is a modern browser-shaped list
// rather than a reproduced one, and it exercises the TLS 1.3 requirement that a
// PSS scheme be offered.
var nodeSignatureAlgorithms = []utls.SignatureScheme{
	utls.ECDSAWithP256AndSHA256,
	utls.PSSWithSHA256,
	utls.PKCS1WithSHA256,
	utls.ECDSAWithP384AndSHA384,
	utls.PSSWithSHA384,
	utls.PKCS1WithSHA384,
	utls.PSSWithSHA512,
	utls.PKCS1WithSHA512,
	utls.PKCS1WithSHA1,
	utls.ECDSAWithSHA1,
}

// nodeSpec builds the ClientHello of the captured Node.js runtime. It is a
// fresh value on every call: uTLS's ApplyPreset rewrites the key share and the
// session id in place, so a shared spec would carry one connection's state into
// the next one and the server would answer it with "bad record MAC".
//
// The extension order is exactly the captured order:
//
//	65281 renegotiation_info, 0 SNI, 11 ec_point_formats, 10 supported_groups,
//	35 session_ticket, 16 ALPN, 22 encrypt_then_mac, 23 extended_master_secret,
//	13 signature_algorithms, 43 supported_versions, 45 psk_key_exchange_modes,
//	51 key_share
//
// with no GREASE in either list. ProtocolH1 keeps the captured ALPN list of
// http/1.1 alone; every other protocol choice adds h2 to it, because a
// transport that wants to speak h2 has to offer it somewhere.
func nodeSpec(http1Only bool) *utls.ClientHelloSpec {
	alpn := []string{"http/1.1"}
	if !http1Only {
		alpn = []string{"h2", "http/1.1"}
	}
	return &utls.ClientHelloSpec{
		CipherSuites:       nodeCipherSuites,
		CompressionMethods: []uint8{0}, // null, the only method that exists
		Extensions: []utls.TLSExtension{
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SNIExtension{},
			&utls.SupportedPointsExtension{SupportedPoints: nodePointFormats},
			&utls.SupportedCurvesExtension{Curves: nodeCurves},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: alpn},
			// encrypt_then_mac (22) has no uTLS type and an empty body; the
			// capture keeps it as a bare type with nothing in it.
			&utls.GenericExtension{Id: 22},
			&utls.ExtendedMasterSecretExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: nodeSignatureAlgorithms},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			// The key share body is not part of JA3, so only groups uTLS can
			// generate a share for go here; 4588 comes first to match the
			// captured supported_groups order.
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519MLKEM768},
				{Group: utls.X25519},
			}},
		},
	}
}

// traeCipherSuites is the 18-suite list from the api.trae.com.cn capture, in
// order. There is no TLS 1.3 suite in it, because the captured client offers
// none: it is a TLS 1.2 handshake from the first byte to the last.
var traeCipherSuites = []uint16{
	49196, 49195, 49200, 49199, // ECDHE ECDSA/RSA AES-256/128-GCM
	49188, 49187, 49192, 49191, // ECDHE ECDSA/RSA AES-256/128-CBC-SHA384 and SHA256
	49162, 49161, 49172, 49171, // ECDHE ECDSA/RSA AES-256/128-CBC-SHA
	157, 156, 61, 60, 53, 47, // RSA AES-GCM, AES-CBC-SHA, AES-CBC-SHA256
}

// traeCurves is supported_groups from the capture: no post-quantum group and no
// FFDHE, only the three curves a TLS 1.2 stack of that vintage offers.
var traeCurves = []utls.CurveID{
	utls.X25519,
	utls.CurveP256,
	utls.CurveP384,
}

// traePointFormats is ec_point_formats from the capture: uncompressed alone,
// against nodeSpec's three formats.
var traePointFormats = []uint8{0}

// traeSignatureAlgorithms is the field JA3 cannot see. The capture records that
// extension 13 was offered, not what was in it, so this is the list a TLS
// 1.2-only stack of that vintage sends: the PSS schemes are absent because a
// client that does not offer TLS 1.3 has no use for them, and that absence is
// itself consistent with the rest of the hello.
var traeSignatureAlgorithms = []utls.SignatureScheme{
	utls.ECDSAWithP256AndSHA256,
	utls.ECDSAWithP384AndSHA384,
	utls.ECDSAWithP521AndSHA512,
	utls.PKCS1WithSHA256,
	utls.PKCS1WithSHA384,
	utls.PKCS1WithSHA512,
	utls.PKCS1WithSHA1,
	utls.ECDSAWithSHA1,
}

// traeSpec builds the ClientHello of the captured Trae desktop client. The
// extension order is exactly the captured order:
//
//	0 SNI, 10 supported_groups, 11 ec_point_formats, 13 signature_algorithms,
//	35 session_ticket, 23 extended_master_secret, 65281 renegotiation_info
//
// Two absences are the fingerprint rather than an oversight. There is no
// supported_versions extension, so the handshake cannot upgrade past TLS 1.2 —
// which is why handshakeALPN pins uTLS's MaxVersion for this profile, so the
// stack does not plan a 1.3 handshake the hello never offered. And there is no
// ALPN extension, so the captured client never names a protocol at all.
//
// A transport that means to speak h2 has to name it somewhere, so ProtocolH1
// keeps the capture exactly and every other protocol choice appends an ALPN
// extension offering h2 and http/1.1. Extension 16 is one entry in JA3 either
// way, so widening the list moves the ALPN contents and not the digest.
func traeSpec(http1Only bool) *utls.ClientHelloSpec {
	spec := &utls.ClientHelloSpec{
		CipherSuites:       traeCipherSuites,
		CompressionMethods: []uint8{0}, // null, the only method that exists
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.SupportedCurvesExtension{Curves: traeCurves},
			&utls.SupportedPointsExtension{SupportedPoints: traePointFormats},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: traeSignatureAlgorithms},
			&utls.SessionTicketExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
		},
	}
	if !http1Only {
		setALPN(spec, []string{"h2", "http/1.1"})
	}
	return spec
}

// builder carries what every dial needs.
type builder struct {
	opts  Options
	hello utls.ClientHelloID

	mu sync.Mutex
	// noHTTP2 remembers hosts that answered the handshake with something other
	// than h2, so only the first request pays for the discovery.
	noHTTP2 map[string]bool
}

func (b *builder) http1Transport() *http.Transport {
	t := &http.Transport{
		Proxy:               proxyFunc(b.opts.Proxy),
		DialContext:         (&net.Dialer{Timeout: b.opts.dialTimeout(), KeepAlive: 30 * time.Second}).DialContext,
		DialTLSContext:      b.dialHTTP1,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		DisableCompression:  b.opts.DisableCompression,
		// DialTLSContext is set, so ForceAttemptHTTP2 would be ignored anyway;
		// saying false makes that explicit rather than incidental.
		ForceAttemptHTTP2: false,
	}
	if b.opts.ResponseHeaderTimeout > 0 {
		t.ResponseHeaderTimeout = b.opts.ResponseHeaderTimeout
	}
	return t
}

func (b *builder) http2Transport() *http2.Transport {
	return &http2.Transport{
		DialTLSContext: b.dialHTTP2,
		TLSClientConfig: &tls.Config{ //nolint:gosec // opt-in, docs say tests only
			InsecureSkipVerify: b.opts.InsecureSkipVerify,
		},
	}
}

// dialHTTP1 offers http/1.1 only, so the server has no way to pick h2 and
// strand a connection the stdlib transport cannot drive.
//
// Mind the signature: net/http wants a three-argument DialTLSContext and
// carries no tls.Config through it, so the server name comes from the address
// and the verification switch from our options. x/net/http2's version of the
// same hook does take a config, which is why the two differ.
func (b *builder) dialHTTP1(ctx context.Context, network, addr string) (net.Conn, error) {
	spec, err := specForDial(b.opts.Profile, b.hello, true)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: %s: %w", addr, err)
	}
	return b.handshake(ctx, network, addr, nil, spec)
}

// dialHTTP2 offers the profile's own ALPN list and records a refused h2, so the
// switching transport can downgrade the host without pattern-matching an error
// string that x/net is free to reword.
func (b *builder) dialHTTP2(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
	spec, err := specForDial(b.opts.Profile, b.hello, false)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: %s: %w", addr, err)
	}
	conn, alpn, err := b.handshakeALPN(ctx, network, addr, cfg, spec)
	if err != nil {
		return nil, err
	}
	if alpn != "h2" {
		b.markNoHTTP2(addr)
		conn.Close()
		return nil, fmt.Errorf("fingerprint: %s negotiated %q, not h2", addr, alpn)
	}
	return conn, nil
}

func (b *builder) markNoHTTP2(addr string) {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.noHTTP2 == nil {
		b.noHTTP2 = make(map[string]bool, 4)
	}
	b.noHTTP2[host] = true
}

func (b *builder) refusedHTTP2(host string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.noHTTP2[host]
}

func (b *builder) handshake(ctx context.Context, network, addr string, cfg *tls.Config, spec *utls.ClientHelloSpec) (net.Conn, error) {
	conn, _, err := b.handshakeALPN(ctx, network, addr, cfg, spec)
	return conn, err
}

// handshakeALPN dials, runs the uTLS handshake against the given spec, and
// reports the negotiated ALPN.
func (b *builder) handshakeALPN(ctx context.Context, network, addr string, cfg *tls.Config, spec *utls.ClientHelloSpec) (net.Conn, string, error) {
	d := &net.Dialer{Timeout: b.opts.dialTimeout(), KeepAlive: 30 * time.Second}
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, "", err
	}

	serverName := ""
	skip := b.opts.InsecureSkipVerify
	if cfg != nil {
		serverName = cfg.ServerName
		if cfg.InsecureSkipVerify {
			skip = true
		}
	}
	if serverName == "" {
		serverName, _, _ = net.SplitHostPort(addr)
	}

	ucfg := &utls.Config{ //nolint:gosec // opt-in, docs say tests only
		ServerName:         serverName,
		InsecureSkipVerify: skip,
	}
	// ProfileTrae imitates a client that never offers TLS 1.3: its hello has no
	// supported_versions extension at all. Saying so here as well keeps uTLS
	// from planning a 1.3 handshake the hello never advertised.
	if b.opts.Profile == ProfileTrae {
		ucfg.MaxVersion = tls.VersionTLS12
	}
	uconn := utls.UClient(raw, ucfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(spec); err != nil {
		raw.Close()
		return nil, "", fmt.Errorf("fingerprint: apply preset for %s: %w", addr, err)
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, "", err
	}
	return uconn, uconn.ConnectionState().NegotiatedProtocol, nil
}

// switching speaks h2 while the server supports it and HTTP/1.1 afterwards, and
// remembers per host which one applies. It exists instead of a plain h2
// transport because the "server has no h2" answer arrives during the handshake,
// and one downgraded host must not turn into a failed request.
type switching struct {
	h2   *http2.Transport
	h1   *http.Transport
	b    *builder
	logf func(string, ...any)
}

func (s *switching) RoundTrip(req *http.Request) (*http.Response, error) {
	// Check the host's verdict first: once a server has refused h2, trying it
	// again would cost a handshake on every single request.
	if req.URL == nil || s.b.refusedHTTP2(req.URL.Hostname()) {
		return s.h1.RoundTrip(req)
	}
	resp, err := s.h2.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if !s.b.refusedHTTP2(req.URL.Hostname()) {
		return nil, err
	}
	if s.logf != nil {
		s.logf("fingerprint: %s refused h2, retrying over http/1.1", req.URL.Host)
	}
	if err := rewind(req); err != nil {
		return nil, err
	}
	return s.h1.RoundTrip(req)
}

// rewind puts a consumed body back so the HTTP/1.1 retry sends the same bytes.
// The handshake failed before any request byte was written, so the only loss is
// the body the h2 transport may have already read.
func rewind(req *http.Request) error {
	if req.Body == nil || req.GetBody == nil {
		return nil
	}
	body, err := req.GetBody()
	if err != nil {
		return fmt.Errorf("fingerprint: rewind request body: %w", err)
	}
	req.Body = body
	return nil
}
