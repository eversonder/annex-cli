package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// tunnel is "annex tunnel <id> <port> [--local <port>]": a port on this
// computer (127.0.0.1 only) that reaches 127.0.0.1:<port> in the sandbox,
// as ssh -L does. Each connection made to it is one WebSocket to
// GET …/sandboxes/{id}/tunnel?port=, its bytes passed both ways.
// ponytail: anything on this computer can reach the local port, a web page
// through the browser included, as with ssh -L.
func (a *App) tunnel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	local := fs.Int("local", 0, "the port on this computer (the sandbox's port if left out)")
	var pos []string
	for { // flags may come after the id and the port
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	id, rest, err := a.pick(ctx, pos)
	if err != nil {
		return err
	}
	port := 0
	if len(rest) == 1 {
		port, _ = strconv.Atoi(rest[0])
	}
	if port < 1 || port > 65535 || *local < 0 || *local > 65535 {
		return errors.New("say which port: annex tunnel <id> <port> [--local <port>]")
	}
	base, err := a.ws()
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cmp.Or(*local, port))))
	if err != nil {
		return err
	}
	defer l.Close()
	context.AfterFunc(ctx, func() { l.Close() })
	fmt.Fprintf(a.Out, "Forwarding %s to port %d in %s. Ctrl+C to stop.\n", l.Addr(), port, id)
	path := fmt.Sprintf("%s/sandboxes/%s/tunnel?port=%d", base, url.PathEscape(id), port)
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			if err := a.forward(ctx, path, conn); err != nil {
				fmt.Fprintln(a.Err, "annex: a connection through the tunnel:", err)
			}
		}()
	}
}

// forward passes one local connection's bytes through its own tunnel until either side ends.
func (a *App) forward(ctx context.Context, path string, conn net.Conn) error {
	defer conn.Close()
	c, err := a.dialWS(ctx, path)
	if err != nil {
		return err
	}
	defer c.close()
	for ready := false; !ready; {
		op, b, err := c.read()
		if err != nil {
			return errors.New("the connection ended before the tunnel opened")
		}
		var m struct {
			Status, Error string
			Ready         bool
		}
		if op == wsText {
			json.Unmarshal(b, &m)
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if m.Status != "" {
			fmt.Fprintln(a.Err, m.Status)
		}
		ready = m.Ready
	}
	go func() { // this side's bytes, to the sandbox
		defer c.close()
		buf := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 && c.write(wsBinary, buf[:n]) != nil || err != nil {
				return
			}
		}
	}()
	for {
		op, b, err := c.read()
		if err != nil {
			return nil // either side closed
		}
		if op == wsBinary {
			if _, err := conn.Write(b); err != nil {
				return nil
			}
			continue
		}
		var m struct{ Error string }
		if json.Unmarshal(b, &m) == nil && m.Error != "" {
			return errors.New(m.Error)
		}
	}
}
