//go:build acceptance

package forge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
	gitlabsdk "gitlab.com/gitlab-org/api/client-go"
)

type gitLabPublication struct {
	fixture *gitlabFixture
	project *gitlabsdk.Project
	repo    *gitRepository
	cli     *forgeCLI
}

func (g *gitlabFixture) newPublication(t *testing.T) publicationFixture {
	t.Helper()
	project := g.newProject(t)
	r := seedGitLabPublication(t, g, project)
	d := &gitLabPublication{
		fixture: g, project: project, repo: r,
		cli: &forgeCLI{
			runner: cli.New(t, "UPDATECLI_GITLAB_USERNAME="+g.owner, "UPDATECLI_GITLAB_TOKEN="+g.token),
			user:   g.owner,
			token:  g.token,
		},
	}
	return publicationFixture{repo: r, repoID: project.ID, driver: d}
}

func seedGitLabPublication(t *testing.T, g *gitlabFixture, project *gitlabsdk.Project) *gitRepository {
	t.Helper()
	require.NotEmpty(t, project.Path)

	// The advertised clone URL can contain the port inside the container.
	root := strings.TrimRight(g.url, "/")
	r := newGitRepository(t, root, g.owner, g.token)
	r.seed(t, root+"/"+project.PathWithNamespace+".git")
	head, _, _ := strings.Cut(r.git(t, "ls-remote", "--symref", "origin", "HEAD"), "\n")
	require.Equal(t, "ref: refs/heads/main\tHEAD", head, "GitLab's advertised HEAD must point to main")

	// The API's project metadata can lag behind the refs just pushed.
	const timeout = 15 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		got, resp, err := g.client.Projects.GetProject(project.ID, nil, gitlabsdk.WithContext(ctx))
		if err != nil {
			reportGitLabReadFailure(c, "get seeded project", resp, err)
			return
		}
		if assert.NotNil(c, got) {
			assert.False(c, got.EmptyRepo, "GitLab still reports the seeded project as empty")
			assert.Equal(c, "main", redactCredential(got.DefaultBranch, g.owner, g.token), "GitLab default branch")
		}
	}, timeout, 250*time.Millisecond, "GitLab did not expose the seeded project metadata")
	return r
}

func (d *gitLabPublication) run(t *testing.T, mode string, input publicationInput) string {
	t.Helper()
	return d.cli.ok(t, mode, d.manifest(t, input))
}

func (d *gitLabPublication) manifest(t *testing.T, input publicationInput) string {
	t.Helper()
	return manifestYAML(t, map[string]any{
		"name":       "Update the fixture version",
		"pipelineid": publicationID,
		"scms": map[string]any{
			"origin": map[string]any{
				"kind": "gitlab",
				"spec": map[string]any{
					"url": d.repo.url, "owner": d.fixture.owner,
					"repository": d.project.Path, "branch": d.repo.base,
					"username": `{{ requiredEnv "UPDATECLI_GITLAB_USERNAME" }}`,
					"token":    `{{ requiredEnv "UPDATECLI_GITLAB_TOKEN" }}`,
				},
			},
		},
		"targets": map[string]any{
			"version": map[string]any{
				"name": "Update the version", "kind": "yaml", "scmid": "origin",
				"spec": map[string]any{"file": "version.yaml", "key": "$.version", "value": input.version},
			},
		},
		"actions": map[string]any{
			"request": map[string]any{
				"kind": "gitlab/mergerequest", "scmid": "origin",
				"spec": map[string]any{"title": input.title},
			},
		},
	})
}

func (d *gitLabPublication) requests(t *testing.T) []publicationRequest {
	t.Helper()
	g := d.fixture
	safe := func(s string) string { return redactCredential(s, g.owner, g.token) }
	head := publicationHead(d.repo)
	listed := listGitLabPublicationRequests(t, g, d.project.ID)
	requests := make([]publicationRequest, 0, len(listed))
	for _, mr := range listed {
		require.NotNil(t, mr)
		var baseSHA string
		// Unexpected MRs fail the shared cardinality/state/identity checks.
		// Read the expected MR's diff refs and title from its detail endpoint.
		if len(listed) == 1 && mr.State == "opened" && mr.SourceBranch == head && mr.SourceProjectID == d.project.ID {
			headSHA := d.repo.refs(t)["refs/heads/"+head]
			require.NotEmpty(t, headSHA, "GitLab MR source branch is missing from the remote")
			detail := readGitLabPublicationRequest(t, g, d.project.ID, mr, headSHA)
			mr = &detail.BasicMergeRequest
			baseSHA = detail.DiffRefs.StartSha
		}
		requests = append(requests, publicationRequest{
			id: mr.ID, number: mr.IID, headRepoID: mr.SourceProjectID, baseRepoID: mr.TargetProjectID,
			open: mr.State == "opened", head: safe(mr.SourceBranch), base: safe(mr.TargetBranch),
			headSHA: safe(mr.SHA), baseSHA: safe(baseSHA), title: safe(mr.Title),
		})
	}
	return requests
}

func listGitLabPublicationRequests(t *testing.T, g *gitlabFixture, projectID int64) []*gitlabsdk.BasicMergeRequest {
	t.Helper()
	// No state filter: closed and merged requests must count, too.
	options := &gitlabsdk.ListProjectMergeRequestsOptions{
		Scope: new("all"),
	}
	var requests []*gitlabsdk.BasicMergeRequest
	for {
		page, resp, err := g.client.MergeRequests.ListProjectMergeRequests(projectID, options, gitlabsdk.WithContext(t.Context()))
		if err != nil {
			gitLabAPIFailure(t, "list project merge requests in all states", resp, err)
		}
		requests = append(requests, page...)
		if resp.NextPage == 0 {
			return requests
		}
		options.Page = resp.NextPage
	}
}

func readGitLabPublicationRequest(t *testing.T, g *gitlabFixture, projectID int64, listed *gitlabsdk.BasicMergeRequest, headSHA string) *gitlabsdk.MergeRequest {
	t.Helper()
	require.Positive(t, listed.IID)
	safe := func(s string) string { return redactCredential(s, g.owner, g.token) }

	// A successful push and MR write can precede GitLab's async diff refresh.
	// Wait only on the detail endpoint, never by repeating the apply.
	const timeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	var mr *gitlabsdk.MergeRequest
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		got, resp, err := g.client.MergeRequests.GetMergeRequest(projectID, listed.IID, nil, gitlabsdk.WithContext(ctx))
		if err != nil {
			reportGitLabReadFailure(c, "get project merge request", resp, err)
			return
		}
		if assert.NotNil(c, got) && assert.Equal(c, headSHA, safe(got.DiffRefs.HeadSha), "GitLab MR diff head") {
			mr = got
		}
	}, timeout, 250*time.Millisecond, "GitLab did not refresh diff refs for MR !%d", listed.IID)
	require.Equal(t, listed.ID, mr.ID, "GitLab MR list and detail returned different IDs")
	require.Equal(t, listed.IID, mr.IID, "GitLab MR list and detail returned different IIDs")
	require.Equal(t, projectID, mr.ProjectID)
	require.Equal(t, headSHA, safe(mr.SHA), "GitLab MR diff and source SHAs differ")
	require.Equal(t, safe(mr.DiffRefs.StartSha), safe(mr.DiffRefs.BaseSha), "GitLab MR target and merge-base SHAs differ")
	require.NotNil(t, mr.Author)
	require.Equal(t, g.owner, safe(mr.Author.Username), "GitLab MR author is not the fixture actor")
	return mr
}

func reportGitLabReadFailure(c *assert.CollectT, action string, resp *gitlabsdk.Response, err error) {
	if resp != nil && resp.Response != nil {
		c.Errorf("GitLab: %s: HTTP %d", action, resp.StatusCode)
		return
	}
	c.Errorf("GitLab: %s: SDK request failed (%T)", action, err)
}
