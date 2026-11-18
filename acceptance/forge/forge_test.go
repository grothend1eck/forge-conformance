//go:build acceptance

package forge

import (
	"os"
	"testing"
)

type capability[F any] struct {
	name string
	run  func(*testing.T, F)
}

var profiles = []struct {
	name string
	run  func(*testing.T)
}{
	{"gitea", func(t *testing.T) { runProfile(t, startServer(t, "gitea"), giteaCapabilities) }},
	{"forgejo", func(t *testing.T) { runProfile(t, startServer(t, "forgejo"), giteaCapabilities) }},
	{"gitlab", func(t *testing.T) { runProfile(t, startGitLab(t), gitlabCapabilities) }},
}

var giteaCapabilities = []capability[*server]{
	{"authentication", testAuthentication},
	publishUpdate[*server](),
	{"publish_review_metadata", testPublishReviewMetadata},
	{"request_safety", testRequestSafety},
	{"close_obsolete_request", testClean},
	{"fork_isolation", testFork},
	{"server_automerge", testProtectedMerge},
	{"manual_merge", testManualMerge},
	{"draft_request", testDraft},
	{"git_scm", testSCM},
	{"branch_resource", testBranches},
	{"tag_resource", testTags},
	{"release_resource", testReleases},
	{"workflow_discovery", testWorkflowDiscovery},
}

var gitlabCapabilities = []capability[*gitlabFixture]{
	publishUpdate[*gitlabFixture](),
}

func runProfile[F any](t *testing.T, fixture F, capabilities []capability[F]) {
	t.Helper()
	if len(capabilities) == 0 {
		t.Fatal("no capabilities enrolled")
	}
	for _, c := range capabilities {
		runRequired(t, c.name, func(t *testing.T) { c.run(t, fixture) })
	}
}

func TestForge(t *testing.T) {
	failOnSkip(t)
	selected, specified := os.LookupEnv("UPDATECLI_TEST_FORGE")
	matched := false
	for _, profile := range profiles {
		if specified && selected != profile.name {
			continue
		}
		matched = true
		runRequired(t, profile.name, profile.run)
	}
	if !matched {
		t.Fatalf("no forge profile matches UPDATECLI_TEST_FORGE=%q", selected)
	}
}

// A skipped enrolled case must fail the check. Go's -run filter still works:
// tests outside the filter never enter the callback.
func runRequired(t *testing.T, name string, f func(*testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		failOnSkip(t)
		f(t)
	})
}

func failOnSkip(t *testing.T) {
	t.Cleanup(func() {
		if t.Skipped() {
			t.Error("enrolled forge test was skipped")
		}
	})
}
