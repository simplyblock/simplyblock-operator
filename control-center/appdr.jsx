// ---------------------------------------------------------------------------
// PROTECTED APPLICATIONS
//
// One application binds a workload to a plan. The plan declares several
// protection methods; each becomes a leg. Exactly one leg is orchestrated —
// Ramen drives one DRPC per application, because a DRPC selects PVCs by label
// and two over the same PVCs would both claim them. The other legs run with
// identical parameters in the data plane and report their lag up the side
// channel, which is the only reason this page can show them at all.
// ---------------------------------------------------------------------------
const PROG_HINT = {
  Completed: "steady state",
  PinningGeneration: "pinning the chosen generation out of band, before the placement is rebound",
  UpdatingPlacement: "rebinding the placement control to the vault policy",
  MaterialisingVolumes: "the driver is building volumes from the pinned generation's objects",
  RestoringKubeObjects: "recreating the application's Kubernetes objects from the metadata bucket",
  WaitingForResourceRestore: "waiting for the restored objects to become ready",
  UpdatedPlacement: "placement updated, waiting for the workload to settle",
  "Cleaning Up": "removing the stale workload at the old site",
  WaitOnUserToCleanUp: "the stale workload has to be deleted by hand before DR resumes",
  EnsuringVolumesAreSecondary: "demoting the old site's volumes so only one side is writable",
  PreparingFinalSync: "flushing the last delta so nothing is lost on a planned move",
  RunningFinalSync: "shipping the final delta",
  Deleting: "tearing the relationship down"
};

// ---- one leg -------------------------------------------------------------
// A leg is one declared method's replication relationship. Its RPO comes from
// DRPC.status only for the orchestrated one; every other leg's lag is a
// side-channel read (payload 5).
function LegRow({leg, plan, app, nav}) {
  const m = mmeta(leg.type);
  const method = plan ? (plan.methods || []).find(x => x.name === leg.method) : null;
  const budget = method && method.interval ? ivMins(method.interval) * 60 : 0;
  const over = budget && leg.lagSeconds > budget;
  const dead = leg.state === "Unprotected";
  return (
    <div className={"legrow" + (leg.orchestrated ? " orch" : "") + (dead ? " dead" : "")}>
      <span className="ldot" style={{background: dead ? "var(--bad)" : m.c}}></span>
      <div className="lmain">
        <div className="lhead">
          <b>{leg.method}</b>
          <MethodBadge type={leg.type} sm />
          {leg.orchestrated
            ? <span className="badge" style={{color: "var(--accent)", borderColor: "var(--accent-line)"}} title="the method Ramen currently drives through the application's single DRPC">orchestrated</span>
            : <span className="badge" title="running in the data plane with identical parameters; its lag is a side-channel read">data plane only</span>}
          <span className="spacer"></span>
          <button className="lab link" onClick={() => nav.openSiteByName && nav.openSiteByName(leg.target)}><Icon n="k8s" s={10} />{leg.target}</button>
        </div>
        {dead
          ? <div className="lnote" style={{color: "var(--bad)"}}>{leg.note}</div>
          : <div className="lstats">
              <div><span>RPO target</span><b>{leg.type === "sync" ? "0" : (method ? method.interval : "—")}</b></div>
              <div><span>Actual lag</span><b style={over ? {color: "var(--bad)"} : leg.type === "sync" ? {color: "var(--ok)"} : null}>{fmtLag(leg.lagSeconds)}</b></div>
              <div><span>Last cycle</span><b>{leg.lastAt ? clockOf(leg.lastAt) : "—"}</b></div>
              {leg.type === "snapshot-s3"
                ? <div><span>Generations</span><b>{leg.generations}</b></div>
                : <div><span>Shipped</span><b>{leg.bytesLastCycle != null ? fmtBytes(leg.bytesLastCycle) : "inline"}</b></div>}
            </div>}
        {leg.type === "snapshot-s3" && !dead && !!leg.generations && (
          <div className="labels" style={{marginTop: 7}}>
            <span className="lab"><i>oldest</i>{fmtDate(leg.oldestAt)}</span>
            <span className="lab"><i>newest</i>{fmtDate(leg.newestAt)}</span>
            {leg.lockedUntil && <span className="lab" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}><Icon n="lock" s={10} />locked until {fmtDate(leg.lockedUntil)}</span>}
            {method && !method.failback && <span className="lab" title="the source is gone or untrusted after a restore, and the vault holds generations rather than a live peer"><i>failback</i>not available</span>}
          </div>
        )}
      </div>
    </div>
  );
}
const ivMins = iv => {
  if (!iv) return 0;
  const n = parseInt(iv, 10), u = String(iv).replace(/[\d.]/g, "");
  return n * (u === "m" ? 1 : u === "h" ? 60 : u === "d" ? 1440 : u === "w" ? 10080 : 1);
};

function LegsCard({a, plan, nav}) {
  const legs = a.legs || [];
  return (
    <>
      <div className="sech"><h2>Protection legs</h2><span className="ln"></span>
        <span className="count">{legs.filter(l => l.orchestrated).length} orchestrated of {legs.length}</span></div>
      <div className="card"><div className="bd" style={{padding: "4px 0"}}>
        {legs.map(l => <LegRow key={l.id} leg={l} plan={plan} app={a} nav={nav} />)}
        {!legs.length && <div className="lmsg">This application's plan declares no method, so nothing is protected.</div>}
      </div></div>
      <p className="mdesc">Ramen drives exactly one leg. Switching which one rebinds the application's placement control — a delete and a create, because every DRPolicy field is immutable. The remaining legs keep replicating with identical parameters; only their reporting path differs, which is why their lag is read from the control plane rather than from Kubernetes.</p>
    </>
  );
}

// ---- protection lag, rolled up over the group -----------------------------
function GroupLagCard({a, nav}) {
  const g = a.group || {volumes: []};
  const orch = (a.legs || []).find(l => l.orchestrated) || {};
  return (
    <div className="card" style={{marginTop: 12}}><h3>Group protection lag</h3><div className="bd">
      <div className="kv">
        <div><span>Group point in time</span><b>{g.lastAt ? clockOf(g.lastAt) : "never"}</b></div>
        <div><span>Written since</span><b style={g.writtenSince ? {color: "var(--warn)"} : null}>{fmtBytes(g.writtenSince || 0)}</b></div>
        <div><span>Orchestrated leg</span><b>{orch.method || "—"}</b></div>
      </div>
      {!!(g.volumes || []).length && <table className="dt" style={{marginTop: 10}}><thead><tr>
        <th>Volume</th><th>Status</th><th style={{textAlign: "right"}}>Last safe</th><th style={{textAlign: "right"}}>Written since</th>
      </tr></thead>
      <tbody>{g.volumes.map(v => (
        <tr key={v.id}>
          <td><button className="tlink" onClick={() => { const o = REG[v.id]; o && nav.detail(o); }}>{v.name}</button></td>
          <td><TrafficLight status={v.status} sm label /></td>
          <td className="mono" style={{textAlign: "right"}}>{v.lastAt ? clockOf(v.lastAt) : "—"}</td>
          <td className="mono" style={{textAlign: "right"}}>{fmtBytes(v.writtenSince)}</td>
        </tr>
      ))}</tbody></table>}
      <p className="mdesc" style={{margin: "9px 0 0"}}>The group is only as current as its most stale member. Crash consistency across the set holds only when every volume shares one storageID — Ramen derives the consistency-group boundary from it, so volumes on different storageIDs land in different groups and a restore spanning them is not consistent.</p>
    </div></div>
  );
}

// ---- generation catalogue (payload 4) -------------------------------------
// No Kubernetes object represents a generation, so this whole table is a
// side-channel read. Selecting one pins it out of band immediately before the
// placement rebind, because PromoteVolume carries no point-in-time argument.
function GenerationsCard({a, nav}) {
  const [tier, setTier] = useState("");
  const {data, loading, reload} = useResource("gens|" + a.id, () => api.appGenerations(a.id), 20000);
  const gens = (data || a.generations || []).filter(g => !tier || g.tier === tier);
  const tiers = [...new Set((a.generations || []).map(g => g.tier))];
  const restore = g => window.__ui.dialog(restoreGenerationDialog(a, g), a);
  const verify = async g => {
    try { await api.appVerifyGeneration(a.id, g.generation); window.__toast(`Generation ${g.generation} verified`); reload(); }
    catch (e) { window.__toast(e.message); }
  };
  if (!a.vaultMethod) return null;
  return (
    <>
      <div className="sech"><h2>Generation catalogue</h2><span className="ln"></span>
        <SourceTag what="simplyblock control plane" />
        <span className="count">{(a.generations || []).length} restorable</span></div>
      <div className="card">
        <div className="ptools">
          <div className="chips">
            <button className={"chip" + (tier ? "" : " on")} onClick={() => setTier("")}>all tiers</button>
            {tiers.map(t => <button key={t} className={"chip" + (tier === t ? " on" : "")} onClick={() => setTier(t)}>{t}</button>)}
          </div>
          <div className="spacer"></div>
          <span className="count">{gens.length}</span>
        </div>
        <div className="bd" style={{padding: 0}}>
          {loading && !gens.length ? <div className="lmsg">loading…</div>
            : !gens.length ? <div className="lmsg">No generation in this tier.</div>
            : <table className="dt"><thead><tr>
                <th>Gen</th><th>Taken</th><th>Tier</th><th>Kind</th>
                <th style={{textAlign: "right"}}>Size</th><th>Integrity</th><th>Object lock</th><th style={{width: 130}}></th>
              </tr></thead>
              <tbody>{gens.map(g => {
                const ok = g.integrity === "Verified";
                const pinned = a.pinnedGeneration === g.generation;
                const restored = a.restoredFromGeneration === g.generation;
                return (
                  <tr key={g.generation} className={pinned ? "on" : ""}>
                    <td className="mono">{g.generation}{pinned && <span className="badge" style={{marginLeft: 5, color: "var(--info)"}}>pinned</span>}{restored && <span className="badge" style={{marginLeft: 5, color: "var(--ok)"}}>restored</span>}</td>
                    <td className="mono">{fmtDate(g.at)}</td>
                    <td>{g.tier}</td>
                    <td>{g.kind === "full"
                      ? <span className="lab" style={{color: "var(--ro)"}}>full</span>
                      : <span style={{color: "var(--dim2)"}}>delta</span>}</td>
                    <td className="mono" style={{textAlign: "right"}}>{fmtBytes(g.size)}</td>
                    <td>{ok
                      ? <span className="lab" style={{color: "var(--ok)"}}><Icon n="check" s={10} />verified</span>
                      : <span className="lab" style={{color: "var(--warn)"}}><Icon n="alert" s={10} />{g.integrity}</span>}</td>
                    <td>{g.lockedUntil
                      ? <span className="lab" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}><Icon n="lock" s={10} />{fmtDate(g.lockedUntil)}</span>
                      : <span style={{color: "var(--dim2)"}}>none</span>}</td>
                    <td>
                      <div style={{display: "flex", gap: 5, justifyContent: "flex-end"}}>
                        {!ok && <button className="chip" onClick={() => verify(g)}>Verify</button>}
                        <button className="chip" disabled={!ok} title={ok ? null : "verify the generation first"} onClick={() => restore(g)}>Restore</button>
                      </div>
                    </td>
                  </tr>
                );
              })}</tbody></table>}
          <p className="mdesc" style={{margin: "0 12px", padding: "9px 0 12px"}}>Restoring pins the chosen generation out of band and then rebinds the application to the vault policy — the ordinary Ramen failover path, with one addition. The driver reads the pin when the promote arrives and materialises volumes from those objects. Locked generations cannot be deleted before their lock expires, which is the guarantee the vault exists for; the object store enforces it, not the resource lifecycle.</p>
        </div>
      </div>
    </>
  );
}

// ---- tile ------------------------------------------------------------------
function ProtectedAppTile({a, nav}) {
  const legs = a.legs || [];
  const orch = legs.find(l => l.orchestrated) || {};
  const busy = ["FailingOver", "Relocating"].includes(a.phase);
  return (
    <div className="tile" style={{"--sc": STATUS_META[a.status].c}} onDoubleClick={() => nav.detail(a)}>
      <TileHead obj={a} left={<><TrafficLight status={a.phase} /><Name>{a.name}</Name></>}
        right={<span className="badge">{a.appKind}</span>} />
      <div className="tsub" style={{marginTop: 2}}>namespace {a.namespace}</div>
      <Uuid value={a.id} />
      <div className="labels">
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openPlanByName && nav.openPlanByName(a.planName);}}><Icon n="shield" s={10} />{a.planName}</button>
        <button className="lab link" onClick={e => {e.stopPropagation(); nav.openSiteByName && nav.openSiteByName(a.activeSite);}}><i>active</i>{a.activeSite}</button>
        {a.activeSite !== a.preferredSite && <span className="lab" style={{color: "var(--warn)"}}><i>preferred</i>{a.preferredSite}</span>}
        {a.cgId && <button className="lab link" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}
          onClick={e => {e.stopPropagation(); nav.openCgroup(a.cgId);}}><Icon n="link" s={10} />{a.cgName}</button>}
      </div>
      <div className="mlist">
        {legs.map(l => (
          <div className={"mrow" + (l.state === "Unprotected" ? " bad" : "")} key={l.id}>
            <MethodBadge type={l.type} sm />
            <b>{l.method}</b>
            {l.orchestrated && <span className="ordot" title="orchestrated by Ramen"></span>}
            <span className="spacer"></span>
            {l.state === "Unprotected"
              ? <span className="mono" style={{color: "var(--bad)"}}>unprotected</span>
              : <span className="mono rpo">{fmtLag(l.lagSeconds)}</span>}
            {l.type === "snapshot-s3" && !!l.generations && <span className="mono" style={{color: "var(--dim2)"}}>{l.generations} gen</span>}
          </div>
        ))}
        {!legs.length && <div className="nolim" style={{color: "var(--bad)"}}>No leg — this application is not protected</div>}
      </div>
      <div className="kv">
        <div><span>PVCs</span><b>{a.counts.pvcs}</b></div>
        <div><span>Group safe as of</span><b style={a.rpoMet ? null : {color: "var(--bad)"}}>{a.group && a.group.lastAt ? clockOf(a.group.lastAt) : "—"}</b></div>
        <div><span>Written since</span><b style={a.group && a.group.writtenSince ? {color: "var(--warn)"} : null}>{fmtBytes((a.group || {}).writtenSince || 0)}</b></div>
      </div>
      {busy && <div className="prepbox running">
        <span className="dots"><i></i><i></i><i></i></span>
        {a.progression} — {PROG_HINT[a.progression] || "in progress"}
      </div>}
      {a.phase === "WaitForUser" && <div className="prepbox" style={{color: "var(--bad)", borderColor: "color-mix(in srgb,var(--bad) 40%,transparent)"}}>
        Waiting for the operator to clean up the stale workload before DR resumes.
      </div>}
      {a.needsReprotect && <div className="prepbox" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}>
        Restored from generation {a.restoredFromGeneration} — re-protect as a new plan with a full baseline.
      </div>}
      <Foot items={[
        {label: "PVCs", count: a.counts.pvcs, icon: "volume", onClick: () => nav.layer(a, "pvcs")},
        {label: "Details", right: true, onClick: () => nav.detail(a)}
      ]} />
    </div>
  );
}

// ---- detail ----------------------------------------------------------------
function ProtectedAppDetail({o: a, nav}) {
  const busy = ["FailingOver", "Relocating"].includes(a.phase);
  const {data: plan} = useResource("app.plan|" + a.planId, () => a.planId ? api.plan(a.planId) : Promise.resolve(null), 0);
  const legs = a.legs || [];
  const orch = legs.find(l => l.orchestrated) || {};
  const method = plan ? (plan.methods || []).find(m => m.name === a.orchestratedMethod) : null;
  // a leg is behind when its lag has passed its own method's interval, which is
  // the only definition the plan gives
  const behind = legs.filter(l => {
    if (l.state === "Unprotected") return true;
    const m = plan ? (plan.methods || []).find(x => x.name === l.method) : null;
    return m && m.interval && l.lagSeconds > ivMins(m.interval) * 60;
  });
  return (
    <div>
      <DetailHead obj={a} title={`${a.namespace}/${a.name}`}
        badge={<><span className="badge">{a.appKind}</span><TrafficLight status={a.phase} />
          {a.kubeObjectProtection && <span className="badge" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 45%,transparent)"}}>k8s objects protected</span>}</>}
        sub={<span style={{fontSize: 11.5, color: "var(--dim)"}}>
          plan <Ref onClick={() => nav.openPlanByName(a.planName)} label={a.planName} /> · orchestrated method <b>{a.orchestratedMethod}</b></span>} />

      {a.phase === "WaitForUser" && <div className="banner"><Icon n="alert" s={15} />
        <span><b>Waiting for the operator.</b> The application is running at the target, but the stale workload at the old site must be deleted before DR can resume. Use <b>Confirm cleanup</b> once it is gone.</span></div>}
      {busy && <div className="banner" style={{color: "var(--info)", background: "color-mix(in srgb,var(--info) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)"}}>
        <Icon n="clock" s={15} /><span><b>{a.phase} — {a.progression}.</b> {PROG_HINT[a.progression] || "In progress."} This is asynchronous; the phase advances on its own.</span></div>}
      {a.pinnedGeneration != null && <div className="banner" style={{color: "var(--ro)", background: "color-mix(in srgb,var(--ro) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"}}>
        <Icon n="camera" s={15} /><span><b>Generation {a.pinnedGeneration} is pinned.</b> The pin is a side-channel value the driver reads when the promote arrives, because PromoteVolume has no point-in-time argument. It is cleared once the promote resolves — a stale pin would make the next ordinary failover resolve to an old generation.</span></div>}
      {a.needsReprotect && <div className="banner" style={{color: "var(--ro)", background: "color-mix(in srgb,var(--ro) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--ro) 35%,transparent)"}}>
        <Icon n="alert" s={15} /><span><b>Restored from generation {a.restoredFromGeneration} — failback is not available.</b> What cannot be reconstructed is the original site's pre-compromise state: the source volume is gone or untrusted, and the vault holds generations rather than a live peer. Re-protect this application as a new plan; it will take a full baseline.</span></div>}
      {behind.length > 0 && !busy && <div className="banner" style={{color: "var(--warn)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)"}}>
        <Icon n="alert" s={15} /><span><b>{behind.length === 1 ? "A leg is" : `${behind.length} legs are`} outside the scheduling interval.</b> {behind.map(l => l.method).join(", ")} {behind.length === 1 ? "is" : "are"} behind; a failover onto {behind.length === 1 ? "that leg" : "those legs"} now would lose more work than the plan allows.</span></div>}

      <div className="stats">
        <Stat k="Phase" v={STATUS_META[a.phase].label} c={STATUS_META[a.phase].c} />
        <Stat k="Progression" v={a.progression} s={PROG_HINT[a.progression] || ""} />
        <Stat k="Active site" v={a.activeSite} s={a.activeSite === a.preferredSite ? "the preferred site" : `preferred is ${a.preferredSite}`} />
        <Stat k="Orchestrated" v={a.orchestratedMethod} s={method ? mmeta(method.type).label : ""} />
        <Stat k="Legs" v={legs.length} s={`${legs.filter(l => l.status === "healthy").length} healthy`} />
        <Stat k="PVCs" v={a.counts.pvcs} s={`VRG ${a.vrgState}`} />
      </div>

      <LegsCard a={a} plan={plan} nav={nav} />

      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="volume" title="Protected PVCs" sub={a.cgId ? `from ${a.cgName}` : "matched by label selector"} count={a.counts.pvcs} onClick={() => nav.layer(a, "pvcs")} />
        <NavCard icon="shield" title="Protection plan" sub={`${a.planName} · ${legs.length} methods`} count="→" onClick={() => nav.openPlanByName(a.planName)} />
        {a.cgId && <NavCard icon="link" title="Consistency group" sub="the volume set failed over together" count="→" onClick={() => nav.openCgroup(a.cgId)} />}
        <NavCard icon="k8s" title="Active site" sub={a.activeSite} count="→" onClick={() => nav.openSiteByName(a.activeSite)} />
      </div>

      <GenerationsCard a={a} nav={nav} />

      <div className="dcols">
        <div className="card"><h3>Placement control</h3><div className="bd" style={{paddingTop: 2, paddingBottom: 2}}>
          <Props rows={[
            ["Application", a.name], ["Namespace", a.namespace], ["Kind", a.appKind],
            ["Plan", <Ref onClick={() => nav.openPlanByName(a.planName)} label={a.planName} />],
            ["Storage profile", a.storageProfile],
            ["Orchestrated method", <span><b>{a.orchestratedMethod}</b>{method ? ` · ${mmeta(method.type).label}` : ""}</span>],
            ["Bound DRPolicy", <span className="mono">{plan && orch.target ? `sb-${a.activeSite}-${orch.target}${method && method.interval ? "-" + method.interval : ""}` : "—"}</span>],
            ["Phase", <TrafficLight status={a.phase} />], ["Progression", a.progression],
            ["Pending action", a.action || "none"],
            ["Preferred site", <Ref onClick={() => nav.openSiteByName(a.preferredSite)} label={a.preferredSite} />],
            ["Active site", <Ref onClick={() => nav.openSiteByName(a.activeSite)} label={a.activeSite} />],
            ["Failover targets", a.failoverTargets.length ? a.failoverTargets.join(", ") : "none — failback unavailable after a vault restore"],
            ["Restore targets", a.restoreTargets.length ? a.restoreTargets.join(", ") : "none"],
            ["Volume basis", a.cgId
              ? <Ref onClick={() => nav.openCgroup(a.cgId)} label={`consistency group ${a.cgName}`} />
              : "PVC label selector"],
            ["VRG state", a.vrgState],
            ["Kubernetes object protection", a.kubeObjectProtection ? "enabled" : "disabled"],
            ["Recipe", a.recipe ? `${a.recipe.namespace}/${a.recipe.name} · ${((a.recipe.recoverWorkflow || {}).sequence || []).length} recover steps` : "none — unordered restore"],
            a.restoredFromGeneration != null ? ["Restored from", `generation ${a.restoredFromGeneration}`] : null,
            ["Protected since", fmtDate(a.createdAt)]
          ].filter(Boolean)} /></div></div>
        <div>
          <div className="card"><h3>{a.cgId ? "Volume basis" : "PVC selector"}</h3><div className="bd">
            {a.cgId
              ? <>
                  <div className="labels">
                    <button className="lab link" style={{color: "var(--ro)", borderColor: "color-mix(in srgb,var(--ro) 40%,transparent)"}}
                      onClick={() => nav.openCgroup(a.cgId)}><Icon n="link" s={10} />{a.cgName}</button>
                    <span className="lab"><i>pvcs</i>{a.counts.pvcs}</span>
                  </div>
                  <div className="fnote" style={{marginTop: 10, marginBottom: 0}}><Icon n="alert" s={12} />
                    The group's members are exactly the volumes protected and failed over together, and its membership is fixed while its policies are attached, so the set cannot drift underneath the application. Crash consistency holds only across volumes sharing one storageID.</div>
                </>
              : <>
                  <div className="labels">{Object.entries((a.selector || {}).matchLabels || a.selector || {}).map(([k, v]) => <span className="lab" key={k}><i>{k}</i>{v}</span>)}</div>
                  <div className="fnote" style={{marginTop: 10, marginBottom: 0}}><Icon n="alert" s={12} />
                    The selector must never be empty — an empty selector claims every PVC in the namespace, and two placement controls would then contend over the same volumes. A PVC created later that matches is picked up automatically; VolSync-created PVCs are excluded.</div>
                </>}
          </div></div>
          <GroupLagCard a={a} nav={nav} />
          <RecipeCard a={a} />
          <div className="card" style={{marginTop: 12}}><h3>What each move costs</h3><div className="bd">
            <p className="mdesc"><b>Fail over</b> — unplanned. The application comes up on a peer site from the last synced group; anything written after that sync is lost, and the old site should be fenced. Available on {a.failoverTargets.length ? a.failoverTargets.join(", ") : "no site"}.</p>
            <p className="mdesc"><b>Fail back</b> — planned. Replication runs in reverse and the move only proceeds once the group is inside its interval, so nothing is lost.</p>
            <p className="mdesc" style={{marginBottom: 0}}><b>Restore from a generation</b> — the ransomware path. A chosen generation is materialised on {a.restoreTargets.length ? a.restoreTargets.join(", ") : "no site"} and the application is re-protected from scratch afterwards. Recovery takes minutes rather than seconds, because volumes are built from object storage.</p>
          </div></div>
        </div>
      </div>
    </div>
  );
}

Object.assign(window, {GroupLagCard, LegsCard, LegRow, GenerationsCard,
  ProtectedAppTile, ProtectedAppDetail, PROG_HINT, ivMins});
