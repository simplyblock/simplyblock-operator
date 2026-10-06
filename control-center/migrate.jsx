// ---------------------------------------------------------------------------
// MIGRATION PATHS — online migration of VMs, containers and their volumes from
// site A to site B inside a stretched Kubernetes cluster.
// ---------------------------------------------------------------------------
const clockOfM = s => { const d = new Date(s); return isNaN(d) ? "—" : d.toISOString().slice(11, 19); };
const MPH = window.MIG_PHASES || ["Queued", "Replicating", "Converged", "MovingWorkloads", "MigratingVolumes", "Cleanup", "Completed"];
const PH_LABEL = {Queued: "queued", Replicating: "replicating", Converged: "converged", MovingWorkloads: "moving workloads", MigratingVolumes: "migrating volumes", Cleanup: "cleanup", Completed: "completed"};
const PH_HINT = {
  Queued: "waits for the groups ahead of it in the queue",
  Replicating: "an asynchronous replication policy copies the group's volumes to the target site; the backlog per volume shrinks until it is zero",
  Converged: "every volume's backlog is zero — the workloads can move without losing data",
  MovingWorkloads: "VMs are live-migrated with KubeVirt, containers are rescheduled to the target site; storage still comes from site A",
  MigratingVolumes: "instant volume migration moves each volume's primary to the target nodes while it is in use",
  Cleanup: "the source copies and the replication policy are deleted",
  Completed: "the group runs entirely at the target site"
};
const phaseIdx = ph => MPH.indexOf(ph);
const PhaseStrip = ({phase, compact}) => {
  const i = phaseIdx(phase);
  return <div className="phases">{MPH.filter(p => p !== "Queued" || !compact).map((p, k) => {
    const j = phaseIdx(p);
    const cls = "ph" + (phase === "Paused" ? "" : j < i ? " done" : j === i ? " cur" : "");
    return <React.Fragment key={p}>{k > 0 && <span className="arr">›</span>}<span className={cls}>{j < i && phase !== "Paused" ? <Icon n="check" s={9} /> : null}{PH_LABEL[p]}</span></React.Fragment>;
  })}</div>;
};

// ---- dialogs -----------------------------------------------------------------
const newMPathDialog = () => ({
  title: "New migration path", confirm: "Create path",
  desc: "A path moves application groups from a source storage cluster to a target storage cluster inside one Kubernetes cluster that spans both sites. The target site's nodes must already be part of that Kubernetes cluster and have a storage class on the target storage cluster.",
  fields: v => [
    {k: "name", label: "Name", type: "text", required: true, placeholder: "site-a-to-site-b"},
    {k: "k8s_cluster_id", label: "Stretched Kubernetes cluster", type: "select", required: true,
      load: () => api.k8sClusters().then(ks => ks.filter(k => k.storageClusterIds.length >= 2).map(k => ({v: k.id, l: `${k.name} · ${k.storageClusterIds.length} storage clusters`}))),
      empty: "No Kubernetes cluster consumes two storage clusters yet — add nodes at the target site first."},
    {k: "source_cluster_id", label: "Source storage cluster (site A)", type: "select", required: true,
      load: () => api.clusters().then(cs => cs.map(c => ({v: c.id, l: c.name})))},
    {k: "target_cluster_id", label: "Target storage cluster (site B)", type: "select", required: true,
      load: () => api.clusters().then(cs => cs.map(c => ({v: c.id, l: c.name})))},
    {k: "n1", type: "note", label: "Volumes replicate asynchronously to B first; the move itself happens only once a group's backlog is zero."}
  ],
  run: v => api.mpathCreate({name: v.name, k8s_cluster_id: v.k8s_cluster_id, source_cluster_id: v.source_cluster_id, target_cluster_id: v.target_cluster_id})
});

const newAppGroupDialog = p => ({
  title: `Add an application group to ${p.name || "the path"}`, confirm: "Queue group", wide: true,
  desc: "An application group is one unit of migration: its VMs and containers, and — identified from them — their PVCs and volumes. Groups run in queue order.",
  fields: [
    {k: "name", label: "Group name", type: "text", required: true, placeholder: "payments"},
    {k: "members", type: "members", k8sClusterId: p.k8sClusterId, label: "Workloads"},
    {k: "approval", label: "When replication has converged", type: "select", def: "manual",
      options: [{v: "manual", l: "Wait for approval before moving the workloads"}, {v: "auto", l: "Move the workloads automatically"}]}
  ],
  run: v => api.appGroupCreate(p.id, {name: v.name, namespace: (v.members || {}).namespace, members: (v.members || {}).members || [], approval: v.approval})
});

// namespace + discovered workloads (via the Kubernetes API), picked one by one
function MembersField({f, val, setVal}) {
  const v = val || {namespace: "", members: []};
  const [disc, setDisc] = useState(null);
  const {data: k} = useResource("mk8s|" + f.k8sClusterId, () => api.k8sCluster(f.k8sClusterId));
  const nss = (k && k.namespaces) || [];
  const discover = ns => {
    const init = {}; ["Deployment", "StatefulSet", "VirtualMachine"].forEach(x => { init[x] = {loading: true}; });
    setDisc(init);
    api.nsResourcesEach(ns, (kind, items, error) => { if (init[kind]) setDisc(d => Object.assign({}, d, {[kind]: {items: items || [], error}})); });
  };
  const has = (kind, name) => v.members.some(m => m.kind === kind && m.name === name);
  const toggle = (kind, name) => setVal(Object.assign({}, v, {members: has(kind, name) ? v.members.filter(m => !(m.kind === kind && m.name === name)) : v.members.concat({kind, name})}));
  return (
    <div className="field">
      <span className="flabel">Namespace</span>
      <div style={{display: "flex", gap: 6}}>
        <select className="finput" value={v.namespace} onChange={e => { setVal({namespace: e.target.value, members: []}); setDisc(null); }}>
          <option value="">choose…</option>{nss.map(n => <option key={n}>{n}</option>)}</select>
        <button type="button" className="chip" disabled={!v.namespace} onClick={() => discover(v.namespace)}><Icon n="search" s={11} />{disc ? "Re-discover" : "Discover workloads"}</button>
      </div>
      {disc && <div className="rcpdisc" style={{marginTop: 8}}>
        {["VirtualMachine", "StatefulSet", "Deployment"].map(kind => { const d = disc[kind] || {}; return <div key={kind} className="rcpkind">
          <b>{kind}</b>
          {d.loading ? <span className="dots"><i></i><i></i><i></i></span> : d.error ? <span className="mdesc" style={{margin: 0, color: "var(--warn)"}}>not available</span>
            : !d.items.length ? <span className="mdesc" style={{margin: 0}}>none</span>
            : <span style={{flex: 1, minWidth: 0, display: "flex", flexWrap: "wrap"}}>{d.items.map(i => <label key={i.metadata.name}>
                <span className={"selbox" + (has(kind, i.metadata.name) ? " on" : "")} onClick={() => toggle(kind, i.metadata.name)}>{has(kind, i.metadata.name) && <Icon n="check" s={10} />}</span>
                <span className="mono" style={{fontSize: 11}}>{i.metadata.name}</span></label>)}</span>}
        </div>; })}
        <span className="fhint">{v.members.length} workload(s) selected · VMs are live-migrated, containers are restarted on the target site. Their PVCs are resolved when the group is queued.</span>
      </div>}
      {!disc && <span className="fhint">Pick the namespace, then discover its VMs, StatefulSets and Deployments.</span>}
    </div>
  );
}

// ---- tiles -------------------------------------------------------------------
function MPathTile({m: p, nav}) {
  const {data: gs} = useResource("mpg|" + p.id, () => api.mpathGroups(p.id), 3000);
  const G = gs || [];
  const cur = G.find(g => !["Queued", "Completed", "Failed"].includes(g.phase));
  const done = G.filter(g => g.phase === "Completed").length;
  const backlog = G.reduce((n, g) => n + g.backlog, 0);
  return (
    <div className="tile" style={{"--sc": STATUS_META[p.status].c}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><TrafficLight status={p.status} /><Name>{p.name}</Name></>} right={<span className="badge">{done}/{G.length} groups</span>} />
      <Uuid value={p.id} />
      <div className="labels">
        <button className="lab link" onClick={e => { e.stopPropagation(); nav.openCluster(p.sourceClusterId); }}><i>from</i>{regName(p.sourceClusterId)}</button>
        <span className="lab" style={{border: "none", background: "none", padding: 0}}>→</span>
        <button className="lab link" onClick={e => { e.stopPropagation(); nav.openCluster(p.targetClusterId); }}><i>to</i>{regName(p.targetClusterId)}</button>
        <button className="lab link" onClick={e => { e.stopPropagation(); nav.k8sDetail(p.k8sClusterId); }}><i>k8s</i>{regName(p.k8sClusterId, "Kubernetes cluster")}</button>
      </div>
      <div className="kv">
        <div><span>Queued</span><b>{G.filter(g => g.phase === "Queued").length}</b></div><div><span>Completed</span><b>{done}</b></div>
        <div><span>Backlog</span><b style={backlog ? null : {color: "var(--ok)"}}>{fmtBytes(backlog)}</b></div><div><span>Volumes</span><b>{G.reduce((n, g) => n + g.counts.volumes, 0)}</b></div>
      </div>
      {cur ? <div className="prepbox running"><span className="dots"><i></i><i></i><i></i></span><span style={{flex: 1}}><b>{cur.name}</b> · {PH_LABEL[cur.phase] || cur.phase} — {cur.message}</span></div>
        : p.status === "paused" ? <div className="prepbox">Paused — nothing starts until the path is resumed.</div>
        : !G.length ? <div className="prepbox">No application groups yet. Add one to start moving workloads.</div> : null}
      <Foot items={[
        {label: "Groups", count: G.length, icon: "cluster", onClick: () => nav.layer(p, "appgroups")},
        {label: "Details", right: true, onClick: () => nav.detail(p)}
      ]} />
    </div>
  );
}

function AppGroupTile({g, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[g.phase].c}} onDoubleClick={() => nav.detail(g)}>
      <TileHead obj={g} left={<><TrafficLight status={g.phase} /><Name>{g.name}</Name></>} right={<span className="badge">#{g.order + 1}</span>} />
      <div className="tsub" style={{marginTop: 2}}>namespace {g.namespace}</div>
      <Uuid value={g.id} />
      <PhaseStrip phase={g.phase} compact />
      <div className="kv">
        <div><span>VMs</span><b>{g.counts.vms}</b></div><div><span>Containers</span><b>{g.counts.members - g.counts.vms}</b></div>
        <div><span>Volumes</span><b>{g.counts.volumes}</b></div><div><span>Backlog</span><b style={g.backlog ? null : {color: "var(--ok)"}}>{fmtBytes(g.backlog)}</b></div>
      </div>
      {g.message && g.phase !== "Completed" && <div className={"prepbox" + (["Queued", "Paused", "Failed"].includes(g.phase) ? "" : " running")}>{g.message}{g.phase === "Converged" && g.approval === "manual" ? " — approve the move from Actions." : ""}</div>}
      <Foot items={[
        {label: "Volumes", count: g.counts.volumes, icon: "volume", onClick: () => nav.layer(g, "volumes")},
        g.phase === "Converged" && g.approval === "manual" ? {label: "Move now", icon: "move", onClick: () => window.__ui.dialog(ACTIONS.appgroup(g).find(a => /Move/.test(a.label)).dialog, g)} : null,
        {label: "Details", right: true, onClick: () => nav.detail(g)}
      ]} />
    </div>
  );
}

// ---- details -----------------------------------------------------------------
function MPathDetail({o: p, nav}) {
  const {data: gs, reload} = useResource("mpgd|" + p.id, () => api.mpathGroups(p.id), 2500);
  const [q, setQ] = useState("");
  const [gf, setGf] = useState("");
  const G = gs || [];
  const cur = G.find(g => !["Queued", "Completed", "Failed"].includes(g.phase));
  const done = G.filter(g => g.phase === "Completed").length;
  const backlog = G.reduce((n, g) => n + g.backlog, 0);
  const move = async (i, d) => { const j = i + d; if (j < 0 || j >= G.length) return; const ids = G.map(g => g.id); [ids[i], ids[j]] = [ids[j], ids[i]]; try { await api.mpathReorder(p.id, ids); reload(); } catch (e) { window.__toast && window.__toast(e.message); } };
  const lines = p.log.filter(l => (!gf || l.groupId === gf) && (!q || l.msg.toLowerCase().includes(q.toLowerCase()))).slice().reverse();
  return (
    <div>
      <DetailHead obj={p} title={p.name} badge={<span className="badge">{done}/{G.length} groups completed</span>}
        sub={<span className="labels"><button className="lab link" onClick={() => nav.openCluster(p.sourceClusterId)}><i>site A</i>{regName(p.sourceClusterId)}{p.sourceZoneId ? ` · ${regName(p.sourceZoneId, "zone")}` : ""}</button><span style={{color: "var(--dim2)"}}>→</span><button className="lab link" onClick={() => nav.openCluster(p.targetClusterId)}><i>site B</i>{regName(p.targetClusterId)}{p.targetZoneId ? ` · ${regName(p.targetZoneId, "zone")}` : ""}</button><button className="lab link" onClick={() => nav.k8sDetail(p.k8sClusterId)}><i>kubernetes</i>{regName(p.k8sClusterId, "Kubernetes cluster")}</button></span>} />
      {p.status === "paused" && <div className="banner" style={{color: "var(--warn)"}}><Icon n="alert" s={15} /><span><b>Paused.</b> A group already moving finishes its current step; no new step and no new group starts until the path is resumed.</span></div>}
      {cur && <div className="banner" style={{color: "var(--info)"}}><span className="dots"><i></i><i></i><i></i></span><span><b>{cur.name} — {PH_LABEL[cur.phase] || cur.phase}.</b> {cur.message}</span>
        <button className="chip" style={{marginLeft: "auto"}} onClick={() => nav.detail(cur)}>Open group</button></div>}
      <div className="stats">
        <Stat k="Status" v={<TrafficLight status={p.status} />} s={cur ? `${cur.name} in progress` : p.status === "completed" ? "all groups migrated" : "idle"} />
        <Stat k="Application groups" v={G.length} s={`${done} completed · ${G.filter(g => g.phase === "Queued").length} queued`} />
        <Stat k="Workloads" v={G.reduce((n, g) => n + g.counts.members, 0)} s={`${G.reduce((n, g) => n + g.counts.vms, 0)} VMs`} />
        <Stat k="Volumes" v={G.reduce((n, g) => n + g.counts.volumes, 0)} s={`${G.reduce((n, g) => n + g.counts.migrated, 0)} at site B`} />
        <Stat k="Backlog" v={fmtBytes(backlog)} c={backlog ? "var(--warn)" : "var(--ok)"} s="remaining to replicate" />
        <Stat k="Data" v={fmtBytes(G.reduce((n, g) => n + g.capacity.total, 0))} s="across all groups" />
      </div>

      <div className="sech"><h2>Queue</h2><span className="ln"></span>
        <button className="btn primary" onClick={() => window.__ui.dialog(newAppGroupDialog(p), p)}><Icon n="plus" s={12} />Add application group</button></div>
      <div className="card"><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
        {!G.length && <p className="mdesc" style={{margin: "8px 0"}}>No groups. Add the first application group — its VMs and containers, their PVCs are resolved automatically.</p>}
        {G.map((g, i) => <div key={g.id} className={"qrow" + (cur && cur.id === g.id ? " cur" : "")}>
          <span className="qn">{g.phase === "Completed" ? <Icon n="check" s={10} /> : i + 1}</span>
          <span className="qname"><button className="lab link" style={{border: "none", background: "none", padding: 0, fontSize: 12.5, color: "var(--text)", fontFamily: "inherit"}} onClick={() => nav.detail(g)}>{g.name}</button><small>{g.namespace} · {g.counts.vms} VM · {g.counts.members - g.counts.vms} containers · {g.counts.volumes} volumes</small></span>
          <span className="qmeta"><TrafficLight status={g.phase} /></span>
          <span className="qmeta">{g.phase === "Replicating" ? `backlog ${fmtBytes(g.backlog)}` : g.phase === "MovingWorkloads" ? `${g.counts.moved}/${g.counts.members} moved` : g.phase === "MigratingVolumes" ? `${g.counts.migrated}/${g.counts.volumes} migrated` : g.phase === "Converged" ? (g.approval === "manual" ? "awaiting approval" : "auto") : g.finishedAt ? fmtAgo(g.finishedAt) : g.approval}</span>
          <span style={{display: "flex", gap: 2, justifyContent: "flex-end"}}>
            {g.phase === "Queued" && <><button className="kebab" disabled={i === 0 || G[i - 1].phase !== "Queued"} onClick={() => move(i, -1)} title="Earlier"><Icon n="chevu" s={11} /></button>
              <button className="kebab" disabled={i === G.length - 1} onClick={() => move(i, 1)} title="Later"><Icon n="chevd" s={11} /></button></>}
            <button className="kebab" onClick={() => nav.detail(g)} title="Open"><Icon n="chev" s={11} /></button>
          </span>
        </div>)}
      </div></div>

      {cur && <>
        <div className="sech"><h2>Current group · {cur.name}</h2><span className="ln"></span></div>
        <div className="dcols">
          <div className="card"><h3>Workloads</h3><div className="bd"><Members g={cur} /></div></div>
          <div className="card"><h3>Volumes and backlog</h3><div className="bd" style={{padding: 0}}><VolumeBacklog g={cur} nav={nav} /></div></div>
        </div>
      </>}

      <div className="card"><h3>Migration log<span className="live" style={{marginLeft: 8}}><i></i>live</span></h3>
        <div className="logtools">
          <select className="sel" value={gf} onChange={e => setGf(e.target.value)}><option value="">all groups</option>{G.map(g => <option key={g.id} value={g.id}>{g.name}</option>)}</select>
          <input className="inp" style={{width: 200}} placeholder="search" value={q} onChange={e => setQ(e.target.value)} />
          <span style={{marginLeft: "auto", fontSize: 11, color: "var(--dim2)"}}>{lines.length} of {p.log.length}</span>
          <CopyBtn get={() => lines.map(l => `${l.ts} ${l.level} ${l.msg}`).join("\n")} />
        </div>
        <LogStream lines={lines} loading={false} height={280} empty="Nothing logged yet." />
      </div>
    </div>
  );
}

const Members = ({g}) => (
  <div className="memlist">{g.members.map(m => <div key={m.kind + m.name} className="memrow">
    <span className="mk">{m.kind}</span><b style={{flex: 1, minWidth: 0, overflow: "hidden", textOverflow: "ellipsis"}}>{m.name}</b>
    {["LiveMigrating", "Restarting"].includes(m.state) && <div className="sbar"><i style={{width: m.progress + "%"}}></i></div>}
    <span style={{display: "flex", gap: 5, alignItems: "center", fontSize: 11, color: STATUS_META[m.state] ? STATUS_META[m.state].c : "var(--dim2)"}}><Dot c={STATUS_META[m.state] ? STATUS_META[m.state].c : "var(--dim2)"} />{m.state === "Pending" ? (phaseIdx(g.phase) < phaseIdx("MovingWorkloads") ? "at site A" : "pending") : (STATUS_META[m.state] || {}).label || m.state}</span>
  </div>)}</div>
);

function VolumeBacklog({g, nav}) {
  return (
    <table className="dt"><thead><tr><th>Volume</th><th style={{textAlign: "right"}}>Size</th><th style={{textAlign: "right"}}>Backlog</th><th>Last replication</th><th>Migration</th></tr></thead><tbody>
      {g.volumes.map(v => <tr key={v.id}>
        <td><button className="lab link" onClick={() => { const lv = REG[v.id]; lv ? nav.detail(lv) : api.volume(v.id).then(x => nav.detail(x)).catch(() => {}); }}>{v.name}</button></td>
        <td className="mono" style={{textAlign: "right"}}>{fmtBytes(v.size)}</td>
        <td className="mono" style={{textAlign: "right", color: v.backlog ? "var(--warn)" : "var(--ok)"}}>{v.backlog ? fmtBytes(v.backlog) : "0"}</td>
        <td className="mono">{v.lastAt ? clockOfM(v.lastAt) : "—"}</td>
        <td>{v.migrated ? <span className="lab" style={{color: "var(--ok)"}}>at site B</span> : g.phase === "MigratingVolumes" ? <div className="sbar" style={{width: 90}}><i style={{width: v.progress + "%"}}></i></div> : <span className="lab">at site A</span>}</td>
      </tr>)}
      {!g.volumes.length && <tr><td colSpan={5} className="lmsg">No bound PVC resolved for these workloads.</td></tr>}
    </tbody></table>
  );
}

function AppGroupDetail({o: g, nav}) {
  const path = REG[g.pathId];
  return (
    <div>
      <DetailHead obj={g} title={g.name} badge={<><span className="badge">#{g.order + 1} in queue</span><span className="badge">{g.approval === "auto" ? "moves automatically" : "approval required"}</span></>}
        sub={<span className="mono" style={{fontSize: 11.5, color: "var(--dim)"}}>namespace {g.namespace}{path ? <> · path <Ref onClick={() => nav.openMPath(g.pathId)} label={path.name} /></> : null}</span>} />
      {g.phase === "Converged" && g.approval === "manual" && <div className="banner" style={{color: "var(--accent)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}>
        <Icon n="check" s={15} /><span><b>Replication has converged.</b> Every volume's backlog is zero. Approve the move to live-migrate the VMs and restart the containers at site B.</span>
        <button className="btn primary" style={{marginLeft: "auto", flex: "none"}} onClick={() => window.__ui.dialog(ACTIONS.appgroup(g).find(a => /Move/.test(a.label)).dialog, g)}>Move workloads now</button></div>}
      {g.phase === "Paused" && <div className="banner" style={{color: "var(--warn)"}}><Icon n="alert" s={15} /><span><b>Paused.</b> The replication policy stays in place, so the backlog keeps being tracked; resume to continue.</span></div>}
      <PhaseStrip phase={g.phase} />
      <p className="mdesc">{PH_HINT[g.phase] || g.message}</p>
      <div className="stats">
        <Stat k="Phase" v={PH_LABEL[g.phase] || g.phase} c={STATUS_META[g.phase].c} s={g.message || ""} />
        <Stat k="Workloads" v={g.counts.members} s={`${g.counts.vms} VMs · ${g.counts.moved} moved`} />
        <Stat k="Volumes" v={g.counts.volumes} s={`${g.counts.migrated} at site B`} />
        <Stat k="Backlog" v={fmtBytes(g.backlog)} c={g.backlog ? "var(--warn)" : "var(--ok)"} s={g.backlog ? "still replicating" : "converged"} />
        <Stat k="Data" v={fmtBytes(g.capacity.total)} />
        <Stat k="Started" v={g.startedAt ? fmtAgo(g.startedAt) : "—"} s={g.finishedAt ? `finished ${fmtAgo(g.finishedAt)}` : ""} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="volume" title="Volumes" sub="with per-volume backlog" count={g.counts.volumes} onClick={() => nav.layer(g, "volumes")} />
        {g.rpolicyId && <NavCard icon="shield" title="Replication policy" sub="created under the hood for this group" count="→" onClick={() => nav.openRPolicy(g.rpolicyId)} />}
        {path && <NavCard icon="move" title="Migration path" sub={path.name} count="→" onClick={() => nav.openMPath(g.pathId)} />}
      </div>
      <div className="dcols">
        <div className="card"><h3>Workloads</h3><div className="bd"><Members g={g} />
          <div className="fnote" style={{marginTop: 10, marginBottom: 0}}><Icon n="alert" s={12} />VMs move by KubeVirt live migration without downtime; containers are restarted on a node at the target site. Both keep using their volumes over the network until the volumes follow.</div></div></div>
        <div className="card"><h3>Volumes and backlog</h3><div className="bd" style={{padding: 0}}><VolumeBacklog g={g} nav={nav} /></div></div>
      </div>
    </div>
  );
}

Object.assign(window, {MPathTile, AppGroupTile, MPathDetail, AppGroupDetail, MembersField, newMPathDialog, newAppGroupDialog, PhaseStrip});
