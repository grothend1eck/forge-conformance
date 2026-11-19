//go:build acceptance

package forge

import (
	"os"
	"path/filepath"
	"testing"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testNativeGitBranchTarget(t *testing.T, s *server) {
	r := s.newRepo(t)
	// The shared gitbranch target performs an anonymous checkout before the
	// authenticated SCM push. Make only this disposable fixture public so
	// the test isolates ref selection at push time.
	repo, resp, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, gitea.EditRepoOption{
		Private: new(false),
	})
	if err != nil {
		s.apiFailure(t, "make local fixture repository public", resp, err)
	}
	require.False(t, repo.Private)

	const branch = "acceptance-new-head"
	const headRef, tagRef = "refs/heads/" + branch, "refs/tags/" + branch
	r.stage(t, r.base, "stable-only.txt", "configured branch tip\n")
	r.git(t, "tag", "-a", "-m", "Existing tag, not the requested branch", branch, "refs/heads/main")
	r.git(t, "push", "origin", tagRef)
	before := r.refs(t)
	base := before["refs/heads/"+r.base]
	require.NotEqual(t, before["refs/heads/main"], base)
	require.Contains(t, before, tagRef)
	require.NotContains(t, before, headRef)
	tagCommit := r.git(t, "ls-remote", "origin", tagRef+"^{}")
	require.Equal(t, before["refs/heads/main"]+"\t"+tagRef+"^{}\n", tagCommit)

	spec := scmTestSpec(r)
	spec["workingbranch"] = false
	manifest := nativeRefManifest(t, spec, "gitbranch", map[string]any{"branch": branch})
	c := newCLI(t, s)
	requireOutput(t, c.ok(t, "diff", manifest), `git branch "acceptance-new-head" should be created`)
	require.Equal(t, before, r.refs(t), "preview must not write refs")

	// The resource pushes the branch itself, without a file commit.
	result := c.run(t, "apply", manifest, "--commit=false")
	published := r.refs(t)
	assert.Zero(t, result.ExitCode, "gitbranch apply should succeed")
	assert.Equal(t, base, published[headRef], "apply exit %d: %s must point to the configured base", result.ExitCode, headRef)
	assertSCMRefChange(t, before, published, headRef)
	require.Equal(t, tagCommit, r.git(t, "ls-remote", "origin", tagRef+"^{}"))

	// The runner uses a fresh clone: an unpublished local branch cannot make
	// this reapply report success.
	requireOutput(t, c.ok(t, "apply", manifest, "--commit=false"), `the git branch "acceptance-new-head" already exist`)
	require.Equal(t, published, r.refs(t), "reapply must leave the branch, tag and other refs unchanged")
}

func nativeRefManifest(t *testing.T, scmSpec map[string]any, kind string, targetSpec map[string]any) string {
	t.Helper()
	return manifestYAML(t, map[string]any{
		"name": "Publish a native Git ref",
		"scms": map[string]any{"repo": map[string]any{"kind": "gitea", "spec": scmSpec}},
		"targets": map[string]any{
			"ref": map[string]any{"name": "Publish a native Git ref", "kind": kind, "scmid": "repo", "spec": targetSpec},
		},
	})
}

func configureNativeTagger(t *testing.T, c *forgeCLI) {
	t.Helper()
	// Annotated tags need a tagger; configure only the isolated candidate home.
	require.NoError(t, os.WriteFile(filepath.Join(c.runner.Home, ".gitconfig"),
		[]byte("[user]\n\tname = Forge Acceptance\n\temail = forge@example.invalid\n"), 0o600))
}

func testNativeRefReads(t *testing.T, s *server) {
	r := s.newRepo(t)
	const shared, tagOnly = "refs-test/shared", "refs-test/tag-only"
	r.stage(t, r.base, "branch-only.txt", "configured branch tip\n")
	r.git(t, "tag", "-a", "-m", "Tag pointing at main", shared, "refs/heads/main")
	r.git(t, "tag", tagOnly, "refs/heads/main")
	r.git(t, "push", "origin", "refs/heads/"+r.base+":refs/heads/"+shared, "refs/tags/"+shared, "refs/tags/"+tagOnly)
	before := r.refs(t)

	spec := scmTestSpec(r)
	spec["workingbranch"] = false
	sources, conditions, targets := map[string]any{}, map[string]any{}, map[string]any{}
	for _, kind := range []string{"branch", "tag"} {
		sources[kind] = map[string]any{
			"kind": "git" + kind, "scmid": "repo",
			"spec": map[string]any{"key": "hash", "versionfilter": map[string]string{"kind": "latest", "pattern": shared}},
		}
		conditions[kind] = map[string]any{
			"kind": "git" + kind, "scmid": "repo", "disablesourceinput": true,
			"spec": map[string]string{kind: shared},
		}
		targets[kind] = map[string]any{
			"kind": "file", "sourceid": kind,
			"spec": map[string]any{"file": kind + ".txt", "forcecreate": true},
		}
	}
	conditions["tag-is-not-branch"] = map[string]any{
		"kind": "gitbranch", "scmid": "repo", "disablesourceinput": true, "failwhen": true,
		"spec": map[string]string{"branch": tagOnly},
	}
	conditions["branch-is-not-tag"] = map[string]any{
		"kind": "gittag", "scmid": "repo", "disablesourceinput": true, "failwhen": true,
		"spec": map[string]string{"tag": r.base},
	}
	manifest := manifestYAML(t, map[string]any{
		"name":       "Read same-named Git branches and tags from a private repository",
		"scms":       map[string]any{"repo": map[string]any{"kind": "gitea", "spec": spec}},
		"sources":    sources,
		"conditions": conditions,
		"targets":    targets,
	})
	c := newCLI(t, s)
	c.ok(t, "apply", manifest)
	assertHash := func(file, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(c.runner.Dir, file))
		require.NoError(t, err)
		require.Equal(t, want, string(got))
	}
	assertHash("branch.txt", before["refs/heads/"+r.base])
	assertHash("tag.txt", before["refs/heads/main"])
	require.Equal(t, before, r.refs(t), "native read resources must not write remote refs")
}
