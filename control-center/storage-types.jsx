// ---------------------------------------------------------------------------
// FILE (pNFS) AND OBJECT (S3) STORAGE
// File: one kernel NFS server on a control-plane worker serves metadata only;
// every worker is a pNFS data client, so data paths bypass it entirely. The
// metadata service keeps no local state, so it restarts on another worker in
// seconds. pNFS on the Linux NFS server supports XFS only, so RWX is XFS-only.
// Object: one bucket = one filesystem = one logical volume — blobs on cluster
// capacity, metadata in FoundationDB — so buckets inherit snapshots, backups
// and both replication modes from the volume beneath them.
// ---------------------------------------------------------------------------
const MDS_STATE = {active: "online", restarting: "in_restart", electing: "in_activation"};

function BucketTile({b, nav}) {
  const quota = b.quota || b.capacity.total;
  return (
    <div className="tile" style={{"--sc": STATUS_META[b.status].c}} onDoubleClick={() => nav.detail(b)}>
      <TileHead obj={b} left={<><TrafficLight status={b.status} /><Name>{b.name}</Name></>}
        right={<>{b.versioning && <span className="badge">versioned</span>}
          {b.objectLock && <span className="badge">locked</span>}
          {b.encrypted && <span className="lab" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"}}><Icon n="lock" s={11} />enc</span>}</>} />
      <Uuid value={b.id} />
      <div className="labels">
        <button className="lab link" title="One bucket is one filesystem is one logical volume"
          onClick={e => {e.stopPropagation(); nav.openVolumeById(b.volumeId);}}><Icon n="volume" s={10} />{b.volumeName}</button>
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openPool(b.clusterId, b.poolId);}}><Icon n="pool" s={10} />{b.poolName}</button>
        {b.access.public && <span className="lab" style={{color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 40%,transparent)"}}>public</span>}
      </div>
      <Capacity label={b.quota ? "Quota" : "Provisioned"} total={quota} used={b.capacity.used} />
      <div className="kv">
        <div><span>Objects</span><b>{fmtNum(b.objects)}</b></div>
        <div><span>Stored</span><b>{fmtBytes(b.capacity.used)}</b></div>
        <div><span>Snapshots</span><b>{b.counts.snapshots}</b></div>
        <div><span>Backups</span><b>{b.counts.backups}</b></div>
      </div>
      <div className="labels">
        <span className="lab" title="Kubernetes service account holding the bucket credentials">
          <Icon n="k8s" s={10} />{b.access.namespace}/{b.access.service_account}</span>
        <span className="lab"><i>policy</i>{b.access.policy}</span>
        {b.region && <span className="lab"><i>region</i>{b.region}</span>}
        {b.storageClass !== "standard" && <span className="lab"><i>class</i>{b.storageClass}</span>}
      </div>
      {!!b.counts.tags && <div className="labels tags" title="S3 bucket tags">
        {Object.entries(b.tags).slice(0, 5).map(([k, v]) => <span key={k} className="lab tag"><i>{k}</i>{v || "—"}</span>)}
        {b.counts.tags > 5 && <span className="lab">+{b.counts.tags - 5}</span>}
      </div>}
      {b.replication && <div className="labels">
        <span className="lab" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}>
          <Icon n="shield" s={10} />{b.replication.mode === "synchronous" ? "sync" : "async"} replicated</span>
      </div>}
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(b)}]} />
    </div>
  );
}

function BucketDetail({o: b, nav}) {
  const a = b.access || {};
  return (
    <div>
      <DetailHead obj={b} title={b.name}
        badge={<><span className="badge">bucket</span>{b.versioning && <span className="badge">versioning on</span>}
          {b.objectLock && <span className="badge">object lock</span>}</>}
        sub={<span style={{fontSize: 11.5, color: "var(--dim)"}}>
          on volume <Ref onClick={() => nav.openVolumeById(b.volumeId)} label={b.volumeName} /></span>} />
      {a.public && <div className="banner" style={{color: "var(--warn)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"}}>
        <Icon n="alert" s={15} /><span><b>Bucket is public.</b> Anonymous requests can read objects without presenting the service account credentials.</span></div>}
      <div className="stats">
        <Stat k="Objects" v={fmtNum(b.objects)} />
        <Stat k="Stored" v={fmtBytes(b.capacity.used)} s={b.quota ? `of ${fmtBytes(b.quota)} quota` : "no quota"} />
        <Stat k="Provisioned" v={fmtBytes(b.capacity.total)} />
        <Stat k="Snapshots" v={b.counts.snapshots} />
        <Stat k="Backups" v={b.counts.backups} />
        <Stat k="Encryption" v={b.encrypted ? "on" : "off"} c={b.encrypted ? "var(--ok)" : undefined} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="volume" title="Backing volume" sub="the filesystem behind the bucket" count="→" onClick={() => nav.openVolumeById(b.volumeId)} />
        <NavCard icon="pool" title="Pool" sub={b.poolName} count="→" onClick={() => nav.openPool(b.clusterId, b.poolId)} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Bucket properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Name", b.name], ["Status", <TrafficLight status={b.status} />],
            ["Cluster", <Ref onClick={() => nav.openCluster(b.clusterId)} label={regName(b.clusterId)} />],
            ["Backing volume", <Ref onClick={() => nav.openVolumeById(b.volumeId)} label={b.volumeName} />],
            ["Pool", <Ref onClick={() => nav.openPool(b.clusterId, b.poolId)} label={b.poolName} />],
            ["Versioning", b.versioning ? "enabled" : "disabled"],
            ["Object lock", b.objectLock ? "enabled" : "disabled"],
            ["Quota", b.quota ? fmtBytes(b.quota) : "none"],
            ["Objects", fmtNum(b.objects)],
            ["Encryption", b.encrypted ? "enabled — keys from the cluster KMS" : "disabled"],
            ["Replication", b.replication
              ? <Ref onClick={() => nav.openRPolicy && nav.openRPolicy(b.replication.policy_id)} label={`${b.replication.mode} · ${b.replication.policy_name}`} />
              : "none — attach to a policy from Actions"],
            ["Created", fmtDate(b.createdAt)]
          ]} /></div></div>
        <div className="card"><h3>S3 metadata</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Region", b.region || "—"], ["Default storage class", b.storageClass],
            ["Owner", b.owner || "—"], ["CORS", b.cors ? "configured" : "none"],
            ["Tags", b.counts.tags ? <div className="labels tags" style={{marginTop: 2}}>
              {Object.entries(b.tags).map(([k, v]) => <span key={k} className="lab tag"><i>{k}</i>{v || "—"}</span>)}</div> : "none"],
            ["Lifecycle rules", b.lifecycle.length ? <div className="code" style={{marginTop: 2}}>{b.lifecycle.map(r =>
              `${r.id}\t${r.prefix ? "prefix " + r.prefix : "all objects"}\t${r.expire_days ? "expire after " + r.expire_days + "d"
                : r.transition_days ? "→ " + r.transition_class + " after " + r.transition_days + "d"
                : "noncurrent versions expire after " + r.noncurrent_expire_days + "d"}\t${r.status}`).join("\n")}</div> : "none"]
          ]} /></div></div>
        <div>
          <div className="card"><h3>Access · Kubernetes-native</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
            <Props rows={[
              ["Service account", `${a.namespace}/${a.service_account}`],
              ["Credentials secret", a.secret_name],
              ["Access key id", a.access_key_id],
              ["Secret access key", "•••••••••••••••• (in the secret)"],
              ["Policy", a.policy],
              ["Anonymous access", a.public ? "allowed" : "denied"]
            ]} /></div></div>
          <div className="card" style={{marginTop: 12}}><h3>How this bucket is built</h3><div className="bd">
            <p className="mdesc">One bucket is one filesystem is one <b>logical volume</b>. Object data lives on the cluster's own capacity; the object metadata lives in <b>FoundationDB</b>, the same state database the control plane uses.</p>
            <p className="mdesc" style={{marginBottom: 0}}>Because the bucket is a volume, everything a volume can do applies to it unchanged — snapshots, backups to object storage, and both synchronous and asynchronous replication.</p>
          </div></div>
        </div>
      </div>
    </div>
  );
}

// ---- cluster panel: file storage -------------------------------------------
function FileStoragePanel({cluster, nav}) {
  const f = cluster.fileStorage || {enabled: false};
  const [busy, setBusy] = useState(false);
  const failover = async () => {
    setBusy(true);
    try { await api.clusterFailoverMds(cluster.id); window.__toast("Metadata server moving to the next worker"); window.__refresh(); }
    catch (e) { window.__toast(e.message); }
    setBusy(false);
  };
  if (!f.enabled) return (
    <div className="empty">
      <Icon n="folder" s={24} />
      <b style={{color: "var(--text)"}}>{supported ? "File storage is off" : "File storage is not available here"}</b>
      <span style={{maxWidth: 520}}>Turning it on creates a pNFS filesystem over this cluster's capacity and makes every worker a pNFS client, so pods can take ReadWriteMany claims.</span>
      <button className="chip" style={{marginTop: 8}} onClick={() => window.__ui.dialog(fileStorageDialog(cluster), cluster)}>
        <Icon n="plus" s={12} />Configure file storage</button>
    </div>
  );
  return (
    <>
      {f.mds_state === "restarting" && <div className="banner" style={{color: "var(--info)", background: "color-mix(in srgb,var(--info) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"}}>
        <Icon n="clock" s={15} /><span><b>Metadata server restarting on {f.mds_host ? f.mds_host.hostname : "another worker"}.</b> Expected back within {f.failover_budget_seconds}s. Clients hold their layouts and keep reading and writing through the data paths — only new metadata operations block.</span></div>}
      <div className="stats">
        <Stat k="Metadata server" v={f.mds_state} c={f.mds_state === "active" ? "var(--ok)" : "var(--info)"}
          s={f.mds_host ? f.mds_host.hostname : "—"} />
        <Stat k="pNFS clients" v={f.client_count || 0} s="every prepared worker" />
        <Stat k="RWX claims" v={cluster.counts.rwxPvcs} s={`of ${f.max_exports} exports`} />
        <Stat k="Filesystem" v={f.filesystem} s="pNFS supports XFS only" />
        <Stat k="Failover budget" v={f.failover_budget_seconds + "s"} />
        <Stat k="Standbys" v={(f.mds_candidates || []).length} />
      </div>
      <div className="sech"><h2>Metadata service</h2><span className="ln"></span>
        <button className="chip" disabled={busy || !(f.mds_candidates || []).length} onClick={failover}>
          <Icon n="move" s={11} />{busy ? "Moving…" : "Fail over now"}</button></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Role</th><th>Worker</th><th>State</th></tr></thead>
          <tbody>
            {f.mds_host && <tr>
              <td><span className="badge k8s">active</span></td>
              <td className="mono" style={{fontWeight: 600}}>{f.mds_host.hostname}</td>
              <td><TrafficLight status={MDS_STATE[f.mds_state] || "online"} /></td>
            </tr>}
            {(f.mds_candidates || []).map(h => (
              <tr key={h.uuid}>
                <td><span className="badge">standby</span></td>
                <td className="mono">{h.hostname}</td>
                <td style={{color: "var(--dim2)"}}>ready</td>
              </tr>
            ))}
          </tbody></table>
      </div></div>
      <div className="dcols">
        <div className="card"><h3>Export configuration</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["NFS version", f.nfs_version], ["Layout type", f.layout_type],
            ["Export root", f.export_root], ["Filesystem", f.filesystem],
            ["Lease", f.lease_seconds + "s"], ["Grace period", f.grace_seconds + "s"],
            ["Failover budget", f.failover_budget_seconds + "s"],
            ["Max exports", f.max_exports],
            ["Active exports", cluster.counts.rwxPvcs]
          ]} /></div></div>
        <div className="card"><h3>How RWX is served</h3><div className="bd">
          <p className="mdesc">A pNFS filesystem sits over the cluster's block capacity. Each worker mounts it as a <b>pNFS client</b> and reads and writes directly against the storage nodes — file data never passes through the NFS server.</p>
          <p className="mdesc">The <b>kernel NFS server</b> on one control-plane worker serves metadata only. Its configuration and backend are shared, so it can restart on any standby worker; clients keep their layouts and see a pause of seconds at most.</p>
          <p className="mdesc" style={{marginBottom: 0}}>The Linux NFS server only supports pNFS on <b>XFS</b>, so every ReadWriteMany claim is XFS — the UI enforces this rather than letting a claim fail at mount time.</p>
        </div></div>
      </div>
    </>
  );
}

Object.assign(window, {BucketTile, BucketDetail, FileStoragePanel});
