//go:build acceptance

package forge

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

type forgeCLI struct {
	runner *cli.Runner
	user   string
	token  string
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
