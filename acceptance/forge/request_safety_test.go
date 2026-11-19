//go:build acceptance

package forge

import (
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRequestSafety(t *testing.T, s *server) {
	runRequired(t, "generated_report_persists", func(t *testing.T) {
		testGeneratedPRReport(t, s)
	})
	runRequired(t, "explicit_patch_branches", func(t *testing.T) {
		testPRBranchOverrides(t, s)
	})

	runRequired(t, "draft_survives_edit", func(t *testing.T) {
		const id = "draft-survives-edit"
		const title = "Acceptance: " + id
		r, before := newProtectedMergeRepo(t, s)
		newCLI(t, s).ok(t, "apply", requestManifest(t, r, id, "v1.1.0", map[string]any{
			"title": title, "merge": map[string]any{"strategy": "manual"}, "mergemethod": "merge",
		}))
		opened := r.assertOpened(t, "after initial Updatecli apply", before, title, false)

		// Mark an existing non-draft PR as WIP outside Updatecli, as a maintainer would.
		_, resp, err := s.admin.PullRequests.EditPullRequest(t.Context(), s.owner, r.name, opened.number,
			gitea.EditPullRequestOption{Title: "WIP: Hold for a human review"})
		if err != nil {
			s.apiFailure(t, "mark the existing PR as a draft", resp, err)
		}
		require.True(t, r.readMergeSnapshot(t, "after maintainer marks the PR as WIP", opened.number).draft)

		const newTitle = "Updated version with automerge requested"
		newCLI(t, s).ok(t, "apply", requestManifest(t, r, id, "v1.2.0", map[string]any{
			"title": newTitle,
			"merge": map[string]any{"strategy": "auto"},
		}))
		const phase = "after applying changed input to a human draft"
		edited := r.readMergeSnapshot(t, phase, opened.number)
		require.NotEmpty(t, edited.headSHA)
		require.NotEqual(t, opened.headSHA, edited.headSHA)
		want := opened
		want.title, want.draft = "WIP: "+newTitle, true
		want.headSHA, want.headTip = edited.headSHA, edited.headSHA
		assertOpen(t, phase, edited, want)
		contents, _ := r.readCommit(t, edited.headBranch, edited.headSHA)
		require.Equal(t, "version: v1.2.0\nkeep: untouched\n", contents)
		r.assertRequestNeverScheduled(t, phase, edited)
	})

	runRequired(t, "review_failure_blocks_automerge", func(t *testing.T) {
		const (
			id    = "review-failure-blocks-automerge"
			title = "Required reviewer does not exist"
		)
		r, before := newProtectedMergeRepo(t, s)
		result := newCLI(t, s).run(t, "apply", requestManifest(t, r, id, "v1.1.0", map[string]any{
			"title": title, "reviewers": []string{"acceptance-nonexistent-reviewer"},
			"merge": map[string]any{"strategy": "auto"},
		}))
		t.Logf("apply with nonexistent reviewer exited %d", result.ExitCode)
		assert.NotZero(t, result.ExitCode, "review failure must fail the CLI")
		assert.True(t, strings.Contains(result.Output, "requesting reviewers for Gitea pull request"),
			"the action must fail at the review request, not another stage")

		const phase = "after the forge rejects the nonexistent reviewer"
		opened := r.assertOpened(t, phase, before, title, false)
		r.assertRequestNeverScheduled(t, phase, opened)
	})
}

func (r *repository) assertRequestNeverScheduled(t *testing.T, phase string, opened mergeSnapshot) {
	t.Helper()
	r.assertScheduledBy(t, phase, opened.number)
	require.Zero(t, r.latestStatus(t, opened.headSHA, requiredMergeCheck).id, "the exact head must start without the required check")
	r.publishStatus(t, opened.headSHA, requiredMergeCheck, "success")
	const passed = "after the required check succeeds on the exact PR head"
	r.assertUnmergedFor(t, passed, opened, 2*time.Second)
	r.assertScheduledBy(t, passed, opened.number)
}
