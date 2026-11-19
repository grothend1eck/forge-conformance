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
			forge.Matrix.Run(t, name, forge.PublicationCases(s.newPublication), map[string]func(*testing.T){
				"authentication":          s.testAuthentication,
				"draft_title_marker":      s.testDraftSurvivesEdit,
				"publish_review_metadata": s.testPublishReviewMetadata,
				"request_safety":          s.testRequestSafety,
				"close_obsolete_request":  s.testClean,
				"fork_isolation":          s.testFork,
				"server_automerge":        s.testProtectedMerge,
				"manual_merge":            s.testManualMerge,
				"draft_request":           s.testDraft,
				"git_scm":                 s.testSCM,
				"branch_resource":         s.testBranches,
				"tag_resource":            s.testTags,
				"release_resource":        s.testReleases,
				"workflow_discovery":      s.testWorkflowDiscovery,
				"gitea_actions_run_link":  s.testGiteaActionsRunLink,
			})
		})
	}
}
