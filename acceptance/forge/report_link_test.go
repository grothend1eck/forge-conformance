//go:build acceptance

package forge

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func testWorkflowRunLink(t *testing.T, s *server) {
	r := s.newRepo(t)
	// Simulate only the runner environment. No Actions workflow is executed.
	c := &forgeCLI{
		runner: cli.New(t,
			"UPDATECLI_GITEA_USERNAME="+s.owner,
			"UPDATECLI_GITEA_TOKEN="+s.token,
			"GITEA_ACTIONS=true",
			"GITHUB_ACTION=fixture",
			"GITHUB_SERVER_URL=https://ci.example.invalid",
			"GITHUB_REPOSITORY=fixture/workflows",
			"GITHUB_RUN_ID=9001",
			"GITHUB_RUN_NUMBER=42",
		),
		user:  s.owner,
		token: s.token,
	}
	c.ok(t, "apply", requestManifest(t, r, "gitea-actions-run-link", "v1.1.0", nil))

	pr := onlyOpenPullRequest(t, r)
	body := redactCredential(pr.Body, c.user, c.token)
	const runs = "https://ci.example.invalid/fixture/workflows/actions/runs/"
	require.Contains(t, body, `<a href="`+runs+`42?by_index=1">Gitea Actions workflow link</a>`)
	require.NotContains(t, body, runs+"9001", "the report must not use GITHUB_RUN_ID")
}
