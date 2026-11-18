//go:build acceptance

package forge

import (
	"crypto/rand"
	"encoding/base64"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPublish(t *testing.T, s *server) {
	r := s.newRepo(t)
	c := newCLI(t, s)
	const (
		id           = "publish"
		assignee     = "publish-assignee"
		reviewer     = "publish-reviewer"
		nextReviewer = "publish-next-reviewer"
	)
	repo := publishRepo(t, r)
	require.True(t, repo.Private)
	require.Equal(t, "main", repo.DefaultBranch)
	require.Equal(t, s.owner+"/"+r.name, repo.FullName)

	// Give the selected branch its own tree, and seed refs the pipeline must not touch.
	r.stage(t, r.base, "stable-only.txt", "not part of the update\n")
	baseRef := "refs/heads/" + r.base
	base := r.refs(t)[baseRef]
	require.NotEmpty(t, base)
	r.git(t, "push", "origin", base+":refs/heads/unrelated", base+":refs/tags/publish-anchor")
	require.NotEqual(t, r.refs(t)["refs/heads/main"], base)

	reviewerPassword := rand.Text()
	s.createUser(t, reviewer, reviewerPassword)
	s.createUser(t, assignee, rand.Text())
	s.createUser(t, nextReviewer, rand.Text())
	for _, user := range []string{assignee, reviewer, nextReviewer} {
		write := gitea.AccessModeWrite
		resp, err := s.client.Repositories.AddCollaborator(t.Context(), s.owner, r.name, user, gitea.AddCollaboratorOption{Permission: &write})
		if err != nil {
			s.apiFailure(t, "grant repository access to "+user, resp, err)
		}
	}
	const (
		firstVersion = "v1.1.0"
		firstTitle   = "Bump the fixture to v1.1.0"
		firstBody    = "Acceptance metadata for v1.1.0.\n\n- First update"
	)
	first := requestManifest(t, r, id, firstVersion, map[string]any{
		"title": firstTitle, "body": firstBody,
		"assignees": []string{assignee}, "reviewers": []string{reviewer},
	})
	head := "updatecli_" + r.base + "_" + id
	before := r.refs(t)
	require.NotEmpty(t, before[baseRef])
	require.NotContains(t, before, "refs/heads/"+head)

	// Diff previews the update without publishing anything.
	output := c.ok(t, "diff", first)
	requireOutput(t, output, "version.yaml", firstVersion)
	require.Equal(t, before, r.refs(t), "diff must not write refs")
	require.Zero(t, len(publishPRs(t, r)), "diff must not create even a closed PR")

	// First apply opens one PR and commits on the configured base, not main.
	c.ok(t, "apply", first)
	prs := publishPRs(t, r)
	require.Equal(t, 1, len(prs), "first apply must open exactly one PR")
	opened := publishPR(t, r, prs[0].Index)
	require.Positive(t, opened.ID)
	require.Positive(t, opened.Index)
	assertPublishedHead(t, r, opened, repo.ID, head, before, firstVersion)
	require.Equal(t, before[baseRef], strings.TrimSpace(r.git(t, "rev-parse", opened.Head.Sha+"^")),
		"first commit must start at the configured base")
	assertPRMetadata(t, opened, firstTitle, firstBody, assignee)
	firstReviews := publishReviews(t, r, opened.Index)
	require.Equal(t, 1, len(firstReviews))
	require.NotNil(t, firstReviews[reviewer], "missing initial review request for %s", reviewer)
	require.Equal(t, gitea.ReviewStateRequestReview, firstReviews[reviewer].State)

	// Identical input preserves the PR, the review request, and every ref.
	afterOpening := r.refs(t)
	c.ok(t, "apply", first)
	require.Equal(t, afterOpening, r.refs(t), "an identical apply must not add a commit or move any ref")
	prs = publishPRs(t, r)
	require.Equal(t, 1, len(prs), "an identical apply must not create or remove a PR")
	unchanged := publishPR(t, r, prs[0].Index)
	require.Equal(t, opened.ID, unchanged.ID)
	require.Equal(t, opened.Index, unchanged.Index)
	require.Equal(t, gitea.StateOpen, unchanged.State)
	require.NotNil(t, unchanged.Head)
	require.Equal(t, opened.Head.RepoID, unchanged.Head.RepoID)
	require.Equal(t, opened.Head.Ref, unchanged.Head.Ref)
	require.Equal(t, opened.Head.Sha, unchanged.Head.Sha)
	require.NotNil(t, unchanged.Base)
	require.Equal(t, opened.Base.RepoID, unchanged.Base.RepoID)
	require.Equal(t, opened.Base.Ref, unchanged.Base.Ref)
	require.Equal(t, opened.Base.Sha, unchanged.Base.Sha)
	assertPRMetadata(t, unchanged, firstTitle, firstBody, assignee)
	unchangedReviews := publishReviews(t, r, unchanged.Index)
	require.Equal(t, 1, len(unchangedReviews))
	require.NotNil(t, unchangedReviews[reviewer], "identical apply must keep the review request for %s", reviewer)
	require.Equal(t, gitea.ReviewStateRequestReview, unchangedReviews[reviewer].State)
	require.Equal(t, firstReviews[reviewer].ID, unchangedReviews[reviewer].ID)

	// Approve the published commit as the requested collaborator.
	asReviewer := s.sdkClient(t, gitea.SetToken(s.accessToken(t, reviewer, reviewerPassword)))
	approval, resp, err := asReviewer.PullRequests.CreatePullReview(t.Context(), s.owner, r.name, unchanged.Index, gitea.CreatePullReviewOptions{
		State: gitea.ReviewStateApproved, CommitID: unchanged.Head.Sha, Body: "Approved the first version",
	})
	if err != nil {
		s.apiFailure(t, "approve the original published commit", resp, err)
	}
	require.NotNil(t, approval)
	require.NotNil(t, approval.Reviewer)
	require.Equal(t, reviewer, approval.Reviewer.UserName)
	require.Equal(t, gitea.ReviewStateApproved, approval.State)

	// Changed input edits the original PR and requests only the new reviewer.
	const (
		nextVersion = "v1.2.0"
		nextTitle   = "Bump the fixture to v1.2.0"
		nextBody    = "Acceptance metadata for v1.2.0.\n\n- Follow-up update"
	)
	c.ok(t, "apply", requestManifest(t, r, id, nextVersion, map[string]any{
		"title": nextTitle, "body": nextBody, "reviewers": []string{reviewer, nextReviewer},
	}))
	prs = publishPRs(t, r)
	require.Equal(t, 1, len(prs), "changing input must update the existing PR, not replace it")
	updated := publishPR(t, r, prs[0].Index)
	require.Equal(t, opened.ID, updated.ID)
	require.Equal(t, opened.Index, updated.Index)
	assertPublishedHead(t, r, updated, repo.ID, head, before, nextVersion)
	require.NotEqual(t, opened.Head.Sha, updated.Head.Sha)
	assertPRMetadata(t, updated, nextTitle, nextBody, assignee)
	updatedReviews := publishReviews(t, r, updated.Index)
	require.Equal(t, 2, len(updatedReviews))
	require.NotNil(t, updatedReviews[reviewer], "missing completed review for %s", reviewer)
	require.Equal(t, approval.ID, updatedReviews[reviewer].ID, "completed approval must not be replaced")
	require.Equal(t, gitea.ReviewStateApproved, updatedReviews[reviewer].State, "original reviewer must not be re-requested")
	require.False(t, updatedReviews[reviewer].Dismissed)
	require.NotNil(t, updatedReviews[nextReviewer], "missing new review request for %s", nextReviewer)
	require.Equal(t, gitea.ReviewStateRequestReview, updatedReviews[nextReviewer].State)
}

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
	assertPublishedHead(t, r, managed, upstream.ID, head, before, managedVersion)
	assertForeignUntouched()
}

func publishRepo(t *testing.T, r *repository) *gitea.Repository {
	t.Helper()
	repo, resp, err := r.server.client.Repositories.GetRepo(t.Context(), r.server.owner, r.name)
	if err != nil {
		r.server.apiFailure(t, "get repository metadata", resp, err)
	}
	return repo
}

func publishPRs(t *testing.T, r *repository) []*gitea.PullRequest {
	t.Helper()
	prs, resp, err := r.server.client.PullRequests.ListRepoPullRequests(t.Context(), r.server.owner, r.name, gitea.ListPullRequestsOptions{State: gitea.StateAll})
	if err != nil {
		r.server.apiFailure(t, "list all repository PRs", resp, err)
	}
	return prs
}

func publishPR(t *testing.T, r *repository, index int64) *gitea.PullRequest {
	t.Helper()
	pr, resp, err := r.server.client.PullRequests.GetPullRequest(t.Context(), r.server.owner, r.name, index)
	if err != nil {
		r.server.apiFailure(t, "get repository PR", resp, err)
	}
	require.NotNil(t, pr)
	return pr
}

func assertPRMetadata(t *testing.T, pr *gitea.PullRequest, title, body, assignee string) {
	t.Helper()
	require.Equal(t, title, pr.Title)
	require.Equal(t, body, pr.Body)
	require.Equal(t, 1, len(pr.Assignees), "the PR must keep exactly one assignee")
	require.NotNil(t, pr.Assignees[0])
	require.Equal(t, assignee, pr.Assignees[0].UserName)
}

func assertPublishedHead(t *testing.T, r *repository, pr *gitea.PullRequest, repoID int64, head string, before map[string]string, version string) {
	t.Helper()
	require.Equal(t, gitea.StateOpen, pr.State, "the PR must be open")
	require.NotNil(t, pr.Head)
	require.NotNil(t, pr.Base)
	require.Equal(t, head, pr.Head.Ref)
	require.Equal(t, r.base, pr.Base.Ref)
	require.Equal(t, repoID, pr.Head.RepoID)
	require.Equal(t, repoID, pr.Base.RepoID)

	headRef := "refs/heads/" + head
	base := before["refs/heads/"+r.base]
	require.NotEmpty(t, pr.Head.Sha)
	require.Equal(t, base, pr.Base.Sha)
	wantRefs := maps.Clone(before)
	wantRefs[headRef] = pr.Head.Sha
	require.Equal(t, wantRefs, r.refs(t), "the PR head must match native Git, with all sibling refs untouched")

	r.git(t, "fetch", "origin", headRef)
	require.Equal(t, "version.yaml", strings.TrimSpace(r.git(t, "diff", "--name-only", base, pr.Head.Sha)))
	require.Equal(t, "version: "+version+"\nkeep: untouched\n", r.git(t, "show", pr.Head.Sha+":version.yaml"))
}

func publishReviews(t *testing.T, r *repository, index int64) map[string]*gitea.PullReview {
	t.Helper()
	reviews, resp, err := r.server.client.PullRequests.ListPullReviews(t.Context(), r.server.owner, r.name, index, gitea.ListPullReviewsOptions{})
	if err != nil {
		r.server.apiFailure(t, "list PR review requests", resp, err)
	}
	byReviewer := make(map[string]*gitea.PullReview)
	for _, review := range reviews {
		require.NotNil(t, review)
		require.NotNil(t, review.Reviewer)
		name := review.Reviewer.UserName
		_, duplicate := byReviewer[name]
		require.False(t, duplicate, "unexpected duplicate review or request for %s", name)
		byReviewer[name] = review
	}
	return byReviewer
}
