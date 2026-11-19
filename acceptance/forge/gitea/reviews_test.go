//go:build acceptance

package gitea_test

import (
	"crypto/rand"
	"strconv"
	"testing"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/forge"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func (s *server) newPublication(t *testing.T) forge.PublicationFixture {
	t.Helper()
	r := s.newRepo(t)
	repo := publishRepo(t, r)
	require.NotNil(t, repo)
	require.True(t, repo.Private)
	require.Equal(t, "main", repo.DefaultBranch)
	require.Equal(t, s.owner+"/"+r.name, repo.FullName)
	return forge.PublicationFixture{Repo: r.GitRepository, RepoID: repo.ID, Driver: giteaPublication{repo: r, cli: newCLI(t, s)}}
}

// Use a separate PR so the common lifecycle never needs provider-specific
// collaborators, review reads or a mutation between its apply calls.
func (s *server) testPublishReviewMetadata(t *testing.T) {
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
	var reviewerPassword string
	for _, user := range []string{assignee, reviewer, nextReviewer} {
		password := rand.Text()
		s.createUser(t, user, password)
		r.grantPRWrite(t, user)
		if user == reviewer {
			reviewerPassword = password
		}
	}
	first := requestManifest(t, r, forge.PublicationID, firstVersion, map[string]any{
		"title": firstTitle, "body": firstBody,
		"assignees": []string{assignee}, "reviewers": []string{reviewer, assignee},
	})
	next := requestManifest(t, r, forge.PublicationID, nextVersion, map[string]any{
		"title": nextTitle, "body": nextBody, "reviewers": []string{reviewer, nextReviewer},
	})
	c.PipelineOK(t, "apply", first)
	opened := onlyOpenPullRequest(t, r)
	assertPRMetadata(t, opened, firstTitle, firstBody, assignee)
	firstReviews := publishReviews(t, r, opened.Index)
	for _, user := range []string{reviewer, assignee} {
		require.Positive(t, firstReviews[user].id, "missing initial review request for %s", user)
		require.Equal(t, gitea.ReviewStateRequestReview, firstReviews[user].state)
	}
	require.Len(t, firstReviews, 2)

	c.PipelineOK(t, "apply", first)
	unchanged := onlyOpenPullRequest(t, r)
	require.Equal(t, giteaRequest(t, opened), giteaRequest(t, unchanged))
	assertPRMetadata(t, unchanged, firstTitle, firstBody, assignee)
	require.Equal(t, firstReviews, publishReviews(t, r, unchanged.Index), "identical apply must keep the original review requests")

	asReviewer := s.sdkClient(t, gitea.SetToken(s.accessToken(t, reviewer, reviewerPassword)))
	approval, resp, err := asReviewer.PullRequests.CreatePullReview(t.Context(), s.owner, r.name, opened.Index, gitea.CreatePullReviewOptions{
		State: gitea.ReviewStateApproved, CommitID: opened.Head.Sha, Body: "Approved the first version",
	})
	s.check(t, "approve the original published commit", resp, err)
	require.NotNil(t, approval)
	require.Positive(t, approval.ID)
	require.Equal(t, reviewer, username(approval.Reviewer))
	require.Equal(t, gitea.ReviewStateApproved, approval.State)
	require.Equal(t, opened.Head.Sha, approval.CommitID)

	c.PipelineOK(t, "apply", next)
	updated := onlyOpenPullRequest(t, r)
	require.Equal(t, opened.ID, updated.ID)
	require.Equal(t, opened.Index, updated.Index)
	require.NotEqual(t, opened.Head.Sha, updated.Head.Sha)
	assertPRMetadata(t, updated, nextTitle, nextBody, assignee)
	updatedReviews := publishReviews(t, r, updated.Index)
	require.Positive(t, updatedReviews[nextReviewer].id, "missing new review request for %s", nextReviewer)
	require.Equal(t, map[string]reviewView{
		reviewer:     {id: approval.ID, state: gitea.ReviewStateApproved},
		assignee:     firstReviews[assignee],
		nextReviewer: {id: updatedReviews[nextReviewer].id, state: gitea.ReviewStateRequestReview},
	}, updatedReviews, "preserve the undismissed approval and the omitted review request; add only the new reviewer")
}

func testReviewFailureBlocksAutomerge(t *testing.T, s *server) {
	const id, title = "review-failure-blocks-automerge", "Required reviewer does not exist"
	r, before := newProtectedMergeRepo(t, s)
	c := newCLI(t, s)
	apply := func(t *testing.T, version, title string) string {
		t.Helper()
		spec := mergeSpec(title, "auto", "")
		spec["reviewers"] = []string{"acceptance-nonexistent-reviewer"}
		result := c.Pipeline(t, "apply", requestManifest(t, r, id, version, spec))
		require.NotZero(t, result.ExitCode, "review failure must fail the CLI")
		return result.Output
	}

	output := apply(t, "v1.1.0", title)
	opened := r.assertInitialMergeRequest(t, before, id, title, false)
	openedURL := publishPR(t, r, opened.number).HTMLURL
	const reviewFailure = "requesting reviewers for Gitea pull request "
	cli.RequireOutput(t, output, reviewFailure+strconv.Quote(openedURL), "* Link: "+openedURL)
	r.assertNoAutomerge(t, opened, "nonexistent reviewer rejected during creation")

	const editedTitle = "Still requires the nonexistent reviewer"
	output = apply(t, "v1.2.0", editedTitle)
	edited := r.assertEdited(t, opened, editedTitle, "v1.2.0", false)
	cli.RequireOutput(t, output, reviewFailure+strconv.Quote(openedURL), "* Link: "+openedURL)
	r.assertNoAutomerge(t, edited, "nonexistent reviewer rejected during edit")
}

func (r *repository) assertReviewBeforeScheduling(t *testing.T, number int64, reviewer string, editedTitle string) {
	t.Helper()
	reviews := publishReviews(t, r, number)
	require.Len(t, reviews, 1)
	require.Positive(t, reviews[reviewer].id)
	require.Equal(t, gitea.ReviewStateRequestReview, reviews[reviewer].state)

	var editID, reviewID, scheduleID int64
	for _, event := range r.issueTimeline(t, number) {
		switch event.Type {
		case "change_title":
			if event.NewTitle == editedTitle {
				editID = event.ID
			}
		case "review_request":
			require.Zero(t, reviewID, "duplicate review request event")
			reviewID = event.ID
		case "pull_scheduled_merge":
			require.Zero(t, scheduleID, "duplicate schedule event")
			require.Equal(t, r.server.owner, username(event.Poster))
			scheduleID = event.ID
		case "pull_cancel_scheduled_merge":
			t.Fatal("native schedule must not be canceled")
		}
	}
	require.Positive(t, reviewID, "expected a review request event")
	if editedTitle != "" {
		require.Positive(t, editID, "expected a title edit event")
		require.Greater(t, reviewID, editID, "the title edit must precede the review request")
	}
	require.Greater(t, scheduleID, reviewID, "the review request must precede the native schedule")
}

func (r *repository) grantPRWrite(t *testing.T, user string) {
	t.Helper()
	s := r.server
	resp, err := s.client.Repositories.AddCollaborator(t.Context(), s.owner, r.name, user, gitea.AddCollaboratorOption{Permission: new(gitea.AccessModeWrite)})
	s.check(t, "grant repository access to "+user, resp, err)
}

func onlyOpenPullRequest(t *testing.T, r *repository) *gitea.PullRequest {
	t.Helper()
	prs := publishPRs(t, r)
	require.Len(t, prs, 1, "must have exactly one PR in all states")
	require.NotNil(t, prs[0])
	require.Positive(t, prs[0].ID)
	require.Positive(t, prs[0].Index)
	pr := publishPR(t, r, prs[0].Index)
	require.Equal(t, prs[0].ID, pr.ID)
	require.Equal(t, prs[0].Index, pr.Index)
	require.Equal(t, gitea.StateOpen, pr.State)
	require.NotNil(t, pr.Head)
	require.NotEmpty(t, pr.Head.Sha)
	require.NotEmpty(t, pr.HTMLURL)
	return pr
}

type giteaPublication struct {
	repo *repository
	cli  *cli.Runner
}

func (p giteaPublication) Run(t *testing.T, mode string, input forge.PublicationInput) string {
	t.Helper()
	manifest := requestManifest(t, p.repo, forge.PublicationID, input.Version, map[string]any{"title": input.Title})
	return p.cli.PipelineOK(t, mode, manifest)
}

func (p giteaPublication) Requests(t *testing.T) []forge.PublicationRequest {
	t.Helper()
	prs := publishPRs(t, p.repo)
	requests := make([]forge.PublicationRequest, 0, len(prs))
	for _, pr := range prs {
		require.NotNil(t, pr)
		requests = append(requests, giteaRequest(t, publishPR(t, p.repo, pr.Index)))
	}
	return requests
}

func giteaRequest(t *testing.T, pr *gitea.PullRequest) forge.PublicationRequest {
	t.Helper()
	require.NotNil(t, pr.Head)
	require.NotNil(t, pr.Base)
	return forge.PublicationRequest{
		ID: pr.ID, Number: pr.Index, Open: pr.State == gitea.StateOpen, Title: pr.Title,
		HeadRepoID: pr.Head.RepoID, Head: pr.Head.Ref, HeadSHA: pr.Head.Sha,
		BaseRepoID: pr.Base.RepoID, Base: pr.Base.Ref, BaseSHA: pr.Base.Sha,
	}
}

func publishRepo(t *testing.T, r *repository) *gitea.Repository {
	t.Helper()
	repo, resp, err := r.server.client.Repositories.GetRepo(t.Context(), r.server.owner, r.name)
	r.server.check(t, "get repository metadata", resp, err)
	require.NotNil(t, repo)
	return repo
}

func publishPRs(t *testing.T, r *repository) []*gitea.PullRequest {
	t.Helper()
	options := gitea.ListPullRequestsOptions{State: gitea.StateAll}
	var prs []*gitea.PullRequest
	for {
		page, resp, err := r.server.client.PullRequests.ListRepoPullRequests(t.Context(), r.server.owner, r.name, options)
		r.server.check(t, "list all repository PRs", resp, err)
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
	r.server.check(t, "get repository PR", resp, err)
	require.NotNil(t, pr)
	return pr
}

func assertPRMetadata(t *testing.T, pr *gitea.PullRequest, title, body, assignee string) {
	t.Helper()
	require.Equal(t, title, pr.Title)
	require.True(t, body == pr.Body, "PR body must match the requested metadata")
	require.Len(t, pr.Assignees, 1)
	require.Equal(t, assignee, username(pr.Assignees[0]))
}

type reviewView struct {
	id        int64
	state     gitea.ReviewStateType
	dismissed bool
}

func publishReviews(t *testing.T, r *repository, index int64) map[string]reviewView {
	t.Helper()
	reviews, resp, err := r.server.client.PullRequests.ListPullReviews(t.Context(), r.server.owner, r.name, index, gitea.ListPullReviewsOptions{})
	r.server.check(t, "list PR review requests", resp, err)
	byReviewer := make(map[string]reviewView)
	for _, review := range reviews {
		require.NotNil(t, review)
		name := username(review.Reviewer)
		require.NotEmpty(t, name)
		require.NotContains(t, byReviewer, name, "unexpected duplicate review or request")
		byReviewer[name] = reviewView{review.ID, review.State, review.Dismissed}
	}
	return byReviewer
}
