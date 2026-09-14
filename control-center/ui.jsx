const {useState, useEffect, useRef, useMemo, useCallback} = React;

const fmtBytes = (n, d) => {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0, v = n;
  while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; }
  return `${v.toFixed(d === undefined ? (v < 10 ? 2 : v < 100 ? 1 : 0) : d)} ${u[i]}`;
};
const fmtNum = n => !n ? "0" : n >= 1e6 ? (n / 1e6).toFixed(2) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(Math.round(n));
// Always returns gigabytes so the fixed "GB/s" label stays truthful.
const fmtBW = n => { const g = (n || 0) / 1e9; return !g ? "0" : g < 1 ? g.toFixed(3) : g < 10 ? g.toFixed(2) : g < 100 ? g.toFixed(1) : g.toFixed(0); };
const pct = (a, b) => b ? Math.min(100, (a / b) * 100) : 0;
const shortId = s => s.slice(0, 8) + "…" + s.slice(-4);

const ICONS = {
  chev: "M6 3.5 10.5 8 6 12.5",
  chevd: "M3.5 6 8 10.5 12.5 6",
  chevu: "M3.5 10 8 5.5 12.5 10",
  pause: "M5 3.5v9M11 3.5v9", play: "M5 3.5 12 8 5 12.5Z",
  list: "M5.5 4h8M5.5 8h8M5.5 12h8M2.5 4h.01M2.5 8h.01M2.5 12h.01",
  search: "M7.2 12.4a5.2 5.2 0 1 0 0-10.4 5.2 5.2 0 0 0 0 10.4ZM11 11l3 3",
  copy: "M5.5 5.5h7v7h-7zM3.5 10.5v-7h7",
  check: "M3.5 8.5 6.5 11.5 12.5 4.5",
  cluster: "M2.5 2.5h5v5h-5zM8.5 2.5h5v5h-5zM2.5 8.5h5v5h-5zM8.5 8.5h5v5h-5z",
  node: "M2.5 3.5h11v4h-11zM2.5 8.5h11v4h-11zM4.5 5.5h.01M4.5 10.5h.01",
  device: "M2.5 4.5h11v7h-11zM10.5 8h1.5",
  pool: "M2.5 5.5c2-1.6 3.5 1.6 5.5 0s3.5 1.6 5.5 0M2.5 9.5c2-1.6 3.5 1.6 5.5 0s3.5 1.6 5.5 0",
  volume: "M3.5 4.5h9v7h-9zM3.5 7h9M6 9.5h.01",
  lock: "M4.5 7.5h7v5h-7zM6 7.5V5.5a2 2 0 0 1 4 0v2",
  ext: "M9 3.5h3.5V7M12.5 3.5 7.5 8.5M11 9.5v3h-8v-8h3",
  sun: "M8 5a3 3 0 1 0 0 6 3 3 0 0 0 0-6M8 1.5v1.5M8 13v1.5M1.5 8H3M13 8h1.5M3.5 3.5l1 1M11.5 11.5l1 1M12.5 3.5l-1 1M4.5 11.5l-1 1",
  moon: "M13 9.5A5.5 5.5 0 0 1 6.5 3a5.5 5.5 0 1 0 6.5 6.5Z",
  alert: "M8 2.5 14.5 13.5h-13zM8 6.5v3.5M8 11.8h.01",
  refresh: "M13 8a5 5 0 1 1-1.6-3.7M13 2.5V5h-2.5",
  x: "M4 4l8 8M12 4l-8 8",
  bell: "M8 2.2a4 4 0 0 0-4 4v3l-1.2 2h10.4L12 9.2v-3a4 4 0 0 0-4-4M6.4 13a1.7 1.7 0 0 0 3.2 0",
  filter: "M2.5 3.5h11l-4.3 5v4l-2.4 1.3v-5.3z",
  gauge: "M3 11.5a5.5 5.5 0 1 1 10 0M8 8.5 10.5 6",
  host: "M2.5 2.5h11v11h-11zM5.5 5.5h5v5h-5zM8 2.5v3M8 10.5v3M2.5 8h3M10.5 8h3",
  power: "M8 2.5v5M11.5 4.4a5 5 0 1 1-7 0",
  plus: "M8 3.5v9M3.5 8h9",
  move: "M2.5 8h11M10.5 5l3 3-3 3M5.5 5l-3 3 3 3",
  trash: "M3 4.5h10M6 4.5V3h4v1.5M4.5 4.5l.7 9h5.6l.7-9M6.8 7v4M9.2 7v4",
  camera: "M2.5 5h2.6l1-1.5h3.8l1 1.5h2.6v8h-11zM8 11a2.4 2.4 0 1 0 0-4.8 2.4 2.4 0 0 0 0 4.8",
  cloud: "M4.6 12.5a3 3 0 0 1-.3-6 4 4 0 0 1 7.6.6 2.7 2.7 0 0 1-.4 5.4z",
  clock: "M8 2.5a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11M8 5v3.2l2.2 1.3",
  link: "M6.8 9.2a2.6 2.6 0 0 0 3.7 0l2-2a2.6 2.6 0 0 0-3.7-3.7l-.9.9M9.2 6.8a2.6 2.6 0 0 0-3.7 0l-2 2a2.6 2.6 0 0 0 3.7 3.7l.9-.9",
  dots: "M8 4.2h.01M8 8h.01M8 11.8h.01",
  shield: "M8 2.2 13 4v4.2c0 3-2.1 4.9-5 5.6-2.9-.7-5-2.6-5-5.6V4z",
  swap: "M3 5.5h9L9.5 3M13 10.5H4l2.5 2.5",
  arrow: "M3 8h10M9.5 4.5 13 8l-3.5 3.5",
  zone: "M2.5 13.5h11M4 13.5V6l4-3.5L12 6v7.5M6.5 13.5v-3h3v3",
  k8s: "M8 1.8 13.4 4.6v6.8L8 14.2 2.6 11.4V4.6zM8 5.4 10.8 6.9v3.2L8 11.6 5.2 10.1V6.9z",
  folder: "M2.5 12.5v-9h4l1.5 2h5.5v7z"
};
const Icon = ({n, s = 14, c, sw = 1.4, style}) => (
  <svg className="ic" width={s} height={s} viewBox="0 0 16 16" fill="none" stroke={c || "currentColor"} strokeWidth={n === "dots" ? 2.4 : sw} strokeLinecap="round" strokeLinejoin="round" style={style} aria-hidden="true"><path d={ICONS[n] || ICONS.chev} /></svg>
);
const fmtDate = s => { if (!s) return "—"; const d = new Date(s); return isNaN(d) ? s : d.toISOString().slice(0, 16).replace("T", " ") + " UTC"; };
const fmtDur = s => { s = Math.round(s || 0); return s < 60 ? s + "s" : s < 3600 ? Math.round(s / 60) + "m" : s < 86400 ? +(s / 3600).toFixed(1) + "h" : +(s / 86400).toFixed(1) + "d"; };
const fmtAgo = s => {
  const d = Date.parse(s); if (isNaN(d)) return "";
  const m = Math.max(0, Math.round(((window.SB_NOW || Date.now()) - d) / 60000));
  return m < 60 ? m + "m ago" : m < 1440 ? Math.round(m / 60) + "h ago" : Math.round(m / 1440) + "d ago";
};

function TrafficLight({status, sm, label}) {
  const m = STATUS_META[status] || {c: "var(--idle)", label: status};
  return <span className={"tl" + (m.blink ? " blink" : "") + (sm ? " sm" : "")} style={{"--c": m.c, color: m.c}}><i className="d"></i>{label !== false && (m.label || status)}</span>;
}

function Uuid({value, short = true}) {
  const [done, setDone] = useState(false);
  return (
    <div className="uuid" title={value}>
      <span>{short ? shortId(value) : value}</span>
      <button className={done ? "done" : ""} title="Copy UUID" onClick={e => {
        e.stopPropagation();
        navigator.clipboard && navigator.clipboard.writeText(value);
        setDone(true); setTimeout(() => setDone(false), 1200);
        window.__toast && window.__toast("UUID copied to clipboard");
      }}><Icon n={done ? "check" : "copy"} s={11} c={done ? "var(--ok)" : undefined} /></button>
    </div>
  );
}

function Capacity({label = "Capacity", total, used, unit}) {
  const p = pct(used, total);
  const c = p > 90 ? "var(--bad)" : p > 75 ? "var(--warn)" : "var(--accent)";
  return (
    <div className="capwrap">
      <div className="caprow"><span>{label} <b className="mono" style={{color: c, fontWeight: 600}}>{p.toFixed(0)}%</b></span>
        <span className="capval">{fmtBytes(used)} <span style={{color: "var(--dim2)"}}>/ {fmtBytes(total)}</span></span></div>
      <div className="bar"><i style={{width: p + "%", "--bc": c}}></i></div>
    </div>
  );
}

function Sparkline({data, color = "var(--dim2)", w = 52, h = 15}) {
  if (!data || !data.length) return null;
  const max = Math.max(...data, 1), min = Math.min(...data);
  const rng = max - min || max || 1;
  const pts = data.map((v, i) => `${(i / (data.length - 1)) * w},${h - ((v - min) / rng) * (h - 2) - 1}`).join(" ");
  return <svg className="spark" width={w} height={h} viewBox={`0 0 ${w} ${h}`} fill="none" aria-hidden="true"><polyline points={pts} stroke={color} strokeWidth="1.2" strokeLinejoin="round" strokeLinecap="round" /></svg>;
}

function Metric({label, unit, r, w, fmt, hist, color}) {
  return (
    <div className="met">
      <div className="k"><span>{label}{unit && <span style={{opacity: .8}}> {unit}</span>}</span><Sparkline data={hist} color={color} /></div>
      <div className="rw">
        <div><span>READ</span><b>{fmt(r)}</b></div>
        <div><span>WRITE</span><b>{fmt(w)}</b></div>
      </div>
    </div>
  );
}

const QosChips = ({qos}) => {
  if (!qos) return <div className="nolim">No QoS limits — pool default applies</div>;
  const rows = [["rw iops", qos.rw_ios_per_sec && fmtNum(qos.rw_ios_per_sec)], ["rw", qos.rw_mbytes_per_sec && qos.rw_mbytes_per_sec + " MB/s"],
    ["r", qos.r_mbytes_per_sec && qos.r_mbytes_per_sec + " MB/s"], ["w", qos.w_mbytes_per_sec && qos.w_mbytes_per_sec + " MB/s"]].filter(x => x[1]);
  if (!rows.length) return <div className="nolim">QoS profile set, all limits unlimited</div>;
  return <div className="qos">{rows.map(([k, v]) => <span className="lab" key={k}><i>{k}</i>{v}</span>)}</div>;
};

function useLocal(key, init) {
  const [v, setV] = useState(() => {
    try { const s = localStorage.getItem(key); return s === null ? init : JSON.parse(s); } catch (e) { return init; }
  });
  useEffect(() => { try { localStorage.setItem(key, JSON.stringify(v)); } catch (e) {} }, [key, v]);
  return [v, setV];
}

// Backup policy schedule: interval · retained backup versions · online snapshots.
// The interval is both the snapshot/backup periodicity and the merge cadence
// for that tier once its version count is exceeded.
const BackupSchedule = ({rows, showTotals = true}) => !rows || !rows.length
  ? <div className="nolim">No schedule rows — this policy triggers nothing.</div>
  : (
    <div className="rettable">
      <div className="rethead"><span>Every</span><span>Versions</span><span>Online</span></div>
      {rows.map((r, i) => (
        <div className="retrow" key={i}>
          <span className="mono">{r.interval}</span>
          <b>{r.versions}×</b>
          <b style={r.online ? {color: "var(--accent)"} : {color: "var(--dim2)", fontWeight: 400}}>{r.online ? r.online + "×" : "—"}</b>
        </div>
      ))}
      {showTotals && <div className="retrow tot">
        <span>retained</span>
        <b>{rows.reduce((a, r) => a + (Number(r.versions) || 0), 0)}×</b>
        <b>{rows.reduce((a, r) => a + (Number(r.online) || 0), 0)}×</b>
      </div>}
    </div>
  );

Object.assign(window, {fmtBytes, fmtNum, fmtBW, fmtDate, fmtAgo, fmtDur, pct, shortId, Icon, ICONS,
  TrafficLight, Uuid, Capacity, Sparkline, Metric, QosChips, BackupSchedule, useLocal,
  useState, useEffect, useRef, useMemo, useCallback});
