//go:build acceptance

package forge

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	firstPublicationVersion   = "v1.1.0"
	changedPublicationVersion = "v1.2.0"
)

type publicationRef struct {
	repositoryID, branch, sha string
}

type publicationRequest struct {
	id         string
	open       bool
	head, base publicationRef
}

// publicationTarget connects the forge's request observations to native Git.
// refs returns remote branch and tag refs by full name, and git runs native Git.
type publicationTarget struct {
	repositoryID, headBranch, baseBranch string
	refs                                 func(*testing.T) map[string]string
	git                                  func(*testing.T, ...string) string
}

type publicationStep string

const (
	publicationOpened    publicationStep = "opened"
	publicationUnchanged publicationStep = "unchanged"
	publicationUpdated   publicationStep = "updated"
)

// publicationFixture adapts a fresh repository seeded with version.yaml and
// sibling refs to the common CLI case. manifest sets the supplied version, run
// invokes the candidate and returns its output on success, and requests observes
// all states, not only open requests. check optionally verifies provider metadata.
type publicationFixture struct {
	publicationTarget
	manifest func(version string) string
	run      func(t *testing.T, mode, manifest string) string
	requests func(*testing.T) []publicationRequest
	check    func(*testing.T, publicationStep)
}

func assertPublicationLifecycle(t *testing.T, f publicationFixture) {
	t.Helper()
	before := f.refs(t)
	base := before["refs/heads/"+f.baseBranch]
	require.NotEmpty(t, base)
	require.NotContains(t, before, "refs/heads/"+f.headBranch)
	first := f.manifest(firstPublicationVersion)
	output := f.run(t, "diff", first)
	require.Contains(t, output, "version.yaml")
	require.Contains(t, output, firstPublicationVersion)
	require.Equal(t, before, f.refs(t), "diff must not write refs")
	require.Empty(t, f.requests(t), "diff must not create even a closed request")

	f.run(t, "apply", first)
	requests := f.requests(t)
	require.Len(t, requests, 1)
	opened := requests[0]
	require.NotEmpty(t, opened.id)
	f.assertHead(t, opened, before, firstPublicationVersion)
	require.Equal(t, base, strings.TrimSpace(f.git(t, "rev-parse", opened.head.sha+"^")),
		"first commit must start at the configured base")
	if f.check != nil {
		f.check(t, publicationOpened)
	}

	afterOpening := f.refs(t)
	f.run(t, "apply", first)
	require.Equal(t, afterOpening, f.refs(t), "an identical apply must not add a commit or move any ref")
	requests = f.requests(t)
	require.Len(t, requests, 1)
	require.Equal(t, opened, requests[0], "an identical apply must keep the same open request, head, and base")
	if f.check != nil {
		f.check(t, publicationUnchanged)
	}

	f.run(t, "apply", f.manifest(changedPublicationVersion))
	requests = f.requests(t)
	require.Len(t, requests, 1, "changing input must update the existing request, not replace it")
	updated := requests[0]
	require.Equal(t, opened.id, updated.id)
	f.assertHead(t, updated, before, changedPublicationVersion)
	require.NotEqual(t, opened.head.sha, updated.head.sha)
	if f.check != nil {
		f.check(t, publicationUpdated)
	}
}

func (p publicationTarget) assertHead(t *testing.T, req publicationRequest, initialRefs map[string]string, value string) {
	t.Helper()
	require.True(t, req.open, "request must be open")
	require.Equal(t, p.headBranch, req.head.branch)
	require.Equal(t, p.baseBranch, req.base.branch)
	require.Equal(t, p.repositoryID, req.head.repositoryID)
	require.Equal(t, p.repositoryID, req.base.repositoryID)

	headRef := "refs/heads/" + p.headBranch
	base := initialRefs["refs/heads/"+p.baseBranch]
	require.NotEmpty(t, req.head.sha)
	require.Equal(t, base, req.base.sha)
	wantRefs := maps.Clone(initialRefs)
	wantRefs[headRef] = req.head.sha
	require.Equal(t, wantRefs, p.refs(t), "the request head must match native Git, with all sibling refs untouched")

	p.git(t, "fetch", "origin", headRef)
	require.Equal(t, "version.yaml", strings.TrimSpace(p.git(t, "diff", "--name-only", base, req.head.sha)))
	require.Equal(t, "version: "+value+"\nkeep: untouched\n", p.git(t, "show", req.head.sha+":version.yaml"))
}
