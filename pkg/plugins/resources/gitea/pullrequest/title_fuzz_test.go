package pullrequest

import (
	"regexp"
	"strings"
	"testing"

	giteasdk "gitea.dev/sdk"
)

func FuzzExistingPullRequestTitle(f *testing.F) {
	f.Add("Update v1", "Update v2", false)
	f.Add("WIP: Update v1", "Update v2", true)
	f.Add("[wIp]Update v1", "Update v2", false)
	f.Add("wip: Update v1", "[WIP] WIP: Update v2", true)
	f.Add("WIP: [WIP] Update v1", "Update v2", false)
	f.Add("WIP: Update v1", " \t[WIP] Update v2", true)
	f.Add("Draft: Update v1", "Update v2", true)
	f.Add("[WIP] Update v1", "", false)
	f.Add("Update v1", "WIP: Update v2", false)

	marker := regexp.MustCompile(`(?i)^(?:WIP:|\[WIP\])`)
	f.Fuzz(func(t *testing.T, remote, requested string, draft bool) {
		pr := giteasdk.PullRequest{Title: remote, Draft: draft}
		got := existingPullRequestTitle(requested, &pr)
		saved := giteasdk.PullRequest{Title: got, Draft: draft}
		if again := existingPullRequestTitle(requested, &saved); again != got {
			t.Fatalf("replaying %q on %q (draft=%t) changed %q to %q", requested, remote, draft, got, again)
		}

		prefix := marker.FindString(remote)
		if prefix == "" || requested == "" {
			return
		}
		if marker.MatchString(requested) {
			if got != requested {
				t.Fatalf("edit of %q changed marked title %q to %q", remote, requested, got)
			}
			return
		}

		// Only whitespace may separate the original marker and requested text.
		rest, hasMarker := strings.CutPrefix(got, prefix)
		separator, hasTitle := strings.CutSuffix(rest, requested)
		if !hasMarker || !hasTitle || strings.TrimSpace(separator) != "" {
			t.Fatalf("edit of %q produced %q; want only marker %q before %q", remote, got, prefix, requested)
		}
	})
}
