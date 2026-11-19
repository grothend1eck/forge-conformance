package pullrequest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	giteasdk "gitea.dev/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/pkg/core/reports"
)

func TestEnableAutoMergeMethods(t *testing.T) {
	tests := []struct {
		name, strategy, method string
		defaultMethod          giteasdk.MergeStyle
		wantMethod             giteasdk.MergeStyle
		wantErr                string
	}{
		{name: "empty strategy is manual", method: "merge"},
		{name: "explicit manual is case insensitive", strategy: "MANUAL", method: "merge"},
		{name: "merge", strategy: "auto", method: "merge", wantMethod: giteasdk.MergeStyleMerge},
		{name: "rebase", strategy: "auto", method: "REBASE", wantMethod: giteasdk.MergeStyleRebase},
		{name: "rebase merge", strategy: "auto", method: "rebase-merge", wantMethod: giteasdk.MergeStyleRebaseMerge},
		{name: "squash", strategy: "auto", method: "squash", wantMethod: giteasdk.MergeStyleSquash},
		{name: "fast forward only", strategy: "auto", method: "fast-forward-only", wantMethod: giteasdk.MergeStyleFastForwardOnly},
		{name: "repository default", strategy: "AUTO", defaultMethod: giteasdk.MergeStyleSquash, wantMethod: giteasdk.MergeStyleSquash},
		{name: "invalid repository default", strategy: "auto", defaultMethod: "manually-merged", wantErr: "unsupported repository default merge method"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const headSHA = "0123456789abcdef"
			var repoReads int
			var requests []giteasdk.MergePullRequestOption
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/v1/version":
					_, _ = w.Write([]byte(`{"version":"1.24.0"}`))
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/updatecli/updatecli":
					repoReads++
					_ = json.NewEncoder(w).Encode(giteasdk.Repository{DefaultMergeStyle: tt.defaultMethod})
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/updatecli/updatecli/pulls/42/merge":
					var sent giteasdk.MergePullRequestOption
					if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
						http.Error(w, "invalid merge request", http.StatusBadRequest)
						return
					}
					requests = append(requests, sent)
					w.WriteHeader(http.StatusCreated)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := giteasdk.NewClient(server.URL)
			require.NoError(t, err)
			g := Gitea{
				spec:   Spec{Merge: MergeSpec{Strategy: tt.strategy}, MergeMethod: tt.method},
				client: client, Owner: "updatecli", Repository: "updatecli",
			}
			require.NoError(t, g.spec.Validate())
			err = g.enableAutoMerge(t.Context(), &giteasdk.PullRequest{Index: 42, Head: &giteasdk.PRBranchInfo{Sha: headSHA}})
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			if tt.defaultMethod != "" {
				require.Equal(t, 1, repoReads)
			} else {
				require.Zero(t, repoReads)
			}
			if tt.wantMethod == "" {
				require.Empty(t, requests)
			} else {
				require.Equal(t, []giteasdk.MergePullRequestOption{{
					Style: tt.wantMethod, MergeWhenChecksSucceed: true, HeadCommitId: headSHA,
				}}, requests)
			}
		})
	}
}

func TestCreateActionExistingPullRequestAutoMerge(t *testing.T) {
	testData := []struct {
		name          string
		requestStatus int
	}{
		{
			name:          "Reviewer request succeeds",
			requestStatus: http.StatusCreated,
		},
		{
			name:          "Reviewer request fails",
			requestStatus: http.StatusForbidden,
		},
	}

	for _, tt := range testData {
		t.Run(tt.name, func(t *testing.T) {
			fake := cleanServer{
				headRepoID: testRepoID,
				listedSha:  []string{"abc"},
				branchSha:  "abc",
			}

			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/version":
					_, _ = w.Write([]byte(`{"version":"1.24.0"}`))
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/settings/api":
					_, _ = w.Write([]byte(`{"max_response_items":50}`))
				case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/api/v1/repos/updatecli/updatecli/pulls/%d/reviews", testPRIndex):
					w.Header().Set("X-Total-Count", "0")
					_, _ = w.Write([]byte(`[]`))
				case r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/api/v1/repos/updatecli/updatecli/pulls/%d/requested_reviewers", testPRIndex):
					calls = append(calls, "reviewers")
					w.WriteHeader(tt.requestStatus)
				case r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/api/v1/repos/updatecli/updatecli/pulls/%d/merge", testPRIndex):
					calls = append(calls, "merge")
					w.WriteHeader(http.StatusOK)
				default:
					if r.Method == http.MethodPatch {
						calls = append(calls, r.Method)
					}
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
					Reviewers:   []string{"alice"},
				},
				client:       client,
				Owner:        "updatecli",
				Repository:   "updatecli",
				SourceBranch: "updatecli",
				TargetBranch: "main",
			}

			report := reports.Action{Title: "title"}

			err = g.CreateAction(t.Context(), &report, false)
			if tt.requestStatus == http.StatusCreated {
				require.NoError(t, err)
				assert.Equal(t, []string{http.MethodPatch, "reviewers", "merge"}, calls)
			} else {
				require.ErrorContains(t, err, "requesting reviewers")
				assert.Equal(t, []string{http.MethodPatch, "reviewers"}, calls)
			}
			assert.Equal(t, "https://gitea.example.com/updatecli/updatecli/pulls/42", report.Link)
		})
	}
}

func TestCreateActionDraftPullRequest(t *testing.T) {
	testData := []struct {
		name          string
		remoteTitle   string
		draft         bool
		specTitle     string
		expectedTitle string
		expectedMerge bool
	}{
		{
			name:          "Pull request which isn't a draft follows the report",
			remoteTitle:   "bump app to 1.0.0",
			expectedTitle: "bump app to 2.0.0",
			expectedMerge: true,
		},
		{
			name:          "WIP prefix is kept",
			remoteTitle:   "WIP: bump app to 1.0.0",
			draft:         true,
			expectedTitle: "WIP: bump app to 2.0.0",
		},
		{
			name:          "Prefix is kept as it was written",
			remoteTitle:   "wip: bump app to 1.0.0",
			draft:         true,
			expectedTitle: "wip: bump app to 2.0.0",
		},
		{
			name:          "Bracket prefix is kept",
			remoteTitle:   "[WIP] bump app to 1.0.0",
			draft:         true,
			expectedTitle: "[WIP] bump app to 2.0.0",
		},
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
		{
			name:          "Manifest title with a prefix isn't prefixed twice",
			remoteTitle:   "WIP: bump app to 1.0.0",
			draft:         true,
			specTitle:     "WIP: custom title",
			expectedTitle: "WIP: custom title",
		},
		{
			name:          "Manifest title on a draft keeps the prefix",
			remoteTitle:   "WIP: bump app to 1.0.0",
			draft:         true,
			specTitle:     "custom title",
			expectedTitle: "WIP: custom title",
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
					Title:       tt.specTitle,
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
			assert.Equal(t, tt.expectedMerge, merged)
		})
	}
}
