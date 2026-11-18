//go:build acceptance

package forge

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type publicationDriver interface {
	// run invokes the real CLI for the given input and requires a successful exit.
	run(t *testing.T, mode, version string) string
	// requests includes all states so closing and recreating a request cannot pass.
	requests(t *testing.T) []publicationRequest
}

type publicationRequest struct {
	id, number, headRepoID, baseRepoID int64
	open                               bool
	head, base, headSHA, baseSHA       string
}

type publication struct {
	repo         *gitRepository
	driver       publicationDriver
	repoID       int64
	head         string
	firstVersion string
	before       map[string]string
	afterOpening map[string]string
	opened       publicationRequest
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

// openPublication previews and publishes the first version on a fixture whose
// configured base has a distinct tree from the default branch.
func openPublication(t *testing.T, r *gitRepository, d publicationDriver, repoID int64, head, firstVersion string) publication {
	t.Helper()
	before := r.refs(t)
	baseRef := "refs/heads/" + r.base
	require.NotEmpty(t, before[baseRef])
	require.NotContains(t, before, "refs/heads/"+head)
	require.Empty(t, d.requests(t), "the publication fixture must start without requests")

	output := d.run(t, "diff", firstVersion)
	requireOutput(t, output, "version.yaml", firstVersion)
	require.Equal(t, before, r.refs(t), "diff must not write refs")
	require.Empty(t, d.requests(t), "diff must not create a request in any state")

	d.run(t, "apply", firstVersion)
	requests := d.requests(t)
	require.Len(t, requests, 1, "first apply must open exactly one request")
	opened := requests[0]
	require.Positive(t, opened.id)
	require.Positive(t, opened.number)
	afterOpening := assertPublicationHead(t, r, opened, repoID, head, before, firstVersion)
	require.Equal(t, before[baseRef], strings.TrimSpace(r.git(t, "rev-parse", opened.headSHA+"^")),
		"first commit must start at the configured base")

	return publication{
		repo: r, driver: d, repoID: repoID, head: head, firstVersion: firstVersion,
		before: before, afterOpening: afterOpening, opened: opened,
	}
}

func (p publication) reapply(t *testing.T) publicationRequest {
	t.Helper()
	p.driver.run(t, "apply", p.firstVersion)
	require.Equal(t, p.afterOpening, p.repo.refs(t), "an identical apply must not add a commit or move any ref")
	requests := p.driver.requests(t)
	require.Len(t, requests, 1, "an identical apply must not create or remove a request")
	unchanged := requests[0]
	require.Equal(t, p.opened, unchanged, "an identical apply must leave the same request open at the same head and base")
	return unchanged
}

func (p publication) update(t *testing.T, version string) publicationRequest {
	t.Helper()
	p.driver.run(t, "apply", version)
	requests := p.driver.requests(t)
	require.Len(t, requests, 1, "changing input must update the existing request, not replace it")
	updated := requests[0]
	require.Equal(t, p.opened.id, updated.id)
	require.Equal(t, p.opened.number, updated.number)
	require.NotEqual(t, p.opened.headSHA, updated.headSHA, "changing input must publish a new head")
	assertPublicationHead(t, p.repo, updated, p.repoID, p.head, p.before, version)
	return updated
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
	require.Equal(t, "version.yaml", strings.TrimSpace(r.git(t, "diff", "--name-only", baseSHA, req.headSHA)))
	require.Equal(t, "version: "+version+"\nkeep: untouched\n", r.git(t, "show", req.headSHA+":version.yaml"))
	return refs
}
