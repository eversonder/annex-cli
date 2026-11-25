package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// update replaces this annex with the build its Annex serves for this
// computer, checked against the hash Annex publishes beside it, the same way
// the installer checks it. The new file is written next to the old one and
// renamed over it, so a cut-off download never leaves a broken annex.
// ponytail: it trusts the same address it downloads from, as install.sh does.
func (a *App) update(ctx context.Context, _ []string) error {
	self := a.Self
	if self == "" {
		p, err := os.Executable()
		if err != nil {
			return fmt.Errorf("can't tell where this annex is: %w", err)
		}
		if self, err = filepath.EvalSymlinks(p); err != nil {
			return err
		}
	}
	build := "annex-" + runtime.GOOS + "-" + runtime.GOARCH
	base := strings.TrimRight(a.cfg.URL, "/") + "/cli/bin/" + build
	// The same version is the newest, whatever built it (go install makes another file of it).
	if v, err := a.fetch(ctx, strings.TrimRight(a.cfg.URL, "/")+"/cli/version", 64); err == nil && strings.TrimSpace(string(v)) == Version {
		fmt.Fprintf(a.Out, "annex %s is the newest.\n", Version)
		return nil
	}

	want, err := a.fetch(ctx, base+".sha256", 1<<10)
	if err != nil {
		return err
	}
	sum := strings.TrimSpace(string(want))
	if len(sum) != 64 {
		return fmt.Errorf("%s didn't say what the new build should be", a.cfg.URL)
	}
	if have, err := os.ReadFile(self); err == nil {
		if h := sha256.Sum256(have); hex.EncodeToString(h[:]) == sum {
			fmt.Fprintf(a.Out, "annex %s is the newest.\n", Version)
			return nil
		}
	}

	dir := filepath.Dir(self)
	tmp, err := os.CreateTemp(dir, ".annex-update-*")
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("can't write to %s. Run: sudo annex update", dir)
	}
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	res, err := a.HTTP.Do(req)
	if err != nil {
		tmp.Close()
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("%s has no build for this computer (%s): %s", a.cfg.URL, build, res.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(res.Body, 256<<20)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return errors.New("the download doesn't match what Annex says it should be. Nothing changed; try again")
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), self); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("can't replace %s. Run: sudo annex update", self)
		}
		return err
	}
	now := "a newer build"
	if out, err := exec.CommandContext(ctx, self, "version").Output(); err == nil {
		now = strings.TrimSpace(strings.TrimPrefix(string(out), "annex "))
	}
	fmt.Fprintf(a.Out, "Updated annex %s to %s.\n", Version, now)
	return nil
}

// fetch GETs a small file from Annex, at most limit bytes.
func (a *App) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s has no build for this computer (%s-%s): %s", a.cfg.URL, runtime.GOOS, runtime.GOARCH, res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, limit))
}
