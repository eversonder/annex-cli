package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
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
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		ctx := r.Context()
		if first {
			c.Write(ctx, websocket.MessageText, []byte(`{"error":"Nothing in this sandbox answered on port 5432."}`))
			return
		}
		c.Write(ctx, websocket.MessageText, []byte(`{"status":"Putting Annex's terminal helper in this sandbox."}`))
		c.Write(ctx, websocket.MessageText, []byte(`{"ready":true}`))
		for { // echo
			kind, b, err := c.Read(ctx)
			if err != nil || c.Write(ctx, kind, b) != nil {
				return
			}
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
