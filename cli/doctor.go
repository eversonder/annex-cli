package cli

import (
	"context"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// doctor checks, in order, what a command needs, and says how long each
// step took: this annex against the build Annex serves, the key, each
// connected account's key, and one command in a running sandbox (or the
// one named). It reports every problem it finds rather than stopping at the
// first, so one paste says everything (asked for after bug.mov, 2026-11-24).
func (a *App) doctor(ctx context.Context, args []string) error {
	bad := 0
	say := func(ok bool, what, detail string, took time.Duration) {
		mark := "ok     "
		if !ok {
			mark, bad = "problem", bad+1
		}
		if took > 0 {
			detail += fmt.Sprintf(" (%s)", took.Round(time.Millisecond))
		}
		fmt.Fprintf(a.Out, "%s  %-10s %s\n", mark, what, detail)
	}
	timed := func(f func() error) (time.Duration, error) {
		start := time.Now()
		err := f()
		return time.Since(start), err
	}
	fmt.Fprintf(a.Out, "annex %s on %s-%s, talking to %s\n\n", Version, runtime.GOOS, runtime.GOARCH, a.cfg.URL)

	// 1. Is this annex the newest? The version Annex's builds are.
	var v []byte
	took, err := timed(func() (err error) {
		v, err = a.fetch(ctx, strings.TrimRight(a.cfg.URL, "/")+"/cli/version", 64)
		return err
	})
	switch newest := strings.TrimSpace(string(v)); {
	case err != nil:
		say(false, "annex", "couldn't reach "+a.cfg.URL+": "+err.Error(), took)
	case newest == Version:
		say(true, "annex", "the newest, "+Version, took)
	default:
		say(false, "annex", "Annex has "+newest+": run annex update", took)
	}

	// 2. The key, and which workspace it is for.
	if a.cfg.Key == "" {
		say(false, "key", "not signed in: run annex login", 0)
		return exit(1)
	}
	var k struct {
		Key struct {
			Name      string
			Workspace struct{ ID, Name string }
		}
	}
	took, err = timed(func() error { return a.call(ctx, "GET", "/v1/key", nil, &k) })
	if err != nil {
		say(false, "key", err.Error(), took)
		return exit(1)
	}
	if a.cfg.Workspace == "" {
		a.cfg.Workspace, a.cfg.WorkspaceName = k.Key.Workspace.ID, k.Key.Workspace.Name
	}
	say(true, "key", fmt.Sprintf("%s, for %s", cmpOr(k.Key.Name, "works"), cmpOr(k.Key.Workspace.Name, k.Key.Workspace.ID)), took)
	base := "/v1/workspaces/" + url.PathEscape(a.cfg.Workspace)

	// 3. Each connected account's key, as the provider sees it now.
	var cs struct {
		Connections []struct {
			ID, Provider, Label, Method string
		}
	}
	took, err = timed(func() error { return a.call(ctx, "GET", base+"/connections?perPage=100", nil, &cs) })
	if err != nil {
		say(false, "accounts", err.Error(), took)
	} else if len(cs.Connections) == 0 {
		say(false, "accounts", "none connected: annex providers add", took)
	}
	for _, c := range cs.Connections {
		name := c.Label + " (" + c.Provider + ")"
		if c.Method != "api_key" || c.Provider == "cloudflare" {
			say(true, "account", name+": connected without a key Annex can ask about", 0)
			continue
		}
		var out struct {
			Connection struct {
				KeyProblem string `json:"key_problem"`
			}
		}
		took, err := timed(func() error {
			return a.call(ctx, "POST", base+"/connections/"+url.PathEscape(c.ID)+"/check", struct{}{}, &out)
		})
		switch e, _ := err.(*apiError); {
		case e != nil && e.Status == 429:
			say(true, "account", name+": checked in the last minute; try again shortly", took)
		case err != nil:
			say(false, "account", name+": "+err.Error(), took)
		case out.Connection.KeyProblem != "":
			say(false, "account", name+": "+out.Connection.KeyProblem+" Replace it: annex providers key "+c.ID, took)
		default:
			say(true, "account", name+": the key works", took)
		}
	}

	// 4. One command, timed, in the sandbox named or the one running.
	id := ""
	if len(args) > 0 {
		id = args[0]
	} else if picked, _, err := a.pick(ctx, nil); err == nil {
		id = picked
	} else {
		say(true, "sandbox", "skipped: "+err.Error(), 0)
	}
	if id != "" {
		out, errs := a.Out, a.Err
		var got strings.Builder
		a.Out = &got
		var code int
		took, err := timed(func() (err error) { code, err = a.exec(ctx, id, "echo annex-ok", "", 30, quickWait); return err })
		a.Out, a.Err = out, errs
		switch {
		case err != nil:
			say(false, "sandbox", id+": "+err.Error(), took)
		case code != 0 || !strings.Contains(got.String(), "annex-ok"):
			say(false, "sandbox", fmt.Sprintf("%s: a test command exited %d and printed %q", id, code, strings.TrimSpace(got.String())), took)
		default:
			say(true, "sandbox", id+": ran a command", took)
		}
	}

	fmt.Fprintln(a.Out)
	if bad > 0 {
		fmt.Fprintf(a.Out, "%d problem(s). Add ANNEX_TIMING=1 to see each request to Annex.\n", bad)
		return exit(1)
	}
	fmt.Fprintln(a.Out, "All good.")
	return nil
}
