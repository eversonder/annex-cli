// Package cli is the annex command: Annex from a terminal, for people who'd
// rather not open the dashboard. "annex login" is approved once in a browser
// and leaves an API key in ~/.config/annex/config.json; every other command
// is the public HTTP API with that key, the same one agents use.
//
// Standard library only, and nothing of Annex's own: it is built for people's
// computers and must stay small and auditable.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Version is this build of the command.
const Version = "0.9.0"

// DefaultURL is where Annex lives; ANNEX_URL or login --url say otherwise.
const DefaultURL = "https://annex.eversonder.com"

const usage = `annex: Annex from your terminal

  annex login                 sign in (approve once in your browser)
  annex login --key anx_…     sign in with an API key you already have
  annex logout                forget this computer's key
  annex whoami                which workspace this terminal uses

  annex ls [-a]               sandboxes (running; -a for all)
  annex start [flags]         start a sandbox   --route cheapest --vcpu 2 --memory 4 --region eu-central --template node-dev
                              (in a folder with an annex.yml, that template is saved and used)
  annex info <id>             one sandbox
  annex stop|pause|resume <id>

  annex run <id> -- <command> run a command, streaming its output; exits with its exit code
  annex shell <id>            a terminal in the sandbox, key by key (--lines: a line at a time, as before 0.9)
  annex files <id> [path]     list a folder
  annex cp <from> <to>        copy a file in or out; the sandbox side is <id>:/path
  annex logs <id>             what the sandbox has logged

  annex template [ls]         the workspace's templates: what new sandboxes start as
  annex template apply [file] save the template in annex.yml (or another file)
  annex template show <name>  print one as an annex.yml
  annex template from <id>    write an annex.yml from what was run in a sandbox:  annex template from > annex.yml
  annex template rm <name>

  annex providers             the accounts this workspace has connected, and whether a key was refused
  annex providers add [name]  connect a provider: opens the dashboard's page for it
  annex providers key <id>    replace an account's key: opens the dashboard's page for it
  annex providers check <id>  ask the provider whether the account's key still works

  annex connect claude        give Claude Code Annex's tools, with this computer's key
  annex connect cursor        the same for Cursor (--project: a file for this project that holds no key)
  annex update                replace this annex with the newest build
  annex doctor [id]           check this annex, its key, your accounts and a sandbox, and how long each takes
  annex version

<id> may be left out when exactly one sandbox is running. Add --json to print the API's own answer.
ANNEX_TIMING=1 before any command prints how long each request to Annex took, and where.
Docs: https://annex.eversonder.com/docs
`

// App is one run of the command: its streams, its environment and where its
// config lives, so tests can run it against a stand-in server.
type App struct {
	In       io.Reader
	Out, Err io.Writer
	Env      func(string) string
	Dir      string // the config folder; "" is ~/.config/annex
	HTTP     *http.Client
	Open     func(url string)                        // opens a browser; nil doesn't try
	NoFile   bool                                    // don't look for an annex.yml in the current folder (tests)
	Exec     func(name string, args ...string) error // runs another command (connect); nil is os/exec
	Home     string                                  // the home folder; "" is the user's (connect)
	Self     string                                  // this annex's own file; "" is os.Executable (update)

	cfg  config
	json bool
}

type config struct {
	URL           string `json:"url"`
	Key           string `json:"api_key"`
	Workspace     string `json:"workspace"`
	WorkspaceName string `json:"workspace_name,omitempty"`
}

// exit is a command that ends with a code and nothing more to say.
type exit int

func (e exit) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// Run is the whole command; the result is the process's exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if a.HTTP == nil {
		a.HTTP = &http.Client{}
	}
	if a.Env == nil {
		a.Env = os.Getenv
	}
	rest := args[:0:0]
	for i, s := range args { // --json anywhere before a "--"
		if s == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if s == "--json" {
			a.json = true
			continue
		}
		rest = append(rest, s)
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		fmt.Fprint(a.Out, usage)
		return 0
	}
	a.load()
	cmd, rest := rest[0], rest[1:]
	run := map[string]func(context.Context, []string) error{
		"login": a.login, "logout": a.logout, "whoami": a.whoami,
		"ls": a.ls, "start": a.start, "info": a.info, "stop": a.act("stop"), "pause": a.act("pause"), "resume": a.act("resume"),
		"run": a.run, "shell": a.shell, "files": a.files, "cp": a.cp, "logs": a.logs, "providers": a.providers, "connect": a.connect, "update": a.update, "doctor": a.doctor,
		"template": a.templates, "templates": a.templates,
		"version": func(context.Context, []string) error { fmt.Fprintln(a.Out, "annex", Version); return nil },
	}[cmd]
	if run == nil {
		fmt.Fprintf(a.Err, "annex: no command %q. Run annex help.\n", cmd)
		return 2
	}
	err := run(ctx, rest)
	var code exit
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.Is(err, flag.ErrHelp):
		return 2
	}
	fmt.Fprintln(a.Err, "annex:", err)
	return 1
}

// ---------- config ----------

func (a *App) file() string {
	dir := a.Dir
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "annex")
	}
	return filepath.Join(dir, "config.json")
}

// load reads the config; ANNEX_API_KEY, ANNEX_WORKSPACE and ANNEX_URL win over it (for scripts and CI).
func (a *App) load() {
	if b, err := os.ReadFile(a.file()); err == nil {
		json.Unmarshal(b, &a.cfg)
	}
	if v := a.Env("ANNEX_API_KEY"); v != "" {
		a.cfg.Key, a.cfg.Workspace, a.cfg.WorkspaceName = v, a.Env("ANNEX_WORKSPACE"), ""
	}
	if v := a.Env("ANNEX_URL"); v != "" {
		a.cfg.URL = v
	}
	if a.cfg.URL == "" {
		a.cfg.URL = DefaultURL
	}
	a.cfg.URL = strings.TrimRight(a.cfg.URL, "/")
}

// save keeps the key readable by this user only.
func (a *App) save() error {
	if err := os.MkdirAll(filepath.Dir(a.file()), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	return os.WriteFile(a.file(), append(b, '\n'), 0o600)
}

// ---------- the API ----------

// apiError is Annex's error envelope, in its own words.
type apiError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Annex answered %d", e.Status)
	}
	return e.Message
}

func (a *App) request(ctx context.Context, method, p string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.URL+p, body)
	if err != nil {
		return nil, err
	}
	if a.cfg.Key != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.Key)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("User-Agent", "annex-cli/"+Version)
	res, err := a.do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s: %w", a.cfg.URL, err)
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		e := &apiError{Status: res.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		json.Unmarshal(b, e)
		if res.StatusCode == 401 {
			e.Message = "this terminal isn't signed in, or its key was revoked. Run annex login."
		}
		return nil, e
	}
	return res, nil
}

// do sends req. With ANNEX_TIMING set it asks Annex where the time went
// (Server-Timing: each step Annex times, "provider" among them) and prints,
// for each request, how long the whole round trip took and Annex's own part.
// A stream's time is until its first byte, not its end.
func (a *App) do(req *http.Request) (*http.Response, error) {
	if a.Env("ANNEX_TIMING") == "" {
		return a.HTTP.Do(req)
	}
	req.Header.Set("Annex-Timing", "1")
	start := time.Now()
	res, err := a.HTTP.Do(req)
	took := time.Since(start).Round(time.Millisecond)
	p := req.URL.Path
	if req.URL.RawQuery != "" {
		p += "?" + req.URL.RawQuery
	}
	if err != nil {
		fmt.Fprintf(a.Err, "timing: %s %s failed after %s\n", req.Method, p, took)
		return res, err
	}
	fmt.Fprintf(a.Err, "timing: %s %s %d in %s", req.Method, p, res.StatusCode, took)
	if st := res.Header.Get("Server-Timing"); st != "" {
		fmt.Fprintf(a.Err, " (Annex: %s)", serverTiming(st))
	}
	fmt.Fprintln(a.Err)
	return res, nil
}

// serverTiming reads "app;dur=812.3, provider;dur=790.1" as "app 812ms, provider 790ms".
func serverTiming(h string) string {
	var parts []string
	for _, m := range strings.Split(h, ",") {
		name, rest, _ := strings.Cut(strings.TrimSpace(m), ";")
		_, dur, ok := strings.Cut(rest, "dur=")
		if ms, err := strconv.ParseFloat(strings.TrimSpace(dur), 64); ok && err == nil {
			parts = append(parts, fmt.Sprintf("%s %.0fms", name, ms))
		} else if name != "" {
			parts = append(parts, name)
		}
	}
	return strings.Join(parts, ", ")
}

// call sends in as JSON (nil: no body) and decodes the answer into out (nil:
// ignored). With --json the answer is printed as it came and out is left alone.
func (a *App) call(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, _ := json.Marshal(in)
		body, ctype = bytes.NewReader(b), "application/json"
	}
	res, err := a.request(ctx, method, p, body, ctype)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return err
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// show prints v as the API sent it when --json is on, and says whether it did.
func (a *App) show(v any) bool {
	if !a.json {
		return false
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(a.Out, string(b))
	return true
}

// ws is the workspace's part of every path; an error when not signed in.
func (a *App) ws() (string, error) {
	if a.cfg.Key == "" {
		return "", errors.New("not signed in. Run annex login.")
	}
	if a.cfg.Workspace == "" { // a key from ANNEX_API_KEY alone: ask which workspace it is for
		var k struct {
			Key struct {
				Workspace struct{ ID, Name string }
			}
		}
		if err := a.call(context.Background(), "GET", "/v1/key", nil, &k); err != nil {
			return "", err
		}
		a.cfg.Workspace, a.cfg.WorkspaceName = k.Key.Workspace.ID, k.Key.Workspace.Name
	}
	return "/v1/workspaces/" + url.PathEscape(a.cfg.Workspace), nil
}

type sandbox struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	Region     string `json:"region"`
	Status     string `json:"status"`
	VCPU       int    `json:"vcpu"`
	MemoryGiB  int    `json:"memory_gib"`
	RateMicros int64  `json:"rate_per_hour_micros"`
	CostMicros int64  `json:"cost_micros"`
	CreatedAt  string `json:"created_at"`
	Machine    *struct {
		Name      string  `json:"name"`
		VCPU      int     `json:"vcpu"`
		MemoryGiB float64 `json:"memory_gib"`
		Note      string  `json:"note"`
	} `json:"machine"`
	Template string `json:"template"`
	Setup    *struct {
		State string `json:"state"`
		Note  string `json:"note"`
		Steps int    `json:"steps"`
	} `json:"setup"`
}

func usd(micros int64) string { return fmt.Sprintf("$%.4f", float64(micros)/1e6) }

func age(at string) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// pick is the sandbox a command means: the id given, or the only one running.
func (a *App) pick(ctx context.Context, args []string) (id string, rest []string, err error) {
	if len(args) > 0 && strings.HasPrefix(args[0], "sbx_") {
		return args[0], args[1:], nil
	}
	base, err := a.ws()
	if err != nil {
		return "", nil, err
	}
	var out struct{ Sandboxes []sandbox }
	if err := a.call(ctx, "GET", base+"/sandboxes?status=running&perPage=5", nil, &out); err != nil {
		return "", nil, err
	}
	switch len(out.Sandboxes) {
	case 1:
		return out.Sandboxes[0].ID, args, nil
	case 0:
		return "", nil, errors.New("no sandbox is running. Start one with annex start.")
	}
	return "", nil, errors.New("more than one sandbox is running: say which (annex ls shows their ids).")
}

// ---------- signing in ----------

func (a *App) login(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	key := fs.String("key", "", "an API key you already have, in place of approving in the browser")
	at := fs.String("url", "", "another Annex than "+DefaultURL)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *at != "" {
		a.cfg.URL = strings.TrimRight(*at, "/")
	}
	a.cfg.Key, a.cfg.Workspace, a.cfg.WorkspaceName = "", "", ""
	if *key != "" {
		a.cfg.Key = *key
		if _, err := a.ws(); err != nil {
			return err
		}
	} else {
		host, _ := os.Hostname()
		var started struct {
			Login struct {
				DeviceCode string `json:"device_code"`
				UserCode   string `json:"user_code"`
				VerifyURL  string `json:"verify_url"`
				ExpiresAt  string `json:"expires_at"`
				Interval   int    `json:"interval_seconds"`
			} `json:"cli_login"`
		}
		if err := a.call(ctx, "POST", "/v1/auth/cli/start", map[string]string{"name": strings.TrimSuffix(host, ".local")}, &started); err != nil {
			return err
		}
		l := started.Login
		fmt.Fprintf(a.Out, "To sign in, open this page and approve the code %s:\n\n  %s\n\nWaiting for you to approve…\n", l.UserCode, l.VerifyURL)
		if a.Open != nil {
			a.Open(l.VerifyURL)
		}
		every := time.Duration(max(l.Interval, 1)) * time.Second
		for {
			select {
			case <-ctx.Done():
				return errors.New("stopped before the sign-in was approved")
			case <-time.After(every):
			}
			var got struct {
				Status string `json:"status"`
				Login  struct {
					APIKey    struct{ Secret string } `json:"api_key"`
					Workspace struct{ ID, Name string }
				} `json:"cli_login"`
			}
			if err := a.call(ctx, "POST", "/v1/auth/cli/token", map[string]string{"device_code": l.DeviceCode}, &got); err != nil {
				return err
			}
			if got.Login.APIKey.Secret != "" {
				a.cfg.Key, a.cfg.Workspace, a.cfg.WorkspaceName = got.Login.APIKey.Secret, got.Login.Workspace.ID, got.Login.Workspace.Name
				break
			}
		}
	}
	if err := a.save(); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Signed in to %s. Try annex ls, or annex start.\n", cmpOr(a.cfg.WorkspaceName, a.cfg.Workspace))
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (a *App) logout(context.Context, []string) error {
	if err := os.Remove(a.file()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintln(a.Out, "Signed out here. The key itself still works until you revoke it under API keys in the dashboard.")
	return nil
}

func (a *App) whoami(ctx context.Context, _ []string) error {
	if _, err := a.ws(); err != nil {
		return err
	}
	var k any
	if err := a.call(ctx, "GET", "/v1/key", nil, &k); err != nil { // also proves the key still works
		return err
	}
	if a.show(k) {
		return nil
	}
	fmt.Fprintf(a.Out, "%s (%s) at %s\n", cmpOr(a.cfg.WorkspaceName, "workspace"), a.cfg.Workspace, a.cfg.URL)
	return nil
}

// ---------- sandboxes ----------

func (a *App) ls(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	all := fs.Bool("a", false, "every sandbox, not only the running ones")
	if err := fs.Parse(args); err != nil {
		return err
	}
	base, err := a.ws()
	if err != nil {
		return err
	}
	q := "?perPage=50"
	if !*all {
		q += "&status=running"
	}
	var raw json.RawMessage
	if err := a.call(ctx, "GET", base+"/sandboxes"+q, nil, &raw); err != nil {
		return err
	}
	if a.show(raw) {
		return nil
	}
	var out struct{ Sandboxes []sandbox }
	json.Unmarshal(raw, &out)
	if len(out.Sandboxes) == 0 {
		fmt.Fprintln(a.Out, "No sandboxes running. Start one with annex start.")
		return nil
	}
	w := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tPROVIDER\tREGION\tSIZE\tAGE\tRATE/H\tCOST")
	for _, s := range out.Sandboxes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d vCPU · %d GiB\t%s\t%s\t%s\n", s.ID, s.Status, s.Provider, cmpOr(s.Region, "-"), s.VCPU, s.MemoryGiB, age(s.CreatedAt), usd(s.RateMicros), usd(s.CostMicros))
	}
	return w.Flush()
}

func (a *App) describe(s sandbox) {
	fmt.Fprintf(a.Out, "%s  %s on %s", s.ID, s.Status, s.Provider)
	if s.Region != "" {
		fmt.Fprintf(a.Out, " (%s)", s.Region)
	}
	fmt.Fprintf(a.Out, "\n  asked %d vCPU · %d GiB, %s an hour, %s so far\n", s.VCPU, s.MemoryGiB, usd(s.RateMicros), usd(s.CostMicros))
	if s.Machine != nil && s.Machine.Note != "" {
		fmt.Fprintln(a.Out, "  note:", s.Machine.Note)
	}
	if s.Template != "" {
		state := ""
		if s.Setup != nil {
			state = map[string]string{"running": ": its set-up commands are still running (annex logs shows them)", "done": ": set up",
				"failed": ": set-up stopped. " + s.Setup.Note, "interrupted": ": " + s.Setup.Note}[s.Setup.State]
		}
		fmt.Fprintf(a.Out, "  template %s%s\n", s.Template, state)
	}
}

func (a *App) start(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	route := fs.String("route", "cheapest", "cheapest, fastest, or a provider's name")
	vcpu := fs.Int("vcpu", 0, "vCPUs (the workspace's default when left out)")
	mem := fs.Int("memory", 0, "memory in GiB (the workspace's default when left out)")
	region := fs.String("region", "", "a region code such as eu-central; anywhere when left out")
	tpl := fs.String("template", "", "a template to start from; none for no template; left out, the annex.yml here or the workspace's default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	base, err := a.ws()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(templateFile); *tpl == "" && statErr == nil && !a.NoFile {
		// A folder's annex.yml is its template: saved under its name, then used.
		t, err := a.applyFile(ctx, templateFile)
		if err != nil {
			return err
		}
		*tpl = t.Name
		if !a.json {
			fmt.Fprintf(a.Out, "Using %s here (template %s).\n", templateFile, t.Name)
		}
	}
	in := map[string]any{}
	if *tpl != "" {
		in["template"] = *tpl
	}
	routeSet := false
	fs.Visit(func(f *flag.Flag) { routeSet = routeSet || f.Name == "route" })
	if routeSet || *tpl == "none" { // otherwise the route is the template's, or the workspace's default
		in["route"] = *route
	}
	if *vcpu > 0 {
		in["vcpu"] = *vcpu
	}
	if *mem > 0 {
		in["memory_gib"] = *mem
	}
	if *region != "" {
		in["region"] = *region
	}
	var raw json.RawMessage
	err = a.call(ctx, "POST", base+"/sandboxes", in, &raw)
	var refused *apiError
	if _, said := in["route"]; err != nil && !said && errors.As(err, &refused) && refused.Status == 422 {
		in["route"] = *route // no template or workspace default named a route: the cheapest, as annex start always meant
		err = a.call(ctx, "POST", base+"/sandboxes", in, &raw)
	}
	if err != nil {
		return err
	}
	if a.show(raw) {
		return nil
	}
	var out struct{ Sandbox sandbox }
	json.Unmarshal(raw, &out)
	a.describe(out.Sandbox)
	fmt.Fprintf(a.Out, "\nannex shell %s   opens a terminal in it\nannex stop %s    stops it (it costs money while it runs)\n", out.Sandbox.ID, out.Sandbox.ID)
	return nil
}

func (a *App) info(ctx context.Context, args []string) error {
	id, _, err := a.pick(ctx, args)
	if err != nil {
		return err
	}
	base, _ := a.ws()
	var raw json.RawMessage
	if err := a.call(ctx, "GET", base+"/sandboxes/"+id, nil, &raw); err != nil {
		return err
	}
	if a.show(raw) {
		return nil
	}
	var out struct{ Sandbox sandbox }
	json.Unmarshal(raw, &out)
	a.describe(out.Sandbox)
	return nil
}

// act is stop, pause or resume: one POST, then the sandbox as it is now.
func (a *App) act(verb string) func(context.Context, []string) error {
	return func(ctx context.Context, args []string) error {
		id, _, err := a.pick(ctx, args)
		if err != nil {
			return err
		}
		base, _ := a.ws()
		var raw json.RawMessage
		if err := a.call(ctx, "POST", base+"/sandboxes/"+id+"/"+verb, nil, &raw); err != nil {
			return err
		}
		if a.show(raw) {
			return nil
		}
		var out struct{ Sandbox sandbox }
		json.Unmarshal(raw, &out)
		fmt.Fprintf(a.Out, "%s is %s\n", id, cmpOr(out.Sandbox.Status, map[string]string{"stop": "stopped", "pause": "paused", "resume": "running"}[verb]))
		return nil
	}
}

// providers lists the connected accounts. Adding one and replacing a key
// open the dashboard: the API takes those from a signed-in person only, never
// an API key, so an agent holding this computer's key can't widen what it
// reaches (Lanre asked for both from the terminal, 2026-11-24).
func (a *App) providers(ctx context.Context, args []string) error {
	base, err := a.ws()
	if err != nil {
		return err
	}
	page := strings.TrimRight(a.cfg.URL, "/") + "/dashboard/w/" + url.PathEscape(a.cfg.Workspace) + "/connections"
	if len(args) > 0 {
		switch args[0] {
		case "add", "key":
			if args[0] == "key" && len(args) < 2 {
				return errors.New("which account? annex providers key <id> (annex providers lists them)")
			}
			q := ""
			if len(args) > 1 {
				q = map[string]string{"add": "?add=", "key": "?key="}[args[0]] + url.QueryEscape(args[1])
			}
			fmt.Fprintf(a.Out, "Annex takes keys from a signed-in person, not from this computer's key. Opening:\n  %s%s\n", page, q)
			if a.Open != nil {
				a.Open(page + q)
			}
			return nil
		case "check":
			if len(args) < 2 {
				return errors.New("which account? annex providers check <id> (annex providers lists them)")
			}
			var raw json.RawMessage
			if err := a.call(ctx, "POST", base+"/connections/"+url.PathEscape(args[1])+"/check", struct{}{}, &raw); err != nil {
				return err
			}
			if a.show(raw) {
				return nil
			}
			var out struct {
				Connection struct {
					Provider, Label string
					KeyProblem      string `json:"key_problem"`
				}
			}
			json.Unmarshal(raw, &out)
			c := out.Connection
			if c.KeyProblem != "" {
				fmt.Fprintf(a.Out, "%s (%s): the key was refused. %s\nReplace it: annex providers key %s\n", c.Label, c.Provider, c.KeyProblem, args[1])
				return exit(1)
			}
			fmt.Fprintf(a.Out, "%s (%s): the key works.\n", c.Label, c.Provider)
			return nil
		default:
			return fmt.Errorf("no providers command %q: add, key or check", args[0])
		}
	}
	var raw json.RawMessage
	if err := a.call(ctx, "GET", base+"/connections?perPage=100", nil, &raw); err != nil {
		return err
	}
	if a.show(raw) {
		return nil
	}
	var out struct {
		Connections []struct {
			ID, Provider, Label string
			KeyProblem          string `json:"key_problem"`
		}
	}
	json.Unmarshal(raw, &out)
	if len(out.Connections) == 0 {
		fmt.Fprintf(a.Out, "No provider accounts connected yet. Connect one with annex providers add, or at %s.\n", page)
		return nil
	}
	w := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tNAME\tID\tKEY")
	refused := false
	for _, c := range out.Connections {
		key := "" // not checked lately, or it worked: nothing to say
		if c.KeyProblem != "" {
			key, refused = "refused", true
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Provider, c.Label, c.ID, key)
	}
	if refused {
		defer fmt.Fprintln(a.Out, "A refused key needs replacing: annex providers key <id>")
	}
	return w.Flush()
}

// ---------- commands ----------

type command struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	ExitCode  *int    `json:"exit_code"`
	Output    *string `json:"output"` // with ?wait=, when it ended in time: stdout and stderr together
	Truncated bool    `json:"truncated"`
}

// quickWait is how long the shell asks Annex to wait for a line before
// streaming it: a command that ends by then comes back in the one answer,
// which on providers that run commands in the background (Namespace) is one
// call to them instead of three (bug.mov, 2026-11-24: each line took about 2 s).
// Ctrl+C can't stop a line until Annex has answered, so it stays short.
const quickWait = 5

// exec runs one command in the sandbox and streams its output here until it
// ends, answering its exit code. Ctrl+C stops the command, as it would locally.
// exec runs line and passes on its output. wait > 0 asks Annex to hold the
// answer that many seconds: a command that ends by then is printed from the
// answer, stdout and stderr together; any other is streamed as usual.
func (a *App) exec(ctx context.Context, id, line, cwd string, timeout, wait int) (int, error) {
	base, err := a.ws()
	if err != nil {
		return 1, err
	}
	in := map[string]any{"command": line}
	if cwd != "" {
		in["cwd"] = cwd
	}
	if timeout > 0 {
		in["timeout_seconds"] = timeout
	}
	q := ""
	if wait > 0 {
		q = "?wait=" + strconv.Itoa(wait)
	}
	var made struct{ Command command }
	if err := a.call(ctx, "POST", base+"/sandboxes/"+id+"/commands"+q, in, &made); err != nil {
		return 1, err
	}
	if c := made.Command; wait > 0 && c.Status != "running" && c.Output != nil && !c.Truncated {
		io.WriteString(a.Out, *c.Output)
		if c.ExitCode != nil {
			return *c.ExitCode, nil
		}
		return 1, fmt.Errorf("the command ended %s", cmpOr(c.Status, "without an exit code"))
	}
	at := base + "/sandboxes/" + id + "/commands/" + made.Command.ID

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	defer signal.Stop(stop)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			a.call(context.Background(), "POST", at+"/cancel", nil, nil) // the stream then ends with its exit code
		case <-done:
		}
	}()

	last := ""
	for try := 0; ; try++ {
		code, ended, cursor, err := a.follow(ctx, at+"/stream", last)
		if ended {
			return code, err
		}
		if cursor != "" {
			last, try = cursor, 0
		}
		if try >= 5 {
			return 1, cmpErr(err, errors.New("lost the command's output; it may still be running"))
		}
		time.Sleep(time.Second) // the stream broke: pick it up after the last chunk
	}
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// follow reads one output stream to its end. ended is false when it broke off
// early; cursor is the last chunk seen, to carry on from.
func (a *App) follow(ctx context.Context, p, last string) (code int, ended bool, cursor string, err error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", a.cfg.URL+p, nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.Key)
	req.Header.Set("Accept", "text/event-stream")
	if last != "" {
		req.Header.Set("Last-Event-ID", last)
	}
	res, err := a.do(req)
	if err != nil {
		return 1, false, "", err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		e := &apiError{Status: res.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		json.Unmarshal(b, e)
		return 1, res.StatusCode != 429 && res.StatusCode != 503, "", e // too many streams or a busy server: try again
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	event, data := "", ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "id:"):
			cursor = strings.TrimSpace(line[3:])
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimPrefix(line[5:], " ")
		case line == "":
			switch event {
			case "output":
				var c struct{ Stream, Text string }
				json.Unmarshal([]byte(data), &c)
				if c.Stream == "stderr" {
					io.WriteString(a.Err, c.Text)
				} else {
					io.WriteString(a.Out, c.Text)
				}
			case "exit":
				var c command
				json.Unmarshal([]byte(data), &c)
				if c.ExitCode != nil {
					return *c.ExitCode, true, cursor, nil
				}
				return 1, true, cursor, fmt.Errorf("the command ended %s", cmpOr(c.Status, "without an exit code"))
			case "gone":
				e := &apiError{}
				json.Unmarshal([]byte(data), e)
				return 1, true, cursor, e
			}
			event, data = "", ""
		}
	}
	return 1, false, cursor, sc.Err()
}

func (a *App) run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	cwd := fs.String("cwd", "", "the folder to run it in")
	timeout := fs.Int("timeout", 0, "stop it after this many seconds")
	var before, cmd []string
	for i, s := range args {
		if s == "--" {
			before, cmd = args[:i], args[i+1:]
			break
		}
	}
	if cmd == nil { // no "--": the sandbox id (if any), then the command
		before, cmd = nil, args
		if len(args) > 0 && strings.HasPrefix(args[0], "sbx_") {
			before, cmd = args[:1], args[1:]
		}
	}
	id, rest, err := a.pick(ctx, before)
	if err != nil {
		return err
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if len(cmd) == 0 {
		return errors.New("say what to run: annex run <id> -- npm test")
	}
	code, err := a.exec(ctx, id, strings.Join(cmd, " "), *cwd, *timeout, 0) // run keeps stdout and stderr apart: always streamed
	if err != nil {
		return err
	}
	if code != 0 {
		return exit(code)
	}
	return nil
}

// cwdTap passes a command's output through, holding back the last line in
// case it is the folder marker the shell adds, which it keeps for itself.
type cwdTap struct {
	w    io.Writer
	mark string
	held []byte
	cwd  string
	last byte // the last byte passed on
}

func (t *cwdTap) Write(p []byte) (int, error) {
	t.held = append(t.held, p...)
	if i := bytes.LastIndexByte(t.held, '\n'); i >= 0 { // whole lines but the last may go
		head := t.held[:i+1]
		cut := bytes.LastIndex(head, []byte("\n"+t.mark)) // where the output before the marker ends
		at := cut + 1
		if cut < 0 && bytes.HasPrefix(head, []byte(t.mark)) {
			cut, at = 0, 0
		}
		var after []byte
		if cut >= 0 {
			// The folder is the marker's own line and no more: when stdout and
			// stderr come back in one answer, stderr follows the marker.
			line := head[at+len(t.mark):]
			end := bytes.IndexByte(line, '\n') // there is one: head ends with it
			t.cwd, after, head = strings.TrimSpace(string(line[:end])), line[end+1:], head[:cut]
		}
		t.put(head)
		if len(after) > 0 && t.last != 0 && t.last != '\n' {
			t.put([]byte("\n"))
		}
		t.put(after)
		t.held = append(t.held[:0], t.held[i+1:]...)
	}
	return len(p), nil
}

func (t *cwdTap) put(b []byte) {
	if len(b) > 0 {
		t.w.Write(b)
		t.last = b[len(b)-1]
	}
}

// flush ends the command's output on a line of its own, for the next prompt.
func (t *cwdTap) flush() {
	if len(t.held) > 0 && !bytes.HasPrefix(t.held, []byte(t.mark)) {
		t.put(t.held)
	}
	if t.last != 0 && t.last != '\n' {
		fmt.Fprintln(t.w)
	}
	t.held = nil
}

// ---------- files and logs ----------

func (a *App) files(ctx context.Context, args []string) error {
	id, rest, err := a.pick(ctx, args)
	if err != nil {
		return err
	}
	base, _ := a.ws()
	q := url.Values{"perPage": {"100"}}
	if len(rest) > 0 {
		q.Set("path", rest[0])
	}
	var raw json.RawMessage
	if err := a.call(ctx, "GET", base+"/sandboxes/"+id+"/files?"+q.Encode(), nil, &raw); err != nil {
		return err
	}
	if a.show(raw) {
		return nil
	}
	var out struct {
		Files []struct {
			Name, Type string
			Size       int64
		}
	}
	json.Unmarshal(raw, &out)
	w := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
	for _, f := range out.Files {
		name, size := f.Name, fmt.Sprint(f.Size)
		if f.Type == "dir" {
			name, size = name+"/", ""
		}
		fmt.Fprintf(w, "%s\t%s\n", name, size)
	}
	return w.Flush()
}

// cp copies one file in or out. The sandbox side is "sbx_…:/absolute/path".
func (a *App) cp(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("annex cp <from> <to>, where the sandbox side is <id>:/path, e.g. annex cp report.csv sbx_…:/home/user/report.csv")
	}
	remote := func(s string) (id, p string, ok bool) {
		id, p, ok = strings.Cut(s, ":")
		return id, p, ok && strings.HasPrefix(id, "sbx_")
	}
	base, err := a.ws()
	if err != nil {
		return err
	}
	fromID, fromPath, fromRemote := remote(args[0])
	toID, toPath, toRemote := remote(args[1])
	switch {
	case fromRemote == toRemote:
		return errors.New("one side must be a file here and the other <id>:/path in a sandbox")
	case toRemote: // up
		f, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		if toPath == "" || strings.HasSuffix(toPath, "/") {
			toPath = path.Join(cmpOr(toPath, "/tmp"), filepath.Base(args[0]))
		}
		res, err := a.request(ctx, "PUT", base+"/sandboxes/"+toID+"/files/content?"+url.Values{"path": {toPath}}.Encode(), f, "application/octet-stream")
		if err != nil {
			return err
		}
		res.Body.Close()
		fmt.Fprintf(a.Out, "%s → %s:%s\n", args[0], toID, toPath)
	default: // down
		res, err := a.request(ctx, "GET", base+"/sandboxes/"+fromID+"/files/content?"+url.Values{"path": {fromPath}}.Encode(), nil, "")
		if err != nil {
			return err
		}
		defer res.Body.Close()
		to := args[1]
		if st, err := os.Stat(to); err == nil && st.IsDir() {
			to = filepath.Join(to, path.Base(fromPath))
		}
		// Written beside the target and renamed, so a broken download never leaves half a file under its name.
		tmp, err := os.CreateTemp(filepath.Dir(to), ".annex-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := io.Copy(tmp, res.Body); err != nil {
			tmp.Close()
			return fmt.Errorf("the download broke off: %w", err)
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), to); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "%s:%s → %s\n", fromID, fromPath, to)
	}
	return nil
}

func (a *App) logs(ctx context.Context, args []string) error {
	id, _, err := a.pick(ctx, args)
	if err != nil {
		return err
	}
	base, _ := a.ws()
	var raw json.RawMessage
	if err := a.call(ctx, "GET", base+"/sandboxes/"+id+"/logs?limit=200", nil, &raw); err != nil {
		return err
	}
	if a.show(raw) {
		return nil
	}
	var out struct {
		Logs struct {
			Lines []struct {
				At, Source, Text string
			}
		}
	}
	json.Unmarshal(raw, &out)
	if len(out.Logs.Lines) == 0 {
		fmt.Fprintln(a.Out, "Nothing logged yet.")
	}
	for _, l := range out.Logs.Lines {
		at := l.At
		if t, err := time.Parse(time.RFC3339Nano, l.At); err == nil {
			at = t.Local().Format("15:04:05")
		}
		fmt.Fprintf(a.Out, "%s  %-8s %s\n", at, l.Source, strings.TrimRight(l.Text, "\n"))
	}
	return nil
}

// OpenBrowser opens url in the person's browser, quietly doing nothing where it can't.
func OpenBrowser(url string) {
	name := map[string]string{"darwin": "open", "linux": "xdg-open"}[runtime.GOOS]
	if name == "" {
		return
	}
	startProcess(name, url)
}
