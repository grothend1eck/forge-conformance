//go:build acceptance

package gitea_test

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/forge"
)

func (s *server) testClean(t *testing.T) {
	r := s.newRepo(t)
	c := newCLI(t, s)
	const (
		id     = "clean"
		value  = "v1.1.0"
		reason = "Pull request closed as no changed file detected"
	)
	manifest := requestManifest(t, r, id, value, nil)
	c.PipelineOK(t, "apply", manifest)
	index := onlyOpenPullRequest(t, r).Index
	headRef := "refs/heads/updatecli_" + r.Base + "_" + id
	refsWithChanges := r.Refs(t)
	require.NotEmpty(t, refsWithChanges[headRef])

	c.PipelineOK(t, "apply", manifest, "--clean-git-branches")
	require.Equal(t, gitea.StateOpen, publishPR(t, r, index).State, "never close a nonempty PR")
	require.Equal(t, refsWithChanges, r.Refs(t), "never prune the branch of a nonempty PR")

	// Simulate an update landed independently and the managed branch being reset to that base.
	// Merging the original PR would let the forge close it instead of exercising Updatecli.
	r.Stage(t, r.Base, "version.yaml", "version: "+value+"\nkeep: untouched\n")
	base := r.Refs(t)["refs/heads/"+r.Base]
	require.NotEmpty(t, base)
	r.Git(t, "push", "--force", "origin", base+":"+headRef)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		// The forge refreshes its PR ref asynchronously; wait for both API views to catch up.
		list, _, err := s.client.PullRequests.ListRepoPullRequests(t.Context(), s.owner, r.name, gitea.ListPullRequestsOptions{State: gitea.StateOpen})
		require.True(collect, err == nil, "list open PRs while waiting for the reset working branch")
		require.Len(collect, list, 1, "the forge must leave the PR open for Updatecli to close")
		require.NotNil(collect, list[0])
		require.Equal(collect, index, list[0].Index)
		require.NotNil(collect, list[0].Head)
		require.Equal(collect, base, list[0].Head.Sha)
		pr, _, err := s.client.PullRequests.GetPullRequest(t.Context(), s.owner, r.name, index)
		require.True(collect, err == nil, "get PR #%d while waiting for its changed-file count to reach zero", index)
		require.NotNil(collect, pr)
		require.NotNil(collect, pr.Head)
		require.Equal(collect, base, pr.Head.Sha)
		require.NotNil(collect, pr.ChangedFiles)
		require.Zero(collect, *pr.ChangedFiles)
	}, 20*time.Second, 200*time.Millisecond)

	obsolete := r.Refs(t)
	require.Equal(t, base, obsolete[headRef])
	c.PipelineOK(t, "diff", manifest)
	require.Equal(t, gitea.StateOpen, publishPR(t, r, index).State, "diff must not clean an obsolete PR")
	require.Equal(t, obsolete, r.Refs(t))

	c.PipelineOK(t, "apply", manifest, "--clean-git-branches")
	closed := publishPR(t, r, index)
	require.Equal(t, gitea.StateClosed, closed.State)
	require.False(t, closed.HasMerged)
	require.Len(t, publishPRs(t, r), 1, "cleanup must not create another PR")
	comments, resp, err := s.client.Issues.ListIssueComments(t.Context(), s.owner, r.name, index, gitea.ListIssueCommentOptions{})
	s.check(t, "list PR cleanup comments", resp, err)
	require.True(t, slices.ContainsFunc(comments, func(comment *gitea.Comment) bool {
		return comment != nil && comment.Body == reason
	}), "expected cleanup comment %q to attribute closure to Updatecli, not the branch deletion hook", reason)
	delete(obsolete, headRef)
	require.Equal(t, obsolete, r.Refs(t), "only the obsolete working ref should disappear")
}

func (s *server) testFork(t *testing.T) {
	r := s.newRepo(t)
	c := newCLI(t, s)
	upstream := publishRepo(t, r)
	require.True(t, upstream.HasPullRequests)
	const (
		id        = "fork"
		forkTitle = "Independent fork change"
		forkBody  = "This PR is not managed by Updatecli."
	)
	head := "updatecli_" + r.Base + "_" + id
	before := r.Refs(t)

	// Seed a real fork and its PR through the native API using a different account.
	// The upstream gitea SCM writes its own repo, not a cross-repository PR head.
	forker := s.admin
	account, resp, err := forker.Users.GetMyUserInfo(t.Context())
	s.check(t, "identify fixture fork owner", resp, err)
	require.NotEqual(t, s.owner, account.UserName)
	r.grantPRWrite(t, account.UserName)
	fork, resp, err := forker.Repositories.CreateFork(t.Context(), s.owner, r.name, gitea.CreateForkOption{})
	s.check(t, "create fixture fork", resp, err)
	require.True(t, fork.Fork)
	require.NotNil(t, fork.Owner)
	require.NotEqual(t, s.owner, fork.Owner.UserName)
	require.NotEqual(t, upstream.ID, fork.ID)
	change, resp, err := forker.Repositories.CreateFile(t.Context(), fork.Owner.UserName, fork.Name, "fork-only.txt", gitea.CreateFileOptions{
		FileOptions: gitea.FileOptions{BranchName: r.Base, NewBranchName: head},
		Content:     base64.StdEncoding.EncodeToString([]byte("keep the independent change\n")),
	})
	s.check(t, "seed independent fork branch", resp, err)
	require.NotNil(t, change.Commit)
	foreign, resp, err := forker.PullRequests.CreatePullRequest(t.Context(), s.owner, r.name, gitea.CreatePullRequestOption{
		Head: fork.Owner.UserName + ":" + head, Base: r.Base, Title: forkTitle, Body: forkBody,
	})
	s.check(t, "open independent fork PR", resp, err)
	foreign = publishPR(t, r, foreign.Index)
	original := giteaRequest(t, foreign)
	// These are the branch labels that Updatecli compares before the repo IDs.
	require.Equal(t, head, foreign.Head.Name)
	require.Equal(t, r.Base, foreign.Base.Name)
	require.Equal(t, fork.ID, original.HeadRepoID)
	require.Equal(t, upstream.ID, original.BaseRepoID)
	require.Equal(t, change.Commit.SHA, original.HeadSHA)
	require.Equal(t, head, original.Head)
	require.Equal(t, forkTitle, original.Title)
	require.True(t, original.Open)

	assertForeignUntouched := func() {
		t.Helper()
		pr := publishPR(t, r, foreign.Index)
		require.Equal(t, original, giteaRequest(t, pr))
		require.True(t, forkBody == pr.Body, "independent PR body must not change")
	}

	// With no update to publish, neither lookup nor cleanup should claim the fork PR.
	require.NotEmpty(t, foreign.HTMLURL)
	output := c.PipelineOK(t, "apply", requestManifest(t, r, id, "v1.0.0", nil), "--clean-git-branches")
	require.False(t, strings.Contains(output, foreign.HTMLURL), "the CLI must not report the independent fork PR")
	require.Len(t, publishPRs(t, r), 1, "a no-op must leave only the fork PR")
	require.Equal(t, before, r.Refs(t))
	assertForeignUntouched()

	// An otherwise matching fork PR must not be reused, edited, or closed.
	const (
		managedTitle   = "Managed update on the upstream repository"
		managedVersion = "v1.1.0"
	)
	c.PipelineOK(t, "apply", requestManifest(t, r, id, managedVersion, map[string]any{"title": managedTitle}), "--clean-git-branches")
	prs := publishPRs(t, r)
	require.Len(t, prs, 2, "the managed PR must be separate from the fork PR")
	var managed *gitea.PullRequest
	for _, pr := range prs {
		require.NotNil(t, pr)
		if pr.Index != foreign.Index {
			require.Nil(t, managed, "expected just one managed PR")
			managed = publishPR(t, r, pr.Index)
		}
	}
	require.NotNil(t, managed)
	require.Equal(t, managedTitle, managed.Title)
	forge.AssertPublicationHead(t, r.GitRepository, giteaRequest(t, managed), upstream.ID, head, before, managedVersion)
	assertForeignUntouched()
}

func testPRBranchOverrides(t *testing.T, s *server) {
	r, patch := s.newRepo(t), s.newRepo(t)
	repoID := publishRepo(t, patch).ID
	const (
		id    = "patch-branches"
		base  = "release/1.x"
		head  = "patch/1.x"
		title = "Patch the supported release"
	)
	patch.Stage(t, base, "support.txt", "supported release\n")
	before := patch.awaitBranches(t, base)
	// Prepare a real patch head based on release/1.x, not the SCM's stable
	// branch. The pipeline still pushes its working branch to the SCM repo.
	patch.Git(t, "push", "origin", before["refs/heads/"+base]+":refs/heads/"+head)
	patch.Stage(t, head, "version.yaml", "version: v1.0.1\nkeep: untouched\n")
	before = patch.awaitBranches(t, head)
	scmBefore := r.Refs(t)
	manifest := requestManifest(t, r, id, "v1.1.0", map[string]any{
		"sourcebranch": head, "targetbranch": base,
		"owner": s.owner, "repository": patch.name, "title": title,
	})
	newCLI(t, s).PipelineOK(t, "apply", manifest)
	require.Empty(t, publishPRs(t, r), "no PR should be opened in the SCM repository")
	pr := onlyOpenPullRequest(t, patch)
	opened := giteaRequest(t, pr)
	require.Equal(t, forge.PublicationRequest{
		ID: pr.ID, Number: pr.Index, Title: title, Open: true,
		HeadRepoID: repoID, Head: head, HeadSHA: before["refs/heads/"+head],
		BaseRepoID: repoID, Base: base, BaseSHA: before["refs/heads/"+base],
	}, opened)
	require.Equal(t, "version.yaml", strings.TrimSpace(patch.Git(t, "diff", "--name-only", opened.BaseSHA, opened.HeadSHA)))
	require.Equal(t, before, patch.Refs(t), "creating the PR must not move either explicit patch ref")

	gotRefs := r.Refs(t)
	workingRef := "refs/heads/updatecli_" + r.Base + "_" + id
	working := gotRefs[workingRef]
	require.NotEmpty(t, working, "the target must push its own working branch")
	require.NotEqual(t, opened.HeadSHA, working)
	r.Git(t, "fetch", "origin", workingRef)
	require.Equal(t, mergeFixtureAfter, r.Git(t, "show", working+":version.yaml"))
	require.Equal(t, []string{scmBefore["refs/heads/"+r.Base]}, r.commitParents(t, working))
	scmBefore[workingRef] = working
	require.Equal(t, scmBefore, gotRefs, "only the SCM working branch should change")
}
