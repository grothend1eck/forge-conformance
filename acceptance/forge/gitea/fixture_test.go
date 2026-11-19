//go:build acceptance

package gitea_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gitea.dev/sdk"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/updatecli/updatecli/acceptance/forge"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

type server struct {
	provider, url, owner, token string
	client, admin               *gitea.Client
}

var images = map[string]string{
	"gitea":   "gitea/gitea:28.2.0@sha256:30777c91280d973fa02be48b1ade735bd6859e8f6f7af3b4444d519437852504",
	"forgejo": "data.forgejo.org/forgejo/forgejo:17.0.0@sha256:06d38310de496a10186d952bdad5ca764c153c8023ede8dfa0944c79a1dc5412",
}

func startServer(t *testing.T, provider string) *server {
	t.Helper()
	require.Contains(t, images, provider)
	prefix := strings.ToUpper(provider) + "__"
	const port = "3000/tcp"
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c, err := testcontainers.Run(ctx, images[provider],
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
		testcontainers.WithHostConfigModifier(func(cfg *container.HostConfig) {
			cfg.PortBindings = network.PortMap{network.MustParsePort(port): {{HostIP: netip.MustParseAddr("127.0.0.1")}}}
		}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/api/v1/version").
			WithPort(port).WithForcedIPv4LocalHost().WithStartupTimeout(90*time.Second)),
	)
	testcontainers.CleanupContainer(t, c)
	require.NoError(t, err)
	mapped, err := c.MappedPort(ctx, port)
	require.NoError(t, err)
	s := &server{provider: provider, url: "http://127.0.0.1:" + mapped.Port(), owner: "fixture-bot"}

	// Only the fixture gets an administrator. Updatecli acts as the repository owner.
	const admin = "fixture-admin"
	password := rand.Text()
	code, _, err := c.Exec(ctx, []string{provider, "admin", "user", "create", "--admin",
		"--username", admin, "--password", password, "--email", admin + "@example.test",
		"--must-change-password=false"}, tcexec.WithUser("git"))
	require.True(t, err == nil && code == 0, "could not create fixture administrator (exit %d)", code)
	s.admin = s.sdkClient(t, gitea.SetToken(s.accessToken(t, admin, password, gitea.AccessTokenScopeAll)))
	password = rand.Text()
	s.createUser(t, s.owner, password)
	s.token = s.accessToken(t, s.owner, password)
	s.client = s.sdkClient(t, gitea.SetToken(s.token))
	actor, resp, err := s.client.Users.GetMyUserInfo(ctx)
	s.check(t, "authenticate fixture actor", resp, err)
	require.Equal(t, s.owner, actor.UserName)
	require.False(t, actor.IsAdmin)
	return s
}

func (s *server) sdkClient(t *testing.T, options ...gitea.ClientOption) *gitea.Client {
	t.Helper()
	httpClient := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	client, err := gitea.NewClient(s.url, append(options, gitea.SetHTTPClient(httpClient))...)
	require.True(t, err == nil, "construct %s SDK client", s.provider)
	return client
}

func (s *server) accessToken(t *testing.T, user, password string, scopes ...gitea.AccessTokenScope) string {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []gitea.AccessTokenScope{"write:repository", "write:issue", gitea.AccessTokenScopeReadUser}
	}
	client := s.sdkClient(t, gitea.SetBasicAuth(user, password))
	token, resp, err := client.Users.CreateAccessToken(t.Context(), gitea.CreateAccessTokenOption{Name: "acceptance", Scopes: scopes})
	s.check(t, "create fixture token", resp, err)
	require.True(t, token.Token != "", "empty fixture token")
	return token.Token
}

func (s *server) createUser(t *testing.T, user, password string) {
	t.Helper()
	u, resp, err := s.admin.Admin.CreateUser(t.Context(), gitea.CreateUserOption{
		Username: user, Email: user + "@example.test", Password: password, MustChangePassword: new(false),
	})
	s.check(t, "create fixture user "+user, resp, err)
	require.Equal(t, user, u.UserName)
	require.False(t, u.IsAdmin)
}

// SDK errors may contain request or response bodies, including a newly issued token.
func (s *server) check(t *testing.T, operation string, resp *gitea.Response, err error) {
	t.Helper()
	if err != nil {
		s.apiFailure(t, operation, resp, err)
	}
}

func (s *server) apiFailure(t *testing.T, operation string, resp *gitea.Response, err error) {
	t.Helper()
	if resp != nil && resp.Response != nil {
		t.Fatalf("%s: %s: HTTP %d", s.provider, operation, resp.StatusCode)
	}
	t.Fatalf("%s: %s: SDK request failed (%T)", s.provider, operation, err)
}

type repository struct {
	*forge.GitRepository
	server *server
	name   string
}

func (s *server) newRepo(t *testing.T) *repository {
	t.Helper()
	name := "acceptance-" + strings.ToLower(rand.Text())
	repo, resp, err := s.admin.Admin.CreateRepo(t.Context(), s.owner, gitea.CreateRepoOption{
		Name: name, Private: true, DefaultBranch: "main",
	})
	s.check(t, "create private repo", resp, err)
	require.True(t, repo.Private)
	require.Equal(t, s.owner, repo.Owner.UserName)
	r := &repository{forge.NewGitRepository(t, s.url, s.owner, s.token), s, name}
	r.Seed(t, s.url+"/"+s.owner+"/"+name+".git")
	r.awaitBranches(t, "main", r.Base)
	_, resp, err = s.sdkClient(t).Repositories.GetRepo(t.Context(), s.owner, name)
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		s.apiFailure(t, "anonymous read of private repo (want HTTP 404)", resp, err)
	}
	return r
}

// Git can acknowledge a push before the API sees it. Retry reads, never the push.
func (r *repository) awaitBranches(t *testing.T, branches ...string) map[string]string {
	t.Helper()
	s := r.server
	refs := r.Refs(t)
	for _, name := range branches {
		require.NotEmpty(t, refs["refs/heads/"+name], "remote branch %s", name)
	}
	for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		repo, resp, err := s.client.Repositories.GetRepo(t.Context(), s.owner, r.name)
		ready := err == nil && repo != nil
		observation := "default branch main"
		if ready {
			require.True(t, repo.Private, "fixture %s unexpectedly public", r.name)
			ready = !repo.Empty && repo.DefaultBranch == "main"
		}
		if ready {
			for _, name := range branches {
				var b *gitea.Branch
				b, resp, err = s.client.Repositories.GetRepoBranch(t.Context(), s.owner, r.name, name)
				want := refs["refs/heads/"+name]
				if err != nil || b == nil || b.Commit == nil || b.Name != name || b.Commit.ID != want {
					observation, ready = name+" at "+want, false
					break
				}
			}
		}
		if ready {
			return refs
		}
		if time.Now().After(deadline) {
			s.check(t, "read "+observation, resp, err)
			t.Fatalf("%s: API did not see %s", r.name, observation)
		}
	}
}

func newCLI(t *testing.T, s *server) *cli.Runner {
	t.Helper()
	return newCLIWithCredentials(t, s.owner, s.token)
}

func newCLIWithCredentials(t *testing.T, user, token string) *cli.Runner {
	t.Helper()
	return cli.NewWithCredentials(t, user, token, "UPDATECLI_GITEA_USERNAME="+user, "UPDATECLI_GITEA_TOKEN="+token)
}

func requestManifest(t *testing.T, r *repository, id, version string, spec map[string]any) string {
	t.Helper()
	return cli.Manifest(t, requestConfig(r, id, version, spec))
}

func requestConfig(r *repository, id, version string, spec map[string]any) map[string]any {
	scm := map[string]any{"kind": "gitea", "spec": map[string]any{
		"url": r.server.url, "owner": r.server.owner, "repository": r.name, "branch": r.Base,
	}}
	action := map[string]any{"kind": "gitea/pullrequest", "scmid": "origin"}
	if len(spec) > 0 {
		action["spec"] = spec
	}
	return forge.PublicationConfig(id, version, scm, action)
}
