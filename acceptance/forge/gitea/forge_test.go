//go:build acceptance

package gitea_test

import (
	"os"
	"testing"

	"github.com/updatecli/updatecli/acceptance/forge"
	"github.com/updatecli/updatecli/acceptance/internal/conformance"
)

func TestCapabilities(t *testing.T) {
	conformance.Observe(t)
	selected, set := os.LookupEnv("UPDATECLI_TEST_FORGE")
	if set && selected != "gitea" && selected != "forgejo" {
		t.Fatalf("unknown Gitea provider fixture %q", selected)
	}
	for _, name := range []string{"gitea", "forgejo"} {
		if set && selected != name {
			continue
		}
		t.Run(name, func(t *testing.T) {
			s := startServer(t, name)
			for _, test := range []struct {
				name string
				run  func(*testing.T, *server)
			}{
				{"authentication", testAuthentication},
				{"publish_update", func(t *testing.T, s *server) { forge.RunPublishUpdate(t, s.newPublication) }},
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
				{"gitea_actions_run_link", testWorkflowRunLink},
			} {
				t.Run(test.name, func(t *testing.T) { test.run(t, s) })
			}
		})
	}
}
