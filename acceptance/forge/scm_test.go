//go:build acceptance

package forge

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testSCM(t *testing.T, s *server) {
	t.Run("private_branch_read_and_direct_push", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const input, copied = "scm-input.txt", "scm-copy.txt"
		const stableValue = "value from stable\n"
		r.stage(t, "main", input, "value from main\n")
		r.stage(t, r.base, input, stableValue)
		before := r.refs(t)

		// Keep the shallow reader separate from the full-history writer: the
		// direct push must be fast-forward, not rely on forcing a shallow push.
		readDir := filepath.Join(c.dir, "shallow")
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
	})

	t.Run("custom_working_branch_existing_only", func(t *testing.T) {
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
		out := skipCLI.ok(t, "apply", manifest(skipCLI.dir, "first\n"), "--existing-only")
		require.Contains(t, out, "existing-only mode")
		require.Equal(t, before, r.refs(t), "existing-only must not publish a missing branch")

		c.ok(t, "apply", manifest(c.dir, "first\n"))
		assertSCMRefChange(t, before, r.refs(t), workingRef)
		r.git(t, "fetch", "origin", workingRef)
		require.Equal(t, before[baseRef]+"\n", r.git(t, "rev-parse", "FETCH_HEAD^"), "working branch must start at the configured base")
		require.Equal(t, "first\n", r.git(t, "show", "FETCH_HEAD:"+file))

		// Advance the remote independently, leaving the candidate's cached ref
		// stale. An existing-only update must preserve the remote commit.
		r.stage(t, branch, "remote-marker.txt", "added independently\n")
		advanced := r.refs(t)
		c.ok(t, "apply", manifest(c.dir, "second\n"), "--existing-only")
		assertSCMRefChange(t, advanced, r.refs(t), workingRef)
		r.git(t, "fetch", "origin", workingRef)
		require.Equal(t, advanced[workingRef]+"\n", r.git(t, "rev-parse", "FETCH_HEAD^"), "must preserve the independently advanced tip")
		require.Equal(t, "second\n", r.git(t, "show", "FETCH_HEAD:"+file))
		require.Equal(t, "added independently\n", r.git(t, "show", "FETCH_HEAD:remote-marker.txt"))
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
