// ---------------------------------------------------------------------------
// CONSISTENCY GROUPS — a named set of volumes, independent of replication.
// Group snapshots are crash-consistent across every member, can be backed up,
// and can be restored into new volumes in this or any other cluster.
// ---------------------------------------------------------------------------
function CgroupTile({g, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[g.status].c}} onDoubleClick={() => nav.detail(g)}>
      <TileHead obj={g} left={<><TrafficLight status={g.status} /><Name>{g.name}</Name></>}
        right={<><span className="badge">{g.counts.volumes} volumes</span>
          {g.locked && <span className="lab" title="membership is fixed while a policy is attached" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"}}><Icon n="lock" s={10} /></span>}</>} />
      <Uuid value={g.id} />
      <div className="labels">
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openCluster(g.clusterId);}}><Icon n="cluster" s={10} />{regName(g.clusterId)}</button>
        {g.backupPolicy
          ? <button className="lab link" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"}}
              onClick={e => {e.stopPropagation(); nav.openPolicy(g.clusterId, g.backupPolicy.uuid);}}><Icon n="cloud" s={10} />{g.backupPolicy.policy_name}</button>
          : <span className="lab" style={{opacity: .65}}><i>backup</i>none</span>}
        {g.replicationConfig
          ? <span className="lab" style={{color: "var(--accent)", borderColor: "var(--accent-line)"}}><Icon n="shield" s={10} />every {g.replicationConfig.frequency} min</span>
          : <span className="lab" style={{opacity: .65}}><i>replication</i>none</span>}
        {!!(g.drPolicyIds || []).length && <span className="lab"><i>dr policies</i>{g.drPolicyIds.length}</span>}
        {!!g.counts.apps && <span className="lab" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}><i>dr apps</i>{g.counts.apps}</span>}
      </div>
      <Capacity label="Provisioned" total={g.capacity.total} used={g.capacity.used} />
      <div className="kv">
        <div><span>Volumes</span><b>{g.counts.volumes}</b></div>
        <div><span>Group snapshots</span><b>{g.counts.snapshots}</b></div>
        <div><span>Backed up</span><b>{g.counts.backedUp}</b></div>
      </div>
      {g.replication && <div className="kv">
        <div><span>Replication</span><b style={{color: STATUS_META[g.replication.status].c}}>{g.replication.status}</b></div>
        <div><span>Last cycle</span><b>{g.replication.lastAt ? clockOf(g.replication.lastAt) : "—"}</b></div>
        <div><span>Backlog</span><b>{fmtBytes(g.replication.backlog)}</b></div>
      </div>}
      <Foot items={[
        {label: "Volumes", count: g.counts.volumes, icon: "volume", onClick: () => nav.layer(g, "volumes")},
        {label: "Snapshots", count: g.counts.snapshots, icon: "camera", onClick: () => nav.layer(g, "cgsnapshots")},
        {label: "Details", right: true, onClick: () => nav.detail(g)}
      ]} />
    </div>
  );
}

function CgSnapshotTile({s, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[s.status].c}} onDoubleClick={() => nav.detail(s)}>
      <TileHead obj={s} left={<><TrafficLight status={s.status} /><Name>{s.name}</Name></>}
        right={s.backupVersionId
          ? <span className="lab" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 35%,transparent)"}}><Icon n="cloud" s={10} />{s.backupVersionId}</span>
          : <span className="lab" style={{opacity: .6}}>not backed up</span>} />
      <Uuid value={s.id} />
      <div className="labels">
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openCgroup(s.cgId);}}><Icon n="link" s={10} />{s.cgName}</button>
        <span className="lab"><i>members</i>{s.counts.volumes}</span>
      </div>
      <div className="kv">
        <div><span>Taken</span><b>{fmtDate(s.createdAt)}</b></div>
        <div><span>Age</span><b>{fmtAgo(s.createdAt)}</b></div>
        <div><span>Size</span><b>{fmtBytes(s.capacity.total)}</b></div>
      </div>
      <div className="cgstrip">
        {s.members.slice(0, 6).map(m => <span className="zonechip" key={m.snapshotId} title={`${m.volumeName} · ${fmtBytes(m.size)}`}><Icon n="volume" s={10} />{m.volumeName.split("-").slice(0, 2).join("-")}</span>)}
        {s.members.length > 6 && <span className="zonechip">+{s.members.length - 6}</span>}
      </div>
      {s.bucket && <div className="bucket" title={s.bucket}><Icon n="cloud" s={11} />{s.bucket}</div>}
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(s)}]} />
    </div>
  );
}

// The protection the group owns, and what it means for the members.
function CgProtection({g, nav}) {
  const none = !g.backupPolicy && !g.replicationConfig;
  return (
    <>
      <div className="sech"><h2>Group protection</h2><span className="ln"></span>
        <span className="count">{none ? "not protected as a group" : `applies to all ${g.counts.volumes} members`}</span></div>
      {none
        ? <div className="card"><div className="bd"><p className="mdesc" style={{margin: 0}}>
            This group has no policy of its own, so its members are protected individually — or not at all. Attach a backup policy to snapshot and back every member up at the same instant, or a replication policy to ship them as one crash-consistent set. Both are on the group's actions menu.</p></div></div>
        : <div className="dcols">
            {g.backupPolicy && <div className="card"><h3>Backup policy<span className="cnt" style={{marginLeft: 8, fontSize: 11, color: "var(--dim2)"}}>group-consistent</span></h3><div className="bd">
              <div className="labels" style={{marginBottom: 8}}>
                <button className="lab link" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"}}
                  onClick={() => nav.openPolicy(g.clusterId, g.backupPolicy.uuid)}><Icon n="cloud" s={10} />{g.backupPolicy.policy_name}</button>
                <span className="lab"><i>members</i>{g.counts.volumes}</span>
                <span className="lab"><i>versions</i>{g.counts.backedUp}</span>
              </div>
              <p className="mdesc" style={{margin: 0}}>Every cycle snapshots all {g.counts.volumes} members at one instant and backs the set up as a single version. Any retained version is a point in time the whole group can be restored to — this is what a DR application based on this group recovers from.</p>
            </div></div>}
            {g.replicationConfig && <div className="card"><h3>Replication cadence<span className="cnt" style={{marginLeft: 8, fontSize: 11, color: "var(--dim2)"}}>owned by this group</span></h3><div className="bd">
              <div className="labels" style={{marginBottom: 8}}>
                <span className="lab" style={{color: "var(--accent)", borderColor: "var(--accent-line)"}}><i>every</i>{g.replicationConfig.frequency} min</span>
                <span className="lab"><i>generations</i>{g.replicationConfig.retention.reduce((a, r) => a + r.keep, 0)}</span>
                {g.replication && <span className="lab" style={{color: STATUS_META[g.replication.status].c, borderColor: `color-mix(in srgb,${STATUS_META[g.replication.status].c} 40%,transparent)`}}><i>status</i>{g.replication.status}</span>}
                {g.replication && <span className="lab"><i>backlog</i>{fmtBytes(g.replication.backlog)}</span>}
              </div>
              <p className="mdesc" style={{margin: 0}}>All {g.counts.volumes} members replicate as one set: a group snapshot is taken every {g.replicationConfig.frequency} minutes, so the target always holds a common point in time across the group rather than a mixture. {(g.drPolicyIds || []).length ? `${g.drPolicyIds.length} DR polic${g.drPolicyIds.length === 1 ? "y ships" : "ies ship"} this group to ${g.drPolicyIds.length === 1 ? "its" : "their"} paired cluster${g.drPolicyIds.length === 1 ? "" : "s"} on this cadence.` : "No DR policy names this group yet, so nothing is being shipped — create one under Disaster recovery."} The group's status is the worst of its members'{g.replication && g.replication.lastAt ? `; the oldest member last synced at ${clockOf(g.replication.lastAt)}` : ""}.</p>
            </div></div>}
          </div>}
    </>
  );
}

// DR applications whose PVC set is this group.
function CgAppsCard({g, nav}) {
  const {data} = useResource("cgapps|" + g.id, () => api.cgroupApps(g.id), 8000);
  const apps = data || [];
  if (!apps.length) return null;
  return (
    <>
      <div className="sech"><h2>Protected applications</h2><span className="ln"></span>
        <span className="count">based on this group</span></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Application</th><th>Namespace</th><th>Phase</th><th>Plan</th><th>Orchestrated</th><th>Active site</th><th></th></tr></thead>
          <tbody>{apps.map(a => (
            <tr key={a.id}>
              <td style={{fontWeight: 600}}>{a.name}</td>
              <td className="mono">{a.namespace}</td>
              <td><TrafficLight status={a.phase} sm label /></td>
              <td>{a.planName || "—"}</td>
              <td className="mono">{a.orchestratedMethod || "—"}</td>
              <td className="mono">{a.activeSite || "—"}</td>
              <td style={{textAlign: "right"}}><button className="chip" onClick={() => nav.openProtectedApp(a.id)}>Open</button></td>
            </tr>
          ))}</tbody></table>
      </div>
      <div className="bd"><div className="fnote" style={{margin: 0}}><Icon n="alert" s={12} />
        These applications take their crash-consistent boundary from this group: every member volume fails over together. The group cannot be deleted or have members removed while an application is based on it.</div></div>
      </div>
    </>
  );
}

function CgroupDetail({o: g, nav}) {
  return (
    <div>
      <DetailHead obj={g} title={g.name}
        badge={<><span className="badge">consistency group</span><span className="badge">{g.counts.volumes} volumes</span>
          {g.locked && <span className="badge" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"}}>membership fixed</span>}</>}
        sub={<span style={{fontSize: 11.5, color: "var(--dim)"}}>in <Ref onClick={() => nav.openCluster(g.clusterId)} label={regName(g.clusterId)} /></span>} />
      {g.status !== "online" && <div className="banner"><Icon n="alert" s={15} />
        <span><b>Group not fully online.</b> A group snapshot is only crash-consistent if every member volume is online at the moment it is taken.</span></div>}
      <div className="stats">
        <Stat k="Volumes" v={g.counts.volumes} />
        <Stat k="Provisioned" v={fmtBytes(g.capacity.total)} />
        <Stat k="Utilized" v={fmtBytes(g.capacity.used)} s={pct(g.capacity.used, g.capacity.total).toFixed(0) + "%"} />
        <Stat k="Group snapshots" v={g.counts.snapshots} />
        <Stat k="Backup policy" v={g.backupPolicy ? "yes" : "no"} c={g.backupPolicy ? "var(--ok)" : undefined}
          s={g.backupPolicy ? g.backupPolicy.policy_name : "members not backed up as a group"} />
        <Stat k="Replication" v={g.replicationConfig ? "yes" : "no"}
          c={g.replication ? STATUS_META[g.replication.status].c : undefined}
          s={g.replicationConfig ? `every ${g.replicationConfig.frequency} min` : "no cadence set"} />
      </div>
      {g.locked && <div className="banner" style={{color: "var(--ro)", background: "color-mix(in srgb,var(--ro) 7%,var(--panel))", borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"}}>
        <Icon n="lock" s={15} /><span><b>Membership is fixed.</b> This group carries {[g.backupPolicy && "a backup policy", g.replicationConfig && "a replication cadence"].filter(Boolean).join(" and ")}, and every retained version and replica stream is defined against exactly these {g.counts.volumes} volumes. Volumes cannot be added or removed while {g.backupPolicy && g.replicationConfig ? "they are" : "it is"} attached — detach {g.backupPolicy && g.replicationConfig ? "them" : "it"} to change the members, or create a second group with the volumes you want. A volume can belong to more than one group.</span></div>}
      <CgProtection g={g} nav={nav} />
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="volume" title="Member volumes" sub={g.locked ? "fixed — a policy is attached" : "add or remove members"} count={g.counts.volumes} onClick={() => nav.layer(g, "volumes")} />
        <NavCard icon="camera" title="Group snapshots" sub="crash-consistent across members" count={g.counts.snapshots} onClick={() => nav.layer(g, "cgsnapshots")} />
        {g.backupPolicy && <NavCard icon="cloud" title="Backup policy" sub={g.backupPolicy.policy_name} count="→" onClick={() => nav.openPolicy(g.clusterId, g.backupPolicy.uuid)} />}
        {!!(g.drPolicyIds || []).length && <NavCard icon="shield" title="DR policies" sub="ship this group to a paired cluster" count={g.drPolicyIds.length} onClick={() => nav.openRPolicy(g.drPolicyIds[0])} />}
      </div>
      <CgAppsCard g={g} nav={nav} />
      <div className="dcols">
        <div className="card"><h3>Group properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Name", g.name], ["Status", <TrafficLight status={g.status} />],
            ["Cluster", <Ref onClick={() => nav.openCluster(g.clusterId)} label={regName(g.clusterId)} />],
            ["Volumes", g.counts.volumes + (g.locked ? " · fixed while a policy is attached" : "")],
            ["Provisioned", fmtBytes(g.capacity.total)], ["Utilized", fmtBytes(g.capacity.used)],
            ["Backup policy", g.backupPolicy
              ? <Ref onClick={() => nav.openPolicy(g.clusterId, g.backupPolicy.uuid)} label={g.backupPolicy.policy_name} /> : null],
            ["Replication frequency", g.replicationConfig ? `every ${g.replicationConfig.frequency} min` : null],
            ["Retained generations", g.replicationConfig ? g.replicationConfig.retention.map(r => `${r.interval}×${r.keep}`).join(" ") || "latest only" : null],
            g.replication ? ["Replication status", <TrafficLight status={g.replication.status} label />] : null,
            g.replication ? ["Last group cycle", g.replication.lastAt ? `${clockOf(g.replication.lastAt)} · ${fmtAgo(g.replication.lastAt)}` : "—"] : null,
            g.replication ? ["Backlog", fmtBytes(g.replication.backlog)] : null,
            ["Created", fmtDate(g.createdAt)]
          ]} /></div></div>
        <div className="card"><h3>What a consistency group is</h3><div className="bd">
          <p className="mdesc">A named set of volumes. Taking a <b>group snapshot</b> snapshots every member at the same instant, so they restore to one common point in time.</p>
          <p className="mdesc">A group also <b>owns its protection</b>. Attach a backup policy and every member is snapshotted and backed up together on that schedule, so any retained version is a point in time the whole group returns to. Give it a replication cadence and every member ships as one crash-consistent set — a DR policy then simply names this group, and takes its frequency and retention from here.</p>
          <p className="mdesc" style={{marginBottom: 0}}>Because the group owns the policy, a volume joining it inherits both, and a volume leaving it gives them up. A DR application based on this group inherits the same boundary.</p>
        </div></div>
      </div>
    </div>
  );
}

function CgSnapshotDetail({o: s, nav}) {
  return (
    <div>
      <DetailHead obj={s} title={s.name}
        badge={<><span className="badge">group snapshot</span>
          {s.backupVersionId
            ? <span className="badge" style={{color: "var(--ok)", borderColor: "color-mix(in srgb,var(--ok) 40%,transparent)"}}>backed up · {s.backupVersionId}</span>
            : <span className="badge">not backed up</span>}</>}
        sub={<span style={{fontSize: 11.5, color: "var(--dim)"}}>
          of <Ref onClick={() => nav.openCgroup(s.cgId)} label={s.cgName} /> · {fmtDate(s.createdAt)} · {fmtAgo(s.createdAt)}</span>} />
      <div className="stats">
        <Stat k="Members" v={s.counts.volumes} s="one snapshot per volume" />
        <Stat k="Total size" v={fmtBytes(s.capacity.total)} />
        <Stat k="Taken" v={fmtAgo(s.createdAt)} s={fmtDate(s.createdAt)} />
        <Stat k="Backup" v={s.backupVersionId || "none"} c={s.backupVersionId ? "var(--ok)" : undefined} />
      </div>
      <div className="sech"><h2>Member snapshots</h2><span className="ln"></span>
        <span className="count">all taken at the same instant</span></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Volume</th><th>Snapshot id</th><th style={{textAlign: "right"}}>Delta size</th><th></th></tr></thead>
          <tbody>{s.members.map(m => (
            <tr key={m.snapshotId}>
              <td className="mono" style={{fontWeight: 600}}>{m.volumeName}</td>
              <td className="mono" style={{color: "var(--dim)"}}>{shortId(m.snapshotId)}</td>
              <td className="mono" style={{textAlign: "right"}}>{fmtBytes(m.size)}</td>
              <td style={{textAlign: "right"}}>
                <button className="chip" onClick={() => nav.openSnapshot(s.clusterId, m.snapshotId)}>Open snapshot</button>
              </td>
            </tr>
          ))}</tbody></table>
      </div></div>
      <div className="dcols">
        <div className="card"><h3>Snapshot properties</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Name", s.name], ["Status", <TrafficLight status={s.status} />],
            ["Consistency group", <Ref onClick={() => nav.openCgroup(s.cgId)} label={s.cgName} />],
            ["Cluster", <Ref onClick={() => nav.openCluster(s.clusterId)} label={regName(s.clusterId)} />],
            ["Created", fmtDate(s.createdAt)], ["Members", s.counts.volumes],
            ["Total size", fmtBytes(s.capacity.total)],
            ["Backup version", s.backupVersionId || null],
            ["Bucket", s.bucket || null]
          ]} /></div></div>
        <div className="card"><h3>Restoring</h3><div className="bd">
          <p className="mdesc">Restoring this group snapshot creates <b>one new volume per member</b>, all at the same point in time. The target can be this cluster or any other cluster the control plane manages.</p>
          <p className="mdesc" style={{marginBottom: 0}}>Individual member snapshots can also be restored on their own from the snapshot page — a single volume, same choice of target cluster.</p>
        </div></div>
      </div>
    </div>
  );
}

Object.assign(window, {CgroupTile, CgSnapshotTile, CgroupDetail, CgSnapshotDetail, CgProtection, CgAppsCard});
