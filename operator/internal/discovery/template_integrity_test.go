// What the draft says about the three integrity settings, from what the devices
// it names will be once a storage node has taken them.
//
// The metadata journal follows from one property of each device: whether it
// completes a 4K write whole. Checksum validation is the reviewer's choice, and
// once it is made, atomicity follows from the same property on the devices
// that have neither a metadata format nor a 4K block size. A device whose
// formats the probe could not read is one nothing is known about, and the
// draft treats it as the case that needs the protection.

package discovery

import (
	"strings"
	"testing"

	"github.com/simplyblock/atlas/ptr"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
	"github.com/simplyblock/simplyblock-operator/internal/nodeprobe"
)

// lbaf is one LBA format of a namespace.
func lbaf(index int, dataSize uint32, metadataSize uint16) nodeprobe.LBAFormat {
	return nodeprobe.LBAFormat{Index: index, DataSize: dataSize, MetadataSize: metadataSize, RelativePerformance: "best"}
}

// geometry is how one NVMe device reads: its block size, the atomic write
// units the kernel published for it (zero for none), and its namespace's
// formats with the one in use. No formats is a controller nobody could ask.
type geometry struct {
	blockSize            uint32
	atomicMin, atomicMax uint32
	formats              []nodeprobe.LBAFormat
	current              int
	powerFail            *nodeprobe.PowerFailAtomicity
}

// integrityFleet is one worker per geometry, each handing over one NVMe device.
func integrityFleet(geometries ...geometry) Plan {
	plan := Plan{Class: ClassNVMe}
	for index, g := range geometries {
		address := "0000:5e:0" + string(rune('0'+index)) + ".0"
		device := disk("nvme0n1", address, 0, 3_840_000_000_000)
		device.LogicalBlockSize = g.blockSize
		if g.atomicMax != 0 {
			device.AtomicWriteUnitMinBytes, device.AtomicWriteUnitMaxBytes = ptr.To(g.atomicMin), ptr.To(g.atomicMax)
		}
		r := report("worker-"+string(rune('1'+index)), device)
		controller := nodeprobe.Controller{Address: address, Driver: "nvme"}
		if g.formats != nil {
			controller.LBAFormats = []nodeprobe.NamespaceFormats{{
				NSID: 1, Formats: g.formats, Current: ptr.To(g.current), PowerFailAtomicity: g.powerFail,
			}}
		}
		r.NVMeControllers = []nodeprobe.Controller{controller}
		plan.Workers = append(plan.Workers, Worker{
			Name: r.Node, Report: r, Class: ClassNVMe, Devices: []nodeprobe.Device{device},
		})
	}
	return plan
}

func templateFor(t *testing.T, plan Plan, seed *simplyblockv1alpha2.ClusterTemplate) ClusterTemplate {
	t.Helper()
	template, err := ClusterTemplateFor("a-cluster", plan, TemplateOptions{Seed: seed})
	if err != nil {
		t.Fatalf("the fleet was refused: %v", err)
	}
	return template
}

func noteAbout(template ClusterTemplate, field string) string {
	for _, note := range template.Notes {
		if strings.HasPrefix(note, field+" ") {
			return note
		}
	}
	return ""
}

var (
	// A 512-byte namespace that also offers 4K, the common datacenter drive.
	offers4K = []nodeprobe.LBAFormat{lbaf(0, 512, 0), lbaf(1, 4096, 0)}
	// A namespace that offers nothing but 512 bytes.
	only512 = []nodeprobe.LBAFormat{lbaf(0, 512, 0)}
	// A namespace with a 4K format that carries 8 bytes of metadata.
	with4KMetadata = []nodeprobe.LBAFormat{lbaf(0, 512, 0), lbaf(1, 4096, 0), lbaf(2, 4096, 8)}
)

func TestTheMetadataJournalFollowsWhetherEveryDeviceWritesFourKiBWhole(t *testing.T) {
	format := func(on bool) *simplyblockv1alpha2.ClusterTemplate {
		return &simplyblockv1alpha2.ClusterTemplate{EnableDriveFormat: ptr.To(on)}
	}
	for _, tc := range []struct {
		name    string
		fleet   Plan
		seed    *simplyblockv1alpha2.ClusterTemplate
		journal bool
	}{
		{"a 512-byte drive that is reformatted to 4K",
			integrityFleet(geometry{blockSize: 512, formats: offers4K, current: 0}), nil, false},
		{"a drive already at 4K",
			integrityFleet(geometry{blockSize: 4096, formats: offers4K, current: 1}), format(false), false},
		{"a 512-byte drive left as it is",
			integrityFleet(geometry{blockSize: 512, formats: offers4K, current: 0}), format(false), true},
		{"a 512-only drive that writes 4K whole",
			integrityFleet(geometry{blockSize: 512, atomicMin: 512, atomicMax: 4096, formats: only512}), nil, false},
		{"a 512-only drive that writes only 512 bytes whole",
			integrityFleet(geometry{blockSize: 512, atomicMin: 512, atomicMax: 512, formats: only512}), nil, true},
		{"a 512-only drive on a kernel that cannot say",
			integrityFleet(geometry{blockSize: 512, formats: only512}), nil, true},
		{"a drive whose controller nobody could ask",
			integrityFleet(geometry{}), nil, true},
		{"one drive of two that needs it",
			integrityFleet(
				geometry{blockSize: 512, formats: offers4K, current: 0},
				geometry{blockSize: 512, atomicMin: 512, atomicMax: 512, formats: only512}), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := templateFor(t, tc.fleet, tc.seed)

			got := template.Template.EnableMetadataJournal
			if got == nil || *got != tc.journal {
				t.Fatalf("enableMetadataJournal = %v, want %v", got, tc.journal)
			}
			if noteAbout(template, "enableMetadataJournal") == "" {
				t.Errorf("no note accounts for enableMetadataJournal: %q", template.Notes)
			}
		})
	}
}

func TestTheNoteNamesTheDevicesThatNeedTheJournal(t *testing.T) {
	template := templateFor(t, integrityFleet(
		geometry{blockSize: 512, formats: offers4K, current: 0},
		geometry{blockSize: 512, atomicMin: 512, atomicMax: 512, formats: only512}), nil)

	note := noteAbout(template, "enableMetadataJournal")
	if !strings.Contains(note, "worker-2") || strings.Contains(note, "worker-1") {
		t.Errorf("the note is %q, want it to name worker-2's device and not worker-1's", note)
	}
}

// A stated journal is kept even where the devices disagree, and the note says
// which devices it leaves exposed.
func TestAStatedJournalIsKeptAndChecked(t *testing.T) {
	template := templateFor(t,
		integrityFleet(geometry{blockSize: 512, atomicMin: 512, atomicMax: 512, formats: only512}),
		&simplyblockv1alpha2.ClusterTemplate{EnableMetadataJournal: ptr.To(false)})

	if got := template.Template.EnableMetadataJournal; got == nil || *got {
		t.Fatalf("enableMetadataJournal = %v, want the stated false", got)
	}
	note := noteAbout(template, "enableMetadataJournal")
	if !strings.Contains(note, "stated") || !strings.Contains(note, "worker-1") {
		t.Errorf("the note is %q, want it to say the value was stated and name the exposed device", note)
	}
}

func TestAtomicityFollowsTheDevicesChecksumValidationCannotCheckOtherwise(t *testing.T) {
	checksums := func() *simplyblockv1alpha2.ClusterTemplate {
		return &simplyblockv1alpha2.ClusterTemplate{
			EnableChecksumValidation: ptr.To(true), EnableDriveFormat: ptr.To(false),
		}
	}
	for _, tc := range []struct {
		name      string
		fleet     Plan
		atomicity *bool
	}{
		{"every device checks with metadata",
			integrityFleet(geometry{blockSize: 4096, formats: with4KMetadata, current: 2}), nil},
		{"every device has 4K blocks",
			integrityFleet(geometry{blockSize: 4096, formats: offers4K, current: 1}), nil},
		{"a 512-byte device that writes 4K whole",
			integrityFleet(geometry{blockSize: 512, atomicMin: 512, atomicMax: 4096, formats: only512}), ptr.To(true)},
		{"a 512-byte device on a kernel that cannot say",
			integrityFleet(geometry{blockSize: 512, formats: only512}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := templateFor(t, tc.fleet, checksums())

			got := template.Template.EnableAtomicity4K
			if (got == nil) != (tc.atomicity == nil) || (got != nil && *got != *tc.atomicity) {
				t.Fatalf("enableAtomicity4K = %v, want %v", got, tc.atomicity)
			}
		})
	}
}

// A device the stated checksum validation cannot check is a node add that
// fails, so the draft is refused with the devices named rather than written.
func TestChecksumValidationOverADeviceItCannotCheckIsRefused(t *testing.T) {
	_, err := ClusterTemplateFor("a-cluster",
		integrityFleet(geometry{blockSize: 512, atomicMin: 512, atomicMax: 512, formats: only512}),
		TemplateOptions{Seed: &simplyblockv1alpha2.ClusterTemplate{EnableChecksumValidation: ptr.To(true)}})

	if err == nil || !strings.Contains(err.Error(), "worker-1") {
		t.Fatalf("the draft was not refused over worker-1's device: %v", err)
	}
}

// Unstated, checksum validation stays the reviewer's to turn on, and the draft
// says what turning it on would need.
func TestWithoutChecksumValidationTheDraftSaysWhatItWouldNeed(t *testing.T) {
	template := templateFor(t,
		integrityFleet(geometry{blockSize: 512, atomicMin: 512, atomicMax: 4096, formats: only512}), nil)

	if template.Template.EnableChecksumValidation != nil || template.Template.EnableAtomicity4K != nil {
		t.Fatalf("the draft states checksum validation %v and atomicity %v, want neither",
			template.Template.EnableChecksumValidation, template.Template.EnableAtomicity4K)
	}
	note := noteAbout(template, "enableChecksumValidation")
	if !strings.Contains(note, "enableAtomicity4K") {
		t.Errorf("the note is %q, want it to say checksum validation would need enableAtomicity4K", note)
	}
}

// The drive states its power-fail atomicity in the Identify data the probe
// reads on every kernel, so a 512-byte drive is judged without the 6.11
// attributes, and the drive is believed over the kernel where both answer:
// the kernel ignores the controller-wide AWUPF.
func TestTheDrivesOwnAtomicityDecidesTheJournal(t *testing.T) {
	controllerOnly := func(blocks uint32) *nodeprobe.PowerFailAtomicity {
		return &nodeprobe.PowerFailAtomicity{ControllerBlocks: blocks}
	}
	for _, tc := range []struct {
		name    string
		g       geometry
		journal bool
	}{
		{"a controller atomic to eight blocks, on an old kernel",
			geometry{blockSize: 512, formats: only512, powerFail: controllerOnly(8)}, false},
		{"a controller atomic to one block, on an old kernel",
			geometry{blockSize: 512, formats: only512, powerFail: controllerOnly(1)}, true},
		{"a namespace atomic to eight blocks under a controller atomic to one",
			geometry{blockSize: 512, formats: only512, powerFail: &nodeprobe.PowerFailAtomicity{
				NamespaceSpecific: true, NamespaceBlocks: 8, ControllerBlocks: 1}}, false},
		{"AWUPF the kernel ignored",
			geometry{blockSize: 512, atomicMin: 512, atomicMax: 512, formats: only512, powerFail: controllerOnly(8)}, false},
		// atlas-lib/nvme/testdata/identify_*_samsung_pm983_512.bin, decoded.
		{"a Samsung PM983 left at 512 bytes, as read on dev-ocp",
			geometry{blockSize: 512, atomicMin: 512, atomicMax: 4096, formats: offers4K, current: 0,
				powerFail: &nodeprobe.PowerFailAtomicity{
					NamespaceSpecific: true, NamespaceBlocks: 8, ControllerBlocks: 8, BoundaryBlocks: 8}}, false},
		{"boundaries that split an aligned 4K write",
			geometry{blockSize: 512, formats: only512, powerFail: &nodeprobe.PowerFailAtomicity{
				NamespaceSpecific: true, NamespaceBlocks: 16, BoundaryBlocks: 6}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := templateFor(t, integrityFleet(tc.g), nil)

			if got := template.Template.EnableMetadataJournal; got == nil || *got != tc.journal {
				t.Fatalf("enableMetadataJournal = %v, want %v", got, tc.journal)
			}
		})
	}
}

// A drive that states it writes only one block whole is a definite no, so
// stated checksum validation over it is refused rather than left unknown.
func TestADriveThatSaysOneBlockCannotBeChecked(t *testing.T) {
	_, err := ClusterTemplateFor("a-cluster",
		integrityFleet(geometry{blockSize: 512, formats: only512,
			powerFail: &nodeprobe.PowerFailAtomicity{ControllerBlocks: 1}}),
		TemplateOptions{Seed: &simplyblockv1alpha2.ClusterTemplate{EnableChecksumValidation: ptr.To(true)}})

	if err == nil || !strings.Contains(err.Error(), "worker-1") {
		t.Fatalf("the draft was not refused over worker-1 device: %v", err)
	}
}
