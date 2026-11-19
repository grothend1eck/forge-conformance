package conformance

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// Overview writes the static catalogue without making verification claims.
func (m Matrix) Overview(w io.Writer) error {
	if err := m.validate(); err != nil {
		return err
	}
	return m.render(w, nil)
}

func (m Matrix) render(w io.Writer, passed []bool) error {
	var table, notes strings.Builder
	fmt.Fprintf(&table, "## Conformance: %s\n\n", m.Family)
	row := func(label string, text func(int) string) {
		fmt.Fprintf(&table, "| %s |", label)
		for i := range m.Profiles {
			fmt.Fprintf(&table, " %s |", text(i))
		}
		table.WriteByte('\n')
	}
	row("Capability", func(i int) string { return m.Profiles[i] })
	row("---", func(int) string { return "---" })
	type count struct{ required, applicable int }
	counts := make([]count, len(m.Profiles))
	for _, cap := range m.Rows {
		row(cap.Label, func(i int) string {
			cell := cap.Cells[i]
			if cell.Reason != "" && !slices.Contains(cap.Cells[:i], cell) {
				fmt.Fprintf(&notes, "- %s (%s): %s\n", cap.Label, cell.Status, cell.Reason)
			}
			if cell.Status != NotApplicable {
				counts[i].applicable++
			}
			switch cell.Status {
			case NotEnrolled:
				return "not enrolled"
			case Required:
				counts[i].required++
				if passed != nil {
					if passed[i] {
						return "pass"
					}
					return "unverified"
				}
			}
			return string(cell.Status)
		})
	}
	if passed == nil {
		row("**Required / applicable**", func(i int) string { c := counts[i]; return fmt.Sprintf("%d/%d", c.required, c.applicable) })
	} else {
		verified := func(i int) int {
			if passed[i] {
				return counts[i].required
			}
			return 0
		}
		row("**Verified / required**", func(i int) string { return fmt.Sprintf("%d/%d", verified(i), counts[i].required) })
		row("**Verified / applicable**", func(i int) string { return fmt.Sprintf("%d/%d", verified(i), counts[i].applicable) })
	}
	fmt.Fprintf(&table, "\nOnly N/A is excluded from applicable capabilities.\n\n%s", notes.String())
	_, err := io.WriteString(w, table.String())
	return err
}
