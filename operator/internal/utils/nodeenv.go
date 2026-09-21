// The two init containers a storage-node pod runs before its API starts: one
// that selects this node's entry out of the cluster's per-node ConfigMap, and
// one that runs the configure with it.
//
// They are separate from the DaemonSet that carries them because what they get
// wrong, they get wrong quietly. A shell script that cannot find its input
// answers with an empty file unless it is told not to, and the container after
// it then fails describing a value rather than the input that was missing.

package utils

// Where the per-node entries are mounted, and where the selected one is written
// for the configure to source. The second is an emptyDir shared by the two init
// containers and the pod that follows them.
const (
	perNodeConfigDir = "/etc/per-node-config"
	nodeEnvFile      = "/etc/node-env/env.sh"
)

// configureInvocation is the tail every configure script ends with, after the
// per-node and fleet arguments have been assembled into ARGS.
const configureInvocation = `"
eval sudo -E python3 simplyblock_web/node_configure.py ${ARGS}
`

// nodeEnvScripts returns the configure script and the writer that feeds it.
//
// The configure script is returned without its closing quote: the caller appends
// the fleet-level arguments and configureInvocation, which is what keeps the
// per-node and fleet halves in one string without either being able to inject
// into the other.
func nodeEnvScripts() (configure, writer string) {
	return nodeConfigureScript(), nodeEnvWriterScript()
}

// nodeEnvWriterScript selects this node's entry.
//
// A missing entry is a failure rather than an empty file. The writer runs once
// per pod, and a pod that started before its ConfigMap volume was populated
// would otherwise keep an empty environment for the rest of its life: the
// configure after it crash-loops, and restarting a later init container never
// re-runs an earlier one. Exiting non-zero is what puts the retry on this
// container, where the next attempt reads a volume that has caught up.
//
// HOSTNAME is the node's name, injected from spec.nodeName, and the ConfigMap is
// keyed by it.
func nodeEnvWriterScript() string {
	return `set -e
mkdir -p /etc/node-env
if [ ! -f ` + perNodeConfigDir + `/${HOSTNAME} ]; then
  echo "no per-node configuration for ${HOSTNAME} in ` + perNodeConfigDir + `" >&2
  echo "the cluster's per-node ConfigMap has no entry for this node yet" >&2
  exit 1
fi
cp ` + perNodeConfigDir + `/${HOSTNAME} ` + nodeEnvFile + `
`
}

// nodeConfigureScript assembles the configure's arguments from that entry.
//
// An unset value is left out rather than sent as a default this side invented.
// --max-lvol=0 was such a default, and zero is refused because max-lvol must be
// a positive integer. A cluster that states no maxSubsystemCount has no opinion
// about it, and no opinion is the control plane's own default, not zero.
func nodeConfigureScript() string {
	return `set -e
[ -f ` + nodeEnvFile + ` ] && . ` + nodeEnvFile + `
ARGS=""
[ -n "${MAX_SUBSYS_COUNT}" ] && ARGS="${ARGS} --max-lvol=${MAX_SUBSYS_COUNT}"
[ -n "${PCI_ALLOWED}" ] && ARGS="${ARGS} --pci-allowed=\"${PCI_ALLOWED}\""
[ -n "${PCI_BLOCKED}" ] && ARGS="${ARGS} --pci-blocked=\"${PCI_BLOCKED}\""
[ -n "${NVME_DEVICES}" ] && ARGS="${ARGS} --nvme-devices=\"${NVME_DEVICES}\""
[ -n "${DEVICE_MODEL}" ] && ARGS="${ARGS} --device-model=\"${DEVICE_MODEL}\""
[ -n "${SIZE_RANGE}" ] && ARGS="${ARGS} --size-range=\"${SIZE_RANGE}\""
[ "${LBLK}" = "true" ] && ARGS="${ARGS} --lblk"
[ -n "${BLK_NAMES}" ] && ARGS="${ARGS} --blk-names=\"${BLK_NAMES}\""
[ -n "${BLK_NAMES_EXCLUDE}" ] && ARGS="${ARGS} --blk-names-exclude=\"${BLK_NAMES_EXCLUDE}\""
[ -n "${BLK_SERIALS}" ] && ARGS="${ARGS} --blk-serials=\"${BLK_SERIALS}\""
[ -n "${LBLK_JM_PERCENT}" ] && ARGS="${ARGS} --jm-percent=\"${LBLK_JM_PERCENT}\""
[ "${LBLK_FORCE_FORMAT}" = "true" ] && ARGS="${ARGS} --force-format"
ARGS="${ARGS}`
}
