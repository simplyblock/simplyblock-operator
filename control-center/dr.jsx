// ---------------------------------------------------------------------------
// CROSS-CLUSTER CONFIGURATION — cluster pairs, DR replication policies, zones.
// Pairing is directional: a→b and b→a are two pairs, so bidirectional and
// fan-out topologies (a→b, b→a, a→c, b→c) are just several pairs.
// ---------------------------------------------------------------------------
const fmtMin = m => !m ? "—" : m < 60 ? `${m} min` : m % 60 === 0 ? `${m / 60} h` : `${Math.floor(m / 60)} h ${m % 60} min`;
const clockOf = s => { const d = new Date(s); return isNaN(d) ? "—" : d.toISOString().slice(11, 19); };
const MODE_BADGE = m => m === "synchronous"
  ? <span className="badge sync">synchronous</span>
  : <span className="badge k8s">asynchronous</span>;

const ScheduleTable = ({rows, compact}) => !rows || !rows.length
  ? <div className="nolim">No older generations retained — only the latest snapshot is kept.</div>
  : (
    <div className="rettable two">
      <div className="rethead"><span>Every</span><span>Generations kept</span></div>
      {rows.map((r, i) => <div className="retrow" key={i}><span>{r.interval}</span><b>{r.keep}×</b></div>)}
      {!compact && <div className="retrow" style={{background: "var(--panel2)"}}><span>total</span>
        <b>{rows.reduce((a, r) => a + r.keep, 0)}×</b></div>}
    </div>
  );

// ---- tiles -----------------------------------------------------------------
function ZoneTile({s, nav}) {
  return (
    <div className="tile" style={{"--sc": "var(--ok)"}} onDoubleClick={() => nav.detail(s)}>
      <TileHead obj={s} left={<><TrafficLight status="active" /><Name>{s.name}</Name></>}
        right={<span className="badge">{s.region}</span>} />
      <div className="tsub" style={{marginTop: 2}}>{s.location}</div>
      <Uuid value={s.id} />
      <div className="labels">
        <span className="lab mono" title="topology.kubernetes.io/zone"><i>zone</i>{s.name}</span>
        <span className="lab mono" title="topology.kubernetes.io/region"><i>region</i>{s.region}</span>
        <span className="lab"><i>racks</i>{s.racks.length || "—"}</span>
      </div>
      <div className="kv">
        <div><span>Hosts</span><b>{s.counts.hosts}</b></div>
        <div><span>Prepared</span><b>{s.counts.hostsPrepared}</b></div>
        <div><span>NVMe hosts</span><b>{s.counts.nvme}</b></div>
        <div><span>Storage nodes</span><b>{s.counts.nodes}</b></div>
      </div>
      <div className="zonestrip">{(s.k8sClusters || []).map(kc => (
        <button className="zonechip" key={kc.uuid} onClick={e => {e.stopPropagation(); nav.openK8s(kc.uuid);}}>
          <Icon n="k8s" s={10} />{kc.name}</button>
      ))}</div>
      {s.capacity.total > 0 && <div className="lab wide"><i>raw capacity</i>{fmtBytes(s.capacity.total)}</div>}
      <Foot items={[
        {label: "Hosts", count: s.counts.hosts, icon: "host", onClick: () => nav.layer(s, "hosts")},
        {label: "Clusters", count: s.counts.clusters, icon: "cluster", onClick: () => nav.layer(s, "clusters")},
        {label: "Details", right: true, onClick: () => nav.detail(s)}
      ]} />
    </div>
  );
}

// ---- details ---------------------------------------------------------------
function ZoneDetail({o: s, nav}) {
  return (
    <div>
      <DetailHead obj={s} title={s.name} badge={<><span className="badge">zone</span><span className="badge">{s.region}</span></>}
        sub={<span style={{fontSize: 11.5, color: "var(--dim)"}}>{s.location}</span>} />
      <div className="stats">
        <Stat k="Hosts" v={s.counts.hosts} s={`${s.counts.hostsPrepared} prepared`} />
        <Stat k="NVMe hosts" v={s.counts.nvme} s={`${s.counts.hosts - s.counts.nvme} non-NVMe`} />
        <Stat k="Storage nodes" v={s.counts.nodes} />
        <Stat k="Clusters present" v={s.counts.clusters} />
        <Stat k="Racks" v={s.racks.length} s={s.untaintedHosts ? s.untaintedHosts + " host(s) untainted" : (s.racks.join(", ") || "—")} />
        <Stat k="Raw capacity" v={fmtBytes(s.capacity.total)} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="host" title="Hosts" sub="racked in this zone" count={s.counts.hosts} onClick={() => nav.layer(s, "hosts")} />
        <NavCard icon="cluster" title="Clusters" sub="present in this zone" count={s.counts.clusters} onClick={() => nav.layer(s, "clusters")} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Zone properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Zone label", s.label], ["Region label", s.regionLabel],
            ["Location", s.location], ["Racks", s.racks.join(", ") || null],
            ["Kubernetes clusters", (s.k8sClusters || []).length
              ? <span style={{display: "flex", gap: 8, justifyContent: "flex-end", flexWrap: "wrap"}}>{s.k8sClusters.map(kc => <Ref key={kc.uuid} onClick={() => nav.openK8s(kc.uuid)} label={kc.name} />)}</span>
              : null],
            ["Hosts", s.counts.hosts], ["Storage nodes", s.counts.nodes],
            ["Clusters", s.clusterIds.length
              ? <span style={{display: "flex", gap: 8, justifyContent: "flex-end", flexWrap: "wrap"}}>{s.clusterIds.map(id => <Ref key={id} onClick={() => nav.openCluster(id)} label={regName(id)} />)}</span>
              : null],
            ["Created", fmtDate(s.createdAt)]
          ]} /></div></div>
        <div className="card"><h3>Zones, topology and replication</h3><div className="bd">
          <p className="mdesc">A zone is the standard Kubernetes failure domain, read from the <span className="mono">topology.kubernetes.io/zone</span> node label and grouped under <span className="mono">topology.kubernetes.io/region</span> — not a bespoke object. Worker nodes may additionally be tainted with a rack and cabinet; untainted hosts show no rack.</p>
          <p className="mdesc">Storage classes map these labels onto storage clusters through <span className="mono">zone_cluster_map</span> and <span className="mono">region_cluster_map</span>, so a pod scheduled here is provisioned from storage in the same zone.</p>
          <p className="mdesc">Zones are assigned to a storage cluster <b>at creation time only</b> and cannot be changed afterwards. Storage nodes can only ever be started on hosts inside those zones.</p>
          <p className="mdesc" style={{marginBottom: 0}}>Two or more zones is the precondition for <b>synchronous</b> replication. Asynchronous replication does not use zones — it runs between two clusters over a cluster pair.</p>
        </div></div>
      </div>
    </div>
  );
}

// ---- DR landing ------------------------------------------------------------
function DrHome({nav}) {
  const pairs = useResource("dr.pairs", () => api.pairs(), 8000);
  const pols = useResource("dr.pols", () => api.allRPolicies(), 6000);
  const zones = useResource("dr.zones", () => api.zones(), 20000);
  const clusters = useResource("dr.clusters", () => api.clusters(), 20000);
  const P = pols.data || [], PR = pairs.data || [], S = zones.data || [], C = clusters.data || [];
  const unhealthy = P.filter(p => p.status !== "healthy");
  const backlog = P.reduce((a, p) => a + (p.backlog || 0), 0);
  const attached = P.reduce((a, p) => a + p.counts.volumes, 0);
  const eligible = C.filter(c => c.drEligible).length;
  return (
    <div className="scroll" style={{paddingTop: 14}}>
      <div className="dhead" style={{paddingTop: 0}}>
        <div style={{minWidth: 0, flex: 1}}>
          <h1>Cross-cluster configuration</h1>
          <div style={{fontSize: 12.5, color: "var(--dim)", marginTop: 4, maxWidth: 720}}>
            Pair clusters, then attach replication and failback policies to those pairs. Synchronous replication is configured inside a single cluster that is stretched across zones.
          </div>
        </div>
        <button className="btn primary" onClick={() => window.__ui.dialog(newPairDialog(), {kind: "cluster pair", id: "new"})}><Icon n="plus" s={12} />Pair clusters</button>
      </div>
      <div className="stats">
        <Stat k="Cluster pairs" v={PR.length} s={`${PR.filter(p => p.status === "paired").length} healthy links`} />
        <Stat k="Policies" v={P.length} s={`${P.filter(p => p.mode === "synchronous").length} synchronous`} />
        <Stat k="Volumes" v={attached} s="protected by a policy" />
        <Stat k="Total backlog" v={fmtBytes(backlog)} c={unhealthy.length ? "var(--bad)" : undefined} />
        <Stat k="Unhealthy" v={unhealthy.length} c={unhealthy.length ? "var(--bad)" : "var(--ok)"} />
        <Stat k="DR-eligible" v={`${eligible}/${C.length}`} s="qualified as targets" />
      </div>
      {unhealthy.length > 0 && (
        <>
          <div className="sech"><h2>Needs attention</h2><span className="ln"></span></div>
          <div className="card"><div className="bd" style={{padding: 0}}>
            <table className="dt"><thead><tr><th>Policy</th><th>Mode</th><th>Route</th><th>Last replication</th><th style={{textAlign: "right"}}>Backlog</th><th>State</th></tr></thead>
              <tbody>{unhealthy.map(p => (
                <tr key={p.id} style={{cursor: "pointer"}} onClick={() => nav.detail(p)}>
                  <td className="mono" style={{fontWeight: 600}}>{p.name}</td>
                  <td>{MODE_BADGE(p.mode)}</td>
                  <td className="mono">{regName(p.sourceClusterId)}{p.mode === "synchronous" ? "" : ` → ${regName(p.targetClusterId)}`}</td>
                  <td className="mono">{clockOf(p.lastAt)} <span style={{color: "var(--dim2)"}}>· {fmtAgo(p.lastAt)}</span></td>
                  <td className="mono" style={{textAlign: "right", color: "var(--bad)"}}>{fmtBytes(p.backlog)}</td>
                  <td><TrafficLight status={p.status} /></td>
                </tr>
              ))}</tbody></table>
          </div></div>
        </>
      )}
      <div className="sech"><h2>Configure</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="swap" title="Cluster pairs" sub="links between clusters" count={PR.length} onClick={() => nav.drLayer("pairs")} />
        <NavCard icon="shield" title="DR policies" sub="a pair and a consistency group, or a stretched cluster — volumes and applications" count={P.length} onClick={() => nav.drLayer("rpolicies")} />
        <NavCard icon="zone" title="Zones" sub="hosts, racks, stretched clusters" count={S.length} onClick={() => nav.drLayer("zones")} />
        <NavCard icon="move" title="Migration paths" sub="online site-to-site migration of VMs, containers and their volumes" count="→" onClick={() => nav.drLayer("mpaths")} />
      </div>
      <div className="sech"><h2>Application DR · Ramen</h2><span className="ln"></span>
        <span className="count">the workload, not just the blocks</span></div>
      <AppDrSummary nav={nav} />
    </div>
  );
}

// Replication pairs and policies live in repl.jsx now, on the real CRDs. What
// remains here is the zone/topology layer and the shared schedule helpers.
Object.assign(window, {fmtMin, clockOf, MODE_BADGE, ScheduleTable, ZoneTile, ZoneDetail});
