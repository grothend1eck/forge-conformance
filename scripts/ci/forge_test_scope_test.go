//go:build linux

package ci_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestForgeTestScope(t *testing.T) {
	const doc = "acceptance/README.md"
	const target = "pkg/plugins/resources/gitea/pullrequest/title_fuzz_test.go"
	const seed = "pkg/plugins/resources/gitea/pullrequest/testdata/fuzz/FuzzExistingPullRequestTitle/seed"
	script, err := filepath.Abs("forge-test-scope.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
		changed    []string
		remove     string
		badHistory bool
	}{
		{name: "allowlisted docs", want: "docs", changed: []string{doc}},
		{name: "test and docs", want: "package", changed: []string{doc, target}},
		{name: "added fuzz input", want: "package", changed: []string{seed}},
		{name: "source with a seed", want: "full", changed: []string{seed, "pkg/plugin.go"}},
		{name: "embedded markdown", want: "full", changed: []string{"pkg/core/scaffold/assets/README.md"}},
		{name: "odd filename", want: "full", changed: []string{"unknown\n" + doc}},
		{name: "rename into allowed path", want: "full", changed: []string{seed}, remove: "original"},
		{name: "missing history", want: "full", changed: []string{doc}, badHistory: true},
		{name: "empty diff", want: "full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			run := func(command string, args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), command, args...)
				cmd.Dir = dir
				out, err := cmd.Output()
				if err != nil {
					t.Fatalf("%s %v: %v", command, args, err)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(path string) {
				t.Helper()
				path = filepath.Join(dir, path)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("fixture\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			run("git", "init", "-q")
			run("git", "config", "user.name", "fixture")
			run("git", "config", "user.email", "fixture@example.test")
			write("original")
			run("git", "add", ".")
			run("git", "commit", "-qm", "base")
			base := run("git", "rev-parse", "HEAD")
			if tc.remove != "" {
				run("git", "rm", "-q", tc.remove)
			}
			for _, path := range tc.changed {
				write(path)
			}
			run("git", "add", ".")
			tree := run("git", "write-tree")
			head := run("git", "commit-tree", tree, "-p", base, "-m", "feature")
			merge := run("git", "commit-tree", tree, "-p", base, "-p", head, "-m", "merge")
			if tc.badHistory {
				merge = strings.Repeat("0", len(head))
			}
			if got := run("bash", script, merge, base, head); got != tc.want {
				t.Errorf("selected %q, want %q", got, tc.want)
			}
		})
	}
}
