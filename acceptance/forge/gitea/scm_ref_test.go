//go:build acceptance

package gitea_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func testNativeGitBranchTarget(t *testing.T, s *server) {
	r := s.newRepo(t)
	// The shared gitbranch target performs an anonymous checkout before the
	// authenticated SCM push. Make only this disposable fixture public so
	// the test isolates ref selection at push time.
	repo, resp, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, gitea.EditRepoOption{
		Private: new(false),
	})
	s.check(t, "make local fixture repository public", resp, err)
	require.False(t, repo.Private)

	const branch = "acceptance-new-head"
	const headRef, tagRef = "refs/heads/" + branch, "refs/tags/" + branch
	r.Stage(t, r.Base, "stable-only.txt", "configured branch tip\n")
	r.Git(t, "tag", "-a", "-m", "Existing tag, not the requested branch", branch, "refs/heads/main")
	r.Git(t, "push", "origin", tagRef)
	before := r.Refs(t)
	base := before["refs/heads/"+r.Base]
	require.NotEqual(t, before["refs/heads/main"], base)
	require.Contains(t, before, tagRef)
	require.NotContains(t, before, headRef)
	tagCommit := r.Git(t, "ls-remote", "origin", tagRef+"^{}")
	require.Equal(t, before["refs/heads/main"]+"\t"+tagRef+"^{}\n", tagCommit)

	spec := scmTestSpec(r, forgeMap{"workingbranch": false})
	manifest := nativeRefManifest(t, spec, "gitbranch", forgeMap{"branch": branch})
	c := newCLI(t, s)
	cli.RequireOutput(t, c.PipelineOK(t, "diff", manifest), `git branch "acceptance-new-head" should be created`)
	require.Equal(t, before, r.Refs(t), "preview must not write refs")

	// The resource pushes the branch itself, without a file commit.
	result := c.Pipeline(t, "apply", manifest, "--commit=false")
	published := r.Refs(t)
	assert.Zero(t, result.ExitCode, "gitbranch apply should succeed")
	assert.Equal(t, base, published[headRef], "apply exit %d: %s must point to the configured base", result.ExitCode, headRef)
	assertSCMRefChange(t, before, published, headRef)
	require.Equal(t, tagCommit, r.Git(t, "ls-remote", "origin", tagRef+"^{}"))

	// The runner uses a fresh clone: an unpublished local branch cannot make
	// this reapply report success.
	cli.RequireOutput(t, c.PipelineOK(t, "apply", manifest, "--commit=false"), `the git branch "acceptance-new-head" already exist`)
	require.Equal(t, published, r.Refs(t), "reapply must leave the branch, tag and other refs unchanged")
}

func testNativeGitTagTarget(t *testing.T, s *server) {
	r := s.newRepo(t)
	c := newCLI(t, s)
	const tag = "v1.1.0"
	const tagRef = "refs/tags/" + tag
	const message = "Tag from the SCM acceptance test"
	r.Stage(t, r.Base, "stable-only.txt", "this commit should be tagged\n")
	r.Git(t, "push", "origin", "main:refs/tags/v1.0.0", "refs/heads/main:refs/heads/"+tag)
	before := r.Refs(t)
	stable := before["refs/heads/"+r.Base]
	require.NotContains(t, before, tagRef)
	require.NotEqual(t, stable, before["refs/heads/"+tag])

	configureNativeTagger(t, c)
	spec := scmTestSpec(r, forgeMap{"workingbranch": false})
	manifest := nativeRefManifest(t, spec, "gittag", nativeTagSpec(tag, message))

	// A PAT alone exercises the SCM's username fallback on a private repo.
	preview := newCLIWithCredentials(t, "", s.token).Pipeline(t, "diff", manifest)
	require.Zero(t, preview.ExitCode)
	cli.RequireOutput(t, preview.Output, `git tag "v1.1.0" should be created`)
	require.Equal(t, before, r.Refs(t), "diff must not write remote refs")

	// The target publishes the tag itself; there are no files to commit.
	c.PipelineOK(t, "apply", manifest, "--commit=false")
	published := r.Refs(t)
	assertSCMRefChange(t, before, published, tagRef)
	peeled := tagRef + "^{}"
	require.Equal(t, stable+"\t"+peeled+"\n", r.Git(t, "ls-remote", "origin", peeled),
		"the annotated tag must peel to the configured base, not main")
	r.Git(t, "fetch", "origin", tagRef)
	tagObject := r.Git(t, "cat-file", "tag", published[tagRef])
	require.True(t, strings.HasSuffix(tagObject, "\n\n"+message+"\n"), "tag must contain the configured annotation")

	cli.RequireOutput(t, c.PipelineOK(t, "apply", manifest, "--commit=false"), `git tag "v1.1.0" already exists`)
	require.Equal(t, published, r.Refs(t), "reapply must neither replace the tag object nor move other refs")
}

func nativeTagSpec(tag, message string) forgeMap {
	return forgeMap{"versionfilter": forgeMap{"pattern": tag}, "message": message}
}

func nativeRefManifest(t *testing.T, scmSpec forgeMap, kind string, targetSpec forgeMap) string {
	t.Helper()
	const name = "Publish a native Git ref"
	return cli.Manifest(t, forgeMap{
		"name": name, "scms": scmNodes(scmSpec),
		"targets": forgeMap{"ref": forgeNode(kind, targetSpec, forgeMap{"name": name, "scmid": "repo"})},
	})
}

func configureNativeTagger(t *testing.T, c *cli.Runner) {
	t.Helper()
	// Annotated tags need a tagger; configure only the isolated candidate home.
	require.NoError(t, os.WriteFile(filepath.Join(c.Home, ".gitconfig"),
		[]byte("[user]\n\tname = Forge Acceptance\n\temail = forge@example.invalid\n"), 0o600))
}

func testNativeRefReads(t *testing.T, s *server) {
	r := s.newRepo(t)
	const shared, tagOnly = "refs-test/shared", "refs-test/tag-only"
	r.Stage(t, r.Base, "branch-only.txt", "configured branch tip\n")
	r.Git(t, "tag", "-a", "-m", "Tag pointing at main", shared, "refs/heads/main")
	r.Git(t, "tag", tagOnly, "refs/heads/main")
	r.Git(t, "push", "origin", "refs/heads/"+r.Base+":refs/heads/"+shared, "refs/tags/"+shared, "refs/tags/"+tagOnly)
	before := r.Refs(t)

	refCondition := func(kind, ref string, failWhen bool) forgeMap {
		node := forgeNode("git"+kind, forgeMap{kind: ref}, forgeMap{"scmid": "repo", "disablesourceinput": true})
		if failWhen {
			node["failwhen"] = true
		}
		return node
	}
	sources, conditions, targets := forgeMap{}, forgeMap{}, forgeMap{}
	for _, kind := range []string{"branch", "tag"} {
		sources[kind] = forgeNode("git"+kind, forgeMap{
			"key": "hash", "versionfilter": forgeMap{"kind": "latest", "pattern": shared},
		}, forgeMap{"scmid": "repo"})
		conditions[kind] = refCondition(kind, shared, false)
		targets[kind] = forceFileNode(kind+".txt", "", forgeMap{"sourceid": kind})
	}
	conditions["tag-is-not-branch"] = refCondition("branch", tagOnly, true)
	conditions["branch-is-not-tag"] = refCondition("tag", r.Base, true)
	spec := scmTestSpec(r, forgeMap{"workingbranch": false})
	manifest := cli.Manifest(t, forgeMap{
		"name": "Read same-named Git branches and tags from a private repository",
		"scms": scmNodes(spec), "sources": sources, "conditions": conditions, "targets": targets,
	})
	c := newCLI(t, s)
	c.PipelineOK(t, "apply", manifest)
	assertResourceFile(t, c.Dir, "branch.txt", before["refs/heads/"+r.Base])
	assertResourceFile(t, c.Dir, "tag.txt", before["refs/heads/main"])
	require.Equal(t, before, r.Refs(t), "native read resources must not write remote refs")
}
