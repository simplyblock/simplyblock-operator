// The support tool's page: uploads a bundle, renders the server's analysis into
// five views, and drives the ClusterDeploymentConfig editor.
//
// Every value shown comes from a customer's cluster, so nothing is ever set as
// HTML: el() builds elements and sets text with textContent only.

"use strict";

const state = { id: null, analysis: null, tab: "overview", worker: null, edit: null, built: null, ops: null, helm: null };

// landing is the page before a bundle is open: the drop zone and how to
// collect a bundle. It is kept to show again on a bundle view with no bundle.
const landing = document.getElementById("drop");

// el builds an element. Strings and numbers among the children become text
// nodes, null and false are skipped, and arrays are flattened.
function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else if (key === "class") node.className = value;
    else node.setAttribute(key, value === true ? "" : value);
  }
  const add = (child) => {
    if (child === null || child === undefined || child === false) return;
    if (Array.isArray(child)) return child.forEach(add);
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  };
  children.forEach(add);
  return node;
}

function bytes(n) {
  if (!n) return "0";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n >= 10 || i === 0 ? n.toFixed(0) : n.toFixed(1)} ${units[i]}`;
}

function badge(text, kind) { return el("span", { class: `badge ${kind}` }, text); }
function facts(pairs) {
  return el("dl", { class: "facts" }, pairs.filter(([, v]) => v !== undefined && v !== null && v !== "")
    .map(([k, v]) => [el("dt", {}, k), el("dd", {}, v)]));
}
function table(headers, rows, onRow) {
  return el("div", { class: "table-wrap" }, el("table", {},
    el("thead", {}, el("tr", {}, headers.map((h) => el("th", {}, h)))),
    el("tbody", {}, rows.map((r, i) => el("tr", onRow ? { class: "clickable", onclick: () => onRow(i) } : {},
      r.map((c) => el("td", {}, c)))))));
}
function card(title, ...children) { return el("section", { class: "card" }, title ? el("h2", {}, title) : null, children); }
function pre(text) { return el("pre", {}, text); }

async function api(method, path, body) {
  const res = await fetch(path, body instanceof FormData ? { method, body }
    : { method, body: body ? JSON.stringify(body) : undefined, headers: body ? { "Content-Type": "application/json" } : {} });
  const data = await res.json().catch(() => ({ error: `${res.status} ${res.statusText}` }));
  if (!res.ok) throw new Error(data.error || `${res.status}`);
  return data;
}

// ─── Collecting a bundle ───────────────────────────────────────────────────

// commandLine is a command with a button that copies it.
function commandLine(command) {
  return el("div", { class: "command" }, el("code", {}, command),
    el("button", { class: "secondary", onclick: (e) => {
      navigator.clipboard?.writeText(command);
      e.target.textContent = "Copied";
      setTimeout(() => { e.target.textContent = "Copy"; }, 1500);
    } }, "Copy"));
}

// The command a customer runs is the server's, so the page and the CLI banner
// never disagree. It is on the empty page in full and in the footer of every
// view.
api("GET", "/api/meta").then((meta) => {
  state.meta = meta;
  const collect = meta.collect || {};
  document.getElementById("collect-help")?.replaceChildren(
    el("h3", {}, "Collecting a bundle"),
    el("p", { class: "note" }, "The customer runs this on a machine whose kubectl reaches the cluster. It only reads, never reads a Secret, and writes simplyblock-bundle-<run>-<time>.tar.gz to the current directory:"),
    commandLine(collect.command),
    el("p", { class: "note" }, "To name the namespace and the discovery run instead of the defaults (the context's namespace, and its newest discovery run):"),
    commandLine(collect.withOptions),
    el("p", { class: "note" }, "A customer who cannot or will not run the script runs its steps by hand: ",
      el("button", { class: "secondary", onclick: () => manualDialog(collect.manual || []) }, "Without the script…")));
  document.getElementById("footer").replaceChildren(
    el("span", { class: "muted" }, "Collect a bundle:"), commandLine(collect.command),
    el("button", { class: "secondary", onclick: () => manualDialog(collect.manual || []) }, "Without the script…"));
}).catch(() => {});

// manualDialog lists the script's steps as individual commands. They share shell
// variables, so they run in one session, in order.
function manualDialog(steps) {
  const all = steps.map((s) => s.command).join("\n");
  const dialog = el("dialog", { class: "modal" },
    el("div", { class: "row modal-head" }, el("h2", {}, "Collecting a bundle without the script"),
      el("button", { class: "secondary", onclick: () => dialog.close() }, "Close")),
    el("p", { class: "note" }, "Run these in one bash session, in order, on a machine whose kubectl reaches the cluster. " +
      "Replace <namespace> and <run> in the first one. They only read, never read a Secret, and write the same " +
      "simplyblock-bundle-<run>-<time>.tar.gz the script does."),
    el("ol", { class: "steps" }, steps.map((s) => el("li", {}, el("div", { class: "step-title" }, s.title), commandLine(s.command)))),
    el("div", { class: "row" }, el("button", { class: "primary", onclick: (e) => {
      navigator.clipboard?.writeText(all);
      e.target.textContent = "Copied";
      setTimeout(() => { e.target.textContent = "Copy all"; }, 1500);
    } }, "Copy all")));
  dialog.addEventListener("close", () => dialog.remove());
  document.body.append(dialog);
  dialog.showModal();
}

// ─── Upload ────────────────────────────────────────────────────────────────

async function openBundle(file) {
  const view = document.getElementById("view");
  view.replaceChildren(card(null, el("p", {}, `Reading ${file.name}…`)));
  const form = new FormData();
  form.append("bundle", file);
  try {
    const out = await api("POST", "/api/bundles", form);
    Object.assign(state, { id: out.id, analysis: out.analysis, worker: null, edit: null, built: null, tab: "overview" });
    show(out.analysis.bundle || file.name);
  } catch (e) {
    view.replaceChildren(el("div", { class: "error-banner" }, `This file could not be read as a support bundle: ${e.message}`));
  }
}

document.getElementById("file").addEventListener("change", (e) => {
  if (e.target.files[0]) openBundle(e.target.files[0]);
  e.target.value = "";
});
document.addEventListener("dragover", (e) => { e.preventDefault(); document.getElementById("drop")?.classList.add("over"); });
document.addEventListener("dragleave", () => document.getElementById("drop")?.classList.remove("over"));
document.addEventListener("drop", (e) => {
  e.preventDefault();
  if (e.dataTransfer.files[0]) openBundle(e.dataTransfer.files[0]);
});
document.getElementById("tabs").addEventListener("click", (e) => {
  const tab = e.target.dataset?.tab;
  if (tab) { state.tab = tab; render(); }
});

// show displays a loaded bundle, and keeps it in the URL so that a reload
// finds it again while the server still holds it.
function show(name) {
  document.getElementById("bundle-name").textContent = name;
  state.ops = null;
  render();
}

// A URL naming a bundle the server still holds opens it.
(async () => {
  const id = new URLSearchParams(location.search).get("bundle");
  if (!id) {
    // The run editor needs no bundle, so a link to it opens it.
    if (["#run", "#helm"].includes(location.hash)) { state.tab = location.hash.slice(1); render(); }
    return;
  }
  try {
    state.analysis = await api("GET", `/api/bundles/${encodeURIComponent(id)}`);
    state.id = id;
    const tab = location.hash.slice(1);
    if (["overview", "workers", "cluster", "cdc", "editor", "run", "helm"].includes(tab)) state.tab = tab;
    show(state.analysis.bundle);
  } catch (e) {
    history.replaceState(null, "", location.pathname);
  }
})();

function render() {
  history.replaceState(null, "", state.id ? `?bundle=${encodeURIComponent(state.id)}#${state.tab}` : `#${state.tab}`);
  for (const b of document.querySelectorAll("#tabs button[data-bundle]")) b.hidden = !state.analysis;
  const errors = (state.analysis?.diagnoses || []).filter((d) => d.severity === "error").length;
  const overviewTab = document.querySelector('#tabs button[data-tab="overview"]');
  overviewTab.replaceChildren("Overview", errors ? el("span", { class: "badge err tab-badge" }, String(errors)) : "");
  for (const b of document.querySelectorAll("#tabs button")) b.classList.toggle("active", b.dataset.tab === state.tab);
  const views = { overview, workers, cluster, cdc, editor, run: runEditor, helm: helmEditor };
  const bundleView = !["run", "helm"].includes(state.tab);
  document.getElementById("view").replaceChildren(bundleView && !state.analysis ? landing : views[state.tab]());
}

// ─── Overview ──────────────────────────────────────────────────────────────

function kernelOf(w) {
  if (!w.report) return "";
  if (!w.kernelReported) return el("span", { class: "muted" }, `not in report version ${w.reportVersion}`);
  return w.report.hostOS?.kernel?.release || "";
}

function overview() {
  const a = state.analysis;
  const run = a.run || {};
  const workerRows = a.workers.map((w) => [
    el("strong", {}, w.name),
    (w.node.roles || []).join(", "),
    w.node.internalIP || "",
    w.inReport ? `v${w.reportVersion}` : badge("no report", "warn"),
    w.report?.hostOS?.prettyName || w.report?.hostOS?.distro || "",
    kernelOf(w),
    w.report ? `${w.report.cpu?.physicalCores ?? "?"} cores / ${w.report.cpu?.sockets ?? "?"} socket(s)` : "",
    w.report ? bytes(w.report.memory?.totalBytes) : "",
    w.report ? el("span", {}, `${w.summary.available} of ${w.summary.devices} `, w.summary.available ? badge(bytes(w.summary.availableBytes), "ok") : badge("none", "muted")) : "",
    (w.storageNodes || []).length ? badge(w.storageNodes.join(", "), "muted") : "",
  ]);
  return el("div", {},
    diagnosisCard(a.diagnoses || []),
    el("div", { class: "grid" },
      card("Discovery run", facts([
        ["Name", run.name], ["Namespace", run.namespace], ["Action", run.action],
        ["Phase", run.phase ? badge(run.phase, run.phase === "Succeeded" ? "ok" : run.phase === "Failed" ? "err" : "muted") : ""],
        ["Environment", run.environment], ["Started", run.created], ["Draft", run.configRef], ["Message", run.message],
        ["kubectl", a.kubectlVersion],
      ]), run.settings ? [el("h3", {}, "spec.discover"), pre(run.settings)] : el("p", { class: "note" }, "The run used the default settings.")),
      card("What the bundle could not tell",
        a.problems?.length ? el("ul", {}, a.problems.map((p) => el("li", {}, p))) : el("p", { class: "note" }, "Every file of the bundle was read."),
        el("h3", {}, "Collection errors (errors.txt)"),
        a.collection ? pre(a.collection) : el("p", { class: "note" }, "Nothing failed while collecting."),
        el("h3", {}, "Warning events"),
        a.warningEvents?.length ? pre(a.warningEvents.join("\n")) : el("p", { class: "note" }, "None, or the events had expired when the bundle was collected."))),
    card(`Workers (${a.workers.length})`,
      table(["Worker", "Roles", "Internal IP", "Report", "OS", "Kernel", "CPU", "Memory", "Available devices", "Storage nodes"], workerRows,
        (i) => { state.worker = a.workers[i].name; state.tab = "workers"; render(); })));
}

// diagnosisCard lists what went wrong, errors first. An entry about a worker or
// a view opens it.
function diagnosisCard(list) {
  const counts = { error: 0, warning: 0, info: 0 };
  for (const d of list) counts[d.severity]++;
  const summary = list.length
    ? el("div", { class: "row" },
      counts.error ? badge(`${counts.error} error(s)`, "err") : null,
      counts.warning ? badge(`${counts.warning} warning(s)`, "warn") : null,
      counts.info ? badge(`${counts.info} note(s)`, "muted") : null)
    : el("p", { class: "note" }, "Nothing in this bundle points at a problem.");
  return card("Diagnosis", summary, list.map((d) => el("div", {
    class: `finding ${d.severity} clickable`,
    title: d.tab ? "Open" : null,
    onclick: () => {
      if (!d.tab) return;
      state.tab = d.tab;
      if (d.worker) state.worker = d.worker;
      render();
    },
  }, el("div", {}, el("span", { class: "reason" }, d.title), el("span", { class: "source" }, d.code)),
  d.detail ? el("div", { class: "detail" }, d.detail) : null)));
}

// ─── Workers ───────────────────────────────────────────────────────────────

function workers() {
  const a = state.analysis;
  if (!state.worker && a.workers.length) state.worker = (a.workers.find((w) => w.report) || a.workers[0]).name;
  const w = a.workers.find((x) => x.name === state.worker);
  return el("div", { class: "split" },
    card(null, el("div", { class: "list" }, a.workers.map((x) => el("button", {
      class: x.name === state.worker ? "active" : "", onclick: () => { state.worker = x.name; render(); },
    }, x.name, " ", x.inReport ? "" : badge("no report", "warn"))))),
    w ? workerDetail(w) : card(null, el("p", {}, "The bundle names no workers.")));
}

function factState(f) {
  if (!f?.state) return "";
  const kind = { Enabled: "ok", Available: "ok", Loadable: "ok", Disabled: "warn", NotBuilt: "err", Absent: "warn" }[f.state] || "muted";
  return el("span", {}, badge(f.state, kind), " ", el("span", { class: "muted" }, f.detail || ""));
}

// attachmentsCard is the NetworkAttachmentDefinitions the probe read, and
// whether this worker has the host interface each builds on. A report before
// version 11 carries no reading, which is not a cluster without Multus.
function attachmentsCard(w) {
  const r = w.report;
  if (!w.networkAttachmentsReported) {
    return card("Network attachments", el("p", { class: "note" }, `Report version ${w.reportVersion} carries no network attachments.`));
  }
  const na = r.networkAttachments;
  if (!na) return card("Network attachments", el("p", { class: "note" }, "The probe was given no cluster to read them from."));
  if (na.state === "NotServed") {
    return card("Network attachments", el("p", { class: "note" }, "The cluster does not serve NetworkAttachmentDefinitions (no Multus). Storage nodes use host networking."));
  }
  if (na.state !== "Served") {
    return card("Network attachments", el("div", { class: "finding warning" }, el("span", { class: "reason" }, na.state), na.detail || ""));
  }
  const names = new Set((r.interfaces || []).map((i) => i.name));
  const slots = new Set((r.interfaces || []).map((i) => i.pciAddress).filter(Boolean));
  const onWorker = (d) => {
    if (d.type === "bridge") return "created by the plugin";
    if (d.hostInterface) return names.has(d.hostInterface) ? "yes" : badge(`no ${d.hostInterface}`, "err");
    if (d.pciAddress) return slots.has(d.pciAddress) ? "yes" : badge(`no NIC at ${d.pciAddress}`, "err");
    return el("span", { class: "muted" }, d.resourceName ? `placed by ${d.resourceName}` : "not checked");
  };
  return card(`Network attachments in ${na.namespace || "every namespace"}`, !(na.definitions || []).length
    ? el("p", { class: "note" }, "The namespace holds no NetworkAttachmentDefinition.")
    : table(["Name", "Type", "Host interface", "Mode", "IPAM", "On this worker"], na.definitions.map((d) => [
      el("strong", {}, d.name), d.type || el("span", { class: "muted" }, d.problem || ""), d.hostInterface || d.pciAddress || "",
      d.mode || "", d.ipam || "", onWorker(d)])));
}

function workerDetail(w) {
  const r = w.report;
  const n = w.node;
  const out = [card(w.name, facts([
    ["In the cluster", w.inCluster ? "yes" : badge("no Node object in the bundle", "warn")],
    ["Probe report", w.inReport ? `${w.reportFile}, version ${w.reportVersion}` : badge("none", "warn")],
    ["Roles", (n.roles || []).join(", ")], ["Internal IP", n.internalIP],
    ["Ready", n.ready], ["Cordoned", n.cordoned ? badge("cordoned", "warn") : "no"],
    ["Taints", (n.taints || []).join(", ")], ["Kubelet", n.kubeletVersion], ["OS image", n.osImage],
    ["Storage nodes", (w.storageNodes || []).join(", ")],
  ]), n.labels ? [el("h3", {}, "Labels"), pre(Object.entries(n.labels).map(([k, v]) => `${k}=${v}`).join("\n"))] : null)];
  if (!r) return el("div", {}, out);

  const os = r.hostOS || {};
  const k = os.kernel || {};
  out.push(el("div", { class: "grid" },
    card("Operating system and kernel", facts([
      ["Distribution", os.prettyName || os.distro], ["Family", os.family], ["Architecture", os.architecture],
      ["Kernel", w.kernelReported ? k.release : el("span", { class: "muted" }, `report version ${w.reportVersion} carries no kernel facts`)],
      ...(w.kernelReported ? [
        ["Version", k.version ? `${k.version.major}.${k.version.minor}.${k.version.patch}` : ""],
        ["NVMe multipath", factState(k.nvmeMultipath)], ["/dev/kvm", factState(k.kvm)],
        ["/dev/net/tun", factState(k.tun)], ["pNFS block", factState(k.pnfsBlock)],
      ] : []),
    ])),
    card("CPU and memory", facts([
      ["CPUs", `${r.cpu?.onlineCPUs ?? "?"} online, ${r.cpu?.physicalCores ?? "?"} cores, ${r.cpu?.sockets ?? "?"} socket(s)`],
      ["Hyper-threading", r.cpu?.hyperThreading ? "on" : "off"],
      ["NUMA nodes", (r.cpu?.numaNodes || []).map((x) => `${x.node}: ${x.physicalCores} cores`).join(", ")],
      ["Memory", `${bytes(r.memory?.totalBytes)} total, ${bytes(r.memory?.availableBytes)} available`],
      ["Huge pages", (r.hugePages || []).map((p) => `${bytes(p.sizeBytes)}: ${p.free} of ${p.total} free`).join(", ") || "none"],
    ]), el("h3", {}, "Discovery would choose"), facts([
      ["Management interface", w.suggested.management || el("span", { class: "muted" }, "none qualifies")],
      ["Data interfaces", (w.suggested.data || []).join(", ") || el("span", { class: "muted" }, "none: data runs on the management interface")],
    ]))));

  out.push(attachmentsCard(w));

  // Loop, network, and RAM devices and empty ones are refused on every worker by
  // design, and they outnumber the disks; they are one click away rather than
  // in the way.
  const noise = (d) => ["Loop", "Network", "RAM"].includes(d.kind) || !d.sizeBytes;
  const devices = (w.devices || []).filter((d) => state.showNoise || !noise(d))
    .sort((x, y) => (x.kind === "Disk" ? 0 : 1) - (y.kind === "Disk" ? 0 : 1) || x.name.localeCompare(y.name, undefined, { numeric: true }));
  const hidden = (w.devices || []).filter(noise).length;
  out.push(card("Block devices",
    hidden ? el("div", { class: "row" }, el("label", { class: "row" }, el("input", { type: "checkbox", checked: !!state.showNoise,
      onchange: (e) => { state.showNoise = e.target.checked; render(); } }), `Show loop, network, RAM, and empty devices (${hidden})`)) : null,
    !devices.length ? el("p", { class: "note" }, hidden ? "Only loop, network, RAM, and empty devices." : "The report lists no block device.") :
    table(["Device", "Kind", "Transport", "Size", "Model", "NUMA", "CDC name", "Status"],
      devices.map((d) => [
        el("span", {}, el("strong", {}, d.name), el("br"), el("span", { class: "muted" }, d.stablePath || d.path)),
        d.kind, d.transport || "", bytes(d.sizeBytes), d.model || "", numa(d.numaNode),
        d.kind === "Disk" ? el("code", {}, d.nvmeID || d.blockID || "") : "",
        d.available ? badge(d.content || "available", "ok")
          : el("div", {}, (d.rejections || []).map((x) => el("div", {}, badge(x.reason, "err"), " ", el("span", { class: "muted" }, x.detail || "")))),
      ]))));

  out.push(el("div", { class: "grid" },
    card("Network interfaces", table(["Interface", "Kind", "State", "MTU", "Speed", "Driver", "Addresses"],
      (r.interfaces || []).map((i) => [el("strong", {}, i.name), i.kind || "", i.state || "", i.mtu || "",
        i.speedMbps ? `${i.speedMbps / 1000} Gb/s` : "", i.driver || "", (i.addresses || []).join(", ")]))),
    card("NVMe controllers", (r.nvmeControllers || []).length
      ? table(["Address", "Driver", "Vendor", "Product", "NUMA", "In use"], r.nvmeControllers.map((c) => [
        el("code", {}, c.address), c.driver || badge("none", "muted"), c.vendor || "", c.product || "", numa(c.numaNode),
        inUseOf(c)]))
      : el("p", { class: "note" }, "None."),
    (r.reclaimed || []).length ? [el("h3", {}, "Returned to the kernel by the probe"), pre(r.reclaimed.map((c) => c.address).join("\n"))] : null)));
  out.push(lbaFormatsCard(r.nvmeControllers || []));
  if ((r.unreadable || []).length) out.push(card("Readings the probe could not take", el("ul", {}, r.unreadable.map((u) => el("li", {}, u)))));
  return el("div", {}, out);
}

// numa is a NUMA node, and says so when the kernel places the device on none,
// which is every device of a virtual machine without a NUMA topology.
function numa(n) {
  return n >= 0 ? String(n) : el("span", { class: "muted", title: "the kernel reports -1: no NUMA locality, as on most virtual machines" }, "none");
}

// inUseOf says whether a controller is held. Only one a userspace driver owns
// is checked: the kernel's own driver presents its namespaces as block devices,
// which the block-device checks cover.
function inUseOf(c) {
  const userspace = c.driver === "vfio-pci" || c.driver === "uio_pci_generic";
  if (!userspace) return el("span", { class: "muted", title: "not checked: its disks are block devices" }, c.driver ? "kernel driver" : "no driver");
  if (c.inUse === undefined) return badge("could not be checked", "warn");
  return c.inUse ? badge("held", "err") : badge("free", "ok");
}

// lbaFormatsCard lists the block sizes each kernel-driven controller's
// namespaces can be formatted with, one row per format, with the format in use
// marked. A controller on a userspace driver cannot be asked and has no rows.
function lbaFormatsCard(controllers) {
  const rows = [];
  for (const c of controllers) {
    for (const ns of c.lbaFormats || []) {
      ns.formats.forEach((f, i) => rows.push([
        i ? "" : el("code", {}, c.address),
        i ? "" : (ns.common ? el("span", { class: "muted", title: "no namespace attached: the formats a new namespace could take" }, "common") : `nsid ${ns.nsid}`),
        String(f.index),
        bytes(f.dataSize),
        f.metadataSize ? `${f.metadataSize} B${ns.extendedMetadata && f.index === ns.current ? ", extended" : ""}` : el("span", { class: "muted" }, "none"),
        badge(f.relativePerformance, { best: "ok", better: "ok", good: "muted", degraded: "warn" }[f.relativePerformance] || "muted"),
        f.index === ns.current ? badge("in use", "ok") : "",
      ]));
    }
  }
  const asked = controllers.some((c) => c.driver === "nvme");
  return card("NVMe LBA formats", rows.length
    ? table(["Controller", "Namespace", "Format", "Block size", "Metadata", "Performance", ""], rows)
    : el("p", { class: "note" }, asked
      ? "The report carries no LBA formats: the probe predates them, or the controllers did not answer (see the readings the probe could not take)."
      : "No controller on the kernel's NVMe driver, so none could be asked."));
}

// ─── Cluster ───────────────────────────────────────────────────────────────

function cluster() {
  const c = state.analysis.cluster;
  const objects = (list, extra) => (list || []).length
    ? table(["Name", "Phase", ...extra.map((e) => e[0])], list.map((o) => [el("strong", {}, o.name), o.phase || "", ...extra.map((e) => o[e[1]] || "")]))
    : el("p", { class: "note" }, "None in the bundle.");
  return el("div", {},
    card("Control planes", objects(c.controlPlanes, [])),
    card("Storage clusters", objects(c.storageClusters, [["Role", "role"], ["Supercluster", "supercluster"]])),
    card("Storage nodes", objects(c.storageNodes, [["Worker", "worker"], ["Cluster", "cluster"]])));
}

// ─── CDC ───────────────────────────────────────────────────────────────────

function findingsOf(list) {
  if (!list?.length) return el("p", { class: "note" }, "No problems found.");
  return el("div", {}, list.map((f) => el("div", { class: `finding ${f.severity}` },
    el("span", { class: "reason" }, f.reason), f.message, el("span", { class: "source" }, f.source === "operator" ? "operator check" : "from the bundle's reports"))));
}

function nodeSetsOf(sets) {
  return (sets || []).map((s) => el("div", {}, el("h3", {}, `Node set ${s.name}`),
    table(["Group", "Workers", "Devices", "Management", "Data", "Failure domain"], (s.groups || []).map((g) => [
      el("strong", {}, g.name), g.workers.join(", "), el("span", {}, `${g.class || "none"} × ${(g.devices || []).length}`, el("br"), el("span", { class: "muted" }, (g.devices || []).join(", "))),
      g.networkAttachment ? el("span", {}, badge("attachment", "ok"), " ", g.networkAttachment.management) : g.mgmtInterface || "",
      g.networkAttachment ? g.networkAttachment.data || el("span", { class: "muted" }, "the management attachment")
        : (g.dataInterfaces || []).join(", "),
      g.failureDomain || ""]))));
}

function download(name, text) {
  const a = el("a", { href: URL.createObjectURL(new Blob([text], { type: "application/yaml" })), download: name });
  document.body.append(a);
  a.click();
  a.remove();
}

function cdc() {
  const c = state.analysis.cdc;
  if (!c) {
    return card("ClusterDeploymentConfig", el("p", {}, "The bundle carries no ClusterDeploymentConfig: the run wrote none, or it failed first."),
      el("button", { class: "primary", onclick: () => { state.tab = "editor"; render(); } }, "Create one in the editor"));
  }
  const errors = c.findings.filter((f) => f.severity === "error").length;
  return el("div", {},
    card(c.name, facts([
      ["Approved", c.approved ? badge("approved", "ok") : badge("draft", "muted")],
      ["Phase", c.phase], ["Message", c.message],
      ["Verdict", errors ? badge(`${errors} error(s)`, "err") : badge("no errors", "ok")],
    ]), el("div", { class: "row" },
      el("button", { class: "primary", onclick: () => { state.tab = "editor"; render(); } }, c.approved ? "Copy into the editor" : "Edit"),
      el("button", { class: "secondary", onclick: () => download(`${c.name}.yaml`, c.yaml) }, "Download YAML"))),
    card("Findings", findingsOf(c.findings)),
    card("Layout", nodeSetsOf(c.nodeSets), c.cluster ? [el("h3", {}, "spec.cluster"), pre(c.cluster)] : null),
    card("YAML", pre(c.yaml)));
}

// ─── Helm values editor ────────────────────────────────────────────────────

// getAt, setAt, and deleteAt read and write a path, given as its keys, in
// nested objects. A key may hold dots of its own, as an annotation such as
// reloader.stakater.com/auto does, so a path is never split on them. deleteAt
// drops parents it leaves empty, so the overrides stay minimal.
const getAt = (obj, keys) => keys.reduce((o, k) => (o == null ? undefined : o[k]), obj);
function setAt(obj, keys, value) {
  let node = obj;
  for (const k of keys.slice(0, -1)) node = node[k] = (node[k] && typeof node[k] === "object" && !Array.isArray(node[k])) ? node[k] : {};
  node[keys[keys.length - 1]] = value;
}
function deleteAt(obj, keys) {
  const walk = (node, i) => {
    if (!node || typeof node !== "object") return;
    if (i === keys.length - 1) { delete node[keys[i]]; return; }
    walk(node[keys[i]], i + 1);
    if (node[keys[i]] && typeof node[keys[i]] === "object" && !Object.keys(node[keys[i]]).length) delete node[keys[i]];
  };
  walk(obj, 0);
}
const countLeaves = (o) => (o && typeof o === "object" && !Array.isArray(o) ? Object.values(o).reduce((n, v) => n + countLeaves(v), 0) : 1);
// keyOf names a path for the YAML drafts, and under tells whether one path
// lies within another.
const keyOf = (keys) => JSON.stringify(keys);
const under = (keys, prefix) => prefix.length < keys.length && prefix.every((k, i) => keys[i] === k);

// helmEditor edits the chart's values. Only what differs from the chart's
// defaults is kept, and that is the values file it writes.
function helmEditor() {
  if (!state.helm) {
    api("GET", "/api/helm").then((out) => {
      state.helm = { ...out, overrides: {}, yaml: {}, section: null };
      render();
    }).catch((e) => { state.helm = { error: e.message }; render(); });
    return card(null, el("p", {}, "Reading the chart…"));
  }
  const h = state.helm;
  if (h.error) return el("div", { class: "error-banner" }, h.error);

  const general = h.fields.filter((f) => f.kind !== "object");
  const sections = [...(general.length ? [{ key: "General", segments: [], children: general, help: "The chart's top-level values." }] : []),
    ...h.fields.filter((f) => f.kind === "object")];
  if (!h.section) h.section = sections[0]?.key;
  const current = sections.find((s) => s.key === h.section) || sections[0];
  const drafts = () => Object.values(h.yaml);
  // How many values a section changes: the leaves set under it, and the
  // values given as YAML.
  const overridesIn = (section) => {
    if (!section.segments.length) return general.filter((f) => h.overrides[f.key] !== undefined || h.yaml[keyOf(f.segments)]).length;
    const set = getAt(h.overrides, section.segments);
    return (set === undefined ? 0 : countLeaves(set)) + drafts().filter((d) => under(d.segments, section.segments)).length;
  };
  const changed = (f) => getAt(h.overrides, f.segments) !== undefined || Boolean(h.yaml[keyOf(f.segments)]);

  const control = (f) => {
    const value = getAt(h.overrides, f.segments);
    const set = (v) => { if (v === undefined) deleteAt(h.overrides, f.segments); else setAt(h.overrides, f.segments, v); };
    const shown = (v) => (v === undefined || v === null ? "" : typeof v === "object" ? JSON.stringify(v) : String(v));
    if (f.enum?.length || f.kind === "bool") {
      const options = f.kind === "bool" ? ["true", "false"] : f.enum;
      return el("select", { onchange: (e) => {
        const v = e.target.value;
        set(v === "" ? undefined : f.kind === "bool" ? v === "true" : v);
        render();
      } }, el("option", { value: "" }, `default (${shown(f.default) || "unset"})`),
      options.map((o) => el("option", { value: o, selected: value !== undefined && String(value) === o }, o)));
    }
    if (f.kind === "list") {
      return el("textarea", { rows: 2, placeholder: (f.default || []).join("\n") || "one per line",
        onchange: (e) => { set(lines(e.target.value)); render(); } }, lineText(value));
    }
    if (f.kind === "map") {
      return el("textarea", { rows: 2, placeholder: "key=value, one per line",
        onchange: (e) => {
          const entries = lines(e.target.value).map((l) => l.split("=")).map(([k, ...v]) => [k.trim(), v.join("=").trim()]);
          set(Object.fromEntries(entries)); render();
        } }, Object.entries(value || {}).map(([k, v]) => `${k}=${v}`).join("\n"));
    }
    if (f.kind === "yaml") {
      // A draft is what was typed; without one, the value set, imported or
      // not, is shown as JSON, which YAML reads as well. Editing replaces the
      // value, and emptying the field returns it to the chart's default.
      const draft = h.yaml[keyOf(f.segments)];
      return el("textarea", { rows: 3, placeholder: `YAML; default ${shown(f.default) || "empty"}`,
        onchange: (e) => {
          deleteAt(h.overrides, f.segments);
          if (e.target.value.trim()) h.yaml[keyOf(f.segments)] = { segments: f.segments, text: e.target.value };
          else delete h.yaml[keyOf(f.segments)];
          render();
        } }, draft ? draft.text : value !== undefined ? JSON.stringify(value, null, 2) : "");
    }
    // A text field sets what it holds, the empty string included, since an
    // empty value can be a setting of its own (an empty prometheusURL turns
    // capacity sampling off). The reset button returns it to the default.
    const numeric = f.kind === "int" || f.kind === "number";
    return el("input", { type: numeric ? "number" : "text", value: shown(value), placeholder: shown(f.default),
      onchange: (e) => {
        const v = e.target.value;
        set(numeric ? (v === "" ? undefined : Number(v)) : v);
        render();
      } });
  };
  const reset = (f) => (changed(f)
    ? el("button", { class: "secondary reset", title: "Return to the chart's default", onclick: () => {
      deleteAt(h.overrides, f.segments);
      delete h.yaml[keyOf(f.segments)];
      render();
    } }, "Reset")
    : null);
  const help = (f) => (f.help ? el("div", { class: "help", title: f.help }, f.help.split("\n").slice(0, 2).join(" ")) : null);
  const fieldsOf = (list) => list.map((f) => f.kind === "object"
    ? el("details", { class: "value-group", open: getAt(h.overrides, f.segments) !== undefined || drafts().some((d) => under(d.segments, f.segments)) },
      el("summary", {}, el("strong", {}, f.key), help(f)), fieldsOf(f.children || []))
    : el("div", { class: changed(f) ? "value-row changed" : "value-row" },
      el("div", {}, el("code", {}, f.key), help(f)), el("div", { class: "row value-control" }, control(f), reset(f))));

  const importDialog = () => {
    const area = el("textarea", { rows: 18, placeholder: "paste a values.yaml" });
    const dialog = el("dialog", { class: "modal" },
      el("div", { class: "row modal-head" }, el("h2", {}, "Import a values file"), el("button", { class: "secondary", onclick: () => dialog.close() }, "Close")),
      el("p", { class: "note" }, "The file becomes the overrides, replacing what is set here, and is checked against the chart."),
      area,
      el("div", { class: "row", style: "margin-top:8px" }, el("button", { class: "primary", onclick: async () => {
        try {
          const out = await api("POST", "/api/helm/import", { text: area.value });
          h.overrides = out.overrides || {}; h.yaml = {}; h.built = null; h.imported = out.findings;
          dialog.close(); render();
        } catch (e) { area.setCustomValidity(e.message); area.reportValidity(); }
      } }, "Import")));
    dialog.addEventListener("close", () => dialog.remove());
    document.body.append(dialog);
    dialog.showModal();
  };

  const build = async () => {
    try {
      h.built = await api("POST", "/api/helm/values", { overrides: h.overrides, yaml: drafts() });
    } catch (e) {
      h.built = { error: e.message };
    }
    render();
  };
  const findingsList = (list) => (list || []).length
    ? list.map((x) => el("div", { class: `finding ${x.severity}` }, x.path ? el("span", { class: "reason" }, x.path) : null, x.message))
    : el("p", { class: "note" }, "The chart's schema takes these values.");
  const result = !h.built ? null : h.built.error ? el("div", { class: "error-banner" }, h.built.error) : el("div", {},
    card("Findings", findingsList(h.built.findings)),
    card("values.yaml", el("div", { class: "row" },
      el("button", { class: "primary", onclick: () => download("values.yaml", h.built.yaml) }, "Download values.yaml"),
      el("button", { class: "secondary", onclick: () => navigator.clipboard?.writeText(h.built.yaml) }, "Copy")),
    pre(h.built.yaml)),
    card("Install or upgrade with it", el("p", { class: "note" }, "The install for a new release, the upgrade for an existing one:"),
      h.built.commands.map((c) => commandLine(c))));

  return el("div", {},
    el("div", { class: "row", style: "margin-bottom:12px" },
      el("span", { class: "muted" }, `${h.chart.name} ${h.chart.version}`),
      el("button", { class: "secondary", onclick: importDialog }, "Import values.yaml…"),
      el("button", { class: "primary", onclick: build }, "Build values"),
      el("button", { class: "secondary", onclick: () => { h.overrides = {}; h.yaml = {}; h.built = null; h.imported = null; render(); } }, "Reset to defaults")),
    h.imported ? card("Imported file", findingsList(h.imported)) : null,
    el("div", { class: "split" },
      card(null, el("div", { class: "list" }, sections.map((sec) => {
        const n = overridesIn(sec);
        return el("button", { class: sec.key === current.key ? "active" : "", onclick: () => { h.section = sec.key; render(); } },
          sec.key, n > 0 ? el("span", { class: "badge warn", style: "margin-left:6px" }, String(n)) : null);
      }))),
      card(current.key, help(current), fieldsOf(current.children || []))),
    el("div", { style: "margin-top:16px" }, result));
}

// ─── Discovery run editor ──────────────────────────────────────────────────

// lines is a textarea's entries, one per line, and lineText the reverse.
const lines = (text) => text.split("\n").map((l) => l.trim()).filter(Boolean);
const lineText = (list) => (list || []).join("\n");

// runEditor builds a discovery run, an OperatorOps with action Discover. It
// needs no bundle; with one, it starts from the settings of the bundle's run.
function runEditor() {
  if (!state.ops) {
    api("GET", `/api/operatorops/prefill${state.id ? `?bundle=${encodeURIComponent(state.id)}` : ""}`)
      .then((req) => { req.discover = req.discover || {}; state.ops = { req }; render(); })
      .catch((e) => { state.ops = { error: e.message }; render(); });
    return card(null, el("p", {}, "Preparing the run…"));
  }
  if (state.ops.error) return el("div", { class: "error-banner" }, state.ops.error);
  const { req } = state.ops;
  const d = req.discover;
  d.deviceFilter = d.deviceFilter || {};
  const f = d.deviceFilter;
  d.networkAttachment = d.networkAttachment || {};
  const na = d.networkAttachment;

  const text = (obj, key, placeholder) => el("input", { type: "text", value: obj[key] ?? "", placeholder: placeholder || "",
    oninput: (e) => { obj[key] = e.target.value || undefined; } });
  const list = (obj, key, placeholder) => el("textarea", { rows: 3, placeholder: placeholder || "one per line",
    oninput: (e) => { const v = lines(e.target.value); obj[key] = v.length ? v : undefined; } }, lineText(obj[key]));
  const tri = (obj, key) => el("select", { onchange: (e) => { obj[key] = e.target.value === "" ? undefined : e.target.value === "true"; } },
    [["", "unset"], ["true", "on"], ["false", "off"]].map(([v, l]) => el("option", { value: v, selected: String(obj[key] ?? "") === v }, l)));
  const selector = el("textarea", { rows: 3, placeholder: "key=value, one per line",
    oninput: (e) => {
      const entries = lines(e.target.value).map((l) => l.split("=")).map(([k, ...v]) => [k.trim(), v.join("=").trim()]);
      d.nodeSelector = entries.length ? Object.fromEntries(entries) : undefined;
    } }, Object.entries(d.nodeSelector || {}).map(([k, v]) => `${k}=${v}`).join("\n"));

  d.tolerations = d.tolerations || [];
  const tolerations = el("div", {},
    d.tolerations.map((t, i) => el("div", { class: "row" },
      el("input", { type: "text", value: t.key || "", placeholder: "key", oninput: (e) => { t.key = e.target.value || undefined; } }),
      el("select", { onchange: (e) => { t.operator = e.target.value; } },
        ["Equal", "Exists"].map((o) => el("option", { value: o, selected: (t.operator || "Equal") === o }, o))),
      el("input", { type: "text", value: t.value || "", placeholder: "value", oninput: (e) => { t.value = e.target.value || undefined; } }),
      el("select", { onchange: (e) => { t.effect = e.target.value || undefined; } },
        [["", "any effect"], ["NoSchedule", "NoSchedule"], ["PreferNoSchedule", "PreferNoSchedule"], ["NoExecute", "NoExecute"]]
          .map(([v, l]) => el("option", { value: v, selected: (t.effect || "") === v }, l))),
      el("button", { class: "secondary", onclick: () => { d.tolerations.splice(i, 1); render(); } }, "Remove"))),
    el("button", { class: "secondary", onclick: () => { d.tolerations.push({ operator: "Equal" }); render(); } }, "Add toleration"));

  const workerHint = state.analysis
    ? el("button", { class: "secondary", onclick: () => { d.workers = state.analysis.workers.map((w) => w.name); render(); } }, "Use the bundle's workers")
    : null;

  const build = async () => {
    const body = JSON.parse(JSON.stringify(req));
    const filter = body.discover.deviceFilter;
    if (filter && !Object.values(filter).some((v) => v !== undefined && v !== null && v !== "")) delete body.discover.deviceFilter;
    if (!(body.discover.tolerations || []).length) delete body.discover.tolerations;
    if (!body.discover.networkAttachment?.management && !body.discover.networkAttachment?.data) delete body.discover.networkAttachment;
    try {
      state.ops.built = await api("POST", "/api/operatorops", body);
    } catch (e) {
      state.ops.built = { error: e.message };
    }
    render();
  };

  const built = state.ops.built;
  const result = !built ? null : built.error ? el("div", { class: "error-banner" }, built.error) : el("div", {},
    card("Findings", built.findings.length
      ? built.findings.map((x) => el("div", { class: "finding error" }, el("span", { class: "reason" }, x.source === "webhook" ? "Webhook" : "Admission"), x.message))
      : el("p", { class: "note" }, "The API server admits this run.")),
    card(`${req.name}.yaml`, el("div", { class: "row" },
      el("button", { class: "primary", onclick: () => download(`${req.name}.yaml`, built.yaml) }, "Download YAML"),
      el("button", { class: "secondary", onclick: () => navigator.clipboard?.writeText(built.yaml) }, "Copy")),
    el("p", { class: "note" }, "The customer applies it with kubectl apply -f. The run inspects the workers and writes a ClusterDeploymentConfig in Draft; nothing is deployed until the draft is approved."),
    pre(built.yaml)));

  return el("div", {},
    el("div", { class: "grid" },
      card("Run", el("div", { class: "form" },
        el("label", {}, "Name"), text(req, "name"),
        el("label", {}, "Namespace"), text(req, "namespace"),
        el("label", {}, "Draft name"), text(d, "configName", "discovered-<run>"),
        el("label", {}, "Grow existing cluster"), text(d, "clusterRef", "empty: the draft creates a cluster")),
      state.analysis ? el("p", { class: "note" }, `Started from the settings of ${state.analysis.run?.name || "the bundle's run"}.`) : null),
      card("Which workers", el("div", { class: "form" },
        el("label", {}, "Workers"), el("div", {}, list(d, "workers", "one node name per line; empty inspects every schedulable worker"), workerHint),
        el("label", {}, "Node selector"), selector,
        el("label", {}, "Control-plane nodes"), tri(d, "enableControlPlaneNodes"),
        el("label", {}, "Failure domain label"), text(d, "failureDomainLabel", "topology.kubernetes.io/zone"),
        el("label", {}, "Tolerations"), tolerations))),
    el("div", { class: "grid" },
      card("Networking", el("div", { class: "form" },
        el("label", {}, "Management attachment"), text(na, "management", "empty: host networking"),
        el("label", {}, "Data attachment"), text(na, "data", "empty: data uses the management attachment")),
      el("p", { class: "note" }, "NetworkAttachmentDefinitions in the run's namespace. A cluster without Multus keeps host networking."))),
    el("div", { class: "grid" },
      card("Devices", el("div", { class: "form" },
        el("label", {}, "Logical block devices"), tri(d, "enableLogicalBlockDevices"),
        el("label", {}, "Partitioned devices"), tri(d, "enablePartitionedDevices"),
        el("label", {}, "Force journal device"), tri(d, "forceJournalDevice"),
        el("label", {}, "Size range"), text(f, "driveSizeRange", "for example 500G-4T"))),
      card("Device filter", el("div", { class: "form" },
        el("label", {}, "PCI allow list"), list(f, "pcieAllowList", "PCI addresses, NVMe mode"),
        el("label", {}, "PCI deny list"), list(f, "pcieDenyList", "PCI addresses, NVMe mode"),
        el("label", {}, "PCI model"), text(f, "pcieModel", "part of the model name, NVMe mode"),
        el("label", {}, "Block allow list"), list(f, "blockAllowList", "/dev/... paths, block mode"),
        el("label", {}, "Block deny list"), list(f, "blockDenyList", "/dev/... paths, block mode")))),
    el("div", { class: "row" }, el("button", { class: "primary", onclick: build }, "Build run"),
      el("button", { class: "secondary", onclick: () => { state.ops = null; render(); } }, "Start over")),
    el("div", { style: "margin-top:16px" }, result));
}

// ─── Editor ────────────────────────────────────────────────────────────────

// attachmentNames is the NetworkAttachmentDefinitions a worker's report holds.
function attachmentNames(w) {
  return (w.report?.networkAttachments?.definitions || []).map((d) => d.name);
}

// attachToggle switches a worker from host interfaces to network attachments,
// offered when its report holds a definition. A group names one or the other.
function attachToggle(w, choice) {
  const names = attachmentNames(w);
  if (!names.length) return null;
  return el("button", { class: "secondary", title: "Attach this worker's storage node through a NetworkAttachmentDefinition instead", onclick: () => {
    Object.assign(choice, { networkAttachment: { management: names[0] }, mgmtInterface: "", dataInterfaces: [] });
    render();
  } }, "Use attachment");
}

// attachmentSelect chooses one of a worker's definitions for a plane. Data may
// be empty, which sends it through the management attachment, and clearing
// management returns the worker to host interfaces.
function attachmentSelect(w, choice, plane) {
  const names = attachmentNames(w);
  const current = choice.networkAttachment[plane] || "";
  const options = [...new Set(["", ...names, current])];
  const empty = plane === "management" ? "host interfaces" : "the management attachment";
  return el("select", { onchange: (e) => {
    if (plane === "management" && !e.target.value) { delete choice.networkAttachment; render(); return; }
    choice.networkAttachment[plane] = e.target.value || undefined;
  } }, options.map((n) => el("option", { value: n, selected: current === n }, n || empty)));
}

// currentRequest is what the editor has chosen, as the API takes it.
function currentRequest() {
  const { req } = state.edit;
  return { ...req, workers: Object.values(state.edit.byWorker).filter((w) => state.edit.included.has(w.name)) };
}

// journalSelect offers the journal device. Force exists for logical block
// devices: a document has no field for it and writes the device on, and the
// editor stops warning that the worker's smallest devices tie.
function journalSelect(c, mode) {
  const value = c.forceJournalDevice ? "force" : String(c.enableJournalDevice ?? "");
  const options = [["", "unset"], ["true", "on"], ["false", "off"]];
  if (mode === "block") options.push(["force", "force (accept tied smallest devices)"]);
  return el("select", { onchange: (e) => {
    const v = e.target.value;
    c.forceJournalDevice = v === "force";
    c.enableJournalDevice = v === "" ? null : v !== "false";
    refreshStripes();
  } }, options.map(([v, l]) => el("option", { value: v, selected: value === v }, l)));
}

// stripeSelect offers the supported schemes, the ones the chosen workers and
// devices do not carry disabled with the operator's reason. refreshStripes
// fills it from the server whenever a choice it depends on changes.
function stripeSelect(c) {
  const select = el("select", { id: "stripe", onchange: (e) => {
    const [d, p] = e.target.value.split("+").map(Number);
    c.dataChunks = d; c.parityChunks = p;
  } }, el("option", { value: "" }, `${c.dataChunks ?? "?"}+${c.parityChunks ?? "?"}`));
  queueMicrotask(refreshStripes);
  return el("div", {}, select, el("div", { id: "stripe-note", class: "note" }));
}

let stripeTimer = null;
function refreshStripes() {
  clearTimeout(stripeTimer);
  stripeTimer = setTimeout(async () => {
    const select = document.getElementById("stripe");
    const note = document.getElementById("stripe-note");
    if (!select || !state.edit?.req) return;
    const c = state.edit.req.cluster;
    const current = `${c.dataChunks}+${c.parityChunks}`;
    try {
      const out = await api("POST", `/api/bundles/${state.id}/cdc/stripes`, currentRequest());
      const options = out.options || [];
      select.replaceChildren(...options.map((o) => el("option", {
        value: o.name, selected: o.name === current, disabled: !o.valid,
        title: (o.reasons || []).join("\n"),
      }, o.valid ? o.name : `${o.name} (not carried)`)));
      if (!options.some((o) => o.name === current)) select.prepend(el("option", { value: current, selected: true }, current));
      const chosen = options.find((o) => o.name === current);
      note.textContent = out.error ? out.error
        : chosen && !chosen.valid ? (chosen.reasons || []).join(" ")
          : `${options.filter((o) => o.valid).length} of ${options.length} schemes fit the chosen workers and devices.`;
    } catch (e) {
      note.textContent = e.message;
    }
  }, 150);
}

function editor() {
  if (!state.edit) {
    Promise.all([api("GET", `/api/bundles/${state.id}/cdc/prefill`), state.meta || api("GET", "/api/meta")]).then(([req, meta]) => {
      state.meta = meta;
      // A list the server or a document leaves out is an empty one here.
      req.workers = (req.workers || []).map((w) => ({ ...w, devices: w.devices || [], dataInterfaces: w.dataInterfaces || [] }));
      req.cluster = req.cluster || {};
      state.edit = { req, included: new Set(req.workers.map((w) => w.name)), byWorker: Object.fromEntries(req.workers.map((w) => [w.name, w])) };
      render();
    }).catch((e) => { state.edit = { error: e.message }; render(); });
    return card(null, el("p", {}, "Preparing the editor…"));
  }
  if (state.edit.error) return el("div", { class: "error-banner" }, state.edit.error);
  const { req } = state.edit;
  const workersWithReports = state.analysis.workers.filter((w) => w.report);

  const text = (obj, key, attrs) => el("input", { type: "text", value: obj[key] ?? "", ...attrs, oninput: (e) => { obj[key] = e.target.value; } });
  const num = (obj, key) => el("input", { type: "number", min: 0, value: obj[key] ?? "", oninput: (e) => { obj[key] = e.target.value === "" ? null : Number(e.target.value); } });
  const tri = (obj, key) => el("select", { onchange: (e) => { obj[key] = e.target.value === "" ? null : e.target.value === "true"; } },
    [["", "unset"], ["true", "on"], ["false", "off"]].map(([v, l]) => el("option", { value: v, selected: String(obj[key] ?? "") === v }, l)));
  const pick = (obj, key, options) => el("select", { onchange: (e) => { obj[key] = e.target.value; } },
    options.map(([v, l]) => el("option", { value: v, selected: (obj[key] ?? "") === v }, l)));

  const docCard = card("Document", el("div", { class: "form" },
    el("label", {}, "Name"), text(req, "name"),
    el("label", {}, "Namespace"), text(req, "namespace"),
    el("label", {}, "Device mode"), el("select", { onchange: (e) => {
      req.mode = e.target.value;
      for (const w of Object.values(state.edit.byWorker)) { w.devices = []; w.mode = req.mode; }
      if (req.mode !== "block") req.cluster.forceJournalDevice = false;
      render();
    } },
      [["nvme", "NVMe (PCI addresses)"], ["block", "Logical block devices (paths)"]].map(([v, l]) => el("option", { value: v, selected: req.mode === v }, l))),
    el("label", {}, "Grow existing cluster"), text(req, "clusterRef", { placeholder: "empty: the document creates a cluster", onchange: () => render() }),
    el("label", {}, "Environment"), el("select", { onchange: (e) => {
      req.environment = e.target.value;
      // Only a Vanilla document states the node flags: every other environment
      // decides them, and the schema refuses them there.
      if (req.environment !== "Vanilla") { delete req.cluster.enableCpuTopology; delete req.cluster.enableKubeletConfiguration; }
      render();
    } }, ["", ...(state.meta?.environments || [])].map((v) => el("option", { value: v, selected: (req.environment || "") === v }, v || "unset")))),
  req.fromCDC ? el("p", { class: "note" }, "Built on the bundle's ClusterDeploymentConfig: the fields this editor does not show are kept.") : null);

  const c = req.cluster;
  // Growing a supercluster creates its new members, whose sizing the document
  // states; growing an ordinary cluster states none.
  const growsSupercluster = (state.analysis.cluster.storageClusters || []).some((sc) => sc.name === req.clusterRef && sc.role === "Supercluster");
  const clusterCard = card(growsSupercluster ? "New members' cluster settings" : "Cluster settings",
    req.clusterRef && !growsSupercluster ? el("p", { class: "note" }, "A document that grows a cluster states no cluster settings.") : el("div", { class: "form" },
    el("label", {}, "Cluster name"), text(c, "name"),
    el("label", {}, "Stripe (data + parity)"), stripeSelect(c),
    el("label", {}, "Cluster mode"), el("select", { onchange: (e) => { c.clusterMode = e.target.value; refreshStripes(); } },
      [["", "Standard (default)"], ["Standard", "Standard"], ["Dual", "Dual (two nodes)"], ["Single", "Single (one node)"]]
        .map(([v, l]) => el("option", { value: v, selected: (c.clusterMode ?? "") === v }, l))),
    el("label", {}, "vCPU count"), num(c, "vcpuCount"),
    el("label", {}, "Max subsystem count"), num(c, "maxSubsystemCount"),
    el("label", {}, "Min huge pages size"), text(c, "minHugePagesSize", { placeholder: "for example 16G" }),
    el("label", {}, "Nodes per socket"), num(c, "nodesPerSocket"),
    el("label", {}, "Sockets to use"), el("input", { type: "text", value: (c.socketsToUse || []).join(","), placeholder: "for example 0,1",
      oninput: (e) => { c.socketsToUse = e.target.value.split(",").map((s) => s.trim()).filter(Boolean); } }),
    el("label", {}, "Format drives"), tri(c, "enableDriveFormat"),
    el("label", {}, "Journal device"), journalSelect(c, req.mode),
    el("label", {}, "Failure domains"), tri(c, "enableFailureDomains"),
    el("label", {}, "Checksum validation"), tri(c, "enableChecksumValidation"),
    el("label", {}, "4K atomicity"), tri(c, "enableAtomicity4K"),
    ...(req.environment === "Vanilla"
      ? [el("label", {}, "CPU topology"), tri(c, "enableCpuTopology"),
        el("label", {}, "Kubelet configuration"), tri(c, "enableKubeletConfiguration")]
      : [el("label", {}, "CPU topology, kubelet"), el("span", { class: "note" },
        req.environment ? `decided by ${req.environment}` : "decided by the environment; choose Vanilla to state them")])));

  const rows = workersWithReports.map((w) => {
    let choice = state.edit.byWorker[w.name];
    if (!choice) {
      choice = state.edit.byWorker[w.name] = { name: w.name, nodeSet: "discovered", failureDomain: w.node.labels?.["topology.kubernetes.io/zone"] || "",
        mgmtInterface: w.suggested.management || "", dataInterfaces: [...(w.suggested.data || [])], devices: [] };
    }
    const included = state.edit.included.has(w.name);
    // The links that could carry storage traffic, and whatever is already
    // chosen, so a document naming something else still shows it.
    const kinds = ["physical", "bond", "team", "vlan", "macvlan", "ipvlan"];
    const ifaces = (w.report.interfaces || [])
      .filter((i) => (kinds.includes(i.kind) && !i.loopback && !i.peered && (i.addresses || []).length)
        || i.name === choice.mgmtInterface || (choice.dataInterfaces || []).includes(i.name))
      .map((i) => i.name);
    // A worker's own mode names its devices: a document may name NVMe devices
    // in one node set and block devices in another.
    const mode = choice.mode || req.mode;
    choice.devices = choice.devices || [];
    const deviceChecks = (w.devices || []).filter((d) => (mode === "nvme" ? d.nvmeID : d.blockID) && d.kind === "Disk" && (mode === "block" || d.transport === "NVMe"))
      .map((d) => {
        const id = mode === "nvme" ? d.nvmeID : d.blockID;
        return el("label", { class: d.available ? "" : "refused" },
          el("input", { type: "checkbox", checked: choice.devices.includes(id), onchange: (e) => {
            choice.devices = e.target.checked ? [...choice.devices, id] : choice.devices.filter((x) => x !== id);
            refreshStripes();
          } }),
          el("span", {}, el("code", {}, id), ` ${bytes(d.sizeBytes)} ${d.model || ""} `,
            d.available ? "" : badge((d.rejections || []).map((r) => r.reason).join(", ") || "refused", "err")));
      });
    return [
      el("label", { class: "row" }, el("input", { type: "checkbox", checked: included, onchange: (e) => {
        if (e.target.checked) state.edit.included.add(w.name); else state.edit.included.delete(w.name);
        refreshStripes();
      } }), el("strong", {}, w.name)),
      text(choice, "nodeSet"),
      text(choice, "failureDomain", { placeholder: "none" }),
      choice.networkAttachment ? attachmentSelect(w, choice, "management") : el("div", {},
        el("select", { onchange: (e) => { choice.mgmtInterface = e.target.value; } },
          ["", ...ifaces].map((i) => el("option", { value: i, selected: choice.mgmtInterface === i }, i || "none"))),
        attachToggle(w, choice)),
      choice.networkAttachment ? attachmentSelect(w, choice, "data") :
      el("div", { class: "checks" }, ifaces.map((i) => el("label", {}, el("input", { type: "checkbox", checked: (choice.dataInterfaces || []).includes(i), onchange: (e) => {
        choice.dataInterfaces = e.target.checked ? [...(choice.dataInterfaces || []), i] : (choice.dataInterfaces || []).filter((x) => x !== i);
      } }), i))),
      el("div", { class: "checks" }, deviceChecks.length ? deviceChecks : el("span", { class: "muted" }, "no device in this mode")),
      el("button", { class: "secondary", title: "Give every included worker this worker's interfaces and node set", onclick: () => {
        for (const name of state.edit.included) {
          const other = state.edit.byWorker[name];
          if (!other || other === choice) continue;
          Object.assign(other, { nodeSet: choice.nodeSet, mgmtInterface: choice.mgmtInterface, dataInterfaces: [...(choice.dataInterfaces || [])],
            networkAttachment: choice.networkAttachment ? { ...choice.networkAttachment } : undefined });
        }
        render();
      } }, "Copy to all"),
    ];
  });

  // A worker the document names and the bundle has no report for keeps the
  // choices the document made, since there is nothing to choose from.
  const reported = new Set(workersWithReports.map((w) => w.name));
  for (const name of Object.keys(state.edit.byWorker).filter((n) => !reported.has(n))) {
    const choice = state.edit.byWorker[name];
    rows.push([
      el("label", { class: "row" }, el("input", { type: "checkbox", checked: state.edit.included.has(name), onchange: (e) => {
        if (e.target.checked) state.edit.included.add(name); else state.edit.included.delete(name);
      } }), el("strong", {}, name), badge("no report", "warn")),
      choice.nodeSet, choice.failureDomain || "",
      choice.networkAttachment ? choice.networkAttachment.management : choice.mgmtInterface || "",
      choice.networkAttachment ? choice.networkAttachment.data || "" : (choice.dataInterfaces || []).join(", "),
      el("span", { class: "muted" }, (choice.devices || []).join(", ")), "",
    ]);
  }

  const build = async () => {
    const body = currentRequest();
    try {
      state.built = await api("POST", `/api/bundles/${state.id}/cdc`, body);
      state.built.error = null;
    } catch (e) {
      state.built = { error: e.message };
    }
    render();
  };

  const result = state.built;
  const resultCard = !result ? null : result.error
    ? el("div", { class: "error-banner" }, result.error)
    : el("div", {},
      card("Findings", findingsOf(result.findings)),
      card("Layout", nodeSetsOf(result.nodeSets)),
      card(`${req.name}.yaml`, el("div", { class: "row" },
        el("button", { class: "primary", onclick: () => download(`${req.name}.yaml`, result.yaml) }, "Download YAML"),
        el("button", { class: "secondary", onclick: () => navigator.clipboard?.writeText(result.yaml) }, "Copy")),
      el("p", { class: "note" }, "Apply it with kubectl apply -f, review it, then set spec.approved: true. The document is never approved here."),
      pre(result.yaml)));

  return el("div", {},
    el("div", { class: "grid" }, docCard, clusterCard),
    card("Workers", el("p", { class: "note" }, "Workers with the same node set, devices, interfaces or network attachments, and failure domain share one group in the document."),
      table(["Worker", "Node set", "Failure domain", "Management", "Data", "Devices", ""], rows)),
    el("div", { class: "row" }, el("button", { class: "primary", onclick: build }, "Build document"),
      el("button", { class: "secondary", onclick: () => { state.edit = null; state.built = null; render(); } }, "Start over")),
    el("div", { style: "margin-top:16px" }, resultCard));
}
