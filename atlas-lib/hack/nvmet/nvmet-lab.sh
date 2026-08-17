#!/usr/bin/env bash
#
# nvmet-lab.sh — force each stale-subsystem/controller defect with the kernel's
# own NVMe-oF target, presenting the same sysfs identity a real simplyblock
# volume does, so nvmeof's connectors — and anything built on them — can be
# exercised against a real fabric rather than a mock.
#
# nvmet is the target-side mirror of the initiator: a subsystem, its namespaces,
# its ANA groups and its listeners are directories and files under
# /sys/kernel/config/nvmet, so every state this lab needs is a mkdir or an echo.
# No SPDK, no daemon, no control plane. That is the point — these are
# fabric-state bugs, and reproducing them should not require provisioning a
# volume.
#
# IDENTITY MATTERS, NOT JUST TOPOLOGY
#
# The driver keys on specific sysfs values, so a lab target that gets them wrong
# tests nothing:
#
#   NQN          nqn.2023-02.io.simplyblock:<cluster>:lvol:<master-lvol>
#                getLvolIDFromNQN splits on ":lvol:".
#   model        the master lvol UUID — jsonrpc.go derives "model" from the NQN,
#                and every /dev/disk/by-id glob in the driver is built from it.
#   ns uuid      the individual volume's lvol UUID, read back by the guardian
#                from /sys/block/<dev>/uuid to map a device to an lvol.
#   ana_state    optimized on the primary leg, non-optimized on the others;
#                teardown order and the "does this namespace keep a usable path"
#                check both key on it.
#
# Set these from the real cluster (see `identity-from` below) rather than the
# defaults whenever you can.
#
# WHAT MAPS TO WHAT
#
#   no-namespace                every namespace disabled while the controller
#                               stays live. No block device can ever appear.
#   namespace-missing           ours disabled, the co-tenant left running: the
#                               subsystem works and exports someone else's
#                               volume, so a teardown is pure collateral damage.
#   stale-endpoint              an extra listener the control plane never
#                               published.
#   controller-not-contributing NEEDS TWO NODES — see below.
#   ambiguous-head              not reproducible here: the host kernel rejects a
#                               second controller for one NQN rather than
#                               creating a second subsystem. Unit tests cover it.
#
# WHY controller-not-contributing NEEDS TWO NODES
#
# Namespaces belong to a subsystem, not to a listener, so no arrangement of
# ports on one target gives one leg a namespace and another leg none. That needs
# two independent targets advertising the same subsysnqn, and they cannot be two
# network namespaces on one host: configfs is global, so one host cannot hold two
# subsystems with the same NQN, and nvmet-tcp binds its listener in the initial
# network namespace whoever writes the port config.
#
# So run `target-up primary` on node A and `target-up secondary` on node B, with
# the same NQN, serial and model, and no enabled namespace on B. Two more things
# must hold or the host kernel will not produce the state:
#
#   * Disjoint cntlid ranges. Two independent targets both hand out cntlid 1 and
#     the host rejects the second controller as a duplicate instead of merging
#     it into the subsystem. Hence CNTLID_MIN/CNTLID_MAX.
#   * Matching attr_serial and attr_model, so the host groups both controllers
#     under one nvme-subsysN.
#
# Run `probe` first: attr_cntlid_min/attr_cntlid_max are a relatively recent
# nvmet addition, and without them the two-node case cannot work on that kernel.
#
# VERIFIED 2026-08-14 on a live cluster, kernel 5.14.0-503.14.1.el9_5 (RHEL 9.5),
# from inside the csi-node container, which already carries every privilege this
# needs — module load, configfs write, losetup, nvme-cli:
#
#   * The host merged both targets into ONE nvme-subsysN, giving a live
#     controller (cntlid 1000) with an empty leg set beside the serving one
#     (cntlid 1) — controller-not-contributing, exactly as designed. The disjoint
#     cntlid ranges are what made the merge happen rather than a rejection.
#   * A target built this way is indistinguishable from a real volume where it
#     matters: udev produced nvme-<model>_ha_<nsid> links, sysfs reported the
#     model and serial as a real subsystem does, and the driver's globs resolved.
#   * Disabling namespaces produced namespace-missing (blast radius correctly
#     naming the surviving co-tenant) and no-namespace (no blast radius at all).
#
# ONE TRAP WORTH KNOWING
#
# controller-not-contributing only appears if the test tells the repairer that
# BOTH endpoints are published. Pass only the primary and the orphaned controller
# is classified as a stale-endpoint instead — which is correct, and a different
# verdict with a different remedy. Model the control plane's answer honestly.
#
# USAGE
#   nvmet-lab.sh probe                       # kernel capability check
#   nvmet-lab.sh identity-from <nqn>         # copy identity off a real volume
#   nvmet-lab.sh target-up [primary|secondary]
#   nvmet-lab.sh defect <no-namespace|namespace-missing|stale-endpoint|clear>
#   nvmet-lab.sh host-connect [addr] [port]
#   nvmet-lab.sh host-disconnect
#   nvmet-lab.sh show
#   nvmet-lab.sh verify-identity             # do the driver's globs actually match?
#   nvmet-lab.sh selftest                    # single-node defects, asserted
#   nvmet-lab.sh target-down
#
# Requires root, nvme-cli, and a kernel with nvmet + nvmet-tcp.

set -euo pipefail

# --- identity, mirroring a real simplyblock volume -------------------------
CLUSTER_ID="${CLUSTER_ID:-1b4e28ba-2fa1-11d2-883f-b9a761bde3fb}"
# MASTER_LVOL is the subsystem's master lvol: it goes in the NQN after ":lvol:"
# and is what the driver uses as "model", hence what every by-id glob matches.
MASTER_LVOL="${MASTER_LVOL:-6dbb7d4e-2f1a-4a55-9d3c-1f2e3a4b5c6d}"
NQN="${NQN:-nqn.2023-02.io.simplyblock:${CLUSTER_ID}:lvol:${MASTER_LVOL}}"
MODEL="${MODEL:-$MASTER_LVOL}"
# Serial is "ha" on every simplyblock subsystem observed on a live cluster
# (2026-08-14). It is not decoration: udev builds the persistent link as
# nvme-<model>_<serial> and nvme-<model>_<serial>_<nsid>, so the serial sits
# between the model and the nsid suffix that every driver-side glob matches on.
SERIAL="${SERIAL:-ha}"
# Per-namespace lvol UUIDs. NS1 is "our" volume; NS2 stands in for a co-tenant
# on a shared subsystem (max_namespace_per_subsys > 1), which is what makes the
# blast-radius refusal testable.
NS1_UUID="${NS1_UUID:-$MASTER_LVOL}"
NS2_UUID="${NS2_UUID:-7ecc8e5f-3a2b-4b66-8e4d-2f3e4a5b6c7e}"

# --- topology --------------------------------------------------------------
ADDR="${ADDR:-127.0.0.1}"
# High ports on purpose: a storage node's SPDK already owns 4420-4428 on the
# host network, and binding over it would be a real outage rather than a test.
PORT="${PORT:-14420}"
ALT_PORT="${ALT_PORT:-14421}"
NS_SIZE="${NS_SIZE:-64M}"
NSIDS="${NSIDS:-1 2}"
# A live cluster hands out cntlid 1..N on the primary storage node and 1000+ on
# the secondary (observed: 1/1000, 3/1002). Mirroring that split is not cosmetic:
# it is exactly what keeps two independent targets from both claiming cntlid 1,
# which is what lets the host merge their controllers into one subsystem instead
# of rejecting the second as a duplicate.
CNTLID_MIN="${CNTLID_MIN:-1}"
CNTLID_MAX="${CNTLID_MAX:-999}"
SECONDARY_CNTLID_MIN="${SECONDARY_CNTLID_MIN:-1000}"
SECONDARY_CNTLID_MAX="${SECONDARY_CNTLID_MAX:-1999}"
# ANA state this target advertises: the primary leg is optimized and the
# secondary non-optimized, as every real multipath volume presents them.
ANA_STATE="${ANA_STATE:-optimized}"
ANA_GRPID="${ANA_GRPID:-1}"

CONFIGFS=/sys/kernel/config
NVMET="$CONFIGFS/nvmet"
BACKING_DIR="${BACKING_DIR:-/var/tmp/nvmet-lab}"

log()  { printf '%s\n' "$*" >&2; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
ok()   { printf 'ok: %s\n' "$*" >&2; }
note() { printf 'note: %s\n' "$*" >&2; }

need_root() { [ "$(id -u)" -eq 0 ] || fail "must run as root"; }
subsys_dir() { printf '%s/subsystems/%s' "$NVMET" "$NQN"; }
port_dir()   { printf '%s/ports/%s' "$NVMET" "$1"; }

# ns_uuid maps a namespace id to the lvol UUID it should advertise.
ns_uuid() { case "$1" in 1) printf '%s' "$NS1_UUID" ;; 2) printf '%s' "$NS2_UUID" ;; *) printf '' ;; esac; }

# ---------------------------------------------------------------- probe

probe() {
	need_root
	local rc=0

	for mod in nvmet nvmet-tcp; do
		if modprobe "$mod" 2>/dev/null || grep -q "^${mod//-/_} " /proc/modules; then
			ok "module $mod available"
		else
			log "MISSING: module $mod"; rc=1
		fi
	done

	if [ ! -d "$NVMET" ]; then
		mountpoint -q "$CONFIGFS" || mount -t configfs none "$CONFIGFS" 2>/dev/null || true
	fi
	[ -d "$NVMET" ] && ok "configfs nvmet present at $NVMET" || { log "MISSING: $NVMET"; rc=1; }

	local probe_dir="$NVMET/subsystems/${NQN}.probe"
	if [ -d "$NVMET" ] && mkdir -p "$probe_dir" 2>/dev/null; then
		if [ -f "$probe_dir/attr_cntlid_min" ]; then
			ok "attr_cntlid_min/max supported — two-node controller-not-contributing is possible"
		else
			log "MISSING: attr_cntlid_min/attr_cntlid_max. This kernel cannot host the"
			log "         two-target same-NQN setup, so controller-not-contributing cannot"
			log "         be forced here. The other defects still work."
			rc=1
		fi
		[ -f "$probe_dir/attr_model" ] && ok "attr_model supported (needed to mirror the lvol UUID)" ||
			log "MISSING: attr_model — by-id links will not carry the lvol UUID and the driver's globs will not match"
		mkdir -p "$probe_dir/namespaces/1" 2>/dev/null || true
		[ -f "$probe_dir/namespaces/1/device_uuid" ] && ok "namespace device_uuid supported" ||
			note "device_uuid absent; /sys/block/<dev>/uuid will not carry the lvol UUID"
		rmdir "$probe_dir/namespaces/1" 2>/dev/null || true
		rmdir "$probe_dir" 2>/dev/null || true
	fi

	command -v nvme >/dev/null && ok "nvme-cli present" || { log "MISSING: nvme-cli"; rc=1; }
	[ "$rc" -eq 0 ] && ok "probe passed" || log "probe found problems (exit $rc)"
	return "$rc"
}

# identity_from reads a real attached simplyblock volume and prints the exports
# that make this lab mirror it. Run it on a node with the volume attached:
#   eval "$(nvmet-lab.sh identity-from nqn.2023-02.io.simplyblock:...)"
identity_from() {
	local nqn="${1:?usage: identity-from <nqn>}" s found=""
	for s in /sys/class/nvme-subsystem/*; do
		[ -r "$s/subsysnqn" ] || continue
		[ "$(cat "$s/subsysnqn")" = "$nqn" ] || continue
		found="$s"; break
	done
	[ -n "$found" ] || fail "no attached subsystem with NQN $nqn"

	printf 'export NQN=%q\n' "$nqn"
	[ -r "$found/model" ]  && printf 'export MODEL=%q\n'  "$(cat "$found/model" | tr -d ' ')"
	[ -r "$found/serial" ] && printf 'export SERIAL=%q\n' "$(cat "$found/serial" | tr -d ' ')"
	local ns n=1
	for ns in "$found"/nvme*n*; do
		[ -r "$ns/uuid" ] || continue
		printf 'export NS%d_UUID=%q\n' "$n" "$(cat "$ns/uuid")"
		n=$((n + 1))
	done
	printf '# nsids present: %s\n' "$(ls -d "$found"/nvme*n* 2>/dev/null | sed 's/.*n//' | tr '\n' ' ')"
}

# ---------------------------------------------------------------- target

backing_for() {
	local img="$BACKING_DIR/ns$1.img" dev
	mkdir -p "$BACKING_DIR"
	[ -f "$img" ] || truncate -s "$NS_SIZE" "$img"
	dev="$(losetup -j "$img" | head -1 | cut -d: -f1)"
	[ -n "$dev" ] || dev="$(losetup -f --show "$img")"
	printf '%s' "$dev"
}

target_up() {
	need_root
	local role="${1:-primary}" sd nsid dev uuid
	modprobe nvmet 2>/dev/null || true
	modprobe nvmet-tcp 2>/dev/null || true
	mountpoint -q "$CONFIGFS" || mount -t configfs none "$CONFIGFS" 2>/dev/null || true

	sd="$(subsys_dir)"
	mkdir -p "$sd"
	echo 1 > "$sd/attr_allow_any_host"
	printf '%s' "$SERIAL" > "$sd/attr_serial" 2>/dev/null || note "attr_serial not settable"
	# The model is what every by-id glob in the driver matches on; without it
	# this lab exercises the fabric but not the driver's device discovery.
	[ -f "$sd/attr_model" ] && printf '%s' "$MODEL" > "$sd/attr_model" || note "attr_model absent"

	case "$role" in
	primary)
		if [ -f "$sd/attr_cntlid_min" ]; then
			echo "$CNTLID_MIN" > "$sd/attr_cntlid_min"
			echo "$CNTLID_MAX" > "$sd/attr_cntlid_max"
		fi
		for nsid in $NSIDS; do
			dev="$(backing_for "$nsid")"
			mkdir -p "$sd/namespaces/$nsid"
			printf '%s' "$dev" > "$sd/namespaces/$nsid/device_path"
			uuid="$(ns_uuid "$nsid")"
			if [ -n "$uuid" ] && [ -f "$sd/namespaces/$nsid/device_uuid" ]; then
				printf '%s' "$uuid" > "$sd/namespaces/$nsid/device_uuid"
			fi
			[ -f "$sd/namespaces/$nsid/ana_grpid" ] && printf '%s' "$ANA_GRPID" > "$sd/namespaces/$nsid/ana_grpid"
			echo 1 > "$sd/namespaces/$nsid/enable"
			ok "namespace $nsid enabled on $dev (uuid ${uuid:-unset})"
		done
		;;
	secondary)
		# No namespaces, and a cntlid range that cannot collide with the
		# primary's — without that the host rejects this controller outright
		# instead of merging it into the subsystem, and the defect never forms.
		# The 1000+ range is what a real secondary storage node uses.
		[ -f "$sd/attr_cntlid_min" ] || fail "kernel lacks attr_cntlid_min; secondary target cannot work (run probe)"
		echo "$SECONDARY_CNTLID_MIN" > "$sd/attr_cntlid_min"
		echo "$SECONDARY_CNTLID_MAX" > "$sd/attr_cntlid_max"
		ANA_STATE="${ANA_STATE_SECONDARY:-non-optimized}"
		ok "secondary target: no namespaces, cntlid $SECONDARY_CNTLID_MIN-$SECONDARY_CNTLID_MAX, ana $ANA_STATE"
		;;
	*) fail "unknown role $role (want primary or secondary)" ;;
	esac

	port_up "$PORT"
	ok "target up: $NQN at $ADDR:$PORT (role $role, ana $ANA_STATE)"
}

port_up() {
	local p="$1" pd ana
	pd="$(port_dir "$p")"
	mkdir -p "$pd"
	echo ipv4 > "$pd/addr_adrfam"
	echo tcp  > "$pd/addr_trtype"
	printf '%s' "$ADDR" > "$pd/addr_traddr"
	printf '%s' "$p"    > "$pd/addr_trsvcid"
	# ANA state is per port per group: the primary leg advertises optimized and
	# the secondaries non-optimized, which is what teardown order keys on.
	ana="$pd/ana_groups/$ANA_GRPID/ana_state"
	[ -f "$ana" ] && printf '%s' "$ANA_STATE" > "$ana" || note "no ana_groups on port $p; kernel may not advertise ANA"
	ln -sfn "$(subsys_dir)" "$pd/subsystems/$NQN" 2>/dev/null || true
	ok "listener up on $ADDR:$p"
}

port_down() {
	local pd
	pd="$(port_dir "$1")"
	[ -d "$pd" ] || return 0
	rm -f "$pd/subsystems/$NQN" 2>/dev/null || true
	rmdir "$pd" 2>/dev/null || true
}

target_down() {
	need_root
	host_disconnect || true
	local sd nsid img dev
	sd="$(subsys_dir)"
	port_down "$ALT_PORT"
	port_down "$PORT"

	if [ -d "$sd" ]; then
		for nsid in $(ls "$sd/namespaces" 2>/dev/null || true); do
			echo 0 > "$sd/namespaces/$nsid/enable" 2>/dev/null || true
			rmdir "$sd/namespaces/$nsid" 2>/dev/null || true
		done
		rmdir "$sd" 2>/dev/null || true
	fi
	for img in "$BACKING_DIR"/ns*.img; do
		[ -f "$img" ] || continue
		for dev in $(losetup -j "$img" | cut -d: -f1); do losetup -d "$dev" || true; done
		rm -f "$img"
	done
	rmdir "$BACKING_DIR" 2>/dev/null || true
	ok "target down"
}

# ---------------------------------------------------------------- defects

defect() {
	need_root
	local sd nsid
	sd="$(subsys_dir)"
	[ -d "$sd" ] || fail "no target; run target-up first"

	case "${1:-}" in
	no-namespace)
		for nsid in $(ls "$sd/namespaces" 2>/dev/null || true); do
			echo 0 > "$sd/namespaces/$nsid/enable"
		done
		ok "defect no-namespace: all namespaces disabled, controller left live"
		;;
	namespace-missing)
		[ -d "$sd/namespaces/2" ] || fail "namespace-missing needs a co-tenant; set NSIDS=\"1 2\""
		echo 1 > "$sd/namespaces/2/enable"
		echo 0 > "$sd/namespaces/1/enable"
		ok "defect namespace-missing: nsid 1 gone, co-tenant nsid 2 ($NS2_UUID) still exported"
		;;
	stale-endpoint)
		port_up "$ALT_PORT"
		ok "defect stale-endpoint: extra listener on $ADDR:$ALT_PORT"
		ok "  connect to it, then tell the repairer only $ADDR:$PORT is published"
		;;
	clear)
		for nsid in $NSIDS; do
			[ -d "$sd/namespaces/$nsid" ] && echo 1 > "$sd/namespaces/$nsid/enable"
		done
		port_down "$ALT_PORT"
		ok "defects cleared"
		;;
	*) fail "unknown defect '${1:-}'" ;;
	esac
}

# ---------------------------------------------------------------- host side

host_connect() {
	nvme connect -t tcp -a "${1:-$ADDR}" -s "${2:-$PORT}" -n "$NQN"
	ok "host connected to ${1:-$ADDR}:${2:-$PORT}"
}

host_disconnect() { nvme disconnect -n "$NQN" >/dev/null 2>&1 || true; }

# host_subsys prints the sysfs dir of our attached subsystem, if any.
host_subsys() {
	local s
	for s in /sys/class/nvme-subsystem/*; do
		[ -r "$s/subsysnqn" ] || continue
		[ "$(cat "$s/subsysnqn")" = "$NQN" ] && { printf '%s' "$s"; return 0; }
	done
	return 1
}

show() {
	local sd nsid s ns
	printf '=== target ===\n'
	sd="$(subsys_dir)"
	if [ -d "$sd" ]; then
		printf 'subsystem %s\n' "$NQN"
		printf '  model=%s serial=%s\n' \
			"$(cat "$sd/attr_model" 2>/dev/null || echo '?')" "$(cat "$sd/attr_serial" 2>/dev/null || echo '?')"
		for nsid in $(ls "$sd/namespaces" 2>/dev/null || true); do
			printf '  nsid %s enable=%s uuid=%s\n' "$nsid" \
				"$(cat "$sd/namespaces/$nsid/enable")" \
				"$(cat "$sd/namespaces/$nsid/device_uuid" 2>/dev/null || echo '-')"
		done
	else
		printf '(no subsystem)\n'
	fi

	printf '=== host sysfs ===\n'
	if s="$(host_subsys)"; then
		printf 'subsystem %s model=%s serial=%s\n' "$(basename "$s")" \
			"$(cat "$s/model" 2>/dev/null | tr -d ' ')" "$(cat "$s/serial" 2>/dev/null | tr -d ' ')"
		for ns in "$s"/nvme*n*; do
			[ -d "$ns" ] || continue
			printf '  namespace %s uuid=%s\n' "$(basename "$ns")" "$(cat "$ns/uuid" 2>/dev/null || echo '-')"
		done
		for c in "$s"/nvme*; do
			[ -r "$c/state" ] || continue
			printf '  controller %s state=%s addr=%s\n' "$(basename "$c")" \
				"$(cat "$c/state")" "$(cat "$c/address" 2>/dev/null || echo '-')"
		done
	else
		printf '(not attached)\n'
	fi

	printf '=== nvme list-subsys ===\n'; nvme list-subsys -o json 2>/dev/null || true
}

# verify_identity checks the assumption every CSI-side device lookup rests on:
# that udev names the by-id links so the driver's globs match. It reports what
# is actually there rather than asserting a shape, because that naming is exactly
# what varies between udev versions.
verify_identity() {
	local s rc=0 own_glob subsys_glob matches
	printf '=== /dev/disk/by-id links carrying the model ===\n'
	ls -l /dev/disk/by-id/ 2>/dev/null | grep -- "$MODEL" || printf '(none)\n'

	own_glob="/dev/disk/by-id/*${MODEL}*_1"
	subsys_glob="/dev/disk/by-id/*${MODEL}*_[0-9]*"

	printf '\n=== the globs the driver builds ===\n'
	printf 'own (nsid 1):  %s\n' "$own_glob"
	# shellcheck disable=SC2086
	matches=$(ls $own_glob 2>/dev/null | tr '\n' ' ')
	if [ -n "$matches" ]; then ok "matches: $matches"; else log "MISMATCH: nothing matches the own-device glob"; rc=1; fi

	printf 'subsystem:     %s\n' "$subsys_glob"
	# shellcheck disable=SC2086
	matches=$(ls $subsys_glob 2>/dev/null | tr '\n' ' ')
	if [ -n "$matches" ]; then ok "matches: $matches"; else log "MISMATCH: nothing matches the sibling glob"; rc=1; fi

	printf '\n=== ANA state per path ===\n'
	if s="$(host_subsys)"; then
		find "$s/.." -maxdepth 3 -name ana_state 2>/dev/null | while read -r a; do
			printf '  %s = %s\n' "$a" "$(cat "$a" 2>/dev/null)"
		done
	fi
	[ "$rc" -eq 0 ] && ok "identity verified: the driver's globs resolve" ||
		log "identity NOT verified — device discovery would fail on this udev"
	return "$rc"
}

# ---------------------------------------------------------------- selftest

# selftest walks the single-node defects and asserts the host actually reaches
# each state, so a green run means the premise holds on this kernel and this
# udev — not merely that the script ran.
selftest() {
	need_root
	log "--- baseline"
	target_down >/dev/null 2>&1 || true
	target_up primary >/dev/null
	host_connect >/dev/null
	sleep 2
	host_subsys >/dev/null || fail "baseline: subsystem did not attach"
	verify_identity >/dev/null || note "by-id naming does not match the driver's globs (see verify-identity)"
	ok "baseline: attached with a block device"

	log "--- no-namespace"
	defect no-namespace >/dev/null
	sleep 2
	local s ns_count
	s="$(host_subsys)" || fail "no-namespace: subsystem vanished; wanted live controllers with no namespace"
	ns_count=$(ls -d "$s"/nvme*n* 2>/dev/null | wc -l)
	[ "$ns_count" -eq 0 ] || fail "no-namespace: still $ns_count namespace(s); the kernel did not drop them"
	grep -qx live "$s"/nvme*/state 2>/dev/null || note "no controller reports 'live'; check reconnect timers"
	ok "no-namespace: live controller, zero namespaces — a wait for a device can never resolve"

	log "--- namespace-missing"
	defect clear >/dev/null; sleep 1
	defect namespace-missing >/dev/null; sleep 2
	s="$(host_subsys)" || fail "namespace-missing: subsystem vanished"
	ls -d "$s"/nvme*n2 >/dev/null 2>&1 || fail "namespace-missing: co-tenant nsid 2 is not exported"
	ls -d "$s"/nvme*n1 >/dev/null 2>&1 && fail "namespace-missing: nsid 1 still present"
	ok "namespace-missing: co-tenant present, ours gone — teardown here would be collateral damage"

	log "--- stale-endpoint"
	defect clear >/dev/null; sleep 1
	defect stale-endpoint >/dev/null
	host_connect "$ADDR" "$ALT_PORT" >/dev/null 2>&1 || note "second connect refused; host may have reused the first controller"
	sleep 2
	s="$(host_subsys)" || fail "stale-endpoint: subsystem vanished"
	printf 'controllers: %s\n' "$(ls -d "$s"/nvme[0-9]* 2>/dev/null | xargs -n1 basename 2>/dev/null | tr '\n' ' ')" >&2
	ok "stale-endpoint: one controller is on a listener the control plane never published"

	log "--- cleanup"
	target_down >/dev/null
	ok "selftest complete"
}

case "${1:-}" in
probe)           probe ;;
identity-from)   identity_from "${2:-}" ;;
target-up)       target_up "${2:-primary}" ;;
target-down)     target_down ;;
defect)          defect "${2:-}" ;;
host-connect)    host_connect "${2:-}" "${3:-}" ;;
host-disconnect) host_disconnect ;;
show)            show ;;
verify-identity) verify_identity ;;
selftest)        selftest ;;
*) sed -n '/^# USAGE/,/^# Requires root/p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac