package browser

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ws.go is a minimal RFC 6455 client, just enough to speak the DevTools
// Protocol.
//
// It exists instead of golang.org/x/net/websocket for one concrete reason:
// that package refuses to dial without an Origin header, and Chromium's
// DevTools endpoint rejects any WebSocket handshake that carries one (the
// Origin check is what stops a random page from driving the debug port).  A
// hand-rolled client lets us send exactly the headers Chromium expects and
// nothing else.
//
// Scope is deliberately small: text frames, fragmentation, ping/pong and
// close.  CDP never needs extensions, compression or binary payloads.

// wsGUID is the fixed key from RFC 6455 §1.3.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket opcodes.
const (
	wsContinuation = 0x0
	wsText         = 0x1
	wsBinary       = 0x2
	wsClose        = 0x8
	wsPing         = 0x9
	wsPong         = 0xA
)

// wsMaxFrame caps a single frame.  CDP payloads (DOM snapshots, screenshots)
// can be large, but a multi-megabyte single frame means something is wrong.
const wsMaxFrame = 32 << 20

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

// wsDial performs the opening handshake.  The Origin header is intentionally
// absent; see the file comment.
func wsDial(rawURL string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse the websocket URL: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("the websocket URL must start with ws:// (got %q)", u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, fmt.Errorf("connect to the browser: %w", err)
	}
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		conn.Close()
		return nil, fmt.Errorf("generate the websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	var req strings.Builder
	req.WriteString("GET " + path + " HTTP/1.1\r\n")
	req.WriteString("Host: " + u.Host + "\r\n")
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	req.WriteString("\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send the websocket handshake: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read the websocket handshake: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("the browser refused the websocket handshake (HTTP %d)", resp.StatusCode)
	}
	want := base64.StdEncoding.EncodeToString(sha1Sum(key + wsGUID))
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != want {
		conn.Close()
		return nil, errors.New("the browser returned a bad websocket accept key")
	}

	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br}, nil
}

func sha1Sum(s string) []byte {
	h := sha1.Sum([]byte(s))
	return h[:]
}

// cdpTrace dumps websocket frames when C2A_CDP_DEBUG is set.  It is the only
// way to see what the browser actually said when a command goes unanswered.
func cdpTrace(dir string, head, payload []byte) {
	if os.Getenv("C2A_CDP_DEBUG") == "" {
		return
	}
	preview := payload
	if len(preview) > 400 {
		preview = preview[:400]
	}
	fmt.Fprintf(os.Stderr, "[cdp %s] head=%v payload=%q\n", dir, head, preview)
}

func (c *wsConn) close() error { return c.conn.Close() }

// writeText sends one masked text frame.  Client-to-server frames must be
// masked (RFC 6455 §5.3); servers close the connection otherwise.
func (c *wsConn) writeText(payload []byte) error {
	return c.writeFrame(wsText, payload)
}

func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode) // FIN + opcode
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n)|0x80)
	case n <= 0xFFFF:
		header = append(header, 126|0x80, byte(n>>8), byte(n))
	default:
		header = append(header, 127|0x80)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)

	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}

	c.wmu.Lock()
	cdpTrace(">", header, masked)
	defer c.wmu.Unlock()
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.conn.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

// readMessage returns the next complete text message, transparently answering
// pings and skipping pongs.  A close frame ends the connection.
func (c *wsConn) readMessage() ([]byte, error) {
	var assembled []byte
	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case wsPing:
			if err := c.writeFrame(wsPong, payload); err != nil {
				return nil, err
			}
			continue
		case wsPong:
			continue
		case wsClose:
			return nil, io.EOF
		case wsText, wsBinary:
			assembled = append(assembled[:0], payload...)
		case wsContinuation:
			assembled = append(assembled, payload...)
		default:
			continue
		}
		if fin {
			return assembled, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return
	}
	fin = head[0]&0x80 != 0
	opcode = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > wsMaxFrame {
		err = fmt.Errorf("the browser sent an oversized websocket frame (%d bytes)", length)
		return
	}

	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, length)
	if length > 0 {
		if _, err = io.ReadFull(c.br, payload); err != nil {
			return
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	cdpTrace("<", []byte{head[0], head[1]}, payload)
	return
}
