package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// connect is "annex connect claude|cursor": it gives a coding agent Annex's
// tools (start a sandbox, run commands in it, read and write its files) with
// the key this computer is signed in with, so nobody pastes a key anywhere.
//
// Claude Code is told by its own command, "claude mcp add", for every project
// of this user. Cursor reads a file: ~/.cursor/mcp.json gets the key itself
// (it is this user's own file, kept readable by them only, like annex's own),
// while --project writes ./.cursor/mcp.json with a reference to ANNEX_API_KEY
// instead, so the file can be committed.
func (a *App) connect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	project := fs.Bool("project", false, "cursor: write ./.cursor/mcp.json for this project, naming ANNEX_API_KEY instead of holding the key")
	how := errors.New("say which agent: annex connect claude, or annex connect cursor [--project]")
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' { // the agent may come before the flags
		args = append(args[1:], args[0])
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return how
	}
	if _, err := a.ws(); err != nil { // signed in?
		return err
	}
	address, bearer := a.cfg.URL+"/mcp", "Bearer "+a.cfg.Key
	switch fs.Arg(0) {
	case "claude":
		run := a.Exec
		if run == nil {
			if _, err := exec.LookPath("claude"); err != nil {
				return errors.New("Claude Code isn't installed here (no claude command). Install it, then run this again")
			}
			run = func(name string, args ...string) error {
				c := exec.CommandContext(ctx, name, args...)
				c.Stdout, c.Stderr = a.Err, a.Err
				return c.Run()
			}
			exec.CommandContext(ctx, "claude", "mcp", "remove", "--scope", "user", "annex").Run() // an earlier connection, with an older key; quiet, as there is usually none
		} else {
			run("claude", "mcp", "remove", "--scope", "user", "annex")
		}
		if err := run("claude", "mcp", "add", "--transport", "http", "--scope", "user", "annex", address, "--header", "Authorization: "+bearer); err != nil {
			return fmt.Errorf("Claude Code didn't take it: %w", err)
		}
		fmt.Fprintln(a.Out, "Claude Code now has Annex's tools, in every project. Try: \"Start a sandbox, clone this repo there and run the tests.\"")
	case "cursor":
		home := a.Home
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		file := filepath.Join(home, ".cursor", "mcp.json")
		if *project {
			file, bearer = filepath.Join(".cursor", "mcp.json"), "Bearer ${env:ANNEX_API_KEY}"
		}
		if err := mergeServer(file, map[string]any{"url": address, "headers": map[string]string{"Authorization": bearer}}); err != nil {
			return err
		}
		if *project {
			fmt.Fprintf(a.Out, "Wrote %s. It names ANNEX_API_KEY and holds no key, so it can be committed; set that variable where Cursor runs.\n", file)
		} else {
			fmt.Fprintf(a.Out, "Cursor now has Annex's tools (%s). Restart Cursor, then try: \"Start a sandbox and run the tests there.\"\n", file)
		}
	default:
		return how
	}
	return nil
}

// mergeServer sets mcpServers.annex in a Cursor mcp.json, keeping everything
// else in it. A file that isn't JSON is left alone and said so.
func mergeServer(file string, server map[string]any) error {
	doc := map[string]any{}
	if b, err := os.ReadFile(file); err == nil {
		if json.Unmarshal(b, &doc) != nil {
			return fmt.Errorf("%s isn't valid JSON, so annex left it alone; fix it or move it away, then run this again", file)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["annex"] = server
	doc["mcpServers"] = servers
	b, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".annex-tmp" // written beside it and renamed, so a cut-off write never leaves half a file
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
