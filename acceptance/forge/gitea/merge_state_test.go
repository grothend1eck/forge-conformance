//go:build acceptance

package gitea_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
)

const (
	requiredMergeCheck = "acceptance/required"
	mergeFixtureBefore = "version: v1.0.0\nkeep: untouched\n"
	mergeFixtureAfter  = "version: v1.1.0\nkeep: untouched\n"
	mergePollInterval  = 150 * time.Millisecond
	mergeWaitTimeout   = 20 * time.Second
)

func mergeSpec(title, strategy, method string) map[string]any {
	spec := map[string]any{"title": title, "merge": map[string]any{"strategy": strategy}}
	if method != "" {
		spec["mergemethod"] = method
	}
	return spec
}

func newProtectedMergeRepo(t *testing.T, s *server) (*repository, map[string]string) {
	t.Helper()
	r := s.newRepo(t)
	r.protectBase(t)
	refs := r.Refs(t)
	base := refs["refs/heads/"+r.Base]
	require.NotEmpty(t, base)
	require.NotEmpty(t, refs["refs/heads/main"])
	require.Equal(t, mergeFixtureBefore, r.Git(t, "show", base+":version.yaml"))
	return r, refs
}

// Identity and main should survive both edits and a subsequent merge. The
// remaining fields can change, but must stay fixed while a merge is blocked.
type mergeIdentity struct {
	id, number                         int64
	title, author, baseBranch, mainTip string
	draft                              bool
}

type mergeView struct {
	mergeIdentity
	headBranch, headSHA, headTip, baseTip string
	state                                 gitea.StateType
	merged                                bool
	mergedSHA, mergedBy                   string
}

func (r *repository) mergeView(t *testing.T, pr *gitea.PullRequest) mergeView {
	t.Helper()
	require.NotNil(t, pr.Base, "PR #%d is missing base metadata", pr.Index)
	refs := r.Refs(t)
	view := mergeView{
		mergeIdentity: mergeIdentity{pr.ID, pr.Index, pr.Title, username(pr.Poster), pr.Base.Ref, refs["refs/heads/main"], pr.Draft},
		baseTip:       refs["refs/heads/"+r.Base],
		state:         pr.State,
		merged:        pr.HasMerged,
		mergedBy:      username(pr.MergedBy),
	}
	// Post-merge the API can fall back to PR refs rather than branch tips.
	if pr.Head != nil {
		view.headBranch, view.headSHA = pr.Head.Ref, pr.Head.Sha
		view.headTip = refs["refs/heads/"+pr.Head.Ref]
	}
	if pr.MergedCommitID != nil {
		view.mergedSHA = *pr.MergedCommitID
	}
	return view
}

func (r *repository) assertInitialMergeRequest(t *testing.T, before map[string]string, id, title string, draft bool) mergeView {
	t.Helper()
	pr := onlyOpenPullRequest(t, r)
	view := r.mergeView(t, pr)
	head := "updatecli_" + r.Base + "_" + id
	base := before["refs/heads/"+r.Base]
	require.NotEqual(t, base, view.headSHA)
	require.Equal(t, mergeView{
		mergeIdentity: mergeIdentity{pr.ID, pr.Index, title, r.server.owner, r.Base, before["refs/heads/main"], draft},
		headBranch:    head,
		headSHA:       view.headSHA,
		headTip:       view.headSHA,
		baseTip:       base,
		state:         gitea.StateOpen,
	}, view, "initial PR and Git refs")
	r.Git(t, "fetch", "--no-tags", "origin", "refs/heads/"+head)
	// Without a seeded head, the first update starts at the protected base.
	parent := before["refs/heads/"+head]
	if parent == "" {
		parent = base
	}
	require.Equal(t, mergeFixtureAfter, r.Git(t, "show", view.headSHA+":version.yaml"))
	require.Equal(t, []string{parent}, r.commitParents(t, view.headSHA))
	require.Zero(t, r.latestStatus(t, view.headSHA, requiredMergeCheck).id, "the head must initially lack the required status")
	return view
}

func (r *repository) assertEdited(t *testing.T, previous mergeView, title, version string, draft bool) mergeView {
	t.Helper()
	edited := r.mergeView(t, onlyOpenPullRequest(t, r))
	require.NotEqual(t, previous.headSHA, edited.headSHA)
	want := previous
	want.title, want.draft = title, draft
	want.headSHA, want.headTip = edited.headSHA, edited.headSHA
	require.Equal(t, want, edited, "only the title, draft flag and existing head tip may change")
	r.Git(t, "fetch", "--no-tags", "origin", "refs/heads/"+edited.headBranch)
	require.Equal(t, "version: "+version+"\nkeep: untouched\n", r.Git(t, "show", edited.headSHA+":version.yaml"))
	return edited
}

func (r *repository) assertUnmergedFor(t *testing.T, phase string, opened mergeView, duration time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(duration); ; time.Sleep(mergePollInterval) {
		require.Equal(t, opened, r.mergeView(t, publishPR(t, r, opened.number)), "%s: PR and refs must stay unchanged", phase)
		if time.Now().After(deadline) {
			break
		}
	}
	require.Len(t, publishPRs(t, r), 1, phase)
}

// awaitMerged checks the provider's result against native Git before the
// cases assert style-specific topology on the returned base tip.
func (r *repository) awaitMerged(t *testing.T, opened mergeView, contents string, style gitea.MergeStyle) string {
	t.Helper()
	for deadline := time.Now().Add(mergeWaitTimeout); ; time.Sleep(mergePollInterval) {
		merged := r.mergeView(t, publishPR(t, r, opened.number))
		require.Equal(t, opened.mergeIdentity, merged.mergeIdentity, "PR identity and default branch must not change")
		if merged.merged && merged.state == gitea.StateClosed && merged.baseTip != "" && merged.baseTip != opened.baseTip {
			require.Equal(t, merged.baseTip, merged.mergedSHA, "forge merge SHA must match the native Git tip")
			require.Equal(t, r.server.owner, merged.mergedBy)
			require.Len(t, publishPRs(t, r), 1, "the forge must merge the sole original PR")
			if style == gitea.MergeStyleFastForwardOnly {
				require.Equal(t, opened.headSHA, merged.baseTip, "fast-forward must land the exact PR head")
			} else {
				require.NotEqual(t, opened.headSHA, merged.baseTip, "non-fast-forward must land a new commit")
			}
			r.Git(t, "fetch", "--no-tags", "origin", "refs/heads/"+r.Base)
			require.Equal(t, contents, r.Git(t, "show", merged.baseTip+":version.yaml"))
			return merged.baseTip
		}
		require.False(t, time.Now().After(deadline), "expected automatic merge after required status success on %s, last view %+v", opened.headSHA, merged)
	}
}

func (r *repository) commitParents(t *testing.T, sha string) []string {
	t.Helper()
	return strings.Fields(r.Git(t, "show", "-s", "--format=%P", sha))
}

func username(user *gitea.User) string {
	if user == nil {
		return ""
	}
	return user.UserName
}

type mergeStatus struct {
	id      int64
	check   string
	state   gitea.StatusState
	creator string
}

func (r *repository) publishStatus(t *testing.T, sha, check string, state gitea.StatusState) mergeStatus {
	t.Helper()
	s := r.server
	// The administrator stands in for CI and never invokes a merge.
	status, resp, err := s.admin.Repositories.CreateStatus(t.Context(), s.owner, r.name, sha, gitea.CreateStatusOption{
		State: state, Context: check, Description: "Acceptance fixture; no Actions runner",
	})
	s.check(t, fmt.Sprintf("post %s status %s on %s", state, check, sha), resp, err)
	require.NotNil(t, status)
	require.Positive(t, status.ID)
	creator := username(status.Creator)
	require.NotEmpty(t, creator)
	require.NotEqual(t, s.owner, creator, "status must come from an independent actor")
	want := mergeStatus{status.ID, check, state, creator}
	require.Equal(t, want, statusView(status), "created status")
	require.Equal(t, want, r.latestStatus(t, sha, check), "read back the status on the exact SHA")
	return want
}

func (r *repository) latestStatus(t *testing.T, sha, check string) mergeStatus {
	t.Helper()
	s := r.server
	statuses, resp, err := s.client.Repositories.ListStatusesBySHA(t.Context(), s.owner, r.name, sha, gitea.ListStatusesOption{})
	s.check(t, fmt.Sprintf("read %s status on exact SHA %s", check, sha), resp, err)
	// `highestindex` sorts ascending on the pinned releases and timestamps
	// can tie. Select the newest ID for the exact context ourselves.
	var latest mergeStatus
	for _, status := range statuses {
		if status != nil && status.Context == check && status.ID > latest.id {
			latest = statusView(status)
		}
	}
	return latest
}

func statusView(status *gitea.Status) mergeStatus {
	return mergeStatus{status.ID, status.Context, status.State, username(status.Creator)}
}

func (r *repository) assertScheduledBy(t *testing.T, phase string, number int64, want ...string) {
	t.Helper()
	s := r.server
	// Both pinned providers expose the persisted native schedule in the timeline.
	events, resp, err := s.client.Issues.ListIssueTimeline(t.Context(), s.owner, r.name, number, gitea.ListIssueCommentOptions{})
	s.check(t, fmt.Sprintf("%s: read PR #%d timeline", phase, number), resp, err)
	var actors []string
	for _, event := range events {
		require.NotNil(t, event, "%s: invalid PR #%d timeline event", phase, number)
		require.NotEqual(t, "pull_cancel_scheduled_merge", event.Type, "%s: PR #%d schedule was canceled", phase, number)
		if event.Type == "pull_scheduled_merge" {
			actors = append(actors, username(event.Poster))
		}
	}
	require.Equal(t, want, actors, "%s: PR #%d scheduling actors", phase, number)
}

func (r *repository) protectBase(t *testing.T) {
	t.Helper()
	s := r.server
	// The Gitea SDK doesn't expose Forgejo's apply_to_admins field.
	type rule struct {
		Name              string   `json:"rule_name"`
		EnablePush        bool     `json:"enable_push"`
		PushAllowlist     bool     `json:"enable_push_whitelist"`
		EnableStatusCheck bool     `json:"enable_status_check"`
		Contexts          []string `json:"status_check_contexts"`
		BlockOverride     bool     `json:"block_admin_merge_override,omitempty"`
		ApplyToAdmins     bool     `json:"apply_to_admins,omitempty"`
		BypassAllowlist   bool     `json:"enable_bypass_allowlist"`
	}
	want := rule{Name: r.Base, EnableStatusCheck: true, Contexts: []string{requiredMergeCheck}}
	switch s.provider {
	case "gitea":
		want.BlockOverride = true
	case "forgejo":
		want.ApplyToAdmins = true
	default:
		t.Fatalf("setup protection: no rule for provider %q", s.provider)
	}
	body, err := json.Marshal(want)
	require.NoError(t, err) // Only the test-authored rule is encoded.
	r.protectionRequest(t, http.MethodPost, "", body, http.StatusCreated, nil)
	var got rule
	r.protectionRequest(t, http.MethodGet, "/"+url.PathEscape(r.Base), nil, http.StatusOK, &got)
	require.Equal(t, want, got, "read back the branch protection rule")

	branch, resp, err := s.client.Repositories.GetRepoBranch(t.Context(), s.owner, r.name, r.Base)
	s.check(t, "read effective protection before apply", resp, err)
	require.NotNil(t, branch)
	require.True(t, branch.Protected)
	require.True(t, branch.EnableStatusCheck)
	require.Equal(t, r.Base, branch.EffectiveBranchProtectionName)
	require.False(t, branch.UserCanPush)
	require.Equal(t, want.Contexts, branch.StatusCheckContexts)
}

func (r *repository) protectionRequest(t *testing.T, method, suffix string, body []byte, wantStatus int, out any) {
	t.Helper()
	s := r.server
	phase := method + " branch protection before apply"
	endpoint := strings.TrimRight(s.url, "/") + "/api/v1/repos/" + url.PathEscape(s.owner) + "/" + url.PathEscape(r.name) + "/branch_protections" + suffix
	req, err := http.NewRequestWithContext(t.Context(), method, endpoint, bytes.NewReader(body))
	// Transport and JSON errors can contain request/response data.
	require.True(t, err == nil, "%s: construct request (%T)", phase, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+s.token)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	require.True(t, err == nil, "%s: request failed (%T)", phase, err)
	defer resp.Body.Close()
	require.Equal(t, wantStatus, resp.StatusCode, phase)
	if out != nil {
		err := json.NewDecoder(resp.Body).Decode(out)
		require.True(t, err == nil, "%s: invalid response JSON (%T)", phase, err)
	}
}
