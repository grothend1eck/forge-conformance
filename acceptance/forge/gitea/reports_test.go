//go:build acceptance

package gitea_test

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func (s *server) testRequestSafety(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *server)
	}{
		{"generated_report_persists", testGeneratedPRReport},
		{"explicit_patch_branches", testPRBranchOverrides},
		{"review_failure_blocks_automerge", testReviewFailureBlocksAutomerge},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, s) })
	}
	if s.provider == "forgejo" {
		t.Run("report_link_with_gitea_compatible_environment", s.testGiteaActionsRunLink)
	}
}

func testGeneratedPRReport(t *testing.T, s *server) {
	r := s.newRepo(t)
	// The initial apply changes the version, which is already current on the
	// next apply. The second target then contributes a different report entry.
	r.Stage(t, r.Base, "channel.yaml", "channel: stable\n")
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
		return cli.Manifest(t, config)
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
		text, _, _ := strings.Cut(cli.RedactCredential(body, s.owner, s.token), "\n---\n")
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
	c.PipelineOK(t, "apply", first)
	opened := onlyOpenPullRequest(t, r)
	require.Equal(t, firstTitle, opened.Title, "action title overrides the pipeline and target names")
	reportID, initial := report(opened.Body)
	require.Len(t, initial, 1)
	require.Contains(t, initial["Update the version"].Body, "v1.1.0")

	// New PR settings alone must not trigger an edit or schedule a merge.
	savedRefs := r.Refs(t)
	output := c.PipelineOK(t, "apply", manifest("v1.1.0", "stable", "Unapplied title", map[string]any{
		"body": "Unapplied body", "merge": map[string]any{"strategy": "auto"},
	}))
	cli.RequireOutput(t, output, opened.HTMLURL)
	noOp := onlyOpenPullRequest(t, r)
	require.Equal(t, giteaRequest(t, opened), giteaRequest(t, noOp))
	require.True(t, opened.Body == noOp.Body, "an unchanged apply must not edit the persisted body")
	require.Equal(t, savedRefs, r.Refs(t))
	r.assertScheduledBy(t, "after applying PR settings without target changes", opened.Index)

	c.PipelineOK(t, "apply", manifest("v1.1.0", "beta", firstTitle, map[string]any{"title": specTitle}))
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
	refs := r.Refs(t)
	savedRefs["refs/heads/"+updated.Head.Ref] = updated.Head.Sha
	require.Equal(t, savedRefs, refs, "only the working branch may advance")
	r.Git(t, "fetch", "origin", "refs/heads/"+updated.Head.Ref)
	require.Equal(t, "version: v1.1.0\nkeep: untouched\n", r.Git(t, "show", updated.Head.Sha+":version.yaml"))
	require.Equal(t, "channel: beta\n", r.Git(t, "show", updated.Head.Sha+":channel.yaml"))

	// Advancing the base independently diverges it from the working branch.
	// The force-push resets the report. The old version entry is no longer
	// relevant, even though this is still the same request.
	r.Stage(t, r.Base, "version.yaml", "version: v1.1.0\nkeep: untouched\n")
	newBase := r.Refs(t)["refs/heads/"+r.Base]
	c.PipelineOK(t, "apply", manifest("v1.1.0", "rc", firstTitle, nil))
	reset := onlyOpenPullRequest(t, r)
	require.Equal(t, opened.ID, reset.ID)
	require.Equal(t, opened.Index, reset.Index)
	require.NotEqual(t, updated.Head.Sha, reset.Head.Sha)
	r.Git(t, "fetch", "origin", "refs/heads/"+reset.Head.Ref)
	require.Equal(t, newBase, strings.TrimSpace(r.Git(t, "rev-parse", reset.Head.Sha+"^")), "working branch must reset onto the new base")
	resetID, resetEntries := report(reset.Body)
	require.Equal(t, reportID, resetID)
	require.Len(t, resetEntries, 1)
	require.Contains(t, resetEntries["Change the channel"].Body, "rc")
	require.Equal(t, "channel: rc\n", r.Git(t, "show", reset.Head.Sha+":channel.yaml"))
}

func (s *server) testGiteaActionsRunLink(t *testing.T) {
	r := s.newRepo(t)
	// Test the report with a simulated Gitea Actions environment, not a live
	// runner or link. The Forgejo compatibility case sets the same variables.
	c := cli.NewWithCredentials(t, s.owner, s.token,
		"UPDATECLI_GITEA_USERNAME="+s.owner,
		"UPDATECLI_GITEA_TOKEN="+s.token,
		"GITEA_ACTIONS=true",
		"GITHUB_ACTION=fixture",
		"GITHUB_SERVER_URL=https://ci.example.invalid",
		"GITHUB_REPOSITORY=fixture/workflows",
		"GITHUB_RUN_ID=9001",
		"GITHUB_RUN_NUMBER=42",
	)
	c.PipelineOK(t, "apply", requestManifest(t, r, "gitea-actions-run-link", "v1.1.0", nil))

	pr := onlyOpenPullRequest(t, r)
	body := cli.RedactCredential(pr.Body, s.owner, s.token)
	const runs = "https://ci.example.invalid/fixture/workflows/actions/runs/"
	require.Contains(t, body, `<a href="`+runs+`42?by_index=1">Gitea Actions workflow link</a>`)
	require.NotContains(t, body, runs+"9001", "the report must not use GITHUB_RUN_ID")
}
