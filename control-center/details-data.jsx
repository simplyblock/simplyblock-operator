function PoolDetail({o: p, nav}) {
  return (
    <div>
      <DetailHead obj={p} title={p.name} badge={<><span className="badge">storage pool</span>
        {p.dhchap && <span className="badge" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"}}>dhchap bi-dir</span>}
        {!p.enabled && <span className="badge" style={{color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 45%,transparent)"}}>disabled</span>}</>} />
      {p.dhchap && <div className="banner" style={{color: "var(--ro)", background: "color-mix(in srgb,var(--ro) 7%,var(--panel))", borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"}}>
        <Icon n="lock" s={15} /><span><b>Bi-directional DH-CHAP.</b> Host and subsystem authenticate each other on every connection. It was set when the pool was created and cannot be turned off — a pool without mutual authentication has to be created separately.</span></div>}
      {!p.enabled && <div className="banner" style={{color: "var(--warn)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"}}>
        <Icon n="alert" s={15} /><span><b>Pool disabled.</b> Existing volumes keep serving I/O normally — only provisioning of new volumes into this pool is blocked.</span></div>}
      <div className="stats">
        <Stat k="Volumes" v={p.counts.volumes} s={`${p.counts.volumesOnline} online`} />
        <Stat k="Provisioned" v={fmtBytes(p.capacity.total)} />
        <Stat k="Utilized" v={fmtBytes(p.capacity.used)} s={pct(p.capacity.used, p.capacity.total).toFixed(0) + "% of provisioned"} />
        <Stat k="Volume data" v={fmtBytes(p.lvolBytes)} />
        <Stat k="Snapshot data" v={fmtBytes(p.snapshotBytes)} s={`${p.counts.snapshots} snapshots`} />
        <Stat k="Backup chains" v={p.counts.backups} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="volume" title="Logical volumes" sub="in this pool" count={p.counts.volumes} onClick={() => nav.layer(p, "volumes")} />
        <NavCard icon="camera" title="Snapshots" sub="scoped to this pool" count={p.counts.snapshots} onClick={() => nav.layer(p, "snapshots")} />
        <NavCard icon="cloud" title="Backups" sub="scoped to this pool" count={p.counts.backups} onClick={() => nav.layer(p, "backups")} />
        {p.counts.storageClasses > 0 && <NavCard icon="k8s" title="StorageClasses" sub="generated from this pool" count={p.counts.storageClasses} onClick={() => nav.layer(p, "storageclasses")} />}
      </div>
      <div className="dcols">
        <div className="card"><h3>Pool properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Name", p.name], ["State", <TrafficLight status={p.status} />],
            ["Provisioning", p.enabled ? "allowed" : "blocked — pool disabled"],
            ["Node affinity", "none — pools are cluster-wide and carry no primary, secondary or tertiary node"],
            ["StorageClasses", p.storageClasses.length
              ? <span style={{display: "flex", flexDirection: "column", gap: 3, alignItems: "flex-end"}}>
                  {p.storageClasses.map(sc => (
                    <span key={sc.uuid} style={{display: "flex", gap: 7, alignItems: "baseline"}}>
                      <em style={{fontStyle: "normal", color: "var(--dim2)", fontSize: 10.5}}>{sc.k8s_cluster}</em>
                      <Ref onClick={() => nav.openStorageClass(sc.uuid)} label={sc.name} />
                    </span>
                  ))}
                </span>
              : null],
            ["Cluster", <Ref onClick={() => nav.openCluster(p.clusterId)} label={regName(p.clusterId)} />],
            ["Volumes", p.counts.volumes], ["Provisioned", fmtBytes(p.capacity.total)],
            ["Volume data", fmtBytes(p.lvolBytes)], ["Snapshot data", fmtBytes(p.snapshotBytes)],
            ["Utilized", `${fmtBytes(p.capacity.used)} · ${pct(p.capacity.used, p.capacity.total).toFixed(0)}%`]
          ]} /></div></div>
        <div className="card"><h3>Quality of service</h3><div className="bd"><QosProps qos={p.qos} fallback="No QoS limits configured on this pool." /></div></div>
      </div>
    </div>
  );
}

function VolumeDetail({o: v, nav}) {
  const nref = (r, label) => r ? [label, <Ref onClick={() => nav.openNode(v.clusterId, r.uuid)} label={r.hostname} />] : [label, null];
  const primaryIp = v.nodes.primary && REG[v.nodes.primary.uuid] ? REG[v.nodes.primary.uuid].ip : (v.nodes.primary ? v.nodes.primary.hostname : "—");
  return (
    <div>
      <DetailHead obj={v} title={v.name}
        badge={<>{v.crypto ? <span className="badge" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"}}>encrypted</span> : <span className="badge">unencrypted</span>}
          {v.dataReduction && <span className="badge">comp-dedup</span>}
          {v.baseSnapshot && <span className="badge">clone</span>}</>}
        sub={<span style={{fontSize: 11.5, color: "var(--dim)"}}>pool <Ref onClick={() => nav.openPool(v.clusterId, v.poolId)} label={v.poolName} /></span>} />
      {v.migration && <div className="banner" style={{color: "var(--info)", background: "color-mix(in srgb,var(--info) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"}}>
        <Icon n="move" s={15} />{v.migration.instant
          ? <span><b>Moved instantly.</b> The primary went from {v.migration.from} to {v.migration.target} without copying data ({String(v.migration.reason).replace("_", " ")}). The move was recorded as an <span className="mono">lvol_migration</span> task on the cluster.</span>
          : <span><b>Migration queued.</b> Primary will move to {v.migration.target} at the next opportunity.</span>}</div>}
      {v.affinity && v.affinity.satisfied === false && <div className="banner" style={{color: "var(--warn)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"}}>
        <Icon n="alert" s={15} /><span><b>Affinity not satisfied.</b> The workload {v.affinity.workload} runs on {v.affinity.workload_node}, but front storage is elsewhere. An instant migration would restore locality.</span></div>}
      <div className="stats">
        <Stat k="Provisioned" v={fmtBytes(v.capacity.total)} />
        <Stat k="Utilized" v={fmtBytes(v.capacity.used)} s={pct(v.capacity.used, v.capacity.total).toFixed(0) + "%"} />
        <Stat k="IOPS read" v={fmtNum(v.iops.r)} /><Stat k="IOPS write" v={fmtNum(v.iops.w)} />
        <Stat k="Snapshots" v={v.counts.snapshots} s={`${v.counts.snapshotsBackedUp} backed up`} />
        <Stat k="Backup versions" v={v.counts.backupVersions || 0} s={v.backupChainId ? "one chain" : "no backup chain"} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="camera" title="Snapshots" sub="of this volume" count={v.counts.snapshots} onClick={() => nav.layer(v, "snapshots")} />
        <NavCard icon="cloud" title="Backup chain" sub={v.backupChainId ? "versions in object storage" : "not backed up yet"} count={v.counts.backupVersions || 0} onClick={() => nav.layer(v, "backups")} />
      </div>
      <div className="dcols">
        <div>
          <div className="card"><h3>Volume properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
            <Props rows={[
              ["Name", v.name], ["Status", <TrafficLight status={v.status} />],
              ["Pool", <Ref onClick={() => nav.openPool(v.clusterId, v.poolId)} label={v.poolName} />],
              ["Cluster", <Ref onClick={() => nav.openCluster(v.clusterId)} label={regName(v.clusterId)} />],
              nref(v.nodes.primary, "Primary node"), nref(v.nodes.secondary, "Secondary node"), nref(v.nodes.tertiary, "Tertiary node"),
              ["Base snapshot", v.baseSnapshot
                ? <Ref onClick={() => nav.openSnapshot(v.clusterId, v.baseSnapshot.uuid)} label={v.baseSnapshot.snapshot_name} /> : null],
              ["Backup policy", v.backupPolicy
                ? <Ref onClick={() => nav.layerRef(v.clusterId, "policies")} label={v.backupPolicy.policy_name} /> : null],
              ["Replication policy", v.replication
                ? <Ref onClick={() => nav.openRPolicy(v.replication.policyId)} label={v.replication.policyName} /> : null],
              v.replication ? ["Replication mode", v.replication.mode] : null,
              v.replication ? ["Replication status", <TrafficLight status={v.replication.status} />] : null,
              v.replication ? ["Last replication", `${fmtDate(v.replication.lastAt)} · ${fmtAgo(v.replication.lastAt)}`] : null,
              v.replication ? ["Backlog", v.replication.mode === "synchronous" ? "none (synchronous)" : fmtBytes(v.replication.backlog)] : null,
              (v.consistencyGroups || []).length ? ["Consistency groups", <span className="labels" style={{margin: 0}}>
                {v.consistencyGroups.map(g => <button className="lab link" key={g.uuid} onClick={() => nav.openCgroup(g.uuid)}><Icon n="link" s={10} />{g.name}</button>)}</span>] : null,
              ["Bucket", v.bucket
                ? <Ref onClick={() => nav.openBucket(v.bucket.uuid)} label={v.bucket.name} /> : null],
              ["PVC", v.pvc
                ? <Ref onClick={() => nav.openPvc(v.pvc.uuid)} label={v.pvc.namespace + "/" + v.pvc.name} /> : "not provisioned via CSI"],
              v.pvc ? ["Kubernetes cluster", v.pvc.k8s_cluster] : null,
              v.pvc ? ["Storage class", v.pvc.storage_class] : null,
              v.pvc ? ["Workload", v.pvc.workload] : null,
              ["Affinity", v.affinity
                ? (v.affinity.mode === "pod"
                    ? `pod — follows ${v.affinity.workload}${v.affinity.satisfied === false ? " (not satisfied)" : ""}`
                    : `node — pinned to ${v.affinity.pinned_node}`)
                : "none"],
              v.migration ? ["Last move", v.migration.instant
                ? `${v.migration.from} → ${v.migration.target} · instant · ${String(v.migration.reason).replace("_", " ")}`
                : `queued → ${v.migration.target}`] : null,
              ["Encryption", v.crypto ? "enabled" : "disabled"],
              ["Compression-dedup", v.dataReduction ? "enabled" : "disabled"],
              v.dataReduction ? ["Logical vs on disk",
                `${fmtBytes(v.logicalUsed)} → ${fmtBytes(v.capacity.used)} · ${(v.logicalUsed / Math.max(1, v.capacity.used)).toFixed(2)}×`] : null,
              ["Created", fmtDate(v.createdAt)]
            ]} /></div></div>
          <div className="card" style={{marginTop: 12}}><h3>Quality of service</h3><div className="bd">
            <QosProps qos={v.qos} fallback={`No volume-level QoS — limits inherited from pool ${v.poolName}.`} /></div></div>
          <div className="card" style={{marginTop: 12}}><h3>Connection</h3><div className="bd">
            <div className="code">{`nvme connect --transport=tcp \\\n  --traddr=${primaryIp} --trsvcid=4420 \\\n  --nqn=${v.nqn}`}</div>
          </div></div>
        </div>
        <div>
          <VolumeReplicationCard v={v} nav={nav} />
          <div style={{marginTop: 12}}><IOCards o={v} /></div>
        </div>
      </div>
    </div>
  );
}

function VolumeReplicationCard({v, nav}) {
  const r = v.replication;
  if (!r) return (
    <div className="card"><h3>Replication</h3><div className="bd">
      <div className="nolim">Not replicated. Attach this volume to a replication policy from the policy&rsquo;s volume list, or from the volume actions.</div>
    </div></div>
  );
  const sync = r.mode === "synchronous";
  return (
    <div className="card"><h3 style={{display: "flex", alignItems: "center", gap: 9}}>Replication
      <TrafficLight status={r.status} />
      <span style={{flex: 1}}></span>
      <span className="badge">{r.mode}</span></h3>
      <div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
        <Props rows={[
          ["Policy", <Ref onClick={() => nav.openRPolicy(r.policyId)} label={r.policyName} />],
          ["Status", <TrafficLight status={r.status} />],
          ["Last replication", sync ? "continuous" : `${clockOf(r.lastAt)} · ${fmtAgo(r.lastAt)}`],
          ["Backlog", sync ? "none — writes are acknowledged at every zone" : fmtBytes(r.backlog)],
          ["Generations kept", sync ? null : (r.generations || "latest only")],
          ["Consistency group", r.consistencyGroup || "none"]
        ]} />
      </div>
    </div>
  );
}

function SnapshotDetail({o: s, nav}) {
  return (
    <div>
      <DetailHead obj={s} title={s.name}
        badge={<><span className="badge">snapshot</span>{s.seq && <span className="badge">generation {s.seq}</span>}
          {s.backupVersionId
            ? <span className="badge" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"}}>backed up · {s.backupVersionId}</span>
            : <span className="badge">not backed up</span>}</>}
        sub={<span style={{fontSize: 11.5, color: "var(--dim)"}}>{fmtDate(s.createdAt)} · {fmtAgo(s.createdAt)}</span>} />
      <div className="stats">
        <Stat k="Delta size" v={fmtBytes(s.capacity.total)} />
        <Stat k="Taken" v={fmtAgo(s.createdAt)} s={fmtDate(s.createdAt)} />
        <Stat k="Generation" v={s.seq || "—"} s={s.parentId ? "chains to predecessor" : "chains to volume"} />
        <Stat k="Backup version" v={s.backupVersionId || "none"} c={s.backupVersionId ? "var(--ok)" : undefined} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Snapshot properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Name", s.name], ["Status", <TrafficLight status={s.status} />],
            ["Created", fmtDate(s.createdAt)], ["Delta size", fmtBytes(s.capacity.total)],
            ["Generation", s.seq], ["Chains to", s.parentId ? "predecessor snapshot" : "the volume itself"],
            ["Backup version", s.backupVersionId || null],
            ["Base volume", <Ref onClick={() => nav.openVolume(s.clusterId, s.poolId, s.volumeId)} label={s.volumeName} />],
            ["Pool", <Ref onClick={() => nav.openPool(s.clusterId, s.poolId)} label={s.poolName} />],
            ["Cluster", <Ref onClick={() => nav.openCluster(s.clusterId)} label={regName(s.clusterId)} />]
          ]} /></div></div>
        <div className="card"><h3>Snapshots and backups</h3><div className="bd">
          <p className="mdesc">Copy-on-write snapshot, chained as a delta against its predecessor. It shares blocks with the base volume and grows only as the volume diverges.</p>
          <p className="mdesc" style={{marginBottom: 0}}>{s.backupVersionId
            ? <>A backup version was taken from this snapshot. The two are now <b>independent objects</b> — deleting this online snapshot leaves version <span className="mono">{s.backupVersionId}</span> in the bucket untouched.</>
            : <>No backup has been taken from this snapshot yet. Backups are always taken <b>from a snapshot</b>, never from the live volume.</>}</p>
        </div></div>
      </div>
    </div>
  );
}

function BackupDetail({o: b, nav}) {
  const vs = b.versions || [];
  const merge = v => window.__ui.dialog({
    title: `Merge ${v.id} into its predecessor?`, danger: v.seq === 2,
    desc: v.seq === 2
      ? "This is the earliest delta. Merging it grows the full version to include its changes and removes the delta — this is how older retention is aged out. Restoring to a point in time before this version is no longer possible afterwards."
      : `The changes in ${v.id} are folded into version ${vs[v.seq - 2] ? vs[v.seq - 2].id : "the predecessor"}, which then covers both. ${v.id} disappears from the chain.`,
    confirm: "Merge", run: () => api.backupMergeVersion(b.id, v.id)
  }, b);
  return (
    <div>
      <DetailHead obj={b} title={b.chainId}
        badge={<><span className="badge">backup chain</span><span className="badge">{b.counts.versions} version{b.counts.versions === 1 ? "" : "s"}</span>
          {b.policyName && <span className="badge k8s">{b.policyName}</span>}</>}
        sub={<span style={{fontSize: 11.5, color: "var(--dim)"}}>
          of <Ref onClick={() => nav.openVolume(b.clusterId, b.poolId, b.volumeId)} label={b.volumeName} /> · latest {fmtDate(b.latestAt)}</span>} />
      <div className="stats">
        <Stat k="Versions" v={b.counts.versions} s="1 full + deltas" />
        <Stat k="Full version" v={fmtBytes(b.fullBytes)} />
        <Stat k="Deltas" v={fmtBytes(b.deltaBytes)} />
        <Stat k="Chain size" v={fmtBytes(b.capacity.total)} />
        <Stat k="Oldest point" v={fmtAgo(b.createdAt)} s={fmtDate(b.createdAt)} />
        <Stat k="Merged so far" v={b.counts.merged} s={b.lastMergeAt ? `last ${fmtAgo(b.lastMergeAt)}` : "never merged"} />
      </div>
      <div className="sech"><h2>Version chain</h2><span className="ln"></span>
        <span className="count">oldest first · restore picks a point in time</span></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Version</th><th>Type</th><th>Taken from snapshot</th><th>Tier</th><th>Created</th><th style={{textAlign: "right"}}>Size</th><th>Merged</th><th></th></tr></thead>
          <tbody>{vs.map(v => (
            <tr key={v.id}>
              <td className="mono" style={{fontWeight: 600}}>{v.id}</td>
              <td><span className={"badge " + (v.type === "full" ? "k8s" : "")}>{v.type}</span></td>
              <td className="mono" style={{color: "var(--dim)"}}>{v.snapshotName || <span style={{color: "var(--dim2)"}}>snapshot deleted</span>}</td>
              <td className="mono" style={{color: "var(--dim)"}}>{v.tier}</td>
              <td className="mono">{fmtDate(v.createdAt)}</td>
              <td className="mono" style={{textAlign: "right"}}>{fmtBytes(v.size)}</td>
              <td className="mono" style={{color: v.merged ? "var(--warn)" : "var(--dim2)"}}>{v.merged ? `${v.merged} folded in` : "—"}</td>
              <td style={{textAlign: "right"}}>
                <button className="chip" disabled={v.seq === 1} title={v.seq === 1 ? "the full version has no predecessor" : "Merge into predecessor"}
                  onClick={() => merge(v)}>Merge</button>
              </td>
            </tr>
          ))}</tbody></table>
      </div></div>
      <div className="dcols">
        <div className="card"><h3>Chain properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Chain id", b.chainId], ["Status", <TrafficLight status={b.status} />],
            ["Volume", <Ref onClick={() => nav.openVolume(b.clusterId, b.poolId, b.volumeId)} label={b.volumeName} />],
            ["Pool", <Ref onClick={() => nav.openPool(b.clusterId, b.poolId)} label={b.poolName} />],
            ["Cluster", <Ref onClick={() => nav.openCluster(b.clusterId)} label={regName(b.clusterId)} />],
            ["Backup policy", b.policyName
              ? <Ref onClick={() => nav.layerRef(b.clusterId, "policies")} label={b.policyName} /> : "manual backups only"],
            ["Oldest point in time", fmtDate(b.createdAt)],
            ["Newest version", fmtDate(b.latestAt)],
            ["Last merge", b.lastMergeAt ? `${fmtDate(b.lastMergeAt)} · ${fmtAgo(b.lastMergeAt)}` : null],
            b.exportedTo ? ["Exported to", `${b.exportedTo} (${b.exportedVersion || "latest"})`] : null
          ]} /></div></div>
        <div>
          <div className="card"><h3>How this chain ages</h3><div className="bd">
            <p className="mdesc">Every backup version was taken <b>from a snapshot</b>. Once taken, the two are independent: deleting the online snapshot leaves its backup version in the bucket unchanged.</p>
            <p className="mdesc">Versions are chained exactly like snapshots — one full version followed by deltas. <b>Merging</b> a version folds it into its predecessor. Merging the earliest delta grows the full version and drops that delta, which is how retention ages out without ever losing the full baseline.</p>
            <p className="mdesc" style={{marginBottom: 0}}>Deleting this backup deletes the <b>entire chain</b>; individual versions can only leave it by being merged.</p>
          </div></div>
          <div className="card" style={{marginTop: 12}}><h3>Bucket location</h3><div className="bd">
            <div className="code">{b.bucket}{b.chainId}/</div>
          </div></div>
        </div>
      </div>
    </div>
  );
}

function PolicyDetail({o: p, nav}) {
  const span = r => {
    const n = parseInt(r.interval, 10), unit = r.interval.replace(/[\d]/g, "");
    const mins = n * (unit === "m" ? 1 : unit === "h" ? 60 : unit === "d" ? 1440 : 10080);
    const total = mins * r.versions;
    return total >= 1440 ? Math.round(total / 1440) + " d" : total >= 60 ? Math.round(total / 60) + " h" : total + " min";
  };
  return (
    <div>
      <DetailHead obj={p} title={p.name} badge={<><span className="badge">backup policy</span>{p.consistencyGroup && <span className="badge" style={{color: "var(--ro)"}}>group-consistent</span>}</>} />
      {p.consistencyGroup && <div className="banner" style={{color: "var(--ro)", background: "color-mix(in srgb,var(--ro) 7%,var(--panel))", borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"}}>
        <Icon n="link" s={15} /><span><b>Group-consistent.</b> All linked volumes form one consistency group; every cycle snapshots and backs them up atomically, and every retained version is a recovery point an application can be failed over to — the basis for ransomware recovery.</span></div>}
      <div className="stats">
        <Stat k="Finest interval" v={p.finest || "—"} s="snapshot + backup cadence" />
        <Stat k="Versions retained" v={p.counts.versions} s="across all tiers" />
        <Stat k="Snapshots online" v={p.counts.online} s="the rest live only as backups" />
        <Stat k="Volumes linked" v={p.counts.volumes} />
        <Stat k="Backup chains" v={p.counts.chains} />
        <Stat k="Coverage" v={p.schedule.length ? span(p.schedule[p.schedule.length - 1]) : "—"} s="oldest recoverable point" />
      </div>
      <div className="sech"><h2>Schedule</h2><span className="ln"></span>
        <span className="count">each row is a tier: cadence, retained versions, snapshots kept online</span></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Every</th><th style={{textAlign: "right"}}>Versions kept</th><th style={{textAlign: "right"}}>Snapshots online</th><th style={{textAlign: "right"}}>Covers</th><th>Merge cadence</th></tr></thead>
          <tbody>{p.schedule.map((r, i) => (
            <tr key={i}>
              <td className="mono" style={{fontWeight: 600}}>{r.interval}</td>
              <td className="mono" style={{textAlign: "right"}}>{r.versions}×</td>
              <td className="mono" style={{textAlign: "right", color: r.online ? "var(--accent)" : "var(--dim2)"}}>{r.online ? r.online + "×" : "—"}</td>
              <td className="mono" style={{textAlign: "right", color: "var(--dim)"}}>{span(r)}</td>
              <td style={{color: "var(--dim)", fontSize: 11}}>every {r.interval} once {r.versions} versions exist</td>
            </tr>
          ))}</tbody></table>
      </div></div>
      <div className="dcols">
        <div className="card"><h3>Policy properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Name", p.name], ["Schedule rows", p.schedule.length],
            ["Finest interval", p.finest], ["Versions retained", p.counts.versions],
            ["Snapshots kept online", p.counts.online],
            ["Volumes linked", p.counts.volumes], ["Backup chains", p.counts.chains],
            ["Cluster", <Ref onClick={() => nav.openCluster(p.clusterId)} label={regName(p.clusterId)} />],
            ["Created", fmtDate(p.createdAt)]
          ]} /></div></div>
        <div className="card"><h3>What the schedule means</h3><div className="bd">
          <p className="mdesc">Each row is one tier and carries three things at once: how often a snapshot is taken and backed up, how many backup <b>versions</b> of that tier are retained, and how many of those snapshots stay <b>online</b> on the cluster.</p>
          <p className="mdesc">Once a tier holds more versions than it retains, the oldest is <b>merged</b> into its predecessor — so the interval is also the merge cadence. Snapshots beyond the online count are deleted from the cluster; their backup versions stay in the bucket.</p>
          <p className="mdesc" style={{marginBottom: 0}}>Written the short way, this policy reads:
            <span className="code" style={{marginTop: 7, display: "block"}}>{p.schedule.map(r => `${r.interval}\t${r.versions}x${r.online ? `\t${r.online}x online` : ""}`).join("\n")}</span></p>
        </div></div>
      </div>
    </div>
  );
}

const DETAILS = {cluster: ClusterDetail, host: HostDetail, node: NodeDetail, device: DeviceDetail,
  pool: PoolDetail, volume: VolumeDetail, snapshot: SnapshotDetail, backup: BackupDetail, policy: PolicyDetail};
// pair / rpolicy / zone live in dr.jsx — resolved at render time so load order cannot break the shell
const DETAIL_KIND = {pair: "PairDetail", rpolicy: "RPolicyDetail", zone: "ZoneDetail",
  plan: "PlanDetail", site: "SiteDetail", slot: "SlotDetail", replops: "ReplOpsDetail", cgroup: "CgroupDetail", cgsnapshot: "CgSnapshotDetail", migration: "MigrationDetail",
  k8sc: "K8sDetail", storageclass: "StorageClassDetail", pvc: "PvcDetail", bucket: "BucketDetail",
  protectedapp: "ProtectedAppDetail",
  deployconfig: "DeployConfigDetail", mpath: "MPathDetail", appgroup: "AppGroupDetail"};
const Detail = ({obj, nav}) => {
  const C = DETAILS[obj.kind] || (DETAIL_KIND[obj.kind] ? window[DETAIL_KIND[obj.kind]] : null);
  return C ? <C o={obj} nav={nav} /> : null;
};

Object.assign(window, {PoolDetail, VolumeDetail, VolumeReplicationCard, SnapshotDetail, BackupDetail, PolicyDetail, Detail});
