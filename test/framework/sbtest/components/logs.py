"""Container-log collection: a live follower and a post-run grab.

Two components rather than one flag, because they are genuinely different mechanisms with
different failure modes, and the reason to have both is worth stating.

The kubelet keeps only `containerLogMaxSize x containerLogMaxFiles` per container — 10Mi x 5
on the clusters this runs against — and a busy SPDK container writes that in well under an
hour. A post-run grab of a four-hour run therefore returns its last forty minutes and
silently drops the rest, which is how several runs' worth of early evidence was lost.

So: `logs.stream` follows the high-volume logs for the whole run, and `logs.collect` grabs
everything else at the end. Enabling both is the normal configuration; the stream declares
which containers it owns so the grab skips them.
"""

from __future__ import annotations

import contextlib
import os
import subprocess
from typing import Any

from ..core import Component, RunContext, component
from . import kube

#: Where the host keeps CRI container logs. Mounted read-only into the grabber.
HOST_POD_LOGS = "/var/log/pods"

#: A busybox-ish image with sh, tail and gzip. The fio image already satisfies this and is
#: guaranteed to be pullable wherever these tests run.
DEFAULT_GRABBER_IMAGE = "quay.io/simplyblock-io/fio:latest"


def _log_dir(namespace: str, pod: str, container: str, if_missing: str) -> str:
    """Resolve the container's CRI log directory into $d.

    One directory per pod *UID*, so a pod recreated under the same name leaves two behind.
    Take the most recently written rather than letting the glob expand to several words —
    that would make every quoted use of $d a path that does not exist, which silently
    empties the grab for exactly the pods that have been restarted.
    """
    return (f'd=$(ls -1dt {HOST_POD_LOGS}/{namespace}_{pod}_*/{container}/ 2>/dev/null | head -1); '
            f'[ -n "$d" ] || {{ {if_missing}; }}; ')


#: The live file is "<restartCount>.log"; rotation renames it to "<n>.log.<ts>[.gz]" and
#: opens a new one under the same name. Pick it by name, not mtime: a .gz written by the
#: rotation that just happened is briefly the newest file in the directory.
_CURRENT = 'cur=$(ls -1 "$d" 2>/dev/null | grep -E "^[0-9]+\\.log$" | sort -n | tail -1); '

#: `gzip -cd`, not `zcat`: the latter is a .Z-only alias on some hosts, which would silently
#: drop every gzipped — i.e. every older — segment.
_CAT_ONE = 'case "$f" in *.gz) gzip -cd "$d$f" 2>/dev/null;; *) cat "$d$f" 2>/dev/null;; esac; '


def dump_script(namespace: str, pod: str, container: str) -> str:
    """Everything the host still retains for one container, oldest segment first."""
    return (_log_dir(namespace, pod, container, "exit 0")
            + 'for f in $(ls -1tr "$d" 2>/dev/null); do ' + _CAT_ONE + 'done')


def stream_script(namespace: str, pod: str, container: str, poll_s: int = 5) -> str:
    """Retained segments, then follow the live one for as long as this runs.

    The dump and the follow are one command so there is no gap between them: rotated
    segments are catted oldest-first and the live file is handed to `tail -F` from its first
    line instead of being catted.

    Three things move the log out from under a follower and all three are handled, because
    any one of them leaves the stream silent rather than failing:

    * **rotation** renames the live file and reopens the same name — `tail -F` follows by
      name and reopens it itself.
    * **a container restart** opens `<n+1>.log` beside the old one. `tail -F` would wait
      forever on a file that still exists and never grows, so the target is re-resolved on
      a timer and the old follower killed.
    * **a pod recreation** makes a whole new UID directory, which the same re-resolve picks
      up.

    Only the first target emits the rotated history; on a later switch the history is the
    file that was already being followed. A new target is read from its first line, so a
    restart costs latency, not data.
    """
    return (
        'prev=""; tpid=""; '
        'while :; do '
        + _log_dir(namespace, pod, container, f"sleep {poll_s}; continue")
        + _CURRENT +
        f'[ -n "$cur" ] || {{ sleep {poll_s}; continue; }}; '
        'if [ "$d$cur" != "$prev" ]; then '
        '  [ -n "$tpid" ] && kill "$tpid" 2>/dev/null; '
        '  if [ -z "$prev" ]; then '
        '    for f in $(ls -1tr "$d" 2>/dev/null); do '
        '      [ "$f" = "$cur" ] && continue; ' + _CAT_ONE +
        '    done; '
        '  fi; '
        '  tail -F -n +1 "$d$cur" 2>/dev/null & tpid=$!; '
        '  prev="$d$cur"; '
        'fi; '
        f'sleep {poll_s}; '
        'done')


class _GrabberBase(Component):
    """Shared management of the privileged pod that reads the host's /var/log/pods.

    A Component rather than a bare mixin: it uses `opt` and `name`, so inheriting states
    that dependency instead of leaving it to whatever it happens to be mixed into.
    """

    def _grabber_manifest(self, ctx: RunContext, node: str, name: str, ttl_s: int) -> str:
        import json as _json
        return _json.dumps({
            "apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": name, "namespace": self.opt("namespace"),
                         "labels": {"sbtest-run": ctx.run_id, "sbtest": "loggrab"}},
            "spec": {
                "nodeName": node, "restartPolicy": "Never",
                "tolerations": [{"operator": "Exists"}],
                "containers": [{
                    "name": "grab", "image": self.opt("image"),
                    "imagePullPolicy": "IfNotPresent",
                    "command": ["sh", "-c", f"sleep {ttl_s}"],
                    # privileged + runAsUser 0 is required to read the host's
                    # /var/log/pods under SELinux: without it the container runs as
                    # container_t and gets EACCES even as root, and every grab
                    # silently produces empty files.
                    "securityContext": {"privileged": True, "runAsUser": 0},
                    "volumeMounts": [{"name": "pods", "mountPath": HOST_POD_LOGS,
                                      "readOnly": True}],
                }],
                "volumes": [{"name": "pods", "hostPath": {"path": HOST_POD_LOGS}}],
            },
        })

    def _start_grabbers(self, ctx: RunContext, nodes: list[str], ttl_s: int) -> dict[str, str]:
        # The component name is in the pod name deliberately. Two components can both want a
        # grabber on the same node — the follower needs one for the whole run, the post-run
        # grab needs one at the end — and a Pod is immutable, so sharing a name means the
        # second one to apply fails on a field it is not allowed to change. Which it did:
        # logs.collect silently produced empty files for every node logs.stream had claimed.
        out: dict[str, str] = {}
        slug = self.name.replace(".", "-")
        for node in sorted(nodes):
            name = f"sbtest-{slug}-{kube.short(node)}-{ctx.run_id}"[:63]
            try:
                kube.run(["apply", "-f", "-"],
                         stdin=self._grabber_manifest(ctx, node, name, ttl_s))
            except kube.KubectlError as e:
                ctx.log.warn(f"{self.name}: cannot start grabber on {node}: {e}")
                continue
            out[node] = name
        if out:
            kube.run(["-n", self.opt("namespace"), "wait", "--for=condition=Ready",
                      *[f"pod/{n}" for n in out.values()], "--timeout=120s"],
                     timeout=140, check=False)
        ready = {}
        for node, name in out.items():
            cp = kube.run(["-n", self.opt("namespace"), "get", "pod", name,
                           "-o", "jsonpath={.status.phase}"], check=False, timeout=30)
            if cp.stdout.strip() == "Running":
                ready[node] = name
            else:
                ctx.log.warn(f"{self.name}: grabber on {node} is not Running; "
                             "logs from that node will be missing")
        return ready

    def _delete_grabbers(self, ctx: RunContext, names: list[str]) -> None:
        if not names:
            return
        kube.run(["-n", self.opt("namespace"), "delete", "pod", *names,
                  "--ignore-not-found", "--wait=false"], check=False)


@component
class LogStream(_GrabberBase):
    """Follow the high-volume container logs for the whole run.

    Enable this for anything that outruns kubelet rotation — in practice the SPDK containers
    and their proxies. Everything it follows is registered in
    `ctx.shared["logs.streamed"]` so `logs.collect` does not overwrite a full-run stream
    with the tail the kubelet happens to still have.
    """

    name = "logs.stream"
    summary = "follow chosen container logs live, surviving kubelet rotation and restarts"

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": "default",
            "image": DEFAULT_GRABBER_IMAGE,
            # Pods to follow, matched by substring, and the containers within them.
            "pods_matching": ["snode-spdk"],
            "containers": ["spdk-container", "spdk-proxy-container"],
            # Artifact name: "spdk-<port>" and "spdk-<port>-proxy" for the SPDK pods.
            "name_from": "snode-port",
            "ttl_s": 6 * 3600,
            "poll_s": 5,
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._grabbers: dict[str, str] = {}
        self._streams: list[dict] = []
        self._parts: dict[tuple, int] = {}

    # -- naming ---------------------------------------------------------------------
    @staticmethod
    def _snode_port(pod: str) -> str:
        parts = pod.split("-")  # snode-spdk-pod-<port>-<hash>
        return parts[3] if len(parts) > 3 and parts[3].isdigit() else pod

    def _artifact_name(self, pod: str, container: str) -> str:
        if self.opt("name_from") == "snode-port":
            suffix = "-proxy" if "proxy" in container else ""
            return f"spdk-{self._snode_port(pod)}{suffix}"
        return f"{pod}-{container}"

    # -- lifecycle ------------------------------------------------------------------
    def setup(self, ctx: RunContext) -> None:
        self._pods = kube.list_pods(self.opt("namespace"), self.opt("pods_matching"))
        if not self._pods:
            ctx.log.warn(f"{self.name}: no pods matching {self.opt('pods_matching')}; "
                         "nothing to follow")
            return
        nodes = {p.node for p in self._pods if p.node}
        self._grabbers = self._start_grabbers(ctx, sorted(nodes), int(self.opt("ttl_s")))
        # Published so logs.collect can reuse these instead of starting a second privileged
        # pod per node. Its TTL already covers the whole run, and by the time collection runs
        # the followers have stopped, so the pod is idle and free to be exec'd into again.
        ctx.shared.setdefault("logs.grabbers", {}).update(self._grabbers)

    def start(self, ctx: RunContext) -> None:
        streamed = ctx.shared.setdefault("logs.streamed", set())
        for p in getattr(self, "_pods", []):
            grab = self._grabbers.get(p.node)
            if not grab:
                continue
            for container in self.opt("containers"):
                if container not in p.containers:
                    continue
                self._attach(ctx, grab, p, container)
                streamed.add((p.name, container))
        if self._streams:
            ctx.log.info(f"{self.name}: following {len(self._streams)} container log(s) "
                         "for the whole run")

    def _attach(self, ctx: RunContext, grabber: str, pod: kube.Pod, container: str) -> None:
        base = self._artifact_name(pod.name, container)
        key = (pod.name, container)
        part = self._parts.get(key, 0)
        # A re-attach goes to a new file rather than appending: it re-reads whatever is
        # still retained, which would duplicate a stretch of the previous part in the
        # middle of the file. Separate parts stay internally ordered, which is what makes
        # them readable.
        name = f"{base}.txt" if part == 0 else f"{base}.part{part}.txt"
        path = ctx.path(name)
        try:
            fh = open(path, "ab")  # noqa: SIM115
            proc = subprocess.Popen(  # noqa: S603
                ["kubectl", "-n", self.opt("namespace"), "exec", grabber, "--", "sh", "-c",
                 stream_script(pod.namespace, pod.name, container, int(self.opt("poll_s")))],
                stdout=fh, stderr=subprocess.DEVNULL)
        except Exception as e:  # noqa: BLE001
            ctx.log.warn(f"{self.name}: cannot follow {pod.name}/{container}: {e}")
            return
        self._parts[key] = part + 1
        self._streams.append({"proc": proc, "fh": fh, "path": path, "grabber": grabber,
                              "pod": pod, "container": container})

    def tick(self, ctx: RunContext) -> None:
        """Re-attach followers that died. Cheap enough to run on every tick."""
        for st in list(self._streams):
            if st["proc"].poll() is None:
                continue
            self._streams.remove(st)
            with contextlib.suppress(Exception):
                st["fh"].close()
            size = os.path.getsize(st["path"]) if os.path.exists(st["path"]) else 0
            ctx.log.warn(f"{self.name}: follower for {st['pod'].name}/{st['container']} "
                         f"ended early (rc={st['proc'].returncode}, {size / 1048576:.1f} MiB); "
                         "re-attaching into a new part")
            ctx.timeline.record("logs.stream.reattach", subject=st["pod"].name,
                                container=st["container"])
            self._attach(ctx, st["grabber"], st["pod"], st["container"])

    def stop(self, ctx: RunContext) -> None:
        total = 0
        for st in self._streams:
            proc = st["proc"]
            try:
                proc.terminate()
                proc.wait(timeout=15)
            except Exception:  # noqa: BLE001
                with contextlib.suppress(Exception):
                    proc.kill()
            with contextlib.suppress(Exception):
                st["fh"].flush()
                st["fh"].close()
            if os.path.exists(st["path"]):
                total += os.path.getsize(st["path"])
        if self._streams:
            ctx.log.info(f"{self.name}: stopped {len(self._streams)} follower(s); "
                         f"{total / 1048576:.1f} MiB captured live")
        self._streams = []

    def teardown(self, ctx: RunContext) -> None:
        self.stop(ctx)  # idempotent; covers a run that failed before stop
        for node in self._grabbers:
            ctx.shared.get("logs.grabbers", {}).pop(node, None)
        self._delete_grabbers(ctx, sorted(self._grabbers.values()))
        self._grabbers = {}


@component
class LogCollect(_GrabberBase):
    """Grab container logs from the hosts at the end of the run.

    Correct for anything that fits inside what the kubelet retains, which is everything
    except the SPDK containers on a long run. Skips whatever `logs.stream` followed.
    """

    name = "logs.collect"
    summary = "grab container logs from each host's /var/log/pods after the run"

    def defaults(self) -> dict[str, Any]:
        return {
            "namespace": "default",
            "control_plane_namespace": "simplyblock",
            "image": DEFAULT_GRABBER_IMAGE,
            #: [{pods: [substr], containers: [name]|"all", name_from: ..., namespace: ...}]
            "targets": [
                {"pods": ["snode-spdk"], "containers": ["spdk-container", "spdk-proxy-container"],
                 "name_from": "snode-port"},
                {"pods": ["operator", "webappapi"], "containers": "all",
                 "namespace": "simplyblock", "name_from": "pod-key"},
                # One artifact per container, not per pod. The tasks pod runs seventeen
                # independent runners, so merging them produces a 50 MiB file that is not in
                # time order — which makes its time span meaningless and stops a pattern being
                # scoped to the one runner you care about. The container names are already
                # unique and descriptive, so they are the artifact names.
                {"pods": ["tasks"], "containers": "all",
                 "namespace": "simplyblock", "name_from": "container"},
                # The CSI driver is where the connects, the path reconcilers and
                # NodeStage/NodePublish actually happen, so it is the log that says what the
                # *host side* did and why. Kept per node rather than merged: the node plugin
                # reconciles per host, so "which node" is the first question about anything
                # it did, and a merged file loses it.
                {"pods": ["simplyblock-csi-node"], "containers": ["csi-node"],
                 "namespace": "simplyblock", "name_from": "pod-node", "name": "csi-node"},
                # The node-side agent the control plane talks to on each host. It is what
                # starts and probes the SPDK process, which puts it on the causal path of
                # every "the node went offline" event — including the liveness check that
                # concluded SPDK was dead because a Kubernetes API call blipped.
                {"pods": ["simplyblock-storage-node-ds"], "containers": "all",
                 "namespace": "default", "name_from": "pod-node", "name": "snode-api"},
                {"pods": ["simplyblock-csi-controller"], "containers": "all",
                 "namespace": "simplyblock", "name_from": "container"},
            ],
            "ttl_s": 1800,
        }

    def __init__(self, **options: Any) -> None:
        super().__init__(**options)
        self._grabbers: dict[str, str] = {}
        #: Only the ones this component created — the rest belong to whoever published them
        #: and are that component's to remove.
        self._own: dict[str, str] = {}

    def collect(self, ctx: RunContext) -> None:
        streamed = ctx.shared.get("logs.streamed", set())
        plan: list[tuple[kube.Pod, str, str]] = []
        for target in self.opt("targets"):
            ns = target.get("namespace", self.opt("namespace"))
            for p in kube.list_pods(ns, target["pods"]):
                wanted = (list(p.containers) if target.get("containers") == "all"
                          else [c for c in target["containers"] if c in p.containers])
                for c in wanted:
                    if (p.name, c) in streamed:
                        continue
                    plan.append((p, c, self._name_for(target, p, c)))
        if not plan:
            return

        nodes = {p.node for p, _c, _n in plan if p.node}
        self._grabbers = self._start_grabbers(ctx, sorted(nodes), int(self.opt("ttl_s")))

        grouped: dict[str, list[tuple[kube.Pod, str]]] = {}
        for p, c, artifact in plan:
            grouped.setdefault(artifact, []).append((p, c))

        for artifact, items in sorted(grouped.items()):
            path = ctx.path(f"{artifact}.txt")
            with open(path, "wb") as fh:
                for p, c in items:
                    grab = self._grabbers.get(p.node)
                    if not grab:
                        continue
                    if len(items) > 1:  # several containers share one artifact; header them
                        fh.write(f"==================== {p.name} / {c} "
                                 f"({kube.short(p.node)}) ====================\n".encode())
                    data = kube.run_bytes(
                        ["-n", self.opt("namespace"), "exec", grab, "--", "sh", "-c",
                         dump_script(p.namespace, p.name, c)])
                    fh.write(data)
                    if not data:
                        # The dump script swallows read errors, so an empty grab is
                        # otherwise indistinguishable from "this container logged nothing".
                        ctx.log.warn(f"{self.name}: empty grab for {p.name}/{c}")
            ctx.log.info(f"{self.name}: {artifact}.txt "
                         f"({os.path.getsize(path) / 1048576:.1f} MiB)")

    @staticmethod
    def _name_for(target: dict, pod: kube.Pod, container: str) -> str:
        mode = str(target.get("name_from", "pod-container"))
        if mode == "snode-port":
            parts = pod.name.split("-")
            port = parts[3] if len(parts) > 3 and parts[3].isdigit() else pod.name
            return f"spdk-{port}{'-proxy' if 'proxy' in container else ''}"
        if mode == "pod-key":
            for key in target["pods"]:
                if key in pod.name:
                    return str(key)
        if mode == "container":
            return container
        if mode == "pod-node":
            # "<name>-<node>": one artifact per node, which is how a DaemonSet's behaviour is
            # actually reasoned about. `name` overrides the matched substring, because the
            # generated pod names are long and carry nothing a reader wants.
            key = target.get("name") or next(
                (k for k in target["pods"] if k in pod.name), pod.name)
            return f"{key}-{kube.short(pod.node)}" if pod.node else str(key)
        return f"{pod.name}-{container}"

    def teardown(self, ctx: RunContext) -> None:
        # Only what this component created. Deleting a grabber it merely borrowed would pull
        # the pod out from under the component that owns it.
        self._delete_grabbers(ctx, sorted(self._own.values()))
        self._grabbers = {}
        self._own = {}


@component
class Dmesg(Component):
    """Kernel ring buffer from each storage worker.

    Rotation-limited in a different way from a container log: the ring buffer is a fixed
    size, so a node that is logging heavily keeps only minutes of it — while a quiet one
    keeps hours, which is the real hazard. A buffer spanning ten hours contains the previous
    runs' damage, and counting that against this run is how a green build inherits its
    predecessor's mess.

    That is why the timestamp format matters here more than it looks. `dmesg -T` renders
    *local* time with no offset, so an event cannot be placed against a run window recorded
    in UTC without assuming the two agree. `--time-format=iso` emits an offset, which makes
    the comparison sound; it is preferred, with `-T` kept as a fallback for older util-linux.
    """

    name = "host.dmesg"
    summary = "dmesg from each storage worker, ISO-timestamped so events can be placed in time"

    def defaults(self) -> dict[str, Any]:
        return {"namespace": "default", "pods_matching": ["snode-spdk"],
                "container": "spdk-container"}

    def collect(self, ctx: RunContext) -> None:
        for p in kube.list_pods(self.opt("namespace"), self.opt("pods_matching")):
            if not p.node:
                continue
            data = kube.run_bytes(
                ["-n", p.namespace, "exec", p.name, "-c", self.opt("container"), "--",
                 "sh", "-c", "dmesg --time-format=iso 2>/dev/null || dmesg -T"],
                timeout=120)
            if not data:
                ctx.log.warn(f"{self.name}: empty dmesg from {kube.short(p.node)}")
            with open(ctx.path(f"dmesg-{kube.short(p.node)}.txt"), "wb") as fh:
                fh.write(data)
