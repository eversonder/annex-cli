package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
)

// A WebSocket client, as much of RFC 6455 as a terminal needs: this command
// uses the standard library only, which has none. Whole messages, text or
// binary, no extensions; pings are answered as they are read.

const (
	wsText, wsBinary, wsClose, wsPing, wsPong = 1, 2, 8, 9, 10
	wsMax                                     = 4 << 20 // the most one message may be
)

type wsConn struct {
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex // one frame is written at a time
}

// dialWS opens a WebSocket at p on Annex, signed in with this computer's
// key. An answer other than 101 comes back as an *apiError.
func (a *App) dialWS(ctx context.Context, p string) (*wsConn, error) {
	u, err := url.Parse(a.cfg.URL + p)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), map[string]string{"https": "443"}[u.Scheme]+map[string]string{"http": "80"}[u.Scheme])
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s: %w", a.cfg.URL, err)
	}
	if u.Scheme == "https" {
		t := tls.Client(c, &tls.Config{ServerName: u.Hostname(), NextProtos: []string{"http/1.1"}})
		if err := t.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, fmt.Errorf("couldn't reach %s: %w", a.cfg.URL, err)
		}
		c = t
	}
	key := make([]byte, 16)
	rand.Read(key)
	k := base64.StdEncoding.EncodeToString(key)
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header = http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}, "Sec-WebSocket-Version": {"13"}, "Sec-WebSocket-Key": {k},
		"Authorization": {"Bearer " + a.cfg.Key}, "User-Agent": {"annex-cli/" + Version}}
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	r := bufio.NewReader(c)
	res, err := http.ReadResponse(r, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		defer c.Close()
		e := &apiError{Status: res.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		json.Unmarshal(b, e)
		return nil, e
	}
	sum := sha1.Sum([]byte(k + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if res.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		c.Close()
		return nil, errors.New("Annex didn't answer as a WebSocket")
	}
	return &wsConn{c: c, r: r}, nil
}

// write sends one message (or control frame), masked as a client's must be.
func (w *wsConn) write(op byte, b []byte) error {
	head := []byte{0x80 | op, 0x80}
	switch n := len(b); {
	case n < 126:
		head[1] |= byte(n)
	case n < 1<<16:
		head[1] |= 126
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head[1] |= 127
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	var mask [4]byte
	rand.Read(mask[:])
	out := append(head, mask[:]...)
	for i, c := range b {
		out = append(out, c^mask[i%4])
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.c.Write(out)
	return err
}

// read is the next whole message: its kind (wsText or wsBinary) and bytes.
// io.EOF once the other side has closed.
func (w *wsConn) read() (op byte, msg []byte, err error) {
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.r, h[:]); err != nil {
			return 0, nil, err
		}
		fin, code, n := h[0]&0x80 != 0, h[0]&0x0f, uint64(h[1]&0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if h[1]&0x80 != 0 || n > wsMax || uint64(len(msg))+n > wsMax {
			return 0, nil, errors.New("a WebSocket frame Annex shouldn't send")
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(w.r, b); err != nil {
			return 0, nil, err
		}
		switch code {
		case wsPing:
			w.write(wsPong, b)
		case wsPong:
		case wsClose:
			w.write(wsClose, nil)
			return 0, nil, io.EOF
		default: // text or binary, or (0) more of the message under way
			if code != 0 {
				op = code
			}
			if msg = append(msg, b...); fin {
				return op, msg, nil
			}
		}
	}
}

func (w *wsConn) close() {
	w.write(wsClose, nil)
	w.c.Close()
}
