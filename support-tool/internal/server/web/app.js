// The support tool's page: uploads a bundle, renders the server's analysis into
// five views, and drives the ClusterDeploymentConfig editor.
//
// Every value shown comes from a customer's cluster, so nothing is ever set as
// HTML: el() builds elements and sets text with textContent only.

"use strict";

const state = { id: null, analysis: null, tab: "overview", worker: null, edit: null, built: null };

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
  document.getElementById("tabs").hidden = false;
  render();
}

// A URL naming a bundle the server still holds opens it.
(async () => {
  const id = new URLSearchParams(location.search).get("bundle");
  if (!id) return;
  try {
    state.analysis = await api("GET", `/api/bundles/${encodeURIComponent(id)}`);
    state.id = id;
    const tab = location.hash.slice(1);
    if (["overview", "workers", "cluster", "cdc", "editor"].includes(tab)) state.tab = tab;
    show(state.analysis.bundle);
  } catch (e) {
    history.replaceState(null, "", location.pathname);
  }
})();

function render() {
  history.replaceState(null, "", `?bundle=${encodeURIComponent(state.id)}#${state.tab}`);
  for (const b of document.querySelectorAll("#tabs button")) b.classList.toggle("active", b.dataset.tab === state.tab);
  const views = { overview, workers, cluster, cdc, editor };
  document.getElementById("view").replaceChildren(views[state.tab]());
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
    table(["Device", "Kind", "Transport", "Size", "Model", "NUMA", "CDC name", "Status"],
      devices.map((d) => [
        el("span", {}, el("strong", {}, d.name), el("br"), el("span", { class: "muted" }, d.stablePath || d.path)),
        d.kind, d.transport || "", bytes(d.sizeBytes), d.model || "", d.numaNode >= 0 ? d.numaNode : "",
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
        el("code", {}, c.address), c.driver || badge("none", "muted"), c.vendor || "", c.product || "", c.numaNode >= 0 ? c.numaNode : "",
        c.inUse === undefined ? badge("unknown", "muted") : c.inUse ? badge("held", "err") : badge("free", "ok")]))
      : el("p", { class: "note" }, "None."),
    (r.reclaimed || []).length ? [el("h3", {}, "Returned to the kernel by the probe"), pre(r.reclaimed.map((c) => c.address).join("\n"))] : null)));
  if ((r.unreadable || []).length) out.push(card("Readings the probe could not take", el("ul", {}, r.unreadable.map((u) => el("li", {}, u)))));
  return el("div", {}, out);
}

// ─── Cluster ───────────────────────────────────────────────────────────────

function cluster() {
  const c = state.analysis.cluster;
  const objects = (list, extra) => (list || []).length
    ? table(["Name", "Phase", ...extra.map((e) => e[0])], list.map((o) => [el("strong", {}, o.name), o.phase || "", ...extra.map((e) => o[e[1]] || "")]))
    : el("p", { class: "note" }, "None in the bundle.");
  return el("div", {},
    card("Control planes", objects(c.controlPlanes, [])),
    card("Storage clusters", objects(c.storageClusters, [])),
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
      el("strong", {}, g.name), g.workers.join(", "), el("span", {}, `${g.class} × ${g.devices.length}`, el("br"), el("span", { class: "muted" }, g.devices.join(", "))),
      g.mgmtInterface || "", (g.dataInterfaces || []).join(", "), g.failureDomain || ""]))));
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

// ─── Editor ────────────────────────────────────────────────────────────────

function editor() {
  if (!state.edit) {
    api("GET", `/api/bundles/${state.id}/cdc/prefill`).then((req) => {
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
    el("label", {}, "Device mode"), el("select", { onchange: (e) => { req.mode = e.target.value; for (const w of Object.values(state.edit.byWorker)) w.devices = []; render(); } },
      [["nvme", "NVMe (PCI addresses)"], ["block", "Logical block devices (paths)"]].map(([v, l]) => el("option", { value: v, selected: req.mode === v }, l))),
    el("label", {}, "Grow existing cluster"), text(req, "clusterRef", { placeholder: "empty: the document creates a cluster" }),
    el("label", {}, "Environment"), text(req, "environment")),
  req.fromCDC ? el("p", { class: "note" }, "Built on the bundle's ClusterDeploymentConfig: the fields this editor does not show are kept.") : null);

  const c = req.cluster;
  const clusterCard = card("Cluster settings", req.clusterRef ? el("p", { class: "note" }, "A document that grows a cluster states no cluster settings.") : el("div", { class: "form" },
    el("label", {}, "Cluster name"), text(c, "name"),
    el("label", {}, "Stripe (data + parity)"), el("div", { class: "row" }, num(c, "dataChunks"), "+", num(c, "parityChunks")),
    el("label", {}, "Cluster mode"), pick(c, "clusterMode", [["", "Standard (default)"], ["Standard", "Standard"], ["Dual", "Dual (two nodes)"], ["Single", "Single (one node)"]]),
    el("label", {}, "vCPU count"), num(c, "vcpuCount"),
    el("label", {}, "Max subsystem count"), num(c, "maxSubsystemCount"),
    el("label", {}, "Min huge pages size"), text(c, "minHugePagesSize", { placeholder: "for example 16G" }),
    el("label", {}, "Nodes per socket"), num(c, "nodesPerSocket"),
    el("label", {}, "Sockets to use"), el("input", { type: "text", value: (c.socketsToUse || []).join(","), placeholder: "for example 0,1",
      oninput: (e) => { c.socketsToUse = e.target.value.split(",").map((s) => s.trim()).filter(Boolean); } }),
    el("label", {}, "Format drives"), tri(c, "enableDriveFormat"),
    el("label", {}, "Journal device"), tri(c, "enableJournalDevice"),
    el("label", {}, "Failure domains"), tri(c, "enableFailureDomains"),
    el("label", {}, "Checksum validation"), tri(c, "enableChecksumValidation"),
    el("label", {}, "4K atomicity"), tri(c, "enableAtomicity4K")));

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
    const deviceChecks = (w.devices || []).filter((d) => (req.mode === "nvme" ? d.nvmeID : d.blockID) && d.kind === "Disk" && (req.mode === "block" || d.transport === "NVMe"))
      .map((d) => {
        const id = req.mode === "nvme" ? d.nvmeID : d.blockID;
        return el("label", { class: d.available ? "" : "refused" },
          el("input", { type: "checkbox", checked: choice.devices.includes(id), onchange: (e) => {
            choice.devices = e.target.checked ? [...choice.devices, id] : choice.devices.filter((x) => x !== id);
          } }),
          el("span", {}, el("code", {}, id), ` ${bytes(d.sizeBytes)} ${d.model || ""} `,
            d.available ? "" : badge((d.rejections || []).map((r) => r.reason).join(", ") || "refused", "err")));
      });
    return [
      el("label", { class: "row" }, el("input", { type: "checkbox", checked: included, onchange: (e) => {
        if (e.target.checked) state.edit.included.add(w.name); else state.edit.included.delete(w.name);
      } }), el("strong", {}, w.name)),
      text(choice, "nodeSet"),
      text(choice, "failureDomain", { placeholder: "none" }),
      el("select", { onchange: (e) => { choice.mgmtInterface = e.target.value; } },
        ["", ...ifaces].map((i) => el("option", { value: i, selected: choice.mgmtInterface === i }, i || "none"))),
      el("div", { class: "checks" }, ifaces.map((i) => el("label", {}, el("input", { type: "checkbox", checked: (choice.dataInterfaces || []).includes(i), onchange: (e) => {
        choice.dataInterfaces = e.target.checked ? [...(choice.dataInterfaces || []), i] : (choice.dataInterfaces || []).filter((x) => x !== i);
      } }), i))),
      el("div", { class: "checks" }, deviceChecks.length ? deviceChecks : el("span", { class: "muted" }, "no device in this mode")),
      el("button", { class: "secondary", title: "Give every included worker this worker's interfaces and node set", onclick: () => {
        for (const name of state.edit.included) {
          const other = state.edit.byWorker[name];
          if (!other || other === choice) continue;
          Object.assign(other, { nodeSet: choice.nodeSet, mgmtInterface: choice.mgmtInterface, dataInterfaces: [...(choice.dataInterfaces || [])] });
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
      choice.nodeSet, choice.failureDomain || "", choice.mgmtInterface || "", (choice.dataInterfaces || []).join(", "),
      el("span", { class: "muted" }, (choice.devices || []).join(", ")), "",
    ]);
  }

  const build = async () => {
    const body = { ...req, workers: Object.values(state.edit.byWorker).filter((w) => state.edit.included.has(w.name)) };
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
    card("Workers", el("p", { class: "note" }, "Workers with the same node set, devices, interfaces, and failure domain share one group in the document."),
      table(["Worker", "Node set", "Failure domain", "Management", "Data interfaces", "Devices", ""], rows)),
    el("div", { class: "row" }, el("button", { class: "primary", onclick: build }, "Build document"),
      el("button", { class: "secondary", onclick: () => { state.edit = null; state.built = null; render(); } }, "Start over")),
    el("div", { style: "margin-top:16px" }, resultCard));
}
