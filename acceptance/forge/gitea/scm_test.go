//go:build acceptance

package gitea_test

import (
	"bytes"
	"crypto/rand"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func testSCM(t *testing.T, s *server) {
	t.Run("private_branch_read_and_direct_push", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const input, copied = "scm-input.txt", "scm-copy.txt"
		const stableValue = "value from stable\n"
		r.Stage(t, "main", input, "value from main\n")
		r.Stage(t, r.Base, input, stableValue)
		before := r.Refs(t)

		// Keep the shallow reader separate from the full-history writer: the
		// direct push must be fast-forward, not rely on forcing a shallow push.
		readDir := filepath.Join(c.Dir, "shallow")
		readSpec := scmTestSpec(r, forgeMap{"directory": readDir, "singlebranch": true, "depth": 1, "workingbranch": false})
		writeSpec := scmTestSpec(r, forgeMap{
			"workingbranch": false, "user": "Forge Acceptance", "email": "forge@example.invalid",
			"commitmessage": forgeMap{"type": "fix", "scope": "scm"},
		})
		manifest := cli.Manifest(t, forgeMap{
			"name": "Copy a private file to the configured branch", "pipelineid": "direct",
			"scms":    forgeMap{"read": forgeNode("gitea", readSpec), "write": forgeNode("gitea", writeSpec)},
			"sources": forgeMap{"input": resourceFileNode(input, "", forgeMap{"scmid": "read"})},
			"conditions": forgeMap{
				"stable": resourceFileNode(input, stableValue, forgeMap{"scmid": "read", "disablesourceinput": true}),
			},
			"targets": forgeMap{"copy": forceFileNode(copied, "", forgeMap{
				"name": "Copy branch input", "scmid": "write", "sourceid": "input",
			})},
		})
		cli.RequireOutput(t, c.PipelineOK(t, "diff", manifest), "Copy branch input")
		require.Equal(t, before, r.Refs(t), "diff must not publish a direct update")

		c.PipelineOK(t, "apply", manifest)

		baseRef := "refs/heads/" + r.Base
		after := r.Refs(t)
		assertSCMRefChange(t, before, after, baseRef)
		r.Git(t, "fetch", "origin", baseRef)
		require.Equal(t, before[baseRef]+"\n", r.Git(t, "rev-parse", "FETCH_HEAD^"), "direct push must preserve the old base")
		require.Equal(t, stableValue, r.Git(t, "show", "FETCH_HEAD:"+copied), "the pushed file must come from the configured branch")
		require.Equal(t, "Forge Acceptance <forge@example.invalid>|fix(scm): Copy branch input\n",
			r.Git(t, "log", "-1", "--format=%an <%ae>|%s", "FETCH_HEAD"))
		require.Equal(t, "1\n", r.Git(t, "-C", readDir, "rev-list", "--count", "HEAD"), "depth: 1 limits the checked-out history")
		refs := strings.Fields(r.Git(t, "-C", readDir, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes"))
		require.Contains(t, refs, baseRef)
		for _, ref := range refs {
			require.Contains(t, []string{baseRef, "refs/remotes/origin/" + r.Base}, ref, "singlebranch fetched an unrelated ref")
		}
		c.PipelineOK(t, "apply", manifest, "--clean-git-branches")
		require.Equal(t, after, r.Refs(t), "an identical apply must not advance or prune the configured branch")
	})

	t.Run("submodule_checkout", func(t *testing.T) { testSCMSubmodules(t, s) })

	t.Run("custom_working_branch_existing_only", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const id, prefix, separator = "refresh", "automation", "--"
		const file = "scm-target.txt"
		branch := strings.Join([]string{prefix, r.Base, id}, separator)
		workingRef := "refs/heads/" + branch
		baseRef := "refs/heads/" + r.Base
		r.Stage(t, r.Base, "stable-only.txt", "not on main\n")
		before := r.Refs(t)

		manifest := func(dir, value string) string {
			spec := scmTestSpec(r, forgeMap{
				"directory": filepath.Join(dir, "checkout"), "singlebranch": true,
				"workingbranchprefix": prefix, "workingbranchseparator": separator,
			})
			return cli.Manifest(t, forgeMap{
				"name": "Refresh a custom working branch", "pipelineid": id,
				"scms":    scmNodes(spec),
				"targets": forgeMap{"refresh": scmFileTarget("Refresh working file", file, value)},
			})
		}
		// A skipped apply can leave local changes; isolate it from the publish.
		skipCLI := newCLI(t, s)
		out := skipCLI.PipelineOK(t, "apply", manifest(skipCLI.Dir, "first\n"), "--existing-only")
		cli.RequireOutput(t, out, "existing-only mode")
		require.Equal(t, before, r.Refs(t), "existing-only must not publish a missing branch")

		c.PipelineOK(t, "apply", manifest(c.Dir, "first\n"))
		assertSCMRefChange(t, before, r.Refs(t), workingRef)
		r.Git(t, "fetch", "origin", workingRef)
		require.Equal(t, before[baseRef]+"\n", r.Git(t, "rev-parse", "FETCH_HEAD^"), "working branch must start at the configured base")
		require.Equal(t, "first\n", r.Git(t, "show", "FETCH_HEAD:"+file))

		// Advance the remote independently, leaving the candidate's cached ref
		// stale. An existing-only update must preserve the remote commit.
		r.Stage(t, branch, "remote-marker.txt", "added independently\n")
		advanced := r.Refs(t)
		c.PipelineOK(t, "apply", manifest(c.Dir, "second\n"), "--existing-only")
		assertSCMRefChange(t, advanced, r.Refs(t), workingRef)
		r.Git(t, "fetch", "origin", workingRef)
		require.Equal(t, advanced[workingRef]+"\n", r.Git(t, "rev-parse", "FETCH_HEAD^"), "must preserve the independently advanced tip")
		require.Equal(t, "second\n", r.Git(t, "show", "FETCH_HEAD:"+file))
		require.Equal(t, "added independently\n", r.Git(t, "show", "FETCH_HEAD:remote-marker.txt"))
	})

	t.Run("managed_history", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		r.Stage(t, r.Base, "stable-only.txt", "configured branch tip\n")
		before := r.Refs(t)
		base := before["refs/heads/"+r.Base]
		const id = "managed-history"
		workingRef := "refs/heads/updatecli_" + r.Base + "_" + id

		// Allow rewriting only the Updatecli working branch.
		spec := scmTestSpec(r, forgeMap{"force": true, "commitmessage": forgeMap{"squash": true}})
		apply := func(file, content string) {
			manifest := cli.Manifest(t, forgeMap{
				"name": "Squash the managed working branch", "pipelineid": id,
				"scms":    scmNodes(spec),
				"targets": forgeMap{"write": scmFileTarget("Write "+file, file, content)},
			})
			c.PipelineOK(t, "apply", manifest)
		}

		apply("first.txt", "first apply\n")
		first := r.Refs(t)
		assertSCMRefChange(t, before, first, workingRef)
		r.Git(t, "fetch", "origin", workingRef)
		require.Equal(t, base+"\n", r.Git(t, "show", "-s", "--format=%P", first[workingRef]))
		require.Equal(t, "first apply\n", r.Git(t, "show", first[workingRef]+":first.txt"))

		// The second apply does not target first.txt; its bytes must survive the rewrite.
		apply("second.txt", "second apply\n")
		second := r.Refs(t)
		assertSCMRefChange(t, first, second, workingRef)
		r.Git(t, "fetch", "origin", workingRef)
		tip := second[workingRef]
		require.Equal(t, base+"\n", r.Git(t, "show", "-s", "--format=%P", tip),
			"the replacement commit must have stable as its only parent, not the first working tip")
		require.Equal(t, "first apply\n", r.Git(t, "show", tip+":first.txt"))
		require.Equal(t, "second apply\n", r.Git(t, "show", tip+":second.txt"))

		apply("second.txt", "second apply\n")
		require.Equal(t, second, r.Refs(t), "an unchanged target must not rewrite squashed history")
	})

	t.Run("signed_commit", func(t *testing.T) { testSCMSignedCommit(t, s) })

	t.Run("direct_push_requires_explicit_force_setting", func(t *testing.T) {
		r := s.newRepo(t)
		spec := scmTestSpec(r, forgeMap{"workingbranch": false})
		delete(spec, "force")
		before := r.Refs(t)
		manifest := scmFileManifest(t, spec, "explicit-force", "version-note.txt", "requires explicit push settings\n")
		result := newCLI(t, s).Pipeline(t, "apply", manifest)
		require.NotZero(t, result.ExitCode, "workingbranch: false with an implicit force push must be rejected")
		cli.RequireOutput(t, result.Output, "unclear configuration, better safe than sorry")
		require.Equal(t, before, r.Refs(t))
	})

	t.Run("prune_current_working_branch_only", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const id, file = "prune", "prune.txt"
		branch := "updatecli_" + r.Base + "_" + id
		workingRef := "refs/heads/" + branch
		r.Stage(t, r.Base, file, "current\n")
		r.Git(t, "push", "origin", "refs/heads/"+r.Base+":"+workingRef, "refs/heads/main:refs/tags/"+branch)
		before := r.Refs(t)
		require.Equal(t, before["refs/heads/"+r.Base], before[workingRef])

		manifest := func(value string) string {
			return scmFileManifest(t, scmTestSpec(r), id, file, value)
		}
		c.PipelineOK(t, "apply", manifest("current\n"))
		require.Equal(t, before, r.Refs(t), "branch pruning must be opt-in")
		c.PipelineOK(t, "apply", manifest("current\n"), "--clean-git-branches")
		delete(before, workingRef)
		require.Equal(t, before, r.Refs(t), "pruning must remove only the managed head, not the same-name tag or base")

		c.PipelineOK(t, "apply", manifest("changed\n"), "--clean-git-branches")
		withChange := r.Refs(t)
		assertSCMRefChange(t, before, withChange, workingRef)
		require.NotEqual(t, withChange["refs/heads/"+r.Base], withChange[workingRef])
		c.PipelineOK(t, "apply", manifest("changed\n"), "--clean-git-branches")
		require.Equal(t, withChange, r.Refs(t), "pruning must keep the working branch while its commit differs from the base")
	})

	t.Run("native_gitbranch_target", func(t *testing.T) { testNativeGitBranchTarget(t, s) })
	t.Run("native_ref_sources_and_conditions", func(t *testing.T) { testNativeRefReads(t, s) })
	t.Run("native_gittag_target", func(t *testing.T) { testNativeGitTagTarget(t, s) })
}

func testSCMSubmodules(t *testing.T, s *server) {
	r, sub := s.newRepo(t), s.newRepo(t)
	const input, moduleDir = "module-input.txt", "fixture-module"
	const value = "from the pinned submodule commit\n"
	sub.Stage(t, sub.Base, input, value)
	subBefore := sub.Refs(t)
	subCommit := subBefore["refs/heads/"+sub.Base]
	subURL := s.url + "/" + s.owner + "/" + sub.name + ".git"
	r.Write(t, ".gitmodules", "[submodule \""+moduleDir+"\"]\n\tpath = "+moduleDir+"\n\turl = "+subURL+"\n")
	// Stage the submodule pin without cloning it in the fixture's worktree.
	// The candidate must download the pinned object over authenticated HTTP.
	r.Git(t, "update-index", "--add", "--cacheinfo", "160000,"+subCommit+","+moduleDir)
	r.Git(t, "add", "--", ".gitmodules")
	r.Git(t, "commit", "-m", "Pin a local private submodule")
	r.Git(t, "push", "origin", "HEAD:refs/heads/"+r.Base)
	before := r.Refs(t)

	for _, tc := range []struct {
		name     string
		disabled bool
	}{{"default", false}, {"disabled", true}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLI(t, s)
			dir := filepath.Join(c.Dir, "checkout")
			spec := scmTestSpec(r, forgeMap{"directory": dir, "workingbranch": false, "singlebranch": true})
			if tc.disabled {
				spec["submodules"] = false
			}
			manifest := cli.Manifest(t, forgeMap{
				"name": "Check out a pinned submodule", "scms": scmNodes(spec),
				"sources": forgeMap{"parent": resourceFileNode("version.yaml", "", forgeMap{"scmid": "repo"})},
			})
			c.PipelineOK(t, "diff", manifest)
			require.FileExists(t, filepath.Join(dir, ".gitmodules"))
			modulePath := filepath.Join(dir, moduleDir)
			if tc.disabled {
				require.NoFileExists(t, filepath.Join(modulePath, input))
			} else {
				assertResourceFile(t, modulePath, input, value)
				require.Equal(t, subCommit+"\n", r.Git(t, "-C", modulePath, "rev-parse", "HEAD"))
			}
		})
	}
	require.Equal(t, before, r.Refs(t), "cloning must not change the parent repository")
	require.Equal(t, subBefore, sub.Refs(t), "cloning must not change the submodule repository")
}

func testSCMSignedCommit(t *testing.T, s *server) {
	r := s.newRepo(t)
	const author, email = "Acceptance Signer", "scm-signer@example.invalid"
	const id, file, content = "signed-commit", "signed.txt", "content from the signed update\n"
	key, err := openpgp.NewEntity(author, "", email, nil)
	require.NoError(t, err)
	passphrase := rand.Text()
	require.NoError(t, key.EncryptPrivateKeys([]byte(passphrase), nil))
	armoredKey := func(keyType string) string {
		t.Helper()
		var buf bytes.Buffer
		w, err := armor.Encode(&buf, keyType, nil)
		require.NoError(t, err)
		if keyType == openpgp.PrivateKeyType {
			require.NoError(t, key.SerializePrivateWithoutSigning(w, nil))
		} else {
			require.NoError(t, key.Serialize(w))
		}
		require.NoError(t, w.Close())
		return buf.String()
	}
	private, public := armoredKey(openpgp.PrivateKeyType), armoredKey(openpgp.PublicKeyType)

	// Keep the disposable secret out of the YAML and never report CLI output.
	c := cli.NewWithCredentials(t, s.owner, s.token, "UPDATECLI_GITEA_USERNAME="+s.owner, "UPDATECLI_GITEA_TOKEN="+s.token,
		"FORGE_SCM_SIGNING_KEY="+private, "FORGE_SCM_SIGNING_PASSPHRASE="+passphrase)
	spec := scmTestSpec(r, forgeMap{
		"user": author, "email": email,
		"commitmessage": forgeMap{"type": "fix", "scope": "signing", "body": "Verify the published commit.", "hidecredit": true},
		"gpg": forgeMap{
			"signingkey": "acceptance-signing-key",
			"passphrase": `{{ requiredEnv "FORGE_SCM_SIGNING_PASSPHRASE" }}`,
		},
	})
	// Render a quoted YAML scalar to preserve the armor's newlines.
	manifest := strings.Replace(scmFileManifest(t, spec, id, file, content),
		"signingkey: acceptance-signing-key", `signingkey: {{ printf "%q" (requiredEnv "FORGE_SCM_SIGNING_KEY") }}`, 1)
	before := r.Refs(t)
	out := c.Pipeline(t, "apply", manifest)
	require.Zero(t, out.ExitCode, "publishing a signed SCM commit must succeed")

	workingRef := "refs/heads/updatecli_" + r.Base + "_" + id
	after := r.Refs(t)
	assertSCMRefChange(t, before, after, workingRef)
	tip := after[workingRef]
	r.Git(t, "fetch", "origin", workingRef)
	base := before["refs/heads/"+r.Base]
	require.Equal(t, base+"\n", r.Git(t, "show", "-s", "--format=%P", tip))
	require.Equal(t, content, r.Git(t, "show", tip+":"+file))
	require.Equal(t, author+" <"+email+">|fix(signing): Write "+file+"\n\nVerify the published commit.\n",
		r.Git(t, "show", "-s", "--format=%an <%ae>|%B", tip))

	// Verify the bytes from native Git, not a badge or signature header.
	readCommit := func(sha string) *object.Commit {
		t.Helper()
		raw := new(plumbing.MemoryObject)
		raw.SetType(plumbing.CommitObject)
		_, err := raw.Write([]byte(r.Git(t, "cat-file", "commit", sha)))
		require.NoError(t, err)
		commit, err := object.DecodeCommit(nil, raw)
		require.NoError(t, err)
		require.Equal(t, sha, commit.Hash.String())
		return commit
	}
	_, err = readCommit(base).Verify(public)
	require.Error(t, err, "the unsigned fixture commit must fail verification")
	signer, err := readCommit(tip).Verify(public)
	require.NoError(t, err, "unsigned commits and signatures from any other key must fail verification")
	require.Equal(t, key.PrimaryKey.Fingerprint, signer.PrimaryKey.Fingerprint)
}

func scmFileManifest(t *testing.T, spec forgeMap, id, file, content string) string {
	t.Helper()
	return cli.Manifest(t, forgeMap{
		"name": "Write a file with the configured SCM", "pipelineid": id,
		"scms":    scmNodes(spec),
		"targets": forgeMap{"write": scmFileTarget("Write "+file, file, content)},
	})
}

func scmTestSpec(r *repository, fields ...forgeMap) forgeMap {
	spec := forgeMap{"url": r.server.url, "owner": r.server.owner, "repository": r.name, "branch": r.Base, "force": false}
	for _, extra := range fields {
		maps.Copy(spec, extra)
	}
	return spec
}

func scmNodes(spec forgeMap) forgeMap {
	return forgeMap{"repo": forgeNode("gitea", spec)}
}

func scmFileTarget(name, file, content string) forgeMap {
	return forceFileNode(file, content, forgeMap{"name": name, "scmid": "repo"})
}

func assertSCMRefChange(t *testing.T, before, after map[string]string, ref string) {
	t.Helper()
	require.NotEmpty(t, after[ref], "expected ref %s in %v", ref, after)
	require.NotEqual(t, before[ref], after[ref], "expected %s to advance", ref)
	want := maps.Clone(before)
	want[ref] = after[ref]
	require.Equal(t, want, after, "only %s should change", ref)
}
