// Detail-page panels: tasks, event log, control-plane containers, FDB backups,
// SPDK threads & logs, SMART. Several read from the agent, not from API v2.
const Tabs = ({items, active, onChange}) => (
  <div className="tabs">{items.map(t => (
    <button key={t.k} className={"tab" + (active === t.k ? " on" : "")} onClick={() => onChange(t.k)}>
      <Icon n={t.icon} s={13} />{t.label}{t.n !== undefined && <span className="tn">{t.n}</span>}
    </button>
  ))}</div>
);

const LEVEL_C = {ERROR: "var(--bad)", error: "var(--bad)", WARN: "var(--warn)", WARNING: "var(--warn)", warn: "var(--warn)",
  NOTICE: "var(--info)", INFO: "var(--dim)", info: "var(--dim)", DEBUG: "var(--dim2)"};
const shortTs = s => (s || "").replace("T", " ").replace(/(\.\d+)?Z$/, "");

function LogStream({lines, loading, error, onRetry, empty, tools, height = 340}) {
  if (error) return <div className="bd"><ErrorState error={error} onRetry={onRetry} /></div>;
  return (
    <>
      {tools}
      <div className="logstream" style={{maxHeight: height}}>
        {loading && !lines.length ? <div className="lmsg">loading…</div>
          : !lines.length ? <div className="lmsg">{empty || "No lines match the current filter."}</div>
          : lines.map((l, i) => (
            <div className="lrow" key={i}>
              <span className="lts">{shortTs(l.ts)}</span>
              <span className="llvl" style={{color: LEVEL_C[l.level] || "var(--dim)"}}>{l.level}</span>
              <span className="lmsgtxt">{l.msg}</span>
            </div>
          ))}
      </div>
    </>
  );
}

const CopyBtn = ({get, label = "Copy"}) => {
  const [done, setDone] = useState(false);
  return <button className="chip" onClick={() => {
    navigator.clipboard && navigator.clipboard.writeText(get());
    setDone(true); setTimeout(() => setDone(false), 1300);
  }}><Icon n={done ? "check" : "copy"} s={12} />{done ? "Copied" : label}</button>;
};

// ---- tasks ----------------------------------------------------------------
const TASK_ST = ["running", "new", "suspended", "done"];
const TASK_SM = {running: "var(--info)", new: "var(--idle)", suspended: "var(--warn)", failed: "var(--bad)", done: "var(--ok)"};
const resultColor = r => !r ? "var(--dim2)" : /^canceled/.test(r) ? "var(--warn)" : /fail|error/i.test(r) ? "var(--bad)" : "var(--dim)";
const retryTxt = t => t.maxRetry ? `${t.retry}/${t.maxRetry}` : String(t.retry);

function SubTasks({id, rev, cols}) {
  const {data, loading, error} = useResource("sub|" + id + "|" + rev, () => api.subtasks(id), 4000);
  if (loading) return <tr className="subrow"><td colSpan={cols} className="lmsg">loading subtasks…</td></tr>;
  if (error) return <tr className="subrow"><td colSpan={cols} className="lmsg">could not load subtasks</td></tr>;
  if (!data.length) return <tr className="subrow"><td colSpan={cols} className="lmsg">no subtasks</td></tr>;
  return data.map(s => (
    <tr className="subrow" key={s.id}>
      <td></td>
      <td className="mono" style={{paddingLeft: 20, color: "var(--dim)"}}>↳ {shortId(s.id)}</td>
      <td className="mono">{s.distrib || "—"}</td>
      <td className="mono">{s.fn}</td>
      <td className="mono" style={{color: s.retry ? "var(--warn)" : "var(--dim2)"}}>{retryTxt(s)}</td>
      <td><span className="tstat" style={{"--c": TASK_SM[s.status]}}><i></i>{s.status}</span></td>
      <td style={{color: resultColor(s.result)}}>{s.result || "—"}</td>
      <td className="mono" style={{color: "var(--dim)"}}>{fmtAgo(s.updatedAt)}</td>
      <td></td>
    </tr>
  ));
}

function TasksPanel({cluster}) {
  const [rev, setRev] = useState(0);
  const [q, setQ] = useState("");
  const [fn, setFn] = useState("");
  const [sts, setSts] = useState(["running", "new", "suspended"]);
  const [showDone, setShowDone] = useState(false);
  const [open, setOpen] = useState({});
  const {data, loading, error, reload} = useResource("tasks|" + cluster.id + "|" + rev, () => api.tasks(cluster.id), 3000);
  const tasks = data || [];
  const fns = [...new Set(tasks.map(t => t.fn))].sort();
  const active = showDone ? sts.concat("done") : sts;
  const rows = tasks.filter(t => active.includes(t.status) && (!fn || t.fn === fn)
    && (!q || `${t.id} ${t.fn} ${t.target || ""} ${t.result || ""}`.toLowerCase().includes(q.toLowerCase())));
  const counts = {};
  tasks.forEach(t => counts[t.status] = (counts[t.status] || 0) + 1);
  const cancel = async t => {
    try { await api.taskCancel(t.id); window.__toast(`Task ${shortId(t.id)} cancelled`); setRev(r => r + 1); }
    catch (e) { window.__toast(e.message); }
  };
  if (error) return <ErrorState error={error} onRetry={reload} />;
  return (
    <div className="card">
      <div className="ptools">
        <div className="search" style={{minWidth: 200}}><Icon n="search" s={13} c="var(--dim2)" />
          <input value={q} placeholder="Search task id, target, result…" onChange={e => setQ(e.target.value)} /></div>
        <select className="sel" value={fn} onChange={e => setFn(e.target.value)}>
          <option value="">All functions</option>
          {fns.map(f => <option key={f} value={f}>{f}</option>)}
        </select>
        <div className="chips">
          {TASK_ST.filter(s => s !== "done").map(s => (
            <button key={s} className={"chip" + (sts.includes(s) ? " on" : "")}
              onClick={() => setSts(sts.includes(s) ? sts.filter(x => x !== s) : [...sts, s])}>
              <Dot c={TASK_SM[s]} />{s}<span className="n">{counts[s] || 0}</span></button>
          ))}
          <button className={"chip" + (showDone ? " on" : "")} onClick={() => setShowDone(!showDone)}>
            <Dot c={TASK_SM.done} />done<span className="n">{counts.done || 0}</span></button>
        </div>
        <div className="spacer"></div>
        <span className="count">{rows.length}</span>
        <span className="live"><i></i>live</span>
      </div>
      <table className="dt tasks">
        <thead><tr><th style={{width: 26}}></th><th>Task ID</th><th>Target</th><th>Function</th><th>Retry</th><th>Status</th><th>Result</th><th>Updated</th><th></th></tr></thead>
        <tbody>
          {loading && !tasks.length ? <tr><td colSpan="9" className="lmsg">loading…</td></tr>
            : !rows.length ? <tr><td colSpan="9" className="lmsg">No tasks match. Completed tasks are hidden unless you opt in.</td></tr>
            : rows.map(t => (
              <React.Fragment key={t.id}>
                <tr>
                  <td>{t.subtaskTotal > 0 && <button className="xpand" onClick={() => setOpen(o => Object.assign({}, o, {[t.id]: !o[t.id]}))}>
                    <Icon n={open[t.id] ? "chevd" : "chev"} s={11} /></button>}</td>
                  <td className="mono" style={{fontWeight: 600}}>{shortId(t.id)}</td>
                  <td className="mono" style={{color: "var(--dim)"}}>
                    {t.subtaskTotal > 0
                      ? <span style={{color: "var(--accent)"}}>master · {t.subtaskTotal} subtasks</span>
                      : (t.target || "—").replace(/^NodeID:/, "node ").replace(/^ClusterID:/, "cluster ")}</td>
                  <td className="mono">{t.fn}</td>
                  <td className="mono" style={{color: t.retry ? "var(--warn)" : "var(--dim2)"}}>{retryTxt(t)}</td>
                  <td><span className="tstat" style={{"--c": TASK_SM[t.status]}}><i></i>{t.status}</span></td>
                  <td style={{color: resultColor(t.result)}}>{t.result || "—"}</td>
                  <td className="mono" style={{color: "var(--dim)"}}>{fmtAgo(t.updatedAt)}</td>
                  <td style={{textAlign: "right"}}>
                    <button className="chip" disabled={t.status === "done" || t.canceled} onClick={() => cancel(t)}>Cancel</button>
                  </td>
                </tr>
                {open[t.id] && <SubTasks id={t.id} rev={rev} cols={9} />}
              </React.Fragment>
            ))}
        </tbody>
      </table>
    </div>
  );
}

// ---- alerts ---------------------------------------------------------------
const SEV = {critical: {c: "var(--bad)", label: "critical", rank: 2}, warning: {c: "var(--warn)", label: "warning", rank: 1},
  info: {c: "var(--info)", label: "info", rank: 0}};

// Alerts are indicators, not events. Each row is a condition that is true right
// now; when the condition clears the row disappears on the next poll. Nothing
// is dismissed or closed — a lamp can only be silenced while it is lit.
function AlertsPanel({cluster, nav}) {
  const [rev, setRev] = useState(0);
  const [q, setQ] = useState("");
  const [sev, setSev] = useState([]);
  const [showSilenced, setShowSilenced] = useState(true);
  const {data, loading, error, reload} = useResource("alerts|" + (cluster ? cluster.id : "cp") + "|" + rev,
    () => cluster ? api.alerts(cluster.id) : api.cpAlerts(), 5000);
  const all = data || [];
  const rows = all
    .filter(a => (showSilenced || !a.silenced) && (!sev.length || sev.includes(a.severity))
      && (!q || `${a.title} ${a.detail} ${a.rule} ${(a.nodeNames || []).join(" ")} ${(a.deviceNames || []).join(" ")}`.toLowerCase().includes(q.toLowerCase())))
    .sort((x, y) => (x.silenced - y.silenced) || SEV[y.severity].rank - SEV[x.severity].rank || x.title.localeCompare(y.title));
  const lit = all.filter(a => !a.silenced);
  const counts = {critical: lit.filter(a => a.severity === "critical").length, warning: lit.filter(a => a.severity === "warning").length};
  const act = async (a, f, msg) => {
    try { await f(a.id); window.__toast(msg); setRev(r => r + 1); }
    catch (e) { window.__toast(e.message); }
  };
  if (error) return <ErrorState error={error} onRetry={reload} />;
  return (
    <>
      <div className="stats" style={{marginBottom: 12}}>
        <Stat k="Critical" v={counts.critical} c={counts.critical ? "var(--bad)" : "var(--ok)"} s={counts.critical ? "conditions holding now" : "nothing lit"} />
        <Stat k="Warning" v={counts.warning} c={counts.warning ? "var(--warn)" : "var(--ok)"} s={counts.warning ? "conditions holding now" : "nothing lit"} />
        <Stat k="Silenced" v={all.filter(a => a.silenced).length} s="cleared when the condition clears" />
        <Stat k="Rules watched" v={api.alertRuleCount(cluster ? "cluster" : "control-plane") || "—"} s={cluster ? "cluster scope" : "control plane scope"} />
      </div>
      <div className="card">
        <div className="ptools">
          <div className="search" style={{minWidth: 210}}><Icon n="search" s={13} c="var(--dim2)" />
            <input value={q} placeholder="Search condition, node, device…" onChange={e => setQ(e.target.value)} /></div>
          <div className="chips">
            {["critical", "warning"].map(s => (
              <button key={s} className={"chip" + (sev.includes(s) ? " on" : "")}
                onClick={() => setSev(sev.includes(s) ? sev.filter(v => v !== s) : [...sev, s])}>
                <Dot c={SEV[s].c} />{s}<span className="n">{counts[s]}</span></button>
            ))}
            <button className={"chip" + (showSilenced ? " on" : "")} onClick={() => setShowSilenced(!showSilenced)}>
              <Icon n="check" s={10} />include silenced</button>
          </div>
          <div className="spacer"></div>
          <span className="count">{rows.length}</span>
          <span className="live"><i></i>live</span>
        </div>
        {loading && !all.length ? <div className="lmsg">loading…</div>
          : !rows.length ? <div className="lmsg">{all.length ? "No condition matches this filter." : "Nothing lit. Every watched condition is currently false."}</div>
          : <div className="alertlist">{rows.map(a => (
            <div className={"alertrow " + a.severity + (a.silenced ? " ack" : "")} key={a.id}>
              <span className="asev" title={a.severity}><i></i></span>
              <div style={{minWidth: 0, flex: 1}}>
                <div className="atitle">{a.title}
                  {a.silenced && <span className="badge">silenced</span>}
                </div>
                <div className="adetail">{a.detail}</div>
                <div className="ameta">
                  {!cluster && a.clusterName && <span className="lab"><i>cluster</i>{a.clusterName}</span>}
                  {(a.nodeNames || []).map((nm, i) => (
                    <button className="lab link" key={a.nodeIds[i]} onClick={() => nav.openNode(a.clusterId, a.nodeIds[i])}>
                      <Icon n="node" s={10} />{nm}</button>
                  ))}
                  {(a.deviceNames || []).slice(0, 4).map((nm, i) => (
                    <button className="lab link" key={a.deviceIds[i]} onClick={() => nav.openDevice(a.clusterId, a.nodeId || a.nodeIds[0], a.deviceIds[i])}>
                      <Icon n="device" s={10} />{nm}</button>
                  ))}
                  {(a.deviceNames || []).length > 4 && <span className="lab"><i>+</i>{a.deviceNames.length - 4} more devices</span>}
                  {a.container && <span className="lab"><i>container</i>{a.container}</span>}
                  {!a.nodeNames.length && !a.deviceNames.length && !a.container && cluster
                    && <button className="lab link" onClick={() => nav.openCluster(a.clusterId)}><Icon n="cluster" s={10} />{cluster.name}</button>}
                  <span className="lab"><i>rule</i>{a.rule}</span>
                  <span className="lab"><i>lit</i>{fmtAgo(a.since)}</span>
                </div>
                <div className="aremedy"><Icon n="chev" s={10} />{a.remedy}</div>
              </div>
              <div className="aacts">
                {a.silenced
                  ? <button className="chip" onClick={() => act(a, api.alertUnsilence, "Alert unsilenced")}>Unsilence</button>
                  : <button className="chip" onClick={() => act(a, api.alertSilence, "Alert silenced until the condition clears")}><Icon n="check" s={11} />Silence</button>}
              </div>
            </div>
          ))}</div>}
        <p className="mdesc" style={{margin: "10px 12px 12px"}}>These are indicators, not a history: a row is here because its condition is true right now, and it disappears by itself once the condition resolves. There is no reconciliation event and nothing to close. Silencing keeps a lamp out of the counts while it stays lit; the silence is forgotten when the condition clears.</p>
      </div>
    </>
  );
}

// ---- operations -----------------------------------------------------------
// Every mutation the operator performs is an Ops object with a fixed phase
// list: expansion walks adding node → rebalancing data → complete, removal
// walks data migration → volume migration → removed, migration walks
// restarting node → rebalancing → removing node → migrated. This panel shows
// those state machines while they run and after they land.
const OPS_PHASE_C = {Running: "var(--info)", Succeeded: "var(--ok)", Failed: "var(--bad)", Aborted: "var(--warn)"};

function OpMachine({o, onAbort, nav}) {
  const [open, setOpen] = useState(o.running);
  const c = OPS_PHASE_C[o.phase] || "var(--dim)";
  return (
    <div className={"opcard" + (o.running ? " on" : "")}>
      <div className="oph" onClick={() => setOpen(!open)}>
        <span className="opdot" style={{background: c, boxShadow: o.running ? `0 0 0 3px color-mix(in srgb,${c} 22%,transparent)` : "none"}}></span>
        <b>{o.action}</b>
        <span className="opt">{OPS_TARGET_LABEL[o.targetKind] || "object"}</span>
        {o.targetName && <span className="mono opn">{o.targetName}</span>}
        <span className="spacer"></span>
        <span className="opph2" style={{color: c}}>{o.running ? o.step : o.phase}</span>
        <span className="opstepn">{o.stepIndex + 1}/{o.steps.length}</span>
        <Icon n="chev" s={11} c="var(--dim2)" />
      </div>
      <div className="opbars">{o.steps.map((s, i) => (
        <span key={s + i} className={"opp" + (o.phase === "Succeeded" || i < o.stepIndex ? " done" : i === o.stepIndex && o.running ? " on" : "")} title={s}></span>
      ))}</div>
      {open && <div className="opbody">
        <ol className="opsteps">{o.steps.map((s, i) => {
          const state = o.phase === "Succeeded" || i < o.stepIndex ? "done" : i === o.stepIndex ? (o.running ? "on" : o.phase.toLowerCase()) : "todo";
          return <li key={s + i} className={state}><i></i>{s}
            {state === "on" && <span className="dots"><i></i><i></i><i></i></span>}</li>;
        })}</ol>
        {o.message && <p className="mdesc" style={{margin: "6px 0 0"}}>{o.message}</p>}
        <div className="opev">{o.events.slice(-6).reverse().map((e, i) => (
          <div key={i}><span className="mono">{shortTs(e.at)}</span><b>{e.reason}</b><span>{e.message}</span></div>
        ))}</div>
        <div className="opfoot">
          <span className="lab"><i>started</i>{fmtAgo(o.startedAt)}</span>
          {o.completedAt && <span className="lab"><i>finished</i>{fmtAgo(o.completedAt)}</span>}
          <span className="lab"><i>object</i>{o.name}</span>
          <span className="spacer"></span>
          {o.running && (o.abortable
            ? <button className="chip" onClick={() => onAbort(o)}><Icon n="x" s={11} />Abort</button>
            : <span className="lab" title="this phase cannot be unwound"><i>abort</i>not from {o.step}</span>)}
        </div>
      </div>}
    </div>
  );
}

function OperationsPanel({cluster, nav}) {
  const [rev, setRev] = useState(0);
  const [q, setQ] = useState("");
  const [onlyRunning, setOnlyRunning] = useState(false);
  const {data, loading, error, reload} = useResource("ops|" + (cluster ? cluster.id : "all") + "|" + rev,
    () => api.operations(cluster ? cluster.id : null), 1500);
  const all = data || [];
  const rows = all.filter(o => (!onlyRunning || o.running)
    && (!q || `${o.action} ${o.targetName || ""} ${o.step} ${o.phase}`.toLowerCase().includes(q.toLowerCase())));
  const running = all.filter(o => o.running);
  const abort = async o => {
    try { await api.operationAbort(o.kind, o.name); window.__toast("Abort requested"); setRev(r => r + 1); }
    catch (e) { window.__toast(e.message); }
  };
  if (error) return <ErrorState error={error} onRetry={reload} />;
  return (
    <>
      <div className="stats" style={{marginBottom: 12}}>
        <Stat k="Running" v={running.length} c={running.length ? "var(--info)" : "var(--dim)"} s={running.length ? running.map(o => o.action).join(", ") : "nothing in flight"} />
        <Stat k="Succeeded" v={all.filter(o => o.phase === "Succeeded").length} />
        <Stat k="Failed" v={all.filter(o => o.phase === "Failed").length} c={all.some(o => o.phase === "Failed") ? "var(--bad)" : null} />
        <Stat k="Aborted" v={all.filter(o => o.phase === "Aborted").length} />
      </div>
      <div className="card">
        <div className="ptools">
          <div className="search" style={{minWidth: 210}}><Icon n="search" s={13} c="var(--dim2)" />
            <input value={q} placeholder="Search action, target, phase…" onChange={e => setQ(e.target.value)} /></div>
          <button className={"chip" + (onlyRunning ? " on" : "")} onClick={() => setOnlyRunning(!onlyRunning)}>
            <Icon n="clock" s={10} />running only</button>
          <div className="spacer"></div>
          <span className="count">{rows.length}</span>
          <span className="live"><i></i>live</span>
        </div>
        {loading && !all.length ? <div className="lmsg">loading…</div>
          : !rows.length ? <div className="lmsg">{all.length ? "No operation matches this filter." : "No operation has run on this cluster yet. Shutting down, restarting, expanding, migrating or removing a node all appear here."}</div>
          : <div className="oplist">{rows.map(o => <OpMachine key={o.id} o={o} onAbort={abort} nav={nav} />)}</div>}
        <p className="mdesc" style={{margin: "10px 12px 12px"}}>Each row is one operator operation and its state machine. A phase can only be unwound while the operation is still in an abortable phase — once data has moved, the operation runs to its end.</p>
      </div>
    </>
  );
}

// ---- cluster event log ----------------------------------------------------
const LOG_PRESETS = [
  {k: "status_change", label: "All object status changes"},
  {k: "errors", label: "Error event reporting"},
  {k: "device_status", label: "Device I/O errors"},
  {k: "obj_created", label: "Object lifecycle"},
  {k: "all", label: "Everything"}
];
const EVENT_C = {OBJ_CREATED: "var(--accent)", STATUS_CHANGE: "var(--dim)", device_status: "var(--bad)", jm_compression: "var(--ro)"};

function ClusterLogPanel({cluster}) {
  const [preset, setPreset] = useState("status_change");
  const [q, setQ] = useState("");
  const {data, loading, error, reload} = useResource("logs|" + cluster.id, () => api.clusterLogs(cluster.id), 5000);
  const all = data || [];
  const rows = all.filter(l => {
    if (preset === "errors") return l.level === "Error";
    if (preset === "status_change") return l.event === "STATUS_CHANGE";
    if (preset === "device_status") return l.event === "device_status";
    if (preset === "obj_created") return l.event === "OBJ_CREATED";
    return true;
  }).filter(l => !q || `${l.message} ${l.objectName || ""} ${l.nodeId || ""} ${l.recordStatus}`.toLowerCase().includes(q.toLowerCase()))
    .slice(0, 400);
  if (error) return <ErrorState error={error} onRetry={reload} />;
  return (
    <div className="card">
      <div className="ptools">
        <select className="sel" value={preset} onChange={e => setPreset(e.target.value)}>
          {LOG_PRESETS.map(p => <option key={p.k} value={p.k}>{p.label}</option>)}
        </select>
        <div className="search" style={{minWidth: 220}}><Icon n="search" s={13} c="var(--dim2)" />
          <input value={q} placeholder="Search message, node, status…" onChange={e => setQ(e.target.value)} /></div>
        <div className="spacer"></div>
        <span className="count">{rows.length} / {all.length}</span>
        <CopyBtn get={() => rows.map(l => [l.ts, l.nodeId || "None", l.event, l.level, l.message,
          l.storageId === null || l.storageId === undefined ? "None" : l.storageId, l.vuid || "None", l.recordStatus].join(" | ")).join("\n")} />
        <span className="live"><i></i>live</span>
      </div>
      <div style={{overflow: "auto", maxHeight: 520}}>
        <table className="dt logs">
          <thead><tr><th>Date</th><th>NodeId</th><th>Event</th><th>Level</th><th>Message</th><th style={{textAlign: "right"}}>Storage_ID</th><th style={{textAlign: "right"}}>VUID</th><th>Status</th></tr></thead>
          <tbody>
            {loading && !all.length ? <tr><td colSpan="8" className="lmsg">loading…</td></tr>
              : !rows.length ? <tr><td colSpan="8" className="lmsg">No entries match this filter.</td></tr>
              : rows.map(l => (
                <tr key={l.id}>
                  <td className="mono" style={{color: "var(--dim)", whiteSpace: "nowrap"}}>{shortTs(l.ts)}</td>
                  <td className="mono" style={{color: l.nodeId ? "var(--text)" : "var(--dim2)"}}
                    title={l.objectName ? `${l.objectKind} ${l.objectName}` : ""}>{l.nodeId ? shortId(l.nodeId) : "None"}</td>
                  <td className="mono" style={{color: EVENT_C[l.event] || "var(--dim)"}}>{l.event}</td>
                  <td className="mono" style={{color: LEVEL_C[l.level] || "var(--dim)", fontWeight: l.level === "Info" ? 400 : 600}}>{l.level}</td>
                  <td className="logmsg">{l.message}</td>
                  <td className="mono" style={{textAlign: "right", color: l.storageId === null || l.storageId === undefined ? "var(--dim2)" : "var(--text)"}}>
                    {l.storageId === null || l.storageId === undefined ? "None" : l.storageId}</td>
                  <td className="mono" style={{textAlign: "right", color: l.vuid ? "var(--text)" : "var(--dim2)"}}>{l.vuid || "None"}</td>
                  <td className="mono" style={{color: l.recordStatus === "None" ? "var(--dim2)" : /^skipped|^late|forced/.test(l.recordStatus) ? "var(--warn)" : "var(--dim)"}}>
                    {l.recordStatus}</td>
                </tr>
              ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// ---- control plane containers ---------------------------------------------
const AllocBar = ({label, used, total, unit, color}) => {
  const p = pct(used, total);
  return (
    <div className="alloc">
      <div className="ah"><span>{label}</span><span className="mono">{unit === "cores" ? `${used.toFixed(1)} / ${total} ${unit}` : `${fmtBytes(used)} / ${fmtBytes(total)}`}</span></div>
      <div className="bar"><i style={{width: Math.min(100, p) + "%", "--bc": p > 88 ? "var(--bad)" : p > 70 ? "var(--warn)" : color}}></i></div>
    </div>
  );
};

function ControlPlanePanel() {
  const {data, loading, error, reload} = useResource("cont", () => agent.containers(), 4000);
  const [sel, setSel] = useState(null);
  const list = data || [];
  const groups = [...new Set(list.map(c => c.group))];
  const name = sel || (list[0] && list[0].name);
  if (error) return <ErrorState error={error} onRetry={reload} />;
  return (
    <>
      <div className="sech"><h2>Live allocation</h2><span className="ln"></span><SourceTag what="container runtime" /></div>
      {loading && !list.length ? <div className="grid">{Array.from({length: 6}).map((_, i) => <div className="skel" key={i} style={{height: 150}}></div>)}</div>
        : groups.map(g => (
          <div key={g}>
            <div className="grouplbl">{g}{g === "observability" && <em> · optional deployment</em>}</div>
            <div className="grid" style={{marginBottom: 12}}>
              {list.filter(c => c.group === g).map(c => (
                <div className="tile static" key={c.name} style={{"--sc": c.state === "running" ? "var(--ok)" : "var(--dim2)"}}>
                  <div className="th"><div style={{minWidth: 0, flex: 1}}>
                    <TrafficLight status={c.state === "running" ? "online" : "offline"} />
                    <div className="tname">{c.name}</div>
                    <div className="tsub">{c.image}</div>
                  </div></div>
                  <div style={{marginTop: 10}}>
                    <AllocBar label="vCPU" used={c.cpu.pct / 100} total={c.cpu.alloc} unit="cores" color="var(--accent)" />
                    <AllocBar label="RAM" used={c.mem.used} total={c.mem.limit} color="var(--ok)" />
                    <AllocBar label="Disk" used={c.disk.used} total={c.disk.limit} color="var(--ro)" />
                  </div>
                  <div className="kv" style={{marginTop: 9}}>
                    <div><span>Restarts</span><b>{c.restarts}</b></div>
                    <div><span>Uptime</span><b>{c.uptimeH}h</b></div>
                  </div>
                  <div className="tfoot"><button className="fbtn det" onClick={() => setSel(c.name)}>Logs<Icon n="chev" s={11} /></button></div>
                </div>
              ))}
            </div>
          </div>
        ))}
      {name && <ContainerLogs names={list.map(c => c.name)} name={name} setName={setSel} />}
    </>
  );
}

function ContainerLogs({names, name, setName}) {
  const [q, setQ] = useState("");
  const [lvl, setLvl] = useState("");
  const {data, loading, error, reload} = useResource("clog|" + name, () => agent.containerLogs(name), 4000);
  const lines = (data || []).filter(l => (!lvl || l.level === lvl) && (!q || l.msg.toLowerCase().includes(q.toLowerCase())));
  return (
    <>
      <div className="sech"><h2>Container logs</h2><span className="ln"></span><SourceTag what="kubectl logs" /></div>
      <div className="card">
        <LogStream lines={lines} loading={loading} error={error} onRetry={reload} tools={
          <div className="ptools">
            <select className="sel" value={name} onChange={e => setName(e.target.value)}>
              {names.map(n => <option key={n} value={n}>{n}</option>)}
            </select>
            <select className="sel" value={lvl} onChange={e => setLvl(e.target.value)}>
              <option value="">All levels</option>{["DEBUG", "INFO", "WARN", "ERROR"].map(l => <option key={l} value={l}>{l}</option>)}
            </select>
            <div className="search" style={{minWidth: 200}}><Icon n="search" s={13} c="var(--dim2)" />
              <input value={q} placeholder="Filter lines…" onChange={e => setQ(e.target.value)} /></div>
            <div className="spacer"></div>
            <span className="count">{lines.length}</span>
            <CopyBtn get={() => lines.map(l => `${shortTs(l.ts)} ${l.level} ${l.msg}`).join("\n")} />
            <span className="live"><i></i>live</span>
          </div>} />
      </div>
    </>
  );
}

// ---- FoundationDB backups -------------------------------------------------
function FdbPanel() {
  const [rev, setRev] = useState(0);
  const {data, loading, error, reload} = useResource("fdb|" + rev, () => agent.fdbBackups());
  const list = data || [];
  const restore = b => window.__ui.dialog({
    title: `Restore state database to ${b.version}?`, danger: true,
    desc: "The control plane is stopped, FoundationDB is rolled back to this backup and the services are restarted. Cluster data is untouched, but any control-plane change made after this point is lost.",
    fields: [{k: "confirm", label: "Type RESTORE to confirm", type: "text", match: "RESTORE", required: true}],
    confirm: "Restore state DB", run: () => agent.fdbRestore(b.id).then(() => setRev(r => r + 1))
  }, {kind: "fdb backup", id: b.id});
  if (error) return <ErrorState error={error} onRetry={reload} />;
  return (
    <>
      <div className="sech"><h2>State database backups</h2><span className="ln"></span>
        <span className="count">{list.length} versions</span><SourceTag what="fdbbackup agent" /></div>
      <div className="card"><div className="bd" style={{padding: 0}}>
        <table className="dt"><thead><tr><th>Version</th><th>Backup id</th><th>Type</th><th>Created</th><th style={{textAlign: "right"}}>Size</th><th>Status</th><th></th></tr></thead>
          <tbody>
            {loading && !list.length ? <tr><td colSpan="7" className="lmsg">loading…</td></tr>
              : list.map(b => (
                <tr key={b.id}>
                  <td className="mono" style={{fontWeight: 600}}>{b.version}</td>
                  <td className="mono" style={{color: "var(--dim)"}}>{b.id}</td>
                  <td><span className={"badge " + (b.type === "full" ? "k8s" : "")}>{b.type}</span></td>
                  <td className="mono">{fmtDate(b.createdAt)} <span style={{color: "var(--dim2)"}}>· {fmtAgo(b.createdAt)}</span></td>
                  <td className="mono" style={{textAlign: "right"}}>{fmtBytes(b.size)}</td>
                  <td><TrafficLight status={b.status === "complete" ? "online" : "in_activation"} label={b.status === "complete" ? undefined : false} />
                    {b.status !== "complete" && <span style={{fontSize: 11, color: "var(--info)"}}> writing…</span>}
                    {b.restoreRequestedAt && <span style={{fontSize: 11, color: "var(--warn)"}}> · restore queued</span>}</td>
                  <td style={{textAlign: "right"}}>
                    <button className="chip" disabled={b.status !== "complete"} onClick={() => restore(b)}><Icon n="refresh" s={11} />Restore</button>
                  </td>
                </tr>
              ))}
          </tbody></table>
      </div></div>
    </>
  );
}

// ---- SPDK threads + node logs ---------------------------------------------
function SpdkThreadsPanel({node}) {
  const {data, loading, error, reload} = useResource("spdk|" + node.id, () => agent.spdkThreads(node.id), 2500);
  const threads = data || [];
  const cores = [...new Set(threads.map(t => t.core))].sort((a, b) => a - b);
  if (error) return <ErrorState error={error} onRetry={reload} />;
  return (
    <>
      <div className="sech"><h2>SPDK thread utilization</h2><span className="ln"></span>
        <span className="count">{threads.length} threads · {cores.length} cores</span><SourceTag what="Prometheus" /></div>
      {loading && !threads.length ? <div className="skel" style={{height: 200}}></div>
        : !threads.length ? <div className="empty"><b>No thread metrics</b><span>Prometheus has no samples for this node.</span></div>
        : <div className="card"><div className="bd">
          {cores.map(c => (
            <div className="corerow" key={c}>
              <div className="corelbl mono">core {c}</div>
              <div className="corethreads">
                {threads.filter(t => t.core === c).map(t => (
                  <div className="thread" key={t.name}>
                    <div className="ah"><span className="mono">{t.name}</span><span className="mono">{t.busy.toFixed(1)}%</span></div>
                    <div className="bar"><i style={{width: t.busy + "%", "--bc": t.busy > 85 ? "var(--bad)" : t.busy > 65 ? "var(--warn)" : "var(--accent)"}}></i></div>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div></div>}
    </>
  );
}

// Storage-plane logs for the whole cluster: pick a node, pick a stream. Same
// live source as the node's own log tab — the node agent, not the API.
function StoragePlaneLogPanel({cluster}) {
  const {data: nodes} = useResource("splnodes|" + cluster.id, () => api.nodes(cluster.id));
  const [nodeId, setNodeId] = useState("");
  const [stream, setStream] = useState("spdk");
  const [q, setQ] = useState("");
  const [lvl, setLvl] = useState("");
  const list = nodes || [];
  const active = list.find(n => n.id === nodeId) || list[0];
  const key = active ? active.id : "none";
  const {data, loading, error, reload} = useResource("splog|" + key + "|" + stream,
    () => active ? agent.nodeLogs(active.id, stream) : Promise.resolve([]), 2500);
  const lines = (data || []).filter(l => (!lvl || l.level === lvl) && (!q || l.msg.toLowerCase().includes(q.toLowerCase())));
  if (!list.length) return <div className="empty"><Icon n="node" s={22} /><b>No storage nodes</b><span>Nothing is running on the storage plane yet.</span></div>;
  return (
    <>
      <div className="sech"><h2>Storage plane logs</h2><span className="ln"></span><SourceTag what="node agent" /></div>
      <div className="card">
        <LogStream lines={lines} loading={loading} error={error} onRetry={reload} height={480} tools={
          <div className="ptools">
            <select className="sel" value={active ? active.id : ""} onChange={e => setNodeId(e.target.value)} style={{minWidth: 170}}>
              {list.map(n => <option key={n.id} value={n.id}>{n.hostname}{n.status === "online" ? "" : " · " + n.status}</option>)}
            </select>
            <div className="seg">
              {["spdk", "spdk-proxy"].map(s => <button key={s} className={stream === s ? "on" : ""} onClick={() => setStream(s)}>{s}</button>)}
            </div>
            <select className="sel" value={lvl} onChange={e => setLvl(e.target.value)}>
              <option value="">All levels</option>{["INFO", "NOTICE", "WARNING", "ERROR"].map(l => <option key={l} value={l}>{l}</option>)}
            </select>
            <div className="search" style={{minWidth: 180}}><Icon n="search" s={13} c="var(--dim2)" />
              <input value={q} placeholder="Filter lines…" onChange={e => setQ(e.target.value)} /></div>
            <div className="spacer"></div>
            <span className="count">{lines.length}</span>
            <CopyBtn get={() => lines.map(l => `${shortTs(l.ts)} ${l.level} ${l.msg}`).join("\n")} label="Copy log" />
          </div>
        } />
      </div>
      <p className="mdesc">Streamed from the node agent on {active ? active.hostname : "—"} — the SPDK reactor and its proxy write to the pod's stdout, which the control plane API does not expose. Open the node itself for its thread utilization.</p>
    </>
  );
}

function NodeLogPanel({node}) {
  const [stream, setStream] = useState("spdk");
  const [q, setQ] = useState("");
  const [lvl, setLvl] = useState("");
  const {data, loading, error, reload} = useResource("nlog|" + node.id + "|" + stream, () => agent.nodeLogs(node.id, stream), 2500);
  const lines = (data || []).filter(l => (!lvl || l.level === lvl) && (!q || l.msg.toLowerCase().includes(q.toLowerCase())));
  return (
    <>
      <div className="sech"><h2>Live log stream</h2><span className="ln"></span><SourceTag what="node agent" /></div>
      <div className="card">
        <LogStream lines={lines} loading={loading} error={error} onRetry={reload} height={420} tools={
          <div className="ptools">
            <div className="seg">
              {["spdk", "spdk-proxy"].map(s => <button key={s} className={stream === s ? "on" : ""} onClick={() => setStream(s)}>{s}</button>)}
            </div>
            <select className="sel" value={lvl} onChange={e => setLvl(e.target.value)}>
              <option value="">All levels</option>{["INFO", "NOTICE", "WARNING", "ERROR"].map(l => <option key={l} value={l}>{l}</option>)}
            </select>
            <div className="search" style={{minWidth: 200}}><Icon n="search" s={13} c="var(--dim2)" />
              <input value={q} placeholder="Filter lines…" onChange={e => setQ(e.target.value)} /></div>
            <div className="spacer"></div>
            <span className="count">{lines.length}</span>
            <CopyBtn get={() => lines.map(l => `${shortTs(l.ts)} ${l.level} ${l.msg}`).join("\n")} label="Copy log" />
            <span className="live"><i></i>live</span>
          </div>} />
      </div>
    </>
  );
}

// ---- SMART ----------------------------------------------------------------
function SmartCard({device}) {
  const [rev, setRev] = useState(0);
  const [busy, setBusy] = useState(false);
  // polled, so the report and the traffic light update when a health check
  // started from the actions menu finishes
  const {data, loading, error, reload} = useResource("smart|" + device.id + "|" + rev, () => agent.smart(device.id), 4000);
  const refresh = async () => {
    setBusy(true);
    try { await agent.smartRefresh(device.id); window.__toast("SMART re-read — health status updated from the report"); setRev(r => r + 1); }
    catch (e) { window.__toast(e.message); }
    setBusy(false);
  };
  const gone = data && data.unavailable;
  return (
    <div className="card">
      <h3 style={{display: "flex", alignItems: "center", gap: 10}}>SMART health check<SourceTag what="nvme-cli on the node" />
        <span style={{flex: 1}}></span>
        <button className="chip" disabled={busy || gone} onClick={refresh}
          title={gone ? "the drive is not attached, so nvme-cli cannot reach it" : null}><Icon n="refresh" s={11} />{busy ? "Reading…" : "Run health check"}</button>
      </h3>
      <div className="bd" style={{padding: 0}}>
        {error ? <div style={{padding: 14}}><ErrorState error={error} onRetry={reload} /></div>
          : loading && !data ? <div className="lmsg">loading…</div>
          : gone ? <div className="lmsg">The drive is not attached, so nvme-cli cannot read its SMART log. The last check was {fmtAgo(data.checked_at)}; the health traffic light stays blank until the device is back online.</div>
          : <>
            <div className="smarthead">
              <div><span>Overall</span><b style={{color: data.overall === "PASSED" ? "var(--ok)" : data.overall.indexOf("warn") > 0 ? "var(--warn)" : "var(--bad)"}}>{data.overall}</b></div>
              <div><span>Model</span><b>{data.model}</b></div>
              <div><span>Firmware</span><b>{data.firmware}</b></div>
              <div><span>Last check</span><b>{fmtAgo(data.checked_at)}</b></div>
              <div><span>Health status</span><b style={{color: STATUS_META[data.verdict || "good"].c}}>{STATUS_META[data.verdict || "good"].label}</b></div>
            </div>
            <p className="mdesc" style={{margin: "0 12px", padding: "9px 0 0"}}>The device's health traffic light is this verdict: it is derived from the counters below — available spare against the 10% threshold, media and integrity errors, and the critical warning bit — and is rewritten every time a health check runs.</p>
            <table className="dt"><tbody>
              {data.attributes.map(a => (
                <tr key={a.name}>
                  <td style={{color: "var(--dim)"}}>{a.name}</td>
                  <td className="mono" style={{textAlign: "right", fontWeight: a.note ? 600 : 400, color: a.note ? "var(--warn)" : undefined}}>{a.value}</td>
                  <td style={{color: "var(--warn)", fontSize: 11, width: "40%"}}>{a.note || ""}</td>
                </tr>
              ))}
            </tbody></table>
          </>}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// CONTROL PLANE — one deployment, cross-cluster. Not a child of any cluster:
// it is the thing that manages them all, so it sits at the top level.
function ControlPlaneView({nav}) {
  const [tab, setTab] = useState("services");
  const acc = useAccess();
  const showAccess = acc.canAnywhere("read", "binding") || acc.canAnywhere("read", "role");
  return (
    <div className="scroll">
      <div className="dhead"><div style={{flex: 1, minWidth: 0}}>
        <h1>Control plane</h1>
        <div className="mdesc" style={{marginTop: 4}}>One deployment manages every storage cluster. Its services, logs and state database are shared, so they live here rather than under any single cluster.</div>
      </div></div>
      <div className="tabs">
        {[["services", "Services"], ["alerts", "Alerts"], ["fdb", "State DB"], ["clusters", "Managed clusters"], showAccess && ["access", "Access"]].filter(Boolean).map(function (t) {
          return <button key={t[0]} className={"tab" + (tab === t[0] ? " on" : "")} onClick={() => setTab(t[0])}>{t[1]}</button>;
        })}
      </div>
      {tab === "services" && <ControlPlanePanel />}
      {tab === "alerts" && <AlertsPanel cluster={null} nav={nav} />}
      {tab === "fdb" && <FdbPanel />}
      {tab === "clusters" && <CpClusters nav={nav} />}
      {tab === "access" && showAccess && <AccessView nav={nav} />}
    </div>
  );
}

function CpClusters({nav}) {
  const r = useResource("cp-clusters", () => api.clusters(), 8000);
  const cs = r.data || [];
  return (
    <div className="card"><h3>Clusters managed by this control plane</h3><div className="bd" style={{padding: 0}}>
      <table className="dt"><thead><tr><th>Cluster</th><th>Status</th><th>Nodes</th><th></th></tr></thead><tbody>
        {cs.map(function (c) {
          return <tr key={c.id}>
            <td>{c.name}</td>
            <td><TrafficLight status={c.status} /></td>
            <td className="mono">{c.counts.nodesOnline}/{c.counts.nodes}</td>
            <td style={{textAlign: "right"}}><button className="chip" onClick={() => nav.openCluster(c.id)}>Open</button></td>
          </tr>;
        })}
      </tbody></table>
    </div></div>
  );
}

Object.assign(window, {StoragePlaneLogPanel, OperationsPanel, OpMachine, Tabs, LogStream, CopyBtn, TasksPanel, AlertsPanel, ClusterLogPanel, ControlPlanePanel, FdbPanel, SpdkThreadsPanel, NodeLogPanel, SmartCard, AllocBar, ControlPlaneView, CpClusters});
