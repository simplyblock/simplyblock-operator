"""A thin `sbctl` client — the control plane's own view of the cluster.

Kubernetes objects say what was *asked for*; sbctl says what the storage cluster actually
did. The two disagree often enough that mixing them up is its own class of bug: a
StorageClass can carry a `cluster_id` from an installation that no longer exists, and a
volume's placement drifts under the cluster's own rebalancer without any Kubernetes object
changing. So placement, subsystem grouping and the live cluster id all come from here.

Not a Component: it holds no lifecycle and produces no artifacts. Components construct one
and ask it questions.
"""

from __future__ import annotations

import json
from typing import Any

from . import kube


class SbctlError(RuntimeError):
    pass


class Sbctl:
    """Runs `sbctl ... --json` inside a webappapi pod.

    The pod is resolved once and cached. If it is replaced mid-run the next call re-resolves
    rather than failing the run: a restarted webappapi is normal, and losing placement
    resolution would turn every later verification into "skipped" for no good reason.
    """

    def __init__(self, namespace: str = "simplyblock", pod_match: str = "webappapi") -> None:
        self.namespace = namespace
        self.pod_match = pod_match
        self._pod = ""
        self._cluster_uuid = ""

    # ── plumbing ───────────────────────────────────────────────────────────────────

    def pod(self, refresh: bool = False) -> str:
        if self._pod and not refresh:
            return self._pod
        for p in kube.list_pods(self.namespace, [self.pod_match]):
            if p.phase == "Running":
                self._pod = p.name
                return p.name
        raise SbctlError(f"no running '*{self.pod_match}*' pod in namespace {self.namespace}")

    def _json(self, *argv: str, timeout: int = 60) -> Any:
        for attempt in (0, 1):
            pod = self.pod(refresh=bool(attempt))
            cp = kube.run(["-n", self.namespace, "exec", pod, "--", "sbctl", *argv, "--json"],
                          check=False, timeout=timeout)
            if cp.returncode == 0:
                break
            if attempt:
                raise SbctlError(f"sbctl {' '.join(argv)} failed: "
                                 f"{(cp.stderr or cp.stdout).strip()}")
        try:
            return json.loads(cp.stdout)
        except json.JSONDecodeError as e:
            raise SbctlError(f"sbctl {' '.join(argv)} returned unparseable output: {e}") from e

    # ── queries ────────────────────────────────────────────────────────────────────

    def cluster_uuid(self) -> str:
        """The live cluster's UUID.

        Authoritative after a reinstall, which is the whole reason it is read at all: a
        StorageClass cloned from the pool's own SC can still carry a dead cluster id, and
        every volume provisioned from it would target a cluster that no longer exists.
        """
        if self._cluster_uuid:
            return self._cluster_uuid
        clusters = self._json("cluster", "list")
        if not clusters:
            raise SbctlError("sbctl cluster list returned no clusters")
        active = [c for c in clusters if str(c.get("Status", "")).upper() == "ACTIVE"]
        chosen = active or clusters
        if len(chosen) != 1:
            desc = ", ".join(f"{c.get('Name')}={c.get('UUID')}({c.get('Status')})"
                             for c in chosen)
            raise SbctlError(f"expected exactly one active cluster, found "
                             f"{len(chosen)}: {desc}")
        uuid = str(chosen[0].get("UUID") or "")
        if not uuid:
            raise SbctlError("the active cluster has no UUID in sbctl output")
        self._cluster_uuid = uuid
        return uuid

    def volume_list(self) -> list[dict]:
        out = self._json("volume", "list")
        return out if isinstance(out, list) else []

    def volume_get(self, lvol: str) -> dict:
        out = self._json("volume", "get", lvol)
        return out if isinstance(out, dict) else {}

    def storage_node_list(self) -> list[dict]:
        out = self._json("storage-node", "list")
        return out if isinstance(out, list) else []

    def snapshot_list(self) -> list[dict]:
        out = self._json("snapshot", "list")
        return out if isinstance(out, list) else []

    # ── derived views ──────────────────────────────────────────────────────────────

    def host_map(self) -> tuple[dict[str, str], dict[str, str]]:
        """(sbctl hostname -> node uuid, node uuid -> management IP).

        The translation exists because a volume's `Hostname` is the storage node's own name
        (short host + RPC port, e.g. `vm04_4424`) while everything on the Kubernetes side
        speaks node UUIDs. The management IP is the transport address the node's subsystems
        listen on, which is the only way a sampled path can be attributed to a role.
        """
        hosts: dict[str, str] = {}
        ips: dict[str, str] = {}
        for n in self.storage_node_list():
            host, uuid = str(n.get("Hostname") or ""), str(n.get("UUID") or "")
            if host and uuid:
                hosts[host] = uuid
            if uuid and n.get("Management IP"):
                ips[uuid] = str(n["Management IP"])
        if not hosts:
            raise SbctlError("sbctl storage-node list reported no Hostname/UUID pairs")
        return hosts, ips

    def subsystem_of(self, lvol: str) -> tuple[str, int]:
        """(NQN, ns_id) of one volume. ('', 0) when unresolvable.

        Read per volume rather than derived from the StorageClass, because the control plane
        decides how it packs namespaced volumes into subsystems. What a migration has to move
        follows from the real grouping, not the requested one.
        """
        if not lvol:
            return "", 0
        try:
            data = self.volume_get(lvol)
        except SbctlError:
            return "", 0
        nqn = str(data.get("nqn") or data.get("NQN") or "")
        try:
            ns_id = int(data.get("ns_id") or data.get("NS ID") or 0)
        except (TypeError, ValueError):
            ns_id = 0
        return nqn, ns_id

    def nodes_of(self, lvols: dict[str, str]) -> dict[str, str]:
        """{key: node uuid} for `{key: lvol uuid}`, from a *single* volume listing.

        One listing for the whole set on purpose: the members of a shared subsystem are
        compared against each other, and per-volume listings taken seconds apart could
        straddle a move and make a consistent subsystem look split.
        """
        try:
            vols = self.volume_list()
            hosts, _ = self.host_map()
        except SbctlError:
            return {}
        by_lvol: dict[str, dict] = {}
        for v in vols:
            for k in (v.get("Id"), v.get("LVolUUID")):
                if k:
                    by_lvol[str(k)] = v

        out: dict[str, str] = {}
        missed: set[str] = set()
        for key, lvol in lvols.items():
            vol = by_lvol.get(lvol)
            if not vol:
                continue
            node = hosts.get(str(vol.get("Hostname") or ""))
            if node:
                out[key] = node
            else:
                missed.add(str(vol.get("Hostname") or ""))
        # An unknown hostname is not proof a volume is unplaceable — a node may have joined
        # since the map was built. Rebuild once before giving up, so a stale map cannot
        # quietly turn every placement check into "skipped".
        if missed:
            try:
                hosts, _ = self.host_map()
            except SbctlError:
                return out
            for key, lvol in lvols.items():
                if key in out:
                    continue
                vol = by_lvol.get(lvol)
                node = hosts.get(str(vol.get("Hostname") or "")) if vol else ""
                if node:
                    out[key] = node
        return out
