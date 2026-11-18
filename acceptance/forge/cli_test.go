//go:build acceptance

package forge

import (
	"encoding/base64"
	"fmt"
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
	require.Equal(t, 0, r.ExitCode, "pipeline %s:\n%s", mode, r.Output)
	return r.Output
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
