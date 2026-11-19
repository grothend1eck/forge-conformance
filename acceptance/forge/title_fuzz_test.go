package forge

import (
	"cmp"
	"regexp"
	"strings"
	"testing"

	sdk "gitea.dev/sdk"
	"github.com/updatecli/updatecli/acceptance/internal/conformance"
	gitea "github.com/updatecli/updatecli/pkg/plugins/resources/gitea/pullrequest"
	gitlab "github.com/updatecli/updatecli/pkg/plugins/resources/gitlab/mergerequest"
)

type titleEdit struct {
	current, configured, generated string
	draft                          bool
}

// Read the title from the same SDK options CreateAction sends to the forge.
// Both Gitea and Forgejo use the Gitea action.
type titleEditor struct {
	edit        func(titleEdit) *string
	draftMarker *regexp.Regexp
}

var giteaEditor = titleEditor{
	edit: func(e titleEdit) *string {
		spec := gitea.Spec{Title: e.configured}
		opts := spec.EditOptions(&sdk.PullRequest{Title: e.current, Draft: e.draft}, e.generated, "")
		return &opts.Title
	},
	draftMarker: regexp.MustCompile(`(?i)^(?:WIP:|\[WIP\])`),
}

var titleEditors = map[string]titleEditor{
	"gitea":   giteaEditor,
	"forgejo": giteaEditor,
	"gitlab": {edit: func(e titleEdit) *string {
		spec := gitlab.Spec{Title: e.configured}
		return spec.EditOptions(e.generated, "").Title
	}},
}

func editorsFor(f *testing.F, capability string) []conformance.Implementation[titleEditor] {
	f.Helper()
	editors, err := conformance.Bind(Matrix, capability, titleEditors)
	if err != nil {
		f.Fatal(err)
	}
	if len(editors) == 0 {
		f.Fatalf("%s has no supported implementation to fuzz", capability)
	}
	return editors
}

func FuzzEditRequestTitle(f *testing.F) {
	editors := editorsFor(f, editRequestTitle)
	f.Add("Before", "Bump v2", "")
	f.Add("Keep the request", "Generated title", `Release: "café" → v2`)
	f.Add("", "", "")
	f.Fuzz(func(t *testing.T, previous, generated, configured string) {
		// Start with an ordinary open request, regardless of what the fuzzer
		// generates. Title-marked drafts have their own contract below.
		input := titleEdit{current: "Open: " + previous, generated: generated, configured: configured}
		want := cmp.Or(configured, generated)
		for _, impl := range editors {
			if got := impl.Driver.edit(input); got == nil || *got != want {
				t.Fatalf("%s: edit must send title %q (configured %q, generated %q)", impl.Name, want, configured, generated)
			}
			replay := input
			replay.current = want
			if again := impl.Driver.edit(replay); again == nil || *again != want {
				t.Fatalf("%s: repeating the edit must keep title %q", impl.Name, want)
			}
		}
	})
}

func FuzzPreserveDraftTitle(f *testing.F) {
	editors := editorsFor(f, draftTitleMarker)
	for _, impl := range editors {
		if impl.Driver.draftMarker == nil {
			f.Fatalf("%s: missing a draft marker for this capability", impl.Name)
		}
	}
	f.Add("WIP: Update v1", "Update v2", true)
	f.Add("[wIp]Update v1", "Update v2", false)
	f.Add("wip: Update v1", "[WIP] WIP: Update v2", true)
	f.Add("WIP: [WIP] Update v1", " \t[WIP] Update v2", true)
	f.Add("Draft: Update v1", "Update v2", true)
	f.Add("[WIP] Update v1", "", false)
	f.Add("Update v1", "WIP: Update v2", false)
	f.Fuzz(func(t *testing.T, current, requested string, draft bool) {
		for _, impl := range editors {
			marker := impl.Driver.draftMarker
			input := titleEdit{current: current, generated: requested, draft: draft}
			got := impl.Driver.edit(input)
			if got == nil {
				t.Fatalf("%s: edit omitted the title", impl.Name)
			}
			prefix := marker.FindString(current)
			switch {
			case marker.MatchString(requested), !draft && prefix == "":
				if *got != requested {
					t.Fatalf("%s: got %q, want the requested title %q", impl.Name, *got, requested)
				}
			case requested == "" || prefix == "":
				if *got != current {
					t.Fatalf("%s: edit lost the draft title %q: got %q", impl.Name, current, *got)
				}
			default:
				text, marked := strings.CutPrefix(*got, prefix)
				separator, retained := strings.CutSuffix(text, requested)
				if !marked || !retained || strings.TrimSpace(separator) != "" {
					t.Fatalf("%s: got %q, want marker %q followed by %q", impl.Name, *got, prefix, requested)
				}
			}
			input.current = *got
			if again := impl.Driver.edit(input); again == nil || *again != *got {
				t.Fatalf("%s: repeating the edit changed draft title %q", impl.Name, *got)
			}
		}
	})
}
