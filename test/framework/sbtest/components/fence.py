"""Cutting one pNFS client off from the metadata server to prove that nfsd's fence holds.

A pNFS client writes straight to the shared NVMe-oF namespace with a layout it got from the
metadata server. When the server cannot recall that layout, it fences the client by
preempting the client's reservation key, and from then on the reservation is the only thing
keeping that client off the namespace. `chaos.fence` stages exactly that, once per run, at a
seeded time inside the timed run:

1. It picks a pNFS claim, a victim node, and a recaller node. The victim is preferably a
   node no workload pod mounts a volume on, because the partition takes every pNFS mount
   on that node with it.
2. A probe writer on the victim writes 4 KiB with O_DIRECT through one long-open file
   descriptor, every `write_interval_s`, and logs each write's time and result.
3. Once those writes go direct (a layout, and no NFS WRITE for them), a privileged
   hostNetwork helper on the victim drops TCP port 2049 to and from the metadata server's
   addresses. The NVMe-oF paths stay up.
4. A probe on the recaller truncates the writer's file to a larger size, which makes nfsd
   recall the victim's layout and, once the recall fails, fence the victim. The truncate
   is bounded by `truncate_timeout_s`.
5. The writer keeps writing for `after_s`, then the partition heals, the probes stop, and
   the writer's log is collected.

Everything goes to `fence.json` for the `pnfs.fence` detector. Times come from the nodes'
clocks, not the harness's, because they are compared with the writer's own timestamps.

The partition is the one thing a failed run could leave behind, so it is removed three
ways: teardown deletes exactly the recorded rules through the helper; the helper deletes
them itself when it is terminated; and it deletes them on its own after `max_partition_s`.
A rule teardown cannot remove is logged as an error with the command that removes it.
"""

from __future__ import annotations

import json
import random
import threading
import time
from datetime import UTC, datetime
from typing import Any

from ..core import Component, FenceWrite, RunContext, component
from . import kube
from .nfs import mount_ops

MOUNT = "/data"
NFS_PORT = "2049"
_MDS_PREFIX = "simplyblock-pnfs-mds-"
_CSI_NODE_PREFIX = "simplyblock-csi-node-"


def fence_start(*, seed: int, t0: float, duration: float, min_delay_s: float,
                quiet_tail_s: float, budget_s: float) -> float | None:
    """When the scenario starts, on the scale of t0, or None when the run is too short to
    fit it between min_delay_s and quiet_tail_s."""
    lo = t0 + min_delay_s
    hi = t0 + duration - quiet_tail_s - budget_s
    if hi < lo:
        return None
    return lo + random.Random(seed).random() * (hi - lo)


def pick(rng: random.Random, *, nodes: list[str], client_nodes: list[str],
         claims: list[str], allow_busy_victim: bool) -> tuple[str, str, str] | None:
    """(victim, recaller, claim), or None when there is nothing to fence."""
    nodes = sorted(set(nodes))
    if len(nodes) < 2 or not claims:
        return None
    busy = set(client_nodes)
    victims = [n for n in nodes if n not in busy] or (nodes if allow_busy_victim else [])
    if not victims:
        return None
    victim = rng.choice(victims)
    recaller = rng.choice([n for n in nodes if n != victim])
    return victim, recaller, rng.choice(sorted(claims))


def partition_rules(tag: str, addresses: list[str]) -> list[list[str]]:
    """iptables rule specifications, without the -I or -D, that drop NFS to and from each
    address. Outgoing packets are matched after the ClusterIP is translated, and replies
    before it is translated back, so a Service needs both its ClusterIP and its backends
    listed."""
    out: list[list[str]] = []
    for a in dict.fromkeys(addresses):
        tail = ["-p", "tcp"]
        out.append(["OUTPUT", "-d", a, *tail, "--dport", NFS_PORT,
                    "-m", "comment", "--comment", tag, "-j", "DROP"])
        out.append(["INPUT", "-s", a, *tail, "--sport", NFS_PORT,
                    "-m", "comment", "--comment", tag, "-j", "DROP"])
    return out


def is_direct(before: dict[str, int] | None, after: dict[str, int] | None,
              ok_writes: int) -> bool:
    """Whether the probe's writes between two mountstats readings went to the block device:
    the mount holds a layout, writes succeeded, and the mount sent no NFS WRITE."""
    if before is None or after is None or ok_writes <= 0:
        return False
    return after.get("LAYOUTGET", 0) > 0 and after.get("WRITE", 0) == before.get("WRITE", 0)


def parse_writer_log(text: str) -> list[FenceWrite]:
    """The probe writer's log: `<epoch ms> ok` or `<epoch ms> fail <exit> <first error
    line>` per write. Lines that are neither are skipped."""
    out: list[FenceWrite] = []
    for line in text.splitlines():
        parts = line.split(maxsplit=2)
        if len(parts) < 2 or not parts[0].isdigit() or parts[1] not in ("ok", "fail"):
            continue
        ts = datetime.fromtimestamp(int(parts[0]) / 1000, UTC)
        out.append(FenceWrite(ts=ts, ok=parts[1] == "ok",
                              detail=parts[2].strip() if len(parts) > 2 else ""))
    return out


def _ms(text: str) -> datetime | None:
    text = text.strip()
    return datetime.fromtimestamp(int(text) / 1000, UTC) if text.isdigit() else None


def _iso(t: datetime | None) -> str | None:
    return t.isoformat().replace("+00:00", "Z") if t else None


#: The probe writer. coreutils for O_DIRECT on stdout and millisecond dates. The file stays
#: open on fd 3 for the whole run: an open() needs the metadata server, so reopening it per
#: write would fail every write during the partition for a reason that is not the fence.
_WRITER = """
apk add --no-cache coreutils >/dev/null 2>&1 || {{ echo "apk add coreutils failed" >&2; exit 1; }}
F={file}
dd if=/dev/zero of="$F" bs=1M count=4 oflag=direct conv=fsync 2>/dev/null || exit 1
exec 3<>"$F"
touch /logs/ready
i=0
until test -f /logs/stop; do
  off=$((i % 1024)); i=$((i + 1))
  timeout -k 5 30 dd if=/dev/urandom bs=4k count=1 seek=$off oflag=direct conv=notrunc \\
    2>/tmp/err >&3
  rc=$?
  t=$(date +%s%3N)
  if test $rc -eq 0; then echo "$t ok"; else echo "$t fail $rc $(grep -m1 -v records /tmp/err)"; fi \\
    >>/logs/writes.log
  sleep {interval}
done
touch /logs/stopped
while :; do sleep 5; done
"""

#: The recaller only waits; the truncate is run in it by exec.
_RECALLER = """
apk add --no-cache coreutils >/dev/null 2>&1 || { echo "apk add coreutils failed" >&2; exit 1; }
touch /tmp/ready
while :; do sleep 5; done
"""

#: The partition helper. It removes the rules listed in /tmp/rules when it is terminated
#: and once /tmp/deadline has passed, so a harness that dies mid-run cannot strand them.
_NET = """
apk add --no-cache iptables coreutils >/dev/null 2>&1 || { echo "apk add failed" >&2; exit 1; }
heal() {
  test -f /tmp/rules || return 0
  while read -r rule; do while iptables -D $rule 2>/dev/null; do :; done; done </tmp/rules
  rm -f /tmp/rules /tmp/deadline
  echo "partition removed"
}
trap 'heal; exit 0' TERM INT
touch /tmp/ready
while :; do
  if test -f /tmp/deadline && test "$(date +%s)" -ge "$(cat /tmp/deadline)"; then
    echo "partition deadline passed"; heal
  fi
  sleep 1 & wait $!
done
"""


@component
class Fencer(Component):
    """Cut one pNFS client off from the metadata server and make nfsd fence it."""

    name = "chaos.fence"
    summary = "partition a pNFS client from the MDS, force a layout recall, and record its writes"
    namespace_options = {"namespace": "test", "mds_namespace": "operator"}  # noqa: RUF012

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": None,          # the probes; the test namespace when unset
            "mds_namespace": None,      # the MDS and csi-node pods; the operator's when unset
            "seed": None,               # random, and logged, when unset
            "min_delay_s": 120.0,       # after the timed run starts
            "quiet_tail_s": 120.0,      # before it ends
            "setup_s": 240.0,           # probe pods up and writing directly
            "settle_s": 10.0,           # partitioned before the truncate
            "truncate_timeout_s": 240.0,
            "after_s": 60.0,            # the victim keeps writing after the truncate
            "heal_tail_s": 20.0,        # writes recorded after the heal
            "max_partition_s": 900.0,   # the helper heals by itself after this
            "allow_busy_victim": False,
            "grow_mb": 64,              # the truncate's new size
            "write_interval_s": 0.5,
            "image": "alpine:3.20",
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._lock = threading.Lock()
        self._abort = threading.Event()
        self._thread: threading.Thread | None = None
        self._seed = 0
        self._record: dict[str, Any] | None = None
        #: Rule specifications currently on the victim, and the pods that may still exist.
        self._rules: list[list[str]] = []
        self._net_pod = ""
        self._pods: list[str] = []

    def _budget(self) -> float:
        return sum(float(self.opt(k)) for k in
                   ("setup_s", "settle_s", "truncate_timeout_s", "after_s", "heal_tail_s"))

    def start(self, ctx: RunContext) -> None:
        seed = self.opt("seed")
        self._seed = int(seed) if seed is not None else random.SystemRandom().randrange(2**31)
        duration = float(ctx.shared.get("run.duration_s") or 0)
        t0 = time.monotonic()
        at = fence_start(seed=self._seed, t0=t0, duration=duration,
                         min_delay_s=float(self.opt("min_delay_s")),
                         quiet_tail_s=float(self.opt("quiet_tail_s")),
                         budget_s=self._budget())
        if at is None:
            ctx.log.warn(f"{self.name}: a {duration:.0f}s run has no room for the "
                         f"{self._budget():.0f}s scenario, so nothing is fenced")
            self._record = {"seed": self._seed, "victim_node": "", "recaller_node": "",
                            "claim": "", "error": f"a {duration:.0f}s run is too short"}
            return
        ctx.log.info(f"{self.name}: seed {self._seed}, fencing at {at - t0:.0f}s")

        def run() -> None:
            if self._abort.wait(max(0.0, at - time.monotonic())):
                return
            try:
                self._scenario(ctx)
            except Exception as e:  # noqa: BLE001
                ctx.log.error(f"{self.name}: {e}")
                with self._lock:
                    if self._record is not None:
                        self._record["error"] = self._record.get("error") or str(e)
            finally:
                self._end(ctx)

        self._thread = threading.Thread(target=run, name="chaos-fence", daemon=True)
        self._thread.start()

    # ── the scenario ────────────────────────────────────────────────────────────────

    def _scenario(self, ctx: RunContext) -> None:
        ns, rec = str(self.opt("namespace")), self._new_record(ctx)
        if rec is None:
            return
        victim, recaller, claim = rec["victim_node"], rec["recaller_node"], rec["claim"]
        full = self._full_names()
        self._create_pods(ctx, full[victim], full[recaller], claim)
        writer = rec["victim_pod"]
        if not self._wait_ready(ns, writer, "test -f /logs/ready") \
                or not self._wait_ready(ns, rec["recaller_pod"], "test -f /tmp/ready") \
                or not self._wait_ready(ns, self._net_pod, "test -f /tmp/ready"):
            raise RuntimeError("the probe pods did not start; see the pods' logs")
        if not self._wait_direct(ns, writer):
            raise RuntimeError(f"the probe writer on {victim} never wrote directly")
        addresses = self._mds_addresses(ns, writer)
        if not addresses:
            raise RuntimeError("no metadata server address to partition")
        self._partition(ctx, rec, addresses)
        if self._abort.wait(float(self.opt("settle_s"))):
            return
        self._truncate(ctx, rec)
        self._abort.wait(float(self.opt("after_s")))

    def _new_record(self, ctx: RunContext) -> dict[str, Any] | None:
        rng = random.Random(self._seed)
        nodes = sorted(self._full_names())
        got = pick(rng, nodes=nodes, client_nodes=list(ctx.shared.get("pnfs.client_nodes") or []),
                   claims=list(ctx.shared.get("pnfs.claims") or []),
                   allow_busy_victim=bool(self.opt("allow_busy_victim")))
        rec: dict[str, Any] = {"seed": self._seed, "victim_node": "", "recaller_node": "",
                               "claim": "", "truncate_timeout_s": self.opt("truncate_timeout_s")}
        with self._lock:
            self._record = rec
        if got is None:
            rec["error"] = (f"nothing to fence: {len(nodes)} node(s) with a node plugin, "
                            f"{len(ctx.shared.get('pnfs.claims') or [])} pNFS claim(s), and "
                            "no idle victim unless allow_busy_victim is set")
            ctx.log.warn(f"{self.name}: {rec['error']}")
            return None
        victim, recaller, claim = got
        rec.update(victim_node=victim, recaller_node=recaller, claim=claim,
                   victim_pod=f"{ctx.run_id}-fence-writer",
                   recaller_pod=f"{ctx.run_id}-fence-recaller",
                   file=f"{MOUNT}/{ctx.run_id}-fence.probe")
        ctx.log.info(f"{self.name}: victim {victim}, recaller {recaller}, claim {claim}")
        return rec

    def _full_names(self) -> dict[str, str]:
        """Short to full name of every node running a node plugin, which is every node a
        pNFS claim can be mounted on."""
        pods = kube.list_pods(str(self.opt("mds_namespace")), [_CSI_NODE_PREFIX])
        return {kube.short(p.node): p.node for p in pods
                if p.name.startswith(_CSI_NODE_PREFIX) and p.phase == "Running" and p.node}

    def _create_pods(self, ctx: RunContext, victim: str, recaller: str, claim: str) -> None:
        rec = self._record or {}
        self._net_pod = f"{ctx.run_id}-fence-net"
        self._pods = [rec["victim_pod"], rec["recaller_pod"], self._net_pod]
        labels = {"sbtest": ctx.run_id, "sbtest-run": "true", "app": "sbtest-fence"}
        image = str(self.opt("image"))
        writer = _WRITER.format(file=rec["file"], interval=self.opt("write_interval_s"))

        def pod(name: str, node: str, script: str, data: bool, **spec: Any) -> dict:
            mounts = [{"name": "logs", "mountPath": "/logs"}]
            volumes: list[dict] = [{"name": "logs", "emptyDir": {}}]
            if data:
                mounts.append({"name": "data", "mountPath": MOUNT})
                volumes.append({"name": "data", "persistentVolumeClaim": {"claimName": claim}})
            container = {"name": "probe", "image": image, "imagePullPolicy": "IfNotPresent",
                         "command": ["sh", "-c", script], "volumeMounts": mounts}
            container.update(spec.pop("container", {}))
            return {"apiVersion": "v1", "kind": "Pod",
                    "metadata": {"name": name, "labels": labels},
                    "spec": {"nodeName": node, "restartPolicy": "Never",
                             "terminationGracePeriodSeconds": 10,
                             "containers": [container], "volumes": volumes, **spec}}

        docs = [pod(rec["victim_pod"], victim, writer, True),
                pod(rec["recaller_pod"], recaller, _RECALLER, True),
                pod(self._net_pod, victim, _NET, False, hostNetwork=True,
                    container={"securityContext": {"privileged": True}})]
        kube.apply_each(str(self.opt("namespace")), docs)

    def _wait_ready(self, ns: str, pod: str, test: str) -> bool:
        deadline = time.monotonic() + float(self.opt("setup_s"))
        while time.monotonic() < deadline and not self._abort.is_set():
            cp = kube.run(["-n", ns, "exec", pod, "--", "sh", "-c", test], check=False)
            if cp.returncode == 0:
                return True
            self._abort.wait(3)
        return False

    def _wait_direct(self, ns: str, writer: str) -> bool:
        deadline = time.monotonic() + float(self.opt("setup_s"))
        while time.monotonic() < deadline and not self._abort.is_set():
            before = mount_ops(kube.exec_sh(ns, writer, "cat /proc/self/mountstats"), MOUNT)
            oks = self._ok_writes(ns, writer)
            self._abort.wait(10)
            after = mount_ops(kube.exec_sh(ns, writer, "cat /proc/self/mountstats"), MOUNT)
            if is_direct(before, after, self._ok_writes(ns, writer) - oks):
                return True
        return False

    @staticmethod
    def _ok_writes(ns: str, writer: str) -> int:
        out = kube.exec_sh(ns, writer, "grep -c ' ok$' /logs/writes.log 2>/dev/null || true")
        return int(out.strip() or 0) if out.strip().isdigit() else 0

    def _mds_addresses(self, ns: str, writer: str) -> list[str]:
        """The address the victim mounted the claim from, the backends of the Service that
        address belongs to, and every metadata server pod's IP."""
        out: list[str] = []
        for line in kube.exec_sh(ns, writer, "cat /proc/mounts").splitlines():
            fields = line.split()
            if len(fields) > 3 and fields[1] == MOUNT:
                out += [o[5:] for o in fields[3].split(",") if o.startswith("addr=")]
        cp = kube.run(["get", "svc", "-A", "-o", "json"], check=False)
        for svc in json.loads(cp.stdout or "{}").get("items", []):
            if svc.get("spec", {}).get("clusterIP") not in out:
                continue
            meta = svc.get("metadata", {})
            sl = kube.run(["-n", meta.get("namespace", ""), "get", "endpointslices", "-l",
                           f"kubernetes.io/service-name={meta.get('name', '')}", "-o", "json"],
                          check=False)
            for s in json.loads(sl.stdout or "{}").get("items", []):
                for ep in s.get("endpoints") or []:
                    out += list(ep.get("addresses") or [])
        cp = kube.run(["-n", str(self.opt("mds_namespace")), "get", "pods", "-o", "json"],
                      check=False)
        for p in json.loads(cp.stdout or "{}").get("items", []):
            ip = p.get("status", {}).get("podIP")
            if p.get("metadata", {}).get("name", "").startswith(_MDS_PREFIX) and ip:
                out.append(ip)
        return list(dict.fromkeys(out))

    def _partition(self, ctx: RunContext, rec: dict[str, Any], addresses: list[str]) -> None:
        ns = str(self.opt("namespace"))
        rules = partition_rules(f"sbtest-fence-{ctx.run_id}", addresses)
        listed = "\n".join(" ".join(r) for r in rules)
        deadline = f"$(( $(date +%s) + {int(float(self.opt('max_partition_s')))} ))"
        kube.run(["-n", ns, "exec", self._net_pod, "--", "sh", "-c",
                  f"printf '%s\\n' '{listed}' >/tmp/rules && echo {deadline} >/tmp/deadline"])
        rec["rules"] = [" ".join(r) for r in rules]
        for r in rules:
            with self._lock:
                self._rules.append(r)
            cp = kube.run(["-n", ns, "exec", self._net_pod, "--", "iptables", "-I", *r],
                          check=False)
            if cp.returncode != 0:
                raise RuntimeError(f"iptables -I {' '.join(r)}: {cp.stderr.strip()}")
        rec["partitioned"] = _iso(_ms(kube.exec_sh(ns, self._net_pod, "date +%s%3N")))
        ctx.log.event(f"{self.name}: {rec['victim_node']} cut off from the metadata server "
                      f"({', '.join(addresses)})")

    def _truncate(self, ctx: RunContext, rec: dict[str, Any]) -> None:
        bound = int(float(self.opt("truncate_timeout_s")))
        size = int(self.opt("grow_mb")) * 1024 * 1024
        script = (f"s=$(date +%s%3N); timeout -k 5 {bound} truncate -s {size} {rec['file']} "
                  "2>/tmp/terr; rc=$?; e=$(date +%s%3N); echo \"$s $e $rc $(head -n1 /tmp/terr)\"")
        ctx.log.event(f"{self.name}: truncating {rec['file']} from {rec['recaller_node']}")
        try:
            out = kube.run(["-n", str(self.opt("namespace")), "exec", rec["recaller_pod"], "--",
                            "sh", "-c", script], timeout=bound + 60, check=False).stdout
        except Exception as e:  # noqa: BLE001
            out = ""
            rec["truncate_error"] = f"exec did not return: {e}"
        parts = out.split(maxsplit=3)
        if len(parts) >= 3:
            rec["truncate_issued"] = _iso(_ms(parts[0]))
            rc = int(parts[2]) if parts[2].isdigit() else -1
            # 124 and 137 are timeout's: the truncate did not return within its bound.
            if rc not in (124, 137):
                rec["truncate_returned"] = _iso(_ms(parts[1]))
            if rc != 0:
                rec["truncate_error"] = f"exit {rc}" + (f": {parts[3]}" if len(parts) > 3 else "")
        ctx.log.event(f"{self.name}: truncate returned "
                      f"{rec.get('truncate_returned') or 'never'}"
                      + (f" ({rec['truncate_error']})" if rec.get("truncate_error") else ""))

    def _end(self, ctx: RunContext) -> None:
        """Heal, stop the writer, and read its log. Runs however the scenario ended."""
        ns = str(self.opt("namespace"))
        rec = self._record
        if rec is None or not self._pods:
            return
        if self._rules:
            self._remove_rules(ctx)
            if self._rules:
                # Still partitioned: a heal recorded now would have the detector judge
                # the writes that follow as after it.
                rec["heal_error"] = (f"{len(self._rules)} partition rule(s) could not be "
                                     "removed, so the victim may still be cut off")
                ctx.log.error(f"{self.name}: {rec['heal_error']}")
            else:
                healed = _ms(kube.exec_sh(ns, self._net_pod, "date +%s%3N"))
                rec["healed"] = _iso(healed or datetime.now(UTC))
                ctx.log.event(f"{self.name}: partition healed")
                self._abort.wait(float(self.opt("heal_tail_s")))
        writer = rec.get("victim_pod", "")
        kube.exec_sh(ns, writer, "touch /logs/stop")
        for _ in range(20):
            if kube.run(["-n", ns, "exec", writer, "--", "test", "-f", "/logs/stopped"],
                        check=False).returncode == 0:
                break
            time.sleep(2)
        text = kube.exec_sh(ns, writer, "cat /logs/writes.log 2>/dev/null")
        with open(ctx.path("fence-writes.txt"), "w") as fh:
            fh.write(text)
        rec["writes"] = [{"ts": _iso(w.ts), "ok": w.ok, "detail": w.detail}
                         for w in parse_writer_log(text)]
        self._delete_pods(ctx)

    # ── cleanup ─────────────────────────────────────────────────────────────────────

    def _remove_rules(self, ctx: RunContext) -> None:
        """Delete every recorded rule through the helper. A rule that is already gone
        counts as removed; one that cannot be removed stays recorded and is logged with the
        command a human runs on the node to remove it."""
        with self._lock:
            rules = list(self._rules)
        for r in rules:
            cp = kube.run(["-n", str(self.opt("namespace")), "exec", self._net_pod, "--",
                           "iptables", "-D", *r], check=False)
            gone = ("does a matching rule exist" in cp.stderr
                    or "No such file or directory" in cp.stderr)
            if cp.returncode == 0 or gone:
                with self._lock:
                    self._rules.remove(r)
                continue
            ctx.log.error(f"{self.name}: cannot remove a partition rule on "
                          f"{(self._record or {}).get('victim_node') or 'the victim node'}; remove it by hand "
                          f"with: iptables -D {' '.join(r)} ({cp.stderr.strip()})")

    def _delete_pods(self, ctx: RunContext) -> None:
        if not self._pods:
            return
        cp = kube.run(["-n", str(self.opt("namespace")), "delete", "pod", *self._pods,
                       "--ignore-not-found", "--wait=false"], check=False)
        if cp.returncode != 0:
            ctx.log.warn(f"{self.name}: deleting the probe pods failed: {cp.stderr.strip()}")
            return
        self._pods = []

    def stop(self, ctx: RunContext) -> None:
        # A scenario still running is cut short, but its _end still heals and reads the log.
        self._abort.set()
        if self._thread:
            self._thread.join(timeout=float(self.opt("truncate_timeout_s")) + 180)
            self._thread = None

    def collect(self, ctx: RunContext) -> None:
        with self._lock:
            rec = dict(self._record) if self._record is not None else None
        if rec is not None:
            ctx.save_json("fence.json", rec)

    def teardown(self, ctx: RunContext) -> None:
        if self._rules:
            self._remove_rules(ctx)
        self._delete_pods(ctx)
