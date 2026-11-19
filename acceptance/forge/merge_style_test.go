//go:build acceptance

package forge

import (
	"strings"
	"testing"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testExplicitMergeStyles(t *testing.T, s *server) {
	for _, tc := range []struct {
		style  gitea.MergeStyle
		landed int
	}{
		{gitea.MergeStyleSquash, 1},
		{gitea.MergeStyleRebase, 2},
		{gitea.MergeStyleRebaseMerge, 3},
	} {
		runRequired(t, "explicit_"+string(tc.style), func(t *testing.T) {
			r := s.newRepo(t)
			id := "style-" + string(tc.style)
			head := "updatecli_" + r.base + "_" + id
			title := "Merge with " + string(tc.style)
			const (
				baseFile = "base-only.txt"
				baseText = "Independent base change\n"
				headFile = "head-only.txt"
				headText = "First change on the PR\n"
			)

			// The first PR commit and an independent base commit fork from
			// the same point. Updatecli adds the second PR commit.
			fork := r.refs(t)["refs/heads/"+r.base]
			r.stage(t, head, headFile, headText)
			r.stage(t, r.base, baseFile, baseText)
			refs := r.awaitBranches(t, head, r.base)
			originalFirst, base := refs["refs/heads/"+head], refs["refs/heads/"+r.base]
			require.NotEqual(t, originalFirst, base)
			require.Equal(t, fork, strings.TrimSpace(r.git(t, "rev-parse", originalFirst+"^")))
			require.Equal(t, fork, strings.TrimSpace(r.git(t, "rev-parse", base+"^")))
			r.protectBase(t)

			// Leave other methods available and choose a different default so
			// silently dropping the explicit method cannot pass.
			_, resp, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, gitea.EditRepoOption{
				DefaultMergeStyle: new(gitea.MergeStyleMerge),
				AllowMerge:        new(true),
				AllowSquash:       new(true),
				AllowRebase:       new(true),
				AllowRebaseMerge:  new(true),
			})
			if err != nil {
				s.apiFailure(t, "enable explicit merge styles", resp, err)
			}
			repo := publishRepo(t, r)
			require.Equal(t, gitea.MergeStyleMerge, repo.DefaultMergeStyle)
			require.True(t, repo.AllowMerge && repo.AllowSquash && repo.AllowRebase && repo.AllowRebaseMerge,
				"the pinned server must allow all four methods")

			config := requestConfig(r, id, "v1.1.0", map[string]any{
				"title": title, "merge": map[string]any{"strategy": "auto"}, "mergemethod": tc.style,
			})
			// Do not let SCM checkout reset the deliberately diverged head.
			config["scms"].(map[string]any)["origin"].(map[string]any)["spec"].(map[string]any)["force"] = false
			newCLI(t, s).ok(t, "apply", manifestYAML(t, config))
			pr := onlyOpenPullRequest(t, r)
			opened := r.readMergeSnapshot(t, "before passing the required check", pr.Index)
			assertOpen(t, "before passing the required check", opened, mergeSnapshot{
				id: pr.ID, number: pr.Index, title: title, author: s.owner,
				baseBranch: r.base, baseTip: base, defaultTip: refs["refs/heads/main"],
				headBranch: head, headSHA: opened.headSHA, headTip: opened.headSHA, open: true,
			})
			version, parents := r.readCommit(t, head, opened.headSHA)
			require.Equal(t, []string{originalFirst}, parents, "the PR must still have two commits diverging from the base")
			require.Equal(t, mergeFixtureAfter, version)
			r.assertScheduledBy(t, "before passing the required check", opened.number, s.owner)
			r.publishStatus(t, opened.headSHA, requiredMergeCheck, "success")

			const phase = "after the required check passes"
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				merged, _, err := s.client.PullRequests.GetPullRequest(t.Context(), s.owner, r.name, opened.number)
				require.True(collect, err == nil, "read the original PR after its required check")
				require.NotNil(collect, merged)
				require.True(collect, merged.HasMerged)
				require.Equal(collect, gitea.StateClosed, merged.State)
			}, mergeWaitTimeout, mergePollInterval, "the forge must merge the scheduled PR")
			merged := r.readMergeSnapshot(t, phase, opened.number)
			require.Equal(t, opened.id, merged.id)
			require.Equal(t, opened.baseBranch, merged.baseBranch)
			require.Equal(t, opened.defaultTip, merged.defaultTip)
			require.Equal(t, s.owner, merged.mergedBy)
			require.NotEqual(t, base, merged.baseTip)
			require.Equal(t, merged.baseTip, merged.mergedSHA)
			version, parents = r.readCommit(t, r.base, merged.baseTip)
			require.Equal(t, mergeFixtureAfter, version)
			require.Equal(t, headText, r.git(t, "show", merged.baseTip+":"+headFile))
			require.Equal(t, baseText, r.git(t, "show", merged.baseTip+":"+baseFile))

			landed := strings.Fields(r.git(t, "rev-list", "--topo-order", "--reverse", base+".."+merged.baseTip))
			require.Len(t, landed, tc.landed)
			if tc.style == gitea.MergeStyleSquash {
				require.Equal(t, []string{base}, parents)
				return
			}
			first, second := landed[0], landed[1]
			require.NotEqual(t, originalFirst, first)
			require.NotEqual(t, opened.headSHA, second)
			require.Equal(t, []string{base}, styleParents(t, r, first))
			require.Equal(t, []string{first}, styleParents(t, r, second))
			require.Equal(t, headFile, strings.TrimSpace(r.git(t, "diff", "--name-only", base, first)))
			require.Equal(t, "version.yaml", strings.TrimSpace(r.git(t, "diff", "--name-only", first, second)))
			if tc.style == gitea.MergeStyleRebaseMerge {
				require.Equal(t, []string{base, second}, parents, "merge commit must join the base and rewritten head")
				require.Empty(t, r.git(t, "diff", second, merged.baseTip), "the merge commit must retain the rebased tree")
			} else {
				require.Equal(t, second, merged.baseTip, "plain rebase must land the rewritten head without a merge commit")
			}
		})
	}
}

func styleParents(t *testing.T, r *repository, sha string) []string {
	t.Helper()
	return strings.Fields(r.git(t, "show", "-s", "--format=%P", sha))
}
