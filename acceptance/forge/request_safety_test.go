//go:build acceptance

package forge

import (
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRequestSafety(t *testing.T, s *server) {
	t.Run("draft_survives_edit", func(t *testing.T) {
		const id = "draft-survives-edit"
		m := startMergeCase(t, giteaMergeOpener(s), mergeCaseSpec{pipelineID: id, strategy: "manual"})
		p := m.protectedMergeFixture.(*giteaMergeFixture)

		// Mark an existing non-draft PR as WIP outside Updatecli, as a maintainer would.
		_, resp, err := s.admin.PullRequests.EditPullRequest(t.Context(), s.owner, p.r.name, p.number,
			gitea.EditPullRequestOption{Title: "WIP: Hold for a human review"})
		if err != nil {
			s.apiFailure(t, "mark the existing PR as a draft", resp, err)
		}
		require.True(t, p.snapshot(t, "after maintainer marks the PR as WIP").draft)

		const newTitle = "Updated version with automerge requested"
		newCLI(t, s).ok(t, "apply", requestManifest(p.r, id, "v1.2.0", map[string]any{
			"title": newTitle,
			"merge": map[string]any{"strategy": "auto"},
		}))
		const phase = "after applying changed input to a human draft"
		edited := m.snapshot(t, phase)
		require.NotEmpty(t, edited.headSHA)
		require.NotEqual(t, m.initial.headSHA, edited.headSHA)
		m.original.title = "WIP: " + newTitle
		m.initial.draft = true
		m.initial.headSHA = edited.headSHA
		m.assertOpen(t, phase, edited)
		contents, _ := m.readCommit(t, edited.headBranch, edited.headSHA)
		require.Equal(t, "version: v1.2.0\nkeep: untouched\n", contents)
		assertRequestNeverScheduled(t, m, phase)
	})

	t.Run("review_failure_blocks_automerge", func(t *testing.T) {
		const (
			id    = "review-failure-blocks-automerge"
			title = "Required reviewer does not exist"
		)
		r := s.newRepo(t)
		p := &giteaMergeFixture{s: s, r: r}
		p.protectBase(t)
		refs := r.refs(t)
		original := mergeSeed{
			baseBranch: r.base, baseTip: refs["refs/heads/"+r.base], defaultTip: refs["refs/heads/main"],
			title: title, actor: s.owner,
		}
		result := newCLI(t, s).run(t, "apply", requestManifest(r, id, "v1.1.0", map[string]any{
			"title": title, "reviewers": []string{"acceptance-nonexistent-reviewer"},
			"merge": map[string]any{"strategy": "auto"},
		}))
		t.Logf("apply with nonexistent reviewer exited %d", result.ExitCode)
		assert.NotZero(t, result.ExitCode, "review failure must fail the CLI:\n%s", result.Output)
		assert.Contains(t, result.Output, "requesting reviewers for Gitea pull request",
			"the action must fail at the review request, not another stage")

		const phase = "after the forge rejects the nonexistent reviewer"
		prs := p.listRequests(t, phase)
		require.Len(t, prs, 1)
		require.NotNil(t, prs[0])
		p.number = prs[0].Index
		opened := p.snapshot(t, phase)
		require.False(t, opened.draft, "the failed review, not draft status, must prevent scheduling")
		require.NotEmpty(t, opened.headSHA)
		require.NotEqual(t, original.baseTip, opened.headSHA)
		m := protectedMergeCase{protectedMergeFixture: p, original: original, initial: opened}
		m.assertOpen(t, phase, opened)
		contents, _ := m.readCommit(t, opened.headBranch, opened.headSHA)
		require.Equal(t, mergeFixtureAfter, contents)
		assertRequestNeverScheduled(t, m, phase)
	})
}

func assertRequestNeverScheduled(t *testing.T, m protectedMergeCase, phase string) {
	t.Helper()
	m.assertScheduled(t, phase, false)
	require.Zero(t, m.latestStatus(t, m.initial.headSHA, requiredMergeCheck).id, "the exact head must start without the required check")
	m.publishStatus(t, m.initial.headSHA, requiredMergeCheck, "success")
	const passed = "after the required check succeeds on the exact PR head"
	m.assertUnmergedFor(t, passed, 2*time.Second)
	m.assertScheduled(t, passed, false)
}
