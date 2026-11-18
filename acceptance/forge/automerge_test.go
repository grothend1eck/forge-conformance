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
	runProtectedMerge(t, giteaMergeOpener(s))
}

func testManualMerge(t *testing.T, s *server) {
	runMergeNegative(t, giteaMergeOpener(s), "protected-manual", "manual", false)
}

func testDraft(t *testing.T, s *server) {
	runMergeNegative(t, giteaMergeOpener(s), "protected-draft", "auto", true)
}

type mergeCaseSpec struct {
	pipelineID, strategy string
	draft                bool
	defaultSquash        bool // configure the repository and omit the action's mergemethod
}

type openMergeFixture func(t *testing.T, spec mergeCaseSpec) protectedMergeFixture

// A fixture prepares the protected branch and runs one real apply when opened.
// After that, only postStatus writes; none of the fixtures can request a merge.
type protectedMergeFixture interface {
	seed() mergeSeed
	snapshot(t *testing.T, phase string) mergeSnapshot
	requestCount(t *testing.T, phase string) int
	postStatus(t *testing.T, sha, check, state string) mergeStatus
	latestStatus(t *testing.T, sha, check string) mergeStatus
	schedule(t *testing.T, phase string) mergeSchedule
	readCommit(t *testing.T, branch, sha string) (contents string, parents []string)
}

type mergeSeed struct {
	baseBranch, baseTip, defaultTip string
	title, actor, originalContents  string
}

type mergeSnapshot struct {
	id, number                      int64
	title, author                   string
	baseBranch, headBranch, headSHA string
	baseTip, defaultTip, headTip    string
	open, draft, merged             bool
	mergedSHA, mergedBy             string
}

type mergeStatus struct {
	id                    int64
	check, state, creator string
}

type mergeSchedule struct {
	scheduledBy []string
	canceled    bool
}

func runProtectedMerge(t *testing.T, open openMergeFixture) {
	t.Run("observably_scheduled", func(t *testing.T) {
		m := startMergeCase(t, open, mergeCaseSpec{pipelineID: "schedule-observer", strategy: "auto"})
		const phase = "observe scheduling before status success"
		m.assertScheduled(t, phase, true)
		m.assertOpen(t, phase, m.snapshot(t, phase))
	})

	t.Run("merges_after_required_status", func(t *testing.T) {
		m := startMergeCase(t, open, mergeCaseSpec{pipelineID: "protected-auto", strategy: "auto"})
		head, base := m.initial.headSHA, m.original.baseTip
		pending := m.publishStatus(t, head, requiredMergeCheck, "pending")
		// Neither a success on a different SHA nor a different context on the
		// PR head can fulfill the required status for this head.
		m.publishStatus(t, base, requiredMergeCheck, "success")
		m.publishStatus(t, head, "acceptance/unrelated", "success")
		m.assertUnmergedFor(t, "wrong SHA and wrong context have passed", time.Second)
		if got := m.latestStatus(t, head, requiredMergeCheck); got != pending {
			t.Fatalf("after unrelated successes: required status on %s = %+v, want %+v", head, got, pending)
		}

		// Only the status fixture writes from here on. In particular, no
		// Updatecli rerun, direct merge, or change to the protection rule.
		m.publishStatus(t, head, requiredMergeCheck, "success")
		m.awaitMerged(t, base, head)
	})

	t.Run("repository_merge_method", func(t *testing.T) {
		m := startMergeCase(t, open, mergeCaseSpec{
			pipelineID: "protected-repo-default", strategy: "auto", defaultSquash: true,
		})
		m.publishStatus(t, m.initial.headSHA, requiredMergeCheck, "success")
		// Squash creates a new commit whose only parent is the old base.
		m.awaitMerged(t, m.original.baseTip)
	})
}

func runMergeNegative(t *testing.T, open openMergeFixture, pipelineID, strategy string, draft bool) {
	m := startMergeCase(t, open, mergeCaseSpec{pipelineID: pipelineID, strategy: strategy, draft: draft})
	m.publishStatus(t, m.initial.headSHA, requiredMergeCheck, "success")
	phase := pipelineID + " after required status success"
	m.assertUnmergedFor(t, phase, 2*time.Second)
	m.assertScheduled(t, phase, false)
}

type protectedMergeCase struct {
	protectedMergeFixture
	original mergeSeed
	initial  mergeSnapshot
}

func startMergeCase(t *testing.T, open openMergeFixture, spec mergeCaseSpec) protectedMergeCase {
	t.Helper()
	const phase = "after initial Updatecli apply"
	f := open(t, spec)
	m := protectedMergeCase{f, f.seed(), f.snapshot(t, phase)}
	if m.original.baseTip == "" || m.original.defaultTip == "" || m.original.originalContents != mergeFixtureBefore {
		t.Fatal("before initial Updatecli apply: unexpected seeded Git state")
	}
	pr := m.initial
	if pr.draft != spec.draft || pr.author != m.original.actor ||
		pr.id <= 0 || pr.number <= 0 || pr.headBranch == "" || pr.headBranch == m.original.baseBranch ||
		pr.headSHA == "" || pr.headSHA == m.original.baseTip {
		t.Fatalf("%s: unexpected PR identity, actor, draft state or head: %+v", phase, pr)
	}
	m.assertOpen(t, phase, pr)
	if got := m.requestCount(t, phase); got != 1 {
		t.Fatalf("%s: expected one request, got %d", phase, got)
	}
	if got, _ := m.readCommit(t, pr.headBranch, pr.headSHA); got != mergeFixtureAfter {
		t.Fatalf("%s: head version.yaml = %q, want %q", phase, got, mergeFixtureAfter)
	}
	if got := m.latestStatus(t, pr.headSHA, requiredMergeCheck); got.id != 0 {
		t.Fatalf("%s: required status on %s must initially be absent, got %+v", phase, pr.headSHA, got)
	}
	return m
}

func (m protectedMergeCase) assertIdentity(t *testing.T, phase string, got mergeSnapshot) {
	t.Helper()
	if got.id != m.initial.id || got.number != m.initial.number || got.baseBranch != m.original.baseBranch ||
		got.title != m.original.title || got.draft != m.initial.draft {
		t.Fatalf("%s: request #%d no longer has the expected identity, target, title and draft state: %+v", phase, m.initial.number, got)
	}
	if got.defaultTip != m.original.defaultTip {
		t.Fatalf("%s: non-target default branch moved from %s to %q", phase, m.original.defaultTip, got.defaultTip)
	}
}

func (m protectedMergeCase) assertOpen(t *testing.T, phase string, got mergeSnapshot) {
	t.Helper()
	m.assertIdentity(t, phase, got)
	if !got.open || got.merged || got.headBranch != m.initial.headBranch || got.headSHA != m.initial.headSHA ||
		got.headTip != m.initial.headSHA || got.baseTip != m.original.baseTip {
		t.Fatalf("%s: want request open and unmerged at %s with base %s unchanged, got %+v", phase, m.initial.headSHA, m.original.baseTip, got)
	}
}

func (m protectedMergeCase) publishStatus(t *testing.T, sha, check, state string) mergeStatus {
	t.Helper()
	status := m.postStatus(t, sha, check, state)
	if status.id <= 0 || status.check != check || status.state != state || status.creator == "" || status.creator == m.original.actor {
		t.Fatalf("post %s status %s on %s: want %q from independent status actor, got %+v", state, check, sha, check, status)
	}
	if got := m.latestStatus(t, sha, check); got != status {
		t.Fatalf("read %s status on exact SHA %s: got %+v, want %+v", check, sha, got, status)
	}
	return status
}

func (m protectedMergeCase) assertScheduled(t *testing.T, phase string, want bool) {
	t.Helper()
	got := m.schedule(t, phase)
	if got.canceled || (want && !slices.Equal(got.scheduledBy, []string{m.original.actor})) ||
		(!want && len(got.scheduledBy) != 0) {
		t.Fatalf("%s: request #%d native schedule want=%t (actor %q), got %+v", phase, m.initial.number, want, m.original.actor, got)
	}
}

func (m protectedMergeCase) assertUnmergedFor(t *testing.T, phase string, duration time.Duration) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for {
		m.assertOpen(t, phase, m.snapshot(t, phase))
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(mergePollInterval)
	}
	if got := m.requestCount(t, phase); got != 1 {
		t.Fatalf("%s: expected one request, got %d", phase, got)
	}
}

func (m protectedMergeCase) awaitMerged(t *testing.T, wantParents ...string) {
	t.Helper()
	const phase = "await automatic merge after required status success"
	deadline := time.Now().Add(mergeWaitTimeout)
	for {
		pr := m.snapshot(t, phase)
		m.assertIdentity(t, phase, pr)
		if pr.merged && !pr.open && pr.baseTip != "" && pr.baseTip != m.original.baseTip {
			if pr.mergedSHA != pr.baseTip || pr.mergedBy != m.original.actor || m.requestCount(t, phase) != 1 {
				t.Fatalf("%s: expected the sole original request merged by %q at the remote base tip, got %+v", phase, m.original.actor, pr)
			}
			if pr.baseTip == m.initial.headSHA {
				t.Fatalf("%s: base fast-forwarded to %s instead of creating a new commit", phase, pr.baseTip)
			}
			// Read the actual Git parents and file, independent of the API's
			// post-merge head. The expected parents depend on the merge method.
			contents, parents := m.readCommit(t, m.original.baseBranch, pr.baseTip)
			if !slices.Equal(parents, wantParents) {
				t.Fatalf("%s: merge parents %v, want %v", phase, parents, wantParents)
			}
			if contents != mergeFixtureAfter {
				t.Fatalf("%s: merged version.yaml = %q, want %q", phase, contents, mergeFixtureAfter)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: checked head %s, last view %+v", phase, m.initial.headSHA, pr)
		}
		time.Sleep(mergePollInterval)
	}
}

// Gitea 28.2.0 and Forgejo 17.0.0 implement the same Updatecli action and
// similar APIs; keep their wire and Git readback details in this adapter.
type giteaMergeFixture struct {
	s      *server
	r      *repository
	number int64
	before mergeSeed
}

func giteaMergeOpener(s *server) openMergeFixture {
	return func(t *testing.T, spec mergeCaseSpec) protectedMergeFixture {
		t.Helper()
		r := s.newRepo(t)
		p := &giteaMergeFixture{s: s, r: r}
		p.protectBase(t)
		if spec.defaultSquash {
			p.configureDefaultSquash(t)
		}
		refs := r.refs(t)
		p.before = mergeSeed{
			baseBranch: r.base, baseTip: refs["refs/heads/"+r.base], defaultTip: refs["refs/heads/main"],
			title: "Acceptance: " + spec.pipelineID, actor: s.owner,
		}
		if spec.draft {
			// Both pinned releases derive the API draft flag from the WIP
			// title prefix. Updatecli has no separate draft option here.
			p.before.title = "WIP: " + p.before.title
		}
		if p.before.baseTip == "" {
			t.Fatal("seed before apply: missing remote base ref")
		}
		p.before.originalContents = r.git(t, "show", p.before.baseTip+":version.yaml")
		action := map[string]any{
			"title": p.before.title,
			"merge": map[string]any{"strategy": spec.strategy},
		}
		if !spec.defaultSquash {
			action["mergemethod"] = "merge"
		}
		t.Logf("apply %s strategy on protected %s (draft=%t, repository merge method=%t)", spec.strategy, r.base, spec.draft, spec.defaultSquash)
		newCLI(t, s).ok(t, "apply", requestManifest(r, spec.pipelineID, "v1.1.0", action))
		const phase = "identify request after Updatecli apply"
		prs := p.listRequests(t, phase)
		if len(prs) != 1 || prs[0] == nil {
			t.Fatalf("%s: expected one request, got %d", phase, len(prs))
		}
		p.number = prs[0].Index
		return p
	}
}

func (p *giteaMergeFixture) seed() mergeSeed { return p.before }

func (p *giteaMergeFixture) configureDefaultSquash(t *testing.T) {
	t.Helper()
	repos := p.s.client.Repositories
	// Leave regular merge available so choosing squash is not forced by
	// repository policy. Disallow rebase and fast-forward as alternate ways
	// of getting a linear history.
	_, resp, err := repos.EditRepo(t.Context(), p.s.owner, p.r.name, giteasdk.EditRepoOption{
		DefaultMergeStyle:         new(giteasdk.MergeStyleSquash),
		AllowMerge:                new(true),
		AllowSquash:               new(true),
		AllowRebase:               new(false),
		AllowRebaseMerge:          new(false),
		AllowFastForwardOnlyMerge: new(false),
	})
	if err != nil {
		p.s.apiFailure(t, "configure repository default squash before apply", resp, err)
	}
	repo, resp, err := repos.GetRepo(t.Context(), p.s.owner, p.r.name)
	if err != nil {
		p.s.apiFailure(t, "read back repository default squash before apply", resp, err)
	}
	if repo.DefaultMergeStyle != giteasdk.MergeStyleSquash || !repo.AllowMerge || !repo.AllowSquash ||
		repo.AllowRebase || repo.AllowRebaseMerge || repo.AllowFastForwardOnlyMerge {
		t.Fatalf("read back repository merge methods before apply: default=%q merge=%t squash=%t rebase=%t rebase-merge=%t ff-only=%t",
			repo.DefaultMergeStyle, repo.AllowMerge, repo.AllowSquash, repo.AllowRebase, repo.AllowRebaseMerge, repo.AllowFastForwardOnlyMerge)
	}
}

func (p *giteaMergeFixture) protectBase(t *testing.T) {
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

func (p *giteaMergeFixture) protectionRequest(t *testing.T, method, suffix string, in any, wantStatus int, out any) {
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

func (p *giteaMergeFixture) listRequests(t *testing.T, phase string) []*giteasdk.PullRequest {
	t.Helper()
	prs, resp, err := p.s.client.PullRequests.ListRepoPullRequests(t.Context(), p.s.owner, p.r.name, giteasdk.ListPullRequestsOptions{
		State: giteasdk.StateAll,
	})
	if err != nil {
		p.s.apiFailure(t, phase+": list PRs", resp, err)
	}
	return prs
}

func (p *giteaMergeFixture) requestCount(t *testing.T, phase string) int {
	t.Helper()
	return len(p.listRequests(t, phase))
}

func (p *giteaMergeFixture) snapshot(t *testing.T, phase string) mergeSnapshot {
	t.Helper()
	pr, resp, err := p.s.client.PullRequests.GetPullRequest(t.Context(), p.s.owner, p.r.name, p.number)
	if err != nil {
		p.s.apiFailure(t, fmt.Sprintf("%s: read PR #%d", phase, p.number), resp, err)
	}
	if pr == nil || pr.Base == nil {
		t.Fatalf("%s: PR #%d is missing base metadata", phase, p.number)
	}
	refs := p.r.refs(t)
	got := mergeSnapshot{
		id: pr.ID, number: pr.Index, title: pr.Title, author: mergeUsername(pr.Poster),
		baseBranch: pr.Base.Ref, baseTip: refs["refs/heads/"+p.r.base], defaultTip: refs["refs/heads/main"],
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

func (p *giteaMergeFixture) readCommit(t *testing.T, branch, sha string) (string, []string) {
	t.Helper()
	p.r.git(t, "fetch", "--no-tags", "origin", "refs/heads/"+branch)
	parents := strings.Fields(p.r.git(t, "show", "-s", "--format=%P", sha))
	return p.r.git(t, "show", sha+":version.yaml"), parents
}

func (p *giteaMergeFixture) postStatus(t *testing.T, sha, check, state string) mergeStatus {
	t.Helper()
	phase := fmt.Sprintf("post fixture status %s for %s at %s", state, check, sha)
	// The admin identity stands in for CI and never invokes a merge.
	status, resp, err := p.s.admin.Repositories.CreateStatus(t.Context(), p.s.owner, p.r.name, sha, giteasdk.CreateStatusOption{
		State:       giteasdk.StatusState(state),
		Context:     check,
		Description: "Acceptance fixture; no Actions runner",
	})
	if err != nil {
		p.s.apiFailure(t, phase, resp, err)
	}
	return normalizedMergeStatus(status)
}

func (p *giteaMergeFixture) latestStatus(t *testing.T, sha, check string) mergeStatus {
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
	return normalizedMergeStatus(latest)
}

func normalizedMergeStatus(status *giteasdk.Status) mergeStatus {
	if status == nil {
		return mergeStatus{}
	}
	return mergeStatus{status.ID, status.Context, string(status.State), mergeUsername(status.Creator)}
}

func (p *giteaMergeFixture) schedule(t *testing.T, phase string) mergeSchedule {
	t.Helper()
	// Both pinned providers persist the native event in
	// services/automerge/automerge.go and expose it via the issue timeline.
	events, resp, err := p.s.client.Issues.ListIssueTimeline(t.Context(), p.s.owner, p.r.name, p.number, giteasdk.ListIssueCommentOptions{})
	if err != nil {
		p.s.apiFailure(t, fmt.Sprintf("%s: read PR #%d timeline", phase, p.number), resp, err)
	}
	var got mergeSchedule
	for _, event := range events {
		if event == nil {
			t.Fatalf("%s: PR #%d timeline contained an invalid event", phase, p.number)
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
