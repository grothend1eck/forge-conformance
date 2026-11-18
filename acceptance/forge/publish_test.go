//go:build acceptance

package forge

import (
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (s *server) newPublication(t *testing.T) publicationFixture {
	t.Helper()
	r := s.newRepo(t)
	repo := publishRepo(t, r)
	require.NotNil(t, repo)
	require.True(t, repo.Private)
	require.Equal(t, "main", repo.DefaultBranch)
	require.Equal(t, s.owner+"/"+r.name, repo.FullName)
	return publicationFixture{repo: r.gitRepository, repoID: repo.ID, driver: giteaPublication{repo: r, cli: newCLI(t, s)}}
}

// Use a separate PR so the common lifecycle never needs provider-specific
// collaborators, review reads or a mutation between its apply calls.
func testPublishReviewMetadata(t *testing.T, s *server) {
	r := s.newRepo(t)
	c := newCLI(t, s)
	const (
		assignee     = "publish-assignee"
		reviewer     = "publish-reviewer"
		nextReviewer = "publish-next-reviewer"
		firstVersion = "v1.1.0"
		firstTitle   = "Bump the fixture to v1.1.0"
		firstBody    = "Acceptance metadata for v1.1.0.\n\n- First update"
		nextVersion  = "v1.2.0"
		nextTitle    = "Bump the fixture to v1.2.0"
		nextBody     = "Acceptance metadata for v1.2.0.\n\n- Follow-up update"
	)
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
	first := requestManifest(t, r, publicationID, firstVersion, map[string]any{
		"title": firstTitle, "body": firstBody,
		"assignees": []string{assignee}, "reviewers": []string{reviewer},
	})
	next := requestManifest(t, r, publicationID, nextVersion, map[string]any{
		"title": nextTitle, "body": nextBody, "reviewers": []string{reviewer, nextReviewer},
	})
	onlyOpenPR := func(t *testing.T) *gitea.PullRequest {
		t.Helper()
		prs := publishPRs(t, r)
		require.Len(t, prs, 1, "must have exactly one PR in all states")
		require.NotNil(t, prs[0])
		pr := publishPR(t, r, prs[0].Index)
		require.Positive(t, pr.ID)
		require.Positive(t, pr.Index)
		require.Equal(t, gitea.StateOpen, pr.State)
		require.NotNil(t, pr.Head)
		require.NotEmpty(t, pr.Head.Sha)
		return pr
	}

	c.ok(t, "apply", first)
	opened := onlyOpenPR(t)
	assertPRMetadata(t, opened, firstTitle, firstBody, assignee)
	firstReviews := publishReviews(t, r, opened.Index)
	require.Len(t, firstReviews, 1)
	require.NotNil(t, firstReviews[reviewer], "missing initial review request for %s", reviewer)
	require.Positive(t, firstReviews[reviewer].ID)
	require.Equal(t, gitea.ReviewStateRequestReview, firstReviews[reviewer].State)

	c.ok(t, "apply", first)
	unchanged := onlyOpenPR(t)
	require.Equal(t, opened.ID, unchanged.ID)
	require.Equal(t, opened.Index, unchanged.Index)
	require.Equal(t, opened.Head.Sha, unchanged.Head.Sha)
	assertPRMetadata(t, unchanged, firstTitle, firstBody, assignee)
	unchangedReviews := publishReviews(t, r, unchanged.Index)
	require.Len(t, unchangedReviews, 1)
	require.NotNil(t, unchangedReviews[reviewer], "identical apply must keep the review request for %s", reviewer)
	require.Equal(t, gitea.ReviewStateRequestReview, unchangedReviews[reviewer].State)
	require.Equal(t, firstReviews[reviewer].ID, unchangedReviews[reviewer].ID)

	// Approve the published commit as the requested collaborator.
	asReviewer := s.sdkClient(t, gitea.SetToken(s.accessToken(t, reviewer, reviewerPassword)))
	approval, resp, err := asReviewer.PullRequests.CreatePullReview(t.Context(), s.owner, r.name, opened.Index, gitea.CreatePullReviewOptions{
		State: gitea.ReviewStateApproved, CommitID: opened.Head.Sha, Body: "Approved the first version",
	})
	if err != nil {
		s.apiFailure(t, "approve the original published commit", resp, err)
	}
	require.NotNil(t, approval)
	require.NotNil(t, approval.Reviewer)
	require.Positive(t, approval.ID)
	require.Equal(t, reviewer, approval.Reviewer.UserName)
	require.Equal(t, gitea.ReviewStateApproved, approval.State)
	require.Equal(t, opened.Head.Sha, approval.CommitID)

	// The update must request only the new reviewer.
	c.ok(t, "apply", next)
	updated := onlyOpenPR(t)
	require.Equal(t, opened.ID, updated.ID)
	require.Equal(t, opened.Index, updated.Index)
	require.NotEqual(t, opened.Head.Sha, updated.Head.Sha)
	assertPRMetadata(t, updated, nextTitle, nextBody, assignee)
	updatedReviews := publishReviews(t, r, updated.Index)
	require.Len(t, updatedReviews, 2)
	require.NotNil(t, updatedReviews[reviewer], "missing completed review for %s", reviewer)
	require.Equal(t, approval.ID, updatedReviews[reviewer].ID, "completed approval must not be replaced")
	require.Equal(t, gitea.ReviewStateApproved, updatedReviews[reviewer].State, "original reviewer must not be re-requested")
	require.False(t, updatedReviews[reviewer].Dismissed)
	require.NotNil(t, updatedReviews[nextReviewer], "missing new review request for %s", nextReviewer)
	require.Positive(t, updatedReviews[nextReviewer].ID)
	require.Equal(t, gitea.ReviewStateRequestReview, updatedReviews[nextReviewer].State)
}

type giteaPublication struct {
	repo *repository
	cli  *forgeCLI
}

func (p giteaPublication) run(t *testing.T, mode string, input publicationInput) string {
	t.Helper()
	manifest := requestManifest(t, p.repo, publicationID, input.version, map[string]any{"title": input.title})
	return p.cli.ok(t, mode, manifest)
}

func (p giteaPublication) requests(t *testing.T) []publicationRequest {
	t.Helper()
	prs := publishPRs(t, p.repo)
	requests := make([]publicationRequest, 0, len(prs))
	for _, pr := range prs {
		require.NotNil(t, pr)
		requests = append(requests, giteaRequest(t, publishPR(t, p.repo, pr.Index)))
	}
	return requests
}

func giteaRequest(t *testing.T, pr *gitea.PullRequest) publicationRequest {
	t.Helper()
	require.NotNil(t, pr.Head)
	require.NotNil(t, pr.Base)
	return publicationRequest{
		id: pr.ID, number: pr.Index, open: pr.State == gitea.StateOpen, title: pr.Title,
		headRepoID: pr.Head.RepoID, head: pr.Head.Ref, headSHA: pr.Head.Sha,
		baseRepoID: pr.Base.RepoID, base: pr.Base.Ref, baseSHA: pr.Base.Sha,
	}
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
	assertPublicationHead(t, r.gitRepository, giteaRequest(t, managed), upstream.ID, head, before, managedVersion)
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
	options := gitea.ListPullRequestsOptions{State: gitea.StateAll}
	var prs []*gitea.PullRequest
	for {
		page, resp, err := r.server.client.PullRequests.ListRepoPullRequests(t.Context(), r.server.owner, r.name, options)
		if err != nil {
			r.server.apiFailure(t, "list all repository PRs", resp, err)
		}
		prs = append(prs, page...)
		if resp.NextPage == 0 {
			return prs
		}
		options.Page = resp.NextPage
	}
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
