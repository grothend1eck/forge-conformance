//go:build acceptance

// Package cli runs the Updatecli candidate in an isolated test workspace.
package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"
)

// Runner keeps the same working directory and home across calls. Each command
// gets its own TMPDIR so a previous checkout cannot satisfy a later assertion.
// Do not use a Runner concurrently; the commands may edit its workspace.
type Runner struct {
	Dir, Home   string
	binary      string
	env         []string
	user, token string
}

// New uses the binary named by UPDATECLI_BIN. Supply fixture variables as
// KEY=value, including PATH to replace the inherited PATH default. No other
// parent variables are copied, and runner-controlled settings take precedence.
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
	// exec.Cmd uses the last value for duplicate keys.
	r.env = slices.Concat([]string{"PATH=" + os.Getenv("PATH")}, env, []string{
		"HOME=" + r.Home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "NO_COLOR=1",
		"UPDATECLI_DISABLE_VERSION_CHECK=true",
		"http_proxy=" + proxy.URL, "https_proxy=" + proxy.URL, "no_proxy=" + loopback,
		"HTTP_PROXY=" + proxy.URL, "HTTPS_PROXY=" + proxy.URL, "NO_PROXY=" + loopback,
	})
	return r
}

// NewWithCredentials is like New but redacts the supplied fixture credentials
// from command output. The caller passes credentials via provider-specific env.
func NewWithCredentials(t *testing.T, user, token string, env ...string) *Runner {
	t.Helper()
	r := New(t, env...)
	r.user, r.token = user, token
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
	result := Result{Output: RedactCredential(string(out), r.user, r.token)}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run updatecli: %v", err)
		}
		result.ExitCode = exit.ExitCode()
	}
	return result
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

// Manifest encodes test-authored Updatecli configuration.
func Manifest(t *testing.T, spec map[string]any) string {
	t.Helper()
	b, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("encode Updatecli manifest: %v", err)
	}
	return string(b)
}

// PipelineOK runs a pipeline and requires a successful exit.
func (r *Runner) PipelineOK(t *testing.T, mode, manifest string, flags ...string) string {
	t.Helper()
	result := r.Pipeline(t, mode, manifest, flags...)
	if result.ExitCode != 0 {
		t.Fatalf("pipeline %s: exit status %d, want 0", mode, result.ExitCode)
	}
	return result.Output
}

// RequireOutput reports missing test-authored markers without logging output.
func RequireOutput(t *testing.T, output string, want ...string) {
	t.Helper()
	for _, marker := range want {
		if !strings.Contains(output, marker) {
			t.Fatalf("CLI output does not contain %q", marker)
		}
	}
}

// RedactCredential removes a fixture PAT and its HTTP Basic encoding.
func RedactCredential(text, user, token string) string {
	if token == "" {
		return text
	}
	if user == "" {
		user = "oauth2"
	}
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	return strings.NewReplacer(token, "[redacted]", basic, "[redacted]").Replace(text)
}
