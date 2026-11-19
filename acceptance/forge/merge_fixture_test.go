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

type mergeSeed struct {
	baseTip, defaultTip string
}

func newProtectedMergeRepo(t *testing.T, s *server) (*repository, mergeSeed) {
	t.Helper()
	r := s.newRepo(t)
	r.protectBase(t)
	refs := r.refs(t)
	before := mergeSeed{baseTip: refs["refs/heads/"+r.base], defaultTip: refs["refs/heads/main"]}
	if before.baseTip == "" || before.defaultTip == "" {
		t.Fatalf("before apply: missing seeded Git refs: %+v", before)
	}
	if got := r.git(t, "show", before.baseTip+":version.yaml"); got != mergeFixtureBefore {
		t.Fatalf("before apply: base version.yaml = %q, want %q", got, mergeFixtureBefore)
	}
	return r, before
}

func (r *repository) assertOpened(t *testing.T, phase string, before mergeSeed, title string, draft bool) mergeSnapshot {
	t.Helper()
	prs := r.listRequests(t, phase)
	if len(prs) != 1 || prs[0] == nil || prs[0].ID <= 0 || prs[0].Index <= 0 {
		t.Fatalf("%s: expected one request with a valid ID and number, got %d requests", phase, len(prs))
	}
	pr := r.readMergeSnapshot(t, phase, prs[0].Index)
	if pr.headBranch == "" || pr.headBranch == r.base || pr.headSHA == "" || pr.headSHA == before.baseTip {
		t.Fatalf("%s: expected a distinct PR head, got %+v", phase, pr)
	}
	assertOpen(t, phase, pr, mergeSnapshot{
		id: prs[0].ID, number: prs[0].Index, title: title, author: r.server.owner,
		baseBranch: r.base, baseTip: before.baseTip, defaultTip: before.defaultTip,
		headBranch: pr.headBranch, headSHA: pr.headSHA, headTip: pr.headSHA, open: true, draft: draft,
	})
	if contents, parents := r.readCommit(t, pr.headBranch, pr.headSHA); contents != mergeFixtureAfter ||
		!slices.Equal(parents, []string{before.baseTip}) {
		t.Fatalf("%s: head version.yaml = %q, parents = %v; want %q and [%s]",
			phase, contents, parents, mergeFixtureAfter, before.baseTip)
	}
	if got := r.latestStatus(t, pr.headSHA, requiredMergeCheck); got.id != 0 {
		t.Fatalf("%s: required status on %s must initially be absent, got %+v", phase, pr.headSHA, got)
	}
	return pr
}

func assertOpen(t *testing.T, phase string, got, want mergeSnapshot) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: expected the request to remain open at the expected refs:\n got %+v\nwant %+v", phase, got, want)
	}
}

func (r *repository) assertUnmergedFor(t *testing.T, phase string, opened mergeSnapshot, duration time.Duration) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for {
		assertOpen(t, phase, r.readMergeSnapshot(t, phase, opened.number), opened)
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(mergePollInterval)
	}
	if got := len(r.listRequests(t, phase)); got != 1 {
		t.Fatalf("%s: expected one request, got %d", phase, got)
	}
}

func (r *repository) awaitMerged(t *testing.T, opened mergeSnapshot, wantParents ...string) {
	t.Helper()
	r.awaitMergedCommit(t, opened, mergeFixtureAfter, false, wantParents...)
}

func (r *repository) awaitMergedCommit(t *testing.T, opened mergeSnapshot, wantContents string, fastForward bool, wantParents ...string) {
	t.Helper()
	const phase = "await automatic merge after required status success"
	deadline := time.Now().Add(mergeWaitTimeout)
	for {
		pr := r.readMergeSnapshot(t, phase, opened.number)
		if pr.id != opened.id || pr.number != opened.number || pr.baseBranch != opened.baseBranch ||
			pr.title != opened.title || pr.draft != opened.draft || pr.author != opened.author || pr.defaultTip != opened.defaultTip {
			t.Fatalf("%s: request identity or default branch changed:\n got %+v\nwas %+v", phase, pr, opened)
		}
		if pr.merged && !pr.open && pr.baseTip != "" && pr.baseTip != opened.baseTip {
			if pr.mergedSHA != pr.baseTip || pr.mergedBy != r.server.owner || len(r.listRequests(t, phase)) != 1 {
				t.Fatalf("%s: expected the sole original request merged by %q at the remote base tip, got %+v", phase, r.server.owner, pr)
			}
			if fastForward && pr.baseTip != opened.headSHA {
				t.Fatalf("%s: base advanced to %s, want exact PR head %s for fast-forward", phase, pr.baseTip, opened.headSHA)
			}
			if !fastForward && pr.baseTip == opened.headSHA {
				t.Fatalf("%s: base fast-forwarded to %s instead of creating a new commit", phase, pr.baseTip)
			}
			// Read the actual Git parents and file, independent of the API's
			// post-merge head. The expected parents depend on the merge method.
			contents, parents := r.readCommit(t, r.base, pr.baseTip)
			if !slices.Equal(parents, wantParents) {
				t.Fatalf("%s: merge parents %v, want %v", phase, parents, wantParents)
			}
			if contents != wantContents {
				t.Fatalf("%s: merged version.yaml = %q, want %q", phase, contents, wantContents)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: checked head %s, last view %+v", phase, opened.headSHA, pr)
		}
		time.Sleep(mergePollInterval)
	}
}

func (r *repository) publishStatus(t *testing.T, sha, check, state string) mergeStatus {
	t.Helper()
	status := r.postStatus(t, sha, check, state)
	if status.id <= 0 || status.check != check || status.state != state || status.creator == "" || status.creator == r.server.owner {
		t.Fatalf("post %s status %s on %s: want %q from independent status actor, got %+v", state, check, sha, check, status)
	}
	if got := r.latestStatus(t, sha, check); got != status {
		t.Fatalf("read %s status on exact SHA %s: got %+v, want %+v", check, sha, got, status)
	}
	return status
}

func (r *repository) assertScheduledBy(t *testing.T, phase string, number int64, want ...string) {
	t.Helper()
	got := r.readMergeSchedule(t, phase, number)
	if got.canceled || !slices.Equal(got.scheduledBy, want) {
		t.Fatalf("%s: request #%d native schedule = %+v, want actors %q and no cancellation", phase, number, got, want)
	}
}

func (r *repository) configureDefaultSquash(t *testing.T) {
	t.Helper()
	s := r.server
	repos := s.client.Repositories
	// Leave regular merge available so choosing squash is not forced by
	// repository policy. Disallow rebase and fast-forward as alternate ways
	// of getting a linear history.
	_, resp, err := repos.EditRepo(t.Context(), s.owner, r.name, giteasdk.EditRepoOption{
		DefaultMergeStyle:         new(giteasdk.MergeStyleSquash),
		AllowMerge:                new(true),
		AllowSquash:               new(true),
		AllowRebase:               new(false),
		AllowRebaseMerge:          new(false),
		AllowFastForwardOnlyMerge: new(false),
	})
	if err != nil {
		s.apiFailure(t, "configure repository default squash before apply", resp, err)
	}
	repo, resp, err := repos.GetRepo(t.Context(), s.owner, r.name)
	if err != nil {
		s.apiFailure(t, "read back repository default squash before apply", resp, err)
	}
	if repo.DefaultMergeStyle != giteasdk.MergeStyleSquash || !repo.AllowMerge || !repo.AllowSquash ||
		repo.AllowRebase || repo.AllowRebaseMerge || repo.AllowFastForwardOnlyMerge {
		t.Fatalf("read back repository merge methods before apply: default=%q merge=%t squash=%t rebase=%t rebase-merge=%t ff-only=%t",
			repo.DefaultMergeStyle, repo.AllowMerge, repo.AllowSquash, repo.AllowRebase, repo.AllowRebaseMerge, repo.AllowFastForwardOnlyMerge)
	}
}

func (r *repository) protectBase(t *testing.T) {
	t.Helper()
	s := r.server
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
	want := rule{Name: r.base, EnableStatusCheck: true, Contexts: []string{requiredMergeCheck}}
	switch s.provider {
	case "gitea":
		want.BlockOverride = true
	case "forgejo":
		want.ApplyToAdmins = true
	default:
		t.Fatalf("setup protection: no rule for provider %q", s.provider)
	}
	r.protectionRequest(t, http.MethodPost, "", want, http.StatusCreated, nil)
	var got rule
	r.protectionRequest(t, http.MethodGet, "/"+url.PathEscape(r.base), nil, http.StatusOK, &got)
	if got.Name != want.Name || got.EnablePush || got.PushAllowlist || got.BypassAllowlist ||
		!got.EnableStatusCheck || !slices.Equal(got.Contexts, want.Contexts) ||
		got.BlockOverride != want.BlockOverride || got.ApplyToAdmins != want.ApplyToAdmins {
		t.Fatalf("read back branch protection before apply: got %+v, want %+v", got, want)
	}
	branch, resp, err := s.client.Repositories.GetRepoBranch(t.Context(), s.owner, r.name, r.base)
	if err != nil {
		s.apiFailure(t, "read effective protection before apply", resp, err)
	}
	if !branch.Protected || !branch.EnableStatusCheck || branch.EffectiveBranchProtectionName != r.base ||
		branch.UserCanPush || !slices.Equal(branch.StatusCheckContexts, want.Contexts) {
		t.Fatalf("read effective protection before apply: %s protected=%t rule=%q pushable=%t check enabled=%t contexts=%v",
			r.base, branch.Protected, branch.EffectiveBranchProtectionName, branch.UserCanPush, branch.EnableStatusCheck, branch.StatusCheckContexts)
	}
}

func (r *repository) protectionRequest(t *testing.T, method, suffix string, in any, wantStatus int, out any) {
	t.Helper()
	s := r.server
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
	endpoint := strings.TrimRight(s.url, "/") + "/api/v1/repos/" + url.PathEscape(s.owner) + "/" + url.PathEscape(r.name) + "/branch_protections" + suffix
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: could not construct fixture request (%T)", phase, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+s.token)
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

func (r *repository) listRequests(t *testing.T, phase string) []*giteasdk.PullRequest {
	t.Helper()
	s := r.server
	prs, resp, err := s.client.PullRequests.ListRepoPullRequests(t.Context(), s.owner, r.name, giteasdk.ListPullRequestsOptions{
		State: giteasdk.StateAll,
	})
	if err != nil {
		s.apiFailure(t, phase+": list PRs", resp, err)
	}
	return prs
}

type mergeSnapshot struct {
	id, number                      int64
	title, author                   string
	baseBranch, headBranch, headSHA string
	baseTip, defaultTip, headTip    string
	open, draft, merged             bool
	mergedSHA, mergedBy             string
}

func (r *repository) readMergeSnapshot(t *testing.T, phase string, number int64) mergeSnapshot {
	t.Helper()
	s := r.server
	pr, resp, err := s.client.PullRequests.GetPullRequest(t.Context(), s.owner, r.name, number)
	if err != nil {
		s.apiFailure(t, fmt.Sprintf("%s: read PR #%d", phase, number), resp, err)
	}
	if pr == nil || pr.Base == nil {
		t.Fatalf("%s: PR #%d is missing base metadata", phase, number)
	}
	refs := r.refs(t)
	got := mergeSnapshot{
		id: pr.ID, number: pr.Index, title: pr.Title, author: mergeUsername(pr.Poster),
		baseBranch: pr.Base.Ref, baseTip: refs["refs/heads/"+r.base], defaultTip: refs["refs/heads/main"],
		open: pr.State == giteasdk.StateOpen, draft: pr.Draft, merged: pr.HasMerged,
		mergedBy: mergeUsername(pr.MergedBy),
	}
	// Base.Sha tracks the current branch. A deleted head branch can make
	// either provider fall back to PR refs after merge. Use Git for base tip
	// readback and compare the API head to Git only before the merge.
	if pr.Head != nil {
		got.headBranch, got.headSHA = pr.Head.Ref, pr.Head.Sha
		got.headTip = refs["refs/heads/"+pr.Head.Ref]
	}
	if pr.MergedCommitID != nil {
		got.mergedSHA = *pr.MergedCommitID
	}
	return got
}

func mergeUsername(user *giteasdk.User) string {
	if user != nil {
		return user.UserName
	}
	return ""
}

func (r *repository) readCommit(t *testing.T, branch, sha string) (string, []string) {
	t.Helper()
	r.git(t, "fetch", "--no-tags", "origin", "refs/heads/"+branch)
	parents := strings.Fields(r.git(t, "show", "-s", "--format=%P", sha))
	return r.git(t, "show", sha+":version.yaml"), parents
}

type mergeStatus struct {
	id                    int64
	check, state, creator string
}

func (r *repository) postStatus(t *testing.T, sha, check, state string) mergeStatus {
	t.Helper()
	s := r.server
	phase := fmt.Sprintf("post fixture status %s for %s at %s", state, check, sha)
	// The admin identity stands in for CI and never invokes a merge.
	status, resp, err := s.admin.Repositories.CreateStatus(t.Context(), s.owner, r.name, sha, giteasdk.CreateStatusOption{
		State:       giteasdk.StatusState(state),
		Context:     check,
		Description: "Acceptance fixture; no Actions runner",
	})
	if err != nil {
		s.apiFailure(t, phase, resp, err)
	}
	return normalizedMergeStatus(status)
}

func (r *repository) latestStatus(t *testing.T, sha, check string) mergeStatus {
	t.Helper()
	s := r.server
	// Read actual statuses scoped to the full SHA, not a branch name or
	// aggregate state (which may also contain unrelated contexts).
	phase := fmt.Sprintf("read %s status on exact SHA %s", check, sha)
	statuses, resp, err := s.client.Repositories.ListStatusesBySHA(t.Context(), s.owner, r.name, sha, giteasdk.ListStatusesOption{})
	if err != nil {
		s.apiFailure(t, phase, resp, err)
	}
	// Both pinned servers sort `highestindex` in ascending order, and
	// timestamps may tie. Select the newest status ID for this context.
	var latest *giteasdk.Status
	for _, status := range statuses {
		if status != nil && status.Context == check && (latest == nil || status.ID > latest.ID) {
			latest = status
		}
	}
	return normalizedMergeStatus(latest)
}

func normalizedMergeStatus(status *giteasdk.Status) mergeStatus {
	if status == nil {
		return mergeStatus{}
	}
	return mergeStatus{status.ID, status.Context, string(status.State), mergeUsername(status.Creator)}
}

type mergeSchedule struct {
	scheduledBy []string
	canceled    bool
}

func (r *repository) readMergeSchedule(t *testing.T, phase string, number int64) mergeSchedule {
	t.Helper()
	s := r.server
	// Both pinned providers persist the native event in
	// services/automerge/automerge.go and expose it via the issue timeline.
	events, resp, err := s.client.Issues.ListIssueTimeline(t.Context(), s.owner, r.name, number, giteasdk.ListIssueCommentOptions{})
	if err != nil {
		s.apiFailure(t, fmt.Sprintf("%s: read PR #%d timeline", phase, number), resp, err)
	}
	var got mergeSchedule
	for _, event := range events {
		if event == nil {
			t.Fatalf("%s: PR #%d timeline contained an invalid event", phase, number)
		}
		switch event.Type {
		case "pull_scheduled_merge":
			got.scheduledBy = append(got.scheduledBy, mergeUsername(event.Poster))
		case "pull_cancel_scheduled_merge":
			got.canceled = true
		}
	}
	return got
}
