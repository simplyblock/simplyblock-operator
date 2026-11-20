// The pinned-volume validator's match condition, evaluated against every
// annotation spelling a pin can carry. The condition lives in a Kustomize patch
// that webhook markers cannot express, so nothing else ties it to the keys.

package webhook

import (
	"os"
	"testing"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/yaml"

	"github.com/simplyblock/atlas/kube"
)

// Regression: 2026-10-06-pinned-volume-matchcondition-canonical-key. The match
// condition left out storage.simplyblock.io/selected-storage-node and
// storage.simplyblock.io/host-id, so a PVC carrying one was never sent to the
// validator and a pin naming an unknown storage node was admitted.
func TestPinnedVolumeMatchConditionCoversEverySpelling(t *testing.T) {
	raw, err := os.ReadFile("../../config/webhook/matchconditions_patch.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var patch struct {
		Webhooks []struct {
			MatchConditions []struct{ Expression string } `json:"matchConditions"`
		} `json:"webhooks"`
	}
	if err := yaml.Unmarshal(raw, &patch); err != nil {
		t.Fatal(err)
	}
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(patch.Webhooks[0].MatchConditions[0].Expression)
	if issues != nil && issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []kube.Key{kube.KeySelectedStorageNode, kube.KeyHostID} {
		for _, spelling := range key {
			t.Run(spelling, func(t *testing.T) {
				object := map[string]any{"metadata": map[string]any{"annotations": map[string]any{spelling: "x"}}}
				out, _, err := program.Eval(map[string]any{"object": object})
				if err != nil {
					t.Fatal(err)
				}
				if out.Value() != true {
					t.Errorf("a PVC annotated %q is not sent to the pinned-volume validator", spelling)
				}
			})
		}
	}
}
