// Integration-test harness: a Talos/QEMU cluster, a fake control plane, and an
// nvmet fabric. Separate module so its Kubernetes and QEMU dependencies stay out
// of atlas-lib and out of the driver it exercises.
module github.com/simplyblock/simplyblock-operator/test/integration

go 1.26.2
