// ---------------------------------------------------------------------------
// What the DR hub knows about its sites, for the forms: values are proposed
// and offered from it, validated against it, and typed by hand only behind an
// explicit override (2026-10-05: a tier selector typed as a bare key selected
// nothing; Gitea was protected with WordPress's PVC selector; a site profile
// named a DHCP server that does not exist).
//
// Sources, all read through the hub's API server:
//   - ManagedClusters: the clusters a plan can name, their availability, and
//     region/zone claims and labels;
//   - SiteProfiles: each cluster's inventory (zones, nodes, NADs, classes);
//   - dr-agent's status, through the ManagedClusterView dr-hub keeps of it
//     (<cluster>/dr-agent-status): Velero's namespace, storage classes with
//     labels, application namespaces with workloads, Services and PVCs, VMs
//     with their addresses per NAD, and in-cluster DHCP servers.
// Every helper below is pure: the forms and the smokes call them alike.
// ---------------------------------------------------------------------------

const AGENT_VIEW = "dr-agent-status";
// The replicated StorageClass a plan selects when nothing else is said: the
// label the hub's StorageClass delivery stamps (storageProfile.provision).
const DEFAULT_SC_SELECTOR = {"simplyblock.io/replicated": "true"};
const DEFAULT_SC_NAME = "simplyblock-dr";
const DNS_LABEL_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const TEST_ID_RE = /^[a-z0-9]{4,12}$/;
const DURATION_RE = /^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$/;
const QUANTITY_RE = /^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$/;
const REGION_KEYS = ["region.open-cluster-management.io", "topology.kubernetes.io/region", "region"];
const ZONE_KEYS = ["topology.kubernetes.io/zone", "zone"];

// The agent's status as the view holds it, null when not fetched yet.
const agentStatusOf = view => {
  const r = view && view.status && view.status.result;
  const raw = r && r.data && r.data["status.json"];
  if (!raw) return null;
  try { return JSON.parse(raw); } catch (e) { return null; }
};
const uniqSorted = xs => [...new Set(xs.filter(Boolean))].sort();
const claimOf = (mc, keys) => {
  const claims = ((mc.status || {}).clusterClaims || []);
  for (const k of keys) {
    const c = claims.find(x => x.name === k);
    if (c && c.value) return c.value;
    const l = ((mc.metadata || {}).labels || {})[k];
    if (l) return l;
  }
  return "";
};

// One cluster as the forms see it.
function discoveredCluster(name, mc, profile, st) {
  const inv = (profile && profile.inventory) || {};
  const site = (st && st.inventory && st.inventory.site) || {};
  const nodes = site.nodes || inv.nodes || [];
  const avail = mc ? (((mc.status || {}).conditions || []).find(c => c.type === "ManagedClusterConditionAvailable") || {}).status === "True" : null;
  const zoneClaim = mc ? claimOf(mc, ZONE_KEYS) : "", regionClaim = mc ? claimOf(mc, REGION_KEYS) : "";
  return {
    name, managed: !!mc, available: avail, reported: !!st, heartbeat: st ? st.heartbeat : null,
    zones: uniqSorted((site.zones || inv.zones || []).concat(nodes.map(n => n.zone), zoneClaim)),
    regions: uniqSorted((site.regions || inv.regions || []).concat(nodes.map(n => n.region), regionClaim)),
    velero: st ? st.veleroNamespace || "" : null,
    storageClasses: (st && st.inventory && st.inventory.storageClasses) || inv.storageClasses || [],
    namespaces: (st && st.workloads) || null,
    nads: site.nads || inv.nads || [],
    nodes,
    vms: (st && st.virtualMachines) || [],
    dhcpServers: (st && st.dhcpServers) || [],
    profile: profile || null
  };
}

// Everything the hub knows, by cluster name. A view that cannot be read (no
// agent yet, or a console without the grant) leaves that cluster unreported:
// the forms then say so and fall back to typing behind the override.
async function loadDiscovery() {
  const [mcs, profiles] = await Promise.all([
    k8s.list("ManagedCluster").catch(() => []),
    k8s.list("SiteProfile").then(xs => xs.map(o => ({name: o.metadata.name, inventory: (o.status || {}).inventory || {}, spec: o.spec || {}}))).catch(() => [])]);
  const names = uniqSorted(mcs.map(m => m.metadata.name).concat(profiles.map(p => p.name)));
  const statuses = await Promise.all(names.map(n => k8s.get("ManagedClusterView", AGENT_VIEW, {namespace: n}).then(agentStatusOf).catch(() => null)));
  const clusters = names.map((n, i) => discoveredCluster(n, mcs.find(m => m.metadata.name === n), profiles.find(p => p.name === n), statuses[i]));
  const disc = {clusters, byName: Object.fromEntries(clusters.map(c => [c.name, c])), loadedAt: Date.now()};
  window.__lastDisc = disc;
  return disc;
}
const discOf = (disc, cluster) => (disc && disc.byName && disc.byName[cluster]) || null;

// ---- sites ----------------------------------------------------------------------
const clusterOptions = disc => (disc ? disc.clusters : []).filter(c => c.managed || c.reported).map(c => ({v: c.name,
  l: `${c.name}${c.available === false ? " (unavailable)" : !c.reported ? " (no agent report)" : ""}`}));
const zoneOptions = (disc, cluster) => ((discOf(disc, cluster) || {}).zones || []).map(z => ({v: z, l: z}));
const regionOptions = (disc, cluster) => ((discOf(disc, cluster) || {}).regions || []).map(z => ({v: z, l: z}));
// A site row checked against what the cluster reports.
const siteRowError = (disc, r) => {
  if (!disc || !r.cluster) return null;
  const c = discOf(disc, r.cluster);
  if (!c) return `${r.cluster} is not a managed cluster of this hub.`;
  if (r.zone && c.zones.length && !c.zones.includes(r.zone)) return `${r.cluster} has no nodes in zone ${r.zone} (zones: ${c.zones.join(", ")}).`;
  return null;
};

// ---- Velero ------------------------------------------------------------------------
// The namespace the sites' agents found Velero in: the plan's value when they
// agree, and per site when they do not.
function veleroProposal(disc, clusters) {
  const per = {};
  const missing = [];
  (clusters || []).filter(Boolean).forEach(c => {
    const d = discOf(disc, c);
    if (d && d.velero) per[c] = d.velero; else missing.push(c);
  });
  const values = uniqSorted(Object.values(per));
  const count = v => Object.values(per).filter(x => x === v).length;
  const value = values.sort((a, b) => count(b) - count(a))[0] || "";
  return {value, perCluster: per, disagree: values.length > 1, missing};
}

// ---- storage classes ----------------------------------------------------------------
const labelsMatch = (labels, matchLabels) => Object.entries(matchLabels || {}).every(([k, v]) => (labels || {})[k] === v);
// The classes the selector matches on each cluster.
const scMatches = (disc, clusters, matchLabels) => Object.fromEntries((clusters || []).filter(Boolean).map(c => {
  const d = discOf(disc, c);
  return [c, d ? d.storageClasses.filter(s => Object.keys(matchLabels || {}).length && labelsMatch(s.labels, matchLabels)).map(s => s.name) : null];
}));

// ---- selectors ----------------------------------------------------------------------
// A selector as typed: "k=v, k2=v2", "key exists" for a key that must be set
// whatever its value. A bare key is refused: it used to become k="" and match
// only objects whose label is empty.
function parseSelector(text, opts) {
  const allowExists = !opts || opts.exists !== false;
  const matchLabels = {}, matchExpressions = [];
  const parts = String(text || "").split(",").map(s => s.trim()).filter(Boolean);
  for (const p of parts) {
    const ex = /^([A-Za-z0-9./_-]+)\s+exists$/i.exec(p);
    if (ex) {
      if (!allowExists) return {error: `"${p}": this selector takes key=value pairs only.`};
      matchExpressions.push({key: ex[1], operator: "Exists"});
      continue;
    }
    const i = p.indexOf("=");
    if (i < 0) return {error: `"${p}" has no value: write ${p}=<value>${allowExists ? `, or "${p} exists" to select every object that carries the label` : ""}.`};
    const k = p.slice(0, i).trim(), v = p.slice(i + 1).trim();
    if (!k) return {error: `"${p}" has no key.`};
    if (!v) return {error: `${k}= has an empty value: it matches only objects whose label ${k} is empty. Name the value${allowExists ? `, or write "${k} exists"` : ""}.`};
    matchLabels[k] = v;
  }
  return {matchLabels, matchExpressions, error: null};
}
const selectorText = sel => Object.entries((sel && sel.matchLabels) || {}).map(([k, v]) => `${k}=${v}`)
  .concat(((sel && sel.matchExpressions) || []).filter(e => e.operator === "Exists").map(e => `${e.key} exists`)).join(", ");
const selectorMatchesLabels = (sel, labels) => labelsMatch(labels, sel.matchLabels) &&
  (sel.matchExpressions || []).every(e => e.operator !== "Exists" || Object.prototype.hasOwnProperty.call(labels || {}, e.key));

const nsReport = (disc, cluster, ns) => (((discOf(disc, cluster) || {}).namespaces) || []).find(n => n.namespace === ns) || null;
// Namespaces that hold infrastructure, not an application: platform and
// DR-stack namespaces, a managed cluster's own namespace (OCM), and one that
// holds nothing but NetworkAttachmentDefinitions.
const INFRA_NS_RE = /^(kube-|cattle-|sitemap-|open-cluster-management|ramen|velero|simplyblock|dr-|openshift|local-path|kubevirt|cdi$|multus|calico|tigera|metallb|cert-manager|longhorn|fleet-|olm$|operators$)/;
const nsCounts = n => ({pvcs: (n.pvcs || []).length, workloads: (n.workloads || []).length});
function nsInfra(disc, cluster, n) {
  if (INFRA_NS_RE.test(n.namespace)) return true;
  if ((disc ? disc.clusters : []).some(c => c.name === n.namespace)) return true;
  const c = nsCounts(n);
  return !c.pvcs && !c.workloads && (((discOf(disc, cluster) || {}).nads) || []).some(x => x.namespace === n.namespace);
}
// The application namespaces a cluster reports: infrastructure hidden, the
// ones that hold something first, empty ones marked as such.
const namespaceOptions = (disc, cluster, site) => ((((discOf(disc, cluster) || {}).namespaces) || []))
  .filter(n => !nsInfra(disc, cluster, n))
  .map(n => Object.assign({n}, nsCounts(n)))
  .sort((a, b) => (b.pvcs + b.workloads > 0) - (a.pvcs + a.workloads > 0) || b.pvcs - a.pvcs || b.workloads - a.workloads || a.n.namespace.localeCompare(b.n.namespace))
  .map(({n, pvcs, workloads}) => ({v: n.namespace,
    l: pvcs + workloads === 0 ? `${n.namespace} — (empty on ${site || cluster})`
      : `${n.namespace} — ${pvcs} PVC${pvcs === 1 ? "" : "s"}, ${workloads} workload${workloads === 1 ? "" : "s"}${n.protected ? " (protected)" : ""}`}));
// Namespaces of these names that hold something on the other clusters: where
// an application is when the chosen source site's namespaces are empty.
function nsElsewhere(disc, cluster, namespaces) {
  return (disc ? disc.clusters : []).filter(c => c.name !== cluster).flatMap(c => (namespaces || []).map(ns => {
    const r = nsReport(disc, c.name, ns);
    const k = r ? nsCounts(r) : {pvcs: 0, workloads: 0};
    return {cluster: c.name, namespace: ns, pvcs: k.pvcs, workloads: k.workloads};
  })).filter(x => x.pvcs || x.workloads);
}
// The PVCs and workloads the namespaces hold on a cluster (null: not reported).
function nsHoldings(disc, cluster, namespaces) {
  if (!reported(disc, cluster, namespaces)) return null;
  return (namespaces || []).map(ns => nsCounts(nsReport(disc, cluster, ns))).reduce((a, c) => ({pvcs: a.pvcs + c.pvcs, workloads: a.workloads + c.workloads}), {pvcs: 0, workloads: 0});
}
// The label pairs of the PVCs in the namespaces, with how many each matches.
function pvcPairs(disc, cluster, namespaces) {
  const pvcs = (namespaces || []).flatMap(ns => ((nsReport(disc, cluster, ns) || {}).pvcs || []));
  const counts = {};
  pvcs.forEach(p => Object.entries(p.labels || {}).forEach(([k, v]) => { const key = `${k}=${v}`; counts[key] = (counts[key] || 0) + 1; }));
  return Object.entries(counts).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).map(([pair, count]) => ({pair, count}));
}
// The PVCs a selector matches, null when the namespaces are not reported.
function pvcMatches(disc, cluster, namespaces, sel) {
  const reps = (namespaces || []).map(ns => nsReport(disc, cluster, ns));
  if (!reps.length || reps.some(r => !r || !r.pvcs)) return null;
  return reps.flatMap(r => r.pvcs.filter(p => selectorMatchesLabels(sel || {}, p.labels)).map(p => `${r.namespace}/${p.name}`));
}

// Kinds a tier selects, as the hub's tiers-resolvable reads them.
const READY_KIND = {vmRunning: "VirtualMachine", deploymentsReady: "Deployment", statefulSetsReady: "StatefulSet", podsReady: "Pod"};
const RESOURCE_KIND = {deployments: "Deployment", statefulsets: "StatefulSet", virtualmachines: "VirtualMachine", pods: "Pod", persistentvolumeclaims: "PersistentVolumeClaim"};
const ALL_KINDS = ["Deployment", "StatefulSet", "VirtualMachine", "Pod", "PersistentVolumeClaim"];
function tierKindsOf(resourceTypes, readyTypes) {
  if ((resourceTypes || []).length) return {kinds: uniqSorted(resourceTypes.map(rt => RESOURCE_KIND[String(rt).toLowerCase().split(".")[0]])), known: true};
  const k = uniqSorted((readyTypes || []).map(t => READY_KIND[t]));
  return k.length ? {kinds: k, known: true} : {kinds: ALL_KINDS, known: false};
}
// The objects of the namespaces with the labels a tier (own labels) or a
// gate (pod template labels) selects them by.
function objectsOf(disc, cluster, namespaces, forPods) {
  const out = [];
  (namespaces || []).forEach(ns => {
    const r = nsReport(disc, cluster, ns);
    if (!r) return;
    (r.workloads || []).forEach(w => {
      if (forPods) { out.push({kind: "Pod", name: w.kind === "Pod" ? w.name : `${w.name} pods`, labels: w.labels || {}, ns}); return; }
      out.push({kind: w.kind, name: w.name, labels: (w.kind === "Pod" ? w.labels : w.objectLabels) || {}, ns});
      if (w.kind !== "Pod" && w.labels) out.push({kind: "Pod", name: `${w.name} pods`, labels: w.labels, ns});
    });
    if (!forPods) (r.pvcs || []).forEach(p => out.push({kind: "PersistentVolumeClaim", name: p.name, labels: p.labels || {}, ns}));
  });
  return out;
}
const reported = (disc, cluster, namespaces) => (namespaces || []).length > 0 && (namespaces || []).every(ns => !!nsReport(disc, cluster, ns));
// The label pairs of the objects, most common first, for a selector's suggestions.
const objectPairs = (objs, kinds) => {
  const counts = {};
  objs.filter(o => !kinds || kinds.includes(o.kind)).forEach(o => Object.entries(o.labels || {}).forEach(([k, v]) => { const key = `${k}=${v}`; counts[key] = (counts[key] || 0) + 1; }));
  return Object.entries(counts).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).map(([pair]) => pair);
};
// What a typed tier selector selects: the parse error, or the matching
// objects (null when the namespaces are not reported).
function tierSelectorCheck(disc, cluster, namespaces, text, readyTypes) {
  const sel = parseSelector(text);
  if (sel.error) return {error: sel.error};
  const {kinds, known} = tierKindsOf([], readyTypes);
  if (!reported(disc, cluster, namespaces)) return {sel, matches: null, kinds, known};
  const objs = objectsOf(disc, cluster, namespaces, false).filter(o => kinds.includes(o.kind));
  return {sel, kinds, known, matches: objs.filter(o => selectorMatchesLabels(sel, o.labels)), offered: objectPairs(objs)};
}
// An exec gate's selector selects pods by their template's labels.
function gateSelectorCheck(disc, cluster, namespaces, text) {
  const sel = parseSelector(text, {exists: false});
  if (sel.error) return {error: sel.error};
  if (!reported(disc, cluster, namespaces)) return {sel, matches: null};
  const pods = objectsOf(disc, cluster, namespaces, true);
  return {sel, matches: pods.filter(o => selectorMatchesLabels(sel, o.labels)), offered: objectPairs(pods)};
}

// ---- DR paths ---------------------------------------------------------------------
// Every ordered pair of the plan's sites, with what the hub can say about it:
// the method (the plan's), sync when both sites are zones of one cluster, and
// a test target when the target reports an isolated NAD.
const isolatedNadOf = (disc, cluster) => {
  const nads = ((discOf(disc, cluster) || {}).nads || []).map(n => `${n.namespace}/${n.name}`);
  return nads.find(n => /drtest|isolat|bubble/.test(n)) || nads.find(n => /test/.test(n)) || "";
};
function proposePaths(plan, disc, paths) {
  if (!plan) return [];
  const sites = plan.sites || [];
  const out = [];
  sites.forEach(a => sites.forEach(b => {
    if (a.name === b.name) return;
    const sync = a.cluster === b.cluster;
    const method = (plan.methods || []).find(m => sync === /^sync/.test(m.type)) || (plan.methods || [])[0] || null;
    const nad = isolatedNadOf(disc, b.cluster);
    const exists = (paths || []).find(p => (p.planName || p.planRef) === plan.name && p.from === a.name && p.to === b.name) || null;
    out.push({plan: plan.name, from: a.name, to: b.name, name: `${a.name}-to-${b.name}`.slice(0, 63), sync, method,
      actions: sync ? ["Failover", "Relocate"] : ["Failover", "Relocate"].concat(nad ? ["Test"] : []),
      nad, recentWithin: "720h", exists: exists ? exists.name : null});
  }));
  return out;
}
// Why a path cannot be declared, or null.
function pathError(plan, paths, v) {
  if (!plan) return "Choose the protection plan.";
  const names = (plan.sites || []).map(s => s.name);
  if (!names.includes(v.from)) return `${v.from || "The source"} is not a site of plan ${plan.name} (sites: ${names.join(", ")}).`;
  if (!names.includes(v.to)) return `${v.to || "The target"} is not a site of plan ${plan.name} (sites: ${names.join(", ")}).`;
  if (v.from === v.to) return "A path joins two different sites.";
  const dup = (paths || []).find(p => (p.planName || p.planRef) === plan.name && p.from === v.from && p.to === v.to);
  if (dup) return `${v.from} → ${v.to} is declared already, as ${dup.name}.`;
  if (!v.name || v.name.length > 63 || !DNS_LABEL_RE.test(v.name)) return "The path's name must be a DNS label: lower-case letters, digits and \"-\".";
  if ((paths || []).some(p => p.name === v.name)) return `A DR path named ${v.name} exists already.`;
  if ((v.actions || []).includes("Test")) {
    if (!v.nad || !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?\/[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(v.nad)) return "Test needs the target's isolated NAD as <namespace>/<name>.";
    if (v.cap && !QUANTITY_RE.test(v.cap)) return `${v.cap} is not a quantity (e.g. 500Gi, 2Ti).`;
    if (v.recent && !DURATION_RE.test(v.recent)) return `${v.recent} is not a duration (e.g. 168h, 720h).`;
  }
  return null;
}

// ---- test ids and bubble namespaces --------------------------------------------------
// The bubble namespace a production namespace recovers into, as dr-hub names it.
const bubbleNamespace = (ns, id) => {
  const suffix = `-drtest-${id}`;
  return ns.length + suffix.length <= 63 ? ns + suffix : `${ns.slice(0, 63 - suffix.length - 5).replace(/-+$/, "")}-…${suffix}`;
};
// A proposed id: the application's letters and the time, unique enough to
// read in a namespace list ("wp" + MMDDhhmm).
const proposeTestID = (name, now) => {
  const d = new Date(now || Date.now()), p = n => String(n).padStart(2, "0");
  const letters = String(name || "t").toLowerCase().replace(/[^a-z0-9]/g, "").slice(0, 4) || "t";
  return (letters + p(d.getUTCMonth() + 1) + p(d.getUTCDate()) + p(d.getUTCHours()) + p(d.getUTCMinutes())).slice(0, 12);
};
// Why a test id cannot be used, or null: its format, an id another test
// holds, or a bubble namespace that exists on the target already.
function testIDError(id, opts) {
  if (!TEST_ID_RE.test(id || "")) return "A test id is 4 to 12 lower-case letters and digits.";
  const taken = (opts.tests || []).find(t => t.testID === id);
  if (taken) return `Test ${taken.name} has the id ${id} already.`;
  const target = discOf(opts.disc, opts.cluster);
  const existing = target && target.namespaces ? target.namespaces.map(n => n.namespace) : [];
  const clash = (opts.namespaces || []).map(ns => bubbleNamespace(ns, id)).filter(b => existing.includes(b));
  if (clash.length) return `Namespace ${clash.join(", ")} exists on ${opts.cluster} already.`;
  return null;
}

// ---- networks (ADR 0020) ---------------------------------------------------------------
const ipToInt = ip => { const p = String(ip).split(".").map(Number); return p.length === 4 && p.every(n => n >= 0 && n < 256) ? ((p[0] << 24) >>> 0) + (p[1] << 16) + (p[2] << 8) + p[3] : null; };
const intToIp = n => [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join(".");
const parseCidr = c => { const m = /^(\d+\.\d+\.\d+\.\d+)\/(\d{1,2})$/.exec(String(c || "").trim()); if (!m) return null; const ip = ipToInt(m[1]), bits = Number(m[2]); if (ip === null || bits > 32) return null; const mask = bits ? (~0 << (32 - bits)) >>> 0 : 0; return {base: (ip & mask) >>> 0, bits, mask}; };
const cidrContains = (cidr, ip) => { const c = parseCidr(cidr), n = ipToInt(ip); return !!c && n !== null && ((n & c.mask) >>> 0) === c.base; };
const hostIdOf = (cidr, ip) => { const c = parseCidr(cidr), n = ipToInt(ip); return c && n !== null ? (n - c.base) >>> 0 : null; };
const v4 = ips => (ips || []).map(i => String(i).split("/")[0]).filter(i => ipToInt(i) !== null);
// The /24 every observed address shares, "" when they do not share one.
const inferCidr = ips => { const xs = v4(ips); if (!xs.length) return ""; const nets = uniqSorted(xs.map(i => intToIp((ipToInt(i) & 0xffffff00) >>> 0) + "/24")); return nets.length === 1 ? nets[0] : ""; };
const nadRef = n => `${n.namespace}/${n.name}`;
// The VMs' addresses on one NAD.
const nadAddresses = (disc, cluster, nad) => v4(((discOf(disc, cluster) || {}).vms || []).flatMap(vm => (vm.networks || []).filter(n => n.nad === nad).flatMap(n => n.ips || [])));
// The guest subnet of a NAD: its IPAM range, else inferred from its VMs.
function nadSubnet(disc, cluster, nad) {
  const n = ((discOf(disc, cluster) || {}).nads || []).find(x => nadRef(x) === nad);
  const ipam = n && (n.ipamRanges || []).find(r => parseCidr(r));
  if (ipam) return {cidr: ipam, source: "the NAD's IPAM"};
  const inf = inferCidr(nadAddresses(disc, cluster, nad));
  return inf ? {cidr: inf, source: "the VMs' addresses"} : {cidr: "", source: ""};
}
// The in-cluster DHCP servers on a NAD, with the bridge/VLAN match: a server
// on another NAD of the same bridge and VLAN serves the same segment.
function dhcpServersOn(disc, cluster, nad) {
  const d = discOf(disc, cluster);
  if (!d) return [];
  const seg = n => n ? `${n.bridge || n.master || ""}|${n.vlan || ""}` : "";
  const target = d.nads.find(x => nadRef(x) === nad);
  return d.dhcpServers.filter(s => (s.nads || []).some(sn => sn.nad === nad || (target && seg(d.nads.find(x => nadRef(x) === sn.nad)) === seg(target) && seg(target) !== "|")));
}
// The isolated test (bubble) networks of a site: the isolated NAD of every
// DRPath that tests on it, and the one the site itself reports.
function testNadsOf(disc, cluster, paths) {
  const own = isolatedNadOf(disc, cluster);
  return uniqSorted((paths || []).filter(p => p.to === cluster && p.test && p.test.isolatedNad).map(p => p.test.isolatedNad).concat(own ? [own] : []));
}
// A DHCP server that serves a test bubble's isolated network (its NAD is a
// test NAD, or shares one's bridge and VLAN): it must not be proposed for a
// guest network, whose reservations it would never answer.
function isBubbleServer(disc, cluster, server, testNads) {
  const d = discOf(disc, cluster);
  const seg = n => n ? `${n.bridge || n.master || ""}|${n.vlan || ""}` : "";
  const nadOf = ref => ((d && d.nads) || []).find(x => nadRef(x) === ref);
  const tests = (testNads || []).map(t => seg(nadOf(t))).filter(x => x && x !== "|");
  return (server.nads || []).some(sn => (testNads || []).includes(sn.nad) || tests.includes(seg(nadOf(sn.nad))) || /drtest|bubble|isolat/.test(sn.nad || ""));
}
// The DHCP server to propose for a site's guest networks: a registered one
// that serves one of the roles' NADs first, else an in-cluster server found on
// one of them that reads its hosts from a ConfigMap (it can be registered).
// Test-bubble servers are never proposed. Never a name nothing registered.
function proposeDHCPServer(disc, cluster, roleNads, servers, testNads) {
  const found = uniqSorted((roleNads || []).filter(Boolean)).flatMap(n => dhcpServersOn(disc, cluster, n))
    .filter((f, i, xs) => xs.indexOf(f) === i && !isBubbleServer(disc, cluster, f, testNads));
  const regOf = f => (servers || []).find(x => x.dnsmasq && x.dnsmasq.namespace === f.namespace && x.dnsmasq.configMap === f.hostsConfigMap);
  const reg = found.map(regOf).find(Boolean);
  if (reg) return {registered: reg.name, discovered: found.find(f => regOf(f) === reg) || null};
  return {registered: null, discovered: found.find(f => f.hostsConfigMap) || null};
}
// The reserved host ids a guest network proposes: the gateway's (.1) and
// every DHCP server's own on the segment.
function proposedReserved(disc, cluster, nad, cidr) {
  const ids = [1];
  dhcpServersOn(disc, cluster, nad).forEach(s => (s.nads || []).forEach(sn => v4(sn.ips).forEach(ip => { if (cidrContains(cidr, ip)) ids.push(hostIdOf(cidr, ip)); })));
  return [...new Set(ids)].sort((a, b) => a - b);
}
// A guest network row checked against what the site reports.
function guestRowError(disc, cluster, row, nad, servers) {
  if (row.cidr && !parseCidr(row.cidr)) return `${row.cidr} is not a CIDR (a.b.c.d/n).`;
  const c = parseCidr(row.cidr);
  if (c) {
    const outside = nadAddresses(disc, cluster, nad).filter(ip => !cidrContains(row.cidr, ip));
    if (outside.length) return `VMs on ${nad} have addresses outside ${row.cidr}: ${outside.slice(0, 4).join(", ")}.`;
    const ids = String(row.reservedHostIDs || "").split(/[,\s]+/).filter(Boolean);
    const bad = ids.filter(x => !/^\d+$/.test(x) || Number(x) < 1 || Number(x) >= Math.pow(2, 32 - c.bits) - 1);
    if (bad.length) return `Reserved host ids ${bad.join(", ")} are not hosts of ${row.cidr} (1 to ${Math.pow(2, 32 - c.bits) - 2}).`;
  }
  if (row.dhcpServerRef && servers && !servers.some(s => s.name === row.dhcpServerRef)) return `DHCP server ${row.dhcpServerRef} is not registered for this site.`;
  return null;
}
// Cross-site role pairing, proposed: for each NAD of this cluster that VMs
// use, the role another site binds the NAD its counterpart VMs use (same VM
// name and namespace), else the one with the most similar name or subnet
// structure. Accepting it is an explicit step.
function proposeRoles(disc, cluster, otherProfiles) {
  const d = discOf(disc, cluster);
  if (!d) return [];
  const used = uniqSorted(d.vms.flatMap(vm => (vm.networks || []).map(n => n.nad)));
  const candidates = d.nads.map(nadRef).filter(n => used.includes(n)).concat(d.nads.map(nadRef).filter(n => !used.includes(n)));
  const out = [];
  const words = s => String(s).toLowerCase().split(/[^a-z]+/).filter(w => w.length > 2 && !/vlan|net/.test(w));
  candidates.forEach(nad => {
    let best = null;
    (otherProfiles || []).forEach(p => (p.spec && p.spec.logicalNetworks || []).forEach(ln => {
      const od = discOf(disc, p.name);
      const sameVMs = od ? d.vms.filter(vm => (vm.networks || []).some(n => n.nad === nad) &&
        od.vms.some(o => o.name === vm.name && o.namespace === vm.namespace && (o.networks || []).some(n => n.nad === ln.nad))).map(vm => `${vm.namespace}/${vm.name}`) : [];
      const shared = words(nad).filter(w => words(ln.nad).includes(w));
      const mine = nadSubnet(disc, cluster, nad).cidr, theirs = od ? nadSubnet(disc, p.name, ln.nad).cidr : "";
      const sameShape = mine && theirs && mine.split("/")[1] === theirs.split("/")[1] && mine.split(".")[0] === theirs.split(".")[0];
      const score = sameVMs.length * 10 + shared.length * 3 + (sameShape ? 1 : 0);
      if (score > 0 && (!best || score > best.score)) best = {score, role: ln.role, site: p.name, theirs: ln.nad,
        why: sameVMs.length ? `the same VMs use it on ${p.name} (${sameVMs.slice(0, 3).join(", ")})` : shared.length ? `its name matches ${ln.nad} on ${p.name}` : `its subnet has the shape of ${theirs} on ${p.name}`};
    }));
    if (best && !out.some(o => o.role === best.role)) out.push({nad, role: best.role, site: best.site, theirs: best.theirs, why: best.why});
  });
  return out;
}

// ---- applications: PVC selector, tiers and gates ----------------------------------------
// The PVC selector proposed for namespaces: a label every PVC carries, the
// consistency group first, then the app label; else the most common one.
const PVC_KEY_PREFERENCE = ["storage.simplyblock.io/consistency-group", "app", "app.kubernetes.io/name", "app.kubernetes.io/instance"];
function proposePVCSelector(disc, cluster, namespaces) {
  const all = (namespaces || []).flatMap(ns => ((nsReport(disc, cluster, ns) || {}).pvcs || []));
  if (!all.length) return "";
  const pairs = pvcPairs(disc, cluster, namespaces);
  const full = pairs.filter(p => p.count === all.length);
  const rank = p => { const i = PVC_KEY_PREFERENCE.indexOf(p.pair.split("=")[0]); return i < 0 ? 99 : i; };
  const pick = (full.length ? full : pairs).slice().sort((a, b) => rank(a) - rank(b) || b.count - a.count)[0];
  return pick ? pick.pair : "";
}
const pvcSelectorOptions = (disc, cluster, namespaces) => {
  const n = (namespaces || []).flatMap(ns => ((nsReport(disc, cluster, ns) || {}).pvcs || [])).length;
  return [{v: "", l: `every PVC in the namespaces (${n})`}].concat(pvcPairs(disc, cluster, namespaces).map(p => ({v: p.pair, l: `${p.pair} (${p.count} of ${n} PVC${n === 1 ? "" : "s"})`})));
};

// What a tier can select by type (Recipe resource types), and the gates
// that need nothing typed.
const RESOURCE_TYPE_OPTIONS = ["configmaps", "secrets", "serviceaccounts", "services", "persistentvolumeclaims", "deployments", "statefulsets", "daemonsets",
  "jobs", "cronjobs", "ingresses", "networkpolicies", "virtualmachines.kubevirt.io", "datavolumes.cdi.kubevirt.io"].map(v => ({v, l: v}));
const GATE_TYPE_OPTIONS = [{v: "vmRunning", l: "VMs running"}, {v: "deploymentsReady", l: "Deployments ready"}, {v: "statefulSetsReady", l: "StatefulSets ready"}, {v: "podsReady", l: "pods ready"}];
const TIER_ORDER = ["config", "tools", "db", "database", "data", "storage", "cache", "queue", "mq", "backend", "app", "api", "worker", "web", "frontend", "ui"];
const tierRank = n => { const i = TIER_ORDER.indexOf(n); return i < 0 ? TIER_ORDER.indexOf("app") + 0.5 : i; };
const TIER_KEY_RE = /(^|\/)tier$/;
const dnsify = s => String(s || "").toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 30);
// The label choices of a tier row: the pairs of the objects of its kinds,
// and "<key> exists" for each key.
function tierLabelOptions(disc, cluster, namespaces, row) {
  const {kinds} = tierKindsOf(row.kinds, row.ready);
  const objs = objectsOf(disc, cluster, namespaces, false).filter(o => kinds.includes(o.kind) && !/ pods$/.test(o.name));
  const pairs = objectPairs(objs);
  const keys = uniqSorted(pairs.map(p => p.split("=")[0]));
  const count = text => objs.filter(o => selectorMatchesLabels(parseSelector(text), o.labels)).length;
  return pairs.map(p => ({v: p, l: `${p} (${count(p)})`})).concat(keys.map(k => ({v: `${k} exists`, l: `${k} exists (${count(`${k} exists`)})`})));
}
// Gate templates from the namespaces' Services: a TCP connect and an HTTP
// GET per port, run from a pod of the application.
const gateCommand = g => g.template === "tcp" ? ["nc", "-z", "-w", "3"].concat(String(g.target || "").split(":"))
  : g.template === "http" ? ["wget", "-q", "-O", "/dev/null", "-T", "5", `http://${g.target}/`]
  : String(g.command || "").trim().split(/\s+/).filter(Boolean);
function serviceTargets(disc, cluster, namespaces) {
  const out = [];
  (namespaces || []).forEach(ns => ((nsReport(disc, cluster, ns) || {}).serviceDetails || []).forEach(s => (s.ports || []).forEach(p =>
    out.push({v: `${s.name}:${p.port}`, l: `${s.name}:${p.port}${p.name ? ` (${p.name})` : ""}${(namespaces || []).length > 1 ? ` in ${ns}` : ""}`}))));
  return out;
}
const podPairOptions = (disc, cluster, namespaces) => {
  const pods = objectsOf(disc, cluster, namespaces, true);
  return objectPairs(pods).map(p => ({v: p, l: `${p} (${pods.filter(o => selectorMatchesLabels(parseSelector(p), o.labels)).length} workload${pods.filter(o => selectorMatchesLabels(parseSelector(p), o.labels)).length === 1 ? "" : "s"})`}));
};
// The tiers proposed for namespaces: config first, then the tools (the
// Deployments and StatefulSets without a tier label), then one tier per
// tier label value in boot order (db before web); a TCP gate on a data
// tier from the tools pod when a Service of the tier's VM answers.
function proposeTiers(disc, cluster, namespaces) {
  const objs = objectsOf(disc, cluster, namespaces, false).filter(o => !/ pods$/.test(o.name));
  const tierKey = o => Object.keys(o.labels || {}).find(k => TIER_KEY_RE.test(k));
  const tiers = [{name: "config", kinds: ["configmaps", "secrets", "serviceaccounts", "services"], labels: [], ready: []}];
  const tools = objs.filter(o => (o.kind === "Deployment" || o.kind === "StatefulSet") && !tierKey(o));
  let toolsPod = "";
  if (tools.length) {
    const common = objectPairs(tools).find(p => tools.every(o => selectorMatchesLabels(parseSelector(p), o.labels)));
    const ready = uniqSorted(tools.map(o => o.kind === "Deployment" ? "deploymentsReady" : "statefulSetsReady"));
    tiers.push(common ? {name: "tools", kinds: [], labels: [common], ready}
      : {name: "tools", kinds: uniqSorted(tools.map(o => o.kind === "Deployment" ? "deployments" : "statefulsets")), labels: [], ready});
    const pods = objectsOf(disc, cluster, namespaces, true).filter(p => tools.some(t => p.name === `${t.name} pods`));
    toolsPod = objectPairs(pods)[0] || "";
  }
  const byValue = {};
  objs.forEach(o => { const k = tierKey(o); if (k) (byValue[`${k}=${o.labels[k]}`] = byValue[`${k}=${o.labels[k]}`] || []).push(o); });
  Object.entries(byValue).map(([pair, os]) => ({name: dnsify(pair.split("=")[1]), labels: [pair], kinds: [],
    ready: uniqSorted(os.map(o => ({VirtualMachine: "vmRunning", Deployment: "deploymentsReady", StatefulSet: "statefulSetsReady"})[o.kind])), objs: os}))
    .sort((a, b) => tierRank(a.name) - tierRank(b.name) || a.name.localeCompare(b.name))
    .forEach(t => tiers.push({name: t.name, kinds: t.kinds, labels: t.labels, ready: t.ready, _objs: t.objs}));
  const gates = [];
  const targets = serviceTargets(disc, cluster, namespaces);
  tiers.filter(t => t._objs && /^(db|database|data|storage|cache)$/.test(t.name)).forEach(t => {
    const svcFor = t._objs.map(o => targets.find(x => x.v.split(":")[0] === o.name)).find(Boolean);
    if (svcFor && toolsPod) gates.push({tier: t.name, template: "tcp", target: svcFor.v, pod: toolsPod, command: "", timeout: 900});
  });
  tiers.forEach(t => delete t._objs);
  return {tiers, gates};
}
// Spec tiers <-> editor rows.
const tierEditorSpec = (rows, gates, keep) => (rows || []).filter(r => (r.name || "").trim()).map(r => {
  const sel = parseSelector((r.labels || []).join(", "));
  const selector = Object.assign({}, (r.kinds || []).length ? {resourceTypes: r.kinds} : {},
    Object.keys(sel.matchLabels || {}).length ? {matchLabels: sel.matchLabels} : {}, (sel.matchExpressions || []).length ? {matchExpressions: sel.matchExpressions} : {});
  const ready = (r.ready || []).map(type => ({type}))
    .concat((gates || []).filter(g => g.tier === r.name).map(g => Object.assign({type: "exec", selector: parseSelector(g.pod || "", {exists: false}).matchLabels || {},
      command: gateCommand(g)}, Number(g.timeout) ? {timeoutSeconds: Number(g.timeout)} : {})))
    .concat(((keep || {})[r.name] || []));
  return Object.assign({name: r.name.trim(), selector}, ready.length ? {ready} : {});
});
function tierEditorRows(tiers) {
  const rows = [], gates = [], keep = {};
  (tiers || []).forEach(t => {
    const sel = t.selector || {};
    rows.push({name: t.name, kinds: sel.resourceTypes || [], labels: selectorText(sel).split(", ").filter(Boolean), ready: (t.ready || []).filter(r => READY_KIND[r.type]).map(r => r.type)});
    (t.ready || []).forEach(r => {
      if (r.type === "condition") (keep[t.name] = keep[t.name] || []).push(r);
      if (r.type !== "exec") return;
      const cmd = r.command || [], pod = Object.entries(r.selector || {}).map(([k, v]) => `${k}=${v}`).join(", ");
      const tcp = cmd[0] === "nc" && cmd.length >= 3 && /^\d+$/.test(cmd[cmd.length - 1]);
      const http = cmd[0] === "wget" && /^https?:\/\//.test(cmd[cmd.length - 1] || "");
      gates.push({tier: t.name, pod, timeout: r.timeoutSeconds || "", command: tcp || http ? "" : cmd.join(" "),
        template: tcp ? "tcp" : http ? "http" : "custom",
        target: tcp ? `${cmd[cmd.length - 2]}:${cmd[cmd.length - 1]}` : http ? cmd[cmd.length - 1].replace(/^https?:\/\//, "").replace(/\/$/, "") : ""});
    });
  });
  return {rows, gates, keep};
}
// What is wrong with a tier row against the reported objects, or null.
function tierRowError(disc, cluster, namespaces, r, rows) {
  if (!r.name) return "Name the tier.";
  if (!DNS_LABEL_RE.test(r.name) || r.name.length > 30) return `${r.name} is not a tier name: lower-case letters, digits and "-", at most 30.`;
  if (rows.filter(x => x.name === r.name).length > 1) return `Tier ${r.name} is declared twice.`;
  if (!(r.kinds || []).length && !(r.labels || []).length) return "Choose what the tier restores: resource types, labels, or both.";
  if (!(r.labels || []).length) return null;
  const sel = parseSelector(r.labels.join(", "));
  if (sel.error) return sel.error;
  const {kinds, known} = tierKindsOf((r.kinds || []).length ? r.kinds : [], r.ready);
  if (!reported(disc, cluster, namespaces)) return null;
  const objs = objectsOf(disc, cluster, namespaces, false).filter(o => kinds.includes(o.kind));
  const hit = objs.filter(o => selectorMatchesLabels(sel, o.labels));
  if (!hit.length && known) return `${r.labels.join(", ")} matches no ${kinds.join(", ")} in ${namespaces.join(", ")}: the restore would wait for objects that never arrive.`;
  return null;
}
function tierRowInfo(disc, cluster, namespaces, r) {
  if (!(r.labels || []).length || !reported(disc, cluster, namespaces)) return null;
  const sel = parseSelector(r.labels.join(", "));
  if (sel.error) return null;
  const {kinds} = tierKindsOf((r.kinds || []).length ? r.kinds : [], r.ready);
  const hit = objectsOf(disc, cluster, namespaces, false).filter(o => kinds.includes(o.kind) && !/ pods$/.test(o.name) && selectorMatchesLabels(sel, o.labels));
  return hit.length ? `selects ${hit.slice(0, 4).map(o => `${o.kind} ${o.name}`).join(", ")}${hit.length > 4 ? ` and ${hit.length - 4} more` : ""}` : "selects none of the reported objects (it may select ConfigMaps or Secrets)";
}
function gateRowError(disc, cluster, namespaces, g, tierNames) {
  if (!tierNames.includes(g.tier)) return "Choose the tier the check gates.";
  if (!g.pod) return "Choose the pod the check runs in.";
  if (g.template !== "custom" && !g.target) return "Choose the service and port.";
  if (g.template === "custom" && !String(g.command || "").trim()) return "Write the command.";
  if (g.timeout && (!/^\d+$/.test(String(g.timeout)) || Number(g.timeout) < 1 || Number(g.timeout) > 3600)) return "The timeout is 1 to 3600 seconds.";
  const chk = gateSelectorCheck(disc, cluster, namespaces, g.pod);
  if (chk.error) return chk.error;
  if (chk.matches && !chk.matches.length) return `${g.pod} matches no pod in ${namespaces.join(", ")}: the check could never run.`;
  return null;
}

Object.assign(window, {uniqSorted, nadRef, AGENT_VIEW, DEFAULT_SC_SELECTOR, DEFAULT_SC_NAME, DNS_LABEL_RE, TEST_ID_RE, DURATION_RE, QUANTITY_RE,
  agentStatusOf, loadDiscovery, discOf, clusterOptions, zoneOptions, regionOptions, siteRowError, veleroProposal, labelsMatch, scMatches,
  parseSelector, selectorText, selectorMatchesLabels, namespaceOptions, pvcPairs, pvcMatches, tierKindsOf, objectsOf, objectPairs,
  tierSelectorCheck, gateSelectorCheck, isolatedNadOf, proposePaths, pathError, bubbleNamespace, proposeTestID, testIDError,
  parseCidr, cidrContains, hostIdOf, inferCidr, nadAddresses, nadSubnet, dhcpServersOn, proposedReserved, guestRowError, proposeRoles,
  testNadsOf, isBubbleServer, proposeDHCPServer,
  nsInfra, nsElsewhere, nsHoldings,
  proposePVCSelector, pvcSelectorOptions, RESOURCE_TYPE_OPTIONS, GATE_TYPE_OPTIONS, tierLabelOptions, gateCommand, serviceTargets, podPairOptions,
  proposeTiers, tierEditorSpec, tierEditorRows, tierRowError, tierRowInfo, gateRowError, nsReport, reported});
