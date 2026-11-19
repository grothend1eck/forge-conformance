//go:build acceptance && knownissues

package gitea_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

// TestPrivateNativeRefTargets is intentionally red until native targets use
// authenticated SCM checkout. It is not part of the qualified git_scm suite.
func TestPrivateNativeRefTargets(t *testing.T) {
	const refName = "v1.1.0"
	for _, provider := range []string{"gitea", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			s := startServer(t, provider)
			for _, tc := range []struct {
				kind, ref, preview string
				workingBranch      bool
				spec               forgeMap
			}{
				{"gitbranch", "refs/heads/" + refName, `git branch "v1.1.0" should be created`, false,
					forgeMap{"branch": refName}},
				// A direct base checkout already works for tags. With a managed
				// working branch, the target's first checkout of the base is anonymous.
				{"gittag", "refs/tags/" + refName, `git tag "v1.1.0" should be created`, true,
					nativeTagSpec(refName, "Private target acceptance tag")},
			} {
				t.Run(tc.kind, func(t *testing.T) {
					r := s.newRepo(t)
					r.Stage(t, r.Base, "stable-only.txt", "configured branch tip\n")
					r.Git(t, "push", "origin", "refs/heads/main:refs/tags/v1.0.0")
					before := r.Refs(t)
					base := before["refs/heads/"+r.Base]
					require.NotEmpty(t, base)
					require.NotEqual(t, before["refs/heads/main"], base)
					require.NotContains(t, before, tc.ref)

					spec := scmTestSpec(r, forgeMap{"workingbranch": tc.workingBranch})
					manifest := nativeRefManifest(t, spec, tc.kind, tc.spec)
					c := newCLI(t, s)
					if tc.kind == "gittag" {
						configureNativeTagger(t, c)
					}
					cli.RequireOutput(t, c.PipelineOK(t, "diff", manifest), tc.preview)
					require.Equal(t, before, r.Refs(t), "diff must not publish refs")

					result := c.Pipeline(t, "apply", manifest, "--commit=false")
					authRejected := strings.Contains(result.Output, "Git checkout branch error") &&
						strings.Contains(result.Output, "authentication required")
					assert.Zero(t, result.ExitCode, "private apply should succeed (checkout authentication rejected: %t)", authRejected)
					after := r.Refs(t)
					assert.NotEmpty(t, after[tc.ref], "private apply should publish %s", tc.ref)
					if tc.kind == "gittag" {
						peeled := tc.ref + "^{}"
						assert.Equal(t, base+"\t"+peeled+"\n", r.Git(t, "ls-remote", "origin", peeled),
							"the annotated tag must peel to the configured base")
					} else {
						assert.Equal(t, base, after[tc.ref], "the branch must point to the configured base")
					}
					delete(after, tc.ref)
					assert.Equal(t, before, after, "apply must not change unrelated refs")
				})
			}
		})
	}
}
