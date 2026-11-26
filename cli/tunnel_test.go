package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// annex tunnel: a local port whose connections each open a tunnel and pass
// bytes both ways; one the sandbox refuses is a line on stderr, and the
// listener goes on taking the next.
func TestTunnel(t *testing.T) {
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workspaces/ws_1/sandboxes/sbx_1/tunnel" || r.URL.Query().Get("port") != "5432" || r.Header.Get("Authorization") != "Bearer anx_live_test" {
			http.Error(w, "no", 404)
			return
		}
		conn, rw := acceptWS(w, r)
		defer conn.Close()
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		send := func(op byte, b []byte) { rw.Write(serverFrame(op, b)); rw.Flush() }
		if first {
			send(wsText, []byte(`{"error":"Nothing in this sandbox answered on port 5432."}`))
			return
		}
		send(wsText, []byte(`{"status":"Putting Annex's terminal helper in this sandbox."}`))
		send(wsText, []byte(`{"ready":true}`))
		for { // echo
			op, b, err := clientFrame(rw.Reader)
			if err != nil || op == wsClose {
				return
			}
			send(op, b)
		}
	}))
	defer srv.Close()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	local := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	var out, errs syncBuffer
	a := &App{Out: &out, Err: &errs, HTTP: srv.Client(), cfg: config{URL: srv.URL, Key: "anx_live_test", Workspace: "ws_1"}}
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- a.tunnel(ctx, []string{"sbx_1", "5432", "--local", local}) }()
	dial := func() net.Conn {
		for range 100 {
			if c, err := net.Dial("tcp", "127.0.0.1:"+local); err == nil {
				return c
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("nothing listening on the local port")
		return nil
	}

	refused := dial()
	if b, _ := io.ReadAll(refused); len(b) != 0 {
		t.Fatalf("a refused tunnel passed %q", b)
	}
	c := dial()
	defer c.Close()
	msg := []byte("\x00\xffhello\r\n")
	c.Write(msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("through the tunnel and back: %q %v", got, err)
	}
	cancel()
	if err := <-ended; err != nil {
		t.Fatalf("after Ctrl+C: %v", err)
	}
	if o := out.String(); o != "Forwarding 127.0.0.1:"+local+" to port 5432 in sbx_1. Ctrl+C to stop.\n" {
		t.Fatalf("stdout: %q", o)
	}
	if e := errs.String(); !strings.Contains(e, "Nothing in this sandbox answered on port 5432.") || !strings.Contains(e, "Putting Annex's terminal helper") {
		t.Fatalf("stderr: %q", e)
	}
}

// syncBuffer is a bytes.Buffer goroutines may write to at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A WebSocket server by hand, as in TestWebSocketClient: the published CLI
// is standard library only, its tests included.
func acceptWS(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter) {
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	c, rw, _ := w.(http.Hijacker).Hijack()
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
	rw.Flush()
	return c, rw
}

func serverFrame(op byte, b []byte) []byte { // unmasked, short messages only
	return append([]byte{0x80 | op, byte(len(b))}, b...)
}

func clientFrame(r *bufio.Reader) (byte, []byte, error) { // masked, under 64 KiB
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[1] & 0x7f)
	if n == 126 {
		var b [2]byte
		io.ReadFull(r, b[:])
		n = int(b[0])<<8 | int(b[1])
	}
	var mask [4]byte
	io.ReadFull(r, mask[:])
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	for i := range b {
		b[i] ^= mask[i%4]
	}
	return h[0] & 0x0f, b, nil
}
