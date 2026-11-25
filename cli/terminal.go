//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

// errNoTerminal is a terminal Annex couldn't give (an older Annex, a sandbox
// that can't call back): the shell falls back to running a line at a time.
var errNoTerminal = errors.New("no terminal")

// terminal is a real terminal in the sandbox: every key goes to a shell
// there as it is pressed and what it prints comes back, over one WebSocket
// (GET …/sandboxes/{id}/terminal). So editors, prompts, Tab and Ctrl+C are
// the sandbox's own shell's, and a line costs no request of its own. It
// returns when that shell exits or the connection ends.
func (a *App) terminal(ctx context.Context, id string, tty *os.File) error {
	base, err := a.ws()
	if err != nil {
		return err
	}
	cols, rows := windowSize(tty.Fd())
	fmt.Fprintf(a.Out, "Opening a terminal in %s…\n", id)
	c, err := a.dialWS(ctx, fmt.Sprintf("%s/sandboxes/%s/terminal?cols=%d&rows=%d", base, url.PathEscape(id), cols, rows))
	var e *apiError
	if errors.As(err, &e) && e.Status != 401 && e.Status != 403 && e.Status != 409 {
		// 409 is the sandbox's own state (paused, stopped, setting up): the line shell would say the same.
		fmt.Fprintf(a.Err, "annex: no terminal here (%s). Running a line at a time instead.\n", e.Error())
		return errNoTerminal
	}
	if err != nil {
		return err
	}
	defer c.close()
	restore, err := makeRaw(tty.Fd())
	if err != nil {
		return err
	}
	defer restore()

	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	defer signal.Stop(resized)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-resized:
				cols, rows := windowSize(tty.Fd())
				b, _ := json.Marshal(map[string]any{"resize": map[string]int{"cols": cols, "rows": rows}})
				c.write(wsText, b)
			case <-done:
				return
			}
		}
	}()
	go func() { // keys, as they are pressed. It ends with the process: a read of the keyboard can't be called off.
		buf := make([]byte, 4096)
		for {
			n, err := tty.Read(buf)
			if n > 0 && c.write(wsBinary, buf[:n]) != nil || err != nil {
				return
			}
		}
	}()

	for {
		op, b, err := c.read()
		if err != nil {
			restore()
			if err != io.EOF {
				fmt.Fprintln(a.Err, "\r\nannex: the terminal's connection ended.")
				return exit(1)
			}
			fmt.Fprintln(a.Err, "\r\nannex: the terminal was closed (the sandbox was paused or stopped, or it stayed open 12 hours).")
			return exit(1)
		}
		if op == wsBinary {
			a.Out.Write(b)
			continue
		}
		var m struct {
			Exit  *int
			Error string
		}
		json.Unmarshal(b, &m)
		restore()
		switch {
		case m.Error != "": // the keyboard is being read by now, so no falling back from here
			fmt.Fprintf(a.Err, "annex: no terminal in this sandbox. %s\nRun a line at a time instead: annex shell --lines %s\n", m.Error, id)
			return exit(1)
		case m.Exit != nil && *m.Exit != 0:
			return exit(*m.Exit)
		case m.Exit != nil:
			return nil
		}
	}
}

// windowSize is the terminal's size in characters, or 80 by 24 if it won't say.
func windowSize(fd uintptr) (cols, rows int) {
	var ws struct{ Rows, Cols, X, Y uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 || ws.Cols == 0 || ws.Rows == 0 {
		return 80, 24
	}
	return int(ws.Cols), int(ws.Rows)
}
