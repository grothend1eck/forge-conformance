//go:build acceptance

package forge

import (
	"crypto/rand"
	"testing"

	gitea "gitea.dev/sdk"
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
		"assignees": []string{assignee}, "reviewers": []string{reviewer, assignee},
	})
	next := requestManifest(t, r, publicationID, nextVersion, map[string]any{
		"title": nextTitle, "body": nextBody, "reviewers": []string{reviewer, nextReviewer},
	})
	c.ok(t, "apply", first)
	opened := onlyOpenPullRequest(t, r)
	assertPRMetadata(t, opened, firstTitle, firstBody, assignee)
	firstReviews := publishReviews(t, r, opened.Index)
	for _, user := range []string{reviewer, assignee} {
		require.NotNil(t, firstReviews[user], "missing initial review request for %s", user)
		require.Positive(t, firstReviews[user].ID)
		require.Equal(t, gitea.ReviewStateRequestReview, firstReviews[user].State)
	}
	require.Len(t, firstReviews, 2)

	c.ok(t, "apply", first)
	unchanged := onlyOpenPullRequest(t, r)
	require.Equal(t, opened.ID, unchanged.ID)
	require.Equal(t, opened.Index, unchanged.Index)
	require.Equal(t, opened.Head.Sha, unchanged.Head.Sha)
	assertPRMetadata(t, unchanged, firstTitle, firstBody, assignee)
	unchangedReviews := publishReviews(t, r, unchanged.Index)
	require.Len(t, unchangedReviews, 2)
	for _, user := range []string{reviewer, assignee} {
		require.NotNil(t, unchangedReviews[user], "identical apply must keep the review request for %s", user)
		require.Equal(t, gitea.ReviewStateRequestReview, unchangedReviews[user].State)
		require.Equal(t, firstReviews[user].ID, unchangedReviews[user].ID)
	}

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

	// The update must request only the new reviewer and leave the omitted
	// review request in place.
	c.ok(t, "apply", next)
	updated := onlyOpenPullRequest(t, r)
	require.Equal(t, opened.ID, updated.ID)
	require.Equal(t, opened.Index, updated.Index)
	require.NotEqual(t, opened.Head.Sha, updated.Head.Sha)
	assertPRMetadata(t, updated, nextTitle, nextBody, assignee)
	updatedReviews := publishReviews(t, r, updated.Index)
	require.Len(t, updatedReviews, 3)
	require.NotNil(t, updatedReviews[reviewer], "missing completed review for %s", reviewer)
	require.Equal(t, approval.ID, updatedReviews[reviewer].ID, "completed approval must not be replaced")
	require.Equal(t, gitea.ReviewStateApproved, updatedReviews[reviewer].State, "original reviewer must not be re-requested")
	require.False(t, updatedReviews[reviewer].Dismissed)
	require.NotNil(t, updatedReviews[assignee], "removing a reviewer from the spec must not withdraw their request")
	require.Equal(t, firstReviews[assignee].ID, updatedReviews[assignee].ID)
	require.Equal(t, gitea.ReviewStateRequestReview, updatedReviews[assignee].State)
	require.NotNil(t, updatedReviews[nextReviewer], "missing new review request for %s", nextReviewer)
	require.Positive(t, updatedReviews[nextReviewer].ID)
	require.Equal(t, gitea.ReviewStateRequestReview, updatedReviews[nextReviewer].State)
}

func onlyOpenPullRequest(t *testing.T, r *repository) *gitea.PullRequest {
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
	require.NotEmpty(t, pr.HTMLURL)
	return pr
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
