//go:build acceptance

package forge

import (
	"testing"
	"time"

	giteasdk "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
)

const (
	requiredMergeCheck = "acceptance/required"
	mergeFixtureBefore = "version: v1.0.0\nkeep: untouched\n"
	mergeFixtureAfter  = "version: v1.1.0\nkeep: untouched\n"
	mergePollInterval  = 150 * time.Millisecond
	mergeWaitTimeout   = 20 * time.Second
)

func testProtectedMerge(t *testing.T, s *server) {
	runRequired(t, "observably_scheduled", func(t *testing.T) {
		const id = "schedule-observer"
		const title = "Acceptance: " + id
		r, before := newProtectedMergeRepo(t, s)
		newCLI(t, s).ok(t, "apply", requestManifest(t, r, id, "v1.1.0", map[string]any{
			"title": title, "merge": map[string]any{"strategy": "auto"}, "mergemethod": "merge",
		}))
		opened := r.assertOpened(t, "after initial Updatecli apply", before, title, false)
		const phase = "observe scheduling before status success"
		r.assertScheduledBy(t, phase, opened.number, s.owner)
		assertOpen(t, phase, r.readMergeSnapshot(t, phase, opened.number), opened)
	})

	runRequired(t, "merges_after_required_status", func(t *testing.T) {
		const id = "protected-auto"
		const title = "Acceptance: " + id
		r, before := newProtectedMergeRepo(t, s)
		newCLI(t, s).ok(t, "apply", requestManifest(t, r, id, "v1.1.0", map[string]any{
			"title": title, "merge": map[string]any{"strategy": "auto"}, "mergemethod": "merge",
		}))
		opened := r.assertOpened(t, "after initial Updatecli apply", before, title, false)
		head, base := opened.headSHA, before.baseTip
		pending := r.publishStatus(t, head, requiredMergeCheck, "pending")
		// Neither a success on a different SHA nor a different context on the
		// PR head can fulfill the required status for this head.
		r.publishStatus(t, base, requiredMergeCheck, "success")
		r.publishStatus(t, head, "acceptance/unrelated", "success")
		r.assertUnmergedFor(t, "wrong SHA and wrong context have passed", opened, time.Second)
		if got := r.latestStatus(t, head, requiredMergeCheck); got != pending {
			t.Fatalf("after unrelated successes: required status on %s = %+v, want %+v", head, got, pending)
		}

		// No second apply, direct merge, or change to the protection rule:
		// only the independent status actor writes after the initial apply.
		r.publishStatus(t, head, requiredMergeCheck, "success")
		r.awaitMerged(t, opened, base, head)
	})

	runRequired(t, "repository_merge_method", func(t *testing.T) {
		const id = "protected-repo-default"
		const title = "Acceptance: " + id
		r, before := newProtectedMergeRepo(t, s)
		r.configureDefaultSquash(t)
		// Omit mergemethod so the forge's configured default must be used.
		newCLI(t, s).ok(t, "apply", requestManifest(t, r, id, "v1.1.0", map[string]any{
			"title": title, "merge": map[string]any{"strategy": "auto"},
		}))
		opened := r.assertOpened(t, "after initial Updatecli apply", before, title, false)
		r.publishStatus(t, opened.headSHA, requiredMergeCheck, "success")
		// Squash creates a new commit whose only parent is the old base.
		r.awaitMerged(t, opened, before.baseTip)
	})

	runRequired(t, "existing_schedule_unchanged", func(t *testing.T) {
		const id = "keep-existing-schedule"
		r, before := newProtectedMergeRepo(t, s)
		r.configureDefaultSquash(t)
		c := newCLI(t, s)
		apply := func(version, strategy, method string) {
			t.Helper()
			c.ok(t, "apply", requestManifest(t, r, id, version, map[string]any{
				"title": version, "merge": map[string]any{"strategy": strategy}, "mergemethod": method,
			}))
		}
		apply("v1.1.0", "AuTo", "MeRgE")
		opened := r.assertOpened(t, "after scheduling with explicit merge over default squash", before, "v1.1.0", false)
		r.assertScheduledBy(t, "after the first scheduling request", opened.number, s.owner)

		apply("v1.2.0", "auto", "squash")
		edited := r.assertScheduleKeptOnEdit(t, opened, "v1.2.0")
		apply("v1.3.0", "manual", "squash")
		edited = r.assertScheduleKeptOnEdit(t, edited, "v1.3.0")

		r.publishStatus(t, edited.headSHA, requiredMergeCheck, "success")
		// The original explicit merge policy, not either later squash setting,
		// must still govern the server's merge of the latest head.
		r.awaitMergedCommit(t, edited, "version: v1.3.0\nkeep: untouched\n", false, before.baseTip, edited.headSHA)
	})

	runRequired(t, "explicit_fast_forward", func(t *testing.T) {
		const (
			id    = "explicit-ff-only"
			title = "Fast-forward after the required check"
		)
		r, before := newProtectedMergeRepo(t, s)
		// Leave regular merge and squash enabled. A fast-forward result must
		// come from the manifest, not the repository default.
		r.configureDefaultSquash(t)
		_, resp, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, giteasdk.EditRepoOption{
			AllowFastForwardOnlyMerge: new(true),
		})
		if err != nil {
			s.apiFailure(t, "allow fast-forward-only merges", resp, err)
		}
		require.True(t, publishRepo(t, r).AllowFastForwardOnlyMerge)
		newCLI(t, s).ok(t, "apply", requestManifest(t, r, id, "v1.1.0", map[string]any{
			"title": title, "merge": map[string]any{"strategy": "auto"}, "mergemethod": "fast-forward-only",
		}))

		opened := r.assertOpened(t, "after requesting an explicit fast-forward", before, title, false)
		r.assertScheduledBy(t, "before publishing the required status", opened.number, s.owner)
		r.publishStatus(t, opened.headSHA, requiredMergeCheck, "success")
		r.awaitMergedCommit(t, opened, mergeFixtureAfter, true, before.baseTip)
	})

	testExplicitMergeStyles(t, s)
}

func (r *repository) assertScheduleKeptOnEdit(t *testing.T, prior mergeSnapshot, version string) mergeSnapshot {
	t.Helper()
	const phase = "after editing an already scheduled request"
	require.Len(t, r.listRequests(t, phase), 1)
	edited := r.readMergeSnapshot(t, phase, prior.number)
	require.NotEmpty(t, edited.headSHA)
	require.NotEqual(t, prior.headSHA, edited.headSHA)
	want := prior
	want.title = version
	want.headSHA, want.headTip = edited.headSHA, edited.headSHA
	assertOpen(t, phase, edited, want)
	contents, _ := r.readCommit(t, edited.headBranch, edited.headSHA)
	require.Equal(t, "version: "+version+"\nkeep: untouched\n", contents)
	r.assertScheduledBy(t, phase, edited.number, r.server.owner)
	return edited
}

func testManualMerge(t *testing.T, s *server) {
	const id = "protected-manual"
	const title = "Acceptance: " + id
	r, before := newProtectedMergeRepo(t, s)
	newCLI(t, s).ok(t, "apply", requestManifest(t, r, id, "v1.1.0", map[string]any{
		"title": title, "merge": map[string]any{"strategy": "manual"}, "mergemethod": "merge",
	}))
	opened := r.assertOpened(t, "after initial Updatecli apply", before, title, false)
	r.publishStatus(t, opened.headSHA, requiredMergeCheck, "success")
	const phase = id + " after required status success"
	r.assertUnmergedFor(t, phase, opened, 2*time.Second)
	r.assertScheduledBy(t, phase, opened.number)
}

func testDraft(t *testing.T, s *server) {
	const id = "protected-draft"
	// Both pinned releases derive the draft flag from the WIP title.
	const title = "WIP: Acceptance: " + id
	r, before := newProtectedMergeRepo(t, s)
	newCLI(t, s).ok(t, "apply", requestManifest(t, r, id, "v1.1.0", map[string]any{
		"title": title, "merge": map[string]any{"strategy": "auto"}, "mergemethod": "merge",
	}))
	opened := r.assertOpened(t, "after initial Updatecli apply", before, title, true)
	r.publishStatus(t, opened.headSHA, requiredMergeCheck, "success")
	const phase = id + " after required status success"
	r.assertUnmergedFor(t, phase, opened, 2*time.Second)
	r.assertScheduledBy(t, phase, opened.number)
}
