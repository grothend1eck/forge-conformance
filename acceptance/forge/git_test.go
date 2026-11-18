//go:build acceptance

package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type gitRepository struct {
	dir, home, base  string
	url, user, token string
}

func newGitRepository(t *testing.T, url, user, token string) *gitRepository {
	t.Helper()
	return &gitRepository{dir: t.TempDir(), home: t.TempDir(), base: "stable", url: url, user: user, token: token}
}

func (r *gitRepository) seed(t *testing.T, remote string) {
	t.Helper()
	r.git(t, "clone", "--", remote, ".")
	r.git(t, "switch", "--orphan", "main")
	r.git(t, "config", "--local", "user.name", r.user)
	r.git(t, "config", "--local", "user.email", r.user+"@example.test")
	r.git(t, "config", "--local", "commit.gpgsign", "false")
	r.write(t, "version.yaml", "version: v1.0.0\nkeep: untouched\n")
	r.git(t, "add", "--", "version.yaml")
	r.git(t, "commit", "-m", "Seed fixture repository")
	r.git(t, "push", "--set-upstream", "origin", "main")
	r.git(t, "switch", "-c", r.base)
	r.git(t, "push", "--set-upstream", "origin", r.base)
	seed := strings.TrimSpace(r.git(t, "rev-parse", "HEAD"))
	refs := r.refs(t)
	if refs["refs/heads/main"] != seed || refs["refs/heads/"+r.base] != seed {
		t.Fatalf("both fixture branches must point to seed %s", seed)
	}
}

// Git credentials are scoped to the fixture URL and never stored in the clone.
func (r *gitRepository) git(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	cmd.WaitDelay = 3 * time.Second
	encoded := base64.StdEncoding.EncodeToString([]byte(r.user + ":" + r.token))
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "LANG=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http." + r.url + "/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + encoded,
		"GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false",
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", redactCredential(strings.Join(args, " "), r.user, r.token), err, redactCredential(stderr.String(), r.user, r.token))
	}
	return redactCredential(string(out), r.user, r.token)
}

func (r *gitRepository) refs(t *testing.T) map[string]string {
	t.Helper()
	out := r.git(t, "ls-remote", "--refs", "origin", "refs/heads/*", "refs/tags/*")
	refs := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		sha, ref, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatal("unexpected output from git ls-remote")
		}
		refs[ref] = sha
	}
	return refs
}

func (r *gitRepository) stage(t *testing.T, branch, filename, contents string) {
	t.Helper()
	r.git(t, "check-ref-format", "--branch", branch)
	r.git(t, "fetch", "--prune", "origin")
	remote := "refs/remotes/origin/" + branch
	branches := strings.Fields(r.git(t, "for-each-ref", "--format=%(refname)", "refs/remotes/origin", "refs/heads"))
	switch {
	case slices.Contains(branches, remote):
		r.git(t, "switch", "-C", branch, remote)
	case slices.Contains(branches, "refs/heads/"+branch):
		r.git(t, "switch", branch)
	default:
		r.git(t, "switch", "-c", branch, "refs/remotes/origin/"+r.base)
	}
	r.write(t, filename, contents)
	r.git(t, "--literal-pathspecs", "add", "--", filename)
	r.git(t, "--literal-pathspecs", "commit", "-m", fmt.Sprintf("Stage fixture change to %s", filename), "--", filename)
	r.git(t, "push", "origin", "HEAD:refs/heads/"+branch)
}

func (r *gitRepository) write(t *testing.T, filename, contents string) {
	t.Helper()
	if !filepath.IsLocal(filename) || filepath.Clean(filename) == "." {
		t.Fatalf("fixture filename must be a relative file path: %q", filename)
	}
	path := filepath.Join(r.dir, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
