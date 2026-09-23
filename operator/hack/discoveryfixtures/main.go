// Writes the discovery generator's test fixtures: one directory per case in
// discovery-generator-test-cases.md, holding the probe reports, the Kubernetes
// nodes, and the OperatorOps that case is run from.
//
// It is a program rather than a table inside a test because the fixtures are
// the thing under review. A reviewer reading a directory of ConfigMaps is
// reading the fleet a case describes. The same cases expressed as Go literals
// inside a test file are readable only to somebody already holding the
// harness. The generated tree is committed, and this exists to write it again
// when a case changes rather than to be run by the tests.
//
// The case list is read from the document, and the inputs from the tables in
// this package, so neither can gain a case the other does not have: a row with
// no builder and a builder with no row are both errors that stop the run.
//
// Only the inputs are written. What each case should produce is settled after
// the tree is reviewed, which is the order §15 of the document sets out.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

func main() {
	document := flag.String("document", "../discovery-generator-test-cases.md",
		"the case document to read the rows from")
	out := flag.String("out", "internal/controllers/deployment/testdata/discovery",
		"the directory to write the case tree into")
	flag.Parse()

	if err := run(*document, *out); err != nil {
		fmt.Fprintln(os.Stderr, "discoveryfixtures:", err)
		os.Exit(1)
	}
}

func run(document, out string) error {
	rows, err := readRows(document)
	if err != nil {
		return err
	}

	builders := allCases()
	if err := reconcile(rows, builders); err != nil {
		return err
	}

	// Directories no case claims any more are removed, so a renamed or
	// withdrawn case leaves nothing behind for a harness to keep loading. What
	// is deliberately kept is the recorded expectation of a case that still
	// exists: regenerating changes the inputs, and the point of the recording
	// is that the test then fails with the diff rather than quietly agreeing
	// with whatever the new inputs produce.
	if err := pruneWithdrawn(out, rows, builders); err != nil {
		return err
	}

	ids := make([]string, 0, len(builders))
	for id := range builders {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	written := 0
	for _, id := range ids {
		if err := writeCase(out, rows[id], builders[id]); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		written++
	}
	fmt.Printf("wrote %d cases into %s\n", written, out)
	return nil
}

// reconcile refuses a document and a builder table that do not describe the
// same set of cases, which is the one way this generator can go quietly wrong.
func reconcile(rows map[string]Row, builders map[string]Case) error {
	var missing, extra []string
	for id := range rows {
		if _, built := builders[id]; !built {
			missing = append(missing, id)
		}
	}
	for id := range builders {
		if _, documented := rows[id]; !documented {
			extra = append(extra, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	var problems []string
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d case(s) in the document have no fixture: %s",
			len(missing), strings.Join(missing, ", ")))
	}
	if len(extra) > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d fixture(s) are in no row of the document: %s",
			len(extra), strings.Join(extra, ", ")))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// pruneWithdrawn removes every case directory no builder claims, and the input
// files of the ones that remain.
//
// The inputs are cleared rather than overwritten because a case whose fleet
// shrank would otherwise keep the report of a worker it no longer has. The
// expectation files are left alone.
func pruneWithdrawn(out string, rows map[string]Row, builders map[string]Case) error {
	wanted := map[string]struct{}{}
	for id, c := range builders {
		wanted[filepath.Join(out, c.Family, c.Directory(rows[id].ID))] = struct{}{}
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", out, err)
	}

	for _, family := range entries {
		if !family.IsDir() {
			continue
		}
		familyDir := filepath.Join(out, family.Name())
		cases, err := os.ReadDir(familyDir)
		if err != nil {
			return fmt.Errorf("read %s: %w", familyDir, err)
		}
		for _, entry := range cases {
			dir := filepath.Join(familyDir, entry.Name())
			if _, keep := wanted[dir]; !keep {
				if err := os.RemoveAll(dir); err != nil {
					return fmt.Errorf("remove %s: %w", dir, err)
				}
				continue
			}
			if err := clearInputs(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// clearInputs removes the files this generator owns, leaving the recorded
// expectations in place.
func clearInputs(dir string) error {
	for _, name := range []string{caseFile, opsFile, nodesFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", filepath.Join(dir, name), err)
		}
	}
	reports := filepath.Join(dir, reportsDir)
	if err := os.RemoveAll(reports); err != nil {
		return fmt.Errorf("remove %s: %w", reports, err)
	}
	return nil
}

// The files this generator owns. Everything else in a case directory is the
// harness's recording and is never touched here.
const (
	caseFile   = "case.md"
	opsFile    = "ops.yaml"
	nodesFile  = "nodes.yaml"
	reportsDir = "reports"
)

// writeCase writes one case directory: what it is, and the objects it is run
// from.
func writeCase(out string, row Row, c Case) error {
	dir := filepath.Join(out, c.Family, c.Directory(row.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, caseFile), []byte(describe(row, c)), 0o644); err != nil {
		return err
	}

	// A GO case substitutes a Planner seam the controller never does, so it has
	// no objects to be run from and its directory carries the row alone.
	if row.Harness == "GO" {
		return nil
	}

	if err := writeYAML(filepath.Join(dir, opsFile), operatorOpsFor(row.ID, c)); err != nil {
		return err
	}
	if len(c.Nodes) > 0 {
		if err := writeDocuments(filepath.Join(dir, nodesFile), c.Nodes); err != nil {
			return err
		}
	}

	maps := make([]*corev1.ConfigMap, 0, len(c.Reports))
	for _, report := range c.Reports {
		cm, err := reportConfigMap(row.ID, report)
		if err != nil {
			return err
		}
		maps = append(maps, cm)
	}
	if c.Amend != nil {
		maps = c.Amend(runName(row.ID), maps)
	}
	if len(maps) == 0 {
		return nil
	}

	if err := os.MkdirAll(filepath.Join(dir, reportsDir), 0o755); err != nil {
		return err
	}
	for i, cm := range maps {
		// The file is named for the worker rather than for the object, whose
		// own name is a digest: a directory whose every entry is
		// `sb-nodeprobe-<hash>` is a directory nobody can read.
		name := fmt.Sprintf("report-%d", i)
		if i < len(c.Reports) && c.Reports[i].Node != "" {
			name = c.Reports[i].Node
		}
		if err := writeYAML(filepath.Join(dir, reportsDir, name+".yaml"), cm); err != nil {
			return err
		}
	}
	return nil
}
