//go:build acceptance

package forge

import (
	"context"
	"crypto/rand"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gitea.dev/sdk"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

type server struct {
	provider, url, owner, token string
	client                      *gitea.Client

	admin *gitea.Client
}

func newCLI(t *testing.T, s *server) *forgeCLI {
	t.Helper()
	return newCLIWithCredentials(t, s.owner, s.token)
}

func newCLIWithCredentials(t *testing.T, user, token string) *forgeCLI {
	t.Helper()
	return &forgeCLI{
		runner: cli.New(t, "UPDATECLI_GITEA_USERNAME="+user, "UPDATECLI_GITEA_TOKEN="+token),
		user:   user,
		token:  token,
	}
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
	*gitRepository
	server *server
	name   string
}

func (s *server) newRepo(t *testing.T) *repository {
	t.Helper()
	r := &repository{
		gitRepository: newGitRepository(t, s.url, s.owner, s.token),
		server:        s,
		name:          "acceptance-" + strings.ToLower(rand.Text()),
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

	r.seed(t, s.url+"/"+s.owner+"/"+r.name+".git")
	r.awaitBranches(t, "main", r.base)
	r.assertNotPublic(t)
	return r
}

// Git may acknowledge a push before the forge API reflects it. Wait on reads,
// never by retrying the mutation under test.
func (r *repository) awaitBranches(t *testing.T, branches ...string) map[string]string {
	t.Helper()
	s := r.server
	refs := r.refs(t)
	for _, branch := range branches {
		if refs["refs/heads/"+branch] == "" {
			t.Fatalf("fixture branch %s is missing from the remote", branch)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		label := "repository metadata"
		repo, resp, err := s.client.Repositories.GetRepo(ctx, s.owner, r.name)
		ready := err == nil && repo != nil && !repo.Empty && repo.DefaultBranch == "main"
		// The SDK also returns a zero Repository on transport/HTTP errors.
		// Only a successful metadata read can establish that it is public.
		if err == nil && repo != nil && !repo.Private {
			t.Fatalf("%s repository %s unexpectedly public", s.provider, r.name)
		}
		if ready {
			for _, branch := range branches {
				var got *gitea.Branch
				got, resp, err = s.client.Repositories.GetRepoBranch(ctx, s.owner, r.name, branch)
				if err != nil || got == nil || got.Name != branch || got.Commit == nil || got.Commit.ID != refs["refs/heads/"+branch] {
					ready, label = false, branch+" branch metadata"
					break
				}
			}
		}
		if ready {
			return refs
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			if err != nil {
				s.apiFailure(t, "wait for "+label, resp, err)
			}
			t.Fatalf("%s %s: %s didn't match the pushed refs", s.provider, r.name, label)
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
