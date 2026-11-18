//go:build acceptance

// Package cli runs the Updatecli candidate in an isolated test workspace.
package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Runner keeps the same working directory and home across calls. Each command
// gets its own TMPDIR so a previous checkout cannot satisfy a later assertion.
type Runner struct {
	Dir, Home string
	binary    string
	env       []string
}

// New uses the binary named by UPDATECLI_BIN. Supply fixture variables as
// KEY=value; the candidate does not inherit the parent process environment.
func New(t *testing.T, env ...string) *Runner {
	t.Helper()
	bin := os.Getenv("UPDATECLI_BIN")
	if !filepath.IsAbs(bin) {
		t.Fatal("set UPDATECLI_BIN to the absolute path of the candidate binary")
	}
	// Report unexpected proxied HTTP even if the candidate handles the error.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("candidate attempted HTTP outside the local fixture")
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	r := &Runner{Dir: t.TempDir(), Home: t.TempDir(), binary: bin}
	const loopback = "127.0.0.1,localhost,::1"
	r.env = slices.Concat(env, []string{
		"HOME=" + r.Home, "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "NO_COLOR=1",
		"UPDATECLI_DISABLE_VERSION_CHECK=true",
		"http_proxy=" + proxy.URL, "https_proxy=" + proxy.URL, "no_proxy=" + loopback,
		"HTTP_PROXY=" + proxy.URL, "HTTPS_PROXY=" + proxy.URL, "NO_PROXY=" + loopback,
	})
	return r
}

// Result contains the exit status and combined stdout and stderr.
type Result struct {
	ExitCode int
	Output   string
}

// Run invokes any Updatecli command. A nonzero exit is returned to the test;
// a failure to start the process or a timeout fails the test itself.
func (r *Runner) Run(t *testing.T, args ...string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Dir = r.Dir
	cmd.Env = append(slices.Clip(r.env), "TMPDIR="+t.TempDir())
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("run updatecli: %v", ctx.Err())
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run updatecli: %v", err)
		}
		return Result{ExitCode: exit.ExitCode(), Output: string(out)}
	}
	return Result{Output: string(out)}
}

// Pipeline writes updatecli.yaml in Dir and runs the given pipeline subcommand.
func (r *Runner) Pipeline(t *testing.T, mode, manifest string, flags ...string) Result {
	t.Helper()
	config := filepath.Join(r.Dir, "updatecli.yaml")
	if err := os.WriteFile(config, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"pipeline", mode, "--config", config, "--disable-version-check", "--disable-udash-report"}
	return r.Run(t, append(args, flags...)...)
}
