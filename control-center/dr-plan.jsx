// ---------------------------------------------------------------------------
// DR TOP LAYER — protection plans and sites
//
// The plan is the only thing authored: sites, a storage profile, and the
// methods it declares. DRCluster, DRPolicy, DRPlacementControl and the
// replication class matrix are all derived from it, and are shown read-only so
// an operator can see what the orchestrator produced without being invited to
// edit objects whose fields are immutable.
// ---------------------------------------------------------------------------

// The one table that puts all three methods side by side. Everything else in
// the control path is identical between them.
const METHOD_META = {
  sync: {label: "synchronous", short: "sync", c: "var(--ok)",
    rpo: "0", rto: "seconds", genSelect: false, failback: true,
    vrMode: "sync",
    target: "Peer cluster storage, inline write mirror",
    how: "Every write is mirrored to the peer before it is acknowledged. Both sites must be in one region."},
  async: {label: "asynchronous", short: "async", c: "var(--info)",
    rpo: "= interval", rto: "seconds", genSelect: false, failback: true,
    vrMode: "async",
    target: "Peer cluster storage, block delta per epoch",
    how: "A block delta is shipped to the peer once per interval. The peer holds a whole volume, one epoch behind."},
  "snapshot-s3": {label: "generation vault", short: "vault", c: "var(--ro)",
    rpo: "= interval", rto: "minutes — materialisation from object store",
    genSelect: true, failback: false,
    vrMode: "snapshot-s3",
    target: "S3 bucket, immutable snapshot objects + manifest",
    how: "Each interval uploads an immutable generation to an object store. A restore materialises a volume set from a chosen generation."}
};
const mmeta = t => METHOD_META[t] || METHOD_META.async;
const MethodBadge = ({type, sm}) => {
  const m = mmeta(type);
  return <span className="badge" style={{color: m.c, borderColor: `color-mix(in srgb,${m.c} 45%,transparent)`}}>{sm ? m.short : m.label}</span>;
};
const fmtLag = s => s == null ? "—" : s === 0 ? "0s"
  : s < 60 ? `${s}s` : s < 3600 ? `${Math.floor(s / 60)}m ${s % 60}s` : `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;

// ---- tiles -----------------------------------------------------------------
function PlanTile({p, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[p.status].c}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><TrafficLight status={p.status} /><Name>{p.name}</Name></>}
        right={<span className="badge">plan</span>} />
      <div className="tsub" style={{marginTop: 2}}>{p.counts.methods} method{p.counts.methods === 1 ? "" : "s"} · {p.counts.sites} sites</div>
      <Uuid value={p.id} />
      {p.protectionGap && <div className="nolim" style={{color: "var(--bad)"}}>
        Interval mismatch — no replication class resolves, so a declared method is protecting nothing</div>}
      <div className="labels">
        <span className="lab"><i>profile</i>{p.storageProfile}</span>
        {p.siteNames.map(s => <span className="lab" key={s}><Icon n="k8s" s={10} />{s}</span>)}
      </div>
      <div className="mlist">
        {p.methods.map(m => (
          <div className="mrow" key={m.name}>
            <MethodBadge type={m.type} sm />
            <b>{m.name}</b>
            <span className="ar">→</span>
            <span className="mono">{m.target}</span>
            <span className="spacer"></span>
            <span className="mono rpo">{m.type === "sync" ? "RPO 0" : "RPO " + m.interval}</span>
            {!m.intervalConsistent && <Icon n="alert" s={11} c="var(--bad)" />}
          </div>
        ))}
        {!p.methods.length && <div className="nolim">No method declared — this plan protects nothing yet.</div>}
      </div>
      <Foot items={[
        {label: "Apps", count: p.counts.apps, icon: "cluster", onClick: () => nav.layer(p, "protectedapps")},
        {label: "Sites", count: p.counts.sites, icon: "k8s", onClick: () => nav.layer(p, "sites")},
        {label: "Gens", count: p.counts.generations, icon: "camera"}
      ]} onDetail={() => nav.detail(p)} />
    </div>
  );
}

function SiteTile({s, nav}) {
  const fenced = s.fencing !== "Unfenced";
  return (
    <div className="tile" style={{"--sc": STATUS_META[s.status] ? STATUS_META[s.status].c : "var(--ok)"}} onDoubleClick={() => nav.detail(s)}>
      <TileHead obj={s} left={<><TrafficLight status={s.status} /><Name>{s.name}</Name></>}
        right={<span className="badge">site</span>} />
      <div className="tsub" style={{marginTop: 2}}>{s.region}</div>
      <Uuid value={s.id} />
      {fenced && <div className="nolim" style={{color: "var(--bad)"}}>{s.fencing} — no I/O is accepted from this site</div>}
      <div className="labels">
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openK8s(s.k8sClusterId);}}><Icon n="k8s" s={10} />managed cluster</button>
        <span className="lab"><i>s3</i>{s.s3Profile}</span>
        <span className="lab"><i>ramen</i>{s.ramen}</span>
      </div>
      <div className="rw">
        <div><span>Active here</span><b>{s.counts.activeApps}</b></div>
        <div><span>Standby for</span><b>{s.counts.standbyApps}</b></div>
      </div>
      <Foot items={[
        {label: "Apps", count: s.counts.activeApps + s.counts.standbyApps, icon: "cluster", onClick: () => nav.layer(s, "protectedapps")},
        {label: "Plans", count: s.counts.plans, icon: "shield", onClick: () => nav.layer(s, "plans")},
        {label: "Classes", count: s.counts.classes, icon: "link"}
      ]} onDetail={() => nav.detail(s)} />
    </div>
  );
}

// ---- the method matrix on the plan detail ----------------------------------
function MethodMatrix({p, nav}) {
  const act = async (fn, msg) => { try { await fn(); window.__toast(msg); } catch (e) { window.__toast(e.message); } };
  return (
    <div className="card"><div className="bd" style={{padding: 0}}>
      <table className="dt"><thead><tr>
        <th>Method</th><th>Type</th><th>Target</th><th>RPO</th><th>RTO</th>
        <th>Generations</th><th>Failback</th><th>Class</th><th style={{width: 34}}></th>
      </tr></thead>
      <tbody>{p.methods.map(m => {
        const mm = mmeta(m.type);
        return (
          <tr key={m.name} className={m.intervalConsistent ? "" : "bad"}>
            <td><b>{m.name}</b></td>
            <td><MethodBadge type={m.type} sm /></td>
            <td className="mono">{m.target}</td>
            <td className="mono">{m.type === "sync" ? "0" : m.interval}</td>
            <td>{mm.rto}</td>
            <td>{mm.genSelect
              ? <span className="lab" style={{color: "var(--ro)"}}><Icon n="camera" s={10} />selectable</span>
              : <span style={{color: "var(--dim2)"}}>—</span>}</td>
            <td>{mm.failback
              ? <span className="lab" style={{color: "var(--ok)"}}><Icon n="check" s={10} />yes</span>
              : <span className="lab" title="the source is gone or untrusted after a vault restore, and the vault holds generations rather than a live peer" style={{color: "var(--dim2)"}}>no</span>}</td>
            <td className="mono">{m.intervalConsistent ? (m.cls ? m.cls.name : "—")
              : <span style={{color: "var(--bad)"}}>unresolved</span>}</td>
            <td><ActionBtn obj={Object.assign({kind: "method", id: p.id + "/" + m.name, planId: p.id, planName: p.name}, m)} /></td>
          </tr>
        );
      })}</tbody></table>
      {!p.methods.length && <div className="lmsg">No method declared. A plan without methods derives no policy and protects nothing.</div>}
    </div></div>
  );
}

// ---- the derivation view ---------------------------------------------------
// Ramen object fields are immutable, so every pair a method could ever use is
// created at onboarding. This is what the orchestrator produced; none of it is
// editable here on purpose.
function DerivationCard({p, nav}) {
  const [open, setOpen] = useState("policies");
  const bad = p.policies.filter(x => !x.validated);
  return (
    <>
      <div className="sech"><h2>Derived Ramen objects</h2><span className="ln"></span>
        <span className="count">{p.counts.sites} DRCluster · {p.counts.policies} DRPolicy · {p.counts.apps} DRPC · {p.methods.length} class</span></div>
      {!!bad.length && <div className="banner">
        <Icon n="alert" s={15} /><span><b>{bad.length} derived policy has no peerClass.</b> The policy validates cleanly and reports no error, but no replication class resolves for it — so any application bound to that method is protected by nothing. Fix the method's interval to emit it to both places at once.</span></div>}
      <div className="card">
        <div className="ptools">
          <div className="seg">
            {[["policies", "DRPolicy"], ["classes", "Replication classes"], ["cardinality", "Cardinality"]].map(([k, l]) => (
              <button key={k} className={open === k ? "on" : ""} onClick={() => setOpen(k)}>{l}</button>
            ))}
          </div>
          <div className="spacer"></div>
          <span className="live"><i></i>read-only — every field is immutable</span>
        </div>
        {open === "policies" && <div className="bd" style={{padding: 0}}>
          <table className="dt"><thead><tr>
            <th>Name</th><th>DR clusters</th><th>Interval</th><th>Selector</th><th>Method</th><th>peerClass</th>
          </tr></thead>
          <tbody>{p.policies.map(x => (
            <tr key={x.name} className={x.validated ? "" : "bad"}>
              <td className="mono">{x.name}</td>
              <td className="mono">{x.drClusters.join(" → ")}</td>
              <td className="mono">{x.schedulingInterval || <span style={{color: "var(--dim2)"}}>unset (sync)</span>}</td>
              <td className="mono">{Object.entries(x.selector).map(([k, v]) => k + "=" + v).join(", ")}</td>
              <td>{x.methodName || <span style={{color: "var(--dim2)"}}>unused pair</span>}</td>
              <td>{x.peerClass
                ? <span className="lab" style={{color: "var(--ok)"}}><Icon n="check" s={10} />{x.peerClass.replicationId}</span>
                : x.methodName ? <span className="lab" style={{color: "var(--bad)"}}><Icon n="alert" s={10} />absent — no protection</span>
                : <span style={{color: "var(--dim2)"}}>—</span>}</td>
            </tr>
          ))}</tbody></table>
          <p className="mdesc" style={{margin: "0 12px", padding: "9px 0 12px"}}>An absent peerClass is the only reliable signal that a method is not protecting anything. Ramen computes one per pair by intersecting the StorageClass storageID and the replication class replicationID across both clusters; if either fails to match, the policy still reports healthy.</p>
        </div>}
        {open === "classes" && <div className="bd" style={{padding: 0}}>
          <table className="dt"><thead><tr>
            <th>Class</th><th>Kind</th><th>replicationID</th><th>Labels</th><th>Parameters</th>
          </tr></thead>
          <tbody>{p.classes.map(c => (
            <tr key={c.name}>
              <td className="mono">{c.name}</td>
              <td style={{fontSize: 11}}>{c.crdKind}</td>
              <td className="mono">{c.replicationId}</td>
              <td className="mono" style={{fontSize: 10.5}}>{Object.entries(c.labels).map(([k, v]) => k + "=" + v).join(" ")}</td>
              <td className="mono" style={{fontSize: 10.5}}>{Object.entries(c.parameters).map(([k, v]) => k + "=" + v).join(" ")}</td>
            </tr>
          ))}</tbody></table>
          <p className="mdesc" style={{margin: "0 12px", padding: "9px 0 12px"}}>The parameters map is the only channel that reaches the driver: mode, interval, bucket, object lock and retention all arrive here. Ramen itself ignores everything except the interval and the selector labels. A single shared replicationID across the async family pre-validates every cluster pair at once, because Ramen matches replicationID pairwise; the synchronous class carries a second, pair-scoped identifier.</p>
        </div>}
        {open === "cardinality" && <div className="bd">
          <div className="cardin">
            {[
              ["DRCluster", p.counts.sites, "one per site, created once at onboarding", "region, s3ProfileName, cidrs, clusterFence"],
              ["DRPolicy", p.counts.policies, "one per pair × interval — all fields immutable, so every pair a method could ever use is created up front", "drClusters[2], schedulingInterval, replicationClassSelector"],
              ["DRPlacementControl", p.counts.apps, "one per application — the only object rewritten during operation; a rebind is a delete plus a create", "drPolicyRef, placementRef, pvcSelector, preferredCluster, action"],
              ["VRClass / VGRClass", p.methods.length, "one per method × interval — not a Ramen object, but the switch Ramen reads", "parameters (mode, schedulingInterval, retention, bucket)"]
            ].map(([k, n, why, fields]) => (
              <div className="cardrow" key={k}>
                <div className="cn"><b>{k}</b><span className="mono">×{n}</span></div>
                <div className="cw">{why}</div>
                <div className="cf mono">{fields}</div>
              </div>
            ))}
          </div>
        </div>}
      </div>
    </>
  );
}

// ---- the bypass channel ----------------------------------------------------
// Everything expressible in a Ramen resource goes through Ramen. These six
// payloads have no field anywhere in its API, and two of them are reads the
// console cannot be truthful without.
const BYPASS = [
  {n: 1, name: "Peer and topology binding", dir: "down",
    why: "VR, VGR and the gRPC calls carry no peer identity at all — Ramen assumes the driver already knows its topology.",
    params: "sourceSite, targetSite | bucket, transport, credentialsRef, replicationID"},
  {n: 2, name: "Generation selection", dir: "down",
    why: "DRPC failover has no point-in-time parameter; PromoteVolume means promote to current.",
    params: "recoveryPointRef | timestamp, consistencyGroup, storageClassMapping"},
  {n: 3, name: "Retention and lock enforcement", dir: "both",
    why: "Class parameters carry the intent, but enforcement state is object-store state with no CR.",
    params: "lockedUntil per object, complianceState, pendingExpiry, actual count vs policy"},
  {n: 4, name: "Generation catalogue", dir: "up", console: true,
    why: "No Kubernetes object represents a generation. VRG status describes the current relationship only.",
    params: "generation, timestamp, sizeBytes, integrityState, lockedUntil"},
  {n: 5, name: "Per-leg lag", dir: "up", console: true,
    why: "Only the orchestrated method has a VRG, so only its RPO appears in DRPC.status.lastGroupSyncTime.",
    params: "legID, lastSyncTime, lag, epoch, health — for every declared method"},
  {n: 6, name: "Arbitration token", dir: "both",
    why: "clusterFence and NetworkFence are pair-scoped; three sites need a quorum decision Ramen does not model.",
    params: "tokenHolder, generation, quorumAck[], fencedSites[]"}
];
function BypassCard() {
  const {data} = useResource("dr.arb", () => api.arbitration(), 8000);
  const t = data || {};
  return (
    <>
      <div className="sech"><h2>Side channel</h2><span className="ln"></span>
        <SourceTag what="simplyblock control plane" /></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr>
          <th style={{width: 26}}>#</th><th>Payload</th><th>Direction</th><th>Why no Ramen field</th><th>Parameters</th>
        </tr></thead>
        <tbody>{BYPASS.map(b => (
          <tr key={b.n}>
            <td className="mono">{b.n}</td>
            <td><b>{b.name}</b>{b.console && <span className="badge" style={{marginLeft: 6, color: "var(--accent)", borderColor: "var(--accent-line)"}}>this console reads it</span>}</td>
            <td className="mono">{b.dir}</td>
            <td style={{fontSize: 11.5, color: "var(--dim)"}}>{b.why}</td>
            <td className="mono" style={{fontSize: 10.5}}>{b.params}</td>
          </tr>
        ))}</tbody></table>
        <p className="mdesc" style={{margin: "0 12px", padding: "9px 0 0"}}>Payloads 4 and 5 are why an application reports every declared method while Ramen reports one. Read from Kubernetes alone, a plan with three methods shows a healthy single-leg posture and no generations at all.</p>
        <div className="arb">
          <span className="lab"><i>token holder</i>{t.holder || "—"}</span>
          <span className="lab"><i>generation</i>{t.generation != null ? t.generation : "—"}</span>
          <span className="lab"><i>quorum</i>{(t.quorumAck || []).length}/{t.quorumSize || "—"}</span>
          {(t.fencedSites || []).length
            ? <span className="lab" style={{color: "var(--bad)"}}><i>fenced</i>{t.fencedSites.join(", ")}</span>
            : <span className="lab" style={{color: "var(--ok)"}}><i>fenced</i>none</span>}
          <span className="lab"><i>updated</i>{fmtAgo(t.updatedAt)}</span>
        </div>
      </div></div>
    </>
  );
}

// ---- details ---------------------------------------------------------------
function PlanDetail({o: p, nav}) {
  const sync = p.methods.filter(m => m.type === "sync");
  const vault = p.methods.find(m => m.type === "snapshot-s3");
  const broken = p.methods.filter(m => !m.intervalConsistent);
  return (
    <div>
      <DetailHead obj={p} title={p.name} sub={p.storageProfile}
        badge={<><span className="badge">ProtectionPlan</span>
          {p.protectionGap && <span className="badge" style={{color: "var(--bad)", borderColor: "color-mix(in srgb,var(--bad) 45%,transparent)"}}>protection gap</span>}</>} />
      {broken.map(m => (
        <div className="banner" key={m.name}>
          <Icon n="alert" s={15} /><span><b>{m.name}: the interval is written to two places and they disagree.</b> The policy carries <span className="mono">{m.interval}</span> and the class parameters carry <span className="mono">{m.classInterval}</span>. Ramen resolves a class by matching the provisioner, that interval string and the selector labels — so no class resolves, no peerClass appears, the policy still validates, and every application on this method is protected by nothing. Both values must be emitted from one field.</span></div>
      ))}
      {p.methods.some(m => m.type === "sync") && p.methods.some(m => m.type !== "sync") && (
        <div className="banner" style={{color: "var(--accent)", background: "var(--accent-soft)", borderColor: "var(--accent-line)"}}>
          <Icon n="alert" s={15} /><span><b>One DRPC per application.</b> A DRPC selects PVCs by label, so two over the same PVCs would both claim them — the documented outcome is data corruption. Each application therefore names one orchestrated method; the others run with identical parameters in the data plane and report their lag out of band.</span></div>
      )}
      <div className="stats">
        <Stat k="Methods" v={p.counts.methods} s={p.methods.map(m => mmeta(m.type).short).join(" · ")} />
        <Stat k="Sites" v={p.counts.sites} s={p.siteNames.join(", ")} />
        <Stat k="Applications" v={p.counts.apps} s={`${p.counts.pvcs} PVCs`} />
        <Stat k="Worst leg lag" v={fmtLag(p.worstLagSeconds)} c={p.status === "healthy" ? null : "var(--warn)"} />
        <Stat k="Generations" v={p.counts.generations} s={vault ? `vault · ${vault.interval}` : "no vault method"} />
      </div>

      <div className="sech"><h2>Declared methods</h2><span className="ln"></span>
        <button className="btn" onClick={() => window.__ui.dialog(addMethodDialog(p), p)}><Icon n="plus" s={12} />Add method</button></div>
      <MethodMatrix p={p} nav={nav} />
      <p className="mdesc">All three methods are configured identically and travel the same control path down to the driver. What differs is the driver's implementation of the relationship: {sync.length ? "an inline write mirror to the peer, " : ""}a block delta per epoch to the peer, and an immutable snapshot stream into an object store. Method selection happens entirely at class resolution — the policy's selector and interval pick exactly one class, and that class's parameters are the whole difference.</p>

      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="cluster" title="Applications" sub="workloads bound to this plan" count={p.counts.apps} onClick={() => nav.layer(p, "protectedapps")} />
        <NavCard icon="k8s" title="Sites" sub="managed clusters this plan spans" count={p.counts.sites} onClick={() => nav.layer(p, "sites")} />
      </div>

      <DerivationCard p={p} nav={nav} />
      <BypassCard />

      <div className="sech"><h2>Plan</h2><span className="ln"></span></div>
      <Props rows={[
        ["Name", p.name], ["Storage profile", p.storageProfile],
        ["storageID", `sb-${p.name}`],
        ["Sites", p.siteNames.join(", ")],
        ["Vault bucket", vault ? vault.bucket : "—"],
        ["Object lock", vault ? (vault.immutable ? "compliance — generations cannot be deleted early" : "none") : "—"],
        ["Created", fmtDate(p.createdAt)]
      ]} />
    </div>
  );
}

function SiteDetail({o: s, nav}) {
  const fenced = s.fencing !== "Unfenced";
  return (
    <div>
      <DetailHead obj={s} title={s.name} sub={s.region}
        badge={<><span className="badge">DRCluster</span>
          {fenced && <span className="badge" style={{color: "var(--bad)", borderColor: "color-mix(in srgb,var(--bad) 45%,transparent)"}}>{s.fencing}</span>}</>} />
      {fenced && <div className="banner"><Icon n="alert" s={15} /><span><b>{s.fencing}.</b> No I/O is accepted from this site. Fencing in Ramen is pair-scoped, so with three or more sites the decision is taken by quorum through the arbitration token rather than by the DRCluster alone.</span></div>}
      <div className="stats">
        <Stat k="Active applications" v={s.counts.activeApps} s="running here now" />
        <Stat k="Standby for" v={s.counts.standbyApps} s="failover or restore target" />
        <Stat k="Plans" v={s.counts.plans} />
        <Stat k="Discovered classes" v={s.counts.classes} s="reported by DRClusterConfig" />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="cluster" title="Applications" sub="active or standby here" count={s.counts.activeApps + s.counts.standbyApps} onClick={() => nav.layer(s, "protectedapps")} />
        <NavCard icon="shield" title="Plans" sub="plans spanning this site" count={s.counts.plans} onClick={() => nav.layer(s, "plans")} />
        {s.k8sClusterId && <NavCard icon="k8s" title="Managed cluster" sub="the Kubernetes cluster itself" count="→" onClick={() => nav.openK8s(s.k8sClusterId)} />}
      </div>
      <div className="sech"><h2>DRCluster</h2><span className="ln"></span></div>
      <Props rows={[
        ["Name", <span className="mono">{s.name}</span>],
        ["Region", <span className="mono">{s.region}</span>],
        ["s3ProfileName", <span className="mono">{s.s3Profile}</span>],
        ["S3 endpoint", <span className="mono">{s.s3Endpoint}</span>],
        ["Metadata bucket", <span className="mono">{s.s3Bucket}</span>],
        ["CIDRs", <span className="mono">{s.cidrs.join(", ")}</span>],
        ["clusterFence", s.fencing],
        ["Ramen", s.ramen],
        ["Onboarded", fmtDate(s.createdAt)]
      ]} />
      <p className="mdesc">The site name must equal the OCM ManagedCluster name — it is the identity Ramen keys DRCluster on. The region is how synchronous and asynchronous protection are declared: an equal region on both sides of a pair permits a synchronous mirror, a distinct region does not.</p>
      {!!s.discoveredClasses.length && <>
        <div className="sech"><h2>Discovered replication classes</h2><span className="ln"></span></div>
        <div className="card"><div className="bd">
          <div className="labels">{s.discoveredClasses.map(c => <span className="lab mono" key={c}>{c}</span>)}</div>
          <p className="mdesc" style={{margin: "9px 0 0"}}>Reported up by DRClusterConfig on the managed cluster. A class present on one side of a pair and absent on the other produces no peerClass, and therefore no protection.</p>
        </div></div>
      </>}
    </div>
  );
}

// ---- DR landing ------------------------------------------------------------
function DrHome({nav}) {
  const plans = useResource("dr.plans", () => api.plans(), 8000);
  const sites = useResource("dr.sites", () => api.sites(), 12000);
  const apps = useResource("dr.apps", () => api.protectedApps(), 6000);
  const ps = plans.data || [], ss = sites.data || [], as = apps.data || [];
  const legs = as.flatMap(a => a.legs || []);
  const gaps = ps.filter(p => p.protectionGap);
  const worst = legs.reduce((n, l) => Math.max(n, l.lagSeconds || 0), 0);
  return (
    <div>
      <div className="dhead"><div style={{minWidth: 0, flex: 1}}>
        <h1>Disaster recovery</h1>
        <div className="dsub">One plan per protection posture, one application per workload. Ramen objects are derived.</div>
      </div>
      <button className="btn primary" onClick={() => window.__ui.dialog(newPlanDialog(ss), {kind: "plan", id: "new"})}><Icon n="plus" s={12} />New plan</button></div>

      {!!gaps.length && <div className="banner">
        <Icon n="alert" s={15} /><span><b>{gaps.length} plan{gaps.length === 1 ? " has" : "s have"} a protection gap.</b> A declared method's interval is written to the policy and to the class parameters and the two disagree, so no class resolves and the applications on that method are protected by nothing — while every object still reports healthy.</span></div>}

      <div className="stats">
        <Stat k="Plans" v={ps.length} s={`${ps.reduce((n, p) => n + p.counts.methods, 0)} declared methods`} />
        <Stat k="Sites" v={ss.length} s={[...new Set(ss.map(s => s.region))].join(", ")} />
        <Stat k="Applications" v={as.length} s={`${as.reduce((n, a) => n + a.counts.pvcs, 0)} PVCs`} />
        <Stat k="Legs" v={legs.length} c={legs.some(l => l.status === "unhealthy") ? "var(--bad)" : null}
          s={`${legs.filter(l => l.orchestrated).length} orchestrated`} />
        <Stat k="Worst leg lag" v={fmtLag(worst)} />
        <Stat k="Generations" v={as.reduce((n, a) => n + a.generations.length, 0)} s="restorable points" />
      </div>

      <div className="sech"><h2>Protection plans</h2><span className="ln"></span>
        <button className="chip" onClick={() => nav.drLayer("plans")}>Open all</button></div>
      <div className="grid">{ps.slice(0, 6).map(p => <PlanTile key={p.id} p={p} nav={nav} />)}</div>

      <div className="sech"><h2>Sites</h2><span className="ln"></span>
        <button className="chip" onClick={() => nav.drLayer("sites")}>Open all</button></div>
      <div className="grid">{ss.slice(0, 6).map(s => <SiteTile key={s.id} s={s} nav={nav} />)}</div>

      <div className="sech"><h2>Applications</h2><span className="ln"></span>
        <button className="chip" onClick={() => nav.drLayer("protectedapps")}>Open all</button></div>
      <div className="grid">{as.slice(0, 6).map(a => <ProtectedAppTile key={a.id} a={a} nav={nav} />)}</div>
    </div>
  );
}

Object.assign(window, {METHOD_META, mmeta, MethodBadge, fmtLag, BYPASS,
  PlanTile, SiteTile, PlanDetail, SiteDetail, MethodMatrix, DerivationCard, BypassCard, DrHome});
