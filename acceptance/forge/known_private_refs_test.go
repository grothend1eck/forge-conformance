//go:build acceptance && knownissues

package forge

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrivateNativeRefTargets is intentionally red until native targets use
// authenticated SCM checkout. It is not part of the qualified git_scm suite.
func TestPrivateNativeRefTargets(t *testing.T) {
	const refName = "v1.1.0"
	for _, provider := range []string{"gitea", "forgejo"} {
		runRequired(t, provider, func(t *testing.T) {
			s := startServer(t, provider)
			for _, tc := range []struct {
				kind, ref, preview string
				workingBranch      bool
				spec               map[string]any
			}{
				{"gitbranch", "refs/heads/" + refName, `git branch "v1.1.0" should be created`, false,
					map[string]any{"branch": refName}},
				// A direct base checkout already works for tags. With a managed
				// working branch, the target's first checkout of the base is anonymous.
				{"gittag", "refs/tags/" + refName, `git tag "v1.1.0" should be created`, true,
					map[string]any{"versionfilter": map[string]any{"pattern": refName}, "message": "Private target acceptance tag"}},
			} {
				runRequired(t, tc.kind, func(t *testing.T) {
					r := s.newRepo(t)
					r.stage(t, r.base, "stable-only.txt", "configured branch tip\n")
					r.git(t, "push", "origin", "refs/heads/main:refs/tags/v1.0.0")
					before := r.refs(t)
					base := before["refs/heads/"+r.base]
					require.NotEmpty(t, base)
					require.NotEqual(t, before["refs/heads/main"], base)
					require.NotContains(t, before, tc.ref)

					spec := scmTestSpec(r)
					spec["workingbranch"] = tc.workingBranch
					manifest := nativeRefManifest(t, spec, tc.kind, tc.spec)
					c := newCLI(t, s)
					if tc.kind == "gittag" {
						configureNativeTagger(t, c)
					}
					requireOutput(t, c.ok(t, "diff", manifest), tc.preview)
					require.Equal(t, before, r.refs(t), "diff must not publish refs")

					result := c.run(t, "apply", manifest, "--commit=false")
					authRejected := strings.Contains(result.Output, "Git checkout branch error") &&
						strings.Contains(result.Output, "authentication required")
					assert.Zero(t, result.ExitCode, "private apply should succeed (checkout authentication rejected: %t)", authRejected)
					after := r.refs(t)
					assert.NotEmpty(t, after[tc.ref], "private apply should publish %s", tc.ref)
					if tc.kind == "gittag" {
						peeled := tc.ref + "^{}"
						assert.Equal(t, base+"\t"+peeled+"\n", r.git(t, "ls-remote", "origin", peeled),
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
