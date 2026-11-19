// The baseline the discovery cases are recorded against.
//
// Every case used to record the whole document it produced, so a decision that
// changes every fleet's draft the same way, such as a new cluster-wide field,
// rewrote all of them and buried the one case that came out differently. The
// values most documents share are recorded once, in testdata/discovery/
// baseline.yaml, and each case records only how its document differs from it,
// as a JSON merge patch (RFC 7386). The comparison stays exact: the delta
// applied to the baseline has to give back the document byte for byte.
//
// The notes are the one place the baseline drops something. A note is about the
// field its first word names, and a case records the note only when its value of
// that field differs from the baseline's. The wording of a note on a field the
// case shares with the baseline is not checked by the case tree.

package deployment

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// discoveryBaseline is every value more than half of the documents share at
// the same path. A list is one value, so a baseline never holds part of one.
func discoveryBaseline(documents []map[string]any) map[string]any {
	type tally struct {
		path  []string
		value any
		count int
	}
	tallies := map[string]*tally{}
	for _, document := range documents {
		walkLeaves(document, nil, func(path []string, value any) {
			canonical, err := json.Marshal(value)
			if err != nil {
				return
			}
			key := strings.Join(path, "\x00") + "\x00" + string(canonical)
			if tallies[key] == nil {
				tallies[key] = &tally{path: path, value: value}
			}
			tallies[key].count++
		})
	}

	keys := make([]string, 0, len(tallies))
	for key := range tallies {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	baseline := map[string]any{}
	for _, key := range keys {
		if entry := tallies[key]; entry.count*2 > len(documents) {
			setPath(baseline, entry.path, entry.value)
		}
	}
	return baseline
}

// walkLeaves calls visit for every value below a map that is not itself a map.
func walkLeaves(node map[string]any, prefix []string, visit func(path []string, value any)) {
	for key, value := range node {
		path := append(append([]string(nil), prefix...), key)
		if child, ok := value.(map[string]any); ok && len(child) > 0 {
			walkLeaves(child, path, visit)
			continue
		}
		visit(path, value)
	}
}

// setPath stores value at path, creating the maps on the way.
func setPath(node map[string]any, path []string, value any) {
	for _, key := range path[:len(path)-1] {
		child, ok := node[key].(map[string]any)
		if !ok {
			child = map[string]any{}
			node[key] = child
		}
		node = child
	}
	node[path[len(path)-1]] = value
}

// lookupPath is the value at path, and nil when any step of it is missing.
func lookupPath(node map[string]any, path []string) any {
	var current any = node
	for _, key := range path {
		parent, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = parent[key]
	}
	return current
}

// discoveryDelta is the merge patch that turns the baseline into the document.
func discoveryDelta(baseline, document map[string]any) (map[string]any, error) {
	base, err := json.Marshal(baseline)
	if err != nil {
		return nil, err
	}
	target, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	patch, err := jsonpatch.CreateMergePatch(base, target)
	if err != nil {
		return nil, err
	}
	var delta map[string]any
	return delta, json.Unmarshal(patch, &delta)
}

// applyDiscoveryDelta is the document a recorded delta stands for.
func applyDiscoveryDelta(baseline, delta map[string]any) (map[string]any, error) {
	base, err := json.Marshal(baseline)
	if err != nil {
		return nil, err
	}
	patch, err := json.Marshal(delta)
	if err != nil {
		return nil, err
	}
	merged, err := jsonpatch.MergePatch(base, patch)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	return document, json.Unmarshal(merged, &document)
}

// recordedDiscoveryNotes are the notes a case records: every note when the run
// wrote no document, and otherwise each note whose field the document sets
// differently from the baseline, plus the notes that name no field at all.
func recordedDiscoveryNotes(notes []string, document, baseline map[string]any) []string {
	if document == nil {
		return notes
	}
	var recorded []string
	for _, note := range notes {
		path := noteFieldPath(note)
		if path != nil && reflect.DeepEqual(lookupPath(document, path), lookupPath(baseline, path)) {
			continue
		}
		recorded = append(recorded, note)
	}
	return recorded
}

// The fields a note can be about: the cluster block's, and the spec's own.
var (
	clusterNoteFields = jsonFieldNames(reflect.TypeFor[simplyblockv1alpha2.ClusterTemplate]())
	specNoteFields    = jsonFieldNames(reflect.TypeFor[simplyblockv1alpha2.ClusterDeploymentConfigSpec]())
)

// noteFieldPath is where in the document the field a note opens with lives,
// and nil for a note that does not open with a field name.
func noteFieldPath(note string) []string {
	field, _, _ := strings.Cut(note, " ")
	switch {
	case clusterNoteFields[field]:
		return []string{"spec", "cluster", field}
	case specNoteFields[field]:
		return []string{"spec", field}
	}
	return nil
}

// jsonFieldNames is the set of JSON names a struct serializes its fields as,
// with inlined structs flattened into it.
func jsonFieldNames(structType reflect.Type) map[string]bool {
	names := map[string]bool{}
	for field := range structType.Fields() {
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if strings.Contains(options, "inline") || (field.Anonymous && name == "") {
			inner := field.Type
			if inner.Kind() == reflect.Pointer {
				inner = inner.Elem()
			}
			for innerName := range jsonFieldNames(inner) {
				names[innerName] = true
			}
			continue
		}
		if name != "" && name != "-" {
			names[name] = true
		}
	}
	return names
}
