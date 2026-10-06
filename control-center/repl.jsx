// ---------------------------------------------------------------------------
// REPLICATION — UI for the real v1alpha1 kinds
//
// Four kinds, and the shape matters:
//   ReplicationPair    reusable {sourceCluster, targetCluster}
//   ReplicationPolicy  {pairRef, mode, interval, snapshotRetention}
//   ReplicationSlot    one per PVC, created by the operator, owned by the PVC
//   ReplicationOps     one-shot {action, scope, ref}
//
// Two things the console must not imply: that a policy has a volume list (it
// does not — a PVC annotation is the whole membership model), and that
// replication can be synchronous (mode is exactly failover | migration).
// ---------------------------------------------------------------------------
const REPL_ANNOTATION = "storage.simplyblock.io/replication-policy";
// The house convention is className="mono"; this is just that span, so the CRD
// field values below read as the literals they are.
const Mono = ({children}) => <span className="mono">{children}</span>;

// Colour and label for a slot state come from STATUS_META, which is the one
// vocabulary the traffic light and the tile stripe both read. This carries the
// explanation and nothing else, so the two can never disagree.
const SLOT_HINT = {
  replicating: "shipping a delta once per interval",
  cutover_pending: "final delta transferred, waiting for the commit",
  cutover_done: "the target is authoritative; the migration is complete",
  failed_over: "the target was promoted; the source is no longer authoritative",
  attaching: "legacy state — an attach is synchronous now, so a new slot reaches replicating directly",
  detaching: "removing the replication snapshots on both sides",
  error: "the backend refused the last call"
};
const smeta = s => Object.assign({label: s, c: "var(--idle)"},
  STATUS_META[s] || {}, {hint: SLOT_HINT[s] || ""});

const MODE_META = {
  failover: {label: "failover", c: "var(--ro)",
    desc: "The target is a DR standby and its volumes are read-only. Promoting it is a deliberate ReplicationOps, never automatic."},
  migration: {label: "migration", c: "var(--info)",
    desc: "A planned online cutover. Both clusters stay up and the commit runs per volume: replicating → cutover_pending → cutover_done."}
};
const ModeBadge = ({mode}) => {
  const m = MODE_META[mode] || MODE_META.failover;
  return <span className="badge" style={{color: m.c, borderColor: `color-mix(in srgb,${m.c} 45%,transparent)`}} title={m.desc}>{m.label}</span>;
};

const OPS_ACTION_META = {
  failover: {label: "fail over", danger: true,
    desc: "Unplanned. The target clone is promoted and the source may be down. Work written after the last replication snapshot is lost."},
  failback: {label: "fail back", danger: false,
    desc: "Restores the source as primary after a failover. A short write freeze holds while the final delta transfers, so plan a window."},
  migration: {label: "cut over", danger: false,
    desc: "Planned. Commits the cutover per volume with both clusters up, optionally deleting the source volume afterwards."}
};
const SCOPE_HINT = {
  target: "every volume of every policy on the pair",
  policy: "every volume attached to this policy",
  volume: "one volume — exactly one slot"
};
// One reference clock, the fixtures': measuring their timestamps against the
// wall clock is what made every slot read days stale and every policy degraded.
const REPL_NOW = () => window.SB_NOW || Date.now();
const minsSince = at => at ? (REPL_NOW() - Date.parse(at)) / 60000 : Infinity;
const relAge = at => {
  if (!at) return "never";
  const m = Math.round(minsSince(at));
  return m < 1 ? "just now" : m < 60 ? m + "m ago" : m < 1440 ? Math.round(m / 60) + "h ago" : Math.round(m / 1440) + "d ago";
};

// ---- tiles -----------------------------------------------------------------
function PairTile({p, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[p.status].c}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><TrafficLight status={p.status} /><Name>{p.name}</Name></>}
        right={<span className="badge" title="ReplicationPair">relpair</span>} />
      <div className="tsub" style={{marginTop: 2}}>{p.sourceCluster} → {p.targetCluster}</div>
      <Uuid value={p.id} />
      {!p.ready && <div className="nolim" style={{color: "var(--bad)"}}>{p.message || "Backend replication target not available"}</div>}
      {p.activeOpsRef && <div className="prepbox running">
        <span className="dots"><i></i><i></i><i></i></span>
        {p.activeOpsRef} holds the pair lock — a second operation waits for it
      </div>}
      <div className="labels">
        <span className="lab"><i>target</i>{p.backendTargetId ? <Mono>{p.backendTargetId.slice(0, 8)}</Mono> : "—"}</span>
        <span className="lab" title="targetCluster is immutable after creation"><i>immutable</i>targetCluster</span>
      </div>
      <div className="rw">
        <div><span>Policies</span><b>{p.counts.policies}</b></div>
        <div><span>Slots</span><b>{p.counts.slots}</b></div>
      </div>
      {(p.counts.failedOver || p.counts.errored) ? <div className="labels">
        {!!p.counts.failedOver && <span className="lab" style={{color: "var(--ro)"}}><i>failed over</i>{p.counts.failedOver}</span>}
        {!!p.counts.errored && <span className="lab" style={{color: "var(--bad)"}}><i>error</i>{p.counts.errored}</span>}
      </div> : null}
      <Foot items={[
        {label: "Policies", count: p.counts.policies, icon: "clock", onClick: () => nav.layer(p, "rpolicies")},
        {label: "Slots", count: p.counts.slots, icon: "volume", onClick: () => nav.layer(p, "slots")}
      ]} onDetail={() => nav.detail(p)} />
    </div>
  );
}

function RPolicyTile({p, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[p.status].c}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><TrafficLight status={p.status} /><Name>{p.name}</Name></>}
        right={<ModeBadge mode={p.mode} />} />
      <div className="tsub" style={{marginTop: 2}}>{p.sourceCluster || "?"} → {p.targetCluster || "?"}</div>
      <Uuid value={p.id} />
      {!p.ready && <div className="nolim" style={{color: "var(--bad)"}}>{p.message || "Backend policy not created"}</div>}
      {p.activeOpsRef && <div className="prepbox running">
        <span className="dots"><i></i><i></i><i></i></span>{p.activeOpsRef} in flight
      </div>}
      <div className="labels">
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openPairByName(p.pairRef);}}>
          <Icon n="link" s={10} />{p.pairRef}</button>
        <span className="lab"><i>interval</i>{p.interval}</span>
        <span className="lab" title="minimum snapshots kept on the target (minimum 2)"><i>retain</i>{p.snapshotRetention}</span>
      </div>
      <div className="rw">
        <div><span>Slots</span><b>{p.counts.slots}</b></div>
        <div><span>Last snapshot</span><b>{relAge(p.lastAt)}</b></div>
      </div>
      {(p.counts.errored || p.counts.late || p.counts.failedOver || p.counts.cutoverPending) ? (
        <div className="labels">
          {!!p.counts.errored && <span className="lab" style={{color: "var(--bad)"}}><i>error</i>{p.counts.errored}</span>}
          {!!p.counts.late && <span className="lab" style={{color: "var(--warn)"}}><i>late</i>{p.counts.late}</span>}
          {!!p.counts.cutoverPending && <span className="lab" style={{color: "var(--warn)"}}><i>cutover</i>{p.counts.cutoverPending}</span>}
          {!!p.counts.failedOver && <span className="lab" style={{color: "var(--ro)"}}><i>failed over</i>{p.counts.failedOver}</span>}
        </div>
      ) : null}
      <Foot items={[
        {label: "Slots", count: p.counts.slots, icon: "volume", onClick: () => nav.layer(p, "slots")},
        {label: "Operations", icon: "clock", onClick: () => nav.layer(p, "replops")}
      ]} onDetail={() => nav.detail(p)} />
    </div>
  );
}

function SlotTile({s, nav}) {
  const m = smeta(s.state);
  return (
    <div className="tile" style={{"--sc": m.c}} onDoubleClick={() => nav.detail(s)}>
      <TileHead obj={s} left={<TrafficLight status={s.state} />}
        right={<span className="badge" title="ReplicationSlot">relslot</span>} />
      <div className="nm" style={{marginTop: 4}}><b>{s.pvcRef}</b></div>
      <div className="tsub">{s.name}</div>
      <Uuid value={s.id} />
      <div className="nolim" style={{color: s.state === "error" ? "var(--bad)" : "var(--dim)"}}>{s.message || m.hint}</div>
      <div className="labels">
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openRPolicyByName(s.policyRef);}}>
          <Icon n="clock" s={10} />{s.policyRef}</button>
        <span className="lab" title="which side of the relationship this cluster holds"><i>direction</i>{s.direction}</span>
        {s.ownedBy && <span className="lab" title="the slot is owned by its PVC, so deleting the PVC cascades"><i>owned by</i>{s.ownedBy}</span>}
      </div>
      <div className="rw">
        <div><span>Last replicated</span><b>{relAge(s.lastAt)}</b></div>
        <div><span>Target volume</span><b>{s.targetLvolId ? <Mono>{s.targetLvolId.slice(0, 8)}</Mono> : "—"}</b></div>
      </div>
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(s)}]} />
    </div>
  );
}

function ReplOpsTile({o, nav}) {
  const m = OPS_ACTION_META[o.action] || {};
  const failed = o.results.filter(r => r.status === "failed").length;
  return (
    <div className="tile" style={{"--sc": STATUS_META[o.status].c}} onDoubleClick={() => nav.detail(o)}>
      <TileHead obj={o} left={<><TrafficLight status={o.status} /><Name>{m.label || o.action}</Name></>}
        right={<span className="badge" title="ReplicationOps">replops</span>} />
      <div className="tsub" style={{marginTop: 2}}>{o.scope} · {o.ref}</div>
      <Uuid value={o.id} />
      {o.phase === "Running" && <div className="prepbox running">
        <span className="dots"><i></i><i></i><i></i></span>{o.subphase || "Running"}
      </div>}
      <div className="nolim" style={{color: failed ? "var(--bad)" : "var(--dim)"}}>{o.message}</div>
      <div className="labels">
        <span className="lab"><i>phase</i>{o.phase}</span>
        {o.deleteSource && <span className="lab" style={{color: "var(--bad)"}}><i>deleteSource</i>yes</span>}
        {o.terminal && <span className="lab" title="a terminal operation is never re-run — a repeat needs a new ReplicationOps"><i>one-shot</i>spent</span>}
      </div>
      {!!o.results.length && <div className="rw">
        <div><span>Succeeded</span><b style={{color: "var(--ok)"}}>{o.results.filter(r => r.status === "succeeded").length}</b></div>
        <div><span>Skipped</span><b>{o.results.filter(r => r.status === "skipped").length}</b></div>
        <div><span>Failed</span><b style={failed ? {color: "var(--bad)"} : null}>{failed}</b></div>
      </div>}
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(o)}]} />
    </div>
  );
}

// ---- details ---------------------------------------------------------------
function PairDetail({o: p, nav}) {
  const {data: ops} = useResource("pair.ops|" + p.name, () => api.refReplOps(p.name), 5000);
  return (
    <div>
      <DetailHead obj={p} title={p.name} sub={`${p.sourceCluster} → ${p.targetCluster}`}
        badge={<span className="badge">ReplicationPair</span>} />
      {!p.ready && <div className="banner"><Icon n="alert" s={15} />
        <span><b>The backend replication target is not available.</b> {p.message} No policy on this pair can replicate until it is.</span></div>}
      {p.activeOpsRef && <div className="banner" style={{color: "var(--info)", background: "color-mix(in srgb,var(--info) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"}}>
        <Icon n="clock" s={15} /><span><b>{p.activeOpsRef} holds the pair lock.</b> Only one target-scoped operation runs per pair; a second one waits for the lock rather than failing.</span></div>}
      <div className="stats">
        <Stat k="Ready" v={p.ready ? "yes" : "no"} c={p.ready ? "var(--ok)" : "var(--bad)"} />
        <Stat k="Policies" v={p.counts.policies} s="each with its own interval" />
        <Stat k="Slots" v={p.counts.slots} s="one per replicated PVC" />
        <Stat k="Failed over" v={p.counts.failedOver} c={p.counts.failedOver ? "var(--ro)" : null} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="clock" title="Policies" sub="schedules on this pair" count={p.counts.policies} onClick={() => nav.layer(p, "rpolicies")} />
        <NavCard icon="volume" title="Slots" sub="volumes replicating across it" count={p.counts.slots} onClick={() => nav.layer(p, "slots")} />
        <NavCard icon="cluster" title="Source cluster" sub={p.sourceCluster} count="→" onClick={() => nav.openClusterByName(p.sourceCluster)} />
        <NavCard icon="cluster" title="Target cluster" sub={p.targetCluster} count="→" onClick={() => nav.openClusterByName(p.targetCluster)} />
      </div>
      <div className="sech"><h2>ReplicationPair</h2><span className="ln"></span></div>
      <Props rows={[
        ["metadata.name", <Mono>{p.name}</Mono>],
        ["spec.sourceCluster", <Mono>{p.sourceCluster}</Mono>],
        ["spec.targetCluster", <span><Mono>{p.targetCluster}</Mono> <span style={{color: "var(--dim2)", fontSize: 11}}>immutable after creation</span></span>],
        ["status.ready", String(p.ready)],
        ["status.backendTargetID", p.backendTargetId ? <Mono>{p.backendTargetId}</Mono> : "—"],
        ["status.activeOpsRef", p.activeOpsRef || "none"],
        ["status.message", p.message || "—"],
        ["Created", fmtDate(p.createdAt)]
      ]} />
      <p className="mdesc">A pair is reusable configuration: several policies may replicate between the same two clusters on different schedules. Both clusters must be StorageCluster resources in this namespace with <Mono>status.uuid</Mono> populated — cross-namespace references are not supported. Because <Mono>spec.targetCluster</Mono> is immutable, changing a target means a new pair. Deleting this one is refused while any policy references it, and it deletes the backend replication target.</p>
      <ConditionsCard conditions={p.conditions} />
      <OpsHistory ops={ops} nav={nav} title="Operations on this pair" />
    </div>
  );
}

function RPolicyDetail({o: p, nav}) {
  const {data: slots} = useResource("pol.slots|" + p.id, () => api.policySlots(p.id), 6000);
  const {data: ops} = useResource("pol.ops|" + p.name, () => api.refReplOps(p.name), 5000);
  const mode = MODE_META[p.mode] || MODE_META.failover;
  return (
    <div>
      <DetailHead obj={p} title={p.name} sub={`${p.sourceCluster || "?"} → ${p.targetCluster || "?"}`}
        badge={<><span className="badge">ReplicationPolicy</span><ModeBadge mode={p.mode} /></>} />
      {!!p.counts.errored && <div className="banner"><Icon n="alert" s={15} />
        <span><b>{p.counts.errored} slot{p.counts.errored === 1 ? "" : "s"} in error.</b> The backend refused the last replication call for those volumes. The rest of the policy keeps replicating — slot state is per volume.</span></div>}
      {!!p.counts.late && <div className="banner" style={{color: "var(--warn)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"}}>
        <Icon n="alert" s={15} /><span><b>{p.counts.late} slot{p.counts.late === 1 ? " is" : "s are"} behind the {p.interval} interval.</b> A failover now would lose more than one interval of work for them.</span></div>}
      {p.activeOpsRef && <div className="banner" style={{color: "var(--info)", background: "color-mix(in srgb,var(--info) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"}}>
        <Icon n="clock" s={15} /><span><b>{p.activeOpsRef} is in flight.</b> One operation runs per policy; anything else queues on the lock.</span></div>}
      <div className="stats">
        <Stat k="Mode" v={mode.label} c={mode.c} />
        <Stat k="Interval" v={p.interval} s="target RPO" />
        <Stat k="Snapshot retention" v={p.snapshotRetention} s="minimum kept on the target" />
        <Stat k="Slots" v={p.counts.slots} s={`${p.counts.replicating} replicating`} />
        <Stat k="Last snapshot" v={relAge(p.lastAt)} s={p.lastAt ? fmtDate(p.lastAt) : ""} />
      </div>
      <p className="mdesc">{mode.desc}</p>

      <div className="sech"><h2>Attached volumes</h2><span className="ln"></span>
        <span className="count">{(slots || []).length} slots</span>
        <button className="btn" onClick={() => window.__ui.dialog(attachPvcDialog(p), p)}><Icon n="plus" s={12} />Attach a PVC</button></div>
      <div className="banner" style={{color: "var(--accent)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}>
        <Icon n="alert" s={15} /><span><b>A policy has no volume list.</b> Membership is the annotation <Mono>{REPL_ANNOTATION}</Mono> on the PVC, or on its StorageClass — the PVC wins. The operator creates one ReplicationSlot per bound PVC and owns it, so attaching and detaching here writes that annotation and nothing else.</span></div>
      <SlotTable slots={slots} nav={nav} interval={p.interval} />

      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="volume" title="Slots" sub="one per replicated PVC" count={p.counts.slots} onClick={() => nav.layer(p, "slots")} />
        <NavCard icon="link" title="Pair" sub={p.pairRef} count="→" onClick={() => nav.openPairByName(p.pairRef)} />
        <NavCard icon="clock" title="Operations" sub="failover, failback, cutover" count={(ops || []).length} onClick={() => nav.layer(p, "replops")} />
      </div>

      <div className="sech"><h2>ReplicationPolicy</h2><span className="ln"></span></div>
      <Props rows={[
        ["metadata.name", <Mono>{p.name}</Mono>],
        ["spec.pairRef", <Ref onClick={() => nav.openPairByName(p.pairRef)} label={p.pairRef} />],
        ["spec.mode", <span><Mono>{p.mode}</Mono> <span style={{color: "var(--dim2)", fontSize: 11}}>failover | migration</span></span>],
        ["spec.interval", <span><Mono>{p.interval}</Mono> <span style={{color: "var(--dim2)", fontSize: 11}}>rounded to whole minutes, minimum 1m</span></span>],
        ["spec.snapshotRetention", <span><Mono>{p.snapshotRetention}</Mono> <span style={{color: "var(--dim2)", fontSize: 11}}>minimum 2</span></span>],
        ["status.ready", String(p.ready)],
        ["status.backendPolicyID", p.backendPolicyId ? <Mono>{p.backendPolicyId}</Mono> : "—"],
        ["status.slotCount", p.counts.slots],
        ["status.activeOpsRef", p.activeOpsRef || "none"],
        ["Created", fmtDate(p.createdAt)]
      ]} />
      <p className="mdesc">There is no tiered retention schedule here and no consistency-group field: one interval and one snapshot count is the whole model. Deletion is refused while any slot references the policy — detach the PVCs first. Repointing a PVC at a different policy is a detach followed by a fresh attach, so the new target takes a full copy.</p>
      <ConditionsCard conditions={p.conditions} />
      <OpsHistory ops={ops} nav={nav} title="Operations on this policy" />
    </div>
  );
}

function SlotDetail({o: s, nav}) {
  const m = smeta(s.state);
  const [cid, pid, vid] = s.volumeParts || [];
  return (
    <div>
      <DetailHead obj={s} title={s.pvcRef} sub={s.name}
        badge={<span className="badge">ReplicationSlot</span>} />
      {s.state === "error" && <div className="banner"><Icon n="alert" s={15} />
        <span><b>{s.message}</b> The operator backs off to a 30-second poll while a slot is in error, instead of the usual 60.</span></div>}
      {s.state === "cutover_pending" && <div className="banner" style={{color: "var(--warn)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"}}>
        <Icon n="alert" s={15} /><span><b>Waiting for the cutover commit.</b> The final delta has transferred. A <Mono>migration</Mono>-scoped ReplicationOps commits it and the slot moves to <Mono>cutover_done</Mono>.</span></div>}
      {s.state === "attaching" && <div className="banner" style={{color: "var(--dim)", background: "var(--panel2)"}}>
        <Icon n="alert" s={15} /><span>{m.hint}</span></div>}
      <div className="stats">
        <Stat k="State" v={m.label} c={m.c} s={m.hint} />
        <Stat k="Direction" v={s.direction} s={s.direction === "source" ? "this cluster holds the source" : "this cluster holds the replica"} />
        <Stat k="Last replicated" v={relAge(s.lastAt)} s={s.lastAt ? fmtDate(s.lastAt) : "no snapshot yet — taking the full copy"} />
        <Stat k="Target volume" v={s.targetLvolId ? s.targetLvolId.slice(0, 8) : "—"} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="clock" title="Policy" sub={s.policyRef} count="→" onClick={() => nav.openRPolicyByName(s.policyRef)} />
        <NavCard icon="volume" title="PVC" sub={s.pvcRef} count="→" onClick={() => nav.openPvcByName(s.pvcRef)} />
      </div>
      <div className="sech"><h2>ReplicationSlot</h2><span className="ln"></span></div>
      <Props rows={[
        ["metadata.name", <span><Mono>{s.name}</Mono> <span style={{color: "var(--dim2)", fontSize: 11}}>&lt;policy&gt;-&lt;pvc&gt;</span></span>],
        ["ownerReferences", s.ownedBy ? <Mono>{s.ownedBy}</Mono> : "—"],
        ["spec.policyRef", <Ref onClick={() => nav.openRPolicyByName(s.policyRef)} label={s.policyRef} />],
        ["spec.pvcRef", <Mono>{s.pvcRef}</Mono>],
        ["spec.volumeID", <Mono>{s.volumeId}</Mono>],
        ["— cluster UUID", cid ? <Mono>{cid}</Mono> : "—"],
        ["— pool UUID", pid ? <Mono>{pid}</Mono> : "—"],
        ["— volume UUID", vid ? <Mono>{vid}</Mono> : "—"],
        ["status.sourceLvolID", s.sourceLvolId ? <Mono>{s.sourceLvolId}</Mono> : "—"],
        ["status.targetLvolID", s.targetLvolId ? <Mono>{s.targetLvolId}</Mono> : "—"],
        ["status.targetNQN", s.targetNqn ? <Mono>{s.targetNqn}</Mono> : "— populated after failover"],
        ["status.message", s.message || "—"],
        ["Created", fmtDate(s.createdAt)]
      ]} />
      <p className="mdesc">All three spec fields are immutable. The slot is owned by its PVC, so deleting the PVC cascades to the slot and the finalizer detaches it in the backend. Clearing the annotation deletes the replication snapshots on both sides before the slot is removed.</p>
      <ConditionsCard conditions={s.conditions} />
    </div>
  );
}

function ReplOpsDetail({o, nav}) {
  const m = OPS_ACTION_META[o.action] || {};
  const failed = o.results.filter(r => r.status === "failed");
  return (
    <div>
      <DetailHead obj={o} title={`${m.label || o.action} · ${o.ref}`} sub={o.name}
        badge={<span className="badge">ReplicationOps</span>} />
      {o.phase === "Running" && <div className="banner" style={{color: "var(--info)", background: "color-mix(in srgb,var(--info) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"}}>
        <Icon n="clock" s={15} /><span><b>{o.subphase}</b> — {o.message}</span></div>}
      {o.phase === "Failed" && <div className="banner"><Icon n="alert" s={15} />
        <span><b>{failed.length} volume{failed.length === 1 ? "" : "s"} failed.</b> Per-volume outcomes are independent: the rest continued, and the operation as a whole is Failed. A retry needs a <b>new</b> ReplicationOps — this one is spent.</span></div>}
      <div className="stats">
        <Stat k="Action" v={m.label || o.action} />
        <Stat k="Scope" v={o.scope} s={SCOPE_HINT[o.scope]} />
        <Stat k="Phase" v={o.phase} c={STATUS_META[o.status].c} />
        <Stat k="Started" v={o.startedAt ? fmtDate(o.startedAt) : "—"} />
        <Stat k="Completed" v={o.completedAt ? fmtDate(o.completedAt) : "—"} />
      </div>
      <p className="mdesc">{m.desc}</p>
      {!!o.results.length && <>
        <div className="sech"><h2>Per-volume results</h2><span className="ln"></span>
          <span className="count">{o.results.length}</span></div>
        <div className="card"><div className="bd" style={{padding: 0}}>
          <table className="dt"><thead><tr>
            <th>Slot</th><th>Status</th><th>Detail</th><th>Target volume</th>
          </tr></thead>
          <tbody>{o.results.map(r => (
            <tr key={r.slotRef} className={r.status === "failed" ? "bad" : ""}>
              <td><button className="tlink" onClick={() => nav.openSlotByName(r.slotRef)}>{r.slotRef}</button></td>
              <td>{r.status === "succeeded" ? <span className="lab" style={{color: "var(--ok)"}}><Icon n="check" s={10} />succeeded</span>
                : r.status === "skipped" ? <span className="lab">skipped</span>
                : <span className="lab" style={{color: "var(--bad)"}}><Icon n="alert" s={10} />failed</span>}</td>
              <td style={{fontSize: 11.5, color: "var(--dim)"}}>{r.detail || "—"}</td>
              <td className="mono">{r.targetLvolId ? r.targetLvolId.slice(0, 8) : "—"}</td>
            </tr>
          ))}</tbody></table>
        </div></div>
      </>}
      <div className="sech"><h2>ReplicationOps</h2><span className="ln"></span></div>
      <Props rows={[
        ["metadata.name", <Mono>{o.name}</Mono>],
        ["spec.action", <Mono>{o.action}</Mono>],
        ["spec.scope", <span><Mono>{o.scope}</Mono> — {SCOPE_HINT[o.scope]}</span>],
        ["spec.ref", <Mono>{o.ref}</Mono>],
        o.sourceClusterId ? ["spec.sourceClusterID", <Mono>{o.sourceClusterId}</Mono>] : null,
        o.action === "migration" ? ["spec.deleteSource", String(o.deleteSource)] : null,
        ["status.phase", o.phase],
        ["status.subphase", o.subphase || "—"],
        ["status.message", o.message || "—"]
      ].filter(Boolean)} />
      <p className="mdesc">Every spec field is immutable and the operation is one-shot: once it reaches Succeeded or Failed it is never re-run, so a repeat or a correction is a new resource. The lock it held — the policy's <Mono>activeOpsRef</Mono>, or the pair's for a target-scoped operation — is released when it completes.</p>
    </div>
  );
}

// ---- shared bits -----------------------------------------------------------
function SlotTable({slots, nav, interval}) {
  const rows = slots || [];
  const budget = ivMinutes(interval) * 2;
  const detach = s => window.__ui.dialog(detachPvcDialog(s), s);
  if (!rows.length) return <div className="card"><div className="lmsg">No PVC carries this policy's annotation yet, so nothing is replicating.</div></div>;
  return (
    <div className="card"><div className="bd" style={{padding: 0}}>
      <table className="dt"><thead><tr>
        <th>PVC</th><th>Slot</th><th>State</th><th>Direction</th>
        <th style={{textAlign: "right"}}>Last replicated</th><th style={{width: 150}}></th>
      </tr></thead>
      <tbody>{rows.map(s => {
        const m = smeta(s.state);
        const late = s.state === "replicating" && s.lastAt && minsSince(s.lastAt) > budget;
        return (
          <tr key={s.id} className={s.state === "error" ? "bad" : ""}>
            <td><button className="tlink" onClick={() => nav.openPvcByName(s.pvcRef)}>{s.pvcRef}</button></td>
            <td><button className="tlink" onClick={() => nav.detail(s)}>{s.name}</button></td>
            <td><TrafficLight status={s.state} sm label /></td>
            <td className="mono">{s.direction}</td>
            <td className="mono" style={Object.assign({textAlign: "right"}, late ? {color: "var(--warn)"} : {})}>{relAge(s.lastAt)}</td>
            <td><div style={{display: "flex", gap: 5, justifyContent: "flex-end"}}>
              <button className="chip" onClick={() => window.__ui.dialog(volumeOpsDialog(s), s)}>Operate…</button>
              <button className="chip" onClick={() => detach(s)}>Detach</button>
            </div></td>
          </tr>
        );
      })}</tbody></table>
    </div></div>
  );
}

const ConditionsCard = ({conditions}) => !(conditions || []).length ? null : (
  <>
    <div className="sech"><h2>Conditions</h2><span className="ln"></span></div>
    <div className="card"><div className="bd" style={{padding: 0}}>
      <table className="dt"><thead><tr><th>Type</th><th>Status</th><th>Reason</th><th>Message</th><th>Since</th></tr></thead>
      <tbody>{conditions.map((c, i) => (
        <tr key={i}>
          <td className="mono">{c.type}</td>
          <td style={{color: c.status === "True" ? "var(--ok)" : "var(--dim)"}}>{c.status}</td>
          <td className="mono">{c.reason || "—"}</td>
          <td style={{fontSize: 11.5, color: "var(--dim)"}}>{c.message || "—"}</td>
          <td className="mono">{c.lastTransitionTime ? relAge(c.lastTransitionTime) : "—"}</td>
        </tr>
      ))}</tbody></table>
    </div></div>
  </>
);

const OpsHistory = ({ops, nav, title}) => {
  const rows = ops || [];
  if (!rows.length) return null;
  return (
    <>
      <div className="sech"><h2>{title}</h2><span className="ln"></span><span className="count">{rows.length}</span></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr>
          <th>Operation</th><th>Action</th><th>Scope</th><th>Phase</th><th>Outcome</th><th>Started</th>
        </tr></thead>
        <tbody>{rows.map(o => {
          const failed = o.results.filter(r => r.status === "failed").length;
          return (
            <tr key={o.id} className={o.phase === "Failed" ? "bad" : ""}>
              <td><button className="tlink" onClick={() => nav.detail(o)}>{o.name}</button></td>
              <td>{(OPS_ACTION_META[o.action] || {}).label || o.action}</td>
              <td className="mono">{o.scope}</td>
              <td>{o.phase}{o.subphase ? <span style={{color: "var(--dim2)"}}> · {o.subphase}</span> : null}</td>
              <td className="mono">{o.results.length ? `${o.results.length - failed}/${o.results.length} ok` : "—"}</td>
              <td className="mono">{o.startedAt ? relAge(o.startedAt) : "—"}</td>
            </tr>
          );
        })}</tbody></table>
      </div></div>
    </>
  );
};

Object.assign(window, {REPL_ANNOTATION, Mono, SLOT_HINT, smeta, MODE_META, ModeBadge,
  OPS_ACTION_META, SCOPE_HINT, relAge, minsSince, REPL_NOW,
  PairTile, RPolicyTile, SlotTile, ReplOpsTile,
  PairDetail, RPolicyDetail, SlotDetail, ReplOpsDetail,
  SlotTable, ConditionsCard, OpsHistory});
