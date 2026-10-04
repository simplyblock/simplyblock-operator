// ---------------------------------------------------------------------------
// DR HUB VIEWS — the console side of dr-simplyblock.
//
// Every screen here maps to a CR of dr.simplyblock.io (see drhub-api.jsx) and
// follows the design's interaction rules: directions are declared DRPaths and
// never inferred; readiness is the gate, and an override is a separate,
// audited path that needs a reason and the "override" verb; inventory is
// shown read-only; the console keeps no state of its own.
// ---------------------------------------------------------------------------
const ACTION_KIND_META = {
  Failover: {label: "Failover", icon: "shield", c: "var(--bad)", desc: "Unplanned move: the target takes over from the last replicated state. The source is assumed lost or fenced."},
  Relocate: {label: "Relocate", icon: "move", c: "var(--info)", desc: "Planned move: the application is stopped at the source, replication drains, the target starts. Failback is a Relocate along the opposite path."},
  Restart: {label: "Restart", icon: "refresh", c: "var(--warn)", desc: "Restart in place after a storage recovery. No path: the application stays where it is."},
  Resume: {label: "Resume", icon: "play", c: "var(--info)", desc: "Follow the move Ramen is still carrying out to its end: no new Ramen action, the blocking error in the journal, the target's probes once Ramen finishes. Use it after fixing what blocked the move."},
  Revert: {label: "Revert", icon: "swap", c: "var(--warn)", desc: "Point the move back at the site it started from: Ramen demotes the half-restored target and promotes the source again. Only while the target was never placed."},
  Test: {label: "Test", icon: "camera", c: "var(--accent)", desc: "Rehearsal in an isolated bubble on the target, from the latest replicated snapshot. Production is untouched."}
};
const VerdictBadge = ({v, sm}) => <TrafficLight status={v || "Unknown"} sm={sm} />;
const KindBadge = ({k}) => { const m = ACTION_KIND_META[k] || {c: "var(--dim)"}; return <span className="badge" style={{color: m.c, borderColor: `color-mix(in srgb,${m.c} 45%,transparent)`}}>{k}</span>; };
const Mono = ({children, dim, style}) => <span className="mono" style={Object.assign({}, dim ? {color: "var(--dim)"} : {}, style || {})}>{children}</span>;
const PathArrow = ({from, to}) => <span className="mono">{from} <span style={{color: "var(--dim2)"}}>→</span> {to}</span>;
const NsName = ({o}) => <span className="mono" style={{color: "var(--dim)"}}>{o.namespace ? o.namespace + "/" : ""}{o.name}</span>;
const Table = ({cols, rows, empty}) => !rows.length ? <div className="nolim">{empty || "Nothing to show."}</div> : (
  <table className="rules" style={{width: "100%", borderCollapse: "collapse"}}>
    <thead><tr>{cols.map((c, i) => <th key={i} style={{textAlign: "left", fontSize: 10, letterSpacing: ".07em", textTransform: "uppercase", color: "var(--dim2)", padding: "4px 8px 6px 0", fontWeight: 600}}>{c}</th>)}</tr></thead>
    <tbody>{rows.map((r, i) => <tr key={i}>{r.map((c, j) => <td key={j} style={{padding: "6px 8px 6px 0", borderTop: "1px solid var(--line)"}}>{c === null || c === undefined || c === "" ? <span style={{color: "var(--dim2)"}}>—</span> : c}</td>)}</tr>)}</tbody>
  </table>
);
const Conditions = ({o}) => !o.conditions.length ? null : <>
  <div className="sech"><h2>Conditions</h2><span className="ln"></span></div>
  <Table cols={["Type", "Status", "Reason", "Message", "Since"]} rows={o.conditions.map(c => [<b>{c.type}</b>,
    <span style={{color: c.status === "True" ? "var(--ok)" : c.status === "False" ? "var(--bad)" : "var(--dim)"}}>{c.status}</span>, <Mono>{c.reason}</Mono>, c.message, fmtAgo(c.since)])} />
</>;
const CheckTable = ({checks}) => <Table cols={["Check", "Status", "Blocking", "Reason", "Message"]} empty="No checks evaluated yet."
  rows={(checks || []).map(c => [<Mono>{c.name}</Mono>, <TrafficLight status={c.status} sm />, c.blocking ? <b style={{color: "var(--bad)"}}>yes</b> : <span style={{color: "var(--dim)"}}>advisory</span>, <Mono dim>{c.reason}</Mono>, c.message])} />;
const StepJournal = ({steps, empty}) => !steps.length ? <div className="nolim">{empty || "No steps recorded yet."}</div> : (
  <div className="steps">{steps.map((s, i) => {
    const res = s.result || s.phase || "";
    const cls = res === "Succeeded" ? "done" : res === "Running" ? "on" : res === "Failed" ? "failed" : res === "Skipped" ? "aborted" : "";
    return (
      <div className={"stp" + (res === "Pending" ? " pending" : "")} key={i}>
        <ul className="opsteps" style={{margin: 0}}><li className={cls}><i></i></li></ul>
        <div style={{flex: 1, minWidth: 0}}>
          <div style={{display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap"}}><b style={{fontSize: 12}}>{s.name}</b>{s.phase && <span className="badge">{s.phase}</span>}<TrafficLight status={res || "Pending"} sm />
            <span className="mono" style={{fontSize: 10.5, color: "var(--dim2)", marginLeft: "auto"}}>{s.startTime ? fmtDate(s.startTime) : ""}{s.endTime ? ` · ${fmtSecs(durMs2(s.startTime, s.endTime) / 1000)}` : ""}</span></div>
          {s.message && <div style={{fontSize: 11.5, color: "var(--dim)", marginTop: 3, overflowWrap: "anywhere"}}>{s.message}</div>}
          {s.logRef && <div className="mono" style={{fontSize: 10.5, color: "var(--dim2)", marginTop: 3}}>log · {s.logRef}</div>}
        </div>
      </div>
    );
  })}</div>
);
const durMs2 = (a, b) => a && b ? Math.max(0, Date.parse(b) - Date.parse(a)) : 0;
const PhaseStripDR = ({phases, current, terminal}) => {
  const idx = phases.indexOf(current);
  return <div className="phases">{phases.map((p, i) => <React.Fragment key={p}>
    {i > 0 && <span className="arr">›</span>}
    <span className={"ph" + (p === current ? " cur" : (idx > i || (terminal && idx < 0)) ? " done" : "")}>{(STATUS_META[p] || {}).label || p}</span>
  </React.Fragment>)}{terminal && !phases.includes(current) && <><span className="arr">›</span><span className="ph cur"><TrafficLight status={current} sm /></span></>}</div>;
};
const RunList = ({actions, tests, nav, limit}) => {
  const rows = [...(actions || []).map(a => ({o: a, kind: a.action, t: a.startTime || a.createdAt})), ...(tests || []).map(t => ({o: t, kind: "Test", t: t.startTime || t.createdAt}))]
    .sort((a, b) => (Date.parse(b.t) || 0) - (Date.parse(a.t) || 0)).slice(0, limit || 50);
  return <Table cols={["Kind", "Run", "Target", "Path", "Status", "Started", "Duration", "Operator"]} empty="No runs yet."
    rows={rows.map(({o, kind, t}) => [<KindBadge k={kind} />, <Ref label={o.name} onClick={() => nav.detail(o)} />, <Mono>{o.targetName}</Mono>, <Mono dim>{o.pathName || "in place"}</Mono>,
      <TrafficLight status={o.status} sm />, fmtDate(t), o.durationMs != null ? fmtSecs(o.durationMs / 1000) : "—", <Mono dim>{o.createdBy}</Mono>])} />;
};

// ---- dialogs ----------------------------------------------------------------
// Run action: the path picker offers only declared DRPaths whose `actions`
// include the kind; readiness on the chosen path is the gate. NotReady needs
// a reason, and the API server checks the override verb, not this form.
const runActionDialog = (target, kind, ctx) => {
  const m = ACTION_KIND_META[kind];
  const paths = (target.kind === "rplan" ? [target.pathName] : target.paths.filter(p => p.actions.includes(kind)).map(p => p.name));
  const verdictOf = name => target.kind === "rplan" ? target.verdict : ((target.paths.find(p => p.name === name) || {}).verdict || "Unknown");
  const opposite = target.kind === "papp" && kind === "Relocate" && target.currentCluster;
  return {
    title: `${m.label} ${target.kind === "rplan" ? "plan" : "application"} ${target.name}`, confirm: `Run ${m.label.toLowerCase()}`, danger: kind === "Failover",
    done: `${m.label} submitted — RecoveryAction created`,
    desc: m.desc,
    fields: v => {
      const pathless = PATHLESS_KINDS.includes(kind);
      const verdict = pathless ? "Ready" : verdictOf(v.path);
      const mv = target.move;
      return [
        mv && (kind === "Resume" || kind === "Revert") && {k: "nm", type: "note", label: `${mv.action} ${mv.from} → ${mv.to}, ${mv.phase === "Stuck" ? "stuck" : "in progress"}${mv.progression ? ` (Ramen: ${mv.progression})` : ""}${mv.blocking ? `. Blocked by: ${mv.blocking}` : ""}.`},
        mv && kind === "Revert" && {k: "nr", type: "note", label: `Ramen relocates the application back to ${mv.from} along the declared path ${mv.to} → ${mv.from}. Nothing ran on ${mv.to}, so nothing written there is lost.`},
        mv && kind === "Resume" && {k: "nu", type: "note", label: `No new Ramen action: the run waits for Ramen to finish the move to ${mv.to}, then checks the application's probes there. Fix what blocks the move first, or the run times out like the move did.`},
        !pathless && {k: "path", label: "DR path", type: "select", required: true,
          options: paths.map(p => { const pp = target.kind === "papp" ? target.paths.find(x => x.name === p) : null; return {v: p, l: pp ? `${p}  (${pp.from} → ${pp.to}, ${pp.verdict})` : p}; }),
          empty: `No declared DRPath offers ${m.label} for this ${target.kind === "rplan" ? "plan" : "application"}. Declaring a direction is a dr-admin decision, not an override.`},
        !pathless && verdict === "NotReady" && {k: "n1", type: "note", label: `Readiness on this path is NotReady: ${blockingChecks(target, v.path).join(", ") || "blocking checks failed"}. Running anyway needs a reason and the "override" verb on recoveryactions (dr-admin). The run is audited with the reason.`},
        !pathless && verdict === "NotReady" && {k: "override", label: "Override reason (10–1024 characters)", type: "text", required: true, maxLen: OVERRIDE_MAX,
          placeholder: "why this action must run despite the verdict", validate: overrideError,
          hint: x => `${(x || "").trim().length} characters; 10 to ${OVERRIDE_MAX}. Kept with the run in the audit record.`},
        !pathless && verdict === "Degraded" && {k: "n2", type: "note", label: "Readiness is Degraded: only advisory checks failed. The action runs without an override."},
        kind === "Restart" && target.kind === "papp" && (target.probes.length
          ? {k: "ptest", type: "check", label: `Health probes of ${target.name} (${target.probes.map(p => p.name || p.type).join(", ")})`, button: "Test probes now",
            hint: `dr-agent evaluates them on ${target.currentCluster || "the application's site"} now; the restart waits for the same probes.`,
            run: () => drhub.probeHealth({app: target}).then(healthAnswer)}
          : {k: "pnone", type: "note", label: "The application has no health probes: the restart reports it up once its tiers are ready. Add probes under Edit tiers & probes."}),
        {k: "timeout", label: "Timeout", type: "text", def: "30m", placeholder: "30m"},
        opposite && {k: "n3", type: "note", label: `The application currently runs on ${target.currentCluster}. A Relocate along a path whose target is the current cluster is refused by the hub.`}
      ].filter(Boolean);
    },
    run: v => drhub.runAction({kind, target, path: v.path, override: v.override && v.override.trim(), timeout: v.timeout && v.timeout.trim()})
  };
};
const OVERRIDE_MAX = 1024;
// The API server refuses a reason under 10 characters ("should be at least 10
// chars long"); said here while typing instead.
const overrideError = x => {
  const n = (x || "").trim().length;
  if (n > 0 && n < 10) return `The reason needs at least 10 characters (${n} so far).`;
  if (n > OVERRIDE_MAX) return `The reason may have at most ${OVERRIDE_MAX} characters (${n}).`;
  return null;
};
const blockingChecks = (target, path) => {
  const p = target.kind === "rplan" ? {checks: target.checks} : target.paths.find(x => x.name === path);
  return ((p && p.checks) || []).filter(c => c.blocking && c.status === "Fail").map(c => c.name);
};
const runTestDialog = target => {
  const paths = target.kind === "rplan" ? [target.pathName] : target.paths.filter(p => p.actions.includes("Test")).map(p => p.name);
  return {
    title: `Test ${target.kind === "rplan" ? "plan" : "application"} ${target.name}`, confirm: "Start test", done: "Test started — TestBubble created",
    desc: ACTION_KIND_META.Test.desc,
    fields: [
      {k: "path", label: "DR path", type: "select", required: true, options: paths.map(p => ({v: p, l: p})),
        empty: "No declared DRPath offers Test here. A path's `actions` must include Test and carry a `test` block (isolated NAD, quotas)."},
      {k: "cloneSource", label: "Clone source", type: "select", options: [{v: "latest-replicated-snapshot", l: "latest replicated snapshot (default)"}, {v: "secondary-snapshot", l: "secondary snapshot (feature gate)"}]},
      {k: "holdFor", label: "Hold the bubble for", type: "text", placeholder: "e.g. 30m — empty tears down right after validation"},
      {k: "maxLifetime", label: "Maximum lifetime", type: "text", def: "24h"},
      {k: "n1", type: "note", label: "A finished test files a report on the TestBubble and, with an archive configured, a PDF and JSON in the plan's bucket. `test-recent` on the path is satisfied by a passed test within recentWithin."}
    ],
    run: v => drhub.runTest({target, path: v.path, cloneSource: v.cloneSource, holdFor: v.holdFor && v.holdFor.trim(), maxLifetime: v.maxLifetime && v.maxLifetime.trim()})
  };
};
const restoreDialog = app => ({
  title: `Restore ${app.name} from backup`, confirm: "Restore", danger: true, done: "RestoreAction created",
  desc: "For sites rebuilt after a disaster: the application's volumes are restored from the newest S3 backup capture onto the cluster where the application is placed, then re-protected. A dr-admin decision; the data loss is reported on the RestoreAction.",
  fields: [
    !app.awaitingRestore && {k: "n0", type: "note", label: "This application does not carry the awaiting-restore annotation, so the hub will refuse the RestoreAction. It is set by dr-restore when the DR state is restored onto a fresh hub."},
    {k: "timeout", label: "Timeout", type: "text", def: "30m"},
    {k: "confirm", label: `Type ${app.name} to confirm`, type: "text", required: true, match: app.name}
  ].filter(Boolean),
  run: v => drhub.runRestore({app, timeout: v.timeout && v.timeout.trim()})
});
const holdDialog = t => ({
  title: `Hold test bubble ${t.name}`, confirm: "Update hold", done: "Hold updated",
  fields: [{k: "holdFor", label: "Hold for (from now)", type: "text", def: t.holdFor || "1h", required: true}],
  run: v => drhub.holdTest(t, v.holdFor.trim())
});
const deleteDialog = (o, note, needsConfirm) => ({
  title: `Delete ${KIND_LABEL_DR[o.kind] || o.kind} ${o.name}`, confirm: "Delete", danger: true, done: `${o.name} deleted`,
  desc: note,
  fields: [
    needsConfirm && {k: "n0", type: "note", label: "This object is still in use. The hub refuses the delete unless it is confirmed; confirming here sets dr.simplyblock.io/confirm-delete before deleting."},
    {k: "confirm", label: `Type ${o.name} to confirm`, type: "text", required: true, match: o.name}
  ].filter(Boolean),
  run: () => drhub.remove(o, needsConfirm)
});
const KIND_LABEL_DR = {pplan: "protection plan", drpath: "DR path", papp: "protected application", rplan: "recovery plan", raction: "recovery action",
  tbubble: "test", tsched: "test schedule", restore: "restore", siteprofile: "site profile", drconfig: "DR configuration", dhcpserver: "DHCP server", sitedeploy: "site storage deployment"};

const METHOD_TYPES = [{v: "async", l: "async — block replication per interval"}, {v: "sync", l: "sync — stretch cluster, RPO 0"},
  {v: "s3-backup", l: "s3-backup — snapshot backups to S3 only"}, {v: "async-s3-backup", l: "async + s3-backup"}, {v: "sync-s3-backup", l: "sync + s3-backup"}];
// Sites are entered as rows (site → cluster, zone, region). The one-line
// form below, name=cluster[/zone][@region]; ..., is still read for a value
// that arrives as text. Blanks are insignificant anywhere:
// around "=", "/", "@" and between entries, which may be separated by ";",
// "," or newlines -- or by blanks alone ("site-a=a site-b=b"). Site, cluster,
// zone and region names never contain blanks, so all of them are dropped
// (a blank kept in a name made the plan's S3 stores never match its sites).
const parseSites = txt => String(txt || "")
  .replace(/\s*([=/@])\s*/g, "$1")
  .split(/[\s;,]+/).filter(Boolean).map(l => {
    const [name, rest] = l.split("=");
    const [clusterZone, region] = (rest || "").split("@");
    const [cluster, zone] = (clusterZone || "").split("/");
    return Object.assign({name, cluster: cluster || name}, zone ? {zone} : {}, region ? {region} : {});
  });
const SITE_COLS = [
  {k: "name", label: "Site", placeholder: "site-a", flex: 1},
  {k: "cluster", label: "Cluster (managed cluster)", placeholder: "same as the site", flex: 1.2},
  {k: "zone", label: "Zone (sync)", placeholder: "", flex: 0.8},
  {k: "region", label: "Region", placeholder: "eu-central", flex: 0.8}
];
const SITE_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const emptySite = () => ({name: "", cluster: "", zone: "", region: ""});
const nb = s => String(s || "").replace(/\s+/g, "");
const sitesSpec = rows => typeof rows === "string" ? parseSites(rows)
  : (rows || []).filter(r => nb(r.name) || nb(r.cluster)).map(r => Object.assign({name: nb(r.name), cluster: nb(r.cluster) || nb(r.name)},
    nb(r.zone) ? {zone: nb(r.zone)} : {}, nb(r.region) ? {region: nb(r.region)} : {}));
// Why the declared sites cannot be saved, or null.
const sitesError = rows => {
  const sites = sitesSpec(rows);
  if (!sites.length) return "Declare the plan's sites: one row per site.";
  const unnamed = sites.filter(s => !s.name).length;
  if (unnamed) return `${unnamed} row${unnamed > 1 ? "s have" : " has"} a cluster but no site name.`;
  const bad = sites.filter(s => s.name.length > 63 || !SITE_RE.test(s.name)).map(s => s.name);
  if (bad.length) return `Not a site name: ${bad.join(", ")}. A site name is a DNS label: lower-case letters, digits and "-", at most 63 characters.`;
  const dup = sites.map(s => s.name).filter((n, i, a) => a.indexOf(n) !== i);
  if (dup.length) return `Site ${[...new Set(dup)].join(", ")} is declared twice.`;
  return null;
};
// The S3 store rows name a declared site from a list.
const s3Cols = siteNames => S3_COLS.map(c => c.k !== "site" ? c : Object.assign({}, c, {type: "select", blank: siteNames.length ? "— site —" : "— declare sites first —",
  options: siteNames.map(n => ({v: n, l: n})), unknown: v => `${v} (not a site of the plan)`}));
const nextSite = (siteNames, rows) => siteNames.find(n => !(rows || []).some(r => r.site === n)) || "";
// "Test" on an S3 store row: dr-hub probes the store (list, write, delete)
// with the row's Secret and answers with the S3 service's own error.
const s3Answer = st => {
  const r = st.result || {};
  if (st.phase === "Passed") return {status: "ok", text: `${r.bucket || "The bucket"} accepts a list, a write and a delete (checked from the DR hub${r.region ? `, region ${r.region}` : ""}).`};
  if (r.code === "SecretNotFound") return {status: "bad", text: `SecretNotFound: ${r.message}`};
  if (r.code) return {status: "bad", text: `${r.code}${r.step ? ` on ${r.step}` : ""}: ${r.message || "no message"}`};
  return {status: "bad", text: st.message || "the probe could not run"};
};
const testStoreRow = row => {
  if (!nb(row.bucket)) throw new Error("Fill in the bucket first.");
  if (!nb(row.endpoint)) throw new Error("Fill in the endpoint first.");
  const s = s3Profiles([Object.assign({}, row, {site: nb(row.site) || "probe"})])[0];
  return drhub.probeS3(Object.assign(s, {site: nb(row.site)})).then(s3Answer);
};
const S3_ROW_TEST = {label: "Test", title: "Probe this store from the DR hub: list, write and delete with its Secret", run: testStoreRow};
// "Test probe": dr-agent evaluates health probes on the site, now.
const healthAnswer = st => {
  const lines = (st.probes || []).map(p => ({status: p.passed ? "ok" : "bad", text: `${p.name}: ${p.message || (p.passed ? "passed" : "failed")}`}));
  const where = st.site ? `${st.site} (cluster ${st.cluster})` : st.cluster;
  if (st.phase === "Error") return {status: "bad", text: st.message || "the probes could not run", lines};
  const failed = lines.filter(l => l.status === "bad").length;
  return {status: failed ? "bad" : "ok", text: failed ? `${failed} of ${lines.length} probes fail${where ? ` on ${where}` : ""}` : `${lines.length === 1 ? "The probe passes" : `All ${lines.length} probes pass`}${where ? ` on ${where}` : ""}`, lines};
};
const probeRowSpec = row => {
  const pr = probesSpec([row]);
  if (!pr.length) throw new Error("Fill in the probe's target first.");
  return pr;
};
// The plan's per-site S3 stores must name every site of the plan: said here
// with the site that is missing, rather than as the API server's generic
// "s3Profiles needs a store for every site".
const checkStores = (sites, stores) => {
  if (!stores.length) return;
  const have = new Set(stores.map(s => s.site));
  const missing = sites.map(s => s.name).filter(n => !have.has(n));
  const unknown = stores.map(s => s.site).filter(n => !sites.some(x => x.name === n));
  if (missing.length || unknown.length)
    throw new Error([missing.length && `No S3 store for site ${missing.join(", ")}`,
      unknown.length && `S3 store for ${unknown.join(", ")}, which is not a site of the plan`].filter(Boolean).join("; ") +
      `. Sites: ${sites.map(s => s.name).join(", ")}.`);
};
// ---- form <-> spec helpers for the editable parts of the DR objects --------
// The secret a store names when the row leaves it empty: the one the DR hub
// chart creates in Ramen's namespace. Pre-filling it in the row looked like a
// placeholder and was typed a second time ("ramen-s3-secretramen-s3-secret").
const DEFAULT_S3_SECRET = "ramen-s3-secret";
const S3_COLS = [
  {k: "site", label: "Site", placeholder: "site-a", flex: 1},
  {k: "bucket", label: "Bucket", placeholder: "dr-site-a", flex: 1.4},
  {k: "endpoint", label: "Endpoint", placeholder: "https://s3.eu-central-1.amazonaws.com", flex: 2},
  {k: "region", label: "Region", placeholder: "eu-central-1", flex: 1},
  {k: "secretRef", label: "Secret (empty: ramen-s3-secret)", placeholder: "ramen-s3-secret", flex: 1}
];
const s3Rows = profiles => (profiles || []).map(p => ({site: p.site || "", bucket: p.bucket || "", endpoint: p.endpoint || "", region: p.region || "", secretRef: typeof p.secretRef === "string" ? p.secretRef : (p.secretRef || {}).name || ""}));
const s3Profiles = rows => (rows || []).filter(r => (r.site || "").trim() && (r.bucket || "").trim()).map(r => Object.assign(
  {site: r.site.replace(/\s+/g, ""), bucket: r.bucket.trim()}, r.endpoint && r.endpoint.trim() ? {endpoint: r.endpoint.trim()} : {},
  r.region && r.region.trim() ? {region: r.region.trim()} : {}, {secretRef: (r.secretRef || "").trim() || DEFAULT_S3_SECRET}));

// Tiers: one row per tier. The selector is either labels (k=v, k2=v2) or
// resource types (configmaps, secrets); the ready gates are a short list:
//   vmRunning | deploymentsReady | podsReady | exec(app=shop-tools; nc -z -w 3 db 3306; 900)
const READY_RE = /^exec\((.*)\)$/;
const tierRows = tiers => (tiers || []).map(t => {
  const sel = t.selector || {};
  const byLabels = sel.matchLabels && Object.keys(sel.matchLabels).length;
  return {name: t.name || "", by: byLabels ? "labels" : "resources",
    selector: byLabels ? Object.entries(sel.matchLabels).map(([k, v]) => `${k}=${v}`).join(", ") : (sel.resourceTypes || []).join(", "),
    ready: (t.ready || []).map(r => r.type === "exec"
      ? `exec(${Object.entries(r.selector || {}).map(([k, v]) => `${k}=${v}`).join(",")}; ${(r.command || []).join(" ")}${r.timeoutSeconds ? `; ${r.timeoutSeconds}` : ""})`
      : r.type).join(", ")};
});
const parseReady = s => (s || "").split(/,(?![^(]*\))/).map(x => x.trim()).filter(Boolean).map(x => {
  const m = READY_RE.exec(x);
  if (!m) return {type: x};
  const parts = m[1].split(";").map(p => p.trim());
  const sel = {};
  (parts[0] || "").split(",").map(p => p.trim()).filter(Boolean).forEach(kv => { const [k, v] = kv.split("="); if (k) sel[k.trim()] = (v || "").trim(); });
  const out = {type: "exec", selector: sel, command: (parts[1] || "").split(/\s+/).filter(Boolean)};
  if (parts[2] && Number(parts[2])) out.timeoutSeconds = Number(parts[2]);
  return out;
});
const tiersSpec = rows => (rows || []).filter(r => (r.name || "").trim()).map(r => {
  const selector = r.by === "resources"
    ? {resourceTypes: csv(r.selector)}
    : {matchLabels: Object.fromEntries(csv(r.selector).map(kv => { const [k, v] = kv.split("="); return [k.trim(), (v || "").trim()]; }).filter(([k]) => k))};
  const ready = parseReady(r.ready);
  return Object.assign({name: r.name.trim(), selector}, ready.length ? {ready} : {});
});
const TIER_COLS = [
  {k: "name", label: "Tier", placeholder: "db", flex: 0.8},
  {k: "by", label: "Select by", type: "select", options: [{v: "labels", l: "labels"}, {v: "resources", l: "resource types"}], flex: 0.9},
  {k: "selector", label: "Selector", placeholder: "dr.simplyblock.io/tier=db  |  configmaps, secrets", flex: 2},
  {k: "ready", label: "Ready when", placeholder: "vmRunning, exec(app=shop-tools; nc -z -w 3 db 3306; 900)", flex: 2.4}
];
const probeRows = probes => (probes || []).map(p => ({name: p.name || "", type: p.type || "http", target: p.target || "", timeout: p.timeout || "", expectStatus: p.expectStatus || ""}));
const probesSpec = rows => (rows || []).filter(r => (r.target || "").trim() || r.type === "vmRunning").map(r => Object.assign(
  {name: (r.name || "").trim() || r.type, type: r.type || "http"}, r.target && r.target.trim() ? {target: r.target.trim()} : {},
  r.timeout && String(r.timeout).trim() ? {timeout: String(r.timeout).trim()} : {}, Number(r.expectStatus) ? {expectStatus: Number(r.expectStatus)} : {}));
const PROBE_COLS = [
  {k: "name", label: "Probe", placeholder: "web", flex: 0.8},
  {k: "type", label: "Type", type: "select", options: [{v: "http", l: "http"}, {v: "tcp", l: "tcp"}], flex: 0.7},
  {k: "target", label: "Target (URL / host:port)", placeholder: "http://web.shop.svc.cluster.local/", flex: 2.4},
  {k: "timeout", label: "Timeout", placeholder: "15s", flex: 0.7},
  {k: "expectStatus", label: "HTTP status", type: "number", placeholder: "any 2xx", flex: 0.8}
];
const TIER_HINT = "Ready gates: vmRunning, deploymentsReady, podsReady, or exec(<pod labels k=v>; <command>; <timeout seconds>) run in a pod of the tier's namespace. Tiers restore in order; the next starts when every gate of the previous holds.";

// Site profile bindings (ADR 0020)
const LNET_COLS = [
  {k: "role", label: "Role", placeholder: "app", flex: 0.8},
  {k: "nad", label: "NetworkAttachmentDefinition (namespace/name)", placeholder: "app-net/vlan110", flex: 2.4}
];
const GNET_COLS = [
  {k: "role", label: "Role", placeholder: "app", flex: 0.7},
  {k: "cidr", label: "Guest subnet", placeholder: "192.168.110.0/24", flex: 1.3},
  {k: "reservedHostIDs", label: "Reserved host ids", placeholder: "1, 2", flex: 0.9},
  {k: "dhcpServerRef", label: "DHCP server", flex: 1.1}
];
// The guest-network DHCP server is one of the site's registered DHCPServers;
// a name that is not registered (typed before, or the server was deleted) is
// kept and flagged: guests on that network get no reservation.
const gnetCols = servers => GNET_COLS.map(c => c.k !== "dhcpServerRef" ? c : Object.assign({}, c, {type: "select", blank: "— the site's default —",
  options: servers.map(d => ({v: d.name, l: d.name})), unknown: v => `${v} (not registered)`}));
const srvRef = r => typeof r === "string" ? r : refName2(r);
// The DHCP servers a profile refers to (site default and per guest network).
const knownServers = sp => Array.from(new Set([srvRef(sp.dhcpServerRef)].concat((sp.guestNetworks || []).map(g => srvRef(g.dhcpServerRef))).filter(Boolean)));
// The ones of them no DHCPServer of the site answers to.
const missingServers = (sp, servers) => knownServers(sp || {}).filter(n => !(servers || []).some(d => d.name === n));
const lnetRows = sp => (sp.logicalNetworks || []).map(l => ({role: l.role || "", nad: l.nad || ""}));
const gnetRows = sp => (sp.guestNetworks || []).map(g => ({role: g.role || "", cidr: g.cidr || "", reservedHostIDs: (g.reservedHostIDs || []).join(", "), dhcpServerRef: g.dhcpServerRef || ""}));
const bindingsSpec = v => ({
  logicalNetworks: (v.lnets || []).filter(r => (r.role || "").trim() && (r.nad || "").trim()).map(r => ({role: r.role.trim(), nad: r.nad.trim()})),
  guestNetworks: (v.gnets || []).filter(r => (r.role || "").trim() && (r.cidr || "").trim()).map(r => Object.assign({role: r.role.trim(), cidr: r.cidr.trim()},
    csv(r.reservedHostIDs).length ? {reservedHostIDs: csv(r.reservedHostIDs).map(Number).filter(n => !Number.isNaN(n))} : {},
    r.dhcpServerRef ? {dhcpServerRef: r.dhcpServerRef} : {})),
  dhcpServerRef: v.dhcp || null
});

const newPlanDialog = () => ({
  title: "New protection plan", confirm: "Create plan", done: "ProtectionPlan created",
  desc: "A plan names the sites that take part in DR, the storage it protects and how it replicates. Ramen's DRCluster and DRPolicy objects and the replication classes are derived from it; directions are declared afterwards as DR paths.",
  fields: v => [
    {k: "name", label: "Name", type: "text", required: true, placeholder: "fra"},
    {k: "sites", label: "Sites — the site's name and the managed cluster it is", type: "rows", cols: SITE_COLS, max: 8, addLabel: "Add site", required: true,
      def: [emptySite(), emptySite()], add: () => emptySite(), validate: sitesError,
      hint: "A cross-cluster plan names one cluster per site; a sync plan (stretch cluster) names the same cluster on every site, each with its own zone."},
    {k: "type", label: "Replication method", type: "select", required: true, options: METHOD_TYPES},
    {k: "method", label: "Method name", type: "text", required: true, def: "primary", placeholder: "primary"},
    /^async/.test(v.type || "") && {k: "interval", label: "Scheduling interval", type: "text", required: true, def: "5m", placeholder: "5m"},
    /sync/.test(v.type || "") && {k: "n1", type: "note", label: "A sync plan is a stretch cluster: every site names the same cluster and its own zone (name=cluster/zone). No Ramen policy is derived; dr-agent moves applications between zones."},
    /backup/.test(v.type || "") && {k: "bInterval", label: "Backup interval", type: "text", required: true, def: "1h"},
    /backup/.test(v.type || "") && {k: "bRetention", label: "Backups retained", type: "number", min: 1, def: 24},
    {k: "sc", label: "Storage class selector (matchLabels)", type: "kv", max: 8},
    {k: "cg", label: "Consistency groups", type: "checkbox", def: false},
    {k: "scName", label: "Create the replicated StorageClass on every site, named (empty: the classes exist already)", type: "text", placeholder: "simplyblock-dr",
      hint: "dr-hub writes it on each site with the selector's labels, the site's storage cluster and pool; the selector needs at least one matchLabel."},
    {k: "scPool", label: "…from the pool (empty: the storage cluster's default pool)", type: "text", placeholder: ""},
    {k: "scFs", label: "…with the filesystem", type: "text", def: "xfs"},
    {k: "s3", label: "S3 stores — one per site (Ramen's metadata store and Velero's backups)", type: "rows", cols: s3Cols(sitesSpec(v.sites).map(s => s.name).filter(Boolean)), max: 8, addLabel: "Add store",
      rowAction: S3_ROW_TEST,
      add: rows => ({site: nextSite(sitesSpec(v.sites).map(s => s.name).filter(Boolean), rows), bucket: "", endpoint: rows.length ? rows[rows.length - 1].endpoint : "", region: rows.length ? rows[rows.length - 1].region : "", secretRef: rows.length ? rows[rows.length - 1].secretRef : ""}),
      hint: "The secret (access key id / secret access key) must exist in Ramen's namespace on the hub. Test probes a store from the hub before the plan is saved. Leave empty to name one existing profile below instead."},
    {k: "velero", label: "Velero namespace on the sites", type: "text", def: "velero", placeholder: "velero"},
    {k: "s3Profile", label: "Ramen S3 profile (single store, instead of per-site stores)", type: "text", placeholder: "existing profile name"},
    {k: "autoRestart", label: "Restart applications in place after a storage recovery", type: "checkbox", def: false},
    {k: "n2", type: "note", label: "Snapshot class selectors and replication parameters are taken from the storage class and the method; the spec stays editable afterwards except for the fields Ramen keys on (sites, methods)."}
  ].filter(Boolean),
  run: v => {
    const type = v.type;
    const method = Object.assign({name: v.method.trim(), type}, /^async/.test(type) ? {schedulingInterval: v.interval.trim()} : {},
      /backup/.test(type) ? {s3Backup: {interval: v.bInterval.trim(), retention: Number(v.bRetention) || 24}} : {});
    const sc = kvToObj(v.sc);
    const stores = s3Profiles(v.s3);
    const err = sitesError(v.sites);
    if (err) throw new Error(err);
    const sites = sitesSpec(v.sites);
    checkStores(sites, stores);
    const spec = Object.assign({sites, methods: [method],
      storageProfile: Object.assign({storageClassSelector: Object.keys(sc).length ? {matchLabels: sc} : {}}, {consistencyGroups: v.cg ? "Enabled" : "Disabled"},
        v.scName && v.scName.trim() ? {provision: Object.assign({name: v.scName.trim()}, v.scPool && v.scPool.trim() ? {pool: v.scPool.trim()} : {}, v.scFs && v.scFs.trim() ? {fsType: v.scFs.trim()} : {})} : {})},
      stores.length ? {s3Profiles: stores} : {}, v.velero && v.velero.trim() ? {veleroNamespace: v.velero.trim()} : {},
      v.s3Profile && v.s3Profile.trim() ? {s3Profile: {name: v.s3Profile.trim()}} : {}, v.autoRestart ? {autoRestart: {enabled: true}} : {});
    return drhub.createPlan({name: v.name.trim(), spec});
  }
});
const editPlanS3Dialog = p => ({
  title: `S3 stores of ${p.name}`, confirm: "Save", done: "ProtectionPlan updated",
  desc: "Ramen keeps its metadata and Velero its backups in one S3 store per site. Changing a store re-derives the DRClusters; applications keep their protection.",
  fields: [
    {k: "s3", label: "S3 stores — one per site", type: "rows", cols: s3Cols(p.sites.map(s => s.name)), max: 8, addLabel: "Add store", def: s3Rows(p.s3Profiles),
      rowAction: S3_ROW_TEST, hint: "Test probes a store from the DR hub (list, write, delete) with its Secret, before saving.",
      add: rows => ({site: nextSite(p.sites.map(s => s.name), rows), bucket: "", endpoint: rows.length ? rows[rows.length - 1].endpoint : "", region: rows.length ? rows[rows.length - 1].region : "", secretRef: rows.length ? rows[rows.length - 1].secretRef : ""})},
    {k: "velero", label: "Velero namespace on the sites", type: "text", def: p.veleroNamespace || "velero"}
  ],
  run: v => {
    const stores = s3Profiles(v.s3);
    checkStores(p.sites, stores);
    return drhub.patchPlan(p, {s3Profiles: stores, veleroNamespace: v.velero && v.velero.trim() ? v.velero.trim() : null});
  }
});

const newPathDialog = plans => ({
  title: "Declare a DR path", confirm: "Create path", done: "DRPath created",
  desc: "A DR path is a declared direction between two sites of a plan, and the set of actions allowed along it. Nothing in the console offers a target cluster: it offers a path.",
  fields: v => {
    const plan = plans.find(p => p.name === v.plan) || plans[0];
    const sites = plan ? plan.sites.map(s => ({v: s.name, l: `${s.name} (${s.cluster}${s.zone ? "/" + s.zone : ""})`})) : [];
    return [
      {k: "plan", label: "Protection plan", type: "select", required: true, options: plans.map(p => ({v: p.name, l: p.name})), empty: "Create a protection plan first."},
      {k: "from", label: "From site", type: "select", required: true, options: sites},
      {k: "to", label: "To site", type: "select", required: true, options: sites.filter(s => s.v !== v.from)},
      {k: "name", label: "Path name", type: "text", required: true, def: v.from && v.to ? `${v.from}-to-${v.to}` : "", placeholder: "fra-a-to-fra-b"},
      {k: "actions", label: "Allowed actions", type: "multiselect", required: true, options: [{v: "Failover", l: "Failover"}, {v: "Relocate", l: "Relocate"}, {v: "Test", l: "Test"}]},
      (v.actions || []).includes("Test") && {k: "nad", label: "Test: isolated NetworkAttachmentDefinition (ns/name)", type: "text", required: true, placeholder: "dr-test/isolated"},
      (v.actions || []).includes("Test") && {k: "cap", label: "Test: max clone capacity", type: "text", placeholder: "500Gi"},
      (v.actions || []).includes("Test") && {k: "recent", label: "Test: test-recent window", type: "text", def: "720h"},
      {k: "handover", label: "Announcement hand-over on move", type: "checkbox", def: false}
    ].filter(Boolean);
  },
  run: v => drhub.createPath({name: v.name.trim(), spec: Object.assign({from: v.from, to: v.to, planRef: v.plan, actions: v.actions, announcementHandover: !!v.handover},
    v.actions.includes("Test") ? {test: Object.assign({mode: "bubble", isolatedNad: v.nad.trim()}, v.cap ? {quotas: {maxCloneCapacity: v.cap.trim()}} : {}, v.recent ? {recentWithin: v.recent.trim()} : {})} : {})})
});
const protectAppDialogDR = (plans, cfg) => ({
  title: "Protect an application", confirm: "Protect", done: "ProtectedApplication created",
  desc: "Binds a workload to a plan, a source site and one target site. A discovered application (no OCM Placement) must live in Ramen's ops namespace; a managed one names its Placement. The hub derives the DRPlacementControl and, from tiers, a Recipe.",
  fields: v => {
    const plan = plans.find(p => p.name === v.plan) || plans[0];
    const sites = plan ? plan.sites.map(s => ({v: s.name, l: s.name})) : [];
    const methods = plan ? plan.methods.map(m => ({v: m.name, l: `${m.name} (${m.type}${m.interval ? " " + m.interval : ""})`})) : [];
    const opsNs = (cfg && cfg.ramen && cfg.ramen.opsNamespace) || DR_NS();
    return [
      {k: "plan", label: "Protection plan", type: "select", required: true, options: plans.map(p => ({v: p.name, l: p.name})), empty: "Create a protection plan first."},
      {k: "appKind", label: "Application kind", type: "select", required: true, options: [{v: "discovered", l: "discovered — no OCM Placement, protected by namespace + PVC selector"}, {v: "managed", l: "managed — deployed through an OCM Placement"}]},
      {k: "name", label: "Name", type: "text", required: true, placeholder: "shop"},
      {k: "namespace", label: "Namespace of the ProtectedApplication", type: "text", required: true, def: v.appKind === "managed" ? "" : opsNs, placeholder: v.appKind === "managed" ? "the Placement's namespace" : opsNs},
      {k: "source", label: "Source site", type: "select", required: true, options: sites},
      {k: "target", label: "Target site", type: "select", required: true, options: sites.filter(s => s.v !== v.source)},
      methods.length > 1 && {k: "method", label: "Method", type: "select", required: true, options: methods},
      v.appKind === "managed" && {k: "placement", label: "Placement name", type: "text", required: true},
      v.appKind !== "managed" && {k: "namespaces", label: "Protected namespaces (comma-separated)", type: "text", required: true, placeholder: "shop"},
      {k: "pvc", label: "PVC selector (matchLabels)", type: "kv", max: 8},
      v.appKind !== "managed" && {k: "recipe", label: "Hand-written Recipe (name, optional)", type: "text", placeholder: "leave empty to let the hub generate one from tiers"},
      {k: "tiers", label: "Tiers — the boot order the hub generates the Recipe from", type: "rows", cols: TIER_COLS, max: 12, addLabel: "Add tier", hint: TIER_HINT,
        add: () => ({name: "", by: "labels", selector: "", ready: ""})},
      {k: "probes", label: "Health probes — what a move waits for on the target", type: "rows", cols: PROBE_COLS, max: 8, addLabel: "Add probe",
        add: () => ({name: "", type: "http", target: "", timeout: "15s", expectStatus: ""}),
        hint: "Test runs a probe now, by dr-agent on the source site, against the running application.",
        rowAction: {label: "Test", title: "dr-agent runs this probe on the source site now", run: (row, fv) => {
          if (!fv.plan || !fv.source) throw new Error("Choose the plan and the source site first.");
          return drhub.probeHealth({plan: fv.plan, site: fv.source, namespaces: fv.appKind === "managed" ? [] : csv(fv.namespaces), probes: probeRowSpec(row)}).then(healthAnswer);
        }}},
      {k: "n1", type: "note", label: "Both directions between source and target must exist as DR paths for readiness to become Ready. External hooks are edited on the object."}
    ].filter(Boolean);
  },
  run: v => {
    const pvc = kvToObj(v.pvc);
    const sel = Object.keys(pvc).length ? {matchLabels: pvc} : {};
    const tiers = tiersSpec(v.tiers), probes = probesSpec(v.probes);
    const spec = Object.assign({planRef: v.plan, source: v.source, target: v.target, kind: v.appKind},
      v.method ? {method: v.method} : {}, tiers.length ? {tiers} : {}, probes.length ? {health: {probes}} : {},
      v.appKind === "managed" ? {managed: {placementRef: {name: v.placement.trim()}, pvcSelector: sel}}
        : {discovered: Object.assign({protectedNamespaces: csv(v.namespaces), pvcSelector: sel}, v.recipe && v.recipe.trim() ? {recipeRef: {name: v.recipe.trim()}} : {})});
    return drhub.createApp({name: v.name.trim(), namespace: v.namespace.trim(), spec});
  }
});
const editTiersDialog = a => ({
  title: `Tiers & probes of ${a.name}`, confirm: "Save", done: "ProtectedApplication updated",
  desc: "The tiers are the boot order: the hub generates the Recipe Ramen restores by from them. The probes are what a Failover or Relocate waits for before it reports the application up on the target.",
  fields: [
    {k: "tiers", label: "Tiers (boot order)", type: "rows", cols: TIER_COLS, max: 12, addLabel: "Add tier", hint: TIER_HINT, def: tierRows(a.tiers),
      add: () => ({name: "", by: "labels", selector: "", ready: ""})},
    {k: "probes", label: "Health probes", type: "rows", cols: PROBE_COLS, max: 8, addLabel: "Add probe", def: probeRows(a.probes),
      add: () => ({name: "", type: "http", target: "", timeout: "15s", expectStatus: ""}),
      rowAction: {label: "Test", title: "dr-agent runs this probe where the application runs, now", run: row => drhub.probeHealth({app: a, probes: probeRowSpec(row)}).then(healthAnswer)}},
    {k: "ptest", type: "check", label: "All probes as edited", button: "Test all probes",
      hint: `dr-agent evaluates them on ${a.currentCluster || "the application's site"} now; nothing is saved.`,
      run: v => { const pr = probesSpec(v.probes); if (!pr.length) throw new Error("No probe to test."); return drhub.probeHealth({app: a, probes: pr}).then(healthAnswer); }}
  ],
  run: v => drhub.patchApp(a, {tiers: tiersSpec(v.tiers), health: {probes: probesSpec(v.probes)}})
});

const newRPlanDialog = (paths, apps) => ({
  title: "New recovery plan", confirm: "Create plan", done: "RecoveryPlan created",
  desc: "An ordered set of applications moved together along one DR path: priorities run in sequence, applications of one priority in parallel. A plan action fans out one RecoveryAction per application.",
  fields: v => {
    const path = paths.find(p => p.name === v.path);
    const onPath = apps.filter(a => !path || a.paths.some(x => x.name === path.name));
    const nss = [...new Set(onPath.map(a => a.namespace))];
    return [
      {k: "path", label: "DR path", type: "select", required: true, options: paths.map(p => ({v: p.name, l: `${p.name} (${p.from} → ${p.to})`}))},
      {k: "namespace", label: "Namespace", type: "select", required: true, options: nss.map(n => ({v: n, l: n})), empty: "No application is on this path yet."},
      {k: "name", label: "Name", type: "text", required: true, placeholder: "tier-1"},
      {k: "apps", label: "Applications (in this namespace)", type: "multiselect", required: true, options: onPath.filter(a => a.namespace === v.namespace).map(a => ({v: a.name, l: `${a.name} · ${a.verdict}`}))},
      {k: "priorities", label: "Priorities — name=priority, comma-separated (default 1)", type: "text", placeholder: "db=1, api=2, web=3"},
      {k: "gate", label: "Between priorities", type: "select", options: [{v: "allHealthy", l: "wait until all applications of the previous priority are healthy"}, {v: "none", l: "no gate"}]},
      {k: "cont", label: "Continue on failure", type: "checkbox", def: false}
    ];
  },
  run: v => {
    const prio = Object.fromEntries(csv(v.priorities).map(x => x.split("=")).filter(x => x.length === 2).map(([k, p]) => [k, Number(p) || 1]));
    return drhub.createRPlan({name: v.name.trim(), namespace: v.namespace, spec: {pathRef: v.path, applications: v.apps.map(a => ({name: a, priority: prio[a] || 1})),
      gates: {betweenPriorities: v.gate || "allHealthy"}, continueOnFailure: !!v.cont}});
  }
});
const newScheduleDialog = (target, nsHint) => ({
  title: target ? `Schedule tests for ${target.name}` : "New test schedule", confirm: "Create schedule", done: "TestSchedule created",
  desc: "Runs a test along a path on a cron schedule (UTC). Schedules never overlap; old bubbles are pruned by the retention.",
  fields: v => [
    !target && {k: "n0", type: "note", label: "Open an application or recovery plan and use its Actions menu to schedule tests against it."},
    {k: "name", label: "Name", type: "text", required: true, def: target ? `${target.name}-weekly` : ""},
    {k: "schedule", label: "Cron (UTC)", type: "text", required: true, def: "0 3 * * 0", placeholder: "0 3 * * 0"},
    target && {k: "path", label: "DR path", type: "select", required: true, options: (target.kind === "rplan" ? [target.pathName] : target.paths.filter(p => p.actions.includes("Test")).map(p => p.name)).map(p => ({v: p, l: p}))},
    {k: "keepLast", label: "Keep last N test bubbles", type: "number", min: 1, def: 10},
    {k: "keepFor", label: "Keep for", type: "text", placeholder: "720h"},
    {k: "suspend", label: "Create suspended", type: "checkbox", def: false}
  ].filter(Boolean),
  run: v => drhub.createSchedule({name: v.name, namespace: target ? target.namespace : nsHint, schedule: v.schedule.trim(), target, path: v.path, keepLast: v.keepLast, keepFor: v.keepFor && v.keepFor.trim(), suspend: v.suspend})
});

const editBindingsDialog = s => ({
  title: `Bindings of ${s.name}`, confirm: "Save", done: "SiteProfile updated",
  desc: "How this site's networks map for recovered VMs (ADR 0020): the NAD each logical role is on here, the guest subnet of each role with the host ids never handed out, and the DHCP server the reservations are rendered to.",
  prepare: () => drhub.dhcpServers().then(ds => ({servers: ds.filter(d => d.site === s.name)})),
  fields: (v, prep) => {
    const servers = (prep && prep.servers) || [];
    const cur = srvRef((s.spec || {}).dhcpServerRef);
    const unregistered = n => n && !servers.some(d => d.name === n);
    return [
    !servers.length && {k: "n0", type: "note", label: `No DHCP server is registered for ${s.name} yet. Register one under Disaster recovery → DHCP servers; until then guest addresses are not reserved on this site.`},
    {k: "lnets", label: "Logical networks — role → NAD on this site", type: "rows", cols: LNET_COLS, max: 8, addLabel: "Add network", def: lnetRows(s.spec || {}),
      add: () => ({role: "app", nad: ""}), hint: s.nads && s.nads.length ? `NADs reported here: ${s.nads.map(n => n.namespace ? `${n.namespace}/${n.name}` : n.name || n).slice(0, 8).join(", ")}` : ""},
    {k: "gnets", label: "Guest networks — the subnet of each role here", type: "rows", cols: gnetCols(servers), max: 8, addLabel: "Add subnet", def: gnetRows(s.spec || {}),
      add: () => ({role: "app", cidr: "", reservedHostIDs: "1, 2", dhcpServerRef: ""}),
      rowError: r => unregistered(r.dhcpServerRef) ? `DHCP server ${r.dhcpServerRef} is not registered for ${s.name}: guests on ${r.role || "this network"} get no reservation.` : null,
      hint: `The DHCP servers registered for ${s.name}: ${servers.map(d => d.name).join(", ") || "none"}. Empty uses the site's default below.`},
    {k: "dhcp", label: "DHCP server of the site (default for every guest network)", type: "select", def: cur,
      options: [{v: "", l: "— none —"}].concat(servers.map(d => ({v: d.name, l: `${d.name} (${d.target || d.type})`})), unregistered(cur) ? [{v: cur, l: `${cur} (not registered)`}] : []),
      validate: x => unregistered(x) ? `DHCP server ${x} is not registered for ${s.name}.` : null}
    ].filter(Boolean);
  },
  run: v => drhub.patchSiteProfile(s, bindingsSpec(Object.assign({}, v, {dhcp: v.dhcp && v.dhcp.trim() ? v.dhcp.trim() : ""})))
});

const newDHCPServerDialog = (sites, profiles) => ({
  title: "Register a DHCP server", confirm: "Create", done: "DHCPServer created",
  desc: "A DHCP server of one site that guest addresses are reserved on. dr-hub never talks to it: it renders the reservations (<mac>,<ip>,<vm>) into the server's ConfigMap on the site, and dnsmasq reads them from its --dhcp-hostsdir. A SiteProfile's guestNetworks[role].dhcpServerRef (or spec.dhcpServerRef) names it.",
  fields: [
    {k: "site", label: "Site (managed cluster)", type: "select", required: true, options: sites.map(s => ({v: s, l: s})), empty: "No site profile reported yet."},
    {k: "name", label: "Name", type: "text", required: true, placeholder: "dhcp-site-a"},
    {k: "type", label: "Type", type: "select", options: [{v: "dnsmasq", l: "dnsmasq — reservations ConfigMap mounted as --dhcp-hostsdir"}]},
    {k: "namespace", label: "ConfigMap namespace (on the site)", type: "text", required: true, placeholder: "dhcp"},
    {k: "configMap", label: "ConfigMap name", type: "text", required: true, placeholder: "sitemap-hosts"},
    {k: "bind", label: "Bind it as the site's DHCP server (the site profile's default and every guest network without one)", type: "checkbox", def: true},
    {k: "n1", type: "note", label: "Guests need a pinned MAC and an address inside the role's guest subnet to get a reservation; the subnets are the site profile's bindings."}
  ],
  run: v => drhub.createDHCPServer({name: v.name.trim(), site: v.site, namespace: v.namespace.trim(), configMap: v.configMap.trim()}).then(r => {
    const prof = (profiles || []).find(p => p.name === v.site);
    if (!v.bind || !prof) return r;
    const sp = prof.spec || {}, name = dns63(v.name.trim());
    return drhub.patchSiteProfile(prof, {dhcpServerRef: name,
      guestNetworks: (sp.guestNetworks || []).map(g => Object.assign({}, g, g.dhcpServerRef ? {} : {dhcpServerRef: name}))}).then(() => r);
  })
});

// ---- a managed site's storage (StorageSiteDeployment) ----------------------
// The hub console cannot reach a site's API server; the operator on the hub
// carries the request there through OCM. The console writes the request, the
// sizing and the approval, and reads back the projected draft and cluster.
const sizingFields = z => [
  {k: "name", label: "Storage cluster name", type: "text", def: (z && z.name) || "", placeholder: "sb-site-a"},
  {k: "vcpuCount", label: "vCPUs per storage node", type: "number", def: z && z.vcpuCount != null ? z.vcpuCount : "", min: 1, placeholder: "8"},
  {k: "minHugePagesSize", label: "Hugepages per storage node", type: "text", def: (z && z.minHugePagesSize) || "", placeholder: "8G"},
  {k: "maxSubsystemCount", label: "NVMe-oF subsystems per node", type: "number", def: z && z.maxSubsystemCount != null ? z.maxSubsystemCount : "", min: 1, placeholder: "30"},
  {k: "dataChunks", label: "Erasure coding: data chunks", type: "number", def: z && z.stripe && z.stripe.dataChunks != null ? z.stripe.dataChunks : "", min: 1, placeholder: "1"},
  {k: "parityChunks", label: "Erasure coding: parity chunks", type: "number", def: z && z.stripe && z.stripe.parityChunks != null ? z.stripe.parityChunks : "", min: 0, placeholder: "1"},
  {k: "enableDriveFormat", label: "Format the devices it takes (data on them is lost)", type: "checkbox", def: !!(z && z.enableDriveFormat)}
];
const num = v => v === "" || v == null ? null : Number(v);
const sizingOf = v => {
  const z = {};
  if (v.name && v.name.trim()) z.name = dns63(v.name.trim());
  if (num(v.vcpuCount) != null) z.vcpuCount = num(v.vcpuCount);
  if (v.minHugePagesSize && v.minHugePagesSize.trim()) z.minHugePagesSize = v.minHugePagesSize.trim();
  if (num(v.maxSubsystemCount) != null) z.maxSubsystemCount = num(v.maxSubsystemCount);
  if (num(v.dataChunks) != null || num(v.parityChunks) != null)
    z.stripe = Object.assign({}, num(v.dataChunks) != null ? {dataChunks: num(v.dataChunks)} : {}, num(v.parityChunks) != null ? {parityChunks: num(v.parityChunks)} : {});
  if (v.enableDriveFormat) z.enableDriveFormat = true;
  return z;
};
const deploySiteDialog = (sites, taken) => ({
  title: "Deploy storage on a managed site", confirm: "Discover", done: "StorageSiteDeployment created — discovery requested on the site",
  desc: "The operator on this hub runs a discovery on the site through Open Cluster Management and writes a draft deployment document there. You review the draft here, with the sizing below applied, and approve it; nothing is configured on any node before the approval.",
  fields: [
    {k: "site", label: "Site (managed cluster)", type: "select", required: true, options: sites.filter(s => !taken.includes(s)).map(s => ({v: s, l: s})), empty: "Every managed cluster has a storage deployment already, or none has joined the hub."},
    {k: "namespace", label: "Namespace of the request on the hub", type: "text", required: true, def: (window.SB_CONFIG || {}).namespace || "simplyblock"},
    {k: "enableControlPlaneNodes", label: "Include control-plane nodes in the discovery (every node of a small site is one)", type: "checkbox", def: true},
    {k: "workers", label: "Limit to these nodes (comma-separated; empty = every node)", type: "text", placeholder: ""},
    ...sizingFields(null),
    {k: "n1", type: "note", label: "Approval is one-way and reboots the site's storage nodes to set hugepages and core isolation."}
  ],
  run: v => drhub.createSiteDeploy({site: v.site, namespace: v.namespace.trim(), enableControlPlaneNodes: v.enableControlPlaneNodes, workers: csv(v.workers), sizing: sizingOf(v)})
});
const resizeSiteDialog = d => ({
  title: `Size the draft of ${d.site}`, confirm: "Apply sizing", done: "Sizing sent to the site's draft",
  desc: "Written onto the draft's cluster template on the site. Fields left empty keep what the discovery wrote.",
  fields: sizingFields(d.sizing),
  run: v => drhub.patchSiteDeploySizing(d, sizingOf(v))
});
const approveSiteDialog = d => ({
  title: `Approve the storage deployment of ${d.site}?`, confirm: "Approve and deploy", danger: true, done: "Approved — the site's draft is expanding",
  desc: `Approval is one-way. The site's ${d.counts.nodes} node(s) are configured (hugepages, core isolation; this reboots them), the storage nodes are added and the cluster ${((d.draft || {}).cluster || {}).name || (d.sizing || {}).name || ""} is activated in the control plane.`,
  fields: [{k: "confirm", label: `Type ${d.site} to confirm`, type: "text", required: true, match: d.site}],
  run: () => drhub.approveSiteDeploy(d)
});

// ---- command registry (kebab menus) ----------------------------------------
// `op` is what access.can() checks: failover/relocate/restart/test map to
// create on the run kinds, override to the override verb, delete to delete.
Object.assign(ACTIONS, {
  pplan: p => [
    {label: "Edit S3 stores", icon: "cloud", op: "update", dialog: editPlanS3Dialog(p)},
    {label: "Delete plan", icon: "trash", danger: true, op: "delete", removes: true, dialog: deleteDialog(p, "Deleting a plan removes the derived DRClusters, DRPolicies and classes. Applications bound to it lose their protection.", true)}
  ],
  drpath: p => [
    {label: "Delete path", icon: "trash", danger: true, op: "delete", removes: true, dialog: deleteDialog(p, "Applications on this path lose the direction; a run along it becomes impossible until it is declared again.", p.counts.apps > 0)}
  ],
  papp: a => [
    {label: "Failover", icon: "shield", op: "failover", danger: true, dialog: runActionDialog(a, "Failover"), disabled: !a.paths.some(p => p.actions.includes("Failover")), hint: "No declared path allows Failover"},
    {label: "Relocate", icon: "move", op: "relocate", dialog: runActionDialog(a, "Relocate"), disabled: !a.paths.some(p => p.actions.includes("Relocate")), hint: "No declared path allows Relocate"},
    {label: "Restart in place", icon: "refresh", op: "restart", dialog: runActionDialog(a, "Restart")},
    {label: "Resume move", icon: "play", op: "relocate", dialog: runActionDialog(a, "Resume"), disabled: !a.move, hint: "No move is in progress"},
    {label: a.move ? `Revert to ${a.move.from}` : "Revert move", icon: "swap", op: "relocate", dialog: runActionDialog(a, "Revert"),
      disabled: !(a.move && a.move.revertible), hint: a.move ? (a.move.revertBlocked || "This move cannot be reverted") : "No move is in progress"},
    {label: "Test", icon: "camera", op: "test", dialog: runTestDialog(a), disabled: !a.paths.some(p => p.actions.includes("Test")), hint: "No declared path allows Test"},
    {label: "Schedule tests", icon: "clock", op: "create", dialog: newScheduleDialog(a), disabled: !a.paths.some(p => p.actions.includes("Test")), hint: "No declared path allows Test"},
    {label: "Restore from backup", icon: "cloud", op: "drrestore", dialog: restoreDialog(a)},
    {label: "Edit tiers & probes", icon: "list", op: "update", dialog: editTiersDialog(a)},
    {label: a.autoRestartOptOut ? "Enable automatic restart" : "Disable automatic restart", icon: "power", op: "update", run: () => drhub.setAutoRestart(a, a.autoRestartOptOut), toast: "Auto-restart preference saved"},
    {label: "Unprotect (delete)", icon: "trash", danger: true, op: "delete", removes: true, dialog: deleteDialog(a, "Removes the ProtectedApplication and the derived DRPlacementControl. The workload keeps running where it is; its volumes stop being replicated.")}
  ],
  rplan: p => [
    {label: "Failover plan", icon: "shield", op: "failover", danger: true, dialog: runActionDialog(p, "Failover")},
    {label: "Relocate plan", icon: "move", op: "relocate", dialog: runActionDialog(p, "Relocate")},
    {label: "Test plan", icon: "camera", op: "test", dialog: runTestDialog(p)},
    {label: "Schedule tests", icon: "clock", op: "create", dialog: newScheduleDialog(p)},
    {label: "Delete plan", icon: "trash", danger: true, op: "delete", removes: true, dialog: deleteDialog(p, "The applications stay protected individually.")}
  ],
  raction: a => [
    {label: "Delete record", icon: "trash", danger: true, op: "delete", removes: true, disabled: !a.terminal, hint: "A running action cannot be deleted", dialog: deleteDialog(a, "Finished runs are immutable and archived; deleting removes the hub's copy only.")}
  ],
  tbubble: t => [
    {label: "Abort — tear down", icon: "x", op: "update", danger: true, disabled: t.terminal || t.abort, hint: t.abort ? "Abort already requested" : "Already finished", run: () => drhub.abortTest(t), toast: "Abort requested; the bubble is torn down"},
    {label: "Change hold", icon: "clock", op: "update", disabled: t.terminal, hint: "Already finished", dialog: holdDialog(t)},
    {label: "Delete record", icon: "trash", danger: true, op: "delete", removes: true, disabled: !t.terminal, hint: "Abort the test first", dialog: deleteDialog(t, "Finished tests are immutable and archived; deleting removes the hub's copy only.")}
  ],
  tsched: s => [
    {label: s.suspend ? "Resume" : "Suspend", icon: s.suspend ? "power" : "pause", op: "update", run: () => drhub.suspendSchedule(s, !s.suspend), toast: s.suspend ? "Schedule resumed" : "Schedule suspended"},
    {label: "Delete schedule", icon: "trash", danger: true, op: "delete", removes: true, dialog: deleteDialog(s, "Existing test bubbles stay; only the schedule goes.")}
  ],
  restore: r => [
    {label: "Delete record", icon: "trash", danger: true, op: "delete", removes: true, disabled: !r.terminal, hint: "A running restore cannot be deleted", dialog: deleteDialog(r, "")}
  ],
  siteprofile: s => [
    {label: "Edit bindings", icon: "link", op: "update", dialog: editBindingsDialog(s)}
  ],
  dhcpserver: d => [
    {label: "Delete server", icon: "trash", danger: true, op: "delete", removes: true, dialog: deleteDialog(d, "Reservations rendered for this server stay in its ConfigMap until the hub re-renders the site; guests whose role names it become Open.")}
  ],
  drconfig: () => [],
  sitedeploy: d => [
    {label: "Size the draft", icon: "gauge", op: "update", dialog: resizeSiteDialog(d), disabled: d.approved, hint: "The deployment is approved"},
    {label: "Approve and deploy", icon: "check", op: "update", dialog: approveSiteDialog(d), disabled: d.approved || d.status !== "Drafted", hint: d.approved ? "Already approved" : "No draft with nodes to approve yet"},
    {label: "Delete request", icon: "trash", danger: true, op: "delete", removes: true, dialog: deleteDialog(d, "Deleting the request withdraws nothing on the site: the discovery, the draft and any storage cluster it produced stay.")}
  ]
});

// ---- tiles ------------------------------------------------------------------
function PPlanTile({o: p, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[p.status].c}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><TrafficLight status={p.status} /><Name>{p.name}</Name></>}
        right={<span className="badge">{p.sync ? "stretch · sync" : "cross-cluster"}</span>} />
      <div className="tsub" style={{marginTop: 2}}>{p.counts.sites} sites · {p.counts.methods} method{p.counts.methods === 1 ? "" : "s"} · {p.counts.paths} path{p.counts.paths === 1 ? "" : "s"}</div>
      <Uuid value={p.id} />
      <div className="labels">
        {p.sites.map(s => <span className="lab" key={s.name} title={`${s.cluster}${s.zone ? "/" + s.zone : ""}${s.region ? " · " + s.region : ""}`}>
          <Dot c={s.agentAvailable === false ? "var(--bad)" : s.agentAvailable ? "var(--ok)" : "var(--idle)"} />{s.name}<i>{s.zone ? `${s.cluster}/${s.zone}` : s.cluster}</i></span>)}
      </div>
      <div className="mlist">
        {p.methods.map(m => <div className="mrow" key={m.name}>
          <span className="badge">{m.type}</span><b>{m.name}</b><span className="spacer"></span>
          <span className="mono rpo">{isSync(m.type) ? "RPO 0" : m.interval ? "RPO " + m.interval : ""}{m.s3Backup ? ` · backup ${m.s3Backup.interval}×${m.s3Backup.retention}` : ""}</span>
        </div>)}
        {!p.methods.length && <div className="nolim">No method declared.</div>}
      </div>
      <Foot items={[
        {label: "Paths", count: p.counts.paths, icon: "swap", onClick: () => nav.layer(p, "paths")},
        {label: "Apps", icon: "cluster", onClick: () => nav.layer(p, "protectedapps")},
        {label: "Details", right: true, onClick: () => nav.detail(p)}
      ]} />
    </div>
  );
}
const isSync = t => /^sync/.test(t || "");

function DRPathTile({o: p, nav}) {
  const la = Object.values(p.lastActions || {});
  return (
    <div className="tile" style={{"--sc": STATUS_META[p.status].c}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><TrafficLight status={p.status} /><Name>{p.name}</Name></>} right={<span className="badge">{p.planName}</span>} />
      <div className="tsub" style={{marginTop: 2}}><PathArrow from={p.from} to={p.to} /></div>
      <Uuid value={p.id} />
      <div className="labels">
        {p.actions.map(a => <span className="lab" key={a}><KindBadge k={a} /></span>)}
        {p.announcementHandover && <span className="lab">announcement hand-over</span>}
        <span className={"lab" + (p.profileConsistency === "Inconsistent" ? " warn" : "")}><i>profiles</i>{p.profileConsistency}</span>
      </div>
      {!!la.length && <div className="kv">{la.map(a => <div key={a.kind}><span>last {a.kind}</span><b><TrafficLight status={a.phase} sm /> {a.completionTime ? fmtAgo(a.completionTime) : ""}</b></div>)}</div>}
      <Foot items={[
        {label: "Apps", count: p.counts.apps, icon: "cluster", onClick: () => nav.layer(p, "protectedapps")},
        {label: "Runs", icon: "clock", onClick: () => nav.layer(p, "ractions")},
        {label: "Details", right: true, onClick: () => nav.detail(p)}
      ]} />
    </div>
  );
}

function PAppTile({o: a, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[a.status].c}} onDoubleClick={() => nav.detail(a)}>
      <TileHead obj={a} left={<><TrafficLight status={a.status} /><Name>{a.name}</Name></>} right={<span className="badge">{a.appKind}</span>} />
      <div className="tsub" style={{marginTop: 2}}>{a.namespace} · plan {a.planName} · <PathArrow from={a.source} to={a.target} /></div>
      <Uuid value={a.id} />
      <div className="labels">
        <span className="lab"><i>now on</i>{a.currentCluster || "—"}</span>
        {a.method && <span className="lab"><i>method</i>{a.method}</span>}
        {a.protected === false && <span className="lab warn">not protected</span>}
        {a.awaitingRestore && <span className="lab warn">awaiting restore</span>}
        {a.zoneBinding && <span className="lab"><i>zone</i>{a.zoneBinding}</span>}
        {a.siteMapping === "Open" && <span className="lab warn"><i>site mapping</i>{a.counts.openFindings} open</span>}
        {a.siteMapping === "Resolved" && <span className="lab"><i>site mapping</i>resolved</span>}
      </div>
      <div className="mlist">
        {a.paths.map(p => <div className={"mrow" + (p.active && p.verdict === "NotReady" ? " bad" : "")} key={p.name}>
          {p.active ? <VerdictBadge v={p.verdict} sm /> : <span className="chip">inactive</span>}<b>{p.name}</b><span className="spacer"></span>
          <span className="mono">{p.actions.join(" · ")}</span>
        </div>)}
        {!a.paths.length && <div className="nolim" style={{padding: "6px 9px"}}>No declared path covers this application yet.</div>}
      </div>
      <Foot items={[
        {label: "Runs", icon: "clock", onClick: () => nav.layer(a, "ractions")},
        {label: "Tests", icon: "camera", onClick: () => nav.layer(a, "tests")},
        {label: "Details", right: true, onClick: () => nav.detail(a)}
      ]} />
    </div>
  );
}

function RPlanTile({o: p, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[p.status].c}} onDoubleClick={() => nav.detail(p)}>
      <TileHead obj={p} left={<><TrafficLight status={p.status} /><Name>{p.name}</Name></>} right={<span className="badge">recovery plan</span>} />
      <div className="tsub" style={{marginTop: 2}}>{p.namespace} · path {p.pathName}</div>
      <Uuid value={p.id} />
      <div className="labels">
        <span className="lab"><i>apps</i>{p.counts.apps}</span><span className="lab"><i>priorities</i>{p.counts.priorities}</span>
        <span className="lab"><i>gate</i>{(p.gates || {}).betweenPriorities || "allHealthy"}</span>
        {p.continueOnFailure && <span className="lab warn">continue on failure</span>}
      </div>
      <Foot items={[{label: "Runs", icon: "clock", onClick: () => nav.layer(p, "ractions")}, {label: "Details", right: true, onClick: () => nav.detail(p)}]} />
    </div>
  );
}

function RActionTile({o: a, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[a.status].c}} onDoubleClick={() => nav.detail(a)}>
      <TileHead obj={a} left={<><TrafficLight status={a.status} /><Name>{a.name}</Name></>} right={<KindBadge k={a.action} />} />
      <div className="tsub" style={{marginTop: 2}}>{a.planName ? `plan ${a.planName}` : `app ${a.appName}`} · {a.pathName || "in place"} · {a.namespace}</div>
      <Uuid value={a.id} />
      <div className="kv">
        <div><span>started</span><b>{a.startTime ? fmtAgo(a.startTime) : "—"}</b></div>
        <div><span>duration</span><b>{a.durationMs != null ? fmtSecs(a.durationMs / 1000) : "—"}</b></div>
        {a.sourceCluster && <div><span>move</span><b>{a.sourceCluster} → {a.targetCluster}</b></div>}
        {a.rtoSeconds != null && <div><span>RTO</span><b>{fmtSecs(a.rtoSeconds)}</b></div>}
        {a.override && <div><span>override</span><b style={{color: "var(--warn)"}}>yes</b></div>}
      </div>
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(a)}]} />
    </div>
  );
}

function TBubbleTile({o: t, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[t.status].c}} onDoubleClick={() => nav.detail(t)}>
      <TileHead obj={t} left={<><TrafficLight status={t.status} /><Name>{t.name}</Name></>} right={<KindBadge k="Test" />} />
      <div className="tsub" style={{marginTop: 2}}>{t.planName ? `plan ${t.planName}` : `app ${t.appName}`} · {t.pathName} · {t.namespace}</div>
      <Uuid value={t.id} />
      <div className="kv">
        <div><span>started</span><b>{t.startTime ? fmtAgo(t.startTime) : "—"}</b></div>
        <div><span>duration</span><b>{t.durationMs != null ? fmtSecs(t.durationMs / 1000) : "—"}</b></div>
        {t.report && t.report.achievedRPOSeconds != null && <div><span>RPO</span><b>{fmtSecs(t.report.achievedRPOSeconds)}</b></div>}
        {t.report && t.report.estimatedRTOSeconds != null && <div><span>est. RTO</span><b>{fmtSecs(t.report.estimatedRTOSeconds)}</b></div>}
        {t.scheduleName && <div><span>schedule</span><b>{t.scheduleName}</b></div>}
      </div>
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(t)}]} />
    </div>
  );
}

function TSchedTile({o: s, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[s.status].c}} onDoubleClick={() => nav.detail(s)}>
      <TileHead obj={s} left={<><TrafficLight status={s.status} /><Name>{s.name}</Name></>} right={<span className="badge mono">{s.schedule}</span>} />
      <div className="tsub" style={{marginTop: 2}}>{s.planName ? `plan ${s.planName}` : `app ${s.appName}`} · {s.pathName} · {s.namespace}</div>
      <Uuid value={s.id} />
      <div className="kv">
        <div><span>last run</span><b>{s.lastScheduleTime ? fmtAgo(s.lastScheduleTime) : "never"}</b></div>
        <div><span>last success</span><b>{s.lastSuccessfulTime ? fmtAgo(s.lastSuccessfulTime) : "—"}</b></div>
        <div><span>keep</span><b>{(s.retention || {}).keepLast || 10}{(s.retention || {}).keepFor ? ` · ${s.retention.keepFor}` : ""}</b></div>
        {s.active && <div><span>running</span><b>{s.active}</b></div>}
      </div>
      <Foot items={[{label: "Tests", icon: "camera", onClick: () => nav.layer(s, "tests")}, {label: "Details", right: true, onClick: () => nav.detail(s)}]} />
    </div>
  );
}

function RestoreTile({o: r, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[r.status].c}} onDoubleClick={() => nav.detail(r)}>
      <TileHead obj={r} left={<><TrafficLight status={r.status} /><Name>{r.name}</Name></>} right={<span className="badge">restore</span>} />
      <div className="tsub" style={{marginTop: 2}}>app {r.appName} · {r.cluster || "—"} · {r.namespace}</div>
      <Uuid value={r.id} />
      <div className="kv">
        <div><span>restore point</span><b>{r.restorePoint ? fmtDate(r.restorePoint) : "—"}</b></div>
        <div><span>data loss</span><b style={r.dataLoss ? {color: "var(--warn)"} : null}>{r.dataLoss || "—"}</b></div>
        <div><span>volumes</span><b>{r.counts.volumes}</b></div>
      </div>
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(r)}]} />
    </div>
  );
}

function SiteProfileTile({o: s, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[s.status].c}} onDoubleClick={() => nav.detail(s)}>
      <TileHead obj={s} left={<><TrafficLight status={s.status} /><Name>{s.name}</Name></>} right={<span className="badge k8s">managed cluster</span>} />
      <div className="tsub" style={{marginTop: 2}}>{s.reportedAt ? `inventory ${fmtAgo(s.reportedAt)}` : "no inventory reported yet"}</div>
      <Uuid value={s.id} />
      <div className="labels">{s.zones.map(z => <span className="lab" key={z}><Icon n="zone" s={10} />{z}</span>)}</div>
      <div className="kv">
        <div><span>nodes ready</span><b>{s.counts.nodesReady}/{s.counts.nodes}</b></div>
        <div><span>storage classes</span><b>{s.counts.storageClasses}</b></div>
        <div><span>NADs</span><b>{s.counts.nads}</b></div>
        <div><span>address pools</span><b>{s.ipAddressPools.length}</b></div>
      </div>
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(s)}]} />
    </div>
  );
}

function DHCPServerTile({o: d, nav}) {
  return (
    <div className="tile" style={{"--sc": STATUS_META[d.status].c}} onDoubleClick={() => nav.detail(d)}>
      <TileHead obj={d} left={<><TrafficLight status={d.status} /><Name>{d.name}</Name></>} right={<span className="badge">{d.type}</span>} />
      <div className="tsub" style={{marginTop: 2}}>site {d.site} · {d.target}</div>
      <Uuid value={d.id} />
      <div className="kv">
        <div><span>reservations</span><b>{d.reservations}</b></div>
        <div><span>generation</span><b>{d.generation || "—"}</b></div>
      </div>
      <Foot items={[{label: "Details", right: true, onClick: () => nav.detail(d)}]} />
    </div>
  );
}

function SiteDeployTile({o: d, nav}) {
  const sc = d.storageCluster;
  return (
    <div className="tile" style={{"--sc": STATUS_META[d.status].c}} onDoubleClick={() => nav.detail(d)}>
      <TileHead obj={d} left={<><TrafficLight status={d.status} /><Name>{d.site}</Name></>} right={<span className="badge">{d.approved ? "approved" : "draft"}</span>} />
      <div className="tsub" style={{marginTop: 2}}>{d.message || "—"}</div>
      <Uuid value={d.id} />
      <div className="kv">
        <div><span>nodes found</span><b>{d.counts.nodes}</b></div>
        <div><span>storage cluster</span><b>{sc ? sc.name : "—"}</b></div>
        <div><span>storage nodes</span><b>{sc ? d.counts.storageNodes : "—"}</b></div>
        <div><span>cluster id</span><b className="mono">{sc && sc.uuid ? sc.uuid.slice(0, 8) : "—"}</b></div>
      </div>
      {d.status === "Drafted" && !d.approved && <div className="prepbox">Review the draft and approve it. Nothing has been applied to any node yet.</div>}
      <Foot items={[
        d.status === "Drafted" && !d.approved ? {label: "Approve", icon: "check", onClick: () => window.__ui.dialog(approveSiteDialog(d), d)} : null,
        {label: "Details", right: true, onClick: () => nav.detail(d)}
      ]} />
    </div>
  );
}
function SiteDeployDetail({o: d, nav}) {
  const t = (d.draft && d.draft.cluster) || {};
  const z = d.sizing || {};
  const sc = d.storageCluster;
  const str = v => v == null ? "" : String(v);
  return (
    <div>
      <DetailHead obj={d} title={`Storage of ${d.site}`} sub={<span className="mono" style={{color: "var(--dim)"}}>StorageSiteDeployment {d.namespace}/{d.name} · draft {d.siteNamespace}/{d.draftName} on the site</span>} badge={<span className="badge">{d.approved ? "approved" : "not approved"}</span>} />
      {d.status === "Failed" && <div className="banner"><Icon n="alert" s={15} /><span><b>The deployment failed.</b> {d.message}</span></div>}
      <div className="stats">
        <Stat k="State" v={<TrafficLight status={d.status} />} s={d.message} />
        <Stat k="Nodes in the draft" v={d.counts.nodes} s={d.discover.enableControlPlaneNodes ? "control-plane nodes included" : ""} />
        <Stat k="Draft" v={(d.draft && d.draft.phase) || "—"} s={d.draft && d.draft.message ? d.draft.message : ""} />
        <Stat k="Storage cluster" v={sc ? sc.name : "—"} s={sc ? `${sc.phase || "not reported"}${sc.uuid ? " · " + sc.uuid : ""}` : "after the approval"} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Draft — what the discovery found</h3><div className="bd" style={{overflowX: "auto"}}>
          <Table cols={["Node set", "Group", "Nodes"]} empty="The site has not written a draft with nodes yet." rows={d.nodeSets.flatMap(s => (s.groups || []).map((g, i) => [<Mono>{s.name}</Mono>, <Mono dim>{g.name || `#${i + 1}`}</Mono>, <Mono>{(g.workers || []).join(", ")}</Mono>]))} />
        </div></div>
        <div className="card"><h3>Cluster template on the site</h3><div className="bd">
          <Table cols={["Field", "On the site", "Requested"]} empty="No draft yet." rows={d.draft ? [
            ["name", t.name, z.name], ["vCPUs per node", t.vcpuCount, z.vcpuCount],
            ["hugepages per node", t.minHugePagesSize, z.minHugePagesSize], ["subsystems per node", t.maxSubsystemCount, z.maxSubsystemCount],
            ["stripe", t.stripe ? `${str(t.stripe.dataChunks)}+${str(t.stripe.parityChunks)}` : "", z.stripe ? `${str(z.stripe.dataChunks)}+${str(z.stripe.parityChunks)}` : ""],
            ["format devices", t.enableDriveFormat, z.enableDriveFormat]
          ].map(r => [<b>{r[0]}</b>, <Mono>{str(r[1])}</Mono>, <Mono dim>{str(r[2])}</Mono>]) : []} />
        </div></div>
      </div>
      {sc && <div className="card"><h3>Storage nodes</h3><div className="bd">
        <Table cols={["Storage node", "Kubernetes node", "Phase"]} empty="No storage node reported yet." rows={(sc.nodes || []).map(n => [<Mono>{n.name}</Mono>, <Mono dim>{n.hostname}</Mono>, <Mono>{n.phase || "—"}</Mono>])} />
        <p className="mdesc" style={{margin: "9px 0 0"}}>A StorageClass on the site names this cluster as cluster_id {sc.uuid || "(not assigned yet)"} and pool {sc.pool || "—"}.</p>
      </div></div>}
      <Conditions o={d} />
    </div>
  );
}

// ---- site mapping (ADR 0020) ------------------------------------------------------
const MappingResult = ({r}) => <TrafficLight status={r || "Unknown"} sm />;
function FindingsTable({findings, nav}) {
  const rows = findings.flatMap(f => (f.paths && f.paths.length ? f.paths : [null]).map(p => [
    <Mono>{f.vm}</Mono>, <Mono dim>{f.network} <span style={{color: "var(--dim2)"}}>#{f.index}</span></Mono>, <span className="badge">{f.category}</span>, <Mono>{f.value}</Mono>,
    p ? <Mono dim>{p.path}</Mono> : "", p ? <MappingResult r={p.result} /> : <span style={{color: "var(--dim2)"}}>no path</span>,
    p ? <Mono dim>{p.strategy}{p.role ? ` · role ${p.role}` : ""}</Mono> : "", p ? <Mono>{p.to}</Mono> : "",
    p ? <span>{p.reason}{p.candidates && p.candidates.length ? <span style={{color: "var(--dim2)"}}> · candidates: {p.candidates.join(", ")}</span> : null}</span> : ""]));
  return <Table cols={["VM", "Network", "Category", "Source value", "Path", "Result", "Strategy", "Target value", "Reason / candidates"]} empty="No site-specific reference found." rows={rows} />;
}
function GuestsTable({guests}) {
  const rows = guests.flatMap(g => (g.reservations && g.reservations.length ? g.reservations : [null]).map((r, i) => [
    i === 0 ? <Mono>{g.vm}</Mono> : "", i === 0 ? <Mono dim>{g.network}</Mono> : "", i === 0 ? <Mono dim>{g.mac || <span style={{color: "var(--bad)"}}>unpinned</span>}</Mono> : "", i === 0 ? <span className="badge">{g.role}</span> : "",
    r ? <Mono>{r.site}{r.path ? <span style={{color: "var(--dim2)"}}> ← {r.path}</span> : <span style={{color: "var(--dim2)"}}> (current)</span>}</Mono> : <Mono dim>{g.currentSite}</Mono>,
    r ? <Mono>{r.ip}</Mono> : <Mono dim>{Object.entries(g.ips || {}).map(([s, ip]) => `${s}: ${ip}`).join(", ")}</Mono>, r ? <Mono dim>{r.dhcpServer}</Mono> : "",
    <MappingResult r={r ? r.result : g.result} />, r ? r.reason : g.reason]));
  return <Table cols={["VM", "Network", "MAC", "Role", "Site", "Address", "DHCP server", "Result", "Reason"]} empty="No guest interface on a guest network." rows={rows} />;
}
const Renderings = ({r}) => <Table cols={["Artifact (ConfigMap)", "Generation"]} empty="Nothing rendered for this site yet." rows={Object.entries(r || {}).sort().map(([k, v]) => [<Mono>{k}</Mono>, <Mono dim>{v}</Mono>])} />;
function MappingPanel({a}) {
  const m = a.mapping;
  return (
    <div style={{marginTop: 10}}>
      {a.siteMapping === "Open" && <div className="banner" style={{color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))"}}><Icon n="alert" s={15} />
        <span><b>{m ? m.counts.open : 0} open on at least one path</b> — readiness fails findings-resolved there. The fix is a decision on the target's SiteProfile (bind the logical-network role to a NAD, add a guest network with a DHCP server), a pinned MAC on the VM, or a DHCPServer object; not an override.</span></div>}
      {a.siteMapping === "NotApplicable" && <div className="nolim">No VM on a multus network: nothing to map; the application recovers exactly as captured.</div>}
      {a.siteMapping === "Unknown" && <div className="nolim">The current site's dr-agent has not reported the VMs yet.</div>}
      {m && <>
        <div className="stats" style={{marginTop: 10}}>
          <Stat k="Verdict" v={<TrafficLight status={a.siteMapping} />} s={m.site ? `discovered on ${m.site}` : ""} />
          <Stat k="Findings" v={m.findings.length} s={`${m.counts.resolved} resolved · ${m.counts.open} open (per path)`} c={m.counts.open ? "var(--warn)" : null} />
          <Stat k="Guest interfaces" v={m.guests.length} s={`${m.guests.filter(g => g.result === "Open").length} open`} />
          <Stat k="Renderings" v={Object.keys(a.renderings).length} s="artifacts delivered per site" />
        </div>
        <div className="card" style={{marginTop: 10}}><h3>Findings · VM networks (NAD) per declared path</h3><div className="bd" style={{overflowX: "auto"}}><FindingsTable findings={m.findings} /></div></div>
        <div className="card" style={{marginTop: 10}}><h3>Guest addresses · DHCP reservations</h3><div className="bd" style={{overflowX: "auto"}}><GuestsTable guests={m.guests} /></div>
          <div className="bd" style={{paddingTop: 0}}><p className="mdesc" style={{margin: 0}}>The target address keeps the host ID of the current one inside the target role's CIDR. A reservation is written to the role's DHCP server on every site a declared path leads to; after a move the RecoveryAction report compares expected and observed addresses.</p></div></div>
        <div className="card" style={{marginTop: 10}}><h3>Renderings</h3><div className="bd"><Renderings r={a.renderings} /></div></div>
      </>}
    </div>
  );
}

// ---- details ------------------------------------------------------------------
function PPlanDetail({o: p, nav}) {
  const paths = useResource("pplan.paths|" + p.id, () => drhub.planPaths(p.id), 10000);
  const apps = useResource("pplan.apps|" + p.id, () => drhub.planApps(p.id), 10000);
  const ps = paths.data || [], as = apps.data || [];
  const sp = p.storageProfile || {};
  return (
    <div>
      <DetailHead obj={p} title={p.name} sub={<span className="mono" style={{color: "var(--dim)"}}>ProtectionPlan · {p.sync ? "stretch cluster (sync)" : "cross-cluster"}</span>} badge={<span className="badge">plan</span>} />
      {p.status === "NotReady" && <div className="banner"><Icon n="alert" s={15} /><span><b>The plan is not ready.</b> {(p.conditions.find(c => c.type === "Ready") || {}).message || "See the conditions below."}</span></div>}
      {(() => { const s3c = p.conditions.find(c => c.type === "S3ProfileResolved"); return s3c && s3c.status === "False" &&
        <div className="banner"><Icon n="alert" s={15} /><span><b>{s3c.reason === "S3StoreRejected" ? "An S3 store refused dr-hub's probe." : "The S3 stores are not ready."}</b> {s3c.message}</span></div>; })()}
      <div className="stats">
        <Stat k="Sites" v={p.counts.sites} s={`${p.counts.agentsAvailable} with dr-agent available`} c={p.counts.agentsAvailable < p.counts.sites ? "var(--warn)" : null} />
        <Stat k="Methods" v={p.counts.methods} s={p.methods.map(m => m.type).join(", ")} />
        <Stat k="DR paths" v={ps.length} s={`${ps.filter(x => x.status === "Valid").length} valid`} />
        <Stat k="Applications" v={as.length} s={`${as.filter(a => a.verdict === "Ready").length} ready · ${as.filter(a => a.verdict === "NotReady").length} not ready`} c={as.some(a => a.verdict === "NotReady") ? "var(--bad)" : null} />
        <Stat k="Derived DRPolicies" v={p.drPolicies.length} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="swap" title="DR paths" sub="declared directions between the sites" count={ps.length} onClick={() => nav.layer(p, "paths")} />
        <NavCard icon="cluster" title="Applications" sub="protected by this plan" count={as.length} onClick={() => nav.layer(p, "protectedapps")} />
      </div>
      <div className="sech"><h2>Sites</h2><span className="ln"></span></div>
      <div className="card"><div className="bd">
        <Table cols={["Site", "Cluster", "Zone", "Region", "DRCluster", "Classes applied", "dr-agent"]} rows={p.sites.map(s => [<b>{s.name}</b>, <Mono>{s.cluster}</Mono>, <Mono>{s.zone}</Mono>, <Mono>{s.region}</Mono>,
          <Mono dim>{s.drCluster}</Mono>, s.classesApplied ? <span style={{color: "var(--ok)"}}>yes</span> : <span style={{color: "var(--dim)"}}>pending</span>,
          s.agentAvailable === true ? <span style={{color: "var(--ok)"}}>available</span> : s.agentAvailable === false ? <span style={{color: "var(--bad)"}}>unavailable</span> : <span style={{color: "var(--dim)"}}>unknown</span>])} />
        <p className="mdesc" style={{margin: "9px 0 0"}}>{p.sync ? "A sync plan is a stretch cluster: all sites name the same Kubernetes cluster split by zone. No Ramen DRCluster or DRPolicy is derived; dr-agent moves applications between zones and the achieved RPO is 0."
          : "Each site is an OCM ManagedCluster. The hub derives one DRCluster per site and one DRPolicy per pair a DR path uses; Ramen's peerClasses resolve from the storage IDs the hub stamps on the classes."}</p>
      </div></div>
      <div className="dcols">
        <div className="card"><h3>Methods</h3><div className="bd">
          <Table cols={["Method", "Type", "Interval", "S3 backup", "Parameters"]} rows={p.methods.map(m => [<b>{m.name}</b>, <span className="badge">{m.type}</span>, <Mono>{m.interval}</Mono>,
            m.s3Backup ? <Mono>{m.s3Backup.interval} × {m.s3Backup.retention}</Mono> : "", Object.keys(m.parameters).length ? <div className="labels" style={{marginTop: 0}}>{Object.entries(m.parameters).map(([k, v]) => <span className="lab" key={k}>{k}={v}</span>)}</div> : ""])} />
        </div></div>
        <div className="card"><h3>Storage profile</h3><div className="bd">
          <Props rows={[
            ["Storage class selector", sp.storageClassSelector && sp.storageClassSelector.matchLabels ? Object.entries(sp.storageClassSelector.matchLabels).map(([k, v]) => `${k}=${v}`).join(", ") : (sp.storageClassSelector ? JSON.stringify(sp.storageClassSelector) : "")],
            ["Snapshot class selector", sp.volumeSnapshotClassSelector ? JSON.stringify(sp.volumeSnapshotClassSelector.matchLabels || sp.volumeSnapshotClassSelector) : ""],
            ["Consistency groups", sp.consistencyGroups || "Disabled"],
            ["Group storage class selector", sp.groupStorageClassSelector ? JSON.stringify(sp.groupStorageClassSelector.matchLabels || sp.groupStorageClassSelector) : ""],
            ["Velero namespace", p.veleroNamespace],
            ["Auto-restart after storage recovery", p.autoRestart ? `${p.autoRestart.enabled ? "enabled" : "disabled"}${p.autoRestart.stableFor ? " · stable for " + p.autoRestart.stableFor : ""}` : "off"]
          ]} />
        </div></div>
      </div>
      <div className="dcols">
        <div className="card"><h3>S3 stores</h3><div className="bd">
          {p.s3Profile ? <Props rows={[["Ramen S3 profile", <Mono>{p.s3Profile}</Mono>]]} />
            : <Table cols={["Site", "Bucket", "Endpoint", "Region", "Secret", "Probe"]} empty="No S3 store declared — backup methods need one per site." rows={p.s3Profiles.map(s => {
                const pr = p.s3Stores[s.site];
                const probe = !pr ? <span style={{color: "var(--dim2)"}}>not probed yet</span>
                  : pr.ok ? <span style={{color: "var(--ok)"}} title={pr.checkedAt ? "checked " + pr.checkedAt : ""}>accepts list, write, delete</span>
                  : <span style={{color: "var(--bad)"}} title={pr.checkedAt ? "checked " + pr.checkedAt : ""}><b>{pr.code}</b>{pr.step ? ` on ${pr.step}` : ""}{pr.message ? ": " + pr.message : ""}</span>;
                return [<b>{s.site}</b>, <Mono>{s.bucket}</Mono>, <Mono>{s.endpoint}</Mono>, <Mono>{s.region}</Mono>, <Mono dim>{refName2(s.secretRef)}</Mono>, probe];
              })} />}
        </div></div>
        <div className="card"><h3>Derived pairs</h3><div className="bd">
          <Table cols={["Sites", "Paths", "DRPolicies", "peerClasses"]} empty="No pair derived yet — a pair exists once a DR path uses it." rows={p.pairs.map((x, i) => [<Mono>{x.sites.join(" ↔ ")}</Mono>, <Mono>{x.paths.join(", ")}</Mono>, <Mono dim>{x.drPolicies.join(", ")}</Mono>,
            x.peerClassesResolved ? <span style={{color: "var(--ok)"}}>resolved</span> : <span style={{color: "var(--warn)"}}>unresolved</span>])} />
        </div></div>
      </div>
      <Conditions o={p} />
    </div>
  );
}
const refName2 = r => (r && r.name) || "";

function DRPathDetail({o: p, nav}) {
  const apps = useResource("drpath.apps|" + p.id, () => drhub.pathApps(p.id), 10000);
  const runs = useResource("drpath.runs|" + p.id, () => Promise.all([drhub.pathActions(p.id), drhub.pathTests(p.id)]), 10000);
  const as = apps.data || [];
  const [actions, tests] = runs.data || [[], []];
  return (
    <div>
      <DetailHead obj={p} title={p.name} sub={<PathArrow from={p.from} to={p.to} />} badge={<><span className="badge">DRPath</span><span className="badge">{p.planName}</span></>} />
      {p.status === "Invalid" && <div className="banner"><Icon n="alert" s={15} /><span><b>Invalid path.</b> {(p.conditions.find(c => c.type === "Valid") || {}).message}</span></div>}
      {p.profileConsistency === "Inconsistent" && <div className="banner" style={{color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))"}}><Icon n="alert" s={15} /><span><b>The two site profiles differ.</b> Readiness reports profile-consistent as advisory; the comparison below names the fields.</span></div>}
      <div className="stats">
        <Stat k="Actions" v={p.actions.join(" · ")} />
        <Stat k="Applications" v={as.length} s={`${as.filter(a => (a.paths.find(x => x.name === p.name) || {}).verdict === "Ready").length} ready on this path`} />
        <Stat k="Runs" v={actions.length + tests.length} s={`${actions.filter(a => !a.terminal).length + tests.filter(t => !t.terminal).length} running`} />
        <Stat k="Profiles" v={p.profileConsistency} c={p.profileConsistency === "Inconsistent" ? "var(--warn)" : null} />
        <Stat k="DRPolicies" v={p.drPolicies.length} s={p.drPolicies.join(", ")} />
      </div>
      <div className="sech"><h2>Drill down</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="cluster" title="Applications" sub="on this path" count={as.length} onClick={() => nav.layer(p, "protectedapps")} />
        <NavCard icon="clock" title="Recovery actions" sub="Failover and Relocate along it" count={actions.length} onClick={() => nav.layer(p, "ractions")} />
        <NavCard icon="camera" title="Tests" sub="rehearsals along it" count={tests.length} onClick={() => nav.layer(p, "tests")} />
        <NavCard icon="list" title="Recovery plans" sub="ordered sets on this path" count="→" onClick={() => nav.layer(p, "rplans")} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Declaration</h3><div className="bd">
          <Props rows={[["From", <Mono>{p.from}</Mono>], ["To", <Mono>{p.to}</Mono>], ["Plan", <Ref label={p.planName} onClick={() => nav.openPlanByName(p.planName)} />],
            ["Actions", p.actions.join(", ")], ["Announcement hand-over", p.announcementHandover ? "yes" : "no"],
            p.test && ["Test mode", p.test.mode], p.test && ["Isolated NAD", <Mono>{p.test.isolatedNad}</Mono>],
            p.test && ["Max clone capacity", <Mono>{(p.test.quotas || {}).maxCloneCapacity}</Mono>], p.test && ["test-recent window", <Mono>{p.test.recentWithin || "720h"}</Mono>]]} />
          <p className="mdesc" style={{margin: "9px 0 0"}}>Directions are declared, not inferred. From, to and the plan are immutable; the actions, hand-over and test block can change.</p>
        </div></div>
        <div className="card"><h3>Last actions</h3><div className="bd">
          <Table cols={["Kind", "Run", "Phase", "Completed"]} empty="Nothing has run along this path yet." rows={Object.values(p.lastActions).map(a => [<KindBadge k={a.kind} />, <Mono>{a.ref}</Mono>, <TrafficLight status={a.phase} sm />, a.completionTime ? fmtDate(a.completionTime) : "—"])} />
        </div></div>
      </div>
      <div className="sech"><h2>Readiness of applications on this path</h2><span className="ln"></span></div>
      <div className="card"><div className="bd">
        <Table cols={["Application", "Verdict", "Blocking failures", "Advisory"]} empty="No application is on this path." rows={as.map(a => { const x = a.paths.find(y => y.name === p.name) || {checks: []}; return [
          <Ref label={`${a.namespace}/${a.name}`} onClick={() => nav.detail(a)} />, <VerdictBadge v={x.verdict} sm />,
          <Mono dim>{x.checks.filter(c => c.blocking && c.status === "Fail").map(c => c.name).join(", ")}</Mono>, <Mono dim>{x.checks.filter(c => !c.blocking && c.status !== "Pass" && c.status !== "NotApplicable").map(c => c.name).join(", ")}</Mono>]; })} />
      </div></div>
      <div className="sech"><h2>Site profile comparison</h2><span className="ln"></span></div>
      <div className="card"><div className="bd">
        <Table cols={["Field", "Status", "Message"]} empty="Not compared yet." rows={p.profileComparison.map(r => [<Mono>{r.field}</Mono>, <TrafficLight status={r.status} sm />, r.message])} />
        <p className="mdesc" style={{margin: "9px 0 0"}}>Source profile left, target right in the design; here the hub's row verdicts. Inventory is rewritten on every scan and never edited — bindings on the site profile are what clears a mismatch.</p>
      </div></div>
      <Conditions o={p} />
    </div>
  );
}

function PAppDetail({o: a, nav}) {
  const runs = useResource("papp.runs|" + a.id, () => Promise.all([drhub.appActions(a.id), drhub.appTests(a.id), drhub.appRestores(a.id), drhub.appSchedules(a.id)]), 8000);
  const [actions, tests, restores, scheds] = runs.data || [[], [], [], []];
  const [tab, setTab] = useState("readiness");
  const running = [...actions, ...tests].filter(r => !r.terminal);
  return (
    <div>
      <DetailHead obj={a} title={a.name} sub={<><span className="mono" style={{color: "var(--dim)"}}>{a.namespace}</span><PathArrow from={a.source} to={a.target} /></>}
        badge={<><span className="badge">{a.appKind}</span>{a.method && <span className="badge">{a.method}</span>}{a.protected === false && <span className="badge" style={{color: "var(--bad)"}}>not protected</span>}</>} />
      {!!running.length && <div className="banner" style={{color: "var(--info)", borderColor: "color-mix(in srgb,var(--info) 35%,transparent)", background: "color-mix(in srgb,var(--info) 8%,var(--panel))"}}><Icon n="refresh" s={15} />
        <span><b>{running.length} run{running.length === 1 ? "" : "s"} in progress:</b> {running.map(r => <Ref key={r.id} label={`${r.action || "Test"} ${r.name}`} onClick={() => nav.detail(r)} />)}</span></div>}
      {a.move && <div className={"banner" + (a.move.phase === "Stuck" ? "" : " info")}><Icon n={a.move.phase === "Stuck" ? "alert" : "move"} s={15} /><span>
        <b>{a.move.action} {a.move.from} → {a.move.to} {a.move.phase === "Stuck" ? "is stuck" : "in progress"}{a.move.since ? ` since ${fmtAgo(a.move.since)}` : ""}.</b>
        {a.move.blocking ? <> Ramen reports: <Mono>{a.move.blocking}</Mono>.</> : a.move.progression ? ` Ramen: ${a.move.progression}.` : ""}
        {a.move.phase === "Stuck" && <> Use ⋮ → <b>Resume move</b> once its cause is fixed{a.move.revertible ? <>, or <b>Revert to {a.move.from}</b></> : <> — it cannot be reverted: {a.move.revertBlocked}</>}.</>}
      </span></div>}
      {!a.move && a.verdict === "NotReady" && <div className="banner"><Icon n="alert" s={15} /><span><b>Not ready on the path it can move along.</b> The run-action controls need an override with a reason on that path; the failing checks are listed under Readiness.</span></div>}
      {a.awaitingRestore && <div className="banner" style={{color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))"}}><Icon n="cloud" s={15} /><span><b>Awaiting restore.</b> The DR state was restored onto a rebuilt site; the volumes come back from the newest S3 capture when a dr-admin creates a RestoreAction.</span></div>}
      {a.siteMapping === "Open" && tab !== "mapping" && <div className="banner" style={{color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))"}}><Icon n="link" s={15} /><span><b>Site mapping open: {a.counts.openFindings} VM network or guest address cannot be carried to a target.</b> <Ref label="See the findings" onClick={() => setTab("mapping")} /></span></div>}
      <div className="stats">
        <Stat k="Readiness" v={<VerdictBadge v={a.verdict} />} s={`${a.paths.length} declared path${a.paths.length === 1 ? "" : "s"}`} />
        <Stat k="Currently on" v={a.currentCluster || "—"} s={a.zoneBinding ? `zone binding ${a.zoneBinding}` : a.drpc ? `DRPC ${a.drpc}` : ""} />
        <Stat k="Plan" v={a.planName} s={a.drPolicy ? `DRPolicy ${a.drPolicy}` : ""} />
        <Stat k="Last action" v={a.lastAction ? a.lastAction.name : "—"} />
        <Stat k="Site mapping" v={<TrafficLight status={a.siteMapping} />} s={a.mapping ? `${a.counts.openFindings} open · ${a.mapping.counts.resolved} resolved` : "VM networks and guest addresses"} />
        <Stat k="Runs" v={actions.length + tests.length} s={`${actions.length} actions · ${tests.length} tests`} />
      </div>
      <Tabs items={[{k: "readiness", label: "Readiness", icon: "shield", n: a.paths.length}, {k: "mapping", label: "Site mapping", icon: "link", n: a.counts.findings + a.counts.guests},
        {k: "runs", label: "Runs", icon: "clock", n: actions.length + tests.length},
        {k: "binding", label: "Binding & recipe", icon: "link"}, {k: "schedules", label: "Schedules & restores", icon: "camera", n: scheds.length + restores.length}]} active={tab} onChange={setTab} />
      {tab === "mapping" && <MappingPanel a={a} />}
      {tab === "readiness" && <>
        {a.paths.map(p => <div className="card" key={p.name} style={{marginTop: 10, opacity: p.active ? 1 : 0.7}}>
          <h3 style={{display: "flex", alignItems: "center", gap: 10}}><span>{p.name}</span><PathArrow from={p.from} to={p.to} />
            {p.active ? <VerdictBadge v={p.verdict} sm /> : <span className="chip" title="Readiness of this path counts once the application runs on its source site">inactive · runs on {a.currentCluster}</span>}
            <span className="spacer" style={{flex: 1}}></span><span style={{textTransform: "none", letterSpacing: 0}}>{p.actions.join(" · ")}{p.since ? ` · since ${fmtAgo(p.since)}` : ""}</span></h3>
          <div className="bd">{p.active ? <CheckTable checks={p.checks} />
            : <span style={{color: "var(--dim2)"}}>The application runs on {a.currentCluster}; this path starts at {p.from}. It becomes the path to act on after a move to {p.from} — its checks are evaluated then.</span>}</div>
        </div>)}
        {!a.paths.length && <div className="empty"><Icon n="swap" s={22} /><b>No declared path</b><span>Declare a DRPath between {a.source} and {a.target} on plan {a.planName}. Readiness is computed per declared path.</span></div>}
      </>}
      {tab === "runs" && <div className="card" style={{marginTop: 10}}><div className="bd"><RunList actions={actions} tests={tests} nav={nav} /></div></div>}
      {tab === "binding" && <div className="dcols">
        <div className="card"><h3>Binding</h3><div className="bd">
          <Props rows={[["Kind", a.appKind], ["Plan", <Ref label={a.planName} onClick={() => nav.openPlanByName(a.planName)} />], ["Source", a.source], ["Target", a.target], ["Method", a.method],
            a.managed && ["Placement", <Mono>{refName2(a.managed.placementRef)}</Mono>],
            a.discovered && ["Protected namespaces", <Mono>{(a.discovered.protectedNamespaces || []).join(", ")}</Mono>],
            ["PVC selector", <Mono>{JSON.stringify(((a.managed || a.discovered || {}).pvcSelector || {}).matchLabels || {})}</Mono>],
            a.drpcRef && ["Adopted DRPC", <Mono>{a.drpcRef}</Mono>],
            ["DRPlacementControl", <Mono dim>{a.drpc}</Mono>], ["Placement (status)", <Mono dim>{a.placement}</Mono>], ["DRPolicy", <Mono dim>{a.drPolicy}</Mono>],
            a.zoneBinding && ["Zone binding", <Mono dim>{a.zoneBinding}</Mono>], a.siteMapping && ["Site mapping", <Mono dim>{a.siteMapping}</Mono>],
            ["Depends on", a.dependsOn.join(", ")], ["Automatic restart", a.autoRestartOptOut ? "opted out" : "per plan"]]} />
        </div></div>
        <div>
          <div className="card"><h3>Recipe & tiers</h3><div className="bd">
            {a.recipe ? <Props rows={[["Recipe", <Mono>{a.recipe.namespace ? a.recipe.namespace + "/" : ""}{a.recipe.name}</Mono>], ["Origin", a.recipe.generated ? "generated from tiers by the hub" : "hand-written — shown, validated, never modified"], a.recipe.hash && ["Hash", <Mono dim>{a.recipe.hash}</Mono>]]} />
              : <div className="nolim">No Recipe bound yet.</div>}
            <div className="sech" style={{margin: "12px 0 8px"}}><h2>Tiers (boot order)</h2><span className="ln"></span></div>
            <Table cols={["#", "Tier", "Selector", "Ready when"]} empty="No tiers declared — the hub protects everything in one group." rows={a.tiers.map((t, i) => [i + 1, <b>{t.name}</b>,
              <Mono dim>{[(t.selector || {}).resourceTypes && t.selector.resourceTypes.join(","), (t.selector || {}).matchLabels && Object.entries(t.selector.matchLabels).map(([k, v]) => `${k}=${v}`).join(",")].filter(Boolean).join(" · ")}</Mono>,
              <Mono dim>{(t.ready || []).map(r => r.type).join(", ")}</Mono>])} />
            {!!a.suggestedTiers.length && <><div className="sech" style={{margin: "12px 0 8px"}}><h2>Suggested tiers</h2><span className="ln"></span></div>
              <div className="labels">{a.suggestedTiers.map((t, i) => <span className="lab" key={i}>{t.name}</span>)}</div>
              <p className="mdesc" style={{margin: "9px 0 0"}}>Suggestions from kind defaults, well-known labels and owner references. Confirming them on the object applies the dr.simplyblock.io/tier label.</p></>}
          </div></div>
          <div className="card" style={{marginTop: 10}}><h3>Health probes & external hooks</h3><div className="bd">
            <Table cols={["Probe", "Type", "Target"]} empty="No probes." rows={a.probes.map(p => [<b>{p.name}</b>, <span className="badge">{p.type}</span>, <Mono dim>{p.target || (p.selector ? JSON.stringify(p.selector) : "")}</Mono>])} />
            <div className="labels">{(a.externalHooks.preSource || []).map(h => <span className="lab" key={"pre" + h.name}><i>preSource</i>{h.name}</span>)}{(a.externalHooks.postTargetReady || []).map(h => <span className="lab" key={"post" + h.name}><i>postTargetReady</i>{h.name}</span>)}</div>
          </div></div>
        </div>
      </div>}
      {tab === "schedules" && <div className="dcols">
        <div className="card"><h3>Test schedules</h3><div className="bd">
          <Table cols={["Schedule", "Cron", "Path", "Status", "Last run"]} empty="No schedule — use Actions › Schedule tests." rows={scheds.map(s => [<Ref label={s.name} onClick={() => nav.detail(s)} />, <Mono>{s.schedule}</Mono>, <Mono dim>{s.pathName}</Mono>, <TrafficLight status={s.status} sm />, s.lastScheduleTime ? fmtAgo(s.lastScheduleTime) : "never"])} />
        </div></div>
        <div className="card"><h3>Restores</h3><div className="bd">
          <Table cols={["Restore", "Phase", "Cluster", "Restore point", "Data loss"]} empty="No restore has run." rows={restores.map(r => [<Ref label={r.name} onClick={() => nav.detail(r)} />, <TrafficLight status={r.status} sm />, <Mono>{r.cluster}</Mono>, r.restorePoint ? fmtDate(r.restorePoint) : "—", r.dataLoss])} />
        </div></div>
      </div>}
      <Conditions o={a} />
    </div>
  );
}

function RPlanDetail({o: p, nav}) {
  const runs = useResource("rplan.runs|" + p.id, () => Promise.all([drhub.rplanActions(p.id), drhub.rplanTests(p.id), drhub.apps()]), 8000);
  const [actions, tests, apps] = runs.data || [[], [], []];
  const byPrio = {};
  p.applications.forEach(a => { (byPrio[a.priority] = byPrio[a.priority] || []).push(a); });
  const appOf = n => apps.find(a => a.name === n && a.namespace === p.namespace);
  return (
    <div>
      <DetailHead obj={p} title={p.name} sub={<><span className="mono" style={{color: "var(--dim)"}}>{p.namespace}</span><span className="mono">path {p.pathName}</span></>} badge={<span className="badge">RecoveryPlan</span>} />
      <div className="stats">
        <Stat k="Readiness" v={<VerdictBadge v={p.verdict} />} s="aggregated over the applications" />
        <Stat k="Applications" v={p.counts.apps} s={`${p.counts.priorities} priorit${p.counts.priorities === 1 ? "y" : "ies"}`} />
        <Stat k="Gate" v={(p.gates || {}).betweenPriorities || "allHealthy"} s={p.continueOnFailure ? "continues on failure" : "stops on failure"} />
        <Stat k="Runs" v={actions.length + tests.length} />
      </div>
      <div className="sech"><h2>Order</h2><span className="ln"></span></div>
      <div className="card"><div className="bd">
        {Object.keys(byPrio).sort((a, b) => a - b).map(pr => <div key={pr} style={{marginBottom: 10}}>
          <div className="tsub" style={{marginBottom: 6}}>priority {pr} · {byPrio[pr].length} in parallel</div>
          <div className="labels" style={{marginTop: 0}}>{byPrio[pr].map(a => { const o = appOf(a.name); const v = o ? ((o.paths.find(x => x.name === p.pathName) || {}).verdict || o.verdict) : "Unknown";
            return <span className={"lab" + (v === "NotReady" ? " warn" : "") + (o ? " link" : "")} key={a.name} onClick={() => o && nav.detail(o)}><Dot c={(STATUS_META[v] || {}).c} />{a.name}{a.dependsOn.length ? <i>after {a.dependsOn.join(", ")}</i> : null}</span>; })}</div>
        </div>)}
      </div></div>
      <div className="sech"><h2>Plan readiness checks</h2><span className="ln"></span></div>
      <div className="card"><div className="bd"><CheckTable checks={p.checks} /></div></div>
      <div className="sech"><h2>Runs</h2><span className="ln"></span></div>
      <div className="card"><div className="bd"><RunList actions={actions} tests={tests} nav={nav} /></div></div>
      <Conditions o={p} />
    </div>
  );
}

const ACTION_PHASES = ["Pending", "PreFlight", "PreSource", "RamenHandoff", "TargetStarting", "Workflow", "PostTargetReady", "Confirming", "Completed"];
function RActionDetail({o: a, nav}) {
  const r = a.report || {};
  return (
    <div>
      <DetailHead obj={a} title={a.name} sub={<><span className="mono" style={{color: "var(--dim)"}}>{a.namespace}</span><span className="mono">{a.planName ? `plan ${a.planName}` : `application ${a.appName}`}{a.pathName ? ` · ${a.pathName}` : " · in place"}</span></>} badge={<KindBadge k={a.action} />} />
      {a.override && <div className="banner" style={{color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))"}}><Icon n="alert" s={15} /><span><b>Readiness override.</b> {a.override.reason}</span></div>}
      {a.status === "Failed" && <div className="banner"><Icon n="alert" s={15} /><span><b>The action failed.</b> {(a.steps.filter(s => s.result === "Failed").slice(-1)[0] || {}).message || "See the journal."}</span></div>}
      <PhaseStripDR phases={ACTION_PHASES} current={a.phase} terminal={a.terminal} />
      <div className="stats">
        <Stat k="Phase" v={<TrafficLight status={a.status} />} />
        <Stat k="Started" v={a.startTime ? fmtDate(a.startTime) : "—"} s={a.startTime ? fmtAgo(a.startTime) : ""} />
        <Stat k="Duration" v={a.durationMs != null ? fmtSecs(a.durationMs / 1000) : "—"} s={a.completionTime ? "completed " + fmtDate(a.completionTime) : a.terminal ? "" : "running"} />
        <Stat k="Move" v={a.sourceCluster ? `${a.sourceCluster} → ${a.targetCluster}` : "—"} />
        <Stat k="RTO" v={a.rtoSeconds != null ? fmtSecs(a.rtoSeconds) : "—"} s={a.rpoSeconds != null ? `achieved RPO ${fmtSecs(a.rpoSeconds)}` : ""} />
        <Stat k="Operator" v={a.createdBy || "—"} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Journal · {a.steps.length} steps</h3><div className="bd"><StepJournal steps={a.steps} /></div></div>
        <div>
          {!!a.children.length && <div className="card" style={{marginBottom: 10}}><h3>Applications in this plan run</h3><div className="bd">
            <Table cols={["Application", "Priority", "Action", "Phase", "Message"]} rows={a.children.map(c => [<b>{c.application}</b>, c.priority, <Mono dim>{c.action}</Mono>, <TrafficLight status={c.phase || "Pending"} sm />, c.message])} />
          </div></div>}
          <div className="card"><h3>Report</h3><div className="bd">
            {a.report ? <>
              <Props rows={[["Operator", r.operator], ["RTO", r.rtoSeconds != null ? fmtSecs(r.rtoSeconds) : ""], ["Achieved RPO", r.achievedRPOSeconds != null ? fmtSecs(r.achievedRPOSeconds) : ""],
                r.overrideReason && ["Override reason", r.overrideReason], ["Archived as", <Mono dim>{a.reportKey}</Mono>]]} />
              {!!(r.warnings || []).length && <><div className="sech" style={{margin: "12px 0 8px"}}><h2>Warnings</h2><span className="ln"></span></div><ul style={{margin: 0, paddingLeft: 18, fontSize: 12, color: "var(--warn)"}}>{r.warnings.map((w, i) => <li key={i}>{w}</li>)}</ul></>}
              {!!(r.probes || []).length && <><div className="sech" style={{margin: "12px 0 8px"}}><h2>Probes</h2><span className="ln"></span></div>
                <Table cols={["Probe", "Result", "Message", "Time"]} rows={r.probes.map(p => [<b>{p.name}</b>, p.passed ? <span style={{color: "var(--ok)"}}>passed</span> : <span style={{color: "var(--bad)"}}>failed</span>, p.message, p.time ? fmtDate(p.time) : ""])} /></>}
              {!!(r.hooks || []).length && <><div className="sech" style={{margin: "12px 0 8px"}}><h2>Hooks</h2><span className="ln"></span></div>
                <Table cols={["Point", "Hook", "Result", "Duration", "Message"]} rows={r.hooks.map(h => [<Mono dim>{h.point}</Mono>, <b>{h.name}</b>, <TrafficLight status={h.result} sm />, h.durationSeconds != null ? fmtSecs(h.durationSeconds) : "", h.message])} /></>}
              {r.preFlight && <><div className="sech" style={{margin: "12px 0 8px"}}><h2>Pre-flight · {r.preFlight.verdict}</h2><span className="ln"></span></div><CheckTable checks={r.preFlight.checks} /></>}
              {!!a.guests.length && <><div className="sech" style={{margin: "12px 0 8px"}}><h2>Guest addresses on the target</h2><span className="ln"></span></div>
                <Table cols={["VM", "Network", "Expected", "Observed", "Match"]} rows={a.guests.map(g => [<Mono>{g.vm}</Mono>, <Mono dim>{g.network}</Mono>, <Mono>{g.expectedIP}</Mono>, <Mono>{g.observedIP}</Mono>, g.match ? <span style={{color: "var(--ok)"}}>yes</span> : <span style={{color: "var(--bad)"}}>no</span>])} />
                {a.guests.some(g => !g.match) && <p className="mdesc" style={{margin: "8px 0 0"}}>A mismatch is reported, not fatal: the guest got an address other than its reservation. Check the DHCP server's ConfigMap generation on the site and the VM's pinned MAC.</p>}</>}
              {r.restart && <><div className="sech" style={{margin: "12px 0 8px"}}><h2>Storage recovery</h2><span className="ln"></span></div>
                <Props rows={[["Storage cluster", <Mono>{r.restart.storageCluster}</Mono>], ["Recovery", r.restart.recovery], ["Recovered at", r.restart.recoveredAt ? fmtDate(r.restart.recoveredAt) : ""],
                  ["Recovery → ready", r.restart.recoveryToReadySeconds != null ? fmtSecs(r.restart.recoveryToReadySeconds) : ""], ["Volumes reconnected", r.restart.volumesReconnected]]} /></>}
            </> : <div className="nolim">The report is written once the run finishes.</div>}
          </div></div>
        </div>
      </div>
      <Conditions o={a} />
    </div>
  );
}

const TEST_PHASES = ["Pending", "Cloning", "Provisioning", "Restoring", "Validating", "Holding", "TearingDown", "Completed"];
function TBubbleDetail({o: t, nav}) {
  const r = t.report || {};
  return (
    <div>
      <DetailHead obj={t} title={t.name} sub={<><span className="mono" style={{color: "var(--dim)"}}>{t.namespace}</span><span className="mono">{t.planName ? `plan ${t.planName}` : `application ${t.appName}`} · {t.pathName}</span></>}
        badge={<><KindBadge k="Test" />{t.scheduleName && <span className="badge">schedule {t.scheduleName}</span>}</>} />
      {t.abort && !t.terminal && <div className="banner" style={{color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))"}}><Icon n="alert" s={15} /><span><b>Abort requested.</b> The bubble is being torn down.</span></div>}
      {t.outcome === "FailedInvariant" && <div className="banner"><Icon n="alert" s={15} /><span><b>An invariant was violated:</b> the test touched production state. See the invariants below.</span></div>}
      <PhaseStripDR phases={TEST_PHASES} current={t.phase} terminal={t.terminal} />
      <div className="stats">
        <Stat k="Outcome" v={<TrafficLight status={t.status} />} s={t.testID ? `test ${t.testID}` : ""} />
        <Stat k="Started" v={t.startTime ? fmtDate(t.startTime) : "—"} />
        <Stat k="Duration" v={t.durationMs != null ? fmtSecs(t.durationMs / 1000) : "—"} s={t.clonesReadyTime ? `clones ready ${fmtAgo(t.clonesReadyTime)}` : ""} />
        <Stat k="Target" v={t.targetCluster || "—"} s={t.sourceCluster ? `from ${t.sourceCluster}` : ""} />
        <Stat k="Test point" v={r.testPoint ? fmtDate(r.testPoint) : "—"} s={r.achievedRPOSeconds != null ? `achieved RPO ${fmtSecs(r.achievedRPOSeconds)}` : ""} />
        <Stat k="Estimated RTO" v={r.estimatedRTOSeconds != null ? fmtSecs(r.estimatedRTOSeconds) : "—"} s={r.consistency || ""} />
      </div>
      <div className="dcols">
        <div>
          <div className="card"><h3>Applications</h3><div className="bd">
            <Table cols={["Application", "Priority", "Phase", "Ready", "Message"]} empty="Not started." rows={t.applications.map(a => [<b>{a.name}</b>, a.priority, <TrafficLight status={a.phase || "Pending"} sm />, a.readyTime ? fmtDate(a.readyTime) : "", a.message])} />
          </div></div>
          <div className="card" style={{marginTop: 10}}><h3>Journal</h3><div className="bd"><StepJournal steps={t.steps} /></div></div>
        </div>
        <div>
          <div className="card"><h3>Checks</h3><div className="bd">
            <Table cols={["Check", "Status", "Message"]} empty="No checks yet." rows={t.checks.map(c => [<Mono>{c.name}</Mono>, <TrafficLight status={c.status} sm />, c.message])} />
          </div></div>
          <div className="card" style={{marginTop: 10}}><h3>Invariants · production untouched</h3><div className="bd">
            <Table cols={["Object", "Field", "Before", "After"]} empty="Nothing compared yet." rows={t.invariants.map(i => [<Mono>{i.object}</Mono>, <Mono dim>{i.field}</Mono>, <Mono>{i.before}</Mono>, <Mono style={i.before !== i.after ? {color: "var(--bad)"} : null}>{i.after}</Mono>])} />
          </div></div>
          <div className="card" style={{marginTop: 10}}><h3>Report</h3><div className="bd">
            {t.report ? <>
              <Props rows={[["Operator", r.operator], ["Outcome", r.outcome], ["Consistency", r.consistency], ["Bubble namespaces", t.bubbleNamespaces.join(", ")], ["Clone source", t.cloneSource], ["Archived as", <Mono dim>{t.reportKey}</Mono>]]} />
              {r.coverage && <><div className="sech" style={{margin: "12px 0 8px"}}><h2>Coverage</h2><span className="ln"></span></div>
                <div className="labels">{(r.coverage.exercised || []).map(x => <span className="lab" key={"e" + x}><Dot c="var(--ok)" />{x}</span>)}{(r.coverage.notExercised || []).map(x => <span className="lab" key={"n" + x}><Dot c="var(--dim2)" />{x}</span>)}</div></>}
              {!!(r.warnings || []).length && <ul style={{margin: "10px 0 0", paddingLeft: 18, fontSize: 12, color: "var(--warn)"}}>{r.warnings.map((w, i) => <li key={i}>{w}</li>)}</ul>}
            </> : <Props rows={[["Clone source", t.cloneSource], ["Hold for", t.holdFor || "no hold"], ["Max lifetime", t.maxLifetime || "24h"], ["Bubble namespaces", t.bubbleNamespaces.join(", ")]]} />}
          </div></div>
        </div>
      </div>
      <Conditions o={t} />
    </div>
  );
}

function TSchedDetail({o: s, nav}) {
  const tests = useResource("tsched.tests|" + s.id, () => drhub.scheduleTests(s.id), 10000);
  const ts = tests.data || [];
  return (
    <div>
      <DetailHead obj={s} title={s.name} sub={<><span className="mono" style={{color: "var(--dim)"}}>{s.namespace}</span><span className="mono">{s.schedule} UTC</span></>} badge={<span className="badge">TestSchedule</span>} />
      <div className="stats">
        <Stat k="State" v={<TrafficLight status={s.status} />} />
        <Stat k="Last run" v={s.lastScheduleTime ? fmtDate(s.lastScheduleTime) : "never"} />
        <Stat k="Last success" v={s.lastSuccessfulTime ? fmtDate(s.lastSuccessfulTime) : "—"} />
        <Stat k="Tests kept" v={ts.length} s={`keep last ${(s.retention || {}).keepLast || 10}${(s.retention || {}).keepFor ? " · " + s.retention.keepFor : ""}`} />
        <Stat k="Passed" v={ts.filter(t => t.outcome === "Passed").length} s={`${ts.filter(t => t.outcome && t.outcome !== "Passed").length} not passed`} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Template</h3><div className="bd">
          <Props rows={[["Path", <Mono>{s.pathName}</Mono>], [s.planName ? "Recovery plan" : "Application", <Mono>{s.targetName}</Mono>], ["Clone source", s.template.cloneSource || "latest-replicated-snapshot"],
            ["Hold for", s.template.holdFor || "none"], ["Max lifetime", s.template.maxLifetime || "24h"], ["Active bubble", <Mono dim>{s.active}</Mono>]]} />
        </div></div>
        <div className="card"><h3>Tests from this schedule</h3><div className="bd">
          <Table cols={["Test", "Outcome", "Started", "Duration"]} empty="No test has run from this schedule yet." rows={ts.map(t => [<Ref label={t.name} onClick={() => nav.detail(t)} />, <TrafficLight status={t.status} sm />, t.startTime ? fmtDate(t.startTime) : "", t.durationMs != null ? fmtSecs(t.durationMs / 1000) : ""])} />
        </div></div>
      </div>
      <Conditions o={s} />
    </div>
  );
}

const RESTORE_PHASES = ["Pending", "Restoring", "Reprotecting", "Verifying", "Completed"];
function RestoreDetail({o: r, nav}) {
  return (
    <div>
      <DetailHead obj={r} title={r.name} sub={<><span className="mono" style={{color: "var(--dim)"}}>{r.namespace}</span><span className="mono">application {r.appName}</span></>} badge={<span className="badge">RestoreAction</span>} />
      {r.message && r.status === "Failed" && <div className="banner"><Icon n="alert" s={15} /><span>{r.message}</span></div>}
      <PhaseStripDR phases={RESTORE_PHASES} current={r.phase} terminal={r.terminal} />
      <div className="stats">
        <Stat k="Phase" v={<TrafficLight status={r.status} />} />
        <Stat k="Cluster" v={r.cluster || "—"} />
        <Stat k="Restore point" v={r.restorePoint ? fmtDate(r.restorePoint) : "—"} s={r.capture ? `capture #${r.capture.number} · ${r.capture.s3Profile}` : ""} />
        <Stat k="Data loss" v={r.dataLoss || "—"} c={r.dataLoss ? "var(--warn)" : null} />
        <Stat k="Duration" v={r.durationMs != null ? fmtSecs(r.durationMs / 1000) : "—"} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Volumes</h3><div className="bd"><Table cols={["PVC", "Point"]} empty="No volume listed yet." rows={r.volumes.map(v => [<Mono>{v.pvc}</Mono>, v.point ? fmtDate(v.point) : ""])} /></div></div>
        <div>
          <div className="card"><h3>Steps</h3><div className="bd"><StepJournal steps={r.steps} /></div></div>
          <div className="card" style={{marginTop: 10}}><h3>Checks</h3><div className="bd"><Table cols={["Check", "Status", "Message"]} empty="No checks yet." rows={r.checks.map(c => [<Mono>{c.name}</Mono>, <TrafficLight status={c.status} sm />, c.message])} /></div></div>
        </div>
      </div>
    </div>
  );
}

function SiteProfileDetail({o: s, nav}) {
  const paths = useResource("sprof.paths|" + s.id, () => drhub.siteProfilePaths(s.id), 15000);
  const dhcp = useResource("sprof.dhcp|" + s.id, () => drhub.siteDHCPServers(s.id), 15000);
  const inv = s.inventory, sp = s.spec || {};
  return (
    <div>
      <DetailHead obj={s} title={s.name} sub={<span className="mono" style={{color: "var(--dim)"}}>SiteProfile · cluster {inv.clusterID || s.name}</span>} badge={<span className="badge k8s">managed cluster</span>} />
      <div className="banner" style={{color: "var(--dim)", borderColor: "var(--line)", background: "var(--panel2)"}}><Icon n="refresh" s={15} /><span><b>Inventory is rewritten on every scan{s.reportedAt ? `, last ${fmtAgo(s.reportedAt)}` : ""}.</b> It is never edited; the bindings in the spec are what a dr-admin sets (Actions → Edit bindings).</span></div>
      {dhcp.data && missingServers(sp, dhcp.data).length > 0 && <div className="banner"><Icon n="alert" s={15} /><span><b>DHCP server {missingServers(sp, dhcp.data).join(", ")} is not registered for {s.name}.</b> The bindings name it, so no reservation is rendered for guests on those networks and a move here fails its guest-address check. Choose a registered server under Edit bindings, or register one with that name.</span></div>}
      <div className="stats">
        <Stat k="Nodes ready" v={`${s.counts.nodesReady}/${s.counts.nodes}`} c={s.counts.nodesReady < s.counts.nodes ? "var(--warn)" : null} />
        <Stat k="Zones" v={s.zones.length} s={s.zones.join(", ")} />
        <Stat k="Storage classes" v={s.storageClasses.length} s={`${s.snapshotClasses.length} snapshot classes`} />
        <Stat k="Networks" v={s.nads.length} s={`${s.ipAddressPools.length} address pools`} />
        <Stat k="On paths" v={(paths.data || []).length} />
        <Stat k="DHCP servers" v={(dhcp.data || []).length} s={`${(dhcp.data || []).reduce((n, d) => n + d.reservations, 0)} reservations rendered`} />
        <Stat k="Renderings" v={Object.keys(s.renderings).length} s="site-mapper artifacts" />
      </div>
      <div className="dcols">
        <div>
          <div className="card"><h3>Nodes</h3><div className="bd"><Table cols={["Node", "Zone", "Ready", "Pod CIDRs"]} empty="No nodes reported." rows={s.nodes.map(n => [<b>{n.name}</b>, <Mono>{n.zone}</Mono>, n.ready ? <span style={{color: "var(--ok)"}}>ready</span> : <span style={{color: "var(--bad)"}}>not ready</span>, <Mono dim>{(n.podCIDRs || []).join(", ")}</Mono>])} /></div></div>
          <div className="card" style={{marginTop: 10}}><h3>Storage</h3><div className="bd">
            <Table cols={["Storage class", "Driver"]} empty="None reported." rows={s.storageClasses.map(c => [<Mono>{c.name}</Mono>, <Mono dim>{c.driver}</Mono>])} />
            {!!s.snapshotClasses.length && <div className="labels">{s.snapshotClasses.map(c => <span className="lab" key={c.name}><i>snapshot class</i>{c.name}</span>)}</div>}
          </div></div>
        </div>
        <div>
          <div className="card"><h3>Networking</h3><div className="bd">
            <Table cols={["NAD", "Type", "Master / bridge", "VLAN", "IPAM"]} empty="No NetworkAttachmentDefinitions reported." rows={s.nads.map(n => [<Mono>{n.namespace}/{n.name}</Mono>, <Mono dim>{n.type}</Mono>, <Mono dim>{n.master || n.bridge}</Mono>, n.vlan, <Mono dim>{n.ipamType}{(n.ipamRanges || []).length ? " " + n.ipamRanges.join(",") : ""}</Mono>])} />
            <Props rows={[["Pod CIDRs", <Mono>{(inv.podCIDRs || []).join(", ")}</Mono>], ["Service CIDRs", <Mono>{(inv.serviceCIDRs || []).join(", ")}</Mono>], ["Ingress domains", <Mono>{(inv.ingressDomains || []).join(", ")}</Mono>], ["Base domain", <Mono>{inv.baseDomain}</Mono>],
              ["Ingress classes", <Mono>{(s.ingressClasses || []).map(c => c.name).join(", ")}</Mono>], ["Gateway classes", <Mono>{(s.gatewayClasses || []).map(c => c.name).join(", ")}</Mono>]]} />
            {!!s.ipAddressPools.length && <Table cols={["Address pool", "Addresses", "Auto-assign"]} rows={s.ipAddressPools.map(p => [<Mono>{p.ns ? p.ns + "/" : ""}{p.name}</Mono>, <Mono dim>{(p.addresses || []).join(", ")}</Mono>, p.autoAssign ? "yes" : "no"])} />}
            {!!s.registryMirrors.length && <Table cols={["Registry", "Mirrors"]} rows={s.registryMirrors.map(m => [<Mono>{m.source}</Mono>, <Mono dim>{(m.mirrors || []).join(", ")}</Mono>])} />}
          </div></div>
          <div className="card" style={{marginTop: 10}}><h3>Bindings (spec)</h3><div className="bd">
            <Props rows={[["Logical networks", (sp.logicalNetworks || []).map(l => `${l.role} → ${l.nad}`).join("; ")],
              ["Address pools", (sp.addressPools || []).map(a => `${a.role} → ${a.pool}`).join("; ")], ["Domains", sp.domains ? JSON.stringify(sp.domains) : ""], ["Registry mirror", sp.registryMirror], ["Site DHCP server", typeof sp.dhcpServerRef === "string" ? sp.dhcpServerRef : refName2(sp.dhcpServerRef)]]} />
            <div className="sech" style={{margin: "12px 0 8px"}}><h2>Guest networks</h2><span className="ln"></span></div>
            <Table cols={["Role", "CIDR", "Gateway", "Reserved host IDs", "DHCP server"]} empty="No guest network bound — guest addresses are not reserved on this site." rows={(sp.guestNetworks || []).map(g => [<span className="badge">{g.role}</span>, <Mono>{g.cidr}</Mono>, <Mono dim>{g.gateway}</Mono>, <Mono dim>{(g.reservedHostIDs || []).join(", ")}</Mono>, <Mono dim>{g.dhcpServerRef || (typeof sp.dhcpServerRef === "string" ? sp.dhcpServerRef + " (site default)" : "")}</Mono>])} />
            <p className="mdesc" style={{margin: "9px 0 0"}}>A role names the same logical network on every site; binding it here is what resolves a VM's NAD finding and derives its guest address on this site.</p>
          </div></div>
          <div className="card" style={{marginTop: 10}}><h3>DHCP servers & renderings</h3><div className="bd">
            <Table cols={["Server", "Type", "Target ConfigMap", "Reservations", "Generation"]} empty="No DHCPServer registered for this site." rows={(dhcp.data || []).map(d => [<Ref label={d.name} onClick={() => nav.detail(d)} />, <span className="badge">{d.type}</span>, <Mono dim>{d.target}</Mono>, d.reservations, <Mono dim>{d.generation}</Mono>])} />
            <div className="sech" style={{margin: "12px 0 8px"}}><h2>Rendered artifacts</h2><span className="ln"></span></div>
            <Renderings r={s.renderings} />
            <p className="mdesc" style={{margin: "9px 0 0"}}>sitemap-live (the Velero resource-modifier) and the DHCP ConfigMaps, delivered by ManifestWork; artifacts-current compares these generations with what dr-agent finds on the site.</p>
          </div></div>
        </div>
      </div>
      <div className="sech"><h2>DR paths touching this site</h2><span className="ln"></span></div>
      <div className="grid">{(paths.data || []).map(p => <DRPathTile key={p.id} o={p} nav={nav} />)}</div>
      <Conditions o={s} />
    </div>
  );
}

function DHCPServerDetail({o: d, nav}) {
  const apps = useResource("dhcp.apps|" + d.id, () => drhub.apps(), 15000);
  const guests = (apps.data || []).flatMap(a => (a.mapping ? a.mapping.guests : []).flatMap(g => (g.reservations || []).filter(r => r.dhcpServer === d.name).map(r => ({app: `${a.namespace}/${a.name}`, g, r}))));
  return (
    <div>
      <DetailHead obj={d} title={d.name} sub={<span className="mono" style={{color: "var(--dim)"}}>DHCPServer · site {d.site}</span>} badge={<span className="badge">{d.type}</span>} />
      <div className="stats">
        <Stat k="State" v={<TrafficLight status={d.status} />} />
        <Stat k="Reservations" v={d.reservations} s="rendered by dr-hub" />
        <Stat k="Generation" v={d.generation || "—"} s="content hash of the rendering" />
        <Stat k="Target" v={d.target} s="ConfigMap on the site, key sitemap.hosts" />
      </div>
      <div className="card"><h3>Reservations known from the applications</h3><div className="bd" style={{overflowX: "auto"}}>
        <Table cols={["Application", "VM", "Network", "MAC", "Address", "Site", "Result", "Reason"]} empty="No guest interface names this server yet." rows={guests.map(x => [<Mono dim>{x.app}</Mono>, <Mono>{x.g.vm}</Mono>, <Mono dim>{x.g.network}</Mono>, <Mono dim>{x.g.mac}</Mono>, <Mono>{x.r.ip}</Mono>, <Mono dim>{x.r.site}{x.r.path ? ` ← ${x.r.path}` : ""}</Mono>, <MappingResult r={x.r.result} />, x.r.reason])} />
        <p className="mdesc" style={{margin: "9px 0 0"}}>The ConfigMap is the API: one "mac,ip,name" line per reservation, dnsmasq --dhcp-hostsdir format. The hub never talks to the server.</p>
      </div></div>
      <Conditions o={d} />
    </div>
  );
}

function DRConfigView({nav}) {
  const {data: c, loading, error, reload} = useResource("drconfig", () => drhub.config(), 10000);
  if (error) return <div className="scroll"><div className="empty"><Icon n="alert" s={22} c="var(--bad)" /><b>Cannot read the DR configuration</b><span>{error.message}</span><button className="chip" onClick={reload}>Retry</button></div></div>;
  if (loading || !c) return <div className="scroll"><div className="stats">{Array.from({length: 4}).map((_, i) => <div className="skel" key={i} style={{height: 62}}></div>)}</div></div>;
  const ex = c.executor || {}, ag = c.agent || {}, rm = c.ramen || {}, ar = c.archive || {}, bs = c.bootstrap || {}, fg = c.featureGates || {};
  return (
    <div className="scroll">
      <DetailHead obj={c} title="DR configuration" sub={<span className="mono" style={{color: "var(--dim)"}}>DRConfig/{c.name}</span>} badge={<>{c.recoveryMode && <span className="badge" style={{color: "var(--warn)"}}>recovery mode — controllers held</span>}</>} />
      {c.ramenConfigured === false && <div className="banner"><Icon n="alert" s={15} /><span><b>Ramen is not configured by the hub.</b> {(c.conditions.find(x => x.type === "RamenConfigured") || {}).message}</span></div>}
      <div className="stats">
        <Stat k="Agents" v={`${c.counts.agentsAvailable}/${c.counts.agents}`} s="dr-agent available per cluster" c={c.counts.agentsAvailable < c.counts.agents ? "var(--warn)" : null} />
        <Stat k="Executor" v={ex.default || "inCluster"} s={ex.aap ? ex.aap.url : ""} />
        <Stat k="Ramen" v={c.ramenConfigured === true ? "configured" : c.ramenConfigured === false ? "not configured" : "—"} s={rm.managed ? "managed by dr-hub" : "external configuration"} />
        <Stat k="Archive" v={c.archive ? "S3" : "none"} s={c.archive ? `${ar.bucket} · ${ar.prefix || "dr/"}` : "runs are never pruned without an archive"} />
        <Stat k="Last state bundle" v={c.lastBundle ? fmtAgo(c.lastBundle.time) : "—"} s={c.lastBundle ? `generation ${c.lastBundle.generation} · ${c.lastBundle.objects} objects` : ""} />
      </div>
      <div className="dcols">
        <div className="card"><h3>Agents</h3><div className="bd">
          <Table cols={["Cluster", "Available", "Version", "Velero namespace", "Last seen"]} empty="No agent has reported yet." rows={c.agents.map(a => [<b>{a.cluster}</b>, a.available ? <span style={{color: "var(--ok)"}}>yes</span> : <span style={{color: "var(--bad)"}}>no</span>, <Mono dim>{a.version}</Mono>, <Mono dim>{a.veleroNamespace}</Mono>, a.lastSeen ? fmtAgo(a.lastSeen) : ""])} />
          <div className="sech" style={{margin: "12px 0 8px"}}><h2>Site stack</h2><span className="ln"></span></div>
          <Table cols={["Cluster", "Version", "Applied", "Message"]} empty="No stack delivered yet." rows={c.stack.map(s => [<b>{s.cluster}</b>, <Mono dim>{s.version}</Mono>, s.applied ? <span style={{color: "var(--ok)"}}>yes</span> : <span style={{color: "var(--warn)"}}>pending</span>, s.message])} />
        </div></div>
        <div>
          <div className="card"><h3>Settings</h3><div className="bd">
            <Props rows={[["Agent namespace", <Mono>{ag.namespace}</Mono>], ["Agent status interval", ag.statusInterval], ["Hook image allow-list", (ag.hookImageAllowList || []).join(", ")],
              ["Ramen namespace", <Mono>{rm.namespace}</Mono>], ["Ramen config", <Mono>{rm.configMapName}</Mono>], ["Velero namespace", <Mono>{rm.veleroNamespace}</Mono>], ["Ops namespace", <Mono>{rm.opsNamespace}</Mono>],
              ["Retention", `${(c.retention || {}).days || 90} days · keep ${(c.retention || {}).keepPerApplication || 10} per application`],
              ["Bootstrap image registry", <Mono>{bs.imageRegistry}</Mono>], ["Feature gates", Object.entries(fg).filter(([, v]) => v).map(([k]) => k).join(", ") || "none"]]} />
          </div></div>
          {c.archive && <div className="card" style={{marginTop: 10}}><h3>Archive</h3><div className="bd">
            <Props rows={[["Endpoint", <Mono>{ar.endpoint}</Mono>], ["Bucket", <Mono>{ar.bucket}</Mono>], ["Region", ar.region], ["Prefix", <Mono>{ar.prefix || "dr/"}</Mono>], ["Credentials", <Mono dim>{refName2(ar.credentialsSecretRef)}</Mono>],
              ["Report object lock", ar.reportRetainDays ? `${ar.reportRetainDays} days` : "off"], ["State bundle", (ar.bundle || {}).disabled ? "disabled" : `every ${(ar.bundle || {}).interval || "5m"} · retain ${(ar.bundle || {}).retainDays || 30} days`],
              c.lastBundle && ["Last bundle key", <Mono dim>{c.lastBundle.key}</Mono>]]} />
          </div></div>}
        </div>
      </div>
      <Conditions o={c} />
    </div>
  );
}

// ---- home: the policy dashboard -----------------------------------------------
function DrHubHome({nav}) {
  const plans = useResource("drhub.plans", () => drhub.plans(), 10000);
  const paths = useResource("drhub.paths", () => drhub.paths(), 10000);
  const apps = useResource("drhub.apps", () => drhub.apps(), 6000);
  const runs = useResource("drhub.runs", () => Promise.all([drhub.actions(), drhub.tests()]), 6000);
  const cfg = useResource("drhub.cfg", () => drhub.config().catch(() => null), 15000);
  const acc = useAccess();
  const ps = plans.data || [], dp = paths.data || [], as = apps.data || [];
  const [actions, tests] = runs.data || [[], []];
  const running = [...actions, ...tests].filter(r => !r.terminal);
  const failed = actions.filter(a => a.status === "Failed" && a.completionTime && Date.now() - Date.parse(a.completionTime) < 7 * 86400e3);
  const c = cfg.data;
  const err = plans.error || paths.error || apps.error;
  const byV = v => as.filter(a => a.verdict === v).length;
  const mayPlan = acc.can("create", "drhub", {kind: "pplan"});
  const mayPath = acc.can("create", "drhub", {kind: "drpath"});
  const mayApp = acc.can("create", "drhub", {kind: "papp", namespace: DR_NS()});
  const inbox = openFindings(as);
  const mappingOpen = as.filter(a => a.siteMapping === "Open").length;
  const gate = (ok, el, why) => ok ? el : React.cloneElement(el, {disabled: true, title: why, onClick: undefined});
  return (
    <div>
      <div className="dhead"><div style={{minWidth: 0, flex: 1}}>
        <h1>Disaster recovery</h1>
        <div className="dsub">Plans declare sites and replication; DR paths declare directions; applications are protected along them. Every screen is a CR on the hub, every write a CR write with your RBAC.</div>
      </div>
        <div style={{display: "flex", gap: 8, flexWrap: "wrap"}}>
          {gate(mayApp, <button className="btn" onClick={() => window.__ui.dialog(protectAppDialogDR(ps, c), {kind: "papp", id: "new"})}><Icon n="shield" s={12} />Protect application</button>, acc.why("create", "drhub", {kind: "papp", namespace: DR_NS()}))}
          {gate(mayPath, <button className="btn" onClick={() => window.__ui.dialog(newPathDialog(ps), {kind: "drpath", id: "new"})}><Icon n="swap" s={12} />Declare path</button>, acc.why("create", "drhub", {kind: "drpath"}))}
          {gate(mayPlan, <button className="btn primary" onClick={() => window.__ui.dialog(newPlanDialog(), {kind: "pplan", id: "new"})}><Icon n="plus" s={12} />New plan</button>, acc.why("create", "drhub", {kind: "pplan"}))}
        </div></div>
      {err && <div className="banner"><Icon n="alert" s={15} /><span><b>Cannot read the DR hub.</b> {err.message}{err.status === 404 ? " — the dr.simplyblock.io CRDs are not installed on this cluster." : err.status === 403 ? " — the console's identity lacks list on dr.simplyblock.io; bind dr-viewer or a wider role." : ""}</span></div>}
      {c && c.counts.agents > 0 && c.counts.agentsAvailable < c.counts.agents && <div className="banner"><Icon n="alert" s={15} /><span><b>{c.counts.agents - c.counts.agentsAvailable} site{c.counts.agents - c.counts.agentsAvailable === 1 ? "" : "s"} without dr-agent.</b> {c.agents.filter(a => !a.available).map(a => a.cluster).join(", ")} — hooks, probes, tests and zone moves cannot run there.</span></div>}
      {c && c.recoveryMode && <div className="banner" style={{color: "var(--warn)", borderColor: "color-mix(in srgb,var(--warn) 35%,transparent)", background: "color-mix(in srgb,var(--warn) 8%,var(--panel))"}}><Icon n="alert" s={15} /><span><b>Recovery mode.</b> The hub's controllers are held while the DR state is restored.</span></div>}
      {!!failed.length && <div className="banner"><Icon n="alert" s={15} /><span><b>{failed.length} action{failed.length === 1 ? "" : "s"} failed in the last 7 days:</b> {failed.slice(0, 4).map(a => <Ref key={a.id} label={a.name} onClick={() => nav.detail(a)} />)}</span></div>}
      <div className="stats">
        <Stat k="Plans" v={ps.length} s={`${ps.filter(p => p.status === "Ready").length} ready`} c={ps.some(p => p.status === "NotReady") ? "var(--bad)" : null} />
        <Stat k="DR paths" v={dp.length} s={`${dp.filter(p => p.status === "Invalid").length} invalid · ${dp.filter(p => p.profileConsistency === "Inconsistent").length} inconsistent`} />
        <Stat k="Applications" v={as.length} s={`${byV("Ready")} ready · ${byV("Degraded")} degraded · ${byV("NotReady")} not ready`} c={byV("NotReady") ? "var(--bad)" : byV("Degraded") ? "var(--warn)" : null} />
        <Stat k="Running now" v={running.length} s={`${actions.filter(a => !a.terminal).length} actions · ${tests.filter(t => !t.terminal).length} tests`} c={running.length ? "var(--info)" : null} />
        <Stat k="Sites with agent" v={c ? `${c.counts.agentsAvailable}/${c.counts.agents}` : "—"} c={c && c.counts.agentsAvailable < c.counts.agents ? "var(--warn)" : null} />
        <Stat k="Open findings" v={inbox.length} s={`${mappingOpen} application${mappingOpen === 1 ? "" : "s"} with open site mapping`} c={inbox.length ? "var(--warn)" : null} />
        <Stat k="Last failover RTO" v={(() => { const f = actions.filter(a => a.action === "Failover" && a.rtoSeconds != null).sort((x, y) => Date.parse(y.completionTime) - Date.parse(x.completionTime))[0]; return f ? fmtSecs(f.rtoSeconds) : "—"; })()} />
      </div>

      <div className="sech"><h2>Readiness matrix · application × declared path</h2><span className="ln"></span><button className="chip" onClick={() => nav.drLayer("protectedapps")}>Open applications</button></div>
      <div className="card"><div className="bd" style={{overflowX: "auto"}}>
        {!as.length ? <div className="nolim">No protected application yet.</div>
          : <Table cols={["Application", "Plan", "Now on", ...dp.map(p => `${p.from}→${p.to}`)]} rows={as.map(a => [
            <Ref label={`${a.namespace}/${a.name}`} onClick={() => nav.detail(a)} />, <Mono dim>{a.planName}</Mono>, <Mono>{a.currentCluster}</Mono>,
            ...dp.map(p => { const x = a.paths.find(y => y.name === p.name); return x ? <span title={x.checks.filter(c => c.status === "Fail").map(c => c.name).join(", ")}><VerdictBadge v={x.verdict} sm /></span> : <span style={{color: "var(--dim2)"}}>·</span>; })])} />}
        <p className="mdesc" style={{margin: "9px 0 0"}}>Columns are exactly the declared paths. A dot means the path does not cover that application; a Relocate-only path is never red for a missing rehearsal.</p>
      </div></div>

      {!!inbox.length && <>
        <div className="sech"><h2>Resolution inbox · open site-mapping decisions</h2><span className="ln"></span></div>
        <div className="card"><div className="bd" style={{overflowX: "auto"}}>
          <Table cols={["Path → target", "Category", "Source value", "Role", "Applications · VMs", "Reason", "Candidates on the target", "Where to decide"]} rows={inbox.map(g => [
            <Mono>{g.path || "current site"} <span style={{color: "var(--dim2)"}}>→</span> {g.site}</Mono>, <span className="badge">{g.category}</span>, <Mono>{g.value}</Mono>, <Mono dim>{g.role}</Mono>,
            <span title={g.vms.join(", ")}>{g.apps.map(n => { const a = as.find(x => `${x.namespace}/${x.name}` === n); return a ? <Ref key={n} label={n} onClick={() => nav.detail(a)} /> : <Mono key={n}>{n}</Mono>; })} <span style={{color: "var(--dim2)"}}>· {g.vms.length} VM{g.vms.length === 1 ? "" : "s"}</span></span>,
            g.reason, <Mono dim>{g.candidates.join(", ")}</Mono>,
            g.kind === "guest" ? <span>SiteProfile <Mono>{g.cluster}</Mono>: guestNetworks[{g.role}] + DHCPServer; pinned MAC on the VM</span> : <span>SiteProfile <Mono>{g.cluster}</Mono>: bind logicalNetworks[{g.role || "role"}] to a candidate NAD</span>])} />
          <p className="mdesc" style={{margin: "9px 0 0"}}>One decision per row clears every occurrence on that path. Decisions are taken on the target's SiteProfile (and DHCPServer objects), never as an override; readiness check findings-resolved blocks the path until then.</p>
        </div></div>
      </>}

      <div className="sech"><h2>Protection plans</h2><span className="ln"></span><button className="chip" onClick={() => nav.drLayer("plans")}>Open all</button></div>
      {ps.length ? <div className="grid">{ps.slice(0, 6).map(p => <PPlanTile key={p.id} o={p} nav={nav} />)}</div>
        : plans.loading ? <div className="grid">{[0, 1, 2].map(i => <div className="skel" key={i}></div>)}</div>
        : <div className="empty"><Icon n="shield" s={22} /><b>No protection plan</b><span>Start by declaring the sites and the replication method.</span></div>}

      <div className="sech"><h2>DR paths</h2><span className="ln"></span><button className="chip" onClick={() => nav.drLayer("paths")}>Open all</button></div>
      {!!dp.length && <div className="grid">{dp.slice(0, 6).map(p => <DRPathTile key={p.id} o={p} nav={nav} />)}</div>}
      {!dp.length && !paths.loading && <div className="nolim">No DR path declared — applications cannot become Ready without one.</div>}

      <div className="sech"><h2>Recent runs</h2><span className="ln"></span>
        <button className="chip" onClick={() => nav.drLayer("ractions")}>Actions</button><button className="chip" onClick={() => nav.drLayer("tests")}>Tests</button><button className="chip" onClick={() => nav.drLayer("tschedules")}>Schedules</button></div>
      <div className="card"><div className="bd"><RunList actions={actions} tests={tests} nav={nav} limit={12} /></div></div>

      <div className="sech"><h2>More</h2><span className="ln"></span></div>
      <div className="navcards">
        <NavCard icon="list" title="Recovery plans" sub="ordered sets of applications" count="→" onClick={() => nav.drLayer("rplans")} />
        <NavCard icon="cloud" title="Restores" sub="from S3 backups onto rebuilt sites" count="→" onClick={() => nav.drLayer("restores")} />
        <NavCard icon="k8s" title="Site profiles" sub="per-cluster inventory and bindings" count="→" onClick={() => nav.drLayer("siteprofiles")} />
        <NavCard icon="link" title="DHCP servers" sub="guest address reservations per site" count="→" onClick={() => nav.drLayer("dhcpservers")} />
        <NavCard icon="cluster" title="Site storage" sub="discover, size and deploy a managed site's storage cluster" count="→" onClick={() => nav.drLayer("sitedeploys")} />
        <NavCard icon="gauge" title="DR configuration" sub="agents, Ramen, archive, executor" count="→" onClick={() => nav.drLayer("drconfig")} />
      </div>
    </div>
  );
}

Object.assign(window, {DrHubHome, DRConfigView, PPlanTile, DRPathTile, PAppTile, RPlanTile, RActionTile, TBubbleTile, TSchedTile, RestoreTile, SiteProfileTile, DHCPServerTile, SiteDeployTile, SiteDeployDetail, deploySiteDialog,
  PPlanDetail, DRPathDetail, PAppDetail, RPlanDetail, RActionDetail, TBubbleDetail, TSchedDetail, RestoreDetail, SiteProfileDetail, DHCPServerDetail, MappingPanel,
  runActionDialog, runTestDialog, restoreDialog, newPPlanDialog: newPlanDialog, newPathDialog, protectAppDialogDR, newRPlanDialog, newScheduleDialog, newDHCPServerDialog, ACTION_KIND_META, KIND_LABEL_DR,
  editPlanS3Dialog, editTiersDialog, editBindingsDialog, sitesSpec, sitesError, overrideError, missingServers});
