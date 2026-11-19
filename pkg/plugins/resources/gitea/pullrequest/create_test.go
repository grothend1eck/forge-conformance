package pullrequest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	giteasdk "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/pkg/core/reports"
)

func testActionReport(targetID string) reports.Action {
	return reports.Action{
		ID:            "action",
		Title:         "report title",
		PipelineTitle: "pipeline",
		Targets: []reports.ActionTarget{
			{ID: targetID, Title: "title of " + targetID},
		},
	}
}

func TestCreateActionExistingPullRequest(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
	}{{"Metadata-only patch", http.StatusOK}, {"Rejected patch", http.StatusForbidden}} {
		t.Run(tt.name, func(t *testing.T) {
			fake := cleanServer{
				headRepoID: testRepoID,
				listedSha:  []string{"abc"},
				branchSha:  "abc",
				title:      "remote title",
				body:       "remote body",
			}

			const path = "/api/v1/repos/updatecli/updatecli/pulls/42"
			const body = "custom body"
			const link = "https://gitea.example.com/updatecli/updatecli/pulls/42"
			var edits int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || r.URL.Path != path {
					fake.handler(w, r)
					return
				}
				edits++
				var sent giteasdk.EditPullRequestOption
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
				assert.Equal(t, giteasdk.EditPullRequestOption{Title: "report title", Body: new(body)}, sent)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				// A response distinguishable from the request proves the report uses persisted metadata.
				_ = json.NewEncoder(w).Encode(giteasdk.PullRequest{
					Index: testPRIndex, Title: "returned title", Body: "returned body", HTMLURL: link,
				})
			}))
			defer server.Close()

			client, err := giteasdk.NewClient(server.URL)
			require.NoError(t, err)

			g := Gitea{
				spec:         Spec{Body: body, Assignees: []string{"keep-existing"}},
				client:       client,
				Owner:        "updatecli",
				Repository:   "updatecli",
				SourceBranch: "updatecli",
				TargetBranch: "main",
			}

			report := testActionReport("current")

			err = g.CreateAction(t.Context(), &report, false)
			assert.Equal(t, 1, edits)
			if tt.status == http.StatusForbidden {
				require.ErrorContains(t, err, "update Gitea pull request")
				assert.Empty(t, report.Link)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "returned title", report.Title)
			assert.Equal(t, "returned body", report.Description)
			assert.Equal(t, link, report.Link)
		})
	}
}

func TestCreateActionFailure(t *testing.T) {
	testData := []struct {
		name string
		spec Spec
	}{
		{
			name: "Without auto-merge",
		},
		{
			name: "With auto-merge",
			spec: Spec{Merge: MergeSpec{Strategy: MergeStrategyAuto}},
		},
	}

	for _, tt := range testData {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/updatecli/updatecli/pulls":
					_ = json.NewEncoder(w).Encode([]giteasdk.PullRequest{})
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/updatecli/updatecli/branches":
					_ = json.NewEncoder(w).Encode([]giteasdk.Branch{{Name: "updatecli"}, {Name: "main"}})
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/updatecli/updatecli/pulls":
					http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
				default:
					http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer server.Close()

			client, err := giteasdk.NewClient(server.URL)
			require.NoError(t, err)

			tt.spec.Owner = "updatecli"
			tt.spec.Repository = "updatecli"
			tt.spec.SourceBranch = "updatecli"
			tt.spec.TargetBranch = "main"

			g := Gitea{
				spec:         tt.spec,
				client:       client,
				Owner:        "updatecli",
				Repository:   "updatecli",
				SourceBranch: "updatecli",
				TargetBranch: "main",
			}

			report := testActionReport("current")

			err = g.CreateAction(t.Context(), &report, false)

			require.ErrorContains(t, err, "create Gitea pull request")
			assert.Empty(t, report.Link)
		})
	}
}
