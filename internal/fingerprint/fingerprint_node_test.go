package fingerprint

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The captured fingerprint this profile exists to reproduce. The JA3 string is
// written out in full rather than only its md5, so a mismatch fails with a diff
// an operator can read instead of a digest that matches nothing.
const (
	nodeJA3    = "0303,4866-4867-4865-49199-49195-49200-49196-158-49191-103-49192-107-163-159-52393-52392-52394-49325-49311-49245-49249-49239-49235-162-49324-49310-49244-49248-49238-49234-49188-106-49187-64-49162-49172-57-56-49161-49171-51-50-157-49309-49233-156-49308-49232-61-60-53-47,65281-0-11-10-35-16-22-23-13-43-45-51,4588-29-23-30-24-25-256-257,0-1-2"
	nodeJA3MD5 = "bd93ef214c72b1c71f7c56e85fc6ee89"
)

// captureClientHello runs one real handshake attempt against a raw TCP listener
// and returns the ClientHello handshake message the client put on the wire.
//
// There is no TLS server here and no httptest: the listener reads the first
// TLS record, hands it back, and closes. The client's handshake therefore
// cannot complete, and the error net/http reports is the expected outcome
// rather than a failure — the bytes on the wire are the whole point.
func captureClientHello(t *testing.T, o Options) []byte {
	t.Helper()

	// Listen and dial by hostname, never by an IP literal: RFC 6066 forbids an
	// IP address as an SNI value, so uTLS's SNIExtension omits extension 0
	// altogether for one and the hello stops matching the capture.
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := net.JoinHostPort("localhost", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))

	type record struct {
		body []byte
		err  error
	}
	got := make(chan record, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- record{err: err}
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header); err != nil {
			got <- record{err: fmt.Errorf("read TLS record header: %w", err)}
			return
		}
		if header[0] != 0x16 {
			got <- record{err: fmt.Errorf("first byte is 0x%02x, want 0x16 (handshake)", header[0])}
			return
		}
		t.Logf("TLS record header: % x (type, legacy record version, length)", header)
		body := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
		if _, err := io.ReadFull(conn, body); err != nil {
			got <- record{err: fmt.Errorf("read TLS record body: %w", err)}
			return
		}
		got <- record{body: body}
	}()

	o.InsecureSkipVerify = true // no TLS server, so nothing to verify
	c, err := New(o)
	if err != nil {
		t.Fatalf("New(%+v): %v", o, err)
	}
	t.Cleanup(c.CloseIdleConnections)

	go func() {
		resp, err := c.Get("https://" + addr + "/")
		if err == nil {
			resp.Body.Close()
		}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("capture ClientHello: %v", r.err)
		}
		return r.body
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the ClientHello")
		return nil
	}
}

// helloReader walks the ClientHello with bounds checks, so a short or
// malformed record fails the test at the field it ran out on instead of
// panicking somewhere inside uTLS.
type helloReader struct {
	t   *testing.T
	b   []byte
	pos int
}

func (r *helloReader) take(n int) []byte {
	r.t.Helper()
	if n < 0 || r.pos+n > len(r.b) {
		r.t.Fatalf("ClientHello truncated: wanted %d bytes at offset %d of %d", n, r.pos, len(r.b))
	}
	out := r.b[r.pos : r.pos+n]
	r.pos += n
	return out
}

func (r *helloReader) u8() uint8   { return r.take(1)[0] }
func (r *helloReader) u16() uint16 { return binary.BigEndian.Uint16(r.take(2)) }
func (r *helloReader) u24() int {
	b := r.take(3)
	return int(b[0])<<16 | int(b[1])<<8 | int(b[2])
}

type tlsExtension struct {
	id   uint16
	data []byte
}

type clientHello struct {
	legacyVersion uint16
	cipherSuites  []uint16
	extensions    []tlsExtension

	alpn         []string
	versions     []uint16
	groups       []uint16
	pointFormats []uint16
}

func (ch *clientHello) extensionTypes() []uint16 {
	out := make([]uint16, len(ch.extensions))
	for i, e := range ch.extensions {
		out[i] = e.id
	}
	return out
}

func (ch *clientHello) extension(id uint16) ([]byte, bool) {
	for _, e := range ch.extensions {
		if e.id == id {
			return e.data, true
		}
	}
	return nil, false
}

// parseClientHello decodes the ClientHello by hand. It reads the pieces JA3 is
// computed from — legacy_version, the cipher suites, the extension list — and
// then the contents of the extensions whose bodies JA3 also uses.
func parseClientHello(t *testing.T, body []byte) *clientHello {
	t.Helper()
	r := &helloReader{t: t, b: body}

	if msg := r.u8(); msg != 0x01 {
		t.Fatalf("first handshake message is 0x%02x, want 0x01 (ClientHello)", msg)
	}
	declared := r.u24()
	if slack := len(body) - r.pos; declared != slack {
		t.Fatalf("the handshake length says %d bytes but %d follow the header", declared, slack)
	}

	ch := &clientHello{legacyVersion: r.u16()}
	r.take(32)          // random
	r.take(int(r.u8())) // legacy_session_id
	suiteBytes := int(r.u16())
	for i := 0; i < suiteBytes/2; i++ {
		ch.cipherSuites = append(ch.cipherSuites, r.u16())
	}
	r.take(int(r.u8())) // legacy_compression_methods

	extTotal := int(r.u16())
	if end := r.pos + extTotal; end != len(body) {
		t.Fatalf("the extension block says %d bytes but %d remain", extTotal, len(body)-r.pos)
	}
	for r.pos < len(body) {
		id := r.u16()
		ch.extensions = append(ch.extensions, tlsExtension{id: id, data: r.take(int(r.u16()))})
	}

	if data, ok := ch.extension(16); ok { // application_layer_protocol_negotiation
		a := &helloReader{t: t, b: data}
		list := a.take(int(a.u16()))
		for len(list) > 0 {
			n := int(list[0])
			if 1+n > len(list) {
				t.Fatalf("ALPN entry overruns the extension: %d bytes left", len(list))
			}
			ch.alpn = append(ch.alpn, string(list[1:1+n]))
			list = list[1+n:]
		}
	}
	if data, ok := ch.extension(43); ok { // supported_versions
		a := &helloReader{t: t, b: data}
		list := a.take(int(a.u8()))
		for len(list) >= 2 {
			ch.versions = append(ch.versions, binary.BigEndian.Uint16(list[:2]))
			list = list[2:]
		}
	}
	if data, ok := ch.extension(10); ok { // supported_groups
		a := &helloReader{t: t, b: data}
		list := a.take(int(a.u16()))
		for len(list) >= 2 {
			ch.groups = append(ch.groups, binary.BigEndian.Uint16(list[:2]))
			list = list[2:]
		}
	}
	if data, ok := ch.extension(11); ok { // ec_point_formats
		a := &helloReader{t: t, b: data}
		list := a.take(int(a.u8()))
		for _, f := range list {
			ch.pointFormats = append(ch.pointFormats, uint16(f))
		}
	}
	return ch
}

// ja3 formats the five fields in the order the JA3 spec puts them. It does no
// GREASE filtering on purpose: the capture has no GREASE, so a GREASE value
// that showed up here must change the string and the md5 rather than being
// quieted into a match.
func ja3(version uint16, ciphers, extTypes, groups, pointFormats []uint16) string {
	return strings.Join([]string{
		fmt.Sprintf("%04x", version),
		dashJoin(ciphers),
		dashJoin(extTypes),
		dashJoin(groups),
		dashJoin(pointFormats),
	}, ",")
}

func dashJoin(vals []uint16) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = strconv.FormatUint(uint64(v), 10)
	}
	return strings.Join(parts, "-")
}

func ja3MD5(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func computedJA3(t *testing.T, o Options) (string, *clientHello) {
	t.Helper()
	ch := parseClientHello(t, captureClientHello(t, o))
	return ja3(ch.legacyVersion, ch.cipherSuites, ch.extensionTypes(), ch.groups, ch.pointFormats), ch
}

// TestTheNodeProfileReproducesTheCapturedJA3 is the point of the profile: a
// real handshake, a raw socket, and a byte-for-byte JA3 match.
func TestTheNodeProfileReproducesTheCapturedJA3(t *testing.T) {
	got, ch := computedJA3(t, Options{Profile: ProfileNode, Protocol: ProtocolH1})

	if got != nodeJA3 {
		t.Errorf("the emitted ClientHello is not the captured one:\n got %s\nwant %s", got, nodeJA3)
	}
	if sum := ja3MD5(got); sum != nodeJA3MD5 {
		t.Errorf("JA3 md5 = %s, want %s", sum, nodeJA3MD5)
	}
	t.Logf("emitted JA3: %s", got)
	t.Logf("emitted JA3 md5: %s", ja3MD5(got))

	if n := len(ch.cipherSuites); n != 52 {
		t.Errorf("the capture offers 52 cipher suites, got %d: %v", n, ch.cipherSuites)
	}
	if n := len(ch.extensions); n != 12 {
		t.Errorf("the capture offers 12 extensions, got %d: %v", n, ch.extensionTypes())
	}
	if !slices.Equal(ch.alpn, []string{"http/1.1"}) {
		t.Errorf("the captured hello offers http/1.1 alone, got %v", ch.alpn)
	}
	for _, want := range []uint16{0x0304, 0x0303} {
		if !slices.Contains(ch.versions, want) {
			t.Errorf("supported_versions must offer 0x%04x, got %v", want, ch.versions)
		}
	}

	// No GREASE anywhere: 4588 is X25519MLKEM768, not a GREASE value, and the
	// capture has none in either list.
	if hasGREASE(ch.cipherSuites) {
		t.Errorf("the captured hello has no GREASE cipher suite, got %v", ch.cipherSuites)
	}
	if hasGREASE(ch.extensionTypes()) || hasGREASE(ch.groups) {
		t.Errorf("the captured hello has no GREASE in its extensions or groups: %v / %v",
			ch.extensionTypes(), ch.groups)
	}
}

// TestTheNodeJA3IsNotAnyOtherProfiles is the negative control: without it, a
// helper that returned the constant would pass the assertion above.
func TestTheNodeJA3IsNotAnyOtherProfiles(t *testing.T) {
	for _, profile := range []Profile{ProfileChrome, ProfileFirefox, ProfileSafari} {
		got, _ := computedJA3(t, Options{Profile: profile, Protocol: ProtocolH1})
		if sum := ja3MD5(got); sum == nodeJA3MD5 {
			t.Errorf("profile %q produced the node md5 %s, so the node assertion proves nothing",
				profile, sum)
		}
	}
}

// TestTheNodeProfileWidensALPNForHTTP2 covers the other half of the protocol
// contract: the capture offers http/1.1 alone, but a transport that means to
// speak h2 has to offer h2, so the profile rewrites its own ALPN extension
// rather than pinning the captured list. JA3 carries the ALPN extension's type
// and not its contents, so the digest must not move when it widens.
func TestTheNodeProfileWidensALPNForHTTP2(t *testing.T) {
	got, ch := computedJA3(t, Options{Profile: ProfileNode, Protocol: ProtocolH2})

	if !slices.Contains(ch.alpn, "h2") {
		t.Errorf("ProtocolH2 must offer h2 for this profile, got %v", ch.alpn)
	}
	if !slices.Contains(ch.alpn, "http/1.1") {
		t.Errorf("the widened list should keep http/1.1 as the fallback, got %v", ch.alpn)
	}
	if sum := ja3MD5(got); sum != nodeJA3MD5 {
		t.Errorf("widening ALPN moved the JA3 md5 to %s, want %s; JA3 has no ALPN contents",
			sum, nodeJA3MD5)
	}
}
