//go:build acceptance

package forge

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.dev/sdk"
	"github.com/stretchr/testify/require"
)

const (
	resourceInput     = "input.txt"
	resourceOutput    = "selected.txt"
	resourceUntouched = "not selected"
	resourceMissing   = "v77.0.0"
)

func testBranches(t *testing.T, s *server) {
	r := s.newRepo(t)
	for _, branch := range []string{"v1.9.0", "v1.10.0", "v2.0.0", "release/candidate"} {
		r.git(t, "push", "origin", r.base+":refs/heads/"+branch)
	}
	before := r.refs(t)
	branches, resp, err := s.client.Repositories.ListRepoBranches(t.Context(), s.owner, r.name, gitea.ListRepoBranchesOptions{})
	if err != nil {
		s.apiFailure(t, "read fixture branches", resp, err)
	}
	if len(branches) < 4 {
		t.Fatalf("read fixture branches: got %d branches, want at least four", len(branches))
	}
	// With no filter the resource takes the final item in server list order,
	// not the repository's default branch or the most recent Git commit.
	last := branches[len(branches)-1].Name
	if before["refs/heads/"+last] == "" {
		t.Fatalf("API branch %q is absent from remote Git refs", last)
	}
	testResourceSelectors(t, s, r.name, "branch", []resourceSelection{
		{name: "default", want: last},
		{name: "semver", kind: "semver", pattern: "~1", want: "v1.10.0"},
		{name: "regex", kind: "regex", pattern: `^release/`, want: "release/candidate"},
		{name: "latest-exact", kind: "latest", pattern: r.base, want: r.base},
		{name: "no-match", kind: "regex", pattern: `^missing/`, wantError: `no version found matching pattern "^missing/"`},
	})
	testResourceConditions(t, s, r.name, "branch", "branch", "release/candidate")
	testUnsupportedRefTarget(t, s, r.name, "branch", "target not supported for the plugin Gitea branch")
	if got := r.refs(t); !maps.Equal(got, before) {
		t.Fatalf("branch sources/conditions and rejected target changed remote refs: got %v, want %v", got, before)
	}
}

func testTags(t *testing.T, s *server) {
	r := s.newRepo(t)
	r.git(t, "tag", "v1.9.0", r.base)
	r.git(t, "push", "origin", "refs/tags/v1.9.0")
	// Use a single known ref for the default: the Git refs endpoint's list
	// order is not a portable recency contract.
	t.Run("source/default", func(t *testing.T) {
		checkResourceSelection(t, s, r.name, "tag", resourceSelection{want: "v1.9.0"})
	})
	for _, tag := range []string{"v1.10.0", "v2.0.0", "snapshot/nightly"} {
		r.git(t, "tag", tag, r.base)
	}
	r.git(t, "push", "origin", "--tags")
	before := r.refs(t)
	testResourceSelectors(t, s, r.name, "tag", []resourceSelection{
		{name: "semver", kind: "semver", pattern: "~1", want: "v1.10.0"},
		{name: "regex", kind: "regex", pattern: `^snapshot/`, want: "snapshot/nightly"},
		{name: "latest-exact", kind: "latest", pattern: "v1.9.0", want: "v1.9.0"},
		{name: "no-match", kind: "semver", pattern: "~7", wantError: `no Gitea tags found matching pattern "~7"`},
	})
	testResourceConditions(t, s, r.name, "tag", "tag", "snapshot/nightly")
	testUnsupportedRefTarget(t, s, r.name, "tag", "target not supported for the plugin Gitea Tags")
	if got := r.refs(t); !maps.Equal(got, before) {
		t.Fatalf("tag sources/conditions and rejected target changed remote refs: got %v, want %v", got, before)
	}
}

func testReleases(t *testing.T, s *server) {
	t.Run("paginated_source", func(t *testing.T) {
		r, release := seedReleaseOnLaterPage(t, s)
		before := r.refs(t)
		checkResourceSelection(t, s, r.name, "release", resourceSelection{
			kind: "latest", pattern: release.TagName, want: release.TagName,
		})
		require.Equal(t, before, r.refs(t), "the selector and condition must not modify remote refs")
	})
	t.Run("selection-and-conditions", func(t *testing.T) {
		r := s.newRepo(t)
		resourceReleaseReady(t, r, r.base)
		// Publish the highest semver first, so default/latest and a semver
		// constraint need not return the same tag. A draft is newer still.
		for _, tag := range []string{"v2.0.0", "v1.9.0", "v1.10.0", "v1.8.0", "v9.0.0"} {
			draft := tag == "v9.0.0"
			if _, resp, err := s.client.Releases.CreateRelease(t.Context(), s.owner, r.name, gitea.CreateReleaseOption{
				TagName: tag, Target: r.base, Title: tag, IsDraft: draft,
			}); err != nil {
				s.apiFailure(t, "seed release "+tag, resp, err)
			}
		}
		releases := readResourceReleases(t, s, r.name)
		before := r.refs(t)
		var latest string
		for _, release := range releases {
			if !release.IsDraft {
				latest = release.TagName
				break
			}
		}
		if len(releases) != 5 || latest == "" {
			t.Fatalf("expected five seeded releases including a published release; got %d, latest published = %q", len(releases), latest)
		}
		testResourceSelectors(t, s, r.name, "release", []resourceSelection{
			{name: "default", want: latest},
			{name: "latest", kind: "latest", want: latest},
			{name: "semver", kind: "semver", pattern: "~1", want: "v1.10.0"},
			{name: "regex", kind: "regex", pattern: `^v2\.`, want: "v2.0.0"},
			{name: "age-match", kind: "semver", pattern: "~1", age: map[string]string{"maximum": "24h"}, want: "v1.10.0"},
			{name: "age-hold", kind: "semver", pattern: "~1", age: map[string]string{"minimum": "30d"}, hold: true},
			{name: "no-match-with-age", kind: "semver", pattern: "~7", age: map[string]string{"minimum": "30d"}, wantError: `no Gitea Release tag found matching pattern "~7"`},
			{name: "draft-excluded", kind: "latest", pattern: "v9.0.0", wantError: `no version found matching pattern "v9.0.0"`},
		})
		testResourceConditions(t, s, r.name, "release", "tag", "v2.0.0")
		t.Run("condition/ignores-selector-age-and-excludes-drafts", func(t *testing.T) {
			c := newCLI(t, s)
			writeResourceFile(t, c.dir, resourceOutput, resourceUntouched)
			fresh := resourceNode(s, r.name, "release", map[string]any{
				"tag": "v2.0.0", "age": map[string]string{"minimum": "30d"},
			})
			fresh["disablesourceinput"] = true
			draft := resourceNode(s, r.name, "release", map[string]any{"tag": "v9.0.0"})
			draft["disablesourceinput"], draft["failwhen"] = true, true
			c.ok(t, "apply", manifestYAML(t, map[string]any{
				"name":       "Only published releases satisfy conditions",
				"conditions": map[string]any{"published": fresh, "notPublished": draft},
				"targets":    map[string]any{"check": resourceFileNode(resourceOutput, "passed")},
			}))
			assertResourceFile(t, c.dir, resourceOutput, "passed")
		})
		if got := r.refs(t); !maps.Equal(got, before) {
			t.Fatalf("release selectors and conditions changed remote refs: got %v, want %v", got, before)
		}
	})
	t.Run("target/explicit-create-preview-and-no-op", func(t *testing.T) {
		r := s.newRepo(t)
		r.stage(t, r.base, "stable.txt", "release commit\n")
		c := newCLI(t, s)
		before := resourceReleaseReady(t, r, r.base)
		const tag, title, description = "v4.0.0-rc.1", "Test candidate", "Release notes\n\n- Ship the fixture"
		writeResourceFile(t, c.dir, resourceInput, "v0.0.1")
		spec := map[string]any{
			"tag": tag, "title": title, "description": description, "commitish": r.base, "prerelease": true,
		}
		target := resourceNode(s, r.name, "release", spec)
		target["sourceid"] = "input" // Explicit tag takes precedence over this value.
		manifest := map[string]any{
			"name":    "Ensure the explicit candidate exists",
			"sources": map[string]any{"input": resourceFileNode(resourceInput, "")},
			"targets": map[string]any{"ensure": target},
		}
		out := c.ok(t, "diff", manifestYAML(t, manifest))
		requireResourceOutput(t, out, "doesn't exist and should be created")
		if got := readResourceReleases(t, s, r.name); len(got) != 0 {
			t.Fatalf("diff created %d API releases", len(got))
		}
		if got := r.refs(t); !maps.Equal(got, before) {
			t.Fatalf("diff changed remote refs: got %v, want %v", got, before)
		}

		c.ok(t, "apply", manifestYAML(t, manifest))
		release := onlyResourceRelease(t, s, r.name)
		assertResourceRelease(t, release, tag, title, description, r.base, false, true)
		tagRef := "refs/tags/" + tag
		r.git(t, "fetch", "origin", tagRef)
		if got, want := strings.TrimSpace(r.git(t, "rev-parse", "FETCH_HEAD^{commit}")), before["refs/heads/"+r.base]; got != want {
			t.Fatalf("created tag points to %q, want explicit commitish %q", got, want)
		}
		after := r.refs(t)
		expected := maps.Clone(before)
		expected[tagRef] = after[tagRef]
		if after[tagRef] == "" || !maps.Equal(after, expected) {
			t.Fatalf("expected creation of only %s, before=%v after=%v", tagRef, before, after)
		}

		// The target only ensures existence. Even when the requested metadata
		// changes, it does not edit or recreate an existing release/tag.
		spec["title"], spec["description"], spec["commitish"], spec["prerelease"] = "Changed title", "Changed body", "main", false
		target["spec"] = resourceNode(s, r.name, "release", spec)["spec"]
		out = c.ok(t, "apply", manifestYAML(t, manifest))
		requireResourceOutput(t, out, "already exist")
		got := onlyResourceRelease(t, s, r.name)
		if got.ID != release.ID || !got.PublishedAt.Equal(release.PublishedAt) {
			t.Fatalf("existing release was replaced or republished: id=%d time=%s; want id=%d time=%s", got.ID, got.PublishedAt, release.ID, release.PublishedAt)
		}
		assertResourceRelease(t, got, tag, title, description, r.base, false, true)
		if refs := r.refs(t); !maps.Equal(refs, after) {
			t.Fatalf("reapply changed remote refs: got %v, want %v", refs, after)
		}
	})
	t.Run("target/source-fed-defaults", func(t *testing.T) {
		r := s.newRepo(t)
		r.stage(t, r.base, "stable.txt", "alternate commit\n")
		before := resourceReleaseReady(t, r, "main")
		main, base := before["refs/heads/main"], before["refs/heads/"+r.base]
		if main == "" || base == "" || r.base == "main" || main == base {
			t.Fatalf("fixture needs distinct main and default branch commits, got %v", before)
		}
		const tag = "v5.0.0"
		c := newCLI(t, s)
		writeResourceFile(t, c.dir, resourceInput, tag)
		target := resourceNode(s, r.name, "release", nil)
		target["sourceid"] = "version"
		c.ok(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Create a release from a source-fed tag",
			"sources": map[string]any{"version": resourceFileNode(resourceInput, "")},
			"targets": map[string]any{"ensure": target},
		}))
		assertResourceRelease(t, onlyResourceRelease(t, s, r.name), tag, tag, "", "main", false, false)
		r.git(t, "fetch", "origin", "refs/tags/"+tag)
		if got := strings.TrimSpace(r.git(t, "rev-parse", "FETCH_HEAD^{commit}")); got != main {
			t.Fatalf("default commitish points to %q, want main at %q", got, main)
		}
	})
	t.Run("target/draft", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const tag = "v6.0.0"
		target := resourceNode(s, r.name, "release", map[string]any{"tag": tag, "commitish": r.base, "draft": true})
		target["disablesourceinput"] = true
		before := resourceReleaseReady(t, r, r.base)
		c.ok(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Create a draft without publishing its Git tag",
			"targets": map[string]any{"draft": target},
		}))
		assertResourceRelease(t, onlyResourceRelease(t, s, r.name), tag, tag, "", r.base, true, false)
		if got := r.refs(t); !maps.Equal(got, before) {
			t.Fatalf("creating a draft changed remote refs: got %v, want %v", got, before)
		}
	})
}

type resourceSelection struct {
	name      string
	kind      string
	pattern   string
	age       map[string]string
	want      string
	hold      bool
	wantError string
}

func testResourceSelectors(t *testing.T, s *server, repo, kind string, cases []resourceSelection) {
	t.Helper()
	for _, tc := range cases {
		t.Run("source/"+tc.name, func(t *testing.T) {
			checkResourceSelection(t, s, repo, kind, tc)
		})
	}
}

func checkResourceSelection(t *testing.T, s *server, repo, kind string, tc resourceSelection) {
	t.Helper()
	c := newCLI(t, s)
	spec := map[string]any{}
	if tc.kind != "" {
		spec["versionfilter"] = map[string]string{"kind": tc.kind, "pattern": tc.pattern}
	}
	if tc.age != nil {
		spec["age"] = tc.age
	}
	manifest := map[string]any{
		"name":    "Select a Gitea " + kind,
		"sources": map[string]any{"selection": resourceNode(s, repo, kind, spec)},
	}
	if tc.wantError != "" {
		// Leave the source as a leaf so its error isn't masked by skipped dependents.
		res := c.run(t, "apply", manifestYAML(t, manifest))
		if res.code != 1 {
			t.Fatalf("source failure: apply status=%d, want 1:\n%s", res.code, res.output)
		}
		requireResourceOutput(t, res.output, tc.wantError)
		return
	}

	writeResourceFile(t, c.dir, resourceOutput, resourceUntouched)
	condition := resourceNode(s, repo, kind, nil)
	condition["sourceid"] = "selection"
	target := resourceFileNode(resourceOutput, "")
	target["sourceid"] = "selection"
	manifest["conditions"] = map[string]any{"exists": condition}
	manifest["targets"] = map[string]any{"record": target}
	out := c.ok(t, "apply", manifestYAML(t, manifest))
	if tc.hold {
		requireResourceOutput(t, out, "matches the age filter yet")
		assertResourceFile(t, c.dir, resourceOutput, resourceUntouched)
		return
	}
	assertResourceFile(t, c.dir, resourceOutput, tc.want)
}

func testResourceConditions(t *testing.T, s *server, repo, kind, field, present string) {
	t.Helper()
	for _, tc := range []struct {
		name       string
		explicit   string
		fed        bool
		failWhen   bool
		wantPassed bool
	}{
		{name: "explicit-overrides-source", explicit: present, fed: true, wantPassed: true},
		{name: "source-missing", fed: true},
		{name: "failwhen-missing", explicit: resourceMissing, failWhen: true, wantPassed: true},
		{name: "failwhen-present", explicit: present, failWhen: true},
	} {
		t.Run("condition/"+tc.name, func(t *testing.T) {
			c := newCLI(t, s)
			writeResourceFile(t, c.dir, resourceOutput, resourceUntouched)
			spec := map[string]any{}
			if tc.explicit != "" {
				spec[field] = tc.explicit
			}
			condition := resourceNode(s, repo, kind, spec)
			condition["failwhen"] = tc.failWhen
			target := resourceFileNode(resourceOutput, "passed")
			target["disablesourceinput"] = true
			manifest := map[string]any{
				"name":       "Gate a local write with Gitea " + kind,
				"conditions": map[string]any{"exists": condition},
				"targets":    map[string]any{"check": target},
			}
			if tc.fed {
				writeResourceFile(t, c.dir, resourceInput, resourceMissing)
				manifest["sources"] = map[string]any{"input": resourceFileNode(resourceInput, "")}
				condition["sourceid"] = "input"
			} else {
				condition["disablesourceinput"] = true
			}
			// An unmet condition gates the target; it is not a CLI execution error.
			c.ok(t, "apply", manifestYAML(t, manifest))
			want := resourceUntouched
			if tc.wantPassed {
				want = "passed"
			}
			assertResourceFile(t, c.dir, resourceOutput, want)
		})
	}
}

func testUnsupportedRefTarget(t *testing.T, s *server, repo, kind, wantError string) {
	t.Helper()
	t.Run("target/unsupported", func(t *testing.T) {
		c := newCLI(t, s)
		target := resourceNode(s, repo, kind, map[string]any{kind: resourceMissing})
		target["disablesourceinput"] = true
		res := c.run(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Reject an unsupported Gitea target",
			"targets": map[string]any{"unsupported": target},
		}))
		if res.code != 1 {
			t.Fatalf("unsupported target: apply status=%d, want 1:\n%s", res.code, res.output)
		}
		requireResourceOutput(t, res.output, wantError)
	})
}

func resourceNode(s *server, repo, kind string, extra map[string]any) map[string]any {
	spec := map[string]any{
		"url": s.url, "owner": s.owner, "repository": repo,
	}
	maps.Copy(spec, extra)
	return map[string]any{"kind": "gitea/" + kind, "spec": spec}
}

func resourceFileNode(file, content string) map[string]any {
	spec := map[string]any{"file": file}
	if content != "" {
		spec["content"] = content
	}
	return map[string]any{"kind": "file", "spec": spec}
}

func writeResourceFile(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertResourceFile(t *testing.T, dir, file, want string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("%s = %q, want %q", file, content, want)
	}
}

func requireResourceOutput(t *testing.T, output, want string) {
	t.Helper()
	if !strings.Contains(output, want) {
		t.Fatalf("CLI output does not contain %q:\n%s", want, output)
	}
}

func resourceReleaseReady(t *testing.T, r *repository, branch string) map[string]string {
	t.Helper()
	refs := r.refs(t)
	want := refs["refs/heads/"+branch]
	require.NotEmpty(t, want, "remote Git branch %s is missing", branch)
	// Forgejo can accept Git pushes before the API sees a nonempty repository.
	// Wait for the repository and commitish via REST; never retry a release write.
	s := r.server
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	require.Eventually(t, func() bool {
		repo, _, err := s.client.Repositories.GetRepo(ctx, s.owner, r.name)
		if err != nil || repo == nil || repo.Empty {
			return false
		}
		ref, _, err := s.client.Repositories.GetRepoBranch(ctx, s.owner, r.name, branch)
		return err == nil && ref != nil && ref.Commit != nil && ref.Commit.ID == want
	}, 15*time.Second, 100*time.Millisecond, "forge REST API did not recognize the seeded repository and %s at %s", branch, want)
	return refs
}

func readResourceReleases(t *testing.T, s *server, repo string) []*gitea.Release {
	t.Helper()
	releases, resp, err := s.client.Releases.ListReleases(t.Context(), s.owner, repo, gitea.ListReleasesOptions{})
	if err != nil {
		s.apiFailure(t, "read releases", resp, err)
	}
	return releases
}

func onlyResourceRelease(t *testing.T, s *server, repo string) *gitea.Release {
	t.Helper()
	releases := readResourceReleases(t, s, repo)
	if len(releases) != 1 {
		t.Fatalf("API returned %d releases, want one", len(releases))
	}
	return releases[0]
}

func assertResourceRelease(t *testing.T, got *gitea.Release, tag, title, description, commitish string, draft, prerelease bool) {
	t.Helper()
	note := description + "\nMade with ❤️️ by updatecli"
	if got.TagName != tag || got.Title != title || got.Note != note || got.Target != commitish || got.IsDraft != draft || got.IsPrerelease != prerelease {
		t.Fatalf("API release tag=%q title=%q body=%q commitish=%q draft=%t prerelease=%t; want %q, %q, %q, %q, %t, %t",
			got.TagName, got.Title, got.Note, got.Target, got.IsDraft, got.IsPrerelease, tag, title, note, commitish, draft, prerelease)
	}
}
