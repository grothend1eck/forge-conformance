//go:build acceptance

package forge

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

// PublicationFixture binds a fresh private repository to the real CLI and
// the provider API used to observe its requests.
type PublicationFixture struct {
	Repo   *GitRepository
	RepoID int64
	Driver PublicationDriver
}

// PublicationInput is the change requested by the Updatecli manifest.
type PublicationInput struct {
	Version, Title string
}

// PublicationDriver supplies the provider-specific manifest and request reads.
type PublicationDriver interface {
	// Run invokes the real CLI for the given input and requires a successful exit.
	Run(t *testing.T, mode string, input PublicationInput) string
	// Requests includes all states so closing and recreating a request cannot pass.
	Requests(t *testing.T) []PublicationRequest
}

// PublicationRequest is the provider's observed PR or MR state.
type PublicationRequest struct {
	ID, Number, HeadRepoID, BaseRepoID int64
	Open                               bool
	Head, Base, HeadSHA, BaseSHA       string
	Title                              string
}

// PublicationID is the manifest's stable pipeline ID.
const PublicationID = "publish"

// PublicationHead returns the working branch expected for PublicationID.
func PublicationHead(r *GitRepository) string {
	return "updatecli_" + r.Base + "_" + PublicationID
}

// PublicationCases binds the shared publication contracts to a provider fixture.
func PublicationCases(newFixture func(*testing.T) PublicationFixture) map[string]func(*testing.T) {
	return map[string]func(*testing.T){
		publishUpdate:    func(t *testing.T) { runPublishUpdate(t, newFixture) },
		editRequestTitle: func(t *testing.T) { runRequestTitle(t, newFixture) },
	}
}

func runPublishUpdate(t *testing.T, newFixture func(*testing.T) PublicationFixture) {
	t.Helper()
	for _, test := range []struct {
		name string
		run  func(*testing.T, PublicationFixture)
	}{
		{"lifecycle", testPublicationLifecycle},
		{"already_current", testPublicationAlreadyCurrent},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			// Give the configured base its own tree and seed refs the pipeline
			// must leave alone.
			r := f.Repo
			r.Stage(t, r.Base, "stable-only.txt", "not part of the update\n")
			base := "refs/heads/" + r.Base
			r.Git(t, "push", "origin", base+":refs/heads/unrelated", base+":refs/tags/publish-anchor")
			test.run(t, f)
		})
	}
}

func (f PublicationFixture) initialRefs(t *testing.T) map[string]string {
	t.Helper()
	before := f.Repo.Refs(t)
	base := before["refs/heads/"+f.Repo.Base]
	require.NotEmpty(t, base)
	require.NotEmpty(t, before["refs/heads/main"])
	require.NotEqual(t, before["refs/heads/main"], base, "the configured base must differ from main")
	require.NotEmpty(t, before["refs/heads/unrelated"])
	require.Equal(t, before["refs/heads/unrelated"], before["refs/tags/publish-anchor"])
	require.NotContains(t, before, "refs/heads/"+PublicationHead(f.Repo))
	require.Empty(t, f.Driver.Requests(t), "the publication fixture must start without requests")
	return before
}

func testPublicationLifecycle(t *testing.T, f PublicationFixture) {
	first := PublicationInput{Version: "v1.1.0", Title: "Bump the fixture to v1.1.0"}
	next := PublicationInput{Version: "v1.2.0", Title: "Bump the fixture to v1.2.0"}
	r, d := f.Repo, f.Driver
	head := PublicationHead(r)
	before := f.initialRefs(t)

	output := d.Run(t, "diff", first)
	cli.RequireOutput(t, output, "version.yaml", first.Version)
	require.Equal(t, before, r.Refs(t), "diff must not write refs")
	require.Empty(t, d.Requests(t), "diff must not create a request in any state")

	d.Run(t, "apply", first)
	requests := d.Requests(t)
	require.Len(t, requests, 1, "first apply must open exactly one request")
	opened := requests[0]
	require.Positive(t, opened.ID)
	require.Positive(t, opened.Number)
	require.Equal(t, first.Title, opened.Title, "title of the published request")
	afterOpening := AssertPublicationHead(t, r, opened, f.RepoID, head, before, first.Version)
	require.Equal(t, []string{opened.HeadSHA, before["refs/heads/"+r.Base]},
		strings.Fields(r.Git(t, "rev-list", "--parents", "-n", "1", opened.HeadSHA)),
		"the configured base must be the first published commit's only parent")

	d.Run(t, "apply", first)
	require.Equal(t, afterOpening, r.Refs(t), "an identical apply must not add a commit or move any ref")
	requests = d.Requests(t)
	require.Len(t, requests, 1, "an identical apply must not create or remove a request")
	require.Equal(t, opened, requests[0], "an identical apply must leave the original request, refs and title unchanged")

	d.Run(t, "apply", next)
	requests = d.Requests(t)
	require.Len(t, requests, 1, "changing input must update the existing request, not replace it")
	updated := requests[0]
	require.Equal(t, opened.ID, updated.ID)
	require.Equal(t, opened.Number, updated.Number)
	require.NotEqual(t, opened.HeadSHA, updated.HeadSHA, "changing input must publish a new head")
	// A push can advance the SHA without editing the request itself.
	require.Equal(t, next.Title, updated.Title, "title must change on the original request")
	AssertPublicationHead(t, r, updated, f.RepoID, head, before, next.Version)
}

func testPublicationAlreadyCurrent(t *testing.T, f PublicationFixture) {
	// Only the configured base is up to date; targeting main would publish.
	input := PublicationInput{Version: "v1.1.0", Title: "Do not publish an already current fixture"}
	f.Repo.Stage(t, f.Repo.Base, "version.yaml", versionFile(input.Version))
	before := f.initialRefs(t)

	output := f.Driver.Run(t, "apply", input)
	cli.RequireOutput(t, output, "version.yaml", `already set to "`+input.Version+`"`)
	require.Equal(t, before, f.Repo.Refs(t), "an already current target must not write refs")
	require.Empty(t, f.Driver.Requests(t), "an already current target must not create a request in any state")
}

// AssertPublicationHead checks request identity and independently verifies its
// proposed change and untouched sibling refs via native Git.
func AssertPublicationHead(t *testing.T, r *GitRepository, req PublicationRequest, repoID int64, head string, before map[string]string, version string) map[string]string {
	t.Helper()
	require.True(t, req.Open, "the request must be open")
	require.Equal(t, head, req.Head)
	require.Equal(t, r.Base, req.Base)
	require.Equal(t, repoID, req.HeadRepoID)
	require.Equal(t, repoID, req.BaseRepoID)
	require.NotEmpty(t, req.HeadSHA)
	baseSHA := before["refs/heads/"+r.Base]
	require.NotEmpty(t, baseSHA)
	require.Equal(t, baseSHA, req.BaseSHA)

	headRef := "refs/heads/" + head
	wantRefs := maps.Clone(before)
	wantRefs[headRef] = req.HeadSHA
	refs := r.Refs(t)
	require.Equal(t, wantRefs, refs, "the request head must match native Git, with all sibling refs untouched")

	r.Git(t, "fetch", "--no-tags", "origin", headRef)
	require.Equal(t, req.HeadSHA, strings.TrimSpace(r.Git(t, "rev-parse", "FETCH_HEAD")), "Git must fetch the request head")
	r.Git(t, "merge-base", "--is-ancestor", baseSHA, req.HeadSHA)
	require.Equal(t, "version.yaml", strings.TrimSpace(r.Git(t, "diff", "--name-only", baseSHA, req.HeadSHA)))
	require.Equal(t, versionFile(version), r.Git(t, "show", req.HeadSHA+":version.yaml"))
	return refs
}

func versionFile(version string) string {
	return "version: " + version + "\nkeep: untouched\n"
}

// PublicationConfig builds a pipeline with a common edit and real,
// provider-specific SCM and request action.
func PublicationConfig(id, version string, scm, action map[string]any) map[string]any {
	return map[string]any{
		"name":       "Update the fixture version",
		"pipelineid": id,
		"scms":       map[string]any{"origin": scm},
		"targets": map[string]any{
			"version": map[string]any{
				"name": "Update the version", "kind": "yaml", "scmid": "origin",
				"spec": map[string]any{"file": "version.yaml", "key": "$.version", "value": version},
			},
		},
		"actions": map[string]any{"request": action},
	}
}
