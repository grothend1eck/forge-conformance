//go:build acceptance

package forge

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testGeneratedPRReport(t *testing.T, s *server) {
	r := s.newRepo(t)
	// The initial apply changes the version, which is already current on the
	// next apply. The second target then contributes a different report entry.
	r.stage(t, r.base, "channel.yaml", "channel: stable\n")
	c := newCLI(t, s)
	const (
		id         = "generated-report"
		firstTitle = "Promote the fixture"
		specTitle  = "Promotion with channel"
	)
	manifest := func(version, channel, actionTitle string, spec map[string]any) string {
		t.Helper()
		config := requestConfig(r, id, version, spec)
		config["actions"].(map[string]any)["request"].(map[string]any)["title"] = actionTitle
		config["targets"].(map[string]any)["channel"] = map[string]any{
			"name": "Change the channel", "kind": "yaml", "scmid": "origin",
			"spec": map[string]any{"file": "channel.yaml", "key": "$.channel", "value": channel},
		}
		return manifestYAML(t, config)
	}
	type entry struct {
		ID    string `xml:"id,attr"`
		Title string `xml:"summary"`
		Body  string `xml:"p"`
	}
	report := func(body string) (string, map[string]entry) {
		t.Helper()
		// Observe the stored XML report rather than regenerating its expected
		// body with Updatecli's code. The decorative Markdown footer isn't XML.
		text, _, _ := strings.Cut(redactCredential(body, s.owner, s.token), "\n---\n")
		var got struct {
			XMLName xml.Name `xml:"Actions"`
			Actions []struct {
				ID      string  `xml:"id,attr"`
				Targets []entry `xml:"details"`
			} `xml:"action"`
		}
		require.NoError(t, xml.Unmarshal([]byte(text), &got))
		require.Len(t, got.Actions, 1, "the same run identity must not produce duplicate report sections")
		require.NotEmpty(t, got.Actions[0].ID)
		entries := map[string]entry{}
		for _, target := range got.Actions[0].Targets {
			require.NotEmpty(t, target.ID)
			require.NotContains(t, entries, target.Title)
			entries[target.Title] = target
		}
		return got.Actions[0].ID, entries
	}
	first := manifest("v1.1.0", "stable", firstTitle, nil)
	c.ok(t, "apply", first)
	opened := onlyOpenPullRequest(t, r)
	require.Equal(t, firstTitle, opened.Title, "action title overrides the pipeline and target names")
	reportID, initial := report(opened.Body)
	require.Len(t, initial, 1)
	require.Contains(t, initial["Update the version"].Body, "v1.1.0")

	// New PR settings alone must not trigger an edit or schedule a merge.
	savedRefs := r.refs(t)
	output := c.ok(t, "apply", manifest("v1.1.0", "stable", "Unapplied title", map[string]any{
		"body": "Unapplied body", "merge": map[string]any{"strategy": "auto"},
	}))
	requireOutput(t, output, opened.HTMLURL)
	noOp := onlyOpenPullRequest(t, r)
	require.Equal(t, giteaRequest(t, opened), giteaRequest(t, noOp))
	require.True(t, opened.Body == noOp.Body, "an unchanged apply must not edit the persisted body")
	require.Equal(t, savedRefs, r.refs(t))
	r.assertScheduledBy(t, "after applying PR settings without target changes", opened.Index)

	c.ok(t, "apply", manifest("v1.1.0", "beta", firstTitle, map[string]any{"title": specTitle}))
	updated := onlyOpenPullRequest(t, r)
	require.Equal(t, opened.ID, updated.ID)
	require.Equal(t, opened.Index, updated.Index)
	require.NotEqual(t, opened.Head.Sha, updated.Head.Sha)
	require.Equal(t, specTitle, updated.Title, "spec.title overrides the action title when both are set")
	updatedID, merged := report(updated.Body)
	require.Equal(t, reportID, updatedID)
	require.Len(t, merged, 2)
	require.Equal(t, initial["Update the version"], merged["Update the version"], "unchanged target report must survive")
	require.Contains(t, merged["Change the channel"].Body, "beta")
	refs := r.refs(t)
	savedRefs["refs/heads/"+updated.Head.Ref] = updated.Head.Sha
	require.Equal(t, savedRefs, refs, "only the working branch may advance")
	r.git(t, "fetch", "origin", "refs/heads/"+updated.Head.Ref)
	require.Equal(t, "version: v1.1.0\nkeep: untouched\n", r.git(t, "show", updated.Head.Sha+":version.yaml"))
	require.Equal(t, "channel: beta\n", r.git(t, "show", updated.Head.Sha+":channel.yaml"))

	// Advancing the base independently diverges it from the working branch.
	// The force-push resets the report. The old version entry is no longer
	// relevant, even though this is still the same request.
	r.stage(t, r.base, "version.yaml", "version: v1.1.0\nkeep: untouched\n")
	newBase := r.refs(t)["refs/heads/"+r.base]
	c.ok(t, "apply", manifest("v1.1.0", "rc", firstTitle, nil))
	reset := onlyOpenPullRequest(t, r)
	require.Equal(t, opened.ID, reset.ID)
	require.Equal(t, opened.Index, reset.Index)
	require.NotEqual(t, updated.Head.Sha, reset.Head.Sha)
	r.git(t, "fetch", "origin", "refs/heads/"+reset.Head.Ref)
	require.Equal(t, newBase, strings.TrimSpace(r.git(t, "rev-parse", reset.Head.Sha+"^")), "working branch must reset onto the new base")
	resetID, resetEntries := report(reset.Body)
	require.Equal(t, reportID, resetID)
	require.Len(t, resetEntries, 1)
	require.Contains(t, resetEntries["Change the channel"].Body, "rc")
	require.Equal(t, "channel: rc\n", r.git(t, "show", reset.Head.Sha+":channel.yaml"))
}

func testPRBranchOverrides(t *testing.T, s *server) {
	r := s.newRepo(t)
	patch := s.newRepo(t)
	c := newCLI(t, s)
	repoID := publishRepo(t, patch).ID
	const (
		id    = "patch-branches"
		base  = "release/1.x"
		head  = "patch/1.x"
		title = "Patch the supported release"
	)
	patch.stage(t, base, "support.txt", "supported release\n")
	before := patch.awaitBranches(t, base)
	// Prepare a real patch head based on release/1.x, not the SCM's stable
	// branch. The pipeline still pushes its working branch to the SCM repo.
	patch.git(t, "push", "origin", before["refs/heads/"+base]+":refs/heads/"+head)
	patch.stage(t, head, "version.yaml", "version: v1.0.1\nkeep: untouched\n")
	before = patch.awaitBranches(t, head)
	scmBefore := r.refs(t)
	manifest := requestManifest(t, r, id, "v1.1.0", map[string]any{
		"sourcebranch": head, "targetbranch": base,
		"owner": s.owner, "repository": patch.name, "title": title,
	})
	c.ok(t, "apply", manifest)
	require.Empty(t, publishPRs(t, r), "no PR should be opened in the SCM repository")
	opened := giteaRequest(t, onlyOpenPullRequest(t, patch))
	require.Equal(t, head, opened.head)
	require.Equal(t, base, opened.base)
	require.Equal(t, repoID, opened.headRepoID)
	require.Equal(t, repoID, opened.baseRepoID)
	require.Equal(t, before["refs/heads/"+head], opened.headSHA)
	require.Equal(t, before["refs/heads/"+base], opened.baseSHA)
	require.Equal(t, title, opened.title)
	require.Equal(t, "version.yaml", strings.TrimSpace(patch.git(t, "diff", "--name-only", opened.baseSHA, opened.headSHA)))
	require.Equal(t, before, patch.refs(t), "creating the PR must not move either explicit patch ref")

	gotRefs := r.refs(t)
	workingRef := "refs/heads/updatecli_" + r.base + "_" + id
	require.NotEmpty(t, gotRefs[workingRef], "the target must push its own working branch")
	require.NotEqual(t, opened.headSHA, gotRefs[workingRef])
	r.git(t, "fetch", "origin", workingRef)
	require.Equal(t, mergeFixtureAfter, r.git(t, "show", gotRefs[workingRef]+":version.yaml"))
	require.Equal(t, scmBefore["refs/heads/"+r.base], strings.TrimSpace(r.git(t, "rev-parse", gotRefs[workingRef]+"^")))
	scmBefore[workingRef] = gotRefs[workingRef]
	require.Equal(t, scmBefore, gotRefs, "only the SCM working branch should change")
}
