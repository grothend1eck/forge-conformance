//go:build acceptance && knownissues

package forge

import (
	"testing"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNativeGitBranchPush is opt-in until the Gitea SCM can publish a branch.
func TestNativeGitBranchPush(t *testing.T) {
	for _, provider := range []string{"gitea", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			s := startServer(t, provider)
			r := s.newRepo(t)
			// Anonymous checkout must work so this test reaches the push.
			// Only the disposable loopback fixture is made public.
			repo, resp, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, gitea.EditRepoOption{
				Private: new(false),
			})
			if err != nil {
				s.apiFailure(t, "make local fixture repository public", resp, err)
			}
			require.False(t, repo.Private)

			const branch = "acceptance-new-head"
			const headRef, tagRef = "refs/heads/" + branch, "refs/tags/" + branch
			r.stage(t, r.base, "stable-only.txt", "configured branch tip\n")
			r.git(t, "push", "origin", "main:"+tagRef)
			before := r.refs(t)
			base := before["refs/heads/"+r.base]
			require.NotEmpty(t, base)
			require.Equal(t, before["refs/heads/main"], before[tagRef])
			require.NotEqual(t, base, before[tagRef])
			require.NotContains(t, before, headRef)

			spec := scmTestSpec(r)
			spec["workingbranch"] = false
			manifest := manifestYAML(t, map[string]any{
				"name": "Publish a native Git branch",
				"scms": map[string]any{"repo": map[string]any{"kind": "gitea", "spec": spec}},
				"targets": map[string]any{
					"branch": map[string]any{
						"name": "Publish a Git branch", "kind": "gitbranch", "scmid": "repo",
						"spec": map[string]any{"branch": branch},
					},
				},
			})
			result := newCLI(t, s).run(t, "apply", manifest, "--commit=false")
			after := r.refs(t)
			assert.Zero(t, result.ExitCode, "gitbranch apply should succeed")
			assert.Equal(t, base, after[headRef], "apply exit %d: %s must point to the configured base", result.ExitCode, headRef)
			delete(after, headRef)
			assert.Equal(t, before, after, "apply must not move the same-name tag or any other ref")
		})
	}
}
