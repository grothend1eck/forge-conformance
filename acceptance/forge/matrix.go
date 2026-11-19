// Package forge defines the capabilities compared across forge providers.
package forge

import "github.com/updatecli/updatecli/acceptance/internal/conformance"

const (
	publishUpdate    = "publish_update"
	editRequestTitle = "edit_request_title"
	draftTitleMarker = "draft_title_marker"
)

// Gitea and Forgejo share an Updatecli provider. GitLab is enrolled only for
// publication and title edits. Omitted cells are unassessed, not product gaps.
var Matrix = conformance.Matrix{
	Family:   "forge",
	Profiles: []string{"gitea", "forgejo", "gitlab"},
	Rows: []conformance.Row{
		giteaPair("authentication", "Private and anonymous access", supported),
		withGitLab(publishUpdate, "Publish and edit a request", supported, supported),
		withGitLab(editRequestTitle, "Edit an existing request's title", supported, supported),
		withGitLab(draftTitleMarker, "Keep a title-marked draft on edit", supported,
			gap("The Updatecli action replaces the title without preserving the draft marker.")),
		withGitLab("publish_review_metadata", "Preserve reviews and assignees", supported,
			gap("The Updatecli action sends replacement reviewer and assignee lists on edit.")),
		giteaPair("request_safety", "Persist reports, honor branch overrides and stop automerge on rejected reviews", supported),
		withGitLab("close_obsolete_request", "Close an obsolete request", supported,
			gap("The Updatecli merge request action does not implement cleanup.")),
		withGitLab("fork_isolation", "Leave another fork's request alone", supported,
			gap("The Updatecli action matches branch names without checking the source project.")),
		giteaPair("server_automerge", "Schedule and complete a protected merge", supported),
		giteaPair("manual_merge", "Leave a manual merge open", supported),
		giteaPair("draft_request", "Keep a new draft open with automerge requested", supported),
		giteaPair("git_scm", "Check out and publish Git changes", supported),
		giteaPair("branch_resource", "Select and check branches", supported),
		giteaPair("tag_resource", "Select and check tags", supported),
		withGitLab("release_resource", "Select, check and publish releases", supported,
			gap("The Updatecli target omits the required ref when publishing a release for a new tag.")),
		withGitLab("workflow_discovery", "Discover Actions uses: dependencies", supported,
			na("GitLab CI uses includes and components, not Actions uses: entries.")),
		{
			ID:    "gitea_actions_run_link",
			Label: "Link the Actions run by repository index",
			Cells: map[string]conformance.Cell{
				"gitea":   supported,
				"forgejo": gap("Forgejo Runner 13.2 sets FORGEJO_ACTIONS; Updatecli does not select the index-based link for that environment."),
				"gitlab":  na("GitLab CI has its own job links, not Actions run-index links."),
			},
		},
		withGitLab("release_changelog", "Extract release notes",
			gap("Both forges expose release notes; the Updatecli resource does not extract them."),
			gap("GitLab exposes release descriptions; the Updatecli resource does not extract them.")),
		giteaPair("request_labels", "Set request labels",
			gap("The forge API accepts labels; the Updatecli action has no label setting.")),
		withGitLab("request_team_reviewers", "Request review from a team",
			gap("The forge API accepts organization teams; the Updatecli action sends individual reviewers only."),
			na("GitLab CE review requests address individual users, not groups.")),
		withGitLab("publish_from_fork", "Publish a request from a fork",
			gap("The forge API accepts fork heads; the Updatecli action expects both branches in one repository."),
			gap("The Updatecli action checks both branches in one project and does not send the target project ID.")),
		giteaPair("private_native_ref_targets", "Create native refs in a private repo",
			gap("Known issue: native branch creation and tag creation with a managed working branch can check out without credentials.")),
		giteaPair("repository_search", "Expand a repository search",
			gap("The forge API can search repositories; Updatecli has no Gitea or Forgejo search SCM at this revision.")),
	},
}

var supported = conformance.Cell{Status: conformance.Supported}

func giteaPair(id, label string, cell conformance.Cell) conformance.Row {
	return conformance.Row{ID: id, Label: label, Cells: map[string]conformance.Cell{"gitea": cell, "forgejo": cell}}
}

func withGitLab(id, label string, gitea, gitlab conformance.Cell) conformance.Row {
	row := giteaPair(id, label, gitea)
	row.Cells["gitlab"] = gitlab
	return row
}

func na(reason string) conformance.Cell {
	return conformance.Cell{Status: conformance.NotApplicable, Reason: reason}
}

func gap(reason string) conformance.Cell {
	return conformance.Cell{Status: conformance.Gap, Reason: reason}
}
