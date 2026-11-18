//go:build acceptance

package forge

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
	"go.yaml.in/yaml/v4"
)

type forgeCLI struct {
	runner *cli.Runner
	user   string
	token  string
}

func newCLI(t *testing.T, s *server) *forgeCLI {
	t.Helper()
	return newCLIWithCredentials(t, s.owner, s.token)
}

func newCLIWithCredentials(t *testing.T, user, token string) *forgeCLI {
	t.Helper()
	return &forgeCLI{
		runner: cli.New(t, "UPDATECLI_GITEA_USERNAME="+user, "UPDATECLI_GITEA_TOKEN="+token),
		user:   user,
		token:  token,
	}
}

func (c *forgeCLI) run(t *testing.T, mode, manifest string, flags ...string) cli.Result {
	t.Helper()
	result := c.runner.Pipeline(t, mode, manifest, flags...)
	result.Output = redactCredential(result.Output, c.user, c.token)
	return result
}

func (c *forgeCLI) ok(t *testing.T, mode, manifest string, flags ...string) string {
	t.Helper()
	r := c.run(t, mode, manifest, flags...)
	if r.ExitCode != 0 {
		t.Fatalf("pipeline %s: exit status %d, want 0", mode, r.ExitCode)
	}
	return r.Output
}

// requireOutput reports only the expected, test-authored markers.
func requireOutput(t *testing.T, output string, want ...string) {
	t.Helper()
	for _, marker := range want {
		if !strings.Contains(output, marker) {
			t.Fatalf("CLI output does not contain %q", marker)
		}
	}
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
func requestManifest(t *testing.T, r *repository, id, value string, actionSpec map[string]any) string {
	t.Helper()
	action := map[string]any{"kind": "gitea/pullrequest", "scmid": "origin"}
	if len(actionSpec) > 0 {
		action["spec"] = actionSpec
	}
	return manifestYAML(t, map[string]any{
		"name":       "Update the fixture version",
		"pipelineid": id,
		"scms": map[string]any{
			"origin": map[string]any{
				"kind": "gitea",
				"spec": map[string]any{
					"url": r.server.url, "owner": r.server.owner,
					"repository": r.name, "branch": r.base,
				},
			},
		},
		"targets": map[string]any{
			"version": map[string]any{
				"name": "Update the version", "kind": "yaml", "scmid": "origin",
				"spec": map[string]any{"file": "version.yaml", "key": "$.version", "value": value},
			},
		},
		"actions": map[string]any{"request": action},
	})
}
