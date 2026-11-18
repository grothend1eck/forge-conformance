//go:build acceptance

package forge

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

type cli struct {
	binary string
	dir    string
	home   string
	user   string
	token  string
	proxy  string
}

func testAuthentication(t *testing.T, s *server) {
	r := s.newRepo(t)
	// A file changed only on stable proves that the binary checked out the
	// requested branch. Use diff so a successful check doesn't publish a PR.
	r.stage(t, r.base, "version.yaml", "version: v1.0.1\nkeep: untouched\n")
	before := r.refs(t)
	manifest := requestManifest(r, "auth", "v1.1.0", nil)
	check := func(t *testing.T, user, token string, accessible bool) {
		t.Helper()
		c := newCLI(t, s)
		c.user, c.token = user, token
		result := c.run(t, "diff", manifest)
		if accessible {
			require.Equal(t, 0, result.code, result.output)
			require.Contains(t, result.output, "v1.0.1")
			require.Contains(t, result.output, "v1.1.0")
		} else {
			require.NotZero(t, result.code, "anonymous clone of a private repository succeeded")
			require.NotContains(t, result.output, "v1.0.1")
		}
		require.Equal(t, before, r.refs(t))
	}
	t.Run("token_without_username", func(t *testing.T) { check(t, "", s.token, true) })
	t.Run("private_without_credentials", func(t *testing.T) { check(t, "", "", false) })
	t.Run("public_without_credentials", func(t *testing.T) {
		_, response, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, gitea.EditRepoOption{Private: new(false)})
		if err != nil {
			s.apiFailure(t, "make fixture repository public", response, err)
		}
		check(t, "", "", true)

		// Public REST lookups don't use the Git transport tested above.
		c := newCLI(t, s)
		c.user, c.token = "", ""
		source := resourceNode(s, r.name, "branch", map[string]any{
			"versionfilter": map[string]string{"kind": "latest", "pattern": r.base},
		})
		target := resourceFileNode(resourceOutput, "")
		target["sourceid"] = "branch"
		writeResourceFile(t, c.dir, resourceOutput, resourceUntouched)
		c.ok(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Read a public branch without a token",
			"sources": map[string]any{"branch": source},
			"targets": map[string]any{"record": target},
		}))
		assertResourceFile(t, c.dir, resourceOutput, r.base)
	})
}

func newCLI(t *testing.T, s *server) *cli {
	t.Helper()
	bin := os.Getenv("UPDATECLI_BIN")
	if bin == "" || !filepath.IsAbs(bin) {
		t.Fatal("set UPDATECLI_BIN to the absolute path of the candidate binary")
	}
	// Refuse an accidental public-forge fallback, even if the candidate
	// catches the network error and reports success. Loopback bypasses this proxy.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("candidate attempted HTTP outside the local forge")
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	return &cli{binary: bin, dir: t.TempDir(), home: t.TempDir(), user: s.owner, token: s.token, proxy: proxy.URL}
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
		"http_proxy=" + c.proxy, "https_proxy=" + c.proxy, "no_proxy=127.0.0.1,localhost,::1",
		"UPDATECLI_GITEA_USERNAME=" + c.user, "UPDATECLI_GITEA_TOKEN=" + c.token,
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("pipeline %s: %v", mode, ctx.Err())
	}
	text := redactCredential(string(out), c.user, c.token)
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
	if user == "" {
		user = "oauth2"
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
