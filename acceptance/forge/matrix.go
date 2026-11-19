package forge

import "github.com/updatecli/updatecli/acceptance/internal/conformance"

const (
	publishUpdate    = "publish_update"
	editRequestTitle = "edit_request_title"
	draftTitleMarker = "draft_title_marker"
)

var (
	supported   = conformance.Cell{Status: conformance.Supported}
	unassessed  = conformance.Cell{}
	noChangelog = gap("Release notes aren't read.")
)

var Matrix = conformance.Matrix{
	Family:   "forge",
	Profiles: []string{"gitea", "forgejo", "gitlab"},
	// Columns in paired rows: Gitea/Forgejo, GitLab.
	Rows: []conformance.Row{
		paired("authentication", supported, unassessed),
		paired(publishUpdate, supported, supported),
		paired(editRequestTitle, supported, supported),
		paired(draftTitleMarker, supported, gap("Title edits drop the draft marker.")),
		paired("publish_review_metadata", supported, gap("Edits replace reviewers and assignees.")),
		paired("request_safety", supported, unassessed),
		paired("close_obsolete_request", supported, gap("The MR action has no cleanup step.")),
		paired("fork_isolation", supported, gap("Branch matching ignores the source project.")),
		paired("server_automerge", supported, unassessed),
		paired("manual_merge", supported, unassessed),
		paired("draft_request", supported, unassessed),
		paired("git_scm", supported, unassessed),
		paired("branch_resource", supported, unassessed),
		paired("tag_resource", supported, unassessed),
		paired("release_resource", supported, gap("New release tags need a ref; the target omits it.")),
		paired("workflow_discovery", supported,
			na("GitLab CI uses includes and components, not Actions uses: entries.")),
		{ID: "gitea_actions_run_link", Cells: map[string]conformance.Cell{
			"gitea":   supported,
			"forgejo": gap("Run links ignore FORGEJO_ACTIONS on Forgejo Runner 13.2."),
			"gitlab":  na("GitLab CI links use job IDs."),
		}},
		paired("release_changelog", noChangelog, noChangelog),
		paired("request_labels", gap("The PR action has no label setting."), unassessed),
		paired("request_team_reviewers",
			gap("The action only requests individual reviewers."),
			na("GitLab CE review requests address users, not groups.")),
		paired("publish_from_fork",
			gap("Both branches must be in one repository."),
			gap("The MR action omits the target project ID.")),
		paired("private_native_ref_targets",
			gap("Branch creation and managed-branch tag creation can check out without credentials."), unassessed),
		paired("repository_search", gap("No repository search SCM."), unassessed),
	},
}

func paired(id string, giteaAndForgejo, gitlab conformance.Cell) conformance.Row {
	cells := make(map[string]conformance.Cell, 3)
	if giteaAndForgejo != unassessed {
		cells["gitea"], cells["forgejo"] = giteaAndForgejo, giteaAndForgejo
	}
	if gitlab != unassessed {
		cells["gitlab"] = gitlab
	}
	return conformance.Row{ID: id, Cells: cells}
}

func gap(reason string) conformance.Cell {
	return conformance.Cell{Status: conformance.Gap, Reason: reason}
}

func na(reason string) conformance.Cell {
	return conformance.Cell{Status: conformance.NotApplicable, Reason: reason}
}
