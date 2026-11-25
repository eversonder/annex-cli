package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// annexStandIn is enough of Annex for the command: the CLI sign-in, one
// sandbox whose commands run with this machine's sh in a scratch folder, its
// output as an event stream, and its files.
type annexStandIn struct {
	t        *testing.T
	mu       sync.Mutex
	polls    int
	cmds     map[string]*exec.Cmd
	out, err map[string]*bytes.Buffer
	root     string
	waited   int // commands answered whole, with ?wait=, as Annex does when one ends in time
	streams  int
}

const (
	testKey = "anx_live_test"
	testWS  = "wsp_test"
	testSbx = "sbx_test00000001"
)

func (s *annexStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	say := func(code int, v any) { w.WriteHeader(code); json.NewEncoder(w).Encode(v) }
	p := r.URL.Path
	switch {
	case p == "/v1/auth/cli/start":
		say(201, map[string]any{"cli_login": map[string]any{"device_code": "anx_dev_x", "user_code": "ABCD-EFGH", "verify_url": "http://annex.test/dashboard/cli/ABCD-EFGH", "interval_seconds": 0}})
		return
	case p == "/v1/auth/cli/token":
		s.mu.Lock()
		s.polls++
		n := s.polls
		s.mu.Unlock()
		if n < 2 {
			say(202, map[string]any{"status": "pending"})
			return
		}
		say(200, map[string]any{"cli_login": map[string]any{"api_key": map[string]any{"secret": testKey}, "workspace": map[string]any{"id": testWS, "name": "Acme"}}})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testKey {
		say(401, map[string]any{"code": "unauthorized", "message": "no"})
		return
	}
	base := "/v1/workspaces/" + testWS + "/sandboxes"
	switch {
	case p == "/v1/key":
		say(200, map[string]any{"key": map[string]any{"id": "key_1", "workspace": map[string]any{"id": testWS, "name": "Acme"}}})
	case p == "/v1/workspaces/"+testWS+"/connections" && r.Method == "GET":
		say(200, map[string]any{"connections": []any{map[string]any{"id": "con_ns", "provider": "namespace", "method": "api_key", "label": "Namespace", "key_problem": "Namespace didn't accept this token."}}})
	case p == "/v1/workspaces/"+testWS+"/connections/con_ns/check" && r.Method == "POST":
		say(200, map[string]any{"connection": map[string]any{"id": "con_ns", "provider": "namespace", "label": "Namespace", "key_problem": "Namespace didn't accept this token."}})
	case p == base && r.Method == "GET":
		say(200, map[string]any{"sandboxes": []any{map[string]any{"id": testSbx, "status": "running", "provider": "fake", "vcpu": 2, "memory_gib": 4}}})
	case p == base+"/"+testSbx+"/commands" && r.Method == "POST":
		var in struct{ Command, Cwd string }
		json.NewDecoder(r.Body).Decode(&in)
		c := exec.Command("sh", "-c", in.Command)
		c.Dir = s.root
		if in.Cwd != "" {
			c.Dir = in.Cwd
		}
		s.mu.Lock()
		id := fmt.Sprintf("cmd_%d", len(s.cmds)+1)
		s.cmds[id], s.out[id], s.err[id] = c, &bytes.Buffer{}, &bytes.Buffer{}
		c.Stdout, c.Stderr = s.out[id], s.err[id]
		s.mu.Unlock()
		if r.URL.Query().Get("wait") != "" && !strings.Contains(in.Command, "sleep") { // ends in time: the answer carries it
			code := 0
			if err := c.Run(); err != nil {
				code = c.ProcessState.ExitCode()
			}
			s.mu.Lock()
			s.waited++
			out := s.out[id].String() + s.err[id].String()
			s.mu.Unlock()
			say(201, map[string]any{"command": map[string]any{"id": id, "status": "exited", "exit_code": code, "output": out}})
			return
		}
		say(201, map[string]any{"command": map[string]any{"id": id, "status": "running"}})
	case strings.HasSuffix(p, "/stream"):
		id := strings.Split(strings.TrimPrefix(p, base+"/"+testSbx+"/commands/"), "/")[0]
		s.mu.Lock()
		s.streams++
		c, out, errs := s.cmds[id], s.out[id], s.err[id]
		s.mu.Unlock()
		code := 0
		if err := c.Run(); err != nil {
			code = c.ProcessState.ExitCode()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(name string, v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\nid: 1\ndata: %s\n\n", name, b)
		}
		// In small pieces, as a provider sends it: the shell's folder marker may be split across them.
		for text := out.String(); len(text) > 0; {
			n := min(len(text), 7)
			event("output", map[string]string{"stream": "stdout", "text": text[:n]})
			text = text[n:]
		}
		if errs.Len() > 0 {
			event("output", map[string]string{"stream": "stderr", "text": errs.String()})
		}
		event("exit", map[string]any{"id": id, "status": "exited", "exit_code": code})
	case strings.HasSuffix(p, "/files/content") && r.Method == "PUT":
		b, _ := io.ReadAll(r.Body)
		os.WriteFile(r.URL.Query().Get("path"), b, 0o644)
		w.WriteHeader(204)
	case strings.HasSuffix(p, "/files/content"):
		b, err := os.ReadFile(r.URL.Query().Get("path"))
		if err != nil {
			say(404, map[string]any{"code": "not_found", "message": "no such file"})
			return
		}
		w.Write(b)
	default:
		say(404, map[string]any{"code": "not_found", "message": "sandbox not found"})
	}
}

// annexEnv is annex with more environment variables, and no stdin.
func annexEnv(t *testing.T, srv *httptest.Server, dir string, env map[string]string, args ...string) (code int, out, errs string) {
	t.Helper()
	var o, e bytes.Buffer
	app := &App{In: strings.NewReader(""), Out: &o, Err: &e, Dir: dir, HTTP: srv.Client(), NoFile: true,
		Env: func(k string) string {
			if k == "ANNEX_URL" {
				return srv.URL
			}
			return env[k]
		}}
	code = app.Run(context.Background(), args)
	return code, o.String(), e.String()
}

// annex runs the command against the stand-in, with a config folder of its own.
func annex(t *testing.T, srv *httptest.Server, dir, stdin string, args ...string) (code int, out, errs string) {
	t.Helper()
	var o, e bytes.Buffer
	app := &App{In: strings.NewReader(stdin), Out: &o, Err: &e, Dir: dir, HTTP: srv.Client(), NoFile: true,
		Env: func(k string) string { return map[string]string{"ANNEX_URL": srv.URL}[k] }}
	code = app.Run(context.Background(), args)
	return code, o.String(), e.String()
}

func TestTheCommand(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	s := &annexStandIn{t: t, cmds: map[string]*exec.Cmd{}, out: map[string]*bytes.Buffer{}, err: map[string]*bytes.Buffer{}, root: root}
	srv := httptest.NewServer(s)
	defer srv.Close()
	dir := t.TempDir()

	// Before signing in, a command says how to.
	if code, _, errs := annex(t, srv, dir, "", "ls"); code != 1 || !strings.Contains(errs, "annex login") {
		t.Fatalf("not signed in: %d %q", code, errs)
	}
	// Signing in: the code and address are shown, it waits while pending, and the key is kept for this user only.
	code, out, errs := annex(t, srv, dir, "", "login")
	if code != 0 || !strings.Contains(out, "ABCD-EFGH") || !strings.Contains(out, "http://annex.test/dashboard/cli/ABCD-EFGH") || !strings.Contains(out, "Signed in to Acme") {
		t.Fatalf("login: %d %q %q", code, out, errs)
	}
	st, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the key's file: %v, mode %v; want it readable by this user only", err, st.Mode().Perm())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.json")); !strings.Contains(string(b), testKey) || !strings.Contains(string(b), testWS) {
		t.Fatalf("config: %s", b)
	}

	// run: stdout and stderr go to their own streams, and the command's exit code is annex's.
	code, out, errs = annex(t, srv, dir, "", "run", testSbx, "--", "echo one; echo two >&2; exit 3")
	if code != 3 || out != "one\n" || errs != "two\n" {
		t.Fatalf("run: %d %q %q", code, out, errs)
	}
	// With one sandbox running the id may be left out.
	if code, out, _ = annex(t, srv, dir, "", "run", "--", "echo", "alone"); code != 0 || out != "alone\n" {
		t.Fatalf("run without an id: %d %q", code, out)
	}

	// shell: cd carries over to the next line, output passes through, and the folder marker never shows.
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	code, out, errs = annex(t, srv, dir, "pwd\ncd sub\npwd\nprintf no-newline\nfalse\necho still here\nexit\n", "shell", testSbx)
	if code != 0 || strings.Contains(out, "__annex") || !strings.Contains(out, root+"\n") || !strings.Contains(out, root+"/sub\n") ||
		!strings.Contains(out, ":"+root+"/sub$ ") || !strings.Contains(out, "no-newline\n") || !strings.Contains(out, "still here\n") {
		t.Fatalf("shell: %d\n%s\n%s", code, out, errs)
	}

	// A shell line that ends within quickWait comes back in the one answer, never streamed; a slow one streams.
	s.waited, s.streams = 0, 0
	code, out, _ = annex(t, srv, dir, "echo quick\nsleep 0.1; echo slow\nexit\n", "shell", testSbx)
	if code != 0 || !strings.Contains(out, "quick\n") || !strings.Contains(out, "slow\n") || strings.Contains(out, "__annex") || s.waited < 2 || s.streams != 1 {
		t.Fatalf("shell, quick lines: %d %q (answered whole %d, streamed %d)", code, out, s.waited, s.streams)
	}

	// An exported variable carries over to the next line too, and cd - goes back, as in a real shell.
	code, out, errs = annex(t, srv, dir, "export GREETING='hello there'\ncd sub\necho \"$GREETING from $(basename $PWD)\"\ncd - >/dev/null\nbasename $PWD\nexit\n", "shell", testSbx)
	if code != 0 || !strings.Contains(out, "hello there from sub\n") || !strings.Contains(out, filepath.Base(root)+"\n") || strings.Contains(out, "__annex") {
		t.Fatalf("shell, variables: %d\n%s\n%s", code, out, errs)
	}

	// cp: a file in, and the same bytes out.
	local := filepath.Join(t.TempDir(), "in.txt")
	os.WriteFile(local, []byte("the file"), 0o644)
	if code, _, errs = annex(t, srv, dir, "", "cp", local, testSbx+":"+root+"/up.txt"); code != 0 {
		t.Fatalf("cp up: %s", errs)
	}
	back := filepath.Join(t.TempDir(), "out.txt")
	if code, _, errs = annex(t, srv, dir, "", "cp", testSbx+":"+root+"/up.txt", back); code != 0 {
		t.Fatalf("cp down: %s", errs)
	}
	if b, _ := os.ReadFile(back); string(b) != "the file" {
		t.Fatalf("cp: got %q back", b)
	}
	// A download that fails leaves no file behind under the name asked for.
	missing := filepath.Join(t.TempDir(), "never.txt")
	if code, _, _ = annex(t, srv, dir, "", "cp", testSbx+":"+root+"/nope", missing); code == 0 {
		t.Fatal("cp of a missing file succeeded")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("a failed download left a file")
	}

	// providers says a refused key, check asks again, and adding or replacing a key sends the person to the dashboard.
	if code, out, _ = annex(t, srv, dir, "", "providers"); code != 0 || !strings.Contains(out, "refused") || !strings.Contains(out, "annex providers key") {
		t.Fatalf("providers: %d %q", code, out)
	}
	if code, out, _ = annex(t, srv, dir, "", "providers", "check", "con_ns"); code != 1 || !strings.Contains(out, "didn't accept") {
		t.Fatalf("providers check: %d %q", code, out)
	}
	if code, out, _ = annex(t, srv, dir, "", "providers", "key", "con_ns"); code != 0 || !strings.Contains(out, "/dashboard/w/"+testWS+"/connections?key=con_ns") {
		t.Fatalf("providers key: %d %q", code, out)
	}

	// doctor says each part in turn and how long it took: here the account's refused key is the one problem,
	// and the sandbox runs a command. With ANNEX_TIMING each request is timed on stderr.
	code, out, errs = annexEnv(t, srv, dir, map[string]string{"ANNEX_TIMING": "1"}, "doctor", testSbx)
	if code != 1 || !strings.Contains(out, "problem  account") || !strings.Contains(out, "didn't accept") ||
		!strings.Contains(out, "ok       sandbox    "+testSbx+": ran a command") || !strings.Contains(out, "ok       key") ||
		!strings.Contains(errs, "timing: GET /v1/key 200 in") {
		t.Fatalf("doctor: %d\n%s\n%s", code, out, errs)
	}

	// logout forgets the key here.
	if code, _, _ = annex(t, srv, dir, "", "logout"); code != 0 {
		t.Fatal("logout failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("logout left the key")
	}
}

// The line editor, driven key by key as a terminal would: typing, moving and
// cutting, the history, Tab, Ctrl+C and Ctrl+D.
func TestLineEditor(t *testing.T) {
	read := func(keys string, e *editor) (string, error, string) {
		var out bytes.Buffer
		e.in, e.out = bufio.NewReader(strings.NewReader(keys)), &out
		line, err := e.edit("$ ")
		return line, err, out.String()
	}
	for _, c := range []struct{ name, keys, want string }{
		{"typing", "echo hi\r", "echo hi"},
		{"left arrow, then insert", "eco\x1b[Dh\r", "echo"},
		{"home and end", "cho\x1b[He\x1b[F!\r", "echo!"},
		{"backspace and delete", "echoo\x7f x\x1b[D\x1b[3~\r", "echo "},
		{"ctrl+a, ctrl+k", "echo hi\x01\x0b\r", ""},
		{"ctrl+u cuts to the start", "junk\x15ls\r", "ls"},
		{"ctrl+w cuts a word", "ls one two\x17\r", "ls one "},
		{"ctrl+c drops the line", "oops\x03ls\r", "ls"},
		{"up recalls the last line", "\x1b[A\r", "git status"},
		{"up twice, down once", "\x1b[A\x1b[A\x1b[B\r", "git status"},
		{"up then back to what was typed", "dra\x1b[A\x1b[Bft\r", "draft"},
		{"tab completes the one match", "cat rep\t\r", "cat report.csv "},
		{"tab stops at what the matches share", "cat no\t\r", "cat notes."},
		{"tab into a folder adds no space", "cd sr\t\r", "cd src/"},
		{"an unknown escape is ignored", "ls\x1b[9~\r", "ls"},
		{"accents count as one character", "café\x7f\x7fe\r", "cae"},
	} {
		e := &editor{history: []string{"make test", "git status"}, complete: func(word string) []string {
			return map[string][]string{"rep": {"report.csv"}, "no": {"notes.md", "notes.txt"}, "sr": {"src/"}}[word]
		}}
		if got, err, _ := read(c.keys, e); err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	e := &editor{}
	if _, err, _ := read("\x04", e); err != io.EOF {
		t.Errorf("ctrl+d on an empty line: %v; want it to leave", err)
	}
	// A second Tab with nothing more to add lists the choices; a line that was run joins the history once.
	e = &editor{complete: func(string) []string { return []string{"notes.md", "notes.txt"} }}
	if _, _, out := read("cat notes\t\t\r", e); !strings.Contains(out, "notes.md  notes.txt") {
		t.Errorf("two tabs didn't list the choices: %q", out)
	}
	read("ls\r", e)
	read("ls\r", e)
	if len(e.history) != 2 || e.history[1] != "ls" {
		t.Errorf("history: %q; want the line once", e.history)
	}
}

// An annex.yml: what parses, what it becomes, and that a template written
// back out reads the same; what doesn't parse says which line and why.
func TestTemplateFile(t *testing.T) {
	src := `# our web projects
name: node-dev
description: "Node 22, pnpm and ripgrep"
route: cheapest   # or fastest
vcpu: 2
memory_gib: 4
default: true
secret_groups: [github, npm]
setup:
  - npm install -g pnpm
  - "echo 'quoted: with a colon'"
  - |
    apt-get update
    apt-get install -y ripgrep
`
	got, err := parseTemplate(src)
	want := template{Name: "node-dev", Description: "Node 22, pnpm and ripgrep", Route: "cheapest", VCPU: 2, MemoryGiB: 4, Default: true,
		SecretGroups: []string{"github", "npm"},
		Setup:        []string{"npm install -g pnpm", "echo 'quoted: with a colon'", "apt-get update\napt-get install -y ripgrep"}}
	if err != nil || fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("parse:\n got %+v\nwant %+v\n%v", got, want, err)
	}
	if again, err := parseTemplate(got.yml()); err != nil || fmt.Sprintf("%+v", again) != fmt.Sprintf("%+v", want) {
		t.Fatalf("written back out and read again:\n%s\n got %+v, %v", got.yml(), again, err)
	}
	for src, why := range map[string]string{
		"vcpu: 2\n":                          "needs a name",
		"name: x\nvcpu: two\n":               "line 2: vcpu is a whole number",
		"name: x\nimage: ubuntu\n":           `line 2: "image" isn't something a template has`,
		"name: x\nsetup: npm i\n":            "line 2: setup is a list",
		"name: x\n  nested: y\n":             "line 2: expected a line like",
		"name: x\ndefault: yes\n":            "line 2: default is true or false",
		"name: x\nsetup:\n  - a\nwhat now\n": "line 4: expected a line like",
	} {
		if _, err := parseTemplate(src); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v; want %q", src, err, why)
		}
	}
}

// "annex connect" hands a coding agent this computer's key without anyone
// pasting it: Claude Code through its own command, Cursor through its file,
// where everything else in the file stays and a project's file holds no key.
func TestConnectAnAgent(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"url":"http://annex.test","api_key":"anx_live_secret","workspace":"wsp_a"}`), 0o600)
	var ran [][]string
	var out bytes.Buffer
	app := func() *App {
		out.Reset()
		return &App{In: strings.NewReader(""), Out: &out, Err: &out, Dir: dir, Home: home, NoFile: true, Env: func(string) string { return "" },
			Exec: func(name string, args ...string) error {
				ran = append(ran, append([]string{name}, args...))
				return nil
			}}
	}
	if code := app().Run(context.Background(), []string{"connect", "claude"}); code != 0 || len(ran) != 2 {
		t.Fatalf("connect claude: %d, ran %v, %s", code, ran, out.String())
	}
	if got := strings.Join(ran[1], " "); got != "claude mcp add --transport http --scope user annex http://annex.test/mcp --header Authorization: Bearer anx_live_secret" {
		t.Fatalf("it ran %q", got)
	}
	if strings.Contains(out.String(), "anx_live_secret") {
		t.Fatal("the key was printed")
	}

	file := filepath.Join(home, ".cursor", "mcp.json")
	os.MkdirAll(filepath.Dir(file), 0o700)
	os.WriteFile(file, []byte(`{"mcpServers":{"other":{"url":"https://other.test"}},"theirs":true}`), 0o644)
	if code := app().Run(context.Background(), []string{"connect", "cursor"}); code != 0 {
		t.Fatalf("connect cursor: %s", out.String())
	}
	var doc struct {
		Theirs     bool
		McpServers map[string]struct {
			URL     string
			Headers map[string]string
		}
	}
	b, _ := os.ReadFile(file)
	if json.Unmarshal(b, &doc); !doc.Theirs || doc.McpServers["other"].URL != "https://other.test" || doc.McpServers["annex"].URL != "http://annex.test/mcp" || doc.McpServers["annex"].Headers["Authorization"] != "Bearer anx_live_secret" {
		t.Fatalf("the file: %s", b)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("the file holds a key and is mode %v; want it readable by this user only", st.Mode().Perm())
	}
	// A file that isn't JSON is never overwritten.
	os.WriteFile(file, []byte(`{ not json`), 0o600)
	if code := app().Run(context.Background(), []string{"connect", "cursor"}); code == 0 || !strings.Contains(out.String(), "left it alone") {
		t.Fatalf("a broken file: %d %s", code, out.String())
	}
	if b, _ := os.ReadFile(file); string(b) != `{ not json` {
		t.Fatal("a broken file was overwritten")
	}
	// A project's file names the variable, never the key.
	t.Chdir(t.TempDir())
	if code := app().Run(context.Background(), []string{"connect", "cursor", "--project"}); code != 0 {
		t.Fatalf("connect cursor --project: %s", out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(".cursor", "mcp.json")); strings.Contains(string(b), "anx_live_secret") || !strings.Contains(string(b), "${env:ANNEX_API_KEY}") {
		t.Fatalf("the project's file: %s", b)
	}
}

// A quick command's stdout and stderr come back as one text, stderr last, so
// the folder marker is not the end of it: the folder is the marker's line only.
func TestCwdTapStderrAfterMarker(t *testing.T) {
	var out bytes.Buffer
	tap := &cwdTap{w: &out, mark: "__annex_cwd__"}
	io.WriteString(tap, "\n__annex_cwd__/root\n\nWARNING: apt\n\nE: Unable to locate package go\n")
	tap.flush()
	if tap.cwd != "/root" || out.String() != "\nWARNING: apt\n\nE: Unable to locate package go\n" {
		t.Fatalf("cwd %q, passed on %q", tap.cwd, out.String())
	}
}
