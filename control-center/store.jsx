// ---------------------------------------------------------------------------
// STORE — one cached, optionally live, collection per (context, kind, scope)
//
// Views used to refetch every list they touch on a six-second timer, once
// per tile. The fleet dashboard reads the same six kinds for every cluster of
// every context, so the reads are shared here instead: a Collection lists
// once, keeps the objects by uid, and either follows the API server's own
// watch stream or, when streams are scarce or broken, polls. Readers that
// only need a fresh list call `store.read`, which serves from the cache
// while it is younger than its TTL and dedupes requests in flight.
//
// Streams are budgeted. Browsers cap HTTP/1.1 connections per origin and
// everything here shares one origin through nginx, so at most
// SB_CONFIG.maxStreams (default 4) watches are open at a time; the rest poll.
// A watch that dies, or a 410 Gone, falls back to a fresh list. This is the
// seam a backend-for-frontend would take over: it would hold the watches and
// serve the console one stream.
// ---------------------------------------------------------------------------
const STORE_TTL = 4000;
const POLL_MS = 5000;
const POLL_HIDDEN_MS = 15000;
const MAX_STREAMS = (window.SB_CONFIG && window.SB_CONFIG.maxStreams) || 4;

class Collection {
  constructor(key, ctx, kind, opts) {
    this.key = key; this.ctx = ctx; this.kind = kind; this.opts = opts || {};
    this.items = new Map(); this.rv = null; this.state = "idle"; this.error = null;
    this.subs = new Set(); this.fetchedAt = 0; this.inflight = null; this.version = 0;
    this.watchHandle = null; this.pollTimer = null; this.idleTimer = null;
  }
  snapshot() {
    return {items: Array.from(this.items.values()), rv: this.rv, state: this.state, error: this.error, version: this.version};
  }
  // list now; shared between callers while in flight
  list() {
    if (this.inflight) return this.inflight;
    this.state = this.state === "watching" ? "watching" : "listing";
    this.inflight = k8s.list(this.kind, Object.assign({ctx: this.ctx}, this.opts)).then(items => {
      const next = new Map();
      let rv = this.rv;
      items.forEach(o => { next.set(o.metadata.uid || `${o.metadata.namespace}/${o.metadata.name}`, o); });
      // the list's own resourceVersion is what a watch resumes from
      const newest = items.map(o => Number((o.metadata || {}).resourceVersion || 0)).filter(n => !isNaN(n));
      if (newest.length) rv = String(Math.max(...newest));
      this.items = next; this.rv = rv; this.error = null; this.fetchedAt = Date.now(); this.version++;
      if (this.state === "listing") this.state = this.subs.size ? "polling" : "idle";
      this.inflight = null; this.emit();
      return this.snapshot().items;
    }).catch(e => {
      this.error = e; this.state = "error"; this.inflight = null; this.version++; this.emit();
      throw e;
    });
    return this.inflight;
  }
  emit() { this.subs.forEach(fn => { try { fn(this.snapshot()); } catch (e) { console.error(e); } }); }
  apply(ev) {
    const o = ev.object;
    if (!o || !o.metadata) return;
    const id = o.metadata.uid || `${o.metadata.namespace}/${o.metadata.name}`;
    if (ev.type === "DELETED") this.items.delete(id);
    else if (ev.type === "ADDED" || ev.type === "MODIFIED") this.items.set(id, o);
    else return;
    this.version++; this.emit();
  }
  start() {
    if (this.pollTimer || this.watchHandle) return;
    this.list().catch(() => {}).then(() => this.follow());
  }
  follow() {
    if (!this.subs.size) return;
    if (store.streams() < MAX_STREAMS && !this.opts.pollOnly) this.watch();
    else this.poll();
  }
  watch() {
    this.stopPolling();
    this.state = "watching"; this.version++; this.emit();
    this.watchHandle = k8s.watch(this.kind, Object.assign({ctx: this.ctx, resourceVersion: this.rv}, this.opts), ev => {
      if (ev.type === "BOOKMARK") { const m = ev.object && ev.object.metadata; if (m && m.resourceVersion) this.rv = m.resourceVersion; return; }
      if (ev.type === "EXPIRED" || ev.type === "ERROR" || ev.type === "CLOSED") {
        this.watchHandle = null;
        if (!this.subs.size) return;
        // a dead stream becomes a relist, then a poll; the next subscriber
        // change gives the stream another chance
        this.list().catch(() => {}).then(() => this.poll());
        return;
      }
      if (ev.object && ev.object.metadata && ev.object.metadata.resourceVersion) this.rv = ev.object.metadata.resourceVersion;
      this.apply(ev);
    });
  }
  poll() {
    if (this.pollTimer) return;
    this.state = "polling"; this.version++; this.emit();
    const tick = () => {
      this.pollTimer = setTimeout(() => {
        this.list().catch(() => {}).then(tick);
      }, document.hidden ? POLL_HIDDEN_MS : POLL_MS);
    };
    tick();
  }
  stopPolling() { if (this.pollTimer) { clearTimeout(this.pollTimer); this.pollTimer = null; } }
  stop() {
    this.stopPolling();
    if (this.watchHandle) { this.watchHandle.cancel(); this.watchHandle = null; }
    this.state = "idle";
  }
  subscribe(fn) {
    this.subs.add(fn);
    if (this.idleTimer) { clearTimeout(this.idleTimer); this.idleTimer = null; }
    if (this.fetchedAt) fn(this.snapshot());
    this.start();
    return () => {
      this.subs.delete(fn);
      // keep the stream a little while: the next view usually wants it too
      if (!this.subs.size) this.idleTimer = setTimeout(() => { if (!this.subs.size) this.stop(); }, 30000);
    };
  }
}

const store = {
  cols: new Map(),
  key: (ctx, kind, opts) => {
    const o = opts || {};
    return [ctx || LOCAL_CTX, kind, o.allNamespaces ? "*" : (o.namespace || ""), o.labelSelector || "", o.fieldSelector || ""].join("|");
  },
  collection(ctx, kind, opts) {
    const key = store.key(ctx, kind, opts);
    let c = store.cols.get(key);
    if (!c) { c = new Collection(key, ctx || LOCAL_CTX, kind, opts); store.cols.set(key, c); }
    return c;
  },
  // a list that is at most `ttl` old (default 4 s), shared across callers
  read(ctx, kind, opts, ttl) {
    const c = store.collection(ctx, kind, opts);
    const maxAge = ttl === undefined ? STORE_TTL : ttl;
    if (c.fetchedAt && Date.now() - c.fetchedAt < maxAge && !c.error) return Promise.resolve(c.snapshot().items);
    return c.list();
  },
  // after a write: forget what is known about the kind in that context
  invalidate(ctx, kind) {
    store.cols.forEach(c => { if ((!ctx || c.ctx === ctx) && (!kind || c.kind === kind)) { c.fetchedAt = 0; if (c.subs.size) c.list().catch(() => {}); } });
  },
  streams: () => Array.from(store.cols.values()).filter(c => c.watchHandle).length,
  subscribe: (ctx, kind, opts, fn) => store.collection(ctx, kind, opts).subscribe(fn)
};

// ---- hooks -----------------------------------------------------------------
// useCollection(ctx, kind, opts, select) -> {data, loading, error, state, reload}
// `select` turns the raw objects into what the view renders and runs once per
// change, not once per render.
function useCollection(ctx, kind, opts, select) {
  const key = store.key(ctx, kind, opts);
  const sel = React.useRef(select); sel.current = select;
  const [snap, setSnap] = React.useState(() => store.collection(ctx, kind, opts).snapshot());
  React.useEffect(() => store.subscribe(ctx, kind, opts, setSnap), [key]);
  const data = React.useMemo(() => (sel.current ? sel.current(snap.items) : snap.items), [key, snap.version]);
  return {data, loading: !snap.version && snap.state !== "error", error: snap.error, state: snap.state,
    reload: () => store.collection(ctx, kind, opts).list().catch(() => {})};
}

// useV2(ctx, path, loader, {pollMs, key}) -> {data, loading, error, state, reload}
// Follows a control plane list with SSE when the context has a control plane
// and the budget allows, else polls the loader. `loader` returns the
// normalized list; `normalize` maps one streamed DTO the same way.
function useV2(ctx, path, loader, normalize, opts) {
  const o = opts || {};
  const key = `${ctx}|${path}|${o.key || ""}`;
  const [s, setS] = React.useState({data: null, loading: true, error: null, state: "idle"});
  const ld = React.useRef(loader); ld.current = loader;
  const nm = React.useRef(normalize); nm.current = normalize;
  React.useEffect(() => {
    let alive = true, timer = null, cancel = null, items = new Map();
    const emit = state => alive && setS({data: Array.from(items.values()), loading: false, error: null, state});
    const load = async state => {
      try { const d = await ld.current(); if (!alive) return; items = new Map(d.map(x => [x.id, x])); emit(state || "polling"); }
      catch (e) { if (alive) setS(p => ({data: p.data, loading: false, error: e, state: "error"})); }
    };
    const poll = () => { timer = setTimeout(() => load("polling").then(poll), document.hidden ? POLL_HIDDEN_MS : (o.pollMs || POLL_MS)); };
    load("listing").then(() => {
      if (!alive) return;
      if (!hasControlPlane(ctx) || store.streams() >= MAX_STREAMS) { poll(); return; }
      cancel = cp.watch(ctx, path, {
        snapshot: d => { items = new Map((Array.isArray(d) ? d : [d]).map(x => nm.current(x)).map(x => [x.id, x])); emit("live"); },
        created: d => { const x = nm.current(d); items.set(x.id, x); emit("live"); },
        updated: d => { const x = nm.current(d); items.set(x.id, x); emit("live"); },
        deleted: d => { const id = d && d.id; if (id) items.delete(id); emit("live"); },
        state: st => { if (st === "polling" && !timer) poll(); }
      });
    });
    return () => { alive = false; if (timer) clearTimeout(timer); if (cancel) cancel(); };
  }, [key]);
  return Object.assign({}, s, {reload: () => ld.current().then(d => setS(p => ({data: d, loading: false, error: null, state: p.state}))).catch(e => setS(p => Object.assign({}, p, {error: e})))});
}

Object.assign(window, {store, Collection, useCollection, useV2, STORE_TTL});
