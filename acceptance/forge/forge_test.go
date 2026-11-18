//go:build acceptance

package forge

import (
	"os"
	"testing"
)

type capability struct {
	name string
	run  func(*testing.T, *server)
}

var profiles = []struct {
	server       string
	capabilities []capability
}{
	{"gitea", giteaCapabilities},
	{"forgejo", giteaCapabilities},
}

var giteaCapabilities = []capability{
	{"authentication", testAuthentication},
	{"publish_update", testPublish},
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

func TestForge(t *testing.T) {
	failOnSkip(t)
	selected, specified := os.LookupEnv("UPDATECLI_TEST_FORGE")
	matched := false
	for _, profile := range profiles {
		if specified && selected != profile.server {
			continue
		}
		matched = true
		runRequired(t, profile.server, func(t *testing.T) {
			if len(profile.capabilities) == 0 {
				t.Fatal("no capabilities enrolled")
			}
			s := startServer(t, profile.server)
			for _, c := range profile.capabilities {
				runRequired(t, c.name, func(t *testing.T) { c.run(t, s) })
			}
		})
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
