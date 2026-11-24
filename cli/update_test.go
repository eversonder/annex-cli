package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// update swaps this annex for Annex's build when the hash matches, says so
// when it is already the newest, and changes nothing when the hash is wrong.
func TestUpdate(t *testing.T) {
	newer := []byte("#!/bin/sh\necho annex 9.9.9\n") // stands in for a build: it answers "version"
	sum := sha256.Sum256(newer)
	published := hex.EncodeToString(sum[:])
	name := "/cli/bin/annex-" + runtime.GOOS + "-" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case name + ".sha256":
			w.Write([]byte(published + "\n"))
		case name:
			w.Write(newer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	self := filepath.Join(t.TempDir(), "annex")
	os.WriteFile(self, []byte("the old build"), 0o755)
	run := func() (int, string, string) {
		var o, e bytes.Buffer
		app := &App{Out: &o, Err: &e, Dir: t.TempDir(), HTTP: srv.Client(), Self: self, NoFile: true,
			Env: func(k string) string { return map[string]string{"ANNEX_URL": srv.URL}[k] }}
		return app.Run(context.Background(), []string{"update"}), o.String(), e.String()
	}

	published = strings.Repeat("0", 64) // a download that doesn't match
	if code, _, errs := run(); code == 0 || !strings.Contains(errs, "doesn't match") {
		t.Fatalf("a wrong hash: %d %q", code, errs)
	}
	if b, _ := os.ReadFile(self); string(b) != "the old build" {
		t.Fatal("a wrong hash changed this annex")
	}
	published = hex.EncodeToString(sum[:])
	if code, out, errs := run(); code != 0 || !strings.Contains(out, "to 9.9.9") {
		t.Fatalf("update: %d %q %q", code, out, errs)
	}
	if b, _ := os.ReadFile(self); !bytes.Equal(b, newer) {
		t.Fatal("this annex wasn't replaced")
	}
	if code, out, _ := run(); code != 0 || !strings.Contains(out, "is the newest") {
		t.Fatalf("already newest: %d %q", code, out)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(self), ".annex-update-*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}
