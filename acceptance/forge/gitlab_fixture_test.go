//go:build acceptance

package forge

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	gitlabsdk "gitlab.com/gitlab-org/api/client-go"
)

const (
	gitLabVersion = "19.3.9"
	gitLabImage   = "docker.io/gitlab/gitlab-ce:" + gitLabVersion + "-ce.0@sha256:7cb320e8da464484b270b5442d2e626d08cb12981219df703dd0caccee78072e"
)

type gitlabFixture struct {
	url    string
	client *gitlabsdk.Client
	owner  string
	token  string

	httpClient *http.Client
	ownerID    int64
}

func startGitLab(t *testing.T) *gitlabFixture {
	t.Helper()
	// The official image is public; never look up an operator's registry credentials.
	t.Setenv("DOCKER_AUTH_CONFIG", "{}")
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	// Require a local engine: the host forwards loopback HTTP to an internal
	// Docker bridge (Docker does not publish ports on an internal network).
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("connect to local Docker for GitLab CE: %v", err)
	}
	dockerHost := docker.DaemonHost()
	_ = docker.Close()
	if !strings.HasPrefix(dockerHost, "unix://") {
		t.Fatal("GitLab CE fixture requires a local Docker Unix socket")
	}
	network, err := tcnetwork.New(ctx, tcnetwork.WithInternal())
	if err != nil {
		t.Fatalf("create internal network for GitLab CE: %v", err)
	}
	testcontainers.CleanupNetwork(t, network)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on GitLab CE fixture loopback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	g := &gitlabFixture{
		url:   "http://" + listener.Addr().String(),
		owner: "fixture-bot",
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{}, // No inherited proxies.
		},
	}
	t.Cleanup(g.httpClient.CloseIdleConnections)

	t.Logf("starting GitLab CE %s", gitLabImage)
	quiet := log.New(io.Discard, "", 0)
	c, err := testcontainers.Run(ctx, gitLabImage,
		testcontainers.WithLogger(quiet),
		tcnetwork.WithNetwork(nil, network),
		testcontainers.WithHostConfigModifier(func(config *container.HostConfig) {
			config.ShmSize = 256 << 20
		}),
		testcontainers.WithEnv(map[string]string{
			"GITLAB_DISABLE_OPENSSH": "true",
			"GITLAB_SKIP_TAIL_LOGS":  "true",
			"GITLAB_OMNIBUS_CONFIG":  "external_url '" + g.url + "'\n" + gitLabOmnibusConfig,
		}),
		testcontainers.WithWaitStrategyAndDeadline(8*time.Minute,
			wait.ForExec([]string{"test", "-f", "/var/opt/gitlab/bootstrapped"}).
				WithPollInterval(2*time.Second).WithStartupTimeout(7*time.Minute),
			// Full readiness checks are restricted to container-local requests.
			wait.ForExec([]string{"curl", "--fail", "--silent", "--max-time", "5", "http://127.0.0.1/-/readiness?all=1"}).
				WithResponseMatcher(func(body io.Reader) bool {
					var status struct{ Status string }
					return json.NewDecoder(body).Decode(&status) == nil && status.Status == "ok"
				}).
				WithPollInterval(2*time.Second).WithStartupTimeout(3*time.Minute),
			wait.ForExec([]string{"/opt/gitlab/embedded/bin/sv", "status", "/opt/gitlab/service/sidekiq"}).
				WithResponseMatcher(func(body io.Reader) bool {
					status, err := io.ReadAll(io.LimitReader(body, 512))
					return err == nil && strings.HasPrefix(string(status), "run:")
				}).
				WithPollInterval(time.Second).WithStartupTimeout(30*time.Second),
		),
	)
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		if c != nil {
			inspect, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			if state, stateErr := c.State(inspect); stateErr == nil && state != nil {
				t.Logf("GitLab CE container state: %s (exit %d, OOM killed: %t)", state.Status, state.ExitCode, state.OOMKilled)
			}
		}
		t.Fatalf("start GitLab CE: %v", err)
	}
	ip, err := c.ContainerIP(ctx)
	if err != nil || net.ParseIP(ip) == nil {
		t.Fatal("GitLab CE must have exactly one Docker network with an IP address")
	}
	backend := &url.URL{Scheme: "http", Host: net.JoinHostPort(ip, "80")}
	transport := &http.Transport{}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		ErrorLog:  quiet,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(backend)
			r.Out.Host = listener.Addr().String()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "GitLab CE fixture backend unavailable", http.StatusBadGateway)
		},
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second}
	t.Cleanup(transport.CloseIdleConnections)
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	_, response, err := g.sdkClient(t, "").Users.CurrentUser(gitlabsdk.WithContext(ctx))
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		gitLabAPIFailure(t, "check anonymous API readiness (want HTTP 401)", response, err)
	}
	t.Logf("GitLab CE ready in %s", time.Since(started).Round(time.Millisecond))
	g.bootstrapActor(t, ctx, c)
	t.Logf("GitLab CE %s with verified project actor ready in %s", gitLabVersion, time.Since(started).Round(time.Millisecond))
	return g
}

const gitLabOmnibusConfig = `
nginx['listen_port'] = 80
puma['worker_processes'] = 0
prometheus_monitoring['enable'] = false
logrotate['enable'] = false
gitlab_kas['enable'] = false
gitlab_rails['gitlab_email_enabled'] = false
gitlab_rails['display_initial_root_password'] = false
gitlab_rails['store_initial_root_password'] = false
gitlab_rails['initial_gitlab_product_usage_data'] = false
gitlab_rails['rake_cache_clear'] = false
`

func (g *gitlabFixture) bootstrapActor(t *testing.T, ctx context.Context, c testcontainers.Container) {
	t.Helper()
	// Use the stock Rails runner for initial root bootstrap only. GitLab
	// generates the PAT itself; it is never in process arguments or the environment.
	const script = `
ApplicationSetting.current.update!(signup_enabled: false)
pat = User.find_by!(username: 'root').personal_access_tokens.create!(
  name: 'acceptance-bootstrap', scopes: ['api'], expires_at: Date.current + 1)
puts "ACCEPTANCE_BOOTSTRAP=#{pat.token}"
`
	code, output, err := c.Exec(ctx, []string{"gitlab-rails", "runner", script}, tcexec.Multiplexed())
	if err != nil || code != 0 || output == nil {
		t.Fatalf("bootstrap GitLab CE root credential: Docker exec failed (exit %d)", code)
	}
	bytes, err := io.ReadAll(io.LimitReader(output, 32<<10))
	if err != nil {
		t.Fatal("read GitLab CE bootstrap result")
	}
	_, raw, found := strings.Cut(string(bytes), "ACCEPTANCE_BOOTSTRAP=")
	rootToken, _, _ := strings.Cut(raw, "\n")
	if !found || !strings.HasPrefix(rootToken, "glpat-") || len(rootToken) > 256 {
		t.Fatal("missing GitLab CE bootstrap credential")
	}

	root := g.sdkClient(t, rootToken)
	api := gitlabsdk.WithContext(ctx)
	actor, response, err := root.Users.CreateUser(&gitlabsdk.CreateUserOptions{
		Username:            &g.owner,
		Name:                gitlabsdk.Ptr("Acceptance Fixture"),
		Email:               gitlabsdk.Ptr(g.owner + "@example.test"),
		ForceRandomPassword: gitlabsdk.Ptr(true),
		SkipConfirmation:    gitlabsdk.Ptr(true),
		Admin:               gitlabsdk.Ptr(false),
	}, api)
	if err != nil {
		gitLabAPIFailure(t, "create project actor", response, err)
	}
	// GitLab's api scope includes read/write Git over HTTP as well as API
	// writes. A second write_repository scope would not narrow it.
	scopes := []string{"api"}
	credential, response, err := root.Users.CreatePersonalAccessToken(actor.ID, &gitlabsdk.CreatePersonalAccessTokenOptions{
		Name:      gitlabsdk.Ptr("acceptance-project-actor"),
		Scopes:    &scopes,
		ExpiresAt: gitlabsdk.Ptr(gitlabsdk.ISOTime(time.Now().UTC().AddDate(0, 0, 1))),
	}, api)
	if err != nil {
		gitLabAPIFailure(t, "create project actor PAT", response, err)
	}
	if response, err := root.PersonalAccessTokens.RevokePersonalAccessTokenSelf(api); err != nil {
		gitLabAPIFailure(t, "revoke root bootstrap PAT", response, err)
	}
	if credential.Token == "" || credential.Token == rootToken {
		t.Fatal("GitLab CE did not issue a separate actor PAT")
	}
	g.token = credential.Token
	g.client = g.sdkClient(t, g.token)
	g.ownerID = actor.ID

	me, response, err := g.client.Users.CurrentUser(api)
	if err != nil {
		gitLabAPIFailure(t, "authenticate project actor", response, err)
	}
	if me.ID != actor.ID || me.Username != g.owner || me.State != "active" || me.IsAdmin || actor.IsAdmin {
		t.Fatal("GitLab CE project actor is not the expected active non-admin user")
	}
	_, response, err = g.client.Settings.GetSettings(api)
	if err == nil || response == nil || (response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusUnauthorized) {
		gitLabAPIFailure(t, "confirm project actor cannot read admin settings (want HTTP 403 or 401)", response, err)
	}
	pat, response, err := g.client.PersonalAccessTokens.GetSinglePersonalAccessToken(api)
	if err != nil {
		gitLabAPIFailure(t, "read back project actor PAT", response, err)
	}
	if !pat.Active || pat.Revoked || pat.UserID != me.ID || !slices.Equal(pat.Scopes, scopes) {
		t.Fatal("GitLab CE project actor PAT is not active, owned by the actor, and API-only")
	}
	version, response, err := g.client.Version.GetVersion(api)
	if err != nil {
		gitLabAPIFailure(t, "read GitLab CE version", response, err)
	}
	if version.Version != gitLabVersion {
		t.Fatalf("unexpected GitLab version %q, want %q", version.Version, gitLabVersion)
	}
}

func (g *gitlabFixture) newProject(t *testing.T) *gitlabsdk.Project {
	t.Helper()
	name := "acceptance-" + strings.ToLower(rand.Text())
	api := gitlabsdk.WithContext(t.Context())
	created, response, err := g.client.Projects.CreateProject(&gitlabsdk.CreateProjectOptions{
		Name:                 &name,
		Visibility:           gitlabsdk.Ptr(gitlabsdk.PrivateVisibility),
		InitializeWithReadme: gitlabsdk.Ptr(false),
		AutoDevopsEnabled:    gitlabsdk.Ptr(false),
	}, api)
	if err != nil {
		gitLabAPIFailure(t, "create empty private project", response, err)
	}
	project, response, err := g.client.Projects.GetProject(created.ID, nil, api)
	if err != nil {
		gitLabAPIFailure(t, "read back empty private project", response, err)
	}
	if project.ID <= 0 || !project.EmptyRepo || project.Visibility != gitlabsdk.PrivateVisibility ||
		project.PathWithNamespace != g.owner+"/"+name || project.Namespace == nil || project.Namespace.Kind != "user" ||
		project.Owner == nil || project.Owner.ID != g.ownerID {
		t.Fatal("GitLab CE project is not empty, private, and in the actor's personal namespace")
	}
	_, response, err = g.sdkClient(t, "").Projects.GetProject(project.ID, nil, api)
	if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
		gitLabAPIFailure(t, "confirm anonymous project read is denied (want HTTP 404)", response, err)
	}
	return project
}

func (g *gitlabFixture) sdkClient(t *testing.T, token string) *gitlabsdk.Client {
	t.Helper()
	client, err := gitlabsdk.NewClient(token, gitlabsdk.WithBaseURL(g.url), gitlabsdk.WithHTTPClient(g.httpClient),
		gitlabsdk.WithoutRetries(), gitlabsdk.WithCustomLogger(nil))
	if err != nil {
		t.Fatal("construct GitLab CE SDK client")
	}
	return client
}

// SDK errors may include request or response data; do not log credential bodies.
func gitLabAPIFailure(t *testing.T, action string, response *gitlabsdk.Response, err error) {
	t.Helper()
	if response != nil && response.Response != nil {
		t.Fatalf("GitLab CE: %s: HTTP %d", action, response.StatusCode)
	}
	t.Fatalf("GitLab CE: %s: SDK request failed (%T)", action, err)
}
