//go:build acceptance

package forge

import (
	"os"
	"testing"

	"github.com/updatecli/updatecli/acceptance/internal/conformance"
)

var profiles = []conformance.Profile{
	giteaProfile("gitea"),
	giteaProfile("forgejo"),
	conformance.NewProfile("gitlab", startGitLab, []conformance.Capability[*gitlabFixture]{
		publishUpdate[*gitlabFixture](),
	},
		conformance.Unavailable{
			ID: "workflow_discovery", Status: conformance.NotApplicable,
			// testWorkflowDiscovery seeds .gitea/.forgejo workflow and composite-action files.
			Reason: "This contract is for Gitea/Forgejo Actions files; GitLab CI uses .gitlab-ci.yml, not that Actions dialect.",
		},
		conformance.Unavailable{
			ID: "gitea_actions_run_link", Status: conformance.NotApplicable,
			Reason: "GitLab CI uses its own job URL rather than the Gitea Actions run-number URL.",
		},
		conformance.Unavailable{
			ID: "release_changelog", Status: conformance.Unimplemented,
			Reason: "Updatecli's gitlab/release Changelog method returns nil.",
		},
	),
}

func giteaProfile(id string) conformance.Profile {
	return conformance.NewProfile(id, func(t *testing.T) *server { return startServer(t, id) },
		[]conformance.Capability[*server]{
			{ID: "authentication", Run: testAuthentication},
			publishUpdate[*server](),
			{ID: "publish_review_metadata", Run: testPublishReviewMetadata},
			{ID: "request_safety", Run: testRequestSafety},
			{ID: "close_obsolete_request", Run: testClean},
			{ID: "fork_isolation", Run: testFork},
			{ID: "server_automerge", Run: testProtectedMerge},
			{ID: "manual_merge", Run: testManualMerge},
			{ID: "draft_request", Run: testDraft},
			{ID: "git_scm", Run: testSCM},
			{ID: "branch_resource", Run: testBranches},
			{ID: "tag_resource", Run: testTags},
			{ID: "release_resource", Run: testReleases},
			{ID: "workflow_discovery", Run: testWorkflowDiscovery},
			{ID: "gitea_actions_run_link", Run: testWorkflowRunLink},
		},
		conformance.Unavailable{
			ID: "release_changelog", Status: conformance.Unimplemented,
			Reason: "Updatecli's gitea/release Changelog method returns nil.",
		},
		conformance.Unavailable{
			ID: "request_labels", Status: conformance.Unimplemented,
			Reason: "Updatecli's gitea/pullrequest action has no labels setting.",
		},
		conformance.Unavailable{
			ID: "request_team_reviewers", Status: conformance.Unimplemented,
			Reason: "Updatecli's gitea/pullrequest action requests reviews from user accounts only.",
		},
		conformance.Unavailable{
			ID: "publish_from_fork", Status: conformance.Unimplemented,
			Reason: "Updatecli's gitea/pullrequest action requires both branches in one repository and ignores existing PRs from forks.",
		},
		conformance.Unavailable{
			ID: "private_native_ref_targets", Status: conformance.KnownIssue,
			Reason: "Private gitbranch creation and managed gittag creation can check out anonymously before the push; see known_private_refs_test.go.",
		},
		conformance.Unavailable{
			ID: "repository_search", Status: conformance.Unimplemented,
			Reason: "Updatecli registers no giteasearch SCM.",
		},
	)
}

func TestForge(t *testing.T) {
	selected, specified := os.LookupEnv("UPDATECLI_TEST_FORGE")
	if specified && selected == "" {
		t.Fatal("UPDATECLI_TEST_FORGE is set but empty")
	}
	conformance.Run(t, selected, os.Getenv("UPDATECLI_TEST_REPORT"), profiles...)
}

func runRequired(t *testing.T, name string, f func(*testing.T)) {
	t.Helper()
	conformance.Required(t, name, f)
}
