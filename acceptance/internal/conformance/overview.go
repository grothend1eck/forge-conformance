package conformance

import (
	"fmt"
	"io"
	"strings"
)

// Overview writes the static catalogue without making verification claims.
func (m Matrix) Overview(w io.Writer) error {
	if err := m.validate(); err != nil {
		return err
	}
	return m.render(w, nil)
}

func (m Matrix) render(w io.Writer, results map[string]profileResult) error {
	var table, notes strings.Builder
	fmt.Fprintf(&table, "## Conformance: %s\n\n", m.Family)
	row := func(label string, text func(string) string) {
		fmt.Fprintf(&table, "| %s |", label)
		for _, profile := range m.Profiles {
			fmt.Fprintf(&table, " %s |", text(profile))
		}
		table.WriteByte('\n')
	}
	row("Capability", func(profile string) string { return profile })
	row("---", func(string) string { return "---" })
	type count struct{ supported, applicable, unassessed int }
	counts := make(map[string]count, len(m.Profiles))
	for _, cap := range m.Rows {
		row(cap.ID, func(profile string) string {
			cell, assessed := cap.Cells[profile]
			c := counts[profile]
			switch {
			case !assessed:
				c.unassessed++
			case cell.Status == Supported:
				c.supported++
				c.applicable++
			case cell.Status == Gap:
				c.applicable++
			}
			counts[profile] = c
			if !assessed {
				return "unassessed"
			}
			if cell.Reason != "" {
				fmt.Fprintf(&notes, "- %s / %s (%s): %s\n", cap.ID, profile, cell.Status, cell.Reason)
			}
			if cell.Status == Supported && results != nil {
				result := results[profile]
				if result.passed {
					return "pass"
				}
				if outcome := result.outcomes[cap.ID]; outcome != "" {
					return outcome
				}
				return "unverified"
			}
			return string(cell.Status)
		})
	}
	if results == nil {
		row("**Supported / assessed applicable**", func(profile string) string {
			c := counts[profile]
			return fmt.Sprintf("%d/%d", c.supported, c.applicable)
		})
	} else {
		verified := func(profile string) int {
			if results[profile].passed {
				return counts[profile].supported
			}
			return 0
		}
		row("**Verified / supported**", func(profile string) string {
			return fmt.Sprintf("%d/%d", verified(profile), counts[profile].supported)
		})
		row("**Verified / assessed applicable**", func(profile string) string {
			return fmt.Sprintf("%d/%d", verified(profile), counts[profile].applicable)
		})
	}
	row("**Unassessed**", func(profile string) string { return fmt.Sprint(counts[profile].unassessed) })
	table.WriteByte('\n')
	table.WriteString(notes.String())
	_, err := io.WriteString(w, table.String())
	return err
}
