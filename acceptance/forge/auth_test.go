//go:build acceptance

package forge

import (
	"testing"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
)

func testAuthentication(t *testing.T, s *server) {
	r := s.newRepo(t)
	// A file changed only on stable proves that the binary checked out the
	// requested branch. Use diff so a successful check doesn't publish a PR.
	r.stage(t, r.base, "version.yaml", "version: v1.0.1\nkeep: untouched\n")
	before := r.refs(t)
	manifest := requestManifest(r, "auth", "v1.1.0", nil)
	check := func(t *testing.T, user, token string, accessible bool) {
		t.Helper()
		c := newCLIWithCredentials(t, user, token)
		result := c.run(t, "diff", manifest)
		if accessible {
			require.Equal(t, 0, result.ExitCode, result.Output)
			require.Contains(t, result.Output, "v1.0.1")
			require.Contains(t, result.Output, "v1.1.0")
		} else {
			require.NotZero(t, result.ExitCode, "anonymous clone of a private repository succeeded")
			require.NotContains(t, result.Output, "v1.0.1")
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
		c := newCLIWithCredentials(t, "", "")
		source := resourceNode(s, r.name, "branch", map[string]any{
			"versionfilter": map[string]string{"kind": "latest", "pattern": r.base},
		})
		target := resourceFileNode(resourceOutput, "")
		target["sourceid"] = "branch"
		writeResourceFile(t, c.runner.Dir, resourceOutput, resourceUntouched)
		c.ok(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Read a public branch without a token",
			"sources": map[string]any{"branch": source},
			"targets": map[string]any{"record": target},
		}))
		assertResourceFile(t, c.runner.Dir, resourceOutput, r.base)
	})
}
