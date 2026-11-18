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
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
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

	const port = "80/tcp"
	t.Logf("starting GitLab CE %s", gitLabImage)
	c, err := testcontainers.Run(ctx, gitLabImage,
		// Report state on failure rather than dumping raw container logs.
		testcontainers.WithLogger(log.New(io.Discard, "", 0)),
		testcontainers.WithExposedPorts(port),
		testcontainers.WithHostConfigModifier(func(config *container.HostConfig) {
			config.ShmSize = 256 << 20
			config.PortBindings = network.PortMap{
				network.MustParsePort(port): {{HostIP: netip.MustParseAddr("127.0.0.1")}},
			}
			// 19.3's scheduled version check ignores version_check_enabled
			// and hardcodes this hostname. Keep that request in the container.
			config.ExtraHosts = []string{"version.gitlab.com:127.0.0.1"}
		}),
		testcontainers.WithEnv(map[string]string{
			"GITLAB_DISABLE_OPENSSH": "true",
			"GITLAB_SKIP_TAIL_LOGS":  "true",
			"GITLAB_OMNIBUS_CONFIG":  gitLabOmnibusConfig,
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
	mapped, err := c.MappedPort(ctx, port)
	if err != nil {
		t.Fatalf("get GitLab CE fixture HTTP port: %v", err)
	}
	g := &gitlabFixture{
		// The API and Git use Docker's mapping, not GitLab's advertised URL.
		url:   "http://" + net.JoinHostPort("127.0.0.1", mapped.Port()),
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
external_url 'http://localhost'
nginx['listen_port'] = 80
puma['worker_processes'] = 0
prometheus_monitoring['enable'] = false
logrotate['enable'] = false
gitlab_kas['enable'] = false
gitlab_rails['gitlab_email_enabled'] = false
gitlab_rails['display_initial_root_password'] = false
gitlab_rails['store_initial_root_password'] = false
gitlab_rails['initial_gitlab_product_usage_data'] = false
gitlab_rails['usage_ping_enabled'] = false
gitlab_rails['rake_cache_clear'] = false
`

func (g *gitlabFixture) bootstrapActor(t *testing.T, ctx context.Context, c testcontainers.Container) {
	t.Helper()
	// Use the stock Rails runner for initial root bootstrap only. GitLab
	// generates the PAT itself; it is never in process arguments or the environment.
	const script = `
ApplicationSetting.current.update!(
  signup_enabled: false, version_check_enabled: false, import_sources: [],
  shared_runners_enabled: false, allow_runner_registration_token: false,
  update_runner_versions_enabled: false)
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
	_, response, err = root.Users.CurrentUser(api)
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		gitLabAPIFailure(t, "confirm revoked root PAT cannot authenticate (want HTTP 401)", response, err)
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
		BuildsAccessLevel:    gitlabsdk.Ptr(gitlabsdk.DisabledAccessControl),
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
