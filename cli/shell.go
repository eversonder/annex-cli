package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// shell is a terminal in the sandbox. Each line runs as a command there, and
// what a real shell keeps between lines is kept here too: the folder (cd), and
// exported variables, which the sandbox saves after each line and loads before
// the next. In a real terminal the line is edited as a shell's is: arrows,
// history (kept between sessions), Tab to complete file names, Ctrl+C to drop
// a line or stop a running command, Ctrl+D or exit to leave.
//
// It is not a full terminal: Annex has no way to send keys to a running
// program, so a program that asks a question or draws the whole screen (an
// editor, top) can't be used through it.
func (a *App) shell(ctx context.Context, args []string) error {
	id, _, err := a.pick(ctx, args)
	if err != nil {
		return err
	}
	cwd, err := a.quiet(ctx, id, "pwd", "")
	if err != nil {
		return err
	}
	cwd = strings.TrimSpace(cwd)
	env := "/tmp/.annex-shell-" + strings.ToLower(rand.Text()[:10]) + ".env" // this session's exported variables
	defer a.quiet(context.WithoutCancel(ctx), id, "rm -f "+env, "")

	// A real terminal gets the line editor; piped input is read a line at a time.
	var ed *editor
	if f, ok := a.In.(*os.File); ok && isTerminal(f.Fd()) {
		ed = &editor{in: bufio.NewReader(f), out: a.Out, fd: f.Fd(), history: loadHistory(a.historyFile())}
		ed.complete = func(word string) []string {
			out, _ := a.quiet(ctx, id, "ls -dp -- "+shellQuote(word)+"* 2>/dev/null | head -50", cwd)
			return strings.Fields(out)
		}
		fmt.Fprintf(a.Out, "Connected to %s. Tab completes file names, ↑ recalls earlier lines, Ctrl+C stops a running command, exit or Ctrl+D leaves.\n", id)
	} else {
		fmt.Fprintf(a.Out, "Connected to %s. cd works, Ctrl+C stops a running command, exit leaves.\n", id)
	}
	lines := bufio.NewReader(a.In)
	for {
		var line string
		var err error
		if ed != nil {
			line, err = ed.read(fmt.Sprintf("\x1b[32m%s\x1b[0m:\x1b[34m%s\x1b[0m$ ", id[:min(len(id), 12)], cwd))
		} else {
			fmt.Fprintf(a.Out, "%s:%s$ ", id[:min(len(id), 12)], cwd)
			line, err = lines.ReadString('\n')
		}
		line = strings.TrimSpace(line)
		if line == "exit" || line == "logout" || (err != nil && line == "") {
			if ed == nil {
				fmt.Fprintln(a.Out)
			} else {
				saveHistory(a.historyFile(), ed.history)
			}
			return nil
		}
		if line == "" {
			continue
		}
		if line == "clear" && ed != nil {
			io.WriteString(a.Out, "\x1b[H\x1b[2J")
			continue
		}
		// What the line leaves behind (its folder, its exports) is where the next one starts.
		const mark = "__annex_cwd__"
		tap := &cwdTap{w: a.Out, mark: mark}
		prev := a.Out
		a.Out = tap
		_, runErr := a.exec(ctx, id, "{ [ -f "+env+" ] && . "+env+"; } 2>/dev/null\n"+line+
			"\n__annex_rc=$?; export -p > "+env+" 2>/dev/null; printf '\\n"+mark+"%s\\n' \"$PWD\"; exit $__annex_rc", cwd, 0)
		a.Out = prev
		tap.flush()
		if tap.cwd != "" {
			cwd = tap.cwd
		}
		if runErr != nil {
			fmt.Fprintln(a.Err, "annex:", runErr)
			var e *apiError
			if errors.As(runErr, &e) && (e.Code == "sandbox_stopped" || e.Code == "sandbox_lost" || e.Status == 404) {
				return exit(1)
			}
		}
	}
}

// quiet runs a command and answers what it printed, showing nothing.
func (a *App) quiet(ctx context.Context, id, line, cwd string) (string, error) {
	var buf bytes.Buffer
	out, errs := a.Out, a.Err
	a.Out, a.Err = &buf, io.Discard
	_, err := a.exec(ctx, id, line, cwd, 30)
	a.Out, a.Err = out, errs
	return buf.String(), err
}

// shellQuote is s as one word to the sandbox's shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (a *App) historyFile() string { return filepath.Join(filepath.Dir(a.file()), "history") }

const historyKept = 1000

func loadHistory(file string) []string {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func saveHistory(file string, h []string) {
	if len(h) > historyKept {
		h = h[len(h)-historyKept:]
	}
	if len(h) > 0 && os.MkdirAll(filepath.Dir(file), 0o700) == nil {
		os.WriteFile(file, []byte(strings.Join(h, "\n")+"\n"), 0o600) // commands can hold secrets: this user only
	}
}

// editor reads one line from a terminal in raw mode, the way a shell does:
// arrows and Ctrl+A/E move, Ctrl+U/K/W cut, up and down walk the history, Tab
// completes the word under the cursor.
// ponytail: it draws the line on one row, so a line longer than the window wraps untidily; and every character counts as one column.
type editor struct {
	in       *bufio.Reader
	out      io.Writer
	fd       uintptr
	history  []string
	complete func(word string) []string // nil: Tab does nothing
}

func (e *editor) read(prompt string) (string, error) {
	if restore, err := makeRaw(e.fd); err == nil {
		defer restore()
	}
	return e.edit(prompt)
}

// edit is read without touching the terminal's mode, so a test can drive it.
func (e *editor) edit(prompt string) (string, error) {
	var buf []rune
	pos, at, draft := 0, len(e.history), ""
	tabbed := false
	draw := func() {
		fmt.Fprintf(e.out, "\r%s%s\x1b[K", prompt, string(buf))
		if back := len(buf) - pos; back > 0 {
			fmt.Fprintf(e.out, "\x1b[%dD", back)
		}
	}
	set := func(s string) { buf = []rune(s); pos = len(buf) }
	walk := func(to int) { // to another line of the history; the line being typed is its last
		if to < 0 || to > len(e.history) {
			return
		}
		if at == len(e.history) {
			draft = string(buf)
		}
		if at = to; at == len(e.history) {
			set(draft)
		} else {
			set(e.history[at])
		}
	}
	draw()
	for {
		r, _, err := e.in.ReadRune()
		if err != nil {
			return string(buf), err
		}
		wasTab := tabbed
		tabbed = false
		switch r {
		case '\r', '\n':
			io.WriteString(e.out, "\r\n")
			line := string(buf)
			if s := strings.TrimSpace(line); s != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != s) {
				e.history = append(e.history, s)
			}
			return line, nil
		case 3: // Ctrl+C: drop the line
			io.WriteString(e.out, "^C\r\n")
			buf, pos, at = nil, 0, len(e.history)
		case 4: // Ctrl+D: leave on an empty line, else delete under the cursor
			if len(buf) == 0 {
				io.WriteString(e.out, "\r\n")
				return "", io.EOF
			}
			if pos < len(buf) {
				buf = append(buf[:pos], buf[pos+1:]...)
			}
		case 127, 8:
			if pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
			}
		case 1:
			pos = 0
		case 5:
			pos = len(buf)
		case 2:
			pos = max(pos-1, 0)
		case 6:
			pos = min(pos+1, len(buf))
		case 11:
			buf = buf[:pos]
		case 21:
			buf, pos = buf[pos:], 0
		case 23: // Ctrl+W: the word before the cursor
			i := pos
			for i > 0 && buf[i-1] == ' ' {
				i--
			}
			for i > 0 && buf[i-1] != ' ' {
				i--
			}
			buf, pos = append(buf[:i], buf[pos:]...), i
		case 12:
			io.WriteString(e.out, "\x1b[H\x1b[2J")
		case 16:
			walk(at - 1)
		case 14:
			walk(at + 1)
		case '\t':
			if e.complete == nil {
				break
			}
			start := pos
			for start > 0 && buf[start-1] != ' ' {
				start--
			}
			word := string(buf[start:pos])
			found := e.complete(word)
			if len(found) == 0 {
				break
			}
			to := commonPrefix(found)
			if len(found) == 1 && !strings.HasSuffix(to, "/") {
				to += " "
			}
			if to != word {
				buf = append(append(append([]rune{}, buf[:start]...), []rune(to)...), buf[pos:]...)
				pos = start + len([]rune(to))
			} else if wasTab { // a second Tab with nothing more to add: show the choices
				fmt.Fprintf(e.out, "\r\n%s\r\n", strings.Join(found, "  "))
			}
			tabbed = true
		case 27: // an escape sequence: arrows, Home, End, Delete
			switch key := e.escape(); key {
			case "A":
				walk(at - 1)
			case "B":
				walk(at + 1)
			case "C":
				pos = min(pos+1, len(buf))
			case "D":
				pos = max(pos-1, 0)
			case "H", "1~", "7~":
				pos = 0
			case "F", "4~", "8~":
				pos = len(buf)
			case "3~":
				if pos < len(buf) {
					buf = append(buf[:pos], buf[pos+1:]...)
				}
			}
		default:
			if r >= 32 {
				buf = append(buf[:pos], append([]rune{r}, buf[pos:]...)...)
				pos++
			}
		}
		draw()
	}
}

// escape reads the rest of an escape sequence and answers its last part: "A"
// for up, "3~" for Delete; "" for one it doesn't know.
func (e *editor) escape() string {
	r, _, err := e.in.ReadRune()
	if err != nil || (r != '[' && r != 'O') {
		return ""
	}
	key := ""
	for range 8 {
		r, _, err := e.in.ReadRune()
		if err != nil {
			return ""
		}
		key += string(r)
		if r >= '@' && r <= '~' {
			return key
		}
	}
	return ""
}

func commonPrefix(list []string) string {
	p := list[0]
	for _, s := range list[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}
