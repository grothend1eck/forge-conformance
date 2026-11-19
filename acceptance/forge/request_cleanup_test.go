//go:build acceptance

package forge

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testClean(t *testing.T, s *server) {
	r := s.newRepo(t)
	c := newCLI(t, s)
	const (
		id     = "clean"
		value  = "v1.1.0"
		reason = "Pull request closed as no changed file detected"
	)
	manifest := requestManifest(t, r, id, value, nil)
	c.ok(t, "apply", manifest)
	prs := publishPRs(t, r)
	require.Equal(t, 1, len(prs), "initial apply must open exactly one PR")
	index := prs[0].Index
	headRef := "refs/heads/updatecli_" + r.base + "_" + id
	refsWithChanges := r.refs(t)
	require.NotEmpty(t, refsWithChanges[headRef])

	c.ok(t, "apply", manifest, "--clean-git-branches")
	require.Equal(t, gitea.StateOpen, publishPR(t, r, index).State, "never close a nonempty PR")
	require.Equal(t, refsWithChanges, r.refs(t), "never prune the branch of a nonempty PR")

	// Simulate an update landed independently and the managed branch being reset to that base.
	// Merging the original PR would let the forge close it instead of exercising Updatecli.
	r.stage(t, r.base, "version.yaml", "version: "+value+"\nkeep: untouched\n")
	base := r.refs(t)["refs/heads/"+r.base]
	require.NotEmpty(t, base)
	r.git(t, "push", "--force", "origin", base+":"+headRef)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		// The forge refreshes its PR ref asynchronously; wait for both API views to catch up.
		list, _, err := s.client.PullRequests.ListRepoPullRequests(t.Context(), s.owner, r.name, gitea.ListPullRequestsOptions{State: gitea.StateOpen})
		require.True(collect, err == nil, "list open PRs while waiting for the reset working branch")
		require.Equal(collect, 1, len(list), "the forge must leave the PR open for Updatecli to close")
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

	obsolete := r.refs(t)
	require.Equal(t, base, obsolete[headRef])
	c.ok(t, "diff", manifest)
	require.Equal(t, gitea.StateOpen, publishPR(t, r, index).State, "diff must not clean an obsolete PR")
	require.Equal(t, obsolete, r.refs(t))

	c.ok(t, "apply", manifest, "--clean-git-branches")
	closed := publishPR(t, r, index)
	require.Equal(t, gitea.StateClosed, closed.State)
	require.False(t, closed.HasMerged)
	require.Equal(t, 1, len(publishPRs(t, r)), "cleanup must not create another PR")
	comments, resp, err := s.client.Issues.ListIssueComments(t.Context(), s.owner, r.name, index, gitea.ListIssueCommentOptions{})
	if err != nil {
		s.apiFailure(t, "list PR cleanup comments", resp, err)
	}
	require.True(t, slices.ContainsFunc(comments, func(comment *gitea.Comment) bool {
		return comment != nil && comment.Body == reason
	}), "expected cleanup comment %q to attribute closure to Updatecli, not the branch deletion hook", reason)
	delete(obsolete, headRef)
	require.Equal(t, obsolete, r.refs(t), "only the obsolete working ref should disappear")
}

func testFork(t *testing.T, s *server) {
	r := s.newRepo(t)
	c := newCLI(t, s)
	upstream := publishRepo(t, r)
	require.True(t, upstream.HasPullRequests)
	const (
		id        = "fork"
		forkTitle = "Independent fork change"
		forkBody  = "This PR is not managed by Updatecli."
	)
	head := "updatecli_" + r.base + "_" + id
	before := r.refs(t)

	// Seed a real fork and its PR through the native API using a different account.
	// The upstream gitea SCM writes its own repo, not a cross-repository PR head.
	forker := s.admin
	account, resp, err := forker.Users.GetMyUserInfo(t.Context())
	if err != nil {
		s.apiFailure(t, "identify fixture fork owner", resp, err)
	}
	require.NotEqual(t, s.owner, account.UserName)
	write := gitea.AccessModeWrite
	resp, err = s.client.Repositories.AddCollaborator(t.Context(), s.owner, r.name, account.UserName, gitea.AddCollaboratorOption{Permission: &write})
	if err != nil {
		s.apiFailure(t, "grant fixture fork owner repository access", resp, err)
	}
	fork, resp, err := forker.Repositories.CreateFork(t.Context(), s.owner, r.name, gitea.CreateForkOption{})
	if err != nil {
		s.apiFailure(t, "create fixture fork", resp, err)
	}
	require.True(t, fork.Fork)
	require.NotNil(t, fork.Owner)
	require.NotEqual(t, s.owner, fork.Owner.UserName)
	require.NotEqual(t, upstream.ID, fork.ID)
	change, resp, err := forker.Repositories.CreateFile(t.Context(), fork.Owner.UserName, fork.Name, "fork-only.txt", gitea.CreateFileOptions{
		FileOptions: gitea.FileOptions{BranchName: r.base, NewBranchName: head},
		Content:     base64.StdEncoding.EncodeToString([]byte("keep the independent change\n")),
	})
	if err != nil {
		s.apiFailure(t, "seed independent fork branch", resp, err)
	}
	require.NotNil(t, change.Commit)
	foreign, resp, err := forker.PullRequests.CreatePullRequest(t.Context(), s.owner, r.name, gitea.CreatePullRequestOption{
		Head: fork.Owner.UserName + ":" + head, Base: r.base, Title: forkTitle, Body: forkBody,
	})
	if err != nil {
		s.apiFailure(t, "open independent fork PR", resp, err)
	}
	foreign = publishPR(t, r, foreign.Index)
	require.NotNil(t, foreign.Head)
	require.NotNil(t, foreign.Base)
	// These are the branch labels that Updatecli compares before the repo IDs.
	require.Equal(t, head, foreign.Head.Name)
	require.Equal(t, r.base, foreign.Base.Name)
	require.Equal(t, fork.ID, foreign.Head.RepoID)
	require.Equal(t, upstream.ID, foreign.Base.RepoID)
	require.Equal(t, change.Commit.SHA, foreign.Head.Sha)

	assertForeignUntouched := func() {
		t.Helper()
		pr := publishPR(t, r, foreign.Index)
		require.Equal(t, foreign.ID, pr.ID)
		require.Equal(t, forkTitle, pr.Title)
		require.Equal(t, forkBody, pr.Body)
		require.Equal(t, gitea.StateOpen, pr.State)
		require.NotNil(t, pr.Head)
		require.Equal(t, fork.ID, pr.Head.RepoID)
		require.Equal(t, head, pr.Head.Ref)
		require.Equal(t, change.Commit.SHA, pr.Head.Sha)
	}

	// With no update to publish, neither lookup nor cleanup should claim the fork PR.
	require.NotEmpty(t, foreign.HTMLURL)
	output := c.ok(t, "apply", requestManifest(t, r, id, "v1.0.0", nil), "--clean-git-branches")
	require.False(t, strings.Contains(output, foreign.HTMLURL), "the CLI must not report the independent fork PR")
	require.Equal(t, 1, len(publishPRs(t, r)), "a no-op must leave only the fork PR")
	require.Equal(t, before, r.refs(t))
	assertForeignUntouched()

	// An otherwise matching fork PR must not be reused, edited, or closed.
	const (
		managedTitle   = "Managed update on the upstream repository"
		managedVersion = "v1.1.0"
	)
	c.ok(t, "apply", requestManifest(t, r, id, managedVersion, map[string]any{"title": managedTitle}), "--clean-git-branches")
	prs := publishPRs(t, r)
	require.Equal(t, 2, len(prs), "the managed PR must be separate from the fork PR")
	var managed *gitea.PullRequest
	for _, pr := range prs {
		if pr.Index != foreign.Index {
			managed = publishPR(t, r, pr.Index)
		}
	}
	require.NotNil(t, managed)
	require.Equal(t, managedTitle, managed.Title)
	assertPublicationHead(t, r.gitRepository, giteaRequest(t, managed), upstream.ID, head, before, managedVersion)
	assertForeignUntouched()
}
