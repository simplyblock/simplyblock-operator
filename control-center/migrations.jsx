// ---------------------------------------------------------------------------
// CLUSTER / VOLUME MIGRATION
// Two mechanisms behind one object:
//  · intra_cluster — volumes move between nodes on a zone by instant migration
//  · cross_cluster — asynchronous replication ships the data, then a short IO
//    freeze applies the last small snapshot and rolls the NVMe paths over
// Operators taint the destination hosts and switch on "follow the workload".
// ---------------------------------------------------------------------------
const MIG_MODE = m => m === "cross_cluster"
  ? <span className="badge k8s">between clusters</span>
  : <span className="badge">within cluster</span>;

function MigrationTile({m, nav}) {
  const cross = m.mode === "cross_cluster";
  return (
    <div className="tile" style={{"--sc": STATUS_META[m.status].c}} onDoubleClick={() => nav.detail(m)}>
      <TileHead obj={m} left={<><TrafficLight status={m.status} /><Name>{m.name}</Name></>}
        right={MIG_MODE(m.mode)} />
      <Uuid value={m.id} />
      <div className="labels">
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openCluster(m.sourceClusterId);}}><i>from</i>{regName(m.sourceClusterId)}</button>
        {cross
          ? <button className="lab link" onClick={e => {e.stopPropagation(); nav.openCluster(m.targetClusterId);}}><i>to</i>{regName(m.targetClusterId)}</button>
          : <span className="lab"><i>to zone</i>{m.targetZoneId ? regName(m.targetZoneId, "zone") : "tainted hosts"}</span>}
        {m.followWorkload && <span className="lab" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}><Icon n="link" s={10} />follows workload</span>}
      </div>
      <div className="capwrap">
        <div className="caprow"><span>Progress <b className="mono" style={{color: "var(--accent)", fontWeight: 600}}>{m.progress}%</b></span>
          <span className="capval">{m.counts.moved} / {m.counts.volumes} volumes</span></div>
        <div className="bar"><i style={{width: m.progress + "%", "--bc": STATUS_META[m.status].c}}></i></div>
      </div>
      {cross ? (
        <div className="kv">
          <div><span>Iteration</span><b>{m.iterations} / {m.iterationLimit}</b></div>
          <div><span>Outstanding</span><b>{fmtBytes(m.lastSnapshot)}</b></div>
          <div><span>Freeze est.</span><b>{m.freezeMs} ms</b></div>
          <div><span>Shipping</span><b>{fmtBW(m.throughput)} GB/s</b></div>
        </div>
      ) : (
        <div className="kv">
          <div><span>Mechanism</span><b>instant</b></div>
          <div><span>Moved</span><b>{m.counts.moved}</b></div>
          <div><span>Started</span><b>{fmtAgo(m.startedAt)}</b></div>
        </div>
      )}
      {m.readyToCutover && <div className="prepbox ready">
        Outstanding snapshot is under the freeze threshold — ready to cut over.
      </div>}
      <Foot items={[
        {label: "Volumes", count: m.counts.volumes, icon: "volume", onClick: () => nav.layer(m, "volumes")},
        {label: "Details", right: true, onClick: () => nav.detail(m)}
      ]} />
    </div>
  );
}

function MigrationDetail({o: m, nav}) {
  const cross = m.mode === "cross_cluster";
  const shrink = m.firstSnapshot && m.lastSnapshot ? (m.firstSnapshot / m.lastSnapshot).toFixed(0) : null;
  return (
    <div>
      <DetailHead obj={m} title={m.name}
        badge={<>{MIG_MODE(m.mode)}<span className="badge">{m.scope === "cluster" ? "whole cluster" : "selected volumes"}</span>
          {m.followWorkload && <span className="badge" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"}}>follows workload</span>}</>}
        sub={<span style={{fontSize: 11.5, color: "var(--dim)"}}>
          <Ref onClick={() => nav.openCluster(m.sourceClusterId)} label={regName(m.sourceClusterId)} />
          {" → "}
          {cross
            ? <Ref onClick={() => nav.openCluster(m.targetClusterId)} label={regName(m.targetClusterId)} />
            : (m.targetZoneId ? <Ref onClick={() => nav.openZone(m.targetZoneId)} label={regName(m.targetZoneId, "zone")} /> : "tainted hosts")}
        </span>} />
      {m.readyToCutover && <div className="banner" style={{color: "var(--warn)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"}}>
        <Icon n="alert" s={15} /><span><b>Ready to cut over.</b> The outstanding snapshot is {fmtBytes(m.lastSnapshot)}, inside the {fmtBytes(m.freezeThreshold)} freeze threshold. Cutting over freezes IO for roughly {m.freezeMs} ms while the last delta is applied and the NVMe paths roll over.</span></div>}
      {m.status === "completed" && <div className="banner" style={{color: "var(--ok)", background: "color-mix(in srgb,var(--ok) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"}}>
        <Icon n="check" s={15} /><span><b>Migration complete.</b> {m.counts.moved} volume(s) now serve from the target.</span></div>}
      <div className="stats">
        <Stat k="Progress" v={m.progress + "%"} />
        <Stat k="Volumes" v={`${m.counts.moved}/${m.counts.volumes}`} s="moved" />
        {cross ? <>
          <Stat k="Iteration" v={`${m.iterations}/${m.iterationLimit}`} s="snapshot rounds" />
          <Stat k="Outstanding" v={fmtBytes(m.lastSnapshot)} s={`threshold ${fmtBytes(m.freezeThreshold)}`} />
          <Stat k="Freeze estimate" v={m.freezeMs + " ms"} c={m.freezeMs > 1000 ? "var(--warn)" : "var(--ok)"} />
          <Stat k="Shipping" v={fmtBW(m.throughput)} s="GB/s" />
        </> : <>
          <Stat k="Mechanism" v="instant" s="no data copied" />
          <Stat k="Started" v={fmtAgo(m.startedAt)} s={fmtDate(m.startedAt)} />
          <Stat k="Follow workload" v={m.followWorkload ? "on" : "off"} />
        </>}
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="volume" title="Volumes in this migration" sub="per-volume placement" count={m.counts.volumes} onClick={() => nav.layer(m, "volumes")} />
        {cross && <NavCard icon="swap" title="Cluster pair" sub="the link data ships over" count="→" onClick={() => nav.drLayer("pairs")} />}
      </div>
      <div className="dcols">
        <div className="card"><h3>Migration properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Name", m.name], ["State", <TrafficLight status={m.status} />],
            ["Mode", cross ? "between clusters / zones" : "within the cluster"],
            ["Scope", m.scope === "cluster" ? "every online volume" : `${m.counts.volumes} selected volume(s)`],
            ["Source cluster", <Ref onClick={() => nav.openCluster(m.sourceClusterId)} label={regName(m.sourceClusterId)} />],
            cross ? ["Target cluster", <Ref onClick={() => nav.openCluster(m.targetClusterId)} label={regName(m.targetClusterId)} />] : null,
            m.targetZoneId ? ["Target zone", <Ref onClick={() => nav.openZone(m.targetZoneId)} label={regName(m.targetZoneId, "zone")} />] : null,
            ["Target taint", m.targetTaint],
            ["Follow the workload", m.followWorkload ? "on — storage rolls over when the workload moves" : "off"],
            ["Started", fmtDate(m.startedAt)],
            m.completedAt ? ["Completed", `${fmtDate(m.completedAt)} · ${fmtAgo(m.completedAt)}`] : null,
            m.frozenAt ? ["IO frozen at", fmtDate(m.frozenAt)] : null,
            m.error ? ["Error", m.error] : null
          ]} /></div></div>
        <div>
          <div className="card"><h3>How this migration works</h3><div className="bd">
            {cross ? <>
              <p className="mdesc">Data ships over the cluster pair by asynchronous replication. Snapshots are taken <b>iteratively</b>, each one covering less change than the last{shrink ? ` — ${fmtBytes(m.firstSnapshot)} down to ${fmtBytes(m.lastSnapshot)}, a factor of ${shrink}` : ""}.</p>
              <p className="mdesc">Once the outstanding snapshot is small enough, cutting over <b>freezes IO briefly</b> ({m.freezeMs} ms at the current size), applies that last delta, and rolls the NVMe paths over to the target. Clients reconnect to the new target.</p>
              <p className="mdesc" style={{marginBottom: 0}}>Taint the destination hosts and switch on <b>follow the workload</b> to have storage roll over as the workload is rescheduled.</p>
            </> : <>
              <p className="mdesc">Volumes move between nodes with <b>instant migration</b>: the primary role is handed to another node without copying data, so each volume moves in one step.</p>
              <p className="mdesc" style={{marginBottom: 0}}>Targets are the hosts you tainted (<span className="mono">{m.targetTaint}</span>){m.targetZoneId ? <> in zone <b>{regName(m.targetZoneId, "zone")}</b></> : null}. With <b>follow the workload</b> on, front storage keeps moving as pods are rescheduled.</p>
            </>}
          </div></div>
          {cross && <div className="card" style={{marginTop: 12}}><h3>Convergence</h3><div className="bd">
            <AllocBar label="Outstanding snapshot vs freeze threshold"
              used={Math.min(m.lastSnapshot, m.freezeThreshold * 4)} total={m.freezeThreshold * 4} color="var(--accent)" />
            <div className="kv" style={{marginTop: 6}}>
              <div><span>First snapshot</span><b>{fmtBytes(m.firstSnapshot)}</b></div>
              <div><span>Latest</span><b>{fmtBytes(m.lastSnapshot)}</b></div>
              <div><span>Threshold</span><b>{fmtBytes(m.freezeThreshold)}</b></div>
            </div>
          </div></div>}
        </div>
      </div>
    </div>
  );
}

Object.assign(window, {MigrationTile, MigrationDetail, MIG_MODE});
