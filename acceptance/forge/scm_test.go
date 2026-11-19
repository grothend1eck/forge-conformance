//go:build acceptance

package forge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testSCM(t *testing.T, s *server) {
	runRequired(t, "private_branch_read_and_direct_push", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const input, copied = "scm-input.txt", "scm-copy.txt"
		const stableValue = "value from stable\n"
		r.stage(t, "main", input, "value from main\n")
		r.stage(t, r.base, input, stableValue)
		before := r.refs(t)

		// Keep the shallow reader separate from the full-history writer: the
		// direct push must be fast-forward, not rely on forcing a shallow push.
		readDir := filepath.Join(c.runner.Dir, "shallow")
		readSpec := scmTestSpec(r)
		readSpec["directory"] = readDir
		readSpec["singlebranch"] = true
		readSpec["depth"] = 1
		readSpec["workingbranch"] = false
		writeSpec := scmTestSpec(r)
		writeSpec["workingbranch"] = false
		writeSpec["user"] = "Forge Acceptance"
		writeSpec["email"] = "forge@example.invalid"
		writeSpec["commitmessage"] = map[string]any{"type": "fix", "scope": "scm"}
		manifest := manifestYAML(t, map[string]any{
			"name":       "Copy a private file to the configured branch",
			"pipelineid": "direct",
			"scms": map[string]any{
				"read":  map[string]any{"kind": "gitea", "spec": readSpec},
				"write": map[string]any{"kind": "gitea", "spec": writeSpec},
			},
			"sources": map[string]any{
				"input": map[string]any{"kind": "file", "scmid": "read", "spec": map[string]any{"file": input}},
			},
			"conditions": map[string]any{
				"stable": map[string]any{
					"kind": "file", "scmid": "read", "disablesourceinput": true,
					"spec": map[string]any{"file": input, "content": stableValue},
				},
			},
			"targets": map[string]any{
				"copy": map[string]any{
					"name": "Copy branch input", "kind": "file", "scmid": "write", "sourceid": "input",
					"spec": map[string]any{"file": copied, "forcecreate": true},
				},
			},
		})
		requireOutput(t, c.ok(t, "diff", manifest), "Copy branch input")
		require.Equal(t, before, r.refs(t), "diff must not publish a direct update")

		c.ok(t, "apply", manifest)

		baseRef := "refs/heads/" + r.base
		after := r.refs(t)
		assertSCMRefChange(t, before, after, baseRef)
		r.git(t, "fetch", "origin", baseRef)
		require.Equal(t, before[baseRef]+"\n", r.git(t, "rev-parse", "FETCH_HEAD^"), "direct push must preserve the old base")
		require.Equal(t, stableValue, r.git(t, "show", "FETCH_HEAD:"+copied), "the pushed file must come from the configured branch")
		require.Equal(t, "Forge Acceptance <forge@example.invalid>|fix(scm): Copy branch input\n",
			r.git(t, "log", "-1", "--format=%an <%ae>|%s", "FETCH_HEAD"))
		require.Equal(t, "1\n", r.git(t, "-C", readDir, "rev-list", "--count", "HEAD"), "depth: 1 limits the checked-out history")
		refs := strings.Fields(r.git(t, "-C", readDir, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes"))
		require.Contains(t, refs, baseRef)
		for _, ref := range refs {
			require.Contains(t, []string{baseRef, "refs/remotes/origin/" + r.base}, ref, "singlebranch fetched an unrelated ref")
		}
		c.ok(t, "apply", manifest, "--clean-git-branches")
		require.Equal(t, after, r.refs(t), "an identical apply must not advance or prune the configured branch")
	})

	runRequired(t, "submodule_checkout", func(t *testing.T) {
		testSCMSubmodules(t, s)
	})

	runRequired(t, "custom_working_branch_existing_only", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const id, prefix, separator = "refresh", "automation", "--"
		const file = "scm-target.txt"
		branch := strings.Join([]string{prefix, r.base, id}, separator)
		workingRef := "refs/heads/" + branch
		baseRef := "refs/heads/" + r.base
		r.stage(t, r.base, "stable-only.txt", "not on main\n")
		before := r.refs(t)

		manifest := func(dir, value string) string {
			spec := scmTestSpec(r)
			spec["directory"] = filepath.Join(dir, "checkout")
			spec["singlebranch"] = true
			spec["workingbranchprefix"] = prefix
			spec["workingbranchseparator"] = separator
			return manifestYAML(t, map[string]any{
				"name": "Refresh a custom working branch", "pipelineid": id,
				"scms": map[string]any{"repo": map[string]any{"kind": "gitea", "spec": spec}},
				"targets": map[string]any{
					"refresh": map[string]any{
						"name": "Refresh working file", "kind": "file", "scmid": "repo",
						"spec": map[string]any{"file": file, "forcecreate": true, "content": value},
					},
				},
			})
		}
		// A skipped apply can leave local changes; isolate it from the publish.
		skipCLI := newCLI(t, s)
		out := skipCLI.ok(t, "apply", manifest(skipCLI.runner.Dir, "first\n"), "--existing-only")
		requireOutput(t, out, "existing-only mode")
		require.Equal(t, before, r.refs(t), "existing-only must not publish a missing branch")

		c.ok(t, "apply", manifest(c.runner.Dir, "first\n"))
		assertSCMRefChange(t, before, r.refs(t), workingRef)
		r.git(t, "fetch", "origin", workingRef)
		require.Equal(t, before[baseRef]+"\n", r.git(t, "rev-parse", "FETCH_HEAD^"), "working branch must start at the configured base")
		require.Equal(t, "first\n", r.git(t, "show", "FETCH_HEAD:"+file))

		// Advance the remote independently, leaving the candidate's cached ref
		// stale. An existing-only update must preserve the remote commit.
		r.stage(t, branch, "remote-marker.txt", "added independently\n")
		advanced := r.refs(t)
		c.ok(t, "apply", manifest(c.runner.Dir, "second\n"), "--existing-only")
		assertSCMRefChange(t, advanced, r.refs(t), workingRef)
		r.git(t, "fetch", "origin", workingRef)
		require.Equal(t, advanced[workingRef]+"\n", r.git(t, "rev-parse", "FETCH_HEAD^"), "must preserve the independently advanced tip")
		require.Equal(t, "second\n", r.git(t, "show", "FETCH_HEAD:"+file))
		require.Equal(t, "added independently\n", r.git(t, "show", "FETCH_HEAD:remote-marker.txt"))
	})

	runRequired(t, "managed_history", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		r.stage(t, r.base, "stable-only.txt", "configured branch tip\n")
		before := r.refs(t)
		base := before["refs/heads/"+r.base]
		const id = "managed-history"
		workingRef := "refs/heads/updatecli_" + r.base + "_" + id

		spec := scmTestSpec(r)
		spec["force"] = true // Allow rewriting only the Updatecli working branch.
		spec["commitmessage"] = map[string]any{"squash": true}
		apply := func(file, content string) {
			manifest := manifestYAML(t, map[string]any{
				"name": "Squash the managed working branch", "pipelineid": id,
				"scms": map[string]any{"repo": map[string]any{"kind": "gitea", "spec": spec}},
				"targets": map[string]any{
					"write": map[string]any{
						"name": "Write " + file, "kind": "file", "scmid": "repo",
						"spec": map[string]any{"file": file, "forcecreate": true, "content": content},
					},
				},
			})
			c.ok(t, "apply", manifest)
		}

		apply("first.txt", "first apply\n")
		first := r.refs(t)
		assertSCMRefChange(t, before, first, workingRef)
		r.git(t, "fetch", "origin", workingRef)
		require.Equal(t, base+"\n", r.git(t, "show", "-s", "--format=%P", first[workingRef]))
		require.Equal(t, "first apply\n", r.git(t, "show", first[workingRef]+":first.txt"))

		// The second apply does not target first.txt; its bytes must survive the rewrite.
		apply("second.txt", "second apply\n")
		second := r.refs(t)
		assertSCMRefChange(t, first, second, workingRef)
		r.git(t, "fetch", "origin", workingRef)
		tip := second[workingRef]
		require.Equal(t, base+"\n", r.git(t, "show", "-s", "--format=%P", tip),
			"the replacement commit must have stable as its only parent, not the first working tip")
		require.Equal(t, "first apply\n", r.git(t, "show", tip+":first.txt"))
		require.Equal(t, "second apply\n", r.git(t, "show", tip+":second.txt"))

		apply("second.txt", "second apply\n")
		require.Equal(t, second, r.refs(t), "an unchanged target must not rewrite squashed history")
	})

	runRequired(t, "signed_commit", func(t *testing.T) {
		testSCMSignedCommit(t, s)
	})

	runRequired(t, "direct_push_requires_explicit_force_setting", func(t *testing.T) {
		r := s.newRepo(t)
		spec := scmTestSpec(r)
		spec["workingbranch"] = false
		delete(spec, "force")
		before := r.refs(t)
		manifest := scmFileManifest(t, spec, "explicit-force", "version-note.txt", "requires explicit push settings\n")
		result := newCLI(t, s).run(t, "apply", manifest)
		require.NotZero(t, result.ExitCode, "workingbranch: false with an implicit force push must be rejected")
		requireOutput(t, result.Output, "unclear configuration, better safe than sorry")
		require.Equal(t, before, r.refs(t))
	})

	runRequired(t, "prune_current_working_branch_only", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const id, file = "prune", "prune.txt"
		branch := "updatecli_" + r.base + "_" + id
		workingRef := "refs/heads/" + branch
		r.stage(t, r.base, file, "current\n")
		r.git(t, "push", "origin", "refs/heads/"+r.base+":"+workingRef, "refs/heads/main:refs/tags/"+branch)
		before := r.refs(t)
		require.Equal(t, before["refs/heads/"+r.base], before[workingRef])

		manifest := func(value string) string {
			return scmFileManifest(t, scmTestSpec(r), id, file, value)
		}
		c.ok(t, "apply", manifest("current\n"))
		require.Equal(t, before, r.refs(t), "branch pruning must be opt-in")
		c.ok(t, "apply", manifest("current\n"), "--clean-git-branches")
		delete(before, workingRef)
		require.Equal(t, before, r.refs(t), "pruning must remove only the managed head, not the same-name tag or base")

		c.ok(t, "apply", manifest("changed\n"), "--clean-git-branches")
		withChange := r.refs(t)
		assertSCMRefChange(t, before, withChange, workingRef)
		require.NotEqual(t, withChange["refs/heads/"+r.base], withChange[workingRef])
		c.ok(t, "apply", manifest("changed\n"), "--clean-git-branches")
		require.Equal(t, withChange, r.refs(t), "pruning must keep the working branch while its commit differs from the base")
	})

	runRequired(t, "native_gitbranch_target", func(t *testing.T) {
		testNativeGitBranchTarget(t, s)
	})

	runRequired(t, "native_ref_sources_and_conditions", func(t *testing.T) {
		testNativeRefReads(t, s)
	})

	runRequired(t, "native_gittag_target", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		r.stage(t, r.base, "stable-only.txt", "this commit should be tagged\n")
		r.git(t, "push", "origin", "main:refs/tags/v1.0.0")
		const tag = "v1.1.0"
		const tagRef = "refs/tags/" + tag
		r.git(t, "push", "origin", "refs/heads/main:refs/heads/"+tag)
		before := r.refs(t)
		require.NotContains(t, before, tagRef)
		require.NotEqual(t, before["refs/heads/"+r.base], before["refs/heads/"+tag])

		configureNativeTagger(t, c)
		spec := scmTestSpec(r)
		spec["workingbranch"] = false
		manifest := nativeRefManifest(t, spec, "gittag", map[string]any{
			"versionfilter": map[string]any{"pattern": tag},
			"message":       "Tag from the SCM acceptance test",
		})

		// Preview the private repo with only a PAT, exercising the SCM's username fallback.
		preview := newCLIWithCredentials(t, "", s.token).run(t, "diff", manifest)
		require.Zero(t, preview.ExitCode)
		requireOutput(t, preview.Output, `git tag "v1.1.0" should be created`)
		require.Equal(t, before, r.refs(t), "diff must not write remote refs")

		// The target publishes the tag itself; there are no files to commit.
		c.ok(t, "apply", manifest, "--commit=false")
		published := r.refs(t)
		assertSCMRefChange(t, before, published, tagRef)
		stable := before["refs/heads/"+r.base]
		require.Equal(t, stable+"\t"+tagRef+"^{}\n", r.git(t, "ls-remote", "origin", tagRef+"^{}"),
			"the annotated tag must peel to the configured base, not main")
		r.git(t, "fetch", "origin", tagRef)
		tagObject := r.git(t, "cat-file", "tag", published[tagRef])
		require.True(t, strings.HasSuffix(tagObject, "\n\nTag from the SCM acceptance test\n"), "tag must contain the configured annotation")

		requireOutput(t, c.ok(t, "apply", manifest, "--commit=false"), `git tag "v1.1.0" already exists`)
		require.Equal(t, published, r.refs(t), "reapply must neither replace the tag object nor move other refs")
	})
}

func testSCMSubmodules(t *testing.T, s *server) {
	r, sub := s.newRepo(t), s.newRepo(t)
	const input, moduleDir = "module-input.txt", "fixture-module"
	const value = "from the pinned submodule commit\n"
	sub.stage(t, sub.base, input, value)
	subBefore := sub.refs(t)
	subCommit := subBefore["refs/heads/"+sub.base]
	subURL := s.url + "/" + s.owner + "/" + sub.name + ".git"
	r.write(t, ".gitmodules", "[submodule \""+moduleDir+"\"]\n\tpath = "+moduleDir+"\n\turl = "+subURL+"\n")
	// Stage the submodule pin without cloning it in the fixture's worktree.
	// The candidate must download the pinned object over authenticated HTTP.
	r.git(t, "update-index", "--add", "--cacheinfo", "160000,"+subCommit+","+moduleDir)
	r.git(t, "add", "--", ".gitmodules")
	r.git(t, "commit", "-m", "Pin a local private submodule")
	r.git(t, "push", "origin", "HEAD:refs/heads/"+r.base)
	before := r.refs(t)

	for _, name := range []string{"default", "disabled"} {
		runRequired(t, name, func(t *testing.T) {
			c := newCLI(t, s)
			dir := filepath.Join(c.runner.Dir, "checkout")
			spec := scmTestSpec(r)
			spec["directory"], spec["workingbranch"] = dir, false
			spec["singlebranch"] = true
			if name == "disabled" {
				spec["submodules"] = false
			}
			manifest := manifestYAML(t, map[string]any{
				"name": "Check out a pinned submodule",
				"scms": map[string]any{"repo": map[string]any{"kind": "gitea", "spec": spec}},
				"sources": map[string]any{
					"parent": map[string]any{"kind": "file", "scmid": "repo", "spec": map[string]string{"file": "version.yaml"}},
				},
			})
			c.ok(t, "diff", manifest)
			require.FileExists(t, filepath.Join(dir, ".gitmodules"))
			path := filepath.Join(dir, moduleDir, input)
			if name == "disabled" {
				require.NoFileExists(t, path)
			} else {
				got, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, value, string(got))
				require.Equal(t, subCommit+"\n", r.git(t, "-C", filepath.Join(dir, moduleDir), "rev-parse", "HEAD"))
			}
		})
	}
	require.Equal(t, before, r.refs(t), "cloning must not change the parent repository")
	require.Equal(t, subBefore, sub.refs(t), "cloning must not change the submodule repository")
}

func scmFileManifest(t *testing.T, spec map[string]any, id, file, content string) string {
	t.Helper()
	return manifestYAML(t, map[string]any{
		"name":       "Write a file with the configured SCM",
		"pipelineid": id,
		"scms":       map[string]any{"repo": map[string]any{"kind": "gitea", "spec": spec}},
		"targets": map[string]any{
			"write": map[string]any{
				"name": "Write " + file, "kind": "file", "scmid": "repo",
				"spec": map[string]any{"file": file, "forcecreate": true, "content": content},
			},
		},
	})
}

func scmTestSpec(r *repository) map[string]any {
	return map[string]any{
		"url":        r.server.url,
		"owner":      r.server.owner,
		"repository": r.name,
		"branch":     r.base,
		"force":      false,
	}
}

func assertSCMRefChange(t *testing.T, before, after map[string]string, ref string) {
	t.Helper()
	require.NotEmpty(t, after[ref], "expected ref %s in %v", ref, after)
	require.NotEqual(t, before[ref], after[ref], "expected %s to advance", ref)
	for key, sha := range before {
		if key != ref {
			require.Equal(t, sha, after[key], "unrelated ref %s changed", key)
		}
	}
	for key := range after {
		if key != ref {
			require.Contains(t, before, key, "unexpected new ref %s", key)
		}
	}
}
