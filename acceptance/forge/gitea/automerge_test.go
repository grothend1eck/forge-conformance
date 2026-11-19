//go:build acceptance

package gitea_test

import (
	"crypto/rand"
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func (s *server) testProtectedMerge(t *testing.T) {
	t.Run("observably_scheduled", func(t *testing.T) {
		r, opened := applyProtectedRequest(t, s, "schedule-observer", "auto", false)
		r.assertScheduledBy(t, "before status success", opened.number, s.owner)
		require.Equal(t, opened, r.mergeView(t, publishPR(t, r, opened.number)))
	})
	t.Run("merges_after_required_status", func(t *testing.T) {
		r, opened := applyProtectedRequest(t, s, "protected-auto", "auto", false)
		head, base := opened.headSHA, opened.baseTip
		pending := r.publishStatus(t, head, requiredMergeCheck, gitea.StatusPending)
		// Neither a success on the wrong SHA nor one for the wrong context
		// can fulfill the required status on the PR head.
		r.publishStatus(t, base, requiredMergeCheck, gitea.StatusSuccess)
		r.publishStatus(t, head, "acceptance/unrelated", gitea.StatusSuccess)
		r.assertUnmergedFor(t, "wrong SHA and wrong context have passed", opened, time.Second)
		require.Equal(t, pending, r.latestStatus(t, head, requiredMergeCheck))

		// After the initial apply, only the independent status actor writes:
		// no second apply, direct merge, or protection change.
		r.publishStatus(t, head, requiredMergeCheck, gitea.StatusSuccess)
		tip := r.awaitMerged(t, opened, mergeFixtureAfter, gitea.MergeStyleMerge)
		require.Equal(t, []string{base, head}, r.commitParents(t, tip))
	})
	t.Run("edited_request_with_reviewer", func(t *testing.T) {
		const id, title, reviewer = "edited-auto", "Enable automatic merge", "edited-auto-reviewer"
		r, opened := applyProtectedRequest(t, s, id, "manual", false)
		r.assertScheduledBy(t, "before enabling automerge", opened.number)
		s.createUser(t, reviewer, rand.Text())
		r.grantPRWrite(t, reviewer)

		spec := mergeSpec(title, "auto", "squash")
		spec["reviewers"] = []string{reviewer}
		newCLI(t, s).PipelineOK(t, "apply", requestManifest(t, r, id, "v1.2.0", spec))
		edited := r.assertEdited(t, opened, title, "v1.2.0", false)
		r.assertReviewBeforeScheduling(t, edited.number, reviewer, title)
		r.publishStatus(t, edited.headSHA, requiredMergeCheck, gitea.StatusSuccess)
		tip := r.awaitMerged(t, edited, "version: v1.2.0\nkeep: untouched\n", gitea.MergeStyleSquash)
		require.Equal(t, []string{edited.baseTip}, r.commitParents(t, tip))
	})
	t.Run("repository_merge_method", func(t *testing.T) {
		const id, title = "protected-repo-default", "Acceptance: protected-repo-default"
		r, before := newProtectedMergeRepo(t, s)
		r.configureDefaultSquash(t, false)
		// Omitting mergemethod must use the forge's default.
		newCLI(t, s).PipelineOK(t, "apply", requestManifest(t, r, id, "v1.1.0", mergeSpec(title, "auto", "")))
		opened := r.assertInitialMergeRequest(t, before, id, title, false)
		r.publishStatus(t, opened.headSHA, requiredMergeCheck, gitea.StatusSuccess)
		tip := r.awaitMerged(t, opened, mergeFixtureAfter, gitea.MergeStyleSquash)
		require.Equal(t, []string{opened.baseTip}, r.commitParents(t, tip))
	})
	t.Run("existing_schedule_unchanged", func(t *testing.T) {
		const id = "keep-existing-schedule"
		r, before := newProtectedMergeRepo(t, s)
		r.configureDefaultSquash(t, false)
		c := newCLI(t, s)
		apply := func(version, strategy, method string) {
			t.Helper()
			c.PipelineOK(t, "apply", requestManifest(t, r, id, version, mergeSpec(version, strategy, method)))
		}
		apply("v1.1.0", "AuTo", "MeRgE")
		opened := r.assertInitialMergeRequest(t, before, id, "v1.1.0", false)
		r.assertScheduledBy(t, "after scheduling explicit merge over default squash", opened.number, s.owner)

		for _, edit := range []struct{ version, strategy string }{{"v1.2.0", "auto"}, {"v1.3.0", "manual"}} {
			apply(edit.version, edit.strategy, "squash")
			opened = r.assertEdited(t, opened, edit.version, edit.version, false)
			r.assertScheduledBy(t, "after editing an already scheduled request to "+edit.version, opened.number, s.owner)
		}

		// The original explicit merge policy must still govern the latest head.
		r.publishStatus(t, opened.headSHA, requiredMergeCheck, gitea.StatusSuccess)
		tip := r.awaitMerged(t, opened, "version: v1.3.0\nkeep: untouched\n", gitea.MergeStyleMerge)
		require.Equal(t, []string{opened.baseTip, opened.headSHA}, r.commitParents(t, tip))
	})
	t.Run("explicit_fast_forward", func(t *testing.T) {
		const id, title = "explicit-ff-only", "Fast-forward after the required check"
		r, before := newProtectedMergeRepo(t, s)
		r.configureDefaultSquash(t, true)
		// Leave regular merge and squash enabled: the result must come from
		// the manifest, not the repository's default or only allowed style.
		newCLI(t, s).PipelineOK(t, "apply", requestManifest(t, r, id, "v1.1.0", mergeSpec(title, "auto", "fast-forward-only")))
		opened := r.assertInitialMergeRequest(t, before, id, title, false)
		r.assertScheduledBy(t, "before publishing the required status", opened.number, s.owner)
		r.publishStatus(t, opened.headSHA, requiredMergeCheck, gitea.StatusSuccess)
		tip := r.awaitMerged(t, opened, mergeFixtureAfter, gitea.MergeStyleFastForwardOnly)
		require.Equal(t, []string{opened.baseTip}, r.commitParents(t, tip))
	})
	testExplicitMergeStyles(t, s)
}

func (s *server) testManualMerge(t *testing.T) {
	r, opened := applyProtectedRequest(t, s, "protected-manual", "manual", false)
	r.assertNoAutomerge(t, opened, "manual request")
}

func (s *server) testDraft(t *testing.T) {
	r, opened := applyProtectedRequest(t, s, "protected-draft", "auto", true)
	r.assertNoAutomerge(t, opened, "draft request")
}

func (s *server) testDraftSurvivesEdit(t *testing.T) {
	const id = "draft-survives-edit"
	r, opened := applyProtectedRequest(t, s, id, "manual", false)
	c := newCLI(t, s)
	const title = "Updated version with automerge requested"
	for _, edit := range []struct{ marker, version, requested string }{
		{"WIP:", "v1.2.0", title},
		{"wip:", "v1.3.0", title},
		{"[WIP]", "v1.4.0", title},
		{"WIP:", "v1.5.0", "WIP: " + title},
	} {
		// Mark the existing PR independently, as a maintainer would.
		_, resp, err := s.admin.PullRequests.EditPullRequest(t.Context(), s.owner, r.name, opened.number,
			gitea.EditPullRequestOption{Title: edit.marker + " Hold for a human review"})
		s.check(t, "mark the existing PR as a draft", resp, err)
		require.True(t, publishPR(t, r, opened.number).Draft)

		spec := mergeSpec(edit.requested, "auto", "")
		config := requestConfig(r, id, edit.version, spec)
		if edit.version == "v1.2.0" {
			// Check that the generated title goes through the same draft handling.
			delete(spec, "title")
			config["name"] = edit.requested
		}
		c.PipelineOK(t, "apply", cli.Manifest(t, config))
		opened = r.assertEdited(t, opened, edit.marker+" "+title, edit.version, true)
		r.assertScheduledBy(t, "after editing a draft with marker "+edit.marker, opened.number)
	}
	r.assertNoAutomerge(t, opened, "human draft after editing")
}

func applyProtectedRequest(t *testing.T, s *server, id, strategy string, draft bool) (*repository, mergeView) {
	t.Helper()
	r, before := newProtectedMergeRepo(t, s)
	title := "Acceptance: " + id
	if draft {
		// Both pinned releases derive the draft flag from a WIP title.
		title = "WIP: " + title
	}
	newCLI(t, s).PipelineOK(t, "apply", requestManifest(t, r, id, "v1.1.0", mergeSpec(title, strategy, "merge")))
	return r, r.assertInitialMergeRequest(t, before, id, title, draft)
}

func (r *repository) assertNoAutomerge(t *testing.T, opened mergeView, reason string) {
	t.Helper()
	r.assertScheduledBy(t, reason+" before status success", opened.number)
	require.Zero(t, r.latestStatus(t, opened.headSHA, requiredMergeCheck).id, "the exact head must start without the required check")
	r.publishStatus(t, opened.headSHA, requiredMergeCheck, gitea.StatusSuccess)
	phase := reason + " after status success"
	r.assertUnmergedFor(t, phase, opened, 2*time.Second)
	r.assertScheduledBy(t, phase, opened.number)
}

func (r *repository) configureDefaultSquash(t *testing.T, allowFastForward bool) {
	t.Helper()
	repo := r.editMergeSettings(t, gitea.EditRepoOption{
		DefaultMergeStyle:         new(gitea.MergeStyleSquash),
		AllowMerge:                new(true),
		AllowSquash:               new(true),
		AllowRebase:               new(false),
		AllowRebaseMerge:          new(false),
		AllowFastForwardOnlyMerge: new(allowFastForward),
	})
	require.Equal(t, gitea.MergeStyleSquash, repo.DefaultMergeStyle)
	require.True(t, repo.AllowMerge && repo.AllowSquash, "both merge and squash must be available")
	require.False(t, repo.AllowRebase || repo.AllowRebaseMerge)
	require.Equal(t, allowFastForward, repo.AllowFastForwardOnlyMerge)
}

func (r *repository) editMergeSettings(t *testing.T, settings gitea.EditRepoOption) *gitea.Repository {
	t.Helper()
	s := r.server
	_, resp, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, settings)
	s.check(t, "configure allowed merge methods before apply", resp, err)
	return publishRepo(t, r)
}

func testExplicitMergeStyles(t *testing.T, s *server) {
	for _, tc := range []struct {
		style  gitea.MergeStyle
		landed int
	}{
		{gitea.MergeStyleSquash, 1},
		{gitea.MergeStyleRebase, 2},
		{gitea.MergeStyleRebaseMerge, 3},
	} {
		t.Run("explicit_"+string(tc.style), func(t *testing.T) {
			r := s.newRepo(t)
			id, title := "style-"+string(tc.style), "Merge with "+string(tc.style)
			head := "updatecli_" + r.Base + "_" + id
			const (
				baseFile = "base-only.txt"
				baseText = "Independent base change\n"
				headFile = "head-only.txt"
				headText = "First change on the PR\n"
			)

			// Create diverging histories; Updatecli adds the second PR commit.
			fork := r.Refs(t)["refs/heads/"+r.Base]
			r.Stage(t, head, headFile, headText)
			r.Stage(t, r.Base, baseFile, baseText)
			refs := r.awaitBranches(t, head, r.Base)
			originalFirst, base := refs["refs/heads/"+head], refs["refs/heads/"+r.Base]
			require.NotEqual(t, originalFirst, base)
			require.Equal(t, []string{fork}, r.commitParents(t, originalFirst))
			require.Equal(t, []string{fork}, r.commitParents(t, base))
			r.protectBase(t)

			// A different default and multiple allowed methods expose ignored configuration.
			repo := r.editMergeSettings(t, gitea.EditRepoOption{
				DefaultMergeStyle: new(gitea.MergeStyleMerge),
				AllowMerge:        new(true),
				AllowSquash:       new(true),
				AllowRebase:       new(true),
				AllowRebaseMerge:  new(true),
			})
			require.Equal(t, gitea.MergeStyleMerge, repo.DefaultMergeStyle)
			require.True(t, repo.AllowMerge && repo.AllowSquash && repo.AllowRebase && repo.AllowRebaseMerge)
			config := requestConfig(r, id, "v1.1.0", mergeSpec(title, "auto", string(tc.style)))
			// Do not let SCM checkout reset the deliberately diverged head.
			config["scms"].(map[string]any)["origin"].(map[string]any)["spec"].(map[string]any)["force"] = false
			newCLI(t, s).PipelineOK(t, "apply", cli.Manifest(t, config))
			opened := r.assertInitialMergeRequest(t, refs, id, title, false)
			r.assertScheduledBy(t, "before passing the required check", opened.number, s.owner)
			r.publishStatus(t, opened.headSHA, requiredMergeCheck, gitea.StatusSuccess)

			tip := r.awaitMerged(t, opened, mergeFixtureAfter, tc.style)
			require.Equal(t, headText, r.Git(t, "show", tip+":"+headFile))
			require.Equal(t, baseText, r.Git(t, "show", tip+":"+baseFile))
			landed := strings.Fields(r.Git(t, "rev-list", "--topo-order", "--reverse", base+".."+tip))
			require.Len(t, landed, tc.landed)
			if tc.style == gitea.MergeStyleSquash {
				require.Equal(t, []string{base}, r.commitParents(t, tip))
				return
			}
			first, second := landed[0], landed[1]
			require.NotEqual(t, originalFirst, first)
			require.NotEqual(t, opened.headSHA, second)
			require.Equal(t, []string{base}, r.commitParents(t, first))
			require.Equal(t, []string{first}, r.commitParents(t, second))
			require.Equal(t, headFile, strings.TrimSpace(r.Git(t, "diff", "--name-only", base, first)))
			require.Equal(t, "version.yaml", strings.TrimSpace(r.Git(t, "diff", "--name-only", first, second)))
			if tc.style == gitea.MergeStyleRebaseMerge {
				require.Equal(t, []string{base, second}, r.commitParents(t, tip), "merge commit must join the base and rewritten head")
				require.Empty(t, r.Git(t, "diff", second, tip), "the merge commit must retain the rebased tree")
			} else {
				require.Equal(t, second, tip, "plain rebase must land the rewritten head without a merge commit")
			}
		})
	}
}
