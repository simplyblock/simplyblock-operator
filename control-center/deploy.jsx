// ---------------------------------------------------------------------------
// DISCOVERY AND CLUSTER DEPLOYMENT — discovery panel and the wizard.
// Control plane (Helm) and operator install happen outside this console.
// Discovery makes a Kubernetes cluster "discovered"; only then can a storage
// cluster be deployed on it. The wizard writes a draft document; approval
// (on the document) starts the three asynchronous steps.
// ---------------------------------------------------------------------------
const GBn = 1e9;
const gb = n => Math.round((n || 0) / GBn);
const devLabel = d => d.kind === "nvme" ? (d.pcie || d.blockdev) : d.blockdev;
const csv = s => String(s || "").split(",").map(x => x.trim()).filter(Boolean);
const up = (set, patch) => set(s => Object.assign({}, s, patch));

// ---- discovery -------------------------------------------------------------
const discoveryDialog = (k, prev) => ({
  title: `Discover ${k.name}`,
  confirm: "Run discovery",
  desc: "The operator deploys an inspection pod on every matching worker node and reports NUMA topology, vCPU, memory, network interfaces and every unmounted, unused device. Filters apply during discovery — an excluded device is never reported. Runs asynchronously; the cluster is marked discovered when it completes.",
  fields: v => [
    {k: "tag_key", label: "Only nodes with this label", type: "text", def: (prev && Object.keys(prev.nodeSelector || {})[0]) || "", placeholder: "simplyblock.io/storage-node-candidate"},
    v.tag_key ? {k: "tag_value", label: "Label value", type: "text", def: "true"} : null,
    {k: "pcie_allow", label: "PCIe addresses — allow", type: "text", def: ((prev && prev.filter.pcieAllowList) || []).join(", "), placeholder: "0000:5e:, 0000:5f:  (prefixes, comma-separated)"},
    {k: "pcie_deny", label: "PCIe addresses — deny", type: "text", def: ((prev && prev.filter.pcieDenyList) || []).join(", "), placeholder: "0000:00:04.0"},
    {k: "blockdev", label: "Block device name pattern", type: "text", def: ((prev && prev.filter.blockDeviceNames) || []).join(", "), placeholder: "nvme*, sd[b-f]  (glob, comma-separated)"},
    {k: "models", label: "Device model contains", type: "text", def: ((prev && prev.filter.models) || []).join(", "), placeholder: "PM9A3, CD8"},
    {k: "size_min", label: "Capacity — smallest", unit: "GB", type: "number", min: 0, def: prev && prev.filter.driveSizeRange ? gb(prev.filter.driveSizeRange.min) : 400},
    {k: "size_max", label: "Capacity — largest (0 = no limit)", unit: "GB", type: "number", min: 0, def: prev && prev.filter.driveSizeRange ? gb(prev.filter.driveSizeRange.max) : 0},
    {k: "n2", type: "note", label: "Leave a filter empty to skip it. All filters combine; a device must pass every one to be reported."}
  ].filter(Boolean),
  run: v => api.discoveryRun(k.id, k.name, {
    pcieAllowList: csv(v.pcie_allow), pcieDenyList: csv(v.pcie_deny), blockDeviceNames: csv(v.blockdev), models: csv(v.models),
    driveSizeRange: {min: Number(v.size_min || 0) * GBn, max: Number(v.size_max || 0) * GBn}, enableLogicalBlockDevices: true
  }, v.tag_key ? {matchLabels: {[v.tag_key]: v.tag_value || "true"}} : {matchLabels: {}})
});

// "Deploy cluster" from the clusters overview: pick a discovered Kubernetes cluster
const deployFromDialog = nav => ({
  title: "Deploy a storage cluster", confirm: "Continue",
  desc: "A storage cluster is deployed onto the worker nodes of a discovered Kubernetes cluster. Undiscovered clusters are not offered — run discovery on them first.",
  fields: [{k: "kid", label: "Kubernetes cluster", type: "select", required: true,
    load: () => api.k8sClusters().then(ks => ks.filter(k => k.discovered).map(k => ({v: k.id, l: `${k.name} · discovered ${fmtAgo(k.discoveredAt)}`}))),
    empty: "No Kubernetes cluster has been discovered yet."}],
  run: v => { nav.deployWizard(v.kid); return Promise.resolve({}); }
});

const FilterProps = ({f}) => (
  <Props rows={[
    ["PCIe allow", (f.pcieAllowList || []).length ? f.pcieAllowList.join(", ") : "—"],
    ["PCIe deny", (f.pcieDenyList || []).length ? f.pcieDenyList.join(", ") : "—"],
    ["Block device pattern", (f.blockDeviceNames || []).length ? f.blockDeviceNames.join(", ") : "—"],
    ["Model contains", (f.models || []).length ? f.models.join(", ") : "—"],
    ["Capacity", `${(f.driveSizeRange || {}).min ? fmtBytes(f.driveSizeRange.min, 0) : "any"} – ${(f.driveSizeRange || {}).max ? fmtBytes(f.driveSizeRange.max, 0) : "any"}`]
  ]} />
);

function DiscoveryPanel({k, nav}) {
  const {data: ds, loading, error, reload} = useResource("disc|" + k.id, () => api.discoveries(k.id), 2500);
  const {data: hosts} = useResource("dhosts|" + k.id, () => api.k8sHosts(k.id), 4000);
  const {data: cfgs} = useResource("dcfg|" + k.id, () => api.k8sDeployConfigs(k.id), 3000);
  const cur = (ds || [])[0];
  const running = cur && cur.status === "running";
  const inv = (hosts || []).filter(h => cur && cur.status === "complete" && cur.hostIds.includes(h.id));
  if (error) return <ErrorState error={error} onRetry={reload} kind="discovery" />;
  return (
    <>
      {!cur && !loading && <div className="empty">
        <Icon n="search" s={24} c="var(--accent)" />
        <b style={{color: "var(--text)"}}>{k.name} has not been discovered</b>
        <span style={{maxWidth: 520}}>The operator is connected and lists {k.counts.workers} worker node(s), but nothing is known about their hardware. Discovery inspects them; a storage cluster can only be deployed on a discovered cluster.</span>
        <button className="btn primary" style={{marginTop: 10}} onClick={() => window.__ui.dialog(discoveryDialog(k), k)}><Icon n="search" s={12} />Run discovery</button>
      </div>}
      {running && <div className="banner" style={{color: "var(--info)"}}><span className="dots"><i></i><i></i><i></i></span>
        <span><b>Discovery running — {cur.step}.</b> Inspection pods are collecting the inventory; this page updates as it lands.</span></div>}
      {cur && <>
        <div className="stats">
          <Stat k="Discovery" v={running ? "running" : "complete"} s={running ? cur.step : fmtAgo(cur.finishedAt || cur.startedAt)} c={running ? "var(--info)" : "var(--ok)"} />
          <Stat k="Worker nodes" v={cur.counts.nodes} s="inspected" />
          <Stat k="Usable devices" v={cur.counts.devices} s={cur.counts.filtered ? `${cur.counts.filtered} excluded by the filter` : "nothing excluded"} />
          <Stat k="Deployment documents" v={(cfgs || []).length} s={(cfgs || []).filter(c => !c.approved).length + " awaiting approval"} />
        </div>
        <div className="dcols">
          <div className="card"><h3>Discovery filter</h3><div className="bd">
            <FilterProps f={cur.filter} />
            <div className="btnrow" style={{marginTop: 10}}>
              <button className="chip" disabled={running} onClick={() => window.__ui.dialog(discoveryDialog(k, cur), k)}><Icon n="refresh" s={12} />Re-run with different filters</button>
            </div>
          </div></div>
          <div className="card"><h3>Node selector</h3><div className="bd">
            {Object.keys(cur.nodeSelector).length ? <Props rows={Object.entries(cur.nodeSelector).map(([a, b]) => [a, b])} />
              : <p className="mdesc" style={{marginBottom: 0}}>No selector — every worker node was inspected.</p>}
            {cur.opName && <div className="uuid" style={{marginTop: 8}}><span>OperatorOps/{cur.opName}</span></div>}
          </div></div>
        </div>
        <div className="sech"><h2>Inventory</h2><span className="ln"></span>
          {!running && <button className="btn primary" onClick={() => nav.deployWizard(k.id)}><Icon n="plus" s={12} />Deploy a cluster</button>}</div>
        {inv.map(h => <HostInventory key={h.id} h={h} />)}
        {!inv.length && !running && <div className="nolim">The inventory is empty — no worker node matched the selector.</div>}
        {!!(cfgs || []).length && <>
          <div className="sech"><h2>Deployment documents</h2><span className="ln"></span></div>
          <div className="grid">{cfgs.map(c => <DeployConfigTile key={c.id} d={c} nav={nav} />)}</div>
        </>}
      </>}
    </>
  );
}

function HostInventory({h}) {
  const sockets = Array.from({length: h.sockets || 1}, (_, i) => i);
  return (
    <div className="card"><h3>{h.hostname}
      <span className="labels" style={{marginLeft: 8}}>
        {h.zone && <span className="lab"><i>zone</i>{h.zone}</span>}
        <span className="lab"><i>vcpu</i>{h.vcpu}</span><span className="lab"><i>ram</i>{fmtBytes(h.memory, 0)}</span>
        <span className="lab"><i>sockets</i>{h.sockets}</span><span className="lab"><i>nics</i>{h.nics.map(n => n.name).join(", ")}</span>
      </span></h3>
      <div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Socket</th><th>Kind</th><th>PCIe address</th><th>Block device</th><th>Serial</th><th>Model</th><th style={{textAlign: "right"}}>Capacity</th><th></th></tr></thead><tbody>
          {sockets.flatMap(s => h.devices.filter(d => d.socket === s).map(d => (
            <tr key={d.id} style={d.assignedNodeId ? {opacity: .55} : null}>
              <td className="mono">{s}</td><td>{d.kind === "nvme" ? "NVMe" : "block"}</td>
              <td className="mono">{d.pcie || "—"}</td><td className="mono">{d.blockdev}</td>
              <td className="mono">{d.serial || "—"}</td><td>{d.model}</td>
              <td className="mono" style={{textAlign: "right"}}>{fmtBytes(d.size)}</td>
              <td>{d.assignedNodeId ? <span className="lab">assigned</span> : <span className="lab" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"}}>free</span>}</td>
            </tr>)))}
        </tbody></table>
      </div>
    </div>
  );
}

// ---- the wizard ------------------------------------------------------------
const EC_OPTS = ["1+1", "2+1", "2+2", "4+1", "4+2", "8+2"];
const Tog = ({on, set, label, hint}) => (
  <label className="fc" onClick={() => set(!on)}>
    <span className={"selbox" + (on ? " on" : "")}>{on && <Icon n="check" s={11} />}</span>
    <span>{label}{hint && <span className="sub" style={{display: "block", fontSize: 10.5, color: "var(--dim2)"}}>{hint}</span>}</span>
  </label>
);
const Fl = ({l, sub, children, sm}) => <label className={"fl" + (sm ? " sm" : "")}>{l}{sub && <span className="sub">{sub}</span>}{children}</label>;
const Inp = ({v, set, ...p}) => <input className="inp" value={v} onChange={e => set(e.target.value)} {...p} />;

// filter object from the wizard's cluster form
const clusterFilter = c => ({
  deviceClass: c.deviceClass,
  pcieAllowList: c.deviceClass === "nvme" ? csv(c.pcieAllow) : [], pcieDenyList: c.deviceClass === "nvme" ? csv(c.pcieDeny) : [],
  models: c.deviceClass === "nvme" ? csv(c.models) : [], blockDeviceNames: c.deviceClass === "block" ? csv(c.namePattern) : [],
  driveSizeRange: {min: Number(c.sizeMin || 0) * GBn, max: Number(c.sizeMax || 0) * GBn}
});
const toMock = d => ({kind: d.kind, pcie_address: d.pcie, device_name: d.blockdev, model_number: d.model, size: d.size});
const matches = (d, f) => !d.assignedNodeId && window.deviceMatches(toMock(d), f);
const quick = (d, q) => !q || [d.pcie, d.blockdev, d.model, d.serial].some(x => (x || "").toLowerCase().includes(q.toLowerCase()));

function DeployWizard({kid, nav}) {
  const {data: k} = useResource("wk|" + kid, () => api.k8sCluster(kid));
  const {data: ds} = useResource("wd|" + kid, () => api.discoveries(kid));
  const {data: hosts} = useResource("wh|" + kid, () => api.k8sHosts(kid));
  const [c, setC] = useState({name: "", deviceClass: "nvme", ec: "2+1", numa: "both", vcpu: 12, maxSubsystems: 128, hugepagesGb: "", memoryGb: 32,
    backups: true, objectStorage: false, failureDomains: true, coreIsolation: true, mgmtNic: "", dataNic1: "", dataNic2: "",
    pcieAllow: "", pcieDeny: "", models: "", sizeMin: 400, sizeMax: 0, namePattern: "",
    s3Endpoint: "", s3Bucket: "", s3Region: "", s3AccessKey: "", s3SecretKey: "",
    kmsEnabled: false, kmsProvider: "vault", kmsAddress: "", kmsKeyName: "", kmsAuth: "token", kmsToken: "", kmsVerifyTls: true});
  const [fd, setFd] = useState({});   // host id → failure domain label, fixed at deployment
  const [mode, setMode] = useState("pick");
  const [tag, setTag] = useState({key: "", value: "true"});
  const [zone, setZone] = useState("");
  const [picked, setPicked] = useState({});
  const [ov, setOv] = useState({});  // per host: {sockets, q, drop:{}, add:{}}
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);

  const disc = (ds || []).find(d => d.status === "complete");
  const pool = useMemo(() => (hosts || []).filter(h => disc && disc.hostIds.includes(h.id)), [hosts, disc]);
  const zones = useMemo(() => [...new Set(pool.map(h => h.zone).filter(Boolean))].sort(), [pool]);
  const nicNames = useMemo(() => [...new Set(pool.flatMap(h => h.nics.map(n => n.name)))].sort(), [pool]);
  const filter = useMemo(() => clusterFilter(c), [c]);
  const numaDefault = c.numa === "both" ? [0, 1] : [Number(c.numa)];
  const listed = pool.filter(h => !zone || h.zone === zone);
  const selected = useMemo(() => mode === "tag"
    ? pool.filter(h => tag.key && (h.k8sLabels[tag.key] || h.labels[tag.key]) === tag.value)
    : pool.filter(h => picked[h.id]), [pool, mode, tag, picked]);

  const ovOf = h => ov[h.id] || {};
  const setOvOf = (h, patch) => setOv(o => Object.assign({}, o, {[h.id]: Object.assign({}, o[h.id] || {}, patch)}));
  const socketsOf = h => (ovOf(h).sockets || numaDefault).filter(s => s < (h.sockets || 1));
  const devSel = (h, d) => { const o = ovOf(h); if (o.drop && o.drop[d.id]) return false; if (o.add && o.add[d.id]) return true; return matches(d, filter) && quick(d, o.q); };
  const devicesOf = (h, s) => h.devices.filter(d => d.socket === s && !d.assignedNodeId && devSel(h, d));
  const hpPerNode = c.hugepagesGb ? Number(c.hugepagesGb) * GBn : hugepagesFor(Number(c.maxSubsystems) || 0);
  const plan = useMemo(() => {
    const groups = selected.flatMap(h => socketsOf(h).map(s => ({h, s, devs: devicesOf(h, s)})));
    return {groups, nodes: groups.length, devices: groups.reduce((n, g) => n + g.devs.length, 0), raw: groups.reduce((n, g) => n + g.devs.reduce((a, d) => a + d.size, 0), 0)};
  }, [selected, ov, filter, c.numa]);
  const [nd, np] = c.ec.split("+").map(Number);
  const usable = plan.raw * (nd / (nd + np));
  const nicMissing = selected.filter(h => [c.mgmtNic, c.dataNic1].filter(Boolean).some(n => !h.nics.some(x => x.name === n)));
  const fdOf = h => (fd[h.id] !== undefined ? fd[h.id] : (h.rack || h.zone || "")).trim();
  const fdMissing = c.failureDomains ? selected.filter(h => !fdOf(h)) : [];
  // a domain must hold at least two nodes, and domains stay within one node of
  // each other — so nodes are chosen in pairs per domain
  const fdTally = {};
  selected.forEach(h => { const f = fdOf(h); if (f) fdTally[f] = (fdTally[f] || 0) + 1; });
  const fdNames = Object.keys(fdTally).sort();
  const fdCount = fdNames.length;
  const fdThin = fdNames.filter(f => fdTally[f] < 2);
  const fdVals = fdNames.map(f => fdTally[f]);
  const fdSpread = fdVals.length ? Math.max(...fdVals) - Math.min(...fdVals) : 0;
  const fdTooFew = c.failureDomains && fdCount < 2;
  const fdBad = c.failureDomains && (fdThin.length > 0 || fdSpread > 1);
  const s3Missing = c.backups && !(c.s3Endpoint && c.s3Bucket && c.s3AccessKey && c.s3SecretKey);
  const kmsMissing = c.kmsEnabled && !(c.kmsAddress && c.kmsKeyName);
  const ready = !!c.name && !!c.mgmtNic && !!c.dataNic1 && plan.nodes > 0 && plan.devices > 0 && !nicMissing.length
    && !fdMissing.length && !fdTooFew && !fdBad && !s3Missing && !kmsMissing;

  const create = async () => {
    setBusy(true); setErr(null);
    try {
      const cfg = await api.deployConfigCreate(c.name + "-deployment", {
        environment: (k && k.environment) || "Vanilla", kubernetesClusterRef: k ? k.name : null,
        nodeSelector: mode === "tag" && tag.key ? {matchLabels: {[tag.key]: tag.value}} : {matchLabels: {}},
        cluster: {name: c.name, deviceClass: c.deviceClass, ec: c.ec, numaSockets: numaDefault,
          vcpu: Number(c.vcpu), maxSubsystems: Number(c.maxSubsystems), hugepagesOverride: c.hugepagesGb ? Number(c.hugepagesGb) * GBn : null, systemMemoryGb: Number(c.memoryGb),
          backups: c.backups, objectStorage: c.objectStorage, failureDomains: c.failureDomains, coreIsolation: c.coreIsolation,
          mgmtNic: c.mgmtNic, dataNics: [c.dataNic1, c.dataNic2].filter(Boolean), deviceFilter: filter,
          backupTarget: c.backups ? {endpoint: c.s3Endpoint, bucket: c.s3Bucket, region: c.s3Region || null, accessKeyId: c.s3AccessKey, secretAccessKeyRef: c.s3SecretKey ? "sb-s3-backup-credentials" : null} : null,
          kms: c.kmsEnabled ? {provider: c.kmsProvider, address: c.kmsAddress, keyName: c.kmsKeyName, auth: c.kmsAuth, verifyTls: c.kmsVerifyTls, secretRef: "sb-kms-credentials"} : null,
          failureDomainLabels: c.failureDomains ? Object.fromEntries(selected.map(h => [h.hostname, fdOf(h)])) : null},
        __kubernetesClusterId: kid,
        __hosts: selected.map(h => ({id: h.id, sockets: socketsOf(h), failureDomain: c.failureDomains ? fdOf(h) : null,
          deviceIds: socketsOf(h).flatMap(s => devicesOf(h, s).map(d => d.id))}))
      });
      nav.deployConfig(kid, cfg.metadata.uid);
    } catch (e) { setErr(e); setBusy(false); }
  };

  if (!k || !hosts || !ds) return <div className="scroll"><div className="stats">{Array.from({length: 4}).map((_, i) => <div className="skel" key={i} style={{height: 62}}></div>)}</div></div>;
  if (!disc) return <div className="scroll"><div className="empty"><Icon n="search" s={24} c="var(--warn)" />
    <b style={{color: "var(--text)"}}>{k.name} is not discovered</b><span style={{maxWidth: 480}}>A storage cluster can only be deployed on a discovered Kubernetes cluster. Run discovery first.</span>
    <button className="btn primary" style={{marginTop: 10}} onClick={() => nav.discovery(kid)}><Icon n="search" s={12} />Go to discovery</button></div></div>;

  const nvme = c.deviceClass === "nvme";
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}>
        <h1>Deploy a storage cluster</h1>
        <div className="mdesc" style={{marginTop: 4}}>On the discovered hardware of <b>{k.name}</b> ({pool.length} candidate node(s)). This writes a deployment document for review — nothing is applied until it is approved.</div>
      </div></div>

      <div className="wzstep"><b>1</b><h2>Cluster parameters</h2><span className="ln"></span></div>
      <div className="card"><div className="bd">
        <div className="frow">
          <Fl l="Cluster name"><Inp v={c.name} set={v => up(setC, {name: v})} placeholder="prod-eu-central-2" /></Fl>
          <Fl l="Device type" sm><select className="sel" value={c.deviceClass} onChange={e => up(setC, {deviceClass: e.target.value})}><option value="nvme">NVMe (PCIe)</option><option value="block">Linux block devices</option></select></Fl>
          <Fl l="Erasure coding" sm><select className="sel" value={c.ec} onChange={e => up(setC, {ec: e.target.value})}>{EC_OPTS.map(x => <option key={x} value={x}>{x} data+parity</option>)}</select></Fl>
          <Fl l="NUMA sockets" sub="one storage node per socket" sm><select className="sel" value={c.numa} onChange={e => up(setC, {numa: e.target.value})}><option value="0">socket 0</option><option value="1">socket 1</option><option value="both">both</option></select></Fl>
        </div>
        <div className="frow">
          <Fl l="vCPU per storage node" sm><Inp type="number" min="4" v={c.vcpu} set={v => up(setC, {vcpu: v})} /></Fl>
          <Fl l="Max subsystems" sm sub={`→ ${fmtBytes(hugepagesFor(Number(c.maxSubsystems) || 0), 0)} hugepages`}><Inp type="number" min="8" v={c.maxSubsystems} set={v => up(setC, {maxSubsystems: v})} /></Fl>
          <Fl l="Hugepage memory override" sub="GB — empty = derived" sm><Inp type="number" min="2" step="2" v={c.hugepagesGb} set={v => up(setC, {hugepagesGb: v})} placeholder={String(gb(hugepagesFor(Number(c.maxSubsystems) || 0)))} /></Fl>
          <Fl l="System memory per node" sub="GB" sm><Inp type="number" min="4" v={c.memoryGb} set={v => up(setC, {memoryGb: v})} /></Fl>
        </div>
        <div className="togs">
          <Tog on={c.coreIsolation} set={v => up(setC, {coreIsolation: v})} label="Core isolation" hint="CPU topology is enforced either way" />
          <Tog on={c.failureDomains} set={v => up(setC, {failureDomains: v})} label="Failure domains" hint="from rack / zone labels" />
          <Tog on={c.backups} set={v => up(setC, {backups: v})} label="Backups" hint="S3 backup target, set after deployment" />
          <Tog on={c.objectStorage} set={v => up(setC, {objectStorage: v})} label="S3 object storage" hint="buckets on this cluster" />
        </div>
        <div className="frow">
          <Fl l="Management NIC" sm><select className="sel" value={c.mgmtNic} onChange={e => up(setC, {mgmtNic: e.target.value})}><option value="">choose…</option>{nicNames.map(n => <option key={n}>{n}</option>)}</select></Fl>
          <Fl l="Data NIC 1" sm><select className="sel" value={c.dataNic1} onChange={e => up(setC, {dataNic1: e.target.value})}><option value="">choose…</option>{nicNames.filter(n => n !== c.mgmtNic).map(n => <option key={n}>{n}</option>)}</select></Fl>
          <Fl l="Data NIC 2" sub="optional — multipath" sm><select className="sel" value={c.dataNic2} onChange={e => up(setC, {dataNic2: e.target.value})}><option value="">none</option>{nicNames.filter(n => n !== c.mgmtNic && n !== c.dataNic1).map(n => <option key={n}>{n}</option>)}</select></Fl>
        </div>
        <div className="sech" style={{margin: "6px 0 8px"}}><h2>Device filter</h2><span className="ln"></span><span className="cnt">{nvme ? "NVMe" : "block devices"}</span></div>
        <div className="frow">
          {nvme ? <>
            <Fl l="PCIe allow" sub="prefixes, comma-separated"><Inp v={c.pcieAllow} set={v => up(setC, {pcieAllow: v})} placeholder="0000:5e:, 0000:5f:" /></Fl>
            <Fl l="PCIe deny"><Inp v={c.pcieDeny} set={v => up(setC, {pcieDeny: v})} placeholder="0000:00:04.0" /></Fl>
            <Fl l="SSD model contains"><Inp v={c.models} set={v => up(setC, {models: v})} placeholder="PM9A3, CD8" /></Fl>
          </> : <Fl l="Block device name pattern" sub="glob, comma-separated"><Inp v={c.namePattern} set={v => up(setC, {namePattern: v})} placeholder="/dev/sd*, /dev/nvme?n1" /></Fl>}
          <Fl l="Capacity min" sub="GB" sm><Inp type="number" min="0" v={c.sizeMin} set={v => up(setC, {sizeMin: v})} /></Fl>
          <Fl l="Capacity max" sub="GB, 0 = any" sm><Inp type="number" min="0" v={c.sizeMax} set={v => up(setC, {sizeMax: v})} /></Fl>
        </div>
        <p className="mdesc" style={{margin: 0}}>This filter picks the default device set on every node; step 3 can narrow it further per node.</p>
      </div></div>

      <div className="wzstep"><b>2</b><h2>Nodes</h2><span className="ln"></span>
        {zones.length > 1 && <select className="sel" value={zone} onChange={e => setZone(e.target.value)}><option value="">all sites</option>{zones.map(z => <option key={z}>{z}</option>)}</select>}
        <div className="seg"><button className={mode === "pick" ? "on" : ""} onClick={() => setMode("pick")}>Pick nodes</button><button className={mode === "tag" ? "on" : ""} onClick={() => setMode("tag")}>By node label</button></div></div>
      {mode === "tag" ? (
        <div className="card"><div className="bd">
          <div className="frow">
            <Fl l="Label key"><Inp v={tag.key} set={v => up(setTag, {key: v})} placeholder="simplyblock.io/storage-node-candidate" /></Fl>
            <Fl l="Value" sm><Inp v={tag.value} set={v => up(setTag, {value: v})} /></Fl>
          </div>
          <p className="mdesc" style={{marginBottom: 0}}>{tag.key ? `${selected.length} of ${pool.length} discovered node(s) carry this label.` : "Enter a label key to match nodes by."}</p>
        </div></div>
      ) : (
        <div className="grid">{listed.map(h => {
          const n = h.devices.filter(d => matches(d, filter)).length;
          return <div key={h.id} className={"tile" + (picked[h.id] ? " sel" : "")} style={{"--sc": "var(--accent)"}} onClick={() => setPicked(p => Object.assign({}, p, {[h.id]: !p[h.id]}))}>
            <div className="th"><div style={{minWidth: 0, flex: 1, display: "flex", gap: 8, alignItems: "flex-start"}}>
              <span className={"selbox" + (picked[h.id] ? " on" : "")}>{picked[h.id] && <Icon n="check" s={11} />}</span><Name>{h.hostname}</Name></div>
              {h.zone && <span className="badge">{h.zone}</span>}</div>
            <div className="kv">
              <div><span>Sockets</span><b>{h.sockets}</b></div><div><span>Matching devices</span><b style={n ? null : {color: "var(--warn)"}}>{n}</b></div>
              <div><span>vCPU</span><b>{h.vcpu}</b></div><div><span>RAM</span><b>{fmtBytes(h.memory, 0)}</b></div>
            </div>
          </div>;
        })}</div>
      )}
      {!listed.length && <div className="nolim">No discovered node{zone ? ` in ${zone}` : ""}.</div>}

      <div className={"wzstep" + (selected.length ? "" : " dis")}><b>3</b><h2>Per-node devices and sockets</h2><span className="ln"></span><span className="cnt">{plan.nodes} storage node(s)</span></div>
      {!!selected.length && <div className="card"><div className="bd" style={{padding: 0}}>
        {selected.map(h => {
          const o = ovOf(h);
          return <div key={h.id} className="nset">
            <div className="nsh"><b>{h.hostname}</b>{h.zone && <span className="lab"><i>site</i>{h.zone}</span>}
              {c.failureDomains && <label className="fdin" title="fixed for the node's lifetime"><i>failure domain</i>
                <input className="inp" list="fdlist" value={fdOf(h)} placeholder="rack-1" onChange={e => setFd(f => Object.assign({}, f, {[h.id]: e.target.value}))} /></label>}
              <div className="socks">{Array.from({length: h.sockets || 1}, (_, s) => {
                const on = socketsOf(h).includes(s);
                return <button key={s} className={"chip" + (on ? " on" : "")} onClick={() => setOvOf(h, {sockets: on ? socketsOf(h).filter(y => y !== s) : [...socketsOf(h), s].sort()})}>socket {s}<b className="n">{h.devices.filter(d => d.socket === s && matches(d, filter)).length}</b></button>;
              })}</div>
              <input className="inp" style={{width: 220, marginLeft: "auto", height: 26}} placeholder="extra filter: pcie / model / name" value={o.q || ""} onChange={e => setOvOf(h, {q: e.target.value, drop: {}, add: {}})} />
              {nicMissing.includes(h) && <span className="lab" style={{color: "var(--bad)"}}>missing NIC {[c.mgmtNic, c.dataNic1].filter(n => n && !h.nics.some(x => x.name === n)).join(", ")}</span>}
            </div>
            {socketsOf(h).map(s => (
              <div key={s} className="devrow"><span className="tlabel">socket {s}</span>
                <div className="devs">{h.devices.filter(d => d.socket === s && !d.assignedNodeId && d.kind === c.deviceClass).map(d => {
                  const on = devSel(h, d);
                  return <button key={d.id} className={"chip" + (on ? " on" : "")} title={`${d.model} · ${fmtBytes(d.size)}${d.serial ? " · " + d.serial : ""}`}
                    onClick={() => setOvOf(h, on ? {drop: Object.assign({}, o.drop, {[d.id]: true}), add: Object.assign({}, o.add, {[d.id]: false})} : {add: Object.assign({}, o.add, {[d.id]: true}), drop: Object.assign({}, o.drop, {[d.id]: false})})}>
                    {devLabel(d)}<b className="n">{fmtBytes(d.size, 0)}</b></button>;
                })}
                {!h.devices.some(d => d.socket === s && !d.assignedNodeId && d.kind === c.deviceClass) && <span className="mdesc" style={{margin: 0}}>no free {nvme ? "NVMe" : "block"} device on this socket</span>}</div>
              </div>))}
          </div>;
        })}
      </div></div>}

      {c.failureDomains && !!selected.length && <div className="fdsum">
        <Icon n="host" s={13} />
        <span>{fdCount || "no"} failure domain{fdCount === 1 ? "" : "s"} across {selected.length} node(s){fdMissing.length ? ` · ${fdMissing.length} node(s) unlabelled` : ""}</span>
        {!!fdCount && <span className="mono">{fdNames.map(f => `${f}: ${fdTally[f]}`).join(" · ")}</span>}
        <datalist id="fdlist">{[...new Set(pool.map(h => h.rack || h.zone).filter(Boolean))].map(x => <option key={x} value={x} />)}</datalist>
        <span className="note">at least two nodes per domain, domains within one node of each other · a node's domain is set once, at deployment, and cannot be changed afterwards</span>
      </div>}

      <div className="wzstep"><b>4</b><h2>Backup target and key management</h2><span className="ln"></span><span className="cnt">correctable later</span></div>
      <div className="card"><div className="bd">
        {c.backups ? <>
          <div className="frow">
            <Fl l="S3 endpoint" sub="backups of snapshot chains are written here"><Inp v={c.s3Endpoint} set={v => up(setC, {s3Endpoint: v})} placeholder="https://s3.eu-central-1.amazonaws.com" /></Fl>
            <Fl l="Bucket" sm><Inp v={c.s3Bucket} set={v => up(setC, {s3Bucket: v})} placeholder="sb-backups-prod" /></Fl>
            <Fl l="Region" sm><Inp v={c.s3Region} set={v => up(setC, {s3Region: v})} placeholder="eu-central-1" /></Fl>
          </div>
          <div className="frow">
            <Fl l="Access key ID" sm><Inp v={c.s3AccessKey} set={v => up(setC, {s3AccessKey: v})} /></Fl>
            <Fl l="Secret access key" sub="stored in a Kubernetes secret" sm><Inp type="password" v={c.s3SecretKey} set={v => up(setC, {s3SecretKey: v})} /></Fl>
          </div>
          <p className="mdesc" style={{margin: 0}}>The credentials are written to <span className="mono">sb-s3-backup-credentials</span> in the operator namespace, never into the deployment document. The endpoint can be corrected after deployment from the cluster's actions.</p>
        </> : <p className="mdesc" style={{margin: 0}}>Backups are off for this cluster, so no S3 target is needed. Turning them on later also asks for the endpoint.</p>}
        <div className="sech" style={{margin: "12px 0 8px"}}><h2>External key management</h2><span className="ln"></span>
          <Tog on={c.kmsEnabled} set={v => up(setC, {kmsEnabled: v})} label="Use an external KMS" hint="encryption keys are fetched per volume" /></div>
        {c.kmsEnabled ? <>
          <div className="frow">
            <Fl l="Provider" sm><select className="sel" value={c.kmsProvider} onChange={e => up(setC, {kmsProvider: e.target.value})}>
              <option value="vault">HashiCorp Vault</option><option value="aws-kms">AWS KMS</option><option value="azure-keyvault">Azure Key Vault</option><option value="gcp-kms">Google Cloud KMS</option></select></Fl>
            <Fl l="Address" sub="API endpoint"><Inp v={c.kmsAddress} set={v => up(setC, {kmsAddress: v})} placeholder="https://vault.internal:8200" /></Fl>
            <Fl l="Key name" sm><Inp v={c.kmsKeyName} set={v => up(setC, {kmsKeyName: v})} placeholder="simplyblock-prod" /></Fl>
          </div>
          <div className="frow">
            <Fl l="Authentication" sm><select className="sel" value={c.kmsAuth} onChange={e => up(setC, {kmsAuth: e.target.value})}>
              <option value="token">token</option><option value="approle">AppRole</option><option value="kubernetes">Kubernetes service account</option><option value="iam">IAM role</option></select></Fl>
            <Fl l="Token / secret" sub="stored in a Kubernetes secret" sm><Inp type="password" v={c.kmsToken} set={v => up(setC, {kmsToken: v})} /></Fl>
            <Fl l="Verify TLS" sm><select className="sel" value={c.kmsVerifyTls ? "1" : "0"} onChange={e => up(setC, {kmsVerifyTls: e.target.value === "1"})}><option value="1">yes</option><option value="0">no</option></select></Fl>
          </div>
          <p className="mdesc" style={{margin: 0}}>Encrypted volumes then hold no key material: the node fetches the data-encryption key from the KMS at attach time. Without a KMS, keys are generated and kept by the control plane.</p>
        </> : <p className="mdesc" style={{margin: 0}}>Keys for encrypted volumes are generated and stored by the control plane. An external KMS can also be configured after deployment.</p>}
      </div></div>

      <div className="wzsum">
        <div className="stats" style={{margin: 0}}>
          <Stat k="Nodes" v={selected.length} s={mode === "tag" ? "matched by label" : "picked"} />
          <Stat k="Storage nodes" v={plan.nodes} s="one per NUMA socket" />
          <Stat k="Devices" v={plan.devices} s="claimed at deployment" />
          <Stat k="Raw capacity" v={fmtBytes(plan.raw)} s={`${fmtBytes(usable)} usable at ${c.ec}`} />
          <Stat k="Hugepages" v={fmtBytes(hpPerNode * plan.nodes, 0)} s={`${fmtBytes(hpPerNode, 0)} per node${c.hugepagesGb ? " (override)" : ""}`} />
        </div>
        {err && <div className="banner" style={{color: "var(--bad)"}}><Icon n="alert" s={15} /><span>{err.message}</span></div>}
        <div className="btnrow">
          <button className="btn" onClick={() => nav.discovery(kid)}>Cancel</button>
          <button className="btn primary" disabled={!ready || busy} onClick={create}>{busy ? "Writing document…" : "Write deployment document"}</button>
        </div>
        <p className="mdesc" style={{margin: 0}}>{ready ? "The document is written unapproved. Review it, then approve it to start the deployment."
          : !c.name ? "Name the cluster." : !c.mgmtNic || !c.dataNic1 ? "Choose the management and at least one data NIC."
          : nicMissing.length ? `${nicMissing.length} node(s) lack the chosen NIC names.`
          : plan.nodes === 0 || plan.devices === 0 ? "Select at least one node with one device."
          : fdMissing.length ? `Give every node a failure domain label — ${fdMissing.length} still unlabelled.`
          : fdTooFew ? "Failure domains need at least two distinct labels across the selected nodes."
          : fdThin.length ? `${fdThin.join(", ")} would carry a single node. Each failure domain needs at least two storage nodes — select them in pairs.`
          : fdSpread > 1 ? `The domains are unbalanced (${fdNames.map(f => f + ": " + fdTally[f]).join(", ")}). Node counts may differ by at most one.`
          : s3Missing ? "Backups are on: give the S3 endpoint, bucket and credentials."
          : kmsMissing ? "The KMS needs an address and a key name." : ""}</p>
      </div>
    </div>
  );
}

Object.assign(window, {DiscoveryPanel, HostInventory, DeployWizard, discoveryDialog, deployFromDialog, FilterProps});
