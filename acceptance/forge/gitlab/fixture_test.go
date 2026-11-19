//go:build acceptance

package gitlab

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
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
	actorName     = "fixture-bot"
)

type fixture struct {
	url, token        string
	ownerID           int64
	client, anonymous *gitlabsdk.Client
	httpClient        *http.Client
}

func start(t *testing.T) *fixture {
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
	g := &fixture{
		// The API and Git use Docker's mapping, not GitLab's advertised URL.
		url: "http://" + net.JoinHostPort("127.0.0.1", mapped.Port()),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{}, // No inherited proxies.
		},
	}
	t.Cleanup(g.httpClient.CloseIdleConnections)

	g.anonymous = g.sdkClient(t, "")
	_, resp, err := g.anonymous.Users.CurrentUser(gitlabsdk.WithContext(ctx))
	expectRejection(t, "anonymous current-user lookup", resp, err, http.StatusUnauthorized)
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

func (g *fixture) bootstrapActor(t *testing.T, ctx context.Context, c testcontainers.Container) {
	t.Helper()
	// Create the actor locally via GitLab's Rails service. Only its PAT comes
	// back over Docker exec; no administrator credential is issued.
	const script = `
ApplicationSetting.current.update!(
  signup_enabled: false, version_check_enabled: false, import_sources: [],
  shared_runners_enabled: false, allow_runner_registration_token: false,
  update_runner_versions_enabled: false)
result = Users::CreateService.new(User.find_by!(username: 'root', admin: true),
  username: '` + actorName + `', email: '` + actorName + `@example.test', name: 'Acceptance Fixture',
  organization_id: Organizations::Organization.first.id,
  admin: false, force_random_password: true, skip_confirmation: true).execute
actor = result.payload[:user]
abort 'fixture actor creation failed' unless result.success? && actor && !actor.admin?
pat = actor.personal_access_tokens.create!(
  name: 'acceptance-project-actor', scopes: ['api'], expires_at: Date.current + 1)
puts "ACCEPTANCE_ACTOR_PAT=#{pat.token}"
`
	code, output, err := c.Exec(ctx, []string{"gitlab-rails", "runner", script}, tcexec.Multiplexed())
	if err != nil || code != 0 || output == nil {
		t.Fatalf("bootstrap GitLab CE project actor: Docker exec failed (exit %d)", code)
	}
	out, err := io.ReadAll(io.LimitReader(output, 32<<10))
	if err != nil {
		t.Fatal("read GitLab CE bootstrap result")
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if token, ok := strings.CutPrefix(line, "ACCEPTANCE_ACTOR_PAT="); ok {
			g.token = strings.TrimSpace(token)
			break
		}
	}
	if !strings.HasPrefix(g.token, "glpat-") || len(g.token) <= len("glpat-") || len(g.token) > 256 {
		t.Fatal("missing GitLab CE project actor PAT")
	}

	g.client = g.sdkClient(t, g.token)
	api := gitlabsdk.WithContext(ctx)
	me, response, err := g.client.Users.CurrentUser(api)
	requireAPI(t, "authenticate project actor", response, err)
	if me == nil || me.ID <= 0 || me.Username != actorName || me.State != "active" || me.IsAdmin {
		t.Fatal("GitLab CE project actor is not the expected active non-admin user")
	}
	g.ownerID = me.ID
	_, response, err = g.client.Settings.GetSettings(api)
	expectRejection(t, "project actor reading admin settings", response, err, http.StatusForbidden, http.StatusUnauthorized)
	pat, response, err := g.client.PersonalAccessTokens.GetSinglePersonalAccessToken(api)
	requireAPI(t, "read back project actor PAT", response, err)
	if pat == nil || !pat.Active || pat.Revoked || pat.UserID != g.ownerID ||
		!slices.Equal(pat.Scopes, []string{"api"}) || pat.ExpiresAt == nil || time.Until(time.Time(*pat.ExpiresAt)) > 24*time.Hour {
		t.Fatal("GitLab CE project actor PAT must be active, short-lived, owned by the actor and API-only")
	}
	version, response, err := g.client.Version.GetVersion(api)
	requireAPI(t, "read GitLab CE version", response, err)
	if version == nil || version.Version != gitLabVersion {
		t.Fatalf("fixture did not report GitLab version %s", gitLabVersion)
	}
}

func (g *fixture) newProject(t *testing.T) *gitlabsdk.Project {
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
	requireAPI(t, "create empty private project", response, err)
	if created == nil || created.ID <= 0 {
		t.Fatal("GitLab CE did not return a project ID")
	}
	project, response, err := g.client.Projects.GetProject(created.ID, nil, api)
	requireAPI(t, "read back empty private project", response, err)
	if project == nil || project.ID != created.ID || !project.EmptyRepo || project.Visibility != gitlabsdk.PrivateVisibility ||
		project.Path != name || project.PathWithNamespace != actorName+"/"+name || project.Namespace == nil || project.Namespace.Kind != "user" ||
		project.Owner == nil || project.Owner.ID != g.ownerID {
		t.Fatal("GitLab CE project is not empty, private, and in the actor's personal namespace")
	}
	_, response, err = g.anonymous.Projects.GetProject(project.ID, nil, api)
	expectRejection(t, "anonymous private project read", response, err, http.StatusNotFound)
	return project
}

func (g *fixture) sdkClient(t *testing.T, token string) *gitlabsdk.Client {
	t.Helper()
	// SDK-level retries also cover writes; only poll explicitly chosen reads.
	client, err := gitlabsdk.NewClient(token, gitlabsdk.WithBaseURL(g.url), gitlabsdk.WithHTTPClient(g.httpClient),
		gitlabsdk.WithoutRetries(), gitlabsdk.WithCustomLogger(nil))
	if err != nil {
		t.Fatal("construct GitLab CE SDK client")
	}
	return client
}

func requireAPI(t *testing.T, action string, response *gitlabsdk.Response, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("GitLab CE: %s: %s", action, apiStatus(response, err))
	}
}

func expectRejection(t *testing.T, action string, response *gitlabsdk.Response, err error, codes ...int) {
	t.Helper()
	if err == nil || response == nil || response.Response == nil || !slices.Contains(codes, response.StatusCode) {
		t.Fatalf("GitLab CE: %s: got %s, want rejection with HTTP status in %v", action, apiStatus(response, err), codes)
	}
}

// SDK errors may include response bodies. Report only the status or error type.
func apiStatus(response *gitlabsdk.Response, err error) string {
	if response != nil && response.Response != nil {
		return fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	return fmt.Sprintf("SDK request result (%T)", err)
}
