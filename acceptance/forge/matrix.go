// Package forge defines the capabilities compared across forge providers.
package forge

import "github.com/updatecli/updatecli/acceptance/internal/conformance"

// Gitea and Forgejo exercise the same Updatecli provider. GitLab currently
// shares only the publication contract; a missing test is not a product gap.
var Matrix = conformance.Matrix{
	Family:   "forge",
	Profiles: []string{"gitea", "forgejo", "gitlab"},
	Rows: []conformance.Row{
		row("authentication", "Private and anonymous access", required, untested),
		row("publish_update", "Publish and edit a request", required, required),
		row("publish_review_metadata", "Preserve reviews and assignees", required, untested),
		row("request_safety", "Drafts, reports and rejected reviews", required, untested),
		row("close_obsolete_request", "Close an obsolete request", required, untested),
		row("fork_isolation", "Leave another fork's request alone", required, untested),
		row("server_automerge", "Schedule and complete a protected merge", required, untested),
		row("manual_merge", "Leave a manual merge open", required, untested),
		row("draft_request", "Keep a draft open", required, untested),
		row("git_scm", "Check out and publish Git changes", required, untested),
		row("branch_resource", "Select and check branches", required, untested),
		row("tag_resource", "Select and check tags", required, untested),
		row("release_resource", "Select, check and publish releases", required, untested),
		row("workflow_discovery", "Discover Actions dependencies", required, na("GitLab CI uses a different workflow format.")),
		row("gitea_actions_run_link", "Link the Actions run number", required, na("GitLab reports its own CI job URL.")),
		row("release_changelog", "Extract release notes", gap("Release changelog not implemented."), gap("Release changelog not implemented.")),
		row("request_labels", "Set request labels", gap("No labels setting on the action."), untested),
		row("request_team_reviewers", "Request team reviews", gap("The action accepts individual reviewers only."), untested),
		row("publish_from_fork", "Publish a request from a fork", gap("The action expects branches in one repository."), untested),
		row("private_native_ref_targets", "Create native refs in a private repo",
			conformance.Cell{Status: conformance.KnownIssue, Reason: "Native ref targets can check out without credentials."}, untested),
		row("repository_search", "Expand a repository search", gap("No giteasearch SCM at this revision."), untested),
	},
}

var (
	required = conformance.Cell{Status: conformance.Required}
	untested = conformance.Cell{}
)

func row(id, label string, gitea, gitlab conformance.Cell) conformance.Row {
	return conformance.Row{ID: id, Label: label, Cells: []conformance.Cell{gitea, gitea, gitlab}}
}

func na(reason string) conformance.Cell {
	return conformance.Cell{Status: conformance.NotApplicable, Reason: reason}
}
func gap(reason string) conformance.Cell {
	return conformance.Cell{Status: conformance.Gap, Reason: reason}
}
