// The Layer contract, asserted against every layer in this package at once.
//
// Each rule below is a sentence the interface promises, quoted where it is
// short enough, and every shipped layer is put through all of them. A rule
// broken in one layer is a bug in that layer; a rule broken in the next layer
// somebody writes is the same bug, and the point of testing them here rather
// than in each layer's own file is that the next one is covered before it is
// written.
//
// What these deliberately do not assert is which answer a layer gives. Whether
// a dead foundation means absent is genuinely the layer's own call: an LVM
// volume group with no member device left may still be mapped as live
// device-mapper nodes this host has to release, while a physical-volume label
// cannot outlive the device it was written on. The contract is that a layer
// answers, and answers the same way twice, not what it answers.
//
// Nothing here reaches a host. Every seam is a fake, so a rule that failed only
// on a real node would not be caught, and the rules chosen are the ones that do
// not need one.

package layers

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/blockdev"
	"github.com/simplyblock/atlas/lvm"
	"github.com/simplyblock/atlas/lvol"
	"github.com/simplyblock/atlas/volstack"
)

// shippedLayers is every layer this package provides, built with seams that
// answer rather than reach a host.
//
// Built fresh per call: the rules below run verbs against these, and a layer
// carrying state from a previous rule would make the next one's result depend
// on the order they happen to run in.
func shippedLayers() map[string]volstack.Layer {
	commands := newLVM()

	return map[string]volstack.Layer{
		"fabric": NewFabric(FabricConfig{
			Connection: lvol.Connection{NQN: "nqn.2023-02.io.simplyblock:vol", NSID: 1},
			Connector:  &fakeConnector{},
			Devices:    &fakeDevices{},
		}),
		"members": NewMembers(volstack.Plan{}),
		"lvmPhysicalVolume": NewLVMPhysicalVolume(LVMPhysicalVolumeConfig{
			VolumeGroup:   "vol-x",
			LogicalVolume: "lv-x",
			Manager:       lvm.NewManagerWithRunner(commands.run),
			Content:       fakeReader{reading: blockdev.Reading{Content: blockdev.ContentBlank}},
		}),
		"lvmVolumeGroup": NewLVMVolumeGroup(LVMVolumeGroupConfig{
			VolumeGroup: "vol-x",
			Manager:     lvm.NewManagerWithRunner(commands.run),
		}),
		"lvmLogicalVolume": NewLVMLogicalVolume(LVMLogicalVolumeConfig{
			VolumeGroup:   "vol-x",
			LogicalVolume: "lv-x",
			Manager:       lvm.NewManagerWithRunner(commands.run),
		}),
		"filesystem": NewFilesystem(FilesystemConfig{
			FsType:      "ext4",
			StagingPath: stagingPath,
			Ops:         newFakeFS(),
			Content:     fakeReader{reading: blockdev.Reading{Content: blockdev.ContentBlank}},
		}),
	}
}

// deadFoundation is the artifact a layer is handed once the layer below it is
// gone, which on the teardown path is the ordinary case rather than an unusual
// one: releasing is what detaches the fabric, so everything above it is
// standing on nothing by the time the walk arrives.
var deadFoundation = volstack.Artifact{}

// contractRule is one sentence the Layer interface promises.
type contractRule struct {
	// promise is the contract's own words, so a failure says which sentence was
	// broken rather than which assertion tripped.
	promise string
	check   func(t *testing.T, layer volstack.Layer)
}

// contractRules is the whole of what a Layer has to do, in the order the
// interface states it.
func contractRules() []contractRule {
	return []contractRule{{
		promise: `Name "identifies the layer in logs and in the stack record. It is stable across releases"`,
		check: func(t *testing.T, layer volstack.Layer) {
			if layer.Name() == "" {
				t.Error("Name is empty, so the stack record cannot name this layer to release it later")
			}
			if first, second := layer.Name(), layer.Name(); first != second {
				t.Errorf("Name changed between calls: %q then %q", first, second)
			}
		},
	}, {
		promise: `Observe "reports what of this layer is present ... without changing anything"`,
		check: func(t *testing.T, layer volstack.Layer) {
			first, _, err := layer.Observe(context.Background(), deadFoundation)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			second, _, err := layer.Observe(context.Background(), deadFoundation)
			if err != nil {
				t.Fatalf("Observe, second call: %v", err)
			}
			if first != second {
				t.Errorf("Observe answered %s then %s; a read that changes what it reads "+
					"makes a survey disagree with the walk that follows it", first, second)
			}
		},
	}, {
		promise: `Observe answers with a state when the layer below is gone, not an error`,
		check: func(t *testing.T, layer volstack.Layer) {
			if _, _, err := layer.Observe(context.Background(), deadFoundation); err != nil {
				t.Errorf("Observe with nothing below returned an error rather than a state: %v\n"+
					"a teardown reaching this layer stops here, and its volume is never released", err)
			}
		},
	}, {
		promise: `"The Artifact is the zero value at StateAbsent"`,
		check: func(t *testing.T, layer volstack.Layer) {
			state, own, err := layer.Observe(context.Background(), deadFoundation)
			if err != nil || state != volstack.StateAbsent {
				t.Skipf("this layer answers %s here, which this rule says nothing about", state)
			}
			if len(own.Devices) != 0 || own.Path != "" || own.Geometry.Known() {
				t.Errorf("an absent layer exposed %+v; the layer above would build on it", own)
			}
		},
	}, {
		promise: `Release "has to succeed when the layer below is already gone"`,
		check: func(t *testing.T, layer volstack.Layer) {
			if err := layer.Release(context.Background(), deadFoundation); err != nil {
				t.Errorf("Release with nothing below: %v", err)
			}
		},
	}, {
		promise: `Release is "safe to re-enter", because a teardown may resume`,
		check: func(t *testing.T, layer volstack.Layer) {
			if err := layer.Release(context.Background(), deadFoundation); err != nil {
				t.Fatalf("Release: %v", err)
			}
			if err := layer.Release(context.Background(), deadFoundation); err != nil {
				t.Errorf("Release again: %v; a teardown that resumes runs this twice", err)
			}
		},
	}, {
		promise: `Destroy removing what is already gone is the state the caller asked for`,
		check: func(t *testing.T, layer volstack.Layer) {
			if err := layer.Destroy(context.Background(), deadFoundation); err != nil {
				t.Errorf("Destroy with nothing below: %v", err)
			}
		},
	}, {
		promise: `Destroy is "safe to re-enter", because a deletion may resume after a crash`,
		check: func(t *testing.T, layer volstack.Layer) {
			if err := layer.Destroy(context.Background(), deadFoundation); err != nil {
				t.Fatalf("Destroy: %v", err)
			}
			if err := layer.Destroy(context.Background(), deadFoundation); err != nil {
				t.Errorf("Destroy again: %v; a deletion that resumes runs this twice", err)
			}
		},
	}, {
		promise: `Recorder.Params "returns no error because a layer returning what it ` +
			`was built with cannot fail at it"`,
		check: func(t *testing.T, layer volstack.Layer) {
			recorder, ok := layer.(volstack.Recorder)
			if !ok {
				t.Skip("this layer records no parameters, which the contract allows")
			}
			if first, second := recorder.Params(), recorder.Params(); !reflect.DeepEqual(first, second) {
				t.Errorf("Params answered %+v then %+v; a record replayed after a restart "+
					"would rebuild a different layer", first, second)
			}
		},
	}, {
		promise: `Healer.Healthy "is a read", so it answers the same way twice`,
		check: func(t *testing.T, layer volstack.Layer) {
			healer, ok := layer.(volstack.Healer)
			if !ok {
				t.Skip("this layer cannot be healed in place, which the contract allows")
			}
			first, err := healer.Healthy(context.Background(), deadFoundation)
			if err != nil {
				t.Skipf("Healthy needs more of a host than this fixture has: %v", err)
			}
			second, err := healer.Healthy(context.Background(), deadFoundation)
			if err != nil {
				t.Fatalf("Healthy, second call: %v", err)
			}
			if first != second {
				t.Errorf("Healthy answered %t then %t with nothing changed between", first, second)
			}
		},
	}, {
		promise: `NodeRequirements "is a declaration rather than an action"`,
		check: func(t *testing.T, layer volstack.Layer) {
			requirements, ok := layer.(volstack.NodeRequirements)
			if !ok {
				t.Skip("this layer constrains placement in no way, which the contract allows")
			}
			if first, second := requirements.NodeCapability(), requirements.NodeCapability(); first != second {
				t.Errorf("NodeCapability answered %q then %q; a volume would be placed "+
					"by one answer and staged by the other", first, second)
			}
		},
	}}
}

// TestLayerContract puts every shipped layer through every rule.
func TestLayerContract(t *testing.T) {
	for name := range shippedLayers() {
		t.Run(name, func(t *testing.T) {
			for _, rule := range contractRules() {
				t.Run(rule.promise, func(t *testing.T) {
					// Rebuilt per rule: a verb one rule ran must not decide what
					// the next rule sees.
					rule.check(t, shippedLayers()[name])
				})
			}
		})
	}
}

// TestEveryLayerInThisPackageIsUnderContract is what makes the above a contract
// rather than a list.
//
// It reads this package's own source for the constructors it exports and
// requires each one's layer to be in the fixture, so a layer added without a row
// there fails here instead of silently never being checked. That is the case the
// rules exist for: the layer nobody has written yet.
func TestEveryLayerInThisPackageIsUnderContract(t *testing.T) {
	covered := map[string]bool{}
	for _, layer := range shippedLayers() {
		covered[reflect.TypeOf(layer).Elem().Name()] = true
	}

	for _, constructed := range exportedLayerTypes(t) {
		if !covered[constructed] {
			t.Errorf("%s is constructed by this package and is in no contract fixture, "+
				"so nothing checks it against the Layer contract; add it to shippedLayers", constructed)
		}
	}
}

// exportedLayerTypes is the pointer type each exported New* constructor returns,
// read from the package's own files. Test files are excluded: a fixture's
// helpers are not layers this package ships.
func exportedLayerTypes(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}

	fset := token.NewFileSet()
	var types []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "New") {
				continue
			}
			if result, ok := pointerResultName(fn); ok {
				types = append(types, result)
			}
		}
	}
	if len(types) == 0 {
		t.Fatal("found no layer constructors at all, so this check proves nothing")
	}
	return types
}

// pointerResultName is the name of the single pointer type fn returns, and
// reports whether it returns one.
func pointerResultName(fn *ast.FuncDecl) (string, bool) {
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		return "", false
	}
	star, ok := fn.Type.Results.List[0].Type.(*ast.StarExpr)
	if !ok {
		return "", false
	}
	ident, ok := star.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return ident.Name, true
}
