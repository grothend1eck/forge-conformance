//go:build acceptance

package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	giteasdk "gitea.dev/sdk"
)

const (
	requiredMergeCheck = "acceptance/required"
	mergeFixtureBefore = "version: v1.0.0\nkeep: untouched\n"
	mergeFixtureAfter  = "version: v1.1.0\nkeep: untouched\n"
	mergePollInterval  = 150 * time.Millisecond
	mergeWaitTimeout   = 20 * time.Second
)

func testProtectedMerge(t *testing.T, s *server) {
	t.Run("observably_scheduled", func(t *testing.T) {
		p := openProtectedPR(t, s, "schedule-observer", "auto", false)
		// This is the forge's native event, not Updatecli output or an inference
		// from the later merge. Both Gitea 28.2.0 and Forgejo 17.0.0 record it in
		// services/automerge/automerge.go and expose it via the issue timeline.
		const phase = "observe scheduling before status success"
		p.assertScheduled(t, phase, true)
		p.assertOpen(t, phase)
	})

	t.Run("merges_after_required_status", func(t *testing.T) {
		p := openProtectedPR(t, s, "protected-auto", "auto", false)
		pending := p.postStatus(t, p.head, requiredMergeCheck, giteasdk.StatusPending)
		// Neither a success on a different SHA nor a different context on the
		// PR head can fulfill the required status for this head.
		p.postStatus(t, p.base, requiredMergeCheck, giteasdk.StatusSuccess)
		p.postStatus(t, p.head, "acceptance/unrelated", giteasdk.StatusSuccess)
		p.assertUnmergedFor(t, "wrong SHA and wrong context have passed", time.Second)
		p.assertStatus(t, p.head, requiredMergeCheck, pending)

		// Only the status fixture writes from here on. In particular, no
		// Updatecli rerun, direct merge, or change to the protection rule.
		p.postStatus(t, p.head, requiredMergeCheck, giteasdk.StatusSuccess)
		p.awaitMerged(t)
	})
}

func testManualMerge(t *testing.T, s *server) {
	p := openProtectedPR(t, s, "protected-manual", "manual", false)
	p.postStatus(t, p.head, requiredMergeCheck, giteasdk.StatusSuccess)
	p.assertUnmergedFor(t, "manual strategy after required status success", 2*time.Second)
	p.assertScheduled(t, "manual strategy after required status success", false)
}

func testDraft(t *testing.T, s *server) {
	p := openProtectedPR(t, s, "protected-draft", "auto", true)
	// Both pinned providers derive the API's `draft` flag from the default
	// WIP title prefixes (models/issues/pull.go, IsWorkInProgress), not a
	// separate draft parameter on the Updatecli action.
	p.postStatus(t, p.head, requiredMergeCheck, giteasdk.StatusSuccess)
	p.assertUnmergedFor(t, "draft after required status success", 2*time.Second)
	p.assertScheduled(t, "draft after required status success", false)
}

type protectedPR struct {
	s          *server
	r          *repository
	number, id int64
	base, head string
	defaultTip string
	headRef    string
	title      string
	draft      bool
}

func openProtectedPR(t *testing.T, s *server, pipelineID, strategy string, draft bool) *protectedPR {
	t.Helper()
	r := s.newRepo(t)
	p := &protectedPR{s: s, r: r, draft: draft, title: "Acceptance: " + pipelineID}
	if draft {
		p.title = "WIP: " + p.title
	}
	p.protectBase(t)
	refs := r.refs(t)
	p.base, p.defaultTip = refs["refs/heads/"+r.base], refs["refs/heads/main"]
	if p.base == "" || p.defaultTip == "" {
		t.Fatal("protected fixture before apply: missing base or default Git branch")
	}
	p.assertVersion(t, "seed before apply", p.base, mergeFixtureBefore)

	t.Logf("apply %s strategy on protected %s (draft=%t)", strategy, r.base, draft)
	newCLI(t, s).ok(t, "apply", requestManifest(r, pipelineID, "v1.1.0", map[string]any{
		"title":       p.title,
		"merge":       map[string]any{"strategy": strategy},
		"mergemethod": "merge",
	}))

	pr := p.onlyPR(t, "after initial Updatecli apply")
	if pr.Index <= 0 || pr.ID <= 0 || pr.Poster == nil || pr.Poster.UserName != s.owner {
		t.Fatal("after initial Updatecli apply: expected a numbered PR by the fixture's Updatecli actor")
	}
	if pr.Head == nil || pr.Head.Sha == "" || pr.Head.Ref == "" || pr.Head.Ref == r.base {
		t.Fatalf("after initial Updatecli apply: PR #%d has no distinct branch and head SHA", pr.Index)
	}
	p.number, p.id = pr.Index, pr.ID
	p.head, p.headRef = pr.Head.Sha, pr.Head.Ref
	p.assertOpen(t, "after initial Updatecli apply without status")
	if got := r.refs(t)["refs/heads/"+p.headRef]; got != p.head || got == p.base {
		t.Fatalf("identify Updatecli head: %s points at %s, PR head %s, base %s", p.headRef, got, p.head, p.base)
	}
	// Fetch the update branch by name, not by API SHA, to prove the proposed
	// bytes exist in the published Git history before we post any status.
	r.git(t, "fetch", "--no-tags", "origin", "refs/heads/"+p.headRef)
	p.assertVersion(t, "verify proposed update", p.head, mergeFixtureAfter)
	p.assertStatus(t, p.head, requiredMergeCheck, nil)
	return p
}

func (p *protectedPR) protectBase(t *testing.T) {
	t.Helper()
	// SDK v1.4.0 models Gitea's block_admin_merge_override but not Forgejo's
	// apply_to_admins. Use the public JSON endpoints to set and read back the
	// provider's actual no-bypass rule. The shared fixture verifies the
	// Updatecli actor is not an instance admin (who could bypass on Forgejo).
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
	want := rule{Name: p.r.base, EnableStatusCheck: true, Contexts: []string{requiredMergeCheck}}
	switch p.s.provider {
	case "gitea":
		want.BlockOverride = true
	case "forgejo":
		want.ApplyToAdmins = true
	default:
		t.Fatalf("setup protection: no rule for provider %q", p.s.provider)
	}
	p.protectionRequest(t, http.MethodPost, "", want, http.StatusCreated, nil)
	var got rule
	p.protectionRequest(t, http.MethodGet, "/"+url.PathEscape(p.r.base), nil, http.StatusOK, &got)
	if got.Name != want.Name || got.EnablePush || got.PushAllowlist || got.BypassAllowlist ||
		!got.EnableStatusCheck || !slices.Equal(got.Contexts, want.Contexts) ||
		got.BlockOverride != want.BlockOverride || got.ApplyToAdmins != want.ApplyToAdmins {
		t.Fatalf("read back branch protection before apply: got %+v, want %+v", got, want)
	}
	branch, resp, err := p.s.client.Repositories.GetRepoBranch(t.Context(), p.s.owner, p.r.name, p.r.base)
	if err != nil {
		p.s.apiFailure(t, "read effective protection before apply", resp, err)
	}
	if !branch.Protected || !branch.EnableStatusCheck || branch.EffectiveBranchProtectionName != p.r.base ||
		branch.UserCanPush || !slices.Equal(branch.StatusCheckContexts, want.Contexts) {
		t.Fatalf("read effective protection before apply: %s protected=%t rule=%q pushable=%t check enabled=%t contexts=%v",
			p.r.base, branch.Protected, branch.EffectiveBranchProtectionName, branch.UserCanPush, branch.EnableStatusCheck, branch.StatusCheckContexts)
	}
}

func (p *protectedPR) protectionRequest(t *testing.T, method, suffix string, in any, wantStatus int, out any) {
	t.Helper()
	phase := method + " branch protection before apply"
	var body []byte
	if in != nil {
		var err error
		body, err = json.Marshal(in)
		if err != nil {
			t.Fatalf("%s: could not encode fixture request (%T)", phase, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(p.s.url, "/") + "/api/v1/repos/" + url.PathEscape(p.s.owner) + "/" + url.PathEscape(p.r.name) + "/branch_protections" + suffix
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: could not construct fixture request (%T)", phase, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+p.s.token)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		t.Fatalf("%s: fixture request failed (%T)", phase, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: HTTP %d, want %d", phase, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s: invalid response JSON (%T)", phase, err)
		}
	}
}

func (p *protectedPR) onlyPR(t *testing.T, phase string) *giteasdk.PullRequest {
	t.Helper()
	prs, resp, err := p.s.client.PullRequests.ListRepoPullRequests(t.Context(), p.s.owner, p.r.name, giteasdk.ListPullRequestsOptions{
		State: giteasdk.StateAll,
	})
	if err != nil {
		p.s.apiFailure(t, phase+": list PRs", resp, err)
	}
	if len(prs) != 1 || prs[0] == nil {
		t.Fatalf("%s: expected exactly one PR (open or closed), got %d", phase, len(prs))
	}
	return prs[0]
}

func (p *protectedPR) readPR(t *testing.T, phase string) *giteasdk.PullRequest {
	t.Helper()
	pr, resp, err := p.s.client.PullRequests.GetPullRequest(t.Context(), p.s.owner, p.r.name, p.number)
	if err != nil {
		p.s.apiFailure(t, fmt.Sprintf("%s: read PR #%d", phase, p.number), resp, err)
	}
	if pr.ID != p.id || pr.Index != p.number || pr.Base == nil || pr.Base.Ref != p.r.base ||
		pr.Title != p.title || pr.Draft != p.draft {
		t.Fatalf("%s: PR #%d no longer has the expected identity, base %q, title, and draft=%t", phase, p.number, p.r.base, p.draft)
	}
	return pr
}

func (p *protectedPR) assertVersion(t *testing.T, phase, sha, content string) {
	t.Helper()
	if got := p.r.git(t, "show", sha+":version.yaml"); got != content {
		t.Fatalf("%s: version.yaml at %s = %q, want %q", phase, sha, got, content)
	}
}

func (p *protectedPR) assertOpen(t *testing.T, phase string) {
	t.Helper()
	pr := p.readPR(t, phase)
	if pr.State != giteasdk.StateOpen || pr.HasMerged || pr.Head == nil || pr.Head.Sha != p.head || pr.Head.Ref != p.headRef {
		t.Fatalf("%s: PR #%d state=%q merged=%t, want open and unmerged at head %s", phase, p.number, pr.State, pr.HasMerged, p.head)
	}
	refs := p.r.refs(t)
	if got := refs["refs/heads/"+p.r.base]; got != p.base {
		t.Fatalf("%s: base %s moved from %s to %q", phase, p.r.base, p.base, got)
	}
	if got := refs["refs/heads/main"]; got != p.defaultTip {
		t.Fatalf("%s: non-target default branch moved from %s to %q", phase, p.defaultTip, got)
	}
}

func (p *protectedPR) postStatus(t *testing.T, sha, check string, state giteasdk.StatusState) *giteasdk.Status {
	t.Helper()
	phase := fmt.Sprintf("post fixture status %s for %s at %s", state, check, sha)
	// The admin identity stands in for CI and never invokes a merge.
	status, resp, err := p.s.adminClient().Repositories.CreateStatus(t.Context(), p.s.owner, p.r.name, sha, giteasdk.CreateStatusOption{
		State:       state,
		Context:     check,
		Description: "Acceptance fixture; no Actions runner",
	})
	if err != nil {
		p.s.apiFailure(t, phase, resp, err)
	}
	if status == nil || status.ID <= 0 || status.State != state || status.Context != check ||
		status.Creator == nil || status.Creator.UserName == "" || status.Creator.UserName == p.s.owner {
		t.Fatalf("%s: expected the requested status from an identity distinct from Updatecli", phase)
	}
	p.assertStatus(t, sha, check, status)
	return status
}

func (p *protectedPR) assertStatus(t *testing.T, sha, check string, want *giteasdk.Status) {
	t.Helper()
	// Read actual statuses scoped to the full SHA, not a branch name or
	// aggregate state (which may also contain unrelated contexts).
	phase := fmt.Sprintf("read %s status on exact SHA %s", check, sha)
	statuses, resp, err := p.s.client.Repositories.ListStatusesBySHA(t.Context(), p.s.owner, p.r.name, sha, giteasdk.ListStatusesOption{})
	if err != nil {
		p.s.apiFailure(t, phase, resp, err)
	}
	// Both pinned servers sort `highestindex` in ascending order, and
	// timestamps may tie. Select the newest status ID for this context.
	var latest *giteasdk.Status
	for _, status := range statuses {
		if status != nil && status.Context == check && (latest == nil || status.ID > latest.ID) {
			latest = status
		}
	}
	switch {
	case want == nil && latest != nil:
		t.Fatalf("%s: expected no status, got %q", phase, latest.State)
	case want != nil && latest == nil:
		t.Fatalf("%s: missing expected %q status ID %d", phase, want.State, want.ID)
	case want != nil:
		if latest.ID != want.ID || latest.State != want.State || latest.Creator == nil ||
			latest.Creator.ID != want.Creator.ID {
			t.Fatalf("%s: got state %q ID %d, want %q ID %d from fixture status poster", phase, latest.State, latest.ID, want.State, want.ID)
		}
	}
}

func (p *protectedPR) assertScheduled(t *testing.T, phase string, want bool) {
	t.Helper()
	events, resp, err := p.s.client.Issues.ListIssueTimeline(t.Context(), p.s.owner, p.r.name, p.number, giteasdk.ListIssueCommentOptions{})
	if err != nil {
		p.s.apiFailure(t, fmt.Sprintf("%s: read PR #%d timeline", phase, p.number), resp, err)
	}
	found := false
	for _, event := range events {
		if event == nil {
			t.Fatalf("%s: PR #%d timeline contained an invalid event", phase, p.number)
		}
		switch event.Type {
		case "pull_scheduled_merge":
			found = true
			if want && (event.Poster == nil || event.Poster.UserName != p.s.owner) {
				t.Fatalf("%s: PR #%d schedule was not attributed to the Updatecli actor", phase, p.number)
			}
		case "pull_cancel_scheduled_merge":
			t.Fatalf("%s: PR #%d unexpectedly has a native cancel-schedule event", phase, p.number)
		}
	}
	if found != want {
		t.Fatalf("%s: PR #%d native pull_scheduled_merge event present=%t, want %t", phase, p.number, found, want)
	}
}

func (p *protectedPR) assertUnmergedFor(t *testing.T, phase string, duration time.Duration) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for {
		p.assertOpen(t, phase)
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(mergePollInterval)
	}
	if pr := p.onlyPR(t, phase); pr.ID != p.id || pr.Index != p.number {
		t.Fatalf("%s: PR #%d was replaced", phase, p.number)
	}
}

func (p *protectedPR) awaitMerged(t *testing.T) {
	t.Helper()
	const phase = "await automatic merge after required status success"
	deadline := time.Now().Add(mergeWaitTimeout)
	for {
		pr := p.readPR(t, phase)
		refs := p.r.refs(t)
		base := refs["refs/heads/"+p.r.base]
		if pr.HasMerged && pr.State == giteasdk.StateClosed && base != "" && base != p.base {
			if pr.MergedCommitID == nil || *pr.MergedCommitID != base || pr.MergedBy == nil || pr.MergedBy.UserName != p.s.owner {
				t.Fatalf("%s: PR #%d merge was not attributed to Updatecli's actor at remote base tip %s", phase, p.number, base)
			}
			if only := p.onlyPR(t, phase); only.ID != p.id || only.Index != p.number || !only.HasMerged {
				t.Fatalf("%s: expected the original PR #%d to be the only merged PR", phase, p.number)
			}
			if got := refs["refs/heads/main"]; got != p.defaultTip {
				t.Fatalf("%s: the non-target default branch moved from %s to %s", phase, p.defaultTip, got)
			}
			// The completion proof is the Git base tip with the exact checked
			// head as a parent. Do not treat API base.sha as an immutable base
			// or post-merge head.sha as canonical (deleted branches can cause
			// the providers to resolve the head from their own PR ref).
			p.r.git(t, "fetch", "--no-tags", "origin", "refs/heads/"+p.r.base)
			wantParents := []string{base, p.base, p.head}
			if got := strings.Fields(p.r.git(t, "rev-list", "--parents", "-n", "1", base)); !slices.Equal(got, wantParents) {
				t.Fatalf("%s: merge commit/parents %v, want %v", phase, got, wantParents)
			}
			p.assertVersion(t, "verify merged base bytes", base, mergeFixtureAfter)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: PR #%d state=%q merged=%t; %s tip %s (original %s), checked head %s",
				phase, p.number, pr.State, pr.HasMerged, p.r.base, base, p.base, p.head)
		}
		time.Sleep(mergePollInterval)
	}
}
