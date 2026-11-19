//go:build acceptance

package gitlab

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/forge"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
	gitlabsdk "gitlab.com/gitlab-org/api/client-go"
)

type publication struct {
	server    *fixture
	repo      *forge.GitRepository
	projectID int64
	path      string
	cli       *cli.Runner
}

func (g *fixture) newPublication(t *testing.T) forge.PublicationFixture {
	t.Helper()
	project := g.newProject(t)
	repo := forge.NewGitRepository(t, g.url, actorName, g.token)
	// Use the mapped address rather than GitLab's advertised clone URL.
	repo.Seed(t, g.url+"/"+project.PathWithNamespace+".git")
	err := awaitRead(t, 15*time.Second, "project default branch after Git seed", func(ctx context.Context) (bool, *gitlabsdk.Response, error) {
		got, resp, err := g.client.Projects.GetProject(project.ID, nil, gitlabsdk.WithContext(ctx))
		if err != nil {
			return false, resp, err
		}
		if got == nil || got.ID != project.ID || got.Visibility != gitlabsdk.PrivateVisibility {
			t.Fatal("seeded project is not the same private project")
		}
		return !got.EmptyRepo && got.DefaultBranch == "main", resp, nil
	})
	require.NoError(t, err)
	head, _, _ := strings.Cut(repo.Git(t, "ls-remote", "--symref", "origin", "HEAD"), "\n")
	require.Equal(t, "ref: refs/heads/main\tHEAD", head, "GitLab's advertised HEAD must point to main")

	runner := cli.NewWithCredentials(t, actorName, g.token,
		"UPDATECLI_GITLAB_USERNAME="+actorName, "UPDATECLI_GITLAB_TOKEN="+g.token)
	return forge.PublicationFixture{
		Repo: repo, RepoID: project.ID,
		Driver: &publication{server: g, repo: repo, projectID: project.ID, path: project.Path, cli: runner},
	}
}

func (p *publication) Run(t *testing.T, mode string, input forge.PublicationInput) string {
	t.Helper()
	scm := map[string]any{"kind": "gitlab", "spec": map[string]any{
		"url": p.server.url, "owner": actorName, "repository": p.path, "branch": p.repo.Base,
		"username": `{{ requiredEnv "UPDATECLI_GITLAB_USERNAME" }}`,
		"token":    `{{ requiredEnv "UPDATECLI_GITLAB_TOKEN" }}`,
	}}
	action := map[string]any{"kind": "gitlab/mergerequest", "scmid": "origin", "spec": map[string]any{"title": input.Title}}
	manifest := cli.Manifest(t, forge.PublicationConfig(forge.PublicationID, input.Version, scm, action))
	return p.cli.PipelineOK(t, mode, manifest)
}

func (p *publication) Requests(t *testing.T) []forge.PublicationRequest {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// No state filter: closed and merged requests count, too.
	options := &gitlabsdk.ListProjectMergeRequestsOptions{Scope: new("all")}
	var response *gitlabsdk.Response
	listed, err := gitlabsdk.ScanAndCollect(func(page gitlabsdk.PaginationOptionFunc) ([]*gitlabsdk.BasicMergeRequest, *gitlabsdk.Response, error) {
		mrs, resp, err := p.server.client.MergeRequests.ListProjectMergeRequests(p.projectID, options, gitlabsdk.WithContext(ctx), page)
		response = resp
		return mrs, resp, err
	})
	requireAPI(t, "list project merge requests in all states", response, err)

	requests := make([]forge.PublicationRequest, 0, len(listed))
	head := forge.PublicationHead(p.repo)
	safe := func(s string) string { return cli.RedactCredential(s, actorName, p.server.token) }
	for _, mr := range listed {
		require.NotNil(t, mr)
		var baseSHA string
		// Poll only the expected MR; the shared checks reject unexpected MRs.
		if len(listed) == 1 && mr.State == "opened" && mr.SourceBranch == head && mr.SourceProjectID == p.projectID {
			headSHA := p.repo.Refs(t)["refs/heads/"+head]
			require.NotEmpty(t, headSHA, "GitLab MR source branch is missing from the Git remote")
			detail := p.readRequest(t, mr, headSHA)
			mr, baseSHA = &detail.BasicMergeRequest, detail.DiffRefs.StartSha
		}
		requests = append(requests, forge.PublicationRequest{
			ID: mr.ID, Number: mr.IID, HeadRepoID: mr.SourceProjectID, BaseRepoID: mr.TargetProjectID,
			Open: mr.State == "opened", Head: safe(mr.SourceBranch), Base: safe(mr.TargetBranch),
			HeadSHA: safe(mr.SHA), BaseSHA: safe(baseSHA), Title: safe(mr.Title),
		})
	}
	return requests
}

func (p *publication) readRequest(t *testing.T, listed *gitlabsdk.BasicMergeRequest, headSHA string) *gitlabsdk.MergeRequest {
	t.Helper()
	require.Positive(t, listed.IID)
	var detail *gitlabsdk.MergeRequest
	// GitLab refreshes MR diff refs asynchronously after a push. Never retry
	// apply. The expected head comes from Git, not the MR list or the CLI.
	err := awaitRead(t, 30*time.Second, "merge request diff refs for the pushed head", func(ctx context.Context) (bool, *gitlabsdk.Response, error) {
		got, resp, err := p.server.client.MergeRequests.GetMergeRequest(p.projectID, listed.IID, nil, gitlabsdk.WithContext(ctx))
		if err != nil {
			return false, resp, err
		}
		require.NotNil(t, got)
		require.Equal(t, listed.ID, got.ID, "list and detail returned different MR IDs")
		require.Equal(t, listed.IID, got.IID, "list and detail returned different MR numbers")
		require.Equal(t, p.projectID, got.ProjectID)
		detail = got
		return got.DiffRefs.HeadSha == headSHA && got.SHA == headSHA && got.DiffRefs.StartSha != "", resp, nil
	})
	if err != nil {
		last := "<not observed>"
		if detail != nil {
			last = displaySHA(detail.DiffRefs.HeadSha)
		}
		t.Fatalf("GitLab MR !%d (ID %d): last diff head %s, want %s: %v", listed.IID, listed.ID, last, displaySHA(headSHA), err)
	}
	require.True(t, detail.DiffRefs.StartSha == detail.DiffRefs.BaseSha,
		"GitLab MR target and merge-base SHAs differ")
	require.NotNil(t, detail.Author)
	require.Equal(t, p.server.ownerID, detail.Author.ID, "GitLab MR must be authored by the fixture actor")
	return detail
}

// Log only object IDs, never arbitrary strings returned in SHA fields.
func displaySHA(sha string) string {
	if sha == "" {
		return "<empty>"
	}
	if len(sha) == 40 || len(sha) == 64 {
		if _, err := hex.DecodeString(sha); err == nil {
			return sha
		}
	}
	return "<invalid>"
}

// The caller reads on the test goroutine, including on the first attempt.
func awaitRead(t *testing.T, timeout time.Duration, label string, read func(context.Context) (bool, *gitlabsdk.Response, error)) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var lastStatus int
	for {
		ready, response, err := read(ctx)
		if ready {
			return nil
		}
		// Retry transient reads, but surface authorization and other
		// permanent client errors immediately.
		if response != nil && response.Response != nil {
			lastStatus = response.StatusCode
			if code := lastStatus; err != nil && code >= 400 && code < 500 &&
				code != http.StatusNotFound && code != http.StatusTooManyRequests {
				requireAPI(t, label, response, err)
			}
		}
		select {
		case <-ctx.Done():
			if lastStatus != 0 {
				return fmt.Errorf("waiting for GitLab CE %s: %w (last response: HTTP %d)", label, ctx.Err(), lastStatus)
			}
			return fmt.Errorf("waiting for GitLab CE %s: %w (no HTTP response)", label, ctx.Err())
		case <-tick.C:
		}
	}
}
