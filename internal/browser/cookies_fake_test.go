package browser

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// cookieFake is a tiny stand-in for a Chromium DevTools endpoint.  It speaks
// just enough of the protocol for Page.Cookies: one websocket, text frames, a
// JSON request, a JSON reply.  Keeping it in-process is what lets the cookie
// reader be tested without a real browser on the machine.
type cookieFake struct {
	ln       net.Listener
	addr     string
	reply    func(method string, params map[string]any) any
	gotCalls []string
}

func startCookieFake(t *testing.T, reply func(method string, params map[string]any) any) *cookieFake {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &cookieFake{ln: ln, addr: ln.Addr().String(), reply: reply}
	go f.serve(t)
	return f
}

func (f *cookieFake) serve(t *testing.T) {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := httpReadRequest(br)
	if err != nil {
		return
	}
	key := ""
	for _, line := range strings.Split(req, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-key:") {
			key = strings.TrimSpace(line[len("sec-websocket-key:"):])
		}
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)

	for {
		payload, err := readServerFrame(br)
		if err != nil {
			return
		}
		var msg struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		f.gotCalls = append(f.gotCalls, msg.Method)
		result := f.reply(msg.Method, msg.Params)
		out := map[string]any{"id": msg.ID, "result": result}
		raw, _ := json.Marshal(out)
		if err := writeServerFrame(conn, raw); err != nil {
			return
		}
	}
}

// httpReadRequest reads the request head up to the blank line.
func httpReadRequest(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return b.String(), err
		}
		b.WriteString(line)
		if line == "\r\n" || line == "\n" {
			return b.String(), nil
		}
	}
}

// readServerFrame reads one masked client frame (clients always mask).
func readServerFrame(br *bufio.Reader) ([]byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return nil, err
	}
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, nil
}

// writeServerFrame sends one unmasked server text frame.
func writeServerFrame(conn net.Conn, payload []byte) error {
	header := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

// newCookiePage dials a fake endpoint and returns a Page wired to it.
func newCookiePage(t *testing.T, addr string) *Page {
	t.Helper()
	ws, err := wsDial("ws://"+addr+"/devtools/browser/fake", 5*time.Second)
	if err != nil {
		t.Fatalf("dial fake cdp: %v", err)
	}
	p := &Page{ws: ws, pending: map[int64]chan cdpMessage{}}
	go p.readLoop()
	return p
}
