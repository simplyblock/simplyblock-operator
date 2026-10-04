// The naming rule for every Prometheus metric the operator exports, checked
// against the source rather than against a list somebody maintains.
//
// design-crd-model.md §7.12 names six aggregation suffixes and no seventh, and
// binds three of them to a metric type: `total` is a counter and only a counter,
// and `count`, `state`, and `info` are gauges. The rule is worth enforcing rather
// than remembering because a gauge ending in `total` reads as something `rate()`
// applies to, and a dashboard built on that is wrong in a way nothing reports.
//
// It lives in a package of its own, above the controllers rather than in one of
// them, because the rule is the operator's and a test inside one package would
// check one package. It reads the source with go/ast instead of scraping a
// registry, so a metric declared in a package nothing imports is covered too.
//
// The csi-driver module exports its own metrics and is not read here. §7.12 is
// this operator's rule, and the driver's names are its design's to state.

package controllers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// metricPrefix is what §7.12's `simplyblock_<entity>_<item>_<agg>` begins with.
const metricPrefix = "simplyblock_"

// optsTypes are the four constructors' option structs. Finding the literal is
// what gives both the name and the metric's type, which no amount of reading the
// name alone can supply.
var optsTypes = map[string]string{
	"CounterOpts":   "counter",
	"GaugeOpts":     "gauge",
	"HistogramOpts": "histogram",
	"SummaryOpts":   "summary",
}

// aggregations are §7.12's six words, each mapped to the metric types it may
// name. An empty set is a suffix that constrains the unit rather than the type:
// a duration or a size can be measured by any of them.
var aggregations = map[string]map[string]bool{
	"total":   {"counter": true},
	"count":   {"gauge": true},
	"state":   {"gauge": true},
	"info":    {"gauge": true},
	"seconds": {},
	"bytes":   {},
}

// legacyFamilies predate §7.12 and belong to the rebalancer, which is being
// absorbed into the operator's own kinds. Their names are part of a dashboard
// somebody is running today, so they are renamed when that subsystem moves
// rather than one at a time; until then the rule would fail on names no new
// metric is allowed to imitate.
var legacyFamilies = []string{
	"simplyblock_rebalancer_",
	"simplyblock_node_fio_",
}

// declaredMetric is one metric found in the source.
type declaredMetric struct {
	name string
	kind string
	file string
	line int
}

func TestEveryMetricIsNamedTheWayTheModelSays(t *testing.T) {
	metrics := metricsDeclaredIn(t, filepath.Join("..", ".."))
	if len(metrics) == 0 {
		t.Fatal("no metric was found, so this test is checking nothing")
	}

	for _, metric := range metrics {
		if isLegacy(metric.name) {
			continue
		}
		where := metric.file + ":" + strconv.Itoa(metric.line)

		if !strings.HasPrefix(metric.name, metricPrefix) {
			t.Errorf("%s: %s does not begin with %s", where, metric.name, metricPrefix)
			continue
		}

		suffix := metric.name[strings.LastIndex(metric.name, "_")+1:]
		kinds, declared := aggregations[suffix]
		if !declared {
			t.Errorf("%s: %s ends in %q, which is not one of §7.12's six aggregations",
				where, metric.name, suffix)
			continue
		}
		if len(kinds) > 0 && !kinds[metric.kind] {
			t.Errorf("%s: %s is a %s, and §7.12 reserves the %q suffix for a %s",
				where, metric.name, metric.kind, suffix, only(kinds))
		}
	}
}

// A name nothing exports twice. Two metrics of one name are a registration panic
// at startup on the same registry, and a silent split of one series across two
// meanings on different ones.
func TestNoMetricNameIsDeclaredTwice(t *testing.T) {
	seen := map[string]declaredMetric{}
	for _, metric := range metricsDeclaredIn(t, filepath.Join("..", "..")) {
		if first, already := seen[metric.name]; already {
			t.Errorf("%s is declared at %s:%d and again at %s:%d",
				metric.name, first.file, first.line, metric.file, metric.line)
			continue
		}
		seen[metric.name] = metric
	}
}

// metricsDeclaredIn reads every non-test Go file below root and returns the
// metrics its Prometheus option literals name.
func metricsDeclaredIn(t *testing.T, root string) []declaredMetric {
	t.Helper()
	var metrics []declaredMetric
	fileSet := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			if entry.Name() == "vendor" || entry.Name() == "bin" {
				return filepath.SkipDir
			}
			return nil
		case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}

		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			kind, ok := optsKind(literal)
			if !ok {
				return true
			}
			name, ok := stringField(literal, "Name")
			if !ok {
				// A name computed rather than written. Nothing does this today,
				// and a test that quietly skipped one would report a clean run
				// over a metric it never read.
				t.Errorf("%s: a %s literal names its metric with something other "+
					"than a string constant, which this rule cannot read",
					fileSet.Position(literal.Pos()), kind)
				return true
			}
			position := fileSet.Position(literal.Pos())
			metrics = append(metrics, declaredMetric{
				name: name, kind: kind, file: position.Filename, line: position.Line,
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("read the operator's source: %v", err)
	}
	return metrics
}

// optsKind reports which of the four option structs a literal is, by the
// selector it is spelled with.
func optsKind(literal *ast.CompositeLit) (string, bool) {
	selector, ok := literal.Type.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "prometheus" {
		return "", false
	}
	kind, ok := optsTypes[selector.Sel.Name]
	return kind, ok
}

// stringField reads one string-constant field out of a composite literal.
func stringField(literal *ast.CompositeLit, field string) (string, bool) {
	for _, element := range literal.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := pair.Key.(*ast.Ident); !ok || key.Name != field {
			continue
		}
		value, ok := pair.Value.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return "", false
		}
		unquoted, err := strconv.Unquote(value.Value)
		if err != nil {
			return "", false
		}
		return unquoted, true
	}
	return "", false
}

func isLegacy(name string) bool {
	for _, family := range legacyFamilies {
		if strings.HasPrefix(name, family) {
			return true
		}
	}
	return false
}

// only names the single metric type a suffix admits, for the failure message.
func only(kinds map[string]bool) string {
	for kind := range kinds {
		return kind
	}
	return ""
}
