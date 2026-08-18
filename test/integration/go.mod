// Integration-test harness: a Talos/QEMU cluster, a fake control plane, and an
// nvmet fabric. Separate module so its Kubernetes and QEMU dependencies stay out
// of atlas-lib and out of the driver it exercises.
module github.com/simplyblock/simplyblock-operator/test/integration

go 1.26.2

require github.com/simplyblock/atlas v0.0.0

require (
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// The harness tests atlas's detection against a real kernel, so it must build
// against the tree it sits in, not a published version.
replace github.com/simplyblock/atlas => ../../atlas-lib
