// ---------------------------------------------------------------------------
// DEPLOYMENT DOCUMENT — tile, detail, per-node progress, general log and
// drill-in to the step / node logs.
// ---------------------------------------------------------------------------
const STEP_PH = {Succeeded: "var(--ok)", Running: "var(--info)", Failed: "var(--bad)", Pending: "var(--idle)"};
const NODE_DONE = {Configured: 1, Added: 1};
const nodePhaseColor = p => NODE_DONE[p] ? "var(--ok)" : p === "Pending" ? "var(--dim2)" : p === "Rebooting" ? "var(--warn)" : "var(--info)";

const StepTracker = ({steps, onLog}) => (
  <div className="steps">{steps.map((s, i) => (
    <div key={s.name} className={"stp " + s.phase.toLowerCase()}>
      <div className="sn" style={{"--c": STEP_PH[s.phase]}}>{s.phase === "Succeeded" ? <Icon n="check" s={11} /> : i + 1}</div>
      <div style={{minWidth: 0, flex: 1}}>
        <div className="sl" style={{display: "flex", gap: 8, alignItems: "center"}}>{s.label}
          {onLog && s.phase !== "Pending" && s.name === "ActivateCluster" && <button className="chip" style={{marginLeft: "auto"}} onClick={() => onLog(s.name)}><Icon n="list" s={11} />Log</button>}</div>
        <div className="sm">{s.message || (s.phase === "Pending" ? "waiting" : s.phase.toLowerCase())}{s.finishedAt ? ` · ${fmtAgo(s.finishedAt)}` : ""}</div>
        {s.phase === "Running" && <div className="sbar"><i style={{width: (s.progress || 3) + "%"}}></i></div>}
      </div>
    </div>))}</div>
);

const approveDialog = d => ({
  title: `Approve and deploy ${d.name}?`, confirm: "Approve and deploy",
  desc: `Approval is one-way. The cluster ${d.cluster.name} is created in the control plane, then three asynchronous steps run: the ${d.counts.hosts} worker node(s) are configured (persistent hugepages, core isolation — this reboots them), ${d.counts.nodes} storage node(s) are added in parallel, and the cluster is activated.`,
  run: () => api.deployConfigApprove(d.name)
});

function DeployConfigTile({d, nav}) {
  const running = d.steps.find(s => s.phase === "Running");
  const done = d.steps.filter(s => s.phase === "Succeeded").length;
  return (
    <div className="tile" style={{"--sc": STATUS_META[d.status].c}} onDoubleClick={() => nav.detail(d)}>
      <TileHead obj={d} left={<><TrafficLight status={d.status} /><Name>{d.name}</Name></>} right={<span className="badge">{d.cluster ? (d.cluster.deviceClass === "nvme" ? "NVMe" : "block") : ""}</span>} />
      <Uuid value={d.id} />
      <div className="labels">
        <span className="lab"><i>cluster</i>{(d.cluster && d.cluster.name) || "—"}</span>
        {d.cluster && <span className="lab"><i>ec</i>{d.cluster.ec}</span>}
        <span className="lab"><i>core isolation</i>{d.sizing.coreIsolation ? "yes" : "no"}</span>
      </div>
      <div className="kv">
        <div><span>Nodes</span><b>{d.counts.hosts}</b></div><div><span>Storage nodes</span><b>{d.counts.nodes}</b></div>
        <div><span>Devices</span><b>{d.counts.devices}</b></div><div><span>Hugepages</span><b>{fmtBytes(d.sizing.hugepages, 0)}</b></div>
      </div>
      {d.status === "Deploying" && <div className="prepbox running">
        <span style={{flex: 1}}>Step {done + 1} of {d.steps.length} — {running ? running.message || running.label : "starting"}</span>
        <div className="sbar" style={{width: 80}}><i style={{width: ((done + (running ? (running.progress || 3) / 100 : 0)) / d.steps.length * 100) + "%"}}></i></div>
      </div>}
      {d.status === "Draft" && <div className="prepbox">Awaiting approval. Nothing has been applied to any node yet.</div>}
      <Foot items={[
        d.status === "Draft" ? {label: "Approve", icon: "check", onClick: () => window.__ui.dialog(approveDialog(d), d)} : null,
        d.clusterId ? {label: "Cluster", icon: "cluster", onClick: () => nav.openCluster(d.clusterId)} : null,
        {label: "Details", right: true, onClick: () => nav.detail(d)}
      ]} />
    </div>
  );
}

// ---- logs ------------------------------------------------------------------
function DeployLog({d, onOpen}) {
  const [step, setStep] = useState("");
  const [node, setNode] = useState("");
  const [q, setQ] = useState("");
  const lines = d.log.filter(l => (!step || l.step === step) && (!node || l.node === node) && (!q || l.msg.toLowerCase().includes(q.toLowerCase()))).slice().reverse();
  const steps = [...new Set(d.log.map(l => l.step))];
  return (
    <div className="card"><h3>Deployment log<span className="live" style={{marginLeft: 8}}><i></i>live</span></h3>
      <div className="logtools">
        <select className="sel" value={step} onChange={e => setStep(e.target.value)}><option value="">all steps</option>{steps.map(s => <option key={s}>{s}</option>)}</select>
        <select className="sel" value={node} onChange={e => setNode(e.target.value)}><option value="">all nodes</option>{d.nodes.map(n => <option key={n.host}>{n.host}</option>)}</select>
        <input className="inp" style={{width: 200}} placeholder="search" value={q} onChange={e => setQ(e.target.value)} />
        <span className="cnt" style={{marginLeft: "auto", fontSize: 11, color: "var(--dim2)"}}>{lines.length} of {d.log.length}</span>
        <CopyBtn get={() => lines.map(l => `${l.ts} ${l.level} ${l.step}${l.node ? " " + l.node : ""} ${l.msg}`).join("\n")} />
      </div>
      <div className="logstream" style={{maxHeight: 320}}>
        {!lines.length ? <div className="lmsg">{d.status === "Draft" ? "The log starts when the document is approved." : "No lines match."}</div>
          : lines.map((l, i) => <div className="lrow" key={i}>
            <span className="lts">{(l.ts || "").replace("T", " ").replace(/(\.\d+)?Z$/, "")}</span>
            <span className="lstep">{l.step}</span>
            <span className="lnode">{l.node ? <button className="lab link" onClick={() => onOpen(`${l.step}/${l.node}`)}>{l.node}</button> : "—"}</span>
            <span className="lmsgtxt">{l.msg}</span>
          </div>)}
      </div>
    </div>
  );
}

// the actual log of one step, or of one node within a step
function StepLogViewer({d, name, setName}) {
  const [q, setQ] = useState("");
  const {data, loading, error, reload} = useResource("dlog|" + d.id + "|" + name, () => api.deployLog(d.id, name), 2000);
  const lines = (data || []).filter(l => !q || l.msg.toLowerCase().includes(q.toLowerCase()));
  const options = d.steps.filter(s => s.phase !== "Pending").flatMap(s => s.name === "ActivateCluster" ? [s.name] : d.nodes.map(n => `${s.name}/${n.host}`));
  return (
    <div className="card"><h3>Log · <span className="mono" style={{fontWeight: 400}}>{name}</span>
      <button className="kebab" style={{marginLeft: "auto"}} onClick={() => setName(null)} title="Close"><Icon n="x" s={12} /></button></h3>
      <div className="logtools">
        <select className="sel" value={name} onChange={e => setName(e.target.value)}>{options.map(o => <option key={o}>{o}</option>)}</select>
        <input className="inp" style={{width: 200}} placeholder="filter" value={q} onChange={e => setQ(e.target.value)} />
        <span style={{marginLeft: "auto"}}></span>
        <CopyBtn get={() => lines.map(l => `${l.ts} ${l.level} ${l.msg}`).join("\n")} />
      </div>
      <LogStream lines={lines} loading={loading} error={error} onRetry={reload} height={300} empty="No output yet." />
    </div>
  );
}

function NodeProgress({d, step, onOpen}) {
  return (
    <div className="card"><h3>{step === "ConfigureNodes" ? "Worker node configuration" : "Storage nodes"}<span className="cnt" style={{marginLeft: 8, fontSize: 11, color: "var(--dim2)"}}>{d.nodes.filter(n => NODE_DONE[n.phase]).length}/{d.nodes.length} done</span></h3>
      <div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
        {d.nodes.map(n => (
          <div key={n.host} className="nodest">
            <span className="nm"><b>{n.host}</b> <span className="mdesc" style={{margin: 0, display: "inline"}}>{n.message || ""}</span></span>
            <span className="ph" style={{color: nodePhaseColor(n.phase), display: "flex", gap: 6, alignItems: "center"}}>
              <Dot c={nodePhaseColor(n.phase)} />{n.phase}
              {n.phase !== "Pending" && <button className="chip" onClick={() => onOpen(`${step}/${n.host}`)}><Icon n="list" s={10} />log</button>}
              {n.storageNodeIds.map(id => <button key={id} className="chip" onClick={() => window.__nav && window.__nav.openNode(d.clusterId, id)}><Icon n="node" s={10} />node</button>)}
            </span>
            {!NODE_DONE[n.phase] && n.phase !== "Pending" && <div className="sbar"><i style={{width: n.progress + "%"}}></i></div>}
          </div>))}
      </div>
    </div>
  );
}

function DeployConfigDetail({o: d, nav}) {
  const [logName, setLogName] = useState(null);
  const running = d.steps.find(s => s.phase === "Running");
  const perNodeStep = running && running.name !== "ActivateCluster" ? running : d.steps.filter(s => s.name !== "ActivateCluster" && s.phase === "Succeeded").slice(-1)[0];
  const byHost = {};
  d.groups.forEach(g => { (byHost[g.node] = byHost[g.node] || []).push(g); });
  const cl = d.cluster || {};
  window.__nav = nav;
  return (
    <div>
      <DetailHead obj={d} title={d.name} badge={<><span className="badge">{cl.deviceClass === "nvme" ? "NVMe" : "block devices"}</span>{d.environment && <span className="badge">{d.environment}</span>}</>}
        sub={<span className="mono" style={{fontSize: 11.5, color: "var(--dim)"}}>ClusterDeploymentConfig/{d.name}</span>} />

      {d.status === "Draft" && <div className="banner" style={{color: "var(--accent)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}>
        <Icon n="alert" s={15} /><span><b>Awaiting approval.</b> Nothing has been applied to any node — approving is what starts the deployment.</span>
        <button className="btn primary" style={{marginLeft: "auto", flex: "none"}} onClick={() => window.__ui.dialog(approveDialog(d), d)}>Approve and deploy</button></div>}
      {d.status === "Deploying" && running && <div className="banner" style={{color: "var(--info)"}}><span className="dots"><i></i><i></i><i></i></span>
        <span><b>Step {d.steps.indexOf(running) + 1} of {d.steps.length}: {running.label}.</b> {running.message || ""}</span></div>}

      <div className="stats">
        <Stat k="Phase" v={<TrafficLight status={d.status} />} s={d.message || "—"} />
        <Stat k="Nodes" v={d.counts.hosts} s={perNodeStep ? `${d.nodes.filter(n => NODE_DONE[n.phase]).length}/${d.nodes.length} ${perNodeStep.name === "ConfigureNodes" ? "configured" : "added"}` : "worker nodes"} />
        <Stat k="Storage nodes" v={d.counts.nodes} s="one per NUMA socket" />
        <Stat k="Devices" v={d.counts.devices} />
        <Stat k="Hugepages" v={fmtBytes(d.sizing.hugepages, 0)} s={cl.hugepagesOverride ? "override" : `from ${cl.maxSubsystems} subsystems/node`} />
        <Stat k="vCPU" v={d.sizing.vcpu} s={cl.coreIsolation ? "core isolation on" : "no core isolation"} />
      </div>

      <div className="sech"><h2>Deployment</h2><span className="ln"></span>{d.clusterId && <button className="chip" onClick={() => nav.openCluster(d.clusterId)}><Icon n="cluster" s={11} />Open cluster</button>}</div>
      <div className="dcols">
        <div className="card"><h3>Steps</h3><div className="bd">
          {d.status === "Draft" && <p className="mdesc">These run asynchronously once approved. The cluster record exists from approval on (status unready); it is usable after the last step.</p>}
          <StepTracker steps={d.steps} onLog={setLogName} />
        </div></div>
        {d.status !== "Draft" && perNodeStep ? <NodeProgress d={d} step={perNodeStep.name} onOpen={setLogName} />
          : <div className="card"><h3>Cluster</h3><div className="bd"><ClusterProps cl={cl} d={d} nav={nav} /></div></div>}
      </div>

      {d.status !== "Draft" && <>
        {logName && <StepLogViewer d={d} name={logName} setName={setLogName} />}
        <DeployLog d={d} onOpen={setLogName} />
      </>}

      <div className="dcols">
        {d.status !== "Draft" && <div className="card"><h3>Cluster</h3><div className="bd"><ClusterProps cl={cl} d={d} nav={nav} /></div></div>}
        <div className="card"><h3>Device filter</h3><div className="bd">
          <FilterProps f={d.filter} />
          <div className="nolim">Applied on top of the discovery filter; per-node picks in the node sets below are the final selection.</div>
        </div></div>
      </div>

      <div className="sech"><h2>Node sets</h2><span className="ln"></span></div>
      {Object.entries(byHost).map(([host, gs]) => (
        <div key={host} className="card"><h3>{host}
          <span className="labels" style={{marginLeft: 8}}><span className="lab"><i>storage nodes</i>{gs.length}</span><span className="lab"><i>mgmt nic</i>{gs[0].mgmtNic}</span>{gs[0].failureDomain && <span className="lab"><i>failure domain</i>{gs[0].failureDomain}</span>}</span></h3>
          <div className="bd" style={{padding: 0}}>
            <table className="dt"><thead><tr><th>Socket</th><th>Devices</th><th>Data NICs</th><th>vCPU</th><th>Hugepages</th><th>System RAM</th><th>Max subsystems</th><th>Core isolation</th></tr></thead><tbody>
              {gs.map(g => <tr key={g.socket}>
                <td className="mono">{g.socket}</td><td className="mono" style={{maxWidth: 280}}>{[].concat(g.nvme, g.block).join(", ") || "—"}</td>
                <td className="mono">{g.dataNics.join(", ") || "—"}</td><td className="mono">{g.vcpu}</td>
                <td className="mono">{fmtBytes(g.hugepages, 0)}</td><td className="mono">{fmtBytes(g.systemMemory, 0)}</td>
                <td className="mono">{g.maxSubsystems}</td><td>{g.coreIsolation ? "yes" : "no"}</td></tr>)}
            </tbody></table>
          </div>
        </div>))}

      <div className="card"><h3>Provenance</h3><div className="bd">
        <Props rows={[
          ["Kubernetes cluster", d.k8sClusterId ? <Ref onClick={() => nav.k8sDetail(d.k8sClusterId)} label={d.k8sClusterName || regName(d.k8sClusterId)} /> : d.k8sClusterName || "—"],
          ["Node selector", Object.keys(d.nodeSelector).length ? Object.entries(d.nodeSelector).map(([a, b]) => `${a}=${b}`).join(", ") : "picked individually"],
          ["Approved", d.approved ? "yes" : "no — not applied"], ["Created", fmtDate(d.createdAt)]
        ]} />
      </div></div>
    </div>
  );
}

const ClusterProps = ({cl, d, nav}) => (
  <Props rows={[
    ["Name", cl.name], ["Device type", cl.deviceClass === "nvme" ? "NVMe (PCIe)" : "Linux block devices"],
    ["Erasure coding", cl.ec], ["NUMA sockets", (cl.numaSockets || []).map(s => "socket " + s).join(", ")],
    ["Per storage node", `${cl.vcpu} vCPU · ${cl.systemMemoryGb} GB RAM · ${cl.maxSubsystems} subsystems`],
    ["Hugepages", cl.hugepagesOverride ? `${fmtBytes(cl.hugepagesOverride, 0)} (override)` : `derived from subsystems`],
    ["NICs", `${cl.mgmtNic} (mgmt) · ${(cl.dataNics || []).join(", ")} (data)`],
    ["Options", [cl.coreIsolation && "core isolation", cl.failureDomains && "failure domains", cl.backups && "backups", cl.objectStorage && "S3 object storage"].filter(Boolean).join(" · ") || "—"],
    d.clusterId ? ["Cluster", <Ref onClick={() => nav.openCluster(d.clusterId)} label={regName(d.clusterId) || cl.name} />] : null
  ].filter(Boolean)} />
);

Object.assign(window, {DeployConfigTile, DeployConfigDetail, StepTracker, approveDialog, DeployLog, StepLogViewer});
