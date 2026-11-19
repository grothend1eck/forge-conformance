package pullrequest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	giteasdk "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/pkg/core/reports"
)

func TestCreateActionDraftMetadataFallbacks(t *testing.T) {
	testData := []struct {
		name          string
		remoteTitle   string
		draft         bool
		expectedTitle string
	}{
		{
			name:          "Prefix is detected without the draft field",
			remoteTitle:   "WIP: bump app to 1.0.0",
			expectedTitle: "WIP: bump app to 2.0.0",
		},
		{
			name:          "Unknown draft prefix keeps the title",
			remoteTitle:   "Draft: bump app to 1.0.0",
			draft:         true,
			expectedTitle: "Draft: bump app to 1.0.0",
		},
	}

	for _, tt := range testData {
		t.Run(tt.name, func(t *testing.T) {
			fake := cleanServer{
				headRepoID: testRepoID,
				listedSha:  []string{"abc"},
				branchSha:  "abc",
				title:      tt.remoteTitle,
				draft:      tt.draft,
			}

			merged := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/version":
					_, _ = w.Write([]byte(`{"version":"1.24.0"}`))
				case r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/api/v1/repos/updatecli/updatecli/pulls/%d/merge", testPRIndex):
					merged = true
					w.WriteHeader(http.StatusOK)
				default:
					fake.handler(w, r)
				}
			}))
			defer server.Close()

			client, err := giteasdk.NewClient(server.URL)
			require.NoError(t, err)

			g := Gitea{
				spec: Spec{
					Merge:       MergeSpec{Strategy: MergeStrategyAuto},
					MergeMethod: "squash",
				},
				client:       client,
				Owner:        "updatecli",
				Repository:   "updatecli",
				SourceBranch: "updatecli",
				TargetBranch: "main",
			}

			report := reports.Action{Title: "bump app to 2.0.0"}

			require.NoError(t, g.CreateAction(t.Context(), &report, false))

			assert.Equal(t, tt.expectedTitle, report.Title)
			assert.False(t, merged, "never send a merge request for a draft")
		})
	}
}
