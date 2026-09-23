package lvm

// vdoProvisioning is the VolumeProvisioning handler for a VDO-backed logical
// volume: one whose LogicalVolumeDefinition asks for client-side compression
// or deduplication, independently of each other. It is registered by this
// package's own init, rather than requiring a caller to import a separate
// subpackage for the side effect, because a forgotten import here is not a
// build failure: CreateLogicalVolume degenerates to a plain linear volume,
// silently dropping compression and deduplication rather than refusing.
//
// lvcreate --type vdo creates the VDO pool and the logical volume inside it in
// one call, which is why poolName (CreateLogicalVolume's <vg>/<pool> target)
// exists at all: no other registered type needs one.
type vdoProvisioning struct{}

func init() {
	RegisterVolumeProvisioning(&vdoProvisioning{})
}

func (v *vdoProvisioning) Name() string { return "vdo" }

// Handles reports whether def asks for compression or deduplication. Either
// one needs a VDO pool, since VDO is the only mechanism this package knows
// that provides either.
func (v *vdoProvisioning) Handles(def LogicalVolumeDefinition) bool {
	return def.Compression || def.Deduplication
}

// CreateVolumeArgs is lvcreate's VDO-specific arguments. Both --compression
// and --deduplication are always passed explicitly, never omitted, because
// the two are independent switches and lvcreate's own default for an unset
// one is not this package's to rely on.
//
// --config "activation{checks=0}" skips a check lvcreate's activation would
// otherwise run before this pool exists to be checked, which the design this
// handler implements verified live: without it, creation of the pool and the
// volume together in one lvcreate fails on the very object this call is in
// the middle of creating.
func (v *vdoProvisioning) CreateVolumeArgs(def LogicalVolumeDefinition) []string {
	return []string{
		"--type", "vdo",
		"--config", "activation{checks=0}",
		"--compression", vdoBoolFlag(def.Compression),
		"--deduplication", vdoBoolFlag(def.Deduplication),
	}
}

func vdoBoolFlag(b bool) string {
	if b {
		return "y"
	}
	return "n"
}
