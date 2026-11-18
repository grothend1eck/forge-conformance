//go:build acceptance

package forge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.dev/sdk"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

type server struct {
	provider, url, owner, token string
	client                      *gitea.Client

	admin        *gitea.Client
	usersMu      sync.Mutex
	checkPrivacy sync.Once
}

func startServer(t *testing.T, provider string) *server {
	t.Helper()

	// Pin the multi-platform OCI index, not a runner-specific manifest.
	var image, prefix string
	switch provider {
	case "gitea":
		image, prefix = "gitea/gitea:28.2.0@sha256:30777c91280d973fa02be48b1ade735bd6859e8f6f7af3b4444d519437852504", "GITEA__"
	case "forgejo":
		image, prefix = "data.forgejo.org/forgejo/forgejo:17.0.0@sha256:06d38310de496a10186d952bdad5ca764c153c8023ede8dfa0944c79a1dc5412", "FORGEJO__"
	default:
		t.Fatalf("unknown forge fixture provider %q", provider)
	}

	const port = "3000/tcp"
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c, err := testcontainers.Run(ctx, image,
		testcontainers.WithEnv(map[string]string{
			prefix + "database__DB_TYPE":                "sqlite3",
			prefix + "security__INSTALL_LOCK":           "true",
			prefix + "server__DISABLE_SSH":              "true",
			prefix + "server__OFFLINE_MODE":             "true",
			prefix + "picture__DISABLE_GRAVATAR":        "true",
			prefix + "picture__ENABLE_FEDERATED_AVATAR": "false",
			prefix + "service__DISABLE_REGISTRATION":    "true",
			prefix + "repository__DEFAULT_BRANCH":       "main",
			prefix + "actions__ENABLED":                 "false",
			prefix + "federation__ENABLED":              "false",
			prefix + "cron.update_checker__ENABLED":     "false",
		}),
		testcontainers.WithExposedPorts(port),
		testcontainers.WithHostConfigModifier(func(config *container.HostConfig) {
			config.PortBindings = network.PortMap{
				network.MustParsePort(port): {{HostIP: netip.MustParseAddr("127.0.0.1")}},
			}
		}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/api/v1/version").
			WithPort(port).WithForcedIPv4LocalHost().WithStartupTimeout(90*time.Second)),
	)
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start %s fixture: %v", image, err)
	}
	mapped, err := c.MappedPort(ctx, port)
	if err != nil {
		t.Fatalf("get %s fixture HTTP port: %v", provider, err)
	}
	s := &server{
		provider: provider,
		url:      "http://" + net.JoinHostPort("127.0.0.1", mapped.Port()),
		owner:    "fixture-bot",
	}

	// Bootstrap the first admin through the image's CLI, then use the API for
	// all other accounts. Keep the CLI output and its arguments out of logs.
	const admin = "fixture-admin"
	adminPassword := rand.Text()
	code, _, err := c.Exec(ctx, []string{provider, "admin", "user", "create",
		"--admin", "--username", admin, "--password", adminPassword,
		"--email", admin + "@example.test", "--must-change-password=false",
	}, tcexec.WithUser("git"))
	if err != nil {
		t.Fatalf("bootstrap %s fixture administrator: docker exec failed", provider)
	}
	if code != 0 {
		t.Fatalf("bootstrap %s fixture administrator: forge CLI exited %d", provider, code)
	}
	adminToken := s.accessToken(t, admin, adminPassword, gitea.AccessTokenScopeAll)
	s.admin = s.sdkClient(t, gitea.SetToken(adminToken))

	actorPassword := rand.Text()
	s.createUser(t, s.owner, actorPassword)
	s.token = s.accessToken(t, s.owner, actorPassword)
	s.client = s.sdkClient(t, gitea.SetToken(s.token))
	actor, resp, err := s.client.Users.GetMyUserInfo(t.Context())
	if err != nil {
		s.apiFailure(t, "authenticate fixture actor", resp, err)
	}
	if actor.UserName != s.owner || actor.IsAdmin {
		t.Fatalf("%s fixture actor is not the expected non-admin user", provider)
	}
	return s
}

func (s *server) sdkClient(t *testing.T, options ...gitea.ClientOption) *gitea.Client {
	t.Helper()
	httpClient := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	client, err := gitea.NewClient(s.url, append([]gitea.ClientOption{gitea.SetHTTPClient(httpClient)}, options...)...)
	if err != nil {
		t.Fatalf("construct %s fixture SDK client", s.provider)
	}
	return client
}

func (s *server) accessToken(t *testing.T, username, password string, scopes ...gitea.AccessTokenScope) string {
	t.Helper()
	if len(scopes) == 0 {
		// v1.4.0 does not export these current repository/issue scope names.
		scopes = []gitea.AccessTokenScope{"write:repository", "write:issue", gitea.AccessTokenScopeReadUser}
	}
	basic := s.sdkClient(t, gitea.SetBasicAuth(username, password))
	token, resp, err := basic.Users.CreateAccessToken(t.Context(), gitea.CreateAccessTokenOption{
		Name:   "acceptance-fixture",
		Scopes: scopes,
	})
	if err != nil {
		s.apiFailure(t, "create fixture access token", resp, err)
	}
	if token.Token == "" {
		t.Fatalf("%s returned an empty fixture access token", s.provider)
	}
	return token.Token
}

func (s *server) adminClient() *gitea.Client {
	return s.admin
}

func (s *server) createUser(t *testing.T, name, password string) {
	t.Helper()
	u, resp, err := s.admin.Admin.CreateUser(t.Context(), gitea.CreateUserOption{
		Username:           name,
		Email:              name + "@example.test",
		Password:           password,
		MustChangePassword: new(false),
	})
	if err != nil {
		s.apiFailure(t, "create fixture user "+name, resp, err)
	}
	if u.UserName != name || u.IsAdmin {
		t.Fatalf("%s fixture user %q is not the expected non-admin user", s.provider, name)
	}
}

// user creates an account, not repository access. Scenarios grant any
// collaborator permissions they need on their private repository.
func (s *server) user(t *testing.T, name string) {
	t.Helper()
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	u, resp, err := s.admin.Users.GetUserInfo(t.Context(), name)
	if err == nil {
		if u.IsAdmin {
			t.Fatalf("%s fixture user %q must not be a site admin", s.provider, name)
		}
		return
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		s.apiFailure(t, "look up fixture user "+name, resp, err)
	}
	s.createUser(t, name, rand.Text())
}

// SDK errors may contain response bodies. Do not log those from fixture
// requests, especially user creation and the token endpoint.
func (s *server) apiFailure(t *testing.T, action string, resp *gitea.Response, err error) {
	t.Helper()
	if resp != nil && resp.Response != nil {
		t.Fatalf("%s: %s: HTTP %d", s.provider, action, resp.StatusCode)
	}
	t.Fatalf("%s: %s: SDK request failed (%T)", s.provider, action, err)
}

type repository struct {
	server          *server
	name, base, dir string
	home            string
}

func (s *server) newRepo(t *testing.T) *repository {
	t.Helper()
	r := &repository{
		server: s,
		name:   "acceptance-" + strings.ToLower(rand.Text()),
		base:   "stable",
		dir:    t.TempDir(),
		home:   t.TempDir(),
	}
	// Provision through the site administrator so the pipeline PAT does not
	// need write:user to create repositories in the actor's namespace.
	repo, resp, err := s.admin.Admin.CreateRepo(t.Context(), s.owner, gitea.CreateRepoOption{
		Name:          r.name,
		Private:       true,
		DefaultBranch: "main",
	})
	if err != nil {
		s.apiFailure(t, "create fixture repository", resp, err)
	}
	if !repo.Private || repo.Owner == nil || repo.Owner.UserName != s.owner {
		t.Fatalf("%s fixture repository is not private and owned by the fixture actor", s.provider)
	}

	// The forge's default branch remains main. Seed both branches at the
	// same commit without relying on provider-specific auto-init content.
	r.git(t, "clone", "--", s.url+"/"+s.owner+"/"+r.name+".git", ".")
	r.git(t, "switch", "--orphan", "main")
	r.git(t, "config", "--local", "user.name", s.owner)
	r.git(t, "config", "--local", "user.email", s.owner+"@example.test")
	r.git(t, "config", "--local", "commit.gpgsign", "false")
	r.write(t, "version.yaml", "version: v1.0.0\nkeep: untouched\n")
	r.git(t, "add", "--", "version.yaml")
	r.git(t, "commit", "-m", "Seed fixture repository")
	r.git(t, "push", "--set-upstream", "origin", "main")
	r.git(t, "switch", "-c", r.base)
	r.git(t, "push", "--set-upstream", "origin", r.base)
	r.awaitSeed(t)
	s.checkPrivacy.Do(func() { r.assertNotPublic(t) })
	return r
}

func (r *repository) awaitSeed(t *testing.T) {
	t.Helper()
	s := r.server
	seed := strings.TrimSpace(r.git(t, "rev-parse", "HEAD"))
	refs := r.refs(t)
	branches := []string{"main", r.base}
	for _, branch := range branches {
		if got := refs["refs/heads/"+branch]; got != seed {
			t.Fatalf("%s remote %s tip = %q, want seed %s", s.provider, branch, got, seed)
		}
	}

	// The Git receive and API metadata can become visible at different
	// times. Poll only reads; every seed mutation must succeed on its own.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		label := "repository metadata"
		repo, resp, err := s.client.Repositories.GetRepo(ctx, s.owner, r.name)
		ready := err == nil && !repo.Empty && repo.DefaultBranch == "main"
		if err == nil && !repo.Private {
			t.Fatalf("%s repository %s unexpectedly public", s.provider, r.name)
		}
		if ready {
			for _, branch := range branches {
				var got *gitea.Branch
				got, resp, err = s.client.Repositories.GetRepoBranch(ctx, s.owner, r.name, branch)
				if err != nil || got.Name != branch || got.Commit == nil || got.Commit.ID != seed {
					ready, label = false, branch+" branch metadata"
					break
				}
			}
		}
		if ready {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			if err != nil {
				s.apiFailure(t, "wait for seeded "+label, resp, err)
			}
			t.Fatalf("%s %s: %s didn't reflect seed %s", s.provider, r.name, label, seed)
		}
	}
}

func (r *repository) assertNotPublic(t *testing.T) {
	t.Helper()
	s := r.server
	anon := s.sdkClient(t)
	_, resp, err := anon.Repositories.GetRepo(t.Context(), s.owner, r.name)
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		s.apiFailure(t, "anonymous read of private repository (want HTTP 404)", resp, err)
	}
}

// git preserves stdout, including newlines, but always redacts authentication.
// The PAT is sent in a URL-scoped header supplied through the child environment.
func (r *repository) git(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	cmd.WaitDelay = 3 * time.Second
	encoded := base64.StdEncoding.EncodeToString([]byte(r.server.owner + ":" + r.server.token))
	safe := strings.NewReplacer(r.server.token, "[redacted]", encoded, "[redacted]")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.home,
		"LANG=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http." + r.server.url + "/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + encoded,
		"GIT_CONFIG_KEY_1=http.followRedirects",
		"GIT_CONFIG_VALUE_1=false",
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", safe.Replace(strings.Join(args, " ")), err, safe.Replace(stderr.String()))
	}
	return safe.Replace(string(stdout))
}

func (r *repository) refs(t *testing.T) map[string]string {
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

func (r *repository) stage(t *testing.T, branch, filename, contents string) {
	t.Helper()
	r.git(t, "check-ref-format", "--branch", branch)
	r.git(t, "fetch", "--prune", "origin")
	remoteRef := "refs/remotes/origin/" + branch
	branches := r.git(t, "for-each-ref", "--format=%(refname)", "refs/remotes/origin", "refs/heads")
	switch refs := strings.Split(branches, "\n"); {
	case slices.Contains(refs, remoteRef):
		// The CLI may have advanced this branch since our fixture clone.
		r.git(t, "switch", "-C", branch, remoteRef)
	case slices.Contains(refs, "refs/heads/"+branch):
		r.git(t, "switch", branch)
	default:
		r.git(t, "switch", "-c", branch, "refs/remotes/origin/"+r.base)
	}
	r.write(t, filename, contents)
	r.git(t, "--literal-pathspecs", "add", "--", filename)
	r.git(t, "--literal-pathspecs", "commit", "-m", fmt.Sprintf("Stage fixture change to %s", filename), "--", filename)
	r.git(t, "push", "origin", "HEAD:refs/heads/"+branch)
}

func (r *repository) write(t *testing.T, filename, contents string) {
	t.Helper()
	if !filepath.IsLocal(filename) || filepath.Clean(filename) == "." {
		t.Fatalf("fixture filename must be a relative file path: %q", filename)
	}
	path := filepath.Join(r.dir, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture file directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
}
