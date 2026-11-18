//go:build acceptance

package forge

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Each case gets a fresh repository on the same running forge.
type publicationProvider interface {
	newPublication(t *testing.T) publicationFixture
}

type publicationFixture struct {
	repo   *gitRepository
	repoID int64
	driver publicationDriver
}

type publicationInput struct {
	version, title string
}

type publicationDriver interface {
	// run invokes the real CLI for the given input and requires a successful exit.
	run(t *testing.T, mode string, input publicationInput) string
	// requests includes all states so closing and recreating a request cannot pass.
	requests(t *testing.T) []publicationRequest
}

type publicationRequest struct {
	id, number, headRepoID, baseRepoID int64
	open                               bool
	head, base, headSHA, baseSHA       string
	title                              string
}

const publicationID = "publish"

func publicationHead(r *gitRepository) string {
	return "updatecli_" + r.base + "_" + publicationID
}

var publicationCases = []capability[publicationFixture]{
	{"lifecycle", testPublicationLifecycle},
	{"already_current", testPublicationAlreadyCurrent},
}

// Enroll a provider in publish_update as a whole, never in individual cases.
func publishUpdate[F publicationProvider]() capability[F] {
	return capability[F]{"publish_update", func(t *testing.T, provider F) {
		if len(publicationCases) == 0 {
			t.Fatal("no publication cases enrolled")
		}
		for _, c := range publicationCases {
			runRequired(t, c.name, func(t *testing.T) {
				fixture := provider.newPublication(t)
				seedPublication(t, fixture.repo)
				c.run(t, fixture)
			})
		}
	}}
}

// seedPublication gives the non-default base its own tree and adds refs the
// pipeline must not touch.
func seedPublication(t *testing.T, r *gitRepository) {
	t.Helper()
	r.stage(t, r.base, "stable-only.txt", "not part of the update\n")
	refs := r.refs(t)
	base := refs["refs/heads/"+r.base]
	require.NotEmpty(t, base)
	require.NotEmpty(t, refs["refs/heads/main"])
	require.NotEqual(t, refs["refs/heads/main"], base, "the configured base must differ from main")
	r.git(t, "push", "origin", base+":refs/heads/unrelated", base+":refs/tags/publish-anchor")
}

func (f publicationFixture) initialRefs(t *testing.T) map[string]string {
	t.Helper()
	before := f.repo.refs(t)
	head := publicationHead(f.repo)
	require.NotEmpty(t, before["refs/heads/"+f.repo.base])
	require.NotContains(t, before, "refs/heads/"+head)
	require.Empty(t, f.driver.requests(t), "the publication fixture must start without requests")
	return before
}

func testPublicationLifecycle(t *testing.T, f publicationFixture) {
	first := publicationInput{version: "v1.1.0", title: "Bump the fixture to v1.1.0"}
	next := publicationInput{version: "v1.2.0", title: "Bump the fixture to v1.2.0"}
	r, d := f.repo, f.driver
	head := publicationHead(r)
	baseRef := "refs/heads/" + r.base
	before := f.initialRefs(t)

	output := d.run(t, "diff", first)
	requireOutput(t, output, "version.yaml", first.version)
	require.Equal(t, before, r.refs(t), "diff must not write refs")
	require.Empty(t, d.requests(t), "diff must not create a request in any state")

	d.run(t, "apply", first)
	requests := d.requests(t)
	require.Len(t, requests, 1, "first apply must open exactly one request")
	opened := requests[0]
	require.Positive(t, opened.id)
	require.Positive(t, opened.number)
	require.Equal(t, first.title, opened.title, "title of the published request")
	afterOpening := assertPublicationHead(t, r, opened, f.repoID, head, before, first.version)
	require.Equal(t, before[baseRef], strings.TrimSpace(r.git(t, "rev-parse", opened.headSHA+"^")),
		"first commit must start at the configured base")

	d.run(t, "apply", first)
	require.Equal(t, afterOpening, r.refs(t), "an identical apply must not add a commit or move any ref")
	requests = d.requests(t)
	require.Len(t, requests, 1, "an identical apply must not create or remove a request")
	require.Equal(t, opened, requests[0], "an identical apply must leave the original request, refs and title unchanged")

	d.run(t, "apply", next)
	requests = d.requests(t)
	require.Len(t, requests, 1, "changing input must update the existing request, not replace it")
	updated := requests[0]
	require.Equal(t, opened.id, updated.id)
	require.Equal(t, opened.number, updated.number)
	require.NotEqual(t, opened.headSHA, updated.headSHA, "changing input must publish a new head")
	// A push can advance the SHA without editing the request itself.
	require.Equal(t, next.title, updated.title, "title must change on the original request")
	assertPublicationHead(t, r, updated, f.repoID, head, before, next.version)
}

func testPublicationAlreadyCurrent(t *testing.T, f publicationFixture) {
	// Only the configured base is up to date; targeting main would publish.
	input := publicationInput{version: "v1.1.0", title: "Do not publish an already current fixture"}
	f.repo.stage(t, f.repo.base, "version.yaml", "version: "+input.version+"\nkeep: untouched\n")
	before := f.initialRefs(t)

	output := f.driver.run(t, "apply", input)
	requireOutput(t, output, "version.yaml", input.version)
	require.Equal(t, before, f.repo.refs(t), "an already current target must not write refs")
	require.Empty(t, f.driver.requests(t), "an already current target must not create a request in any state")
}

func assertPublicationHead(t *testing.T, r *gitRepository, req publicationRequest, repoID int64, head string, before map[string]string, version string) map[string]string {
	t.Helper()
	require.True(t, req.open, "the request must be open")
	require.Equal(t, head, req.head)
	require.Equal(t, r.base, req.base)
	require.Equal(t, repoID, req.headRepoID)
	require.Equal(t, repoID, req.baseRepoID)
	require.NotEmpty(t, req.headSHA)
	baseSHA := before["refs/heads/"+r.base]
	require.Equal(t, baseSHA, req.baseSHA)

	headRef := "refs/heads/" + head
	wantRefs := maps.Clone(before)
	wantRefs[headRef] = req.headSHA
	refs := r.refs(t)
	require.Equal(t, wantRefs, refs, "the request head must match native Git, with all sibling refs untouched")

	r.git(t, "fetch", "origin", headRef)
	r.git(t, "merge-base", "--is-ancestor", baseSHA, req.headSHA)
	require.Equal(t, "version.yaml", strings.TrimSpace(r.git(t, "diff", "--name-only", baseSHA, req.headSHA)))
	require.Equal(t, "version: "+version+"\nkeep: untouched\n", r.git(t, "show", req.headSHA+":version.yaml"))
	return refs
}
