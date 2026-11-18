//go:build acceptance

package forge

import (
	"os"
	"testing"
)

// Gitea and Forgejo run the same Updatecli plugin. An addition here enrolls both.
var capabilities = []struct {
	name string
	run  func(*testing.T, *server)
}{
	{"publish_update", testPublish},
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
	selected := os.Getenv("UPDATECLI_TEST_FORGE")
	if selected != "" && selected != "gitea" && selected != "forgejo" {
		t.Fatalf("unknown forge %q; choose gitea or forgejo", selected)
	}
	for _, name := range []string{"gitea", "forgejo"} {
		if selected != "" && selected != name {
			continue
		}
		t.Run(name, func(t *testing.T) {
			s := startServer(t, name)
			for _, capability := range capabilities {
				t.Run(capability.name, func(t *testing.T) { capability.run(t, s) })
			}
		})
	}
}
