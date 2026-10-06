function PoolTile({p, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[p.status].c}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><TrafficLight status={p.status} /><Name>{p.name}</Name></>}
        right={<span className="badge">{p.enabled ? "pool" : "no new volumes"}</span>} />
      <Uuid value={p.id} />
      <div className="labels">
        {p.dhchap && <span className="lab" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}} title="bi-directional DH-CHAP — set at creation, immutable"><Icon n="lock" s={10} />dhchap bi-dir</span>}
        {p.storageClasses.length
          ? p.storageClasses.map(sc => <button className="lab link" key={sc.uuid} title={"StorageClass " + sc.name}
              onClick={e => {e.stopPropagation(); nav.openStorageClass(sc.uuid);}}><Icon n="k8s" s={10} /><i>{sc.k8s_cluster || "class"}</i>{sc.variant}</button>)
          : <span className="lab" style={{opacity: .6}}><i>storage class</i>none</span>}
      </div>
      <QosChips qos={p.qos} />
      <Capacity label="Provisioned" total={p.capacity.total} used={p.capacity.used} />
      <div className="kv">
        <div><span>Volume data</span><b>{fmtBytes(p.lvolBytes)}</b></div>
        <div><span>Snapshots</span><b>{fmtBytes(p.snapshotBytes)}</b></div>
        <div><span>Utilized</span><b>{fmtBytes(p.capacity.used)}</b></div>
      </div>
      <Foot items={[
        {label: "Volumes", count: p.counts.volumes, icon: "volume", onClick: () => nav.layer(p, "volumes")},
        {label: "Snapshots", count: p.counts.snapshots, icon: "camera", onClick: () => nav.layer(p, "snapshots")},
        p.counts.storageClasses > 0 && {label: "Classes", count: p.counts.storageClasses, icon: "k8s", onClick: () => nav.layer(p, "storageclasses")},
        {label: "Details", right: true, onClick: () => nav.detail(p)}
      ]} />
    </div>
  );
}

function VolumeTile({v, nav}) {
  const link = (ref, role) => ref
    ? <button key={role} className="lab link" title={ref.hostname} onClick={e => {e.stopPropagation(); nav.openNode(v.clusterId, ref.uuid);}}><i>{role}</i>{ref.hostname.split("-").slice(-2).join("-")}</button>
    : null;
  return (
    <div className="tile" style={{"--sc": STATUS_META[v.status].c}} onDoubleClick={() => nav.detail(v)}>
      <TileHead obj={v} left={<><TrafficLight status={v.status} /><Name>{v.name}</Name></>}
        right={<>{v.dataReduction && <span className="lab" style={{color: "var(--accent)", borderColor: "var(--accent-line)"}} title="Compression-dedup enabled">comp-dedup</span>}
          {v.crypto
            ? <span className="lab" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"}}><Icon n="lock" s={11} />enc</span>
            : <span className="lab" style={{opacity: .6}}>no enc</span>}</>} />
      <Uuid value={v.id} />
      <div className="labels">
        {link(v.nodes.primary, "P")}{link(v.nodes.secondary, "S")}{link(v.nodes.tertiary, "T")}
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openPool(v.clusterId, v.poolId);}}><Icon n="pool" s={11} />{v.poolName}</button>
        {v.bucket && <button className="lab link" title="This volume is the filesystem behind an S3 bucket"
          onClick={e => {e.stopPropagation(); nav.openBucket(v.bucket.uuid);}}><Icon n="cloud" s={10} />{v.bucket.name}</button>}
        {v.pvc && <button className="lab link" title={"PVC " + v.pvc.namespace + "/" + v.pvc.name + " · " + v.pvc.k8s_cluster}
          onClick={e => {e.stopPropagation(); nav.openPvc(v.pvc.uuid);}}><Icon n="k8s" s={10} />{v.pvc.namespace}/{v.pvc.name}</button>}
      </div>
      {(v.baseSnapshot || v.backupPolicy || v.replication || v.migration) && (
        <div className="labels">
          {v.baseSnapshot && <button className="lab link" title={`Cloned from ${v.baseSnapshot.snapshot_name}`}
            onClick={e => {e.stopPropagation(); nav.openSnapshot(v.clusterId, v.baseSnapshot.uuid);}}><Icon n="camera" s={10} />{v.baseSnapshot.snapshot_name}</button>}
          {v.backupPolicy && <button className="lab link" onClick={e => {e.stopPropagation(); nav.layerRef(v.clusterId, "policies");}}><Icon n="clock" s={10} />{v.backupPolicy.policy_name}</button>}
          {v.replication && <button className="lab link" title={`${v.replication.mode} replication · ${v.replication.policyName}`}
            onClick={e => {e.stopPropagation(); nav.openRPolicy(v.replication.policyId);}}>
            <Icon n="shield" s={10} />{v.replication.policyName}</button>}
          {(v.consistencyGroups || []).map(g => <button className="lab link" key={g.uuid} style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}
            onClick={e => {e.stopPropagation(); nav.openCgroup(g.uuid);}}><Icon n="link" s={10} />{g.name}</button>)}
          {v.affinity && <span className="lab" style={v.affinity.satisfied === false
            ? {color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 40%,transparent)"}
            : {color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}
            title={v.affinity.mode === "pod" ? `follows workload ${v.affinity.workload}` : `pinned to ${v.affinity.pinned_node}`}>
            <Icon n="link" s={10} />{v.affinity.mode === "pod" ? "pod affinity" : "pinned"}
            {v.affinity.satisfied === false ? " · off-node" : ""}</span>}
          {v.migration && <span className="lab" style={{color: "var(--info)", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"}}
            title={v.migration.instant ? `moved instantly from ${v.migration.from} · ${v.migration.reason}` : ""}>
            <Icon n="move" s={10} />{v.migration.state === "completed" ? "moved → " : "migrating → "}{v.migration.target}</span>}
        </div>
      )}
      {v.replication && (
        <div className={"replbar" + (v.replication.status === "healthy" ? "" : " bad")}>
          <TrafficLight status={v.replication.status} sm />
          <span className="rl">{v.replication.mode === "synchronous" ? "sync" : "async"}</span>
          <span className="rv" title="Last replication">{clockOf(v.replication.lastAt)}</span>
          <span className="rv" title="Backlog">{v.replication.mode === "synchronous" ? "0 backlog" : fmtBytes(v.replication.backlog)}</span>
        </div>
      )}
      <Capacity label="Provisioned" total={v.capacity.total} used={v.capacity.used} />
      {v.dataReduction && v.logicalUsed > v.capacity.used && (
        <div className="kv">
          <div><span>Logical</span><b>{fmtBytes(v.logicalUsed)}</b></div>
          <div><span>On disk</span><b>{fmtBytes(v.capacity.used)}</b></div>
          <div><span>Saving</span><b style={{color: "var(--ok)"}}>{(v.logicalUsed / Math.max(1, v.capacity.used)).toFixed(2)}×</b></div>
        </div>
      )}
      <IoMetrics o={v} />
      <QosChips qos={v.qos} />
      <Foot items={[
        {label: "Snapshots", count: v.counts.snapshots, icon: "camera", onClick: () => nav.layer(v, "snapshots")},
        {label: "Backup", count: v.counts.backupVersions || 0, icon: "cloud", onClick: () => nav.layer(v, "backups")},
        {label: "Details", right: true, onClick: () => nav.detail(v)}
      ]} />
    </div>
  );
}

function SnapshotTile({s, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[s.status].c}} onDoubleClick={() => nav.detail(s)}>
      <TileHead obj={s} left={<><TrafficLight status={s.status} /><Name>{s.name}</Name></>}
        right={s.backupVersionId
          ? <span className="lab" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"}} title={`Backup version ${s.backupVersionId} was taken from this snapshot`}><Icon n="cloud" s={10} />{s.backupVersionId}</span>
          : <span className="lab" style={{opacity: .6}}>not backed up</span>} />
      <Uuid value={s.id} />
      <div className="labels">
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openVolume(s.clusterId, s.poolId, s.volumeId);}}><Icon n="volume" s={10} />{s.volumeName}</button>
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openPool(s.clusterId, s.poolId);}}><Icon n="pool" s={10} />{s.poolName}</button>
        <span className="lab"><i>gen</i>{s.seq || "—"}</span>
      </div>
      <div className="kv">
        <div><span>Taken</span><b>{fmtDate(s.createdAt)}</b></div>
        <div><span>Age</span><b>{fmtAgo(s.createdAt)}</b></div>
        <div><span>Delta size</span><b>{fmtBytes(s.capacity.total)}</b></div>
      </div>
      {!s.parentId && s.seq > 1 && <div className="nolim">Predecessor deleted — this snapshot now chains to the volume</div>}
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(s)}]} />
    </div>
  );
}

function BackupTile({b, nav}) {
  const vs = b.versions || [];
  const latest = vs[vs.length - 1];
  return (
    <div className="tile" style={{"--sc": STATUS_META[b.status].c}} onDoubleClick={() => nav.detail(b)}>
      <TileHead obj={b} left={<><TrafficLight status={b.status} /><Name>{b.volumeName}</Name></>}
        right={<span className="badge">{b.counts.versions} version{b.counts.versions === 1 ? "" : "s"}</span>} />
      <div className="uuid"><span>{b.chainId}</span></div>
      <div className="labels">
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openVolume(b.clusterId, b.poolId, b.volumeId);}}><Icon n="volume" s={10} />volume</button>
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openPool(b.clusterId, b.poolId);}}><Icon n="pool" s={10} />{b.poolName}</button>
        {b.policyName
          ? <button className="lab link" onClick={e => {e.stopPropagation(); nav.layerRef(b.clusterId, "policies");}}><Icon n="clock" s={10} />{b.policyName}</button>
          : <span className="lab" style={{opacity: .65}}><i>policy</i>manual</span>}
      </div>
      <div className="chainbar" title={`${vs.length} versions: 1 full + ${Math.max(0, vs.length - 1)} deltas`}>
        {vs.map(v => <i key={v.id} className={v.type} style={{flex: v.type === "full" ? 3 : 1}}></i>)}
      </div>
      <div className="kv">
        <div><span>Latest version</span><b>{latest ? latest.id : "—"}</b></div>
        <div><span>Taken</span><b>{fmtDate(b.latestAt)}</b></div>
        <div><span>Full</span><b>{fmtBytes(b.fullBytes)}</b></div>
        <div><span>Deltas</span><b>{fmtBytes(b.deltaBytes)}</b></div>
      </div>
      {b.lastMergeAt && <div className="nolim">Last merge {fmtAgo(b.lastMergeAt)} · {b.counts.merged} version(s) merged into the full so far</div>}
      <div className="bucket" title={b.bucket}><Icon n="cloud" s={11} />{b.bucket}</div>
      <Foot items={[
        {label: "Versions", count: b.counts.versions, icon: "cloud", onClick: () => nav.detail(b)},
        {label: "Details", right: true, onClick: () => nav.detail(b)}
      ]} />
    </div>
  );
}

function PolicyTile({p, nav}) {
  return (
    <div className="tile" style={{"--sc": "var(--ok)"}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><TrafficLight status="active" /><Name>{p.name}</Name></>}
        right={<span className="badge">{p.consistencyGroup ? "group-consistent" : "backup policy"}</span>} />
      <Uuid value={p.id} />
      <div className="labels">
        <span className="lab"><i>every</i>{p.finest || "—"}</span>
        {p.consistencyGroup && <span className="lab" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}><Icon n="link" s={10} />one consistency group</span>}
        <span className="lab"><i>volumes</i>{p.counts.volumes}</span>
        <span className="lab"><i>chains</i>{p.counts.chains}</span>
      </div>
      <BackupSchedule rows={p.schedule} />
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(p)}]} />
    </div>
  );
}

Object.assign(window, {PoolTile, VolumeTile, SnapshotTile, BackupTile, PolicyTile});
