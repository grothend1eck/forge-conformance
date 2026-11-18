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

type gitLabPublicationDriver struct {
	fixture *gitlabFixture
	project *gitlabsdk.Project
	repo    *gitRepository
	cli     *forgeCLI
	id      string
	head    string
}

func testGitLabPublish(t *testing.T, g *gitlabFixture) {
	t.Helper()
	const id = "publish"
	project := g.newProject(t)
	r := seedGitLabPublication(t, g, project)
	d := &gitLabPublicationDriver{
		fixture: g, project: project, repo: r, id: id, head: "updatecli_" + r.base + "_" + id,
		cli: &forgeCLI{
			runner: cli.New(t, "UPDATECLI_GITLAB_USERNAME="+g.owner, "UPDATECLI_GITLAB_TOKEN="+g.token),
			user:   g.owner,
			token:  g.token,
		},
	}
	p := openPublication(t, r, d, project.ID, d.head, "v1.1.0")
	p.reapply(t)
	p.update(t, "v1.2.0")
}

func seedGitLabPublication(t *testing.T, g *gitlabFixture, project *gitlabsdk.Project) *gitRepository {
	t.Helper()
	require.NotEmpty(t, project.Path)

	// The advertised clone URL can contain the port inside the container.
	root := strings.TrimRight(g.url, "/")
	r := newGitRepository(t, root, g.owner, g.token)
	r.seed(t, root+"/"+project.PathWithNamespace+".git")
	seedPublication(t, r)

	got, resp, err := g.client.Projects.GetProject(project.ID, nil, gitlabsdk.WithContext(t.Context()))
	if err != nil {
		gitLabAPIFailure(t, "get project after seeding", resp, err)
	}
	require.NotNil(t, got)
	require.Equal(t, "main", redactCredential(got.DefaultBranch, g.owner, g.token), "GitLab default branch")
	return r
}

func (d *gitLabPublicationDriver) run(t *testing.T, mode, version string) string {
	t.Helper()
	return d.cli.ok(t, mode, d.manifest(t, version))
}

func (d *gitLabPublicationDriver) manifest(t *testing.T, version string) string {
	t.Helper()
	return manifestYAML(t, map[string]any{
		"name":       "Update the fixture version",
		"pipelineid": d.id,
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
				"spec": map[string]any{"file": "version.yaml", "key": "$.version", "value": version},
			},
		},
		"actions": map[string]any{
			"request": map[string]any{
				"kind": "gitlab/mergerequest", "scmid": "origin",
			},
		},
	})
}

func (d *gitLabPublicationDriver) requests(t *testing.T) []publicationRequest {
	t.Helper()
	g := d.fixture
	safe := func(s string) string { return redactCredential(s, g.owner, g.token) }
	listed := listGitLabPublicationRequests(t, g, d.project.ID)
	requests := make([]publicationRequest, 0, len(listed))
	for _, mr := range listed {
		require.NotNil(t, mr)
		var baseSHA string
		// The shared oracle checks cardinality and state. Only the expected
		// local MR needs hydrated diff refs.
		if len(listed) == 1 && mr.State == "opened" && mr.SourceBranch == d.head && mr.SourceProjectID == d.project.ID {
			headSHA := d.repo.refs(t)["refs/heads/"+d.head]
			require.NotEmpty(t, headSHA, "GitLab MR source branch is missing from the remote")
			detail := readGitLabPublicationRequest(t, g, d.project.ID, mr, headSHA)
			mr = &detail.BasicMergeRequest
			baseSHA = detail.DiffRefs.StartSha
		}
		requests = append(requests, publicationRequest{
			id: mr.ID, number: mr.IID, headRepoID: mr.SourceProjectID, baseRepoID: mr.TargetProjectID,
			open: mr.State == "opened", head: safe(mr.SourceBranch), base: safe(mr.TargetBranch),
			headSHA: safe(mr.SHA), baseSHA: safe(baseSHA),
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
			if resp != nil && resp.Response != nil {
				c.Errorf("get GitLab MR !%d: HTTP %d", listed.IID, resp.StatusCode)
			} else {
				c.Errorf("get GitLab MR !%d: SDK request failed (%T)", listed.IID, err)
			}
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
