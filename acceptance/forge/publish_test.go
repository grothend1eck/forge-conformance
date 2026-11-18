//go:build acceptance

package forge

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
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
		id       = "publish"
		assignee = "publish-assignee"
		reviewer = "publish-reviewer"
		next     = "publish-next-reviewer"
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
	s.createUser(t, next, rand.Text())
	for _, user := range []string{assignee, reviewer, next} {
		write := gitea.AccessModeWrite
		resp, err := s.client.Repositories.AddCollaborator(t.Context(), s.owner, r.name, user, gitea.AddCollaboratorOption{Permission: &write})
		if err != nil {
			s.apiFailure(t, "grant repository access to "+user, resp, err)
		}
	}
	const (
		firstTitle = "Bump the fixture to v1.1.0"
		firstBody  = "Acceptance metadata for v1.1.0.\n\n- First update"
		nextTitle  = "Bump the fixture to v1.2.0"
		nextBody   = "Acceptance metadata for v1.2.0.\n\n- Follow-up update"
	)
	var observed *gitea.PullRequest
	var requestID, approvalID int64
	assertPublicationLifecycle(t, publicationFixture{
		publicationTarget: giteaPublicationTarget(r, repo.ID, "updatecli_"+r.base+"_"+id),
		manifest: func(version string) string {
			if version == firstPublicationVersion {
				return requestManifest(r, id, version, map[string]any{
					"title": firstTitle, "body": firstBody,
					"assignees": []string{assignee}, "reviewers": []string{reviewer},
				})
			}
			return requestManifest(r, id, version, map[string]any{
				"title": nextTitle, "body": nextBody, "reviewers": []string{reviewer, next},
			})
		},
		run: func(t *testing.T, mode, manifest string) string {
			return c.ok(t, mode, manifest)
		},
		requests: func(t *testing.T) []publicationRequest {
			prs := publishPRs(t, r)
			requests := make([]publicationRequest, len(prs))
			for i, pr := range prs {
				observed = publishPR(t, r, pr.Index)
				requests[i] = giteaPublicationRequest(t, observed)
			}
			return requests
		},
		check: func(t *testing.T, step publicationStep) {
			require.Len(t, observed.Assignees, 1)
			require.Equal(t, assignee, observed.Assignees[0].UserName)
			reviews := publishReviews(t, r, observed.Index)
			require.Contains(t, reviews, reviewer)
			if step == publicationUpdated {
				require.Equal(t, nextTitle, observed.Title)
				require.Equal(t, nextBody, observed.Body)
				require.Len(t, reviews, 2)
				require.Equal(t, approvalID, reviews[reviewer].ID, "completed approval must not be replaced")
				require.Equal(t, gitea.ReviewStateApproved, reviews[reviewer].State, "original reviewer must not be re-requested")
				require.False(t, reviews[reviewer].Dismissed)
				require.Contains(t, reviews, next)
				require.Equal(t, gitea.ReviewStateRequestReview, reviews[next].State)
				return
			}

			require.Equal(t, firstTitle, observed.Title)
			require.Equal(t, firstBody, observed.Body)
			require.Len(t, reviews, 1)
			require.Equal(t, gitea.ReviewStateRequestReview, reviews[reviewer].State)
			if step == publicationOpened {
				requestID = reviews[reviewer].ID
				return
			}
			require.Equal(t, requestID, reviews[reviewer].ID)

			// Complete the first review as the actual collaborator before changing input.
			asReviewer := s.sdkClient(t, gitea.SetToken(s.accessToken(t, reviewer, reviewerPassword)))
			approval, resp, err := asReviewer.PullRequests.CreatePullReview(t.Context(), s.owner, r.name, observed.Index, gitea.CreatePullReviewOptions{
				State: gitea.ReviewStateApproved, CommitID: observed.Head.Sha, Body: "Approved the first version",
			})
			if err != nil {
				s.apiFailure(t, "approve the original published commit", resp, err)
			}
			require.NotNil(t, approval.Reviewer)
			require.Equal(t, reviewer, approval.Reviewer.UserName)
			require.Equal(t, gitea.ReviewStateApproved, approval.State)
			approvalID = approval.ID
		},
	})
}

func testClean(t *testing.T, s *server) {
	r := s.newRepo(t)
	c := newCLI(t, s)
	const (
		id     = "clean"
		value  = "v1.1.0"
		reason = "Pull request closed as no changed file detected"
	)
	manifest := requestManifest(r, id, value, nil)
	c.ok(t, "apply", manifest)
	prs := publishPRs(t, r)
	require.Len(t, prs, 1)
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
		require.NoError(collect, err)
		require.Len(collect, list, 1, "the forge must leave the PR open for Updatecli to close")
		require.Equal(collect, index, list[0].Index)
		require.NotNil(collect, list[0].Head)
		require.Equal(collect, base, list[0].Head.Sha)
		pr, _, err := s.client.PullRequests.GetPullRequest(t.Context(), s.owner, r.name, index)
		require.NoError(collect, err)
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
	require.Len(t, publishPRs(t, r), 1)
	comments, resp, err := s.client.Issues.ListIssueComments(t.Context(), s.owner, r.name, index, gitea.ListIssueCommentOptions{})
	if err != nil {
		s.apiFailure(t, "list PR cleanup comments", resp, err)
	}
	var bodies []string
	for _, comment := range comments {
		bodies = append(bodies, comment.Body)
	}
	require.Contains(t, bodies, reason, "closure must be attributed to Updatecli, not the branch deletion hook")
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
	output := c.ok(t, "apply", requestManifest(r, id, "v1.0.0", nil), "--clean-git-branches")
	require.NotContains(t, output, foreign.HTMLURL)
	require.Len(t, publishPRs(t, r), 1)
	require.Equal(t, before, r.refs(t))
	assertForeignUntouched()

	// An otherwise matching fork PR must not be reused, edited, or closed.
	const managedTitle = "Managed update on the upstream repository"
	c.ok(t, "apply", requestManifest(r, id, firstPublicationVersion, map[string]any{"title": managedTitle}), "--clean-git-branches")
	prs := publishPRs(t, r)
	require.Len(t, prs, 2)
	var managed *gitea.PullRequest
	for _, pr := range prs {
		if pr.Index != foreign.Index {
			managed = publishPR(t, r, pr.Index)
		}
	}
	require.NotNil(t, managed)
	require.Equal(t, managedTitle, managed.Title)
	giteaPublicationTarget(r, upstream.ID, head).assertHead(t, giteaPublicationRequest(t, managed), before, firstPublicationVersion)
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
	return pr
}

func giteaPublicationTarget(r *repository, repoID int64, head string) publicationTarget {
	return publicationTarget{
		repositoryID: strconv.FormatInt(repoID, 10), headBranch: head, baseBranch: r.base,
		refs: r.refs, git: r.git,
	}
}

func giteaPublicationRequest(t *testing.T, pr *gitea.PullRequest) publicationRequest {
	t.Helper()
	require.NotNil(t, pr)
	req := publicationRequest{id: fmt.Sprintf("%d/%d", pr.ID, pr.Index), open: pr.State == gitea.StateOpen}
	if pr.Head != nil {
		req.head = publicationRef{repositoryID: strconv.FormatInt(pr.Head.RepoID, 10), branch: pr.Head.Ref, sha: pr.Head.Sha}
	}
	if pr.Base != nil {
		req.base = publicationRef{repositoryID: strconv.FormatInt(pr.Base.RepoID, 10), branch: pr.Base.Ref, sha: pr.Base.Sha}
	}
	return req
}

func publishReviews(t *testing.T, r *repository, index int64) map[string]*gitea.PullReview {
	t.Helper()
	reviews, resp, err := r.server.client.PullRequests.ListPullReviews(t.Context(), r.server.owner, r.name, index, gitea.ListPullReviewsOptions{})
	if err != nil {
		r.server.apiFailure(t, "list PR review requests", resp, err)
	}
	byReviewer := make(map[string]*gitea.PullReview)
	for _, review := range reviews {
		require.NotNil(t, review.Reviewer)
		name := review.Reviewer.UserName
		require.NotContains(t, byReviewer, name, "unexpected duplicate review or request")
		byReviewer[name] = review
	}
	return byReviewer
}
