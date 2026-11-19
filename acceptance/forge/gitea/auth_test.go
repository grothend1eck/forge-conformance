//go:build acceptance

package gitea_test

import (
	"net/http"
	"strings"
	"testing"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func (s *server) testAuthentication(t *testing.T) {
	r := s.newRepo(t)
	// A file changed only on stable proves that the binary checked out the
	// requested branch. Use diff so a successful check doesn't publish a PR.
	r.Stage(t, r.Base, "version.yaml", "version: v1.0.1\nkeep: untouched\n")
	before := r.Refs(t)
	manifest := requestManifest(t, r, "auth", "v1.1.0", nil)
	check := func(t *testing.T, user, token string, accessible bool) {
		t.Helper()
		c := newCLIWithCredentials(t, user, token)
		if accessible {
			cli.RequireOutput(t, c.PipelineOK(t, "diff", manifest), "v1.0.1", "v1.1.0")
		} else {
			result := c.Pipeline(t, "diff", manifest)
			require.NotZero(t, result.ExitCode, "anonymous clone of a private repository succeeded")
			require.False(t, strings.Contains(result.Output, "v1.0.1"), "anonymous diff reported the private branch version")
		}
		require.Equal(t, before, r.Refs(t))
	}
	t.Run("token_without_username", func(t *testing.T) { check(t, "", s.token, true) })
	t.Run("private_without_credentials", func(t *testing.T) { check(t, "", "", false) })
	source := resourceNode(s, r.name, "branch", forgeMap{
		"versionfilter": map[string]string{"kind": "latest", "pattern": r.Base},
	})
	t.Run("private_rest_without_credentials", func(t *testing.T) {
		anon := s.sdkClient(t)
		_, response, err := anon.Repositories.GetRepoBranch(t.Context(), s.owner, r.name, r.Base)
		if response == nil || response.StatusCode != http.StatusNotFound {
			s.apiFailure(t, "anonymous read of private branch (want HTTP 404)", response, err)
		}
		res := newCLIWithCredentials(t, "", "").Pipeline(t, "apply", cli.Manifest(t, forgeMap{
			"name":    "Reject an anonymous read of a private branch",
			"sources": forgeMap{"branch": source},
		}))
		require.Equal(t, 1, res.ExitCode, "an anonymous private REST read must fail")
		cli.RequireOutput(t, res.Output, "searching gitea branches")
		require.Equal(t, before, r.Refs(t))
	})
	t.Run("public_without_credentials", func(t *testing.T) {
		_, response, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, gitea.EditRepoOption{Private: new(false)})
		s.check(t, "make fixture repository public", response, err)
		check(t, "", "", true)

		// This read independently establishes that the exact branch, not just
		// the Git URL, is public before trying Updatecli's REST client.
		anon := s.sdkClient(t)
		branch, response, err := anon.Repositories.GetRepoBranch(t.Context(), s.owner, r.name, r.Base)
		if err != nil || response == nil || response.StatusCode != http.StatusOK {
			s.apiFailure(t, "read public fixture branch anonymously", response, err)
		}
		require.NotNil(t, branch.Commit)
		require.Equal(t, before["refs/heads/"+r.Base], branch.Commit.ID)

		c := newCLIWithCredentials(t, "", "")
		writeResourceFile(t, c.Dir, resourceOutput, resourceUntouched)
		c.PipelineOK(t, "apply", cli.Manifest(t, forgeMap{
			"name":    "Read a public branch without a token",
			"sources": forgeMap{"branch": source},
			"targets": forgeMap{"record": resourceFileNode(resourceOutput, "", forgeMap{"sourceid": "branch"})},
		}))
		assertResourceFile(t, c.Dir, resourceOutput, r.Base)

		t.Run("release_preview_but_no_anonymous_write", func(t *testing.T) {
			const tag = "v1.2.3"
			manifest := singleTargetYAML(t, "Require credentials to publish a release in a public repo", "release", explicitReleaseNode(r, forgeMap{"tag": tag}))
			require.Empty(t, readResourceReleases(t, s, r.name))
			cli.RequireOutput(t, c.PipelineOK(t, "diff", manifest), "doesn't exist and should be created")
			require.Equal(t, before, r.Refs(t), "anonymous preview must not create a tag")
			require.Empty(t, readResourceReleases(t, s, r.name), "anonymous preview must not create a release")

			res := c.Pipeline(t, "apply", manifest)
			require.Equal(t, 1, res.ExitCode, "an anonymous release write must fail")
			cli.RequireOutput(t, res.Output, `missing parameter "token"`)
			require.Equal(t, before, r.Refs(t), "rejected apply must not create a tag")
			require.Empty(t, readResourceReleases(t, s, r.name), "rejected apply must not create a release")
		})
	})
}
