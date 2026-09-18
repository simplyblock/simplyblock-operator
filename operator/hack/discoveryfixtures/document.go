// Reading the case rows out of the document.
//
// The document is the specification and this generator is its reader, which is
// why the rows are parsed rather than restated here: a mutation described one
// way in the document and another way in a fixture is a case nobody can check,
// and the two drift the first time a row is edited.
//
// The parse is deliberately shallow. It takes the identifier, the two prose
// columns, and the harness out of any table row that opens with a case
// identifier, and it knows nothing about which section the row was in.

package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Row is one case as the document states it.
type Row struct {
	// ID is the case identifier, such as NET-13.
	ID string

	// Mutation and Expected are the row's two prose columns, in the document's
	// own words.
	Mutation string
	Expected string

	// Harness is CM for a case driven from this directory, GO for one that
	// substitutes a Planner seam and has no directory to be driven from.
	Harness string
}

// caseRow matches a table row whose first cell is a case identifier. The
// families are listed rather than matched loosely, so that a table of something
// else that happens to start with a dashed word is not read as a case.
var caseRow = regexp.MustCompile(
	`^\|\s*((?:DEV|NUMA|SIZE|PCI|FLEET|NET|ROLE|FILT|HELD|FAIL|CM|TMPL)-\d+)\s*\|(.*)$`)

// readRows reads every case the document states, keyed by identifier.
func readRows(path string) (map[string]Row, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the case document: %w", err)
	}

	rows := map[string]Row{}
	for _, line := range strings.Split(string(content), "\n") {
		match := caseRow.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		cells := strings.Split(strings.TrimSuffix(strings.TrimSpace(match[2]), "|"), "|")
		if len(cells) < 3 {
			return nil, fmt.Errorf("the row for %s has %d cells, want the mutation, the expectation, and the harness",
				match[1], len(cells)+1)
		}
		row := Row{
			ID:       match[1],
			Mutation: strings.TrimSpace(cells[0]),
			Expected: strings.TrimSpace(cells[1]),
			Harness:  strings.Trim(strings.TrimSpace(cells[2]), "`"),
		}
		if seen, repeated := rows[row.ID]; repeated {
			return nil, fmt.Errorf("%s appears twice: %q and %q", row.ID, seen.Mutation, row.Mutation)
		}
		rows[row.ID] = row
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s holds no case rows", path)
	}
	return rows, nil
}

// describe renders the case.md a directory carries: what the case is, in the
// document's own words, so that a failure is readable without the document
// open beside it.
func describe(row Row, c Case) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", row.ID)
	fmt.Fprintf(&out, "**Mutation.** %s\n\n", row.Mutation)
	fmt.Fprintf(&out, "**Expected.** %s\n\n", row.Expected)
	fmt.Fprintf(&out, "**Harness.** `%s`\n", row.Harness)
	if c.Gap != "" {
		fmt.Fprintf(&out, "\n**Gap.** %s. This case records what the generator does today, "+
			"so that the day it changes the diff is the finding.\n", c.Gap)
	}
	if c.Note != "" {
		fmt.Fprintf(&out, "\n**Note.** %s\n", c.Note)
	}
	return out.String()
}
