//go:build acceptance

package forge

import (
	"net/http"
	"strings"
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
	manifest := requestManifest(t, r, "auth", "v1.1.0", nil)
	check := func(t *testing.T, user, token string, accessible bool) {
		t.Helper()
		c := newCLIWithCredentials(t, user, token)
		if accessible {
			requireOutput(t, c.ok(t, "diff", manifest), "v1.0.1", "v1.1.0")
		} else {
			result := c.run(t, "diff", manifest)
			require.NotZero(t, result.ExitCode, "anonymous clone of a private repository succeeded")
			require.False(t, strings.Contains(result.Output, "v1.0.1"), "anonymous diff reported the private branch version")
		}
		require.Equal(t, before, r.refs(t))
	}
	runRequired(t, "token_without_username", func(t *testing.T) { check(t, "", s.token, true) })
	runRequired(t, "private_without_credentials", func(t *testing.T) { check(t, "", "", false) })
	source := resourceNode(s, r.name, "branch", map[string]any{
		"versionfilter": map[string]string{"kind": "latest", "pattern": r.base},
	})
	runRequired(t, "private_rest_without_credentials", func(t *testing.T) {
		anon := s.sdkClient(t)
		_, response, err := anon.Repositories.GetRepoBranch(t.Context(), s.owner, r.name, r.base)
		if response == nil || response.StatusCode != http.StatusNotFound {
			s.apiFailure(t, "anonymous read of private branch (want HTTP 404)", response, err)
		}
		res := newCLIWithCredentials(t, "", "").run(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Reject an anonymous read of a private branch",
			"sources": map[string]any{"branch": source},
		}))
		require.Equal(t, 1, res.ExitCode, "an anonymous private REST read must fail")
		requireOutput(t, res.Output, "searching gitea branches")
		require.Equal(t, before, r.refs(t))
	})
	runRequired(t, "public_without_credentials", func(t *testing.T) {
		_, response, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, gitea.EditRepoOption{Private: new(false)})
		if err != nil {
			s.apiFailure(t, "make fixture repository public", response, err)
		}
		check(t, "", "", true)

		// This read independently establishes that the exact branch, not just
		// the Git URL, is public before trying Updatecli's REST client.
		anon := s.sdkClient(t)
		branch, response, err := anon.Repositories.GetRepoBranch(t.Context(), s.owner, r.name, r.base)
		if err != nil || response == nil || response.StatusCode != http.StatusOK {
			s.apiFailure(t, "read public fixture branch anonymously", response, err)
		}
		require.NotNil(t, branch.Commit)
		require.Equal(t, before["refs/heads/"+r.base], branch.Commit.ID)

		c := newCLIWithCredentials(t, "", "")
		target := resourceFileNode(resourceOutput, "")
		target["sourceid"] = "branch"
		writeResourceFile(t, c.runner.Dir, resourceOutput, resourceUntouched)
		c.ok(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Read a public branch without a token",
			"sources": map[string]any{"branch": source},
			"targets": map[string]any{"record": target},
		}))
		assertResourceFile(t, c.runner.Dir, resourceOutput, r.base)

		runRequired(t, "release_preview_but_no_anonymous_write", func(t *testing.T) {
			const tag = "v1.2.3"
			target := resourceNode(s, r.name, "release", map[string]any{"tag": tag})
			target["disablesourceinput"] = true
			manifest := manifestYAML(t, map[string]any{
				"name":    "Require credentials to publish a release in a public repo",
				"targets": map[string]any{"release": target},
			})
			require.Empty(t, readResourceReleases(t, s, r.name))
			requireOutput(t, c.ok(t, "diff", manifest), "doesn't exist and should be created")
			require.Equal(t, before, r.refs(t), "anonymous preview must not create a tag")
			require.Empty(t, readResourceReleases(t, s, r.name), "anonymous preview must not create a release")

			res := c.run(t, "apply", manifest)
			require.Equal(t, 1, res.ExitCode, "an anonymous release write must fail")
			requireOutput(t, res.Output, `missing parameter "token"`)
			require.Equal(t, before, r.refs(t), "rejected apply must not create a tag")
			require.Empty(t, readResourceReleases(t, s, r.name), "rejected apply must not create a release")
		})
	})
}
