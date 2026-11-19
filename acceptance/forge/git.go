//go:build acceptance

package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

// GitRepository is a native Git checkout of an acceptance fixture, separate
// from any checkout made by the Updatecli candidate.
type GitRepository struct {
	Base                string
	dir                 string
	origin, user, token string
	env                 []string
}

// NewGitRepository reserves an isolated checkout for a loopback HTTP fixture.
func NewGitRepository(t *testing.T, fixtureURL, user, token string) *GitRepository {
	t.Helper()
	if user == "" || token == "" {
		t.Fatal("native Git fixture needs actor credentials")
	}
	u, err := url.Parse(fixtureURL)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("Git fixture URL must be a bare HTTP(S) origin")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsLoopback() {
		t.Fatal("Git fixture URL must address a loopback IP")
	}
	origin := u.Scheme + "://" + u.Host
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=http:https", "GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.followRedirects", "GIT_CONFIG_VALUE_0=false",
		// Scope the header to the fixture, without storing it in the clone.
		"GIT_CONFIG_KEY_1=http." + origin + "/.extraHeader",
		"GIT_CONFIG_VALUE_1=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token)),
	}
	return &GitRepository{Base: "stable", dir: t.TempDir(), origin: origin, user: user, token: token, env: env}
}

// Seed starts an empty remote with the same version file on main and Base.
func (r *GitRepository) Seed(t *testing.T, remote string) {
	t.Helper()
	if !strings.HasPrefix(remote, r.origin+"/") {
		t.Fatal("Git remote must be on the fixture origin")
	}
	r.Git(t, "clone", "--config", "user.name="+r.user, "--config", "user.email="+r.user+"@example.test", "--", remote, ".")
	r.Git(t, "switch", "--orphan", "main")
	r.Write(t, "version.yaml", versionFile("v1.0.0"))
	r.Git(t, "add", "--", "version.yaml")
	r.Git(t, "commit", "-m", "Seed fixture repository")
	r.Git(t, "push", "--set-upstream", "origin", "main")
	r.Git(t, "switch", "-c", r.Base)
	r.Git(t, "push", "--set-upstream", "origin", r.Base)
	seed := strings.TrimSpace(r.Git(t, "rev-parse", "HEAD"))
	refs := r.Refs(t)
	if refs["refs/heads/main"] != seed || refs["refs/heads/"+r.Base] != seed {
		t.Fatalf("both fixture branches must point to seed %s", seed)
	}
}

// Git runs a native Git command in the fixture checkout.
func (r *GitRepository) Git(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	cmd.WaitDelay = 3 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		t.Fatal(cli.RedactCredential(fmt.Sprintf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String()), r.user, r.token))
	}
	return cli.RedactCredential(string(out), r.user, r.token)
}

// Refs reads branch and tag object IDs from the remote, not local tracking refs.
func (r *GitRepository) Refs(t *testing.T) map[string]string {
	t.Helper()
	out := r.Git(t, "ls-remote", "--refs", "origin", "refs/heads/*", "refs/tags/*")
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

// Stage commits a fixture edit and pushes that branch normally (never forced).
func (r *GitRepository) Stage(t *testing.T, branch, filename, contents string) {
	t.Helper()
	r.Git(t, "check-ref-format", "--branch", branch)
	r.Git(t, "fetch", "--prune", "origin")
	remote := "refs/remotes/origin/" + branch
	branches := strings.Fields(r.Git(t, "for-each-ref", "--format=%(refname)", "refs/remotes/origin", "refs/heads"))
	switch {
	case slices.Contains(branches, remote):
		r.Git(t, "switch", "-C", branch, remote)
	case slices.Contains(branches, "refs/heads/"+branch):
		r.Git(t, "switch", branch)
	default:
		r.Git(t, "switch", "-c", branch, "refs/remotes/origin/"+r.Base)
	}
	r.Write(t, filename, contents)
	r.Git(t, "--literal-pathspecs", "add", "--", filename)
	r.Git(t, "--literal-pathspecs", "commit", "-m", "Stage fixture change to "+filename, "--", filename)
	r.Git(t, "push", "origin", "HEAD:refs/heads/"+branch)
}

// Write changes a worktree file without staging it.
func (r *GitRepository) Write(t *testing.T, filename, contents string) {
	t.Helper()
	if !filepath.IsLocal(filename) || filepath.Clean(filename) == "." {
		t.Fatalf("fixture filename must be a relative file path: %q", filename)
	}
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
