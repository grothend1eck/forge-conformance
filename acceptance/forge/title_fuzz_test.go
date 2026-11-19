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
	edit   func(titleEdit) *string
	drafts *draftMarkers
}

// Each adapter supplies its notation and examples; the properties below
// don't need to know which spelling or capitalization a forge recognizes.
type draftMarkers struct {
	prefix   *regexp.Regexp
	examples []string
}

var giteaEditor = titleEditor{
	edit: func(e titleEdit) *string {
		spec := gitea.Spec{Title: e.configured}
		opts := spec.EditOptions(&sdk.PullRequest{Title: e.current, Draft: e.draft}, e.generated, "")
		return &opts.Title
	},
	drafts: &draftMarkers{regexp.MustCompile(`(?i)^(?:WIP:|\[WIP\])`), []string{"WIP:", "[wIp]"}},
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
		// Start with an ordinary open request, regardless of what the fuzzer generates.
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
	seeded := make(map[*draftMarkers]bool)
	f.Add("Held by the reviewer", "New title", true) // Server-side draft with an unknown marker.
	for _, impl := range editors {
		drafts := impl.Driver.drafts
		if drafts == nil || len(drafts.examples) == 0 {
			f.Fatalf("%s: missing draft notation for this capability", impl.Name)
		}
		if !seeded[drafts] {
			for _, mark := range drafts.examples {
				f.Add(mark+" old", "New title", true)
				f.Add(mark+"old", "New title", false)
				f.Add(strings.ToLower(mark)+" old", mark+" "+mark+" new", true)
				f.Add(mark+" "+mark+" old", " \t"+mark+" new", true)
				f.Add(mark+" old", "", false)
				f.Add("Old title", mark+" new", false)
			}
			seeded[drafts] = true
		}
	}
	f.Fuzz(func(t *testing.T, current, requested string, draft bool) {
		for _, impl := range editors {
			marker := impl.Driver.drafts.prefix
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
