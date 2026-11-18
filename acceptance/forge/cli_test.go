//go:build acceptance

package forge

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

type cli struct {
	binary string
	dir    string
	home   string
	server *server
}

func newCLI(t *testing.T, s *server) *cli {
	t.Helper()
	bin := os.Getenv("UPDATECLI_BIN")
	if bin == "" || !filepath.IsAbs(bin) {
		t.Fatal("set UPDATECLI_BIN to the absolute path of the candidate binary")
	}
	return &cli{binary: bin, dir: t.TempDir(), home: t.TempDir(), server: s}
}

type runResult struct {
	code   int
	output string
}

func (c *cli) run(t *testing.T, mode, manifest string, flags ...string) runResult {
	t.Helper()
	config := filepath.Join(c.dir, "updatecli.yaml")
	require.NoError(t, os.WriteFile(config, []byte(manifest), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	args := []string{"pipeline", mode, "--config", config, "--disable-version-check", "--disable-udash-report"}
	cmd := exec.CommandContext(ctx, c.binary, append(args, flags...)...)
	cmd.Dir = c.dir
	cmd.WaitDelay = 3 * time.Second
	// A fresh clone on each invocation makes the remote, not a previous run's
	// working copy, the source of truth. Never pass host or Actions credentials.
	cmd.Env = []string{
		"HOME=" + c.home, "TMPDIR=" + t.TempDir(), "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "NO_COLOR=1",
		"UPDATECLI_GITEA_USERNAME=" + c.server.owner, "UPDATECLI_GITEA_TOKEN=" + c.server.token,
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("pipeline %s: %v", mode, ctx.Err())
	}
	text := redactCredential(string(out), c.server.owner, c.server.token)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return runResult{exit.ExitCode(), text}
	}
	require.NoError(t, err)
	return runResult{0, text}
}

func (c *cli) ok(t *testing.T, mode, manifest string, flags ...string) string {
	t.Helper()
	r := c.run(t, mode, manifest, flags...)
	require.Equal(t, 0, r.code, "pipeline %s:\n%s", mode, r.output)
	return r.output
}

func redactCredential(text, user, token string) string {
	if token == "" {
		return text
	}
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	return strings.NewReplacer(token, "[redacted]", basic, "[redacted]").Replace(text)
}

func manifestYAML(t *testing.T, spec map[string]any) string {
	t.Helper()
	b, err := yaml.Marshal(spec)
	require.NoError(t, err)
	return string(b)
}

// requestManifest leaves the action's connection and branches for the SCM to
// supply, just as a normal pipeline would. Only the desired edit and PR options vary.
func requestManifest(r *repository, id, value string, actionSpec map[string]any) string {
	options := ""
	if len(actionSpec) > 0 {
		b, err := yaml.Marshal(actionSpec)
		if err != nil {
			panic(err) // test definitions contain only YAML values
		}
		options = "    spec:\n      " + strings.ReplaceAll(strings.TrimSuffix(string(b), "\n"), "\n", "\n      ") + "\n"
	}
	return fmt.Sprintf(`name: Update the fixture version
pipelineid: %q
scms:
  origin:
    kind: gitea
    spec:
      url: %q
      owner: %q
      repository: %q
      branch: %q
targets:
  version:
    name: Update the version
    kind: yaml
    scmid: origin
    spec:
      file: version.yaml
      key: $.version
      value: %q
actions:
  request:
    kind: gitea/pullrequest
    scmid: origin
%s`, id, r.server.url, r.server.owner, r.name, r.base, value, options)
}
