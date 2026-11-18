//go:build acceptance && knownissues

package forge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestExistingReleaseOnLaterPage is opt-in until the release target can find
// existing releases outside the first API page.
func TestExistingReleaseOnLaterPage(t *testing.T) {
	for _, provider := range []string{"gitea", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			s := startServer(t, provider)
			r, release := seedReleaseOnLaterPage(t, s)
			c := newCLI(t, s)
			before := r.refs(t)
			target := resourceNode(s, r.name, "release", map[string]any{"tag": release.TagName, "commitish": r.base})
			target["disablesourceinput"] = true
			manifest := manifestYAML(t, map[string]any{
				"name":    "Ensure a release already on page 2 exists",
				"targets": map[string]any{"ensure": target},
			})

			preview := c.run(t, "diff", manifest)
			assert.Zero(t, preview.ExitCode, "%s", preview.Output)
			assert.NotContains(t, preview.Output, "doesn't exist and should be created",
				"diff must not plan creation of existing release %s (ID %d)", release.TagName, release.ID)
			assert.Equal(t, before, r.refs(t), "diff must not change remote refs")

			applied := c.run(t, "apply", manifest)
			assert.Zero(t, applied.ExitCode, "ensuring existing release %s (ID %d) must succeed:\n%s", release.TagName, release.ID, applied.Output)
			assert.Equal(t, before, r.refs(t), "apply must not change remote refs")
			got, resp, err := s.client.Releases.GetReleaseByTag(t.Context(), s.owner, r.name, release.TagName)
			if err != nil {
				s.apiFailure(t, "read existing release after ensure", resp, err)
			}
			assert.Equal(t, release.ID, got.ID, "apply must preserve the same release")
			assert.Equal(t, release.TagName, got.TagName)
		})
	}
}
