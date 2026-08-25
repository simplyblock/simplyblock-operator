#!/usr/bin/env python3
#
# Audits every place this repository grants a Kubernetes permission, and reports
# the ones that can be used to escalate.
#
# There are four such places and most tooling sees only the first: the kubebuilder
# markers that generate the manager's own ClusterRole, the roles the operator
# builds in Go and applies to what it deploys, the chart's hand-written roles and
# bindings, and the security contexts of the workloads themselves. The second is
# the one nobody audits, and it is where this repository grants cluster-wide
# `pods/exec`.
#
# A finding is not automatically a defect. A CSI node plugin needs a privileged
# container and host paths, and an operator that deploys a component needs to
# create that component's RBAC. What the checker asks is that every escalation
# primitive be deliberate, so anything reviewed and accepted carries a
# justification next to it and stops being reported:
#
#   // rbac-justified: the storage node reads its own pod to find its NQN
#   // +kubebuilder:rbac:groups="",resources=pods,verbs=get
#
#   # rbac-justified: NVMe device access needs the host /dev tree
#   privileged: true
#
# Usage:
#   check-rbac.py                 audit everything
#   check-rbac.py --changed       only sources this change touched
#   check-rbac.py --source markers|go|chart|workload
#   check-rbac.py --severity high report critical and high only
#   check-rbac.py --unjustified   only findings without a justification
#
# Exit status is 1 when an unjustified critical or high finding was reported.

import argparse
import re
import subprocess
import sys
from pathlib import Path

MARKER_DIRS = ("operator/internal", "operator/cmd")
GO_ROLE_DIRS = ("operator/internal/utils",)
GENERATED_ROLE = "operator/config/rbac"
CHART_DIRS = (
    "helm-charts/charts/simplyblock-operator/templates",
    "csi-driver/charts",
)

JUSTIFIED_RE = re.compile(r"rbac-justified:\s*(.+?)\s*$")

MARKER_RE = re.compile(
    r"\+kubebuilder:rbac:groups=(?P<groups>[^,]*),resources=(?P<resources>[^,]*)"
    r"(?:,resourceNames=(?P<names>[^,]*))?,verbs=(?P<verbs>\S+)"
)

# The escalation primitives, in the order they are reported. Each is
# (id, severity, resource matcher, verb matcher, what it buys an attacker).
WRITE_VERBS = {"create", "update", "patch", "delete", "deletecollection", "*"}
READ_VERBS = {"get", "list", "watch", "*"}

PRIMITIVES = (
    ("wildcard-verb", "critical", None, {"*"},
     "every verb on the resource, including ones added by a future Kubernetes release"),
    ("escalate-verb", "critical", None, {"escalate"},
     "granting itself any permission, bypassing RBAC's escalation prevention"),
    ("bind-verb", "critical", None, {"bind"},
     "binding any role, including cluster-admin, to any subject"),
    ("impersonate", "critical", {"users", "groups", "serviceaccounts"}, {"impersonate"},
     "acting as any user, including system:masters"),
    ("pods-exec", "critical", {"pods/exec", "pods/attach", "pods/ephemeralcontainers"},
     {"create", "get", "*"},
     "running code in any existing pod, inheriting its service account"),
    ("pod-create", "high", {"pods"}, {"create"},
     "scheduling a privileged pod that mounts the host filesystem"),
    ("secret-read", "high", {"secrets"}, {"get", "list", "watch", "*"},
     "reading every service-account token in scope, and so acting as any of them"),
    ("token-mint", "critical", {"serviceaccounts/token"}, {"create"},
     "minting a token for any service account"),
    ("rbac-write", "high", {"roles", "clusterroles", "rolebindings", "clusterrolebindings"},
     WRITE_VERBS,
     "writing RBAC. Kubernetes caps this at the granter's own permissions unless "
     "escalate or bind is also held, so the ceiling is this role's own breadth"),
    ("webhook-write", "high",
     {"validatingwebhookconfigurations", "mutatingwebhookconfigurations"}, WRITE_VERBS,
     "disabling or redirecting admission control, which bypasses every policy"),
    ("node-write", "medium", {"nodes", "nodes/proxy"}, WRITE_VERBS,
     "relabeling or cordoning nodes, which redirects where workloads land"),
    ("workload-create", "medium", {"deployments", "daemonsets", "statefulsets", "jobs",
                                   "cronjobs", "replicasets"}, {"create", "*"},
     "creating a workload that runs with its namespace's service account"),
    ("sa-write", "medium", {"serviceaccounts"}, WRITE_VERBS,
     "creating identities, which combined with a binding is a new subject"),
)

# Workload privileges, with what each is for when it is legitimate.
WORKLOAD_CHECKS = (
    ("privileged", "high", re.compile(r"^\s*privileged:\s*true"),
     "full host capability set, so a container escape is a host compromise"),
    ("host-network", "medium", re.compile(r"^\s*hostNetwork:\s*true"),
     "the host network namespace, so the pod reaches anything the node reaches"),
    ("host-pid", "high", re.compile(r"^\s*hostPID:\s*true"),
     "the host process table, so other processes can be inspected and signaled"),
    ("host-ipc", "medium", re.compile(r"^\s*hostIPC:\s*true"),
     "the host IPC namespace"),
    ("host-path", "medium", re.compile(r"^\s*hostPath:"),
     "a host directory. A writable mount of /, /etc, or /var/lib/kubelet is a host takeover"),
    ("allow-escalation", "medium", re.compile(r"^\s*allowPrivilegeEscalation:\s*true"),
     "setuid escalation inside the container"),
    ("run-as-root", "low", re.compile(r"^\s*runAsUser:\s*0\b"),
     "root inside the container"),
    ("cap-add-all", "high", re.compile(r"^\s*-\s*(ALL|SYS_ADMIN)\s*$"),
     "a capability close to full host privilege"),
)

SEVERITY_ORDER = {"critical": 0, "high": 1, "medium": 2, "low": 3}


class Finding:
    def __init__(self, primitive, severity, path, line, detail, justification, source):
        self.primitive = primitive
        self.severity = severity
        self.path = path
        self.line = line
        self.detail = detail
        self.justification = justification
        self.source = source

    @property
    def key(self):
        return (SEVERITY_ORDER[self.severity], str(self.path), self.line)


def justification_for(lines, index, window=4):
    """A `rbac-justified:` note on or just above the granting line."""
    for offset in range(0, window + 1):
        position = index - offset
        if position < 0:
            break
        match = JUSTIFIED_RE.search(lines[position])
        if match:
            return match.group(1)
    return None


def classify(resources, verbs):
    """Every primitive a (resources, verbs) pair matches."""
    hits = []
    resources = {r.strip().lower() for r in resources if r.strip()}
    verbs = {v.strip().lower() for v in verbs if v.strip()}
    for name, severity, wanted_resources, wanted_verbs, what in PRIMITIVES:
        if wanted_resources is not None and not (resources & wanted_resources):
            continue
        if not (verbs & wanted_verbs):
            continue
        hits.append((name, severity, what, sorted(resources & wanted_resources)
                     if wanted_resources else sorted(resources)))
    return hits


def scan_markers(repo, files):
    """The kubebuilder markers that generate the manager's ClusterRole."""
    findings = []
    for path in files:
        lines = path.read_text().splitlines()
        for index, raw in enumerate(lines):
            match = MARKER_RE.search(raw)
            if not match:
                continue
            resources = match.group("resources").split(";")
            verbs = match.group("verbs").split(";")
            scoped = match.group("names")
            for name, severity, what, matched in classify(resources, verbs):
                detail = (
                    f"marker grants {','.join(matched)} [{match.group('verbs')}] "
                    f"cluster-wide: {what}"
                )
                if scoped:
                    detail += f" (limited to resourceNames={scoped})"
                findings.append(Finding(name, severity, path, index + 1, detail,
                                        justification_for(lines, index), "markers"))
    return findings


GO_RULE_RE = re.compile(
    r"APIGroups:\s*\[\]string\{(?P<groups>[^}]*)\}\s*,?\s*"
    r"Resources:\s*\[\]string\{(?P<resources>[^}]*)\}\s*,?\s*"
    r"Verbs:\s*\[\]string\{(?P<verbs>[^}]*)\}",
    re.S,
)


def scan_go_roles(repo, files):
    """Roles the operator builds in Go and applies to what it deploys."""
    findings = []
    for path in files:
        text = path.read_text()
        lines = text.splitlines()
        for match in GO_RULE_RE.finditer(text):
            index = text[: match.start()].count("\n")
            resources = re.findall(r'"([^"]+)"', match.group("resources"))
            verbs = re.findall(r'"([^"]+)"', match.group("verbs"))
            for name, severity, what, matched in classify(resources, verbs):
                findings.append(Finding(
                    name, severity, path, index + 1,
                    f"the operator grants {','.join(matched)} [{','.join(verbs)}] to a "
                    f"component it deploys: {what}",
                    justification_for(lines, index), "go"))
    return findings


def scan_chart(repo, files):
    """Hand-written roles and bindings in the charts."""
    findings = []
    for path in files:
        lines = path.read_text().splitlines()
        resources, verbs, start = [], [], None

        for index, raw in enumerate(lines):
            stripped = raw.strip()

            # The inline list form, where the values sit on the key's own line.
            inline = re.match(r"(resources|verbs):\s*\[(.*)\]", stripped)
            if inline:
                values = re.findall(r'"([^"]+)"', inline.group(2))
                if inline.group(1) == "resources":
                    resources, start = values, index
                else:
                    verbs = values
            elif stripped in ("resources:", "verbs:"):
                collected, cursor = [], index + 1
                while cursor < len(lines):
                    item = lines[cursor].strip()
                    # A list item that carries a key is the next rule, not a value.
                    if not item.startswith("- ") or ":" in item:
                        break
                    collected.append(item[2:].strip().strip('"'))
                    cursor += 1
                if stripped == "resources:":
                    resources, start = collected, index
                else:
                    verbs = collected

            if resources and verbs:
                for name, severity, what, matched in classify(resources, verbs):
                    findings.append(Finding(
                        name, severity, path, (start or index) + 1,
                        f"chart role grants {','.join(matched)} [{','.join(verbs)}]: {what}",
                        justification_for(lines, start or index), "chart"))
                resources, verbs = [], []

            # A binding to the namespace's default service account gives the
            # permission to every pod that did not choose an identity.
            if re.match(r"name:\s*default\s*$", stripped):
                back = "\n".join(lines[max(0, index - 4):index])
                if "ServiceAccount" in back:
                    findings.append(Finding(
                        "default-sa-binding", "high", path, index + 1,
                        "binds a role to the namespace's default ServiceAccount, so every "
                        "pod that does not name an identity receives it and no component "
                        "can be revoked individually",
                        justification_for(lines, index), "chart"))
    return findings


def scan_workloads(repo, files):
    """Security contexts and host mounts in the chart templates."""
    findings = []
    for path in files:
        lines = path.read_text().splitlines()
        for index, raw in enumerate(lines):
            for name, severity, pattern, what in WORKLOAD_CHECKS:
                if not pattern.match(raw):
                    continue
                findings.append(Finding(
                    name, severity, path, index + 1, f"{raw.strip()} grants {what}",
                    justification_for(lines, index), "workload"))
    return findings


def collect(repo, dirs, suffixes, changed):
    files = []
    for directory in dirs:
        base = repo / directory
        if not base.is_dir():
            continue
        for suffix in suffixes:
            files.extend(
                p for p in base.rglob(f"*{suffix}")
                if not p.name.endswith("_test.go") and "zz_generated" not in p.name
            )
    if changed is not None:
        files = [p for p in files if p.resolve() in changed]
    return sorted(set(files))


def changed_paths(repo):
    listed = set()
    for command in (
        ["git", "diff", "--name-only", "HEAD"],
        ["git", "diff", "--name-only", "--cached"],
        ["git", "ls-files", "--others", "--exclude-standard"],
    ):
        result = subprocess.run(command, cwd=repo, capture_output=True, text=True)
        listed.update(line for line in result.stdout.split("\n") if line)
    return {(repo / name).resolve() for name in listed}


def main():
    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("--changed", action="store_true")
    parser.add_argument("--source", choices=("markers", "go", "chart", "workload"))
    parser.add_argument("--severity", choices=("critical", "high", "medium", "low"),
                        default="low", help="report this severity and above")
    parser.add_argument("--unjustified", action="store_true",
                        help="hide findings that carry a rbac-justified note")
    args = parser.parse_args()

    here = Path(__file__).resolve()
    for parent in here.parents:
        if (parent / "operator" / "internal").is_dir():
            repo = parent
            break
    else:
        print("check-rbac.py: repository root not found", file=sys.stderr)
        return 2

    changed = changed_paths(repo) if args.changed else None

    findings = []
    if args.source in (None, "markers"):
        findings += scan_markers(repo, collect(repo, MARKER_DIRS, (".go",), changed))
    if args.source in (None, "go"):
        findings += scan_go_roles(repo, collect(repo, GO_ROLE_DIRS, (".go",), changed))
    if args.source in (None, "chart"):
        findings += scan_chart(repo, collect(repo, CHART_DIRS + (GENERATED_ROLE,),
                                            (".yaml",), changed))
    if args.source in (None, "workload"):
        findings += scan_workloads(repo, collect(repo, CHART_DIRS, (".yaml",), changed))

    ceiling = SEVERITY_ORDER[args.severity]
    findings = [f for f in findings if SEVERITY_ORDER[f.severity] <= ceiling]
    if args.unjustified:
        findings = [f for f in findings if not f.justification]

    blocking = 0
    counts = {}
    for finding in sorted(findings, key=lambda f: f.key):
        counts[finding.severity] = counts.get(finding.severity, 0) + 1
        mark = "ok  " if finding.justification else "    "
        relative = finding.path.relative_to(repo)
        print(f"{mark}{finding.severity.upper():<8} {relative}:{finding.line}  "
              f"[{finding.primitive}] {finding.detail}")
        if finding.justification:
            print(f"        justified: {finding.justification}")
        elif SEVERITY_ORDER[finding.severity] <= SEVERITY_ORDER["high"]:
            blocking += 1

    summary = ", ".join(f"{counts.get(level, 0)} {level}"
                        for level in ("critical", "high", "medium", "low"))
    print(f"\n{len(findings)} findings ({summary}); {blocking} unjustified at high or above")
    return 1 if blocking else 0


if __name__ == "__main__":
    sys.exit(main())
