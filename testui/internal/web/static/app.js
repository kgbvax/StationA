// stationa test UI — schema-aware MQTT monitor + stimulator.
//
// The browser never speaks MQTT. It talks HTTP+SSE to the Go relay (which holds the
// broker connection + credentials). See ../docs/cmd-convention-audit.md for the
// cross-bridge audit that backs the role registry and command builder.
//
// Rendering is expose-driven first (each slot's /meta.expose is the authoritative
// field/command surface), with a role-registry fallback for slots whose expose is
// read-only but still drivable (notably antennaselect's operator-hold `request`), and a
// raw-JSON fallback for anything unknown. radio (flexbridge) and discovery (hadiscovery)
// have no /cmd handler at all → command panel hidden.

// --- canonical data -------------------------------------------------------

// Canonical HF/VHF band edges (Hz), from docs/conventions/band-mode-reference.md.
const BAND_TABLE = [
  { name: '160m', lo: 1_800_000, hi: 1_999_999 },
  { name: '80m',  lo: 3_500_000, hi: 3_999_999 },
  { name: '60m',  lo: 5_351_500, hi: 5_366_500 },
  { name: '40m',  lo: 7_000_000, hi: 7_299_999 },
  { name: '30m',  lo: 10_100_000, hi: 10_149_999 },
  { name: '20m',  lo: 14_000_000, hi: 14_349_999 },
  { name: '17m',  lo: 18_068_000, hi: 18_167_999 },
  { name: '15m',  lo: 21_000_000, hi: 21_449_999 },
  { name: '12m',  lo: 24_890_000, hi: 24_989_999 },
  { name: '10m',  lo: 28_000_000, hi: 29_699_999 },
  { name: '6m',   lo: 50_000_000, hi: 53_999_999 },
  { name: '2m',   lo: 144_000_000, hi: 146_000_000 },
  { name: '70cm', lo: 430_000_000, hi: 440_000_000 },
];

function bandFromFreq(hz) {
  if (!hz && hz !== 0) return '';
  for (const b of BAND_TABLE) {
    if (hz >= b.lo && hz <= b.hi) return b.name;
  }
  // Fallback per the reference: band-N derived from frequency.
  const mhz = hz / 1_000_000;
  return `band-${Math.round(mhz)}`;
}

const CANONICAL_MODES = ['cw', 'usb', 'lsb', 'am', 'fm', 'data'];

// Roles with NO /cmd handler: hide the command panel entirely.
const NO_CMD_ROLES = new Set(['radio', 'discovery']);

// Role-registry fallback for slots whose /meta.expose is read-only but which still
// accept a /cmd. Currently only the reconciler's operator-hold surface. Each entry
// yields the command inputs the expose block does not.
const ROLE_REGISTRY = {
  reconciler: {
    label: 'Operator hold',
    topicSuffix: 'cmd',
    commands: [
      {
        key: 'request', label: 'Hold', kind: 'enum',
        options: ['auto', 'off', 'port1', 'port2', 'port3', 'port4', 'port5', 'port6'],
        current: (st) => st?.request ?? '',
        build: (v) => ({ request: v }),     // value-key-only, no action
        retain: false,
        help: 'auto = release to band policy; off/port1..port6 = operator hold (ladder tier 2)',
      },
    ],
  },
};

// Slot-name registry: for slots that publish NO /meta (so no role/expose to drive the
// command panel) but still accept a /cmd. Keyed by the last path segment of the address,
// which is always available — unlike role, which needs /meta. Currently ant-switch: a 1:6
// switch taking a value-key-only {"select":"portN"} (confirmed by the retained /cmd on the
// bus). Rendered as a button grid so there's no dropdown to snap shut and no send step.
const SLOT_REGISTRY = {
  'ant-switch': {
    label: 'Antenna switch',
    topicSuffix: 'cmd',
    commands: [
      {
        key: 'select', label: 'Select', kind: 'grid',
        options: ['off', 'port1', 'port2', 'port3', 'port4', 'port5', 'port6'],
        current: (st) => st?.selected ?? '',
        build: (v) => ({ select: v }),       // value-key-only, no action
        retain: false,
        help: '1:6 antenna switch — /cmd {"select":"portN"} (value-key-only)',
      },
    ],
  },
};

// Per-field rendering hints the expose block can't express: derived displays + which
// fields are "special". Keys are field names; roles refine them where needed.
const FIELD_HINTS = {
  freq_hz:   { fmt: 'freq', derive: 'band' },
  band:      { kind: 'band' },
  mode:      { kind: 'mode' },
  tx:        { kind: 'tx' },
  keyed:     { kind: 'keyed' },
  fault:     { kind: 'fault' },
  power:     { kind: 'power' },
  selected:  { kind: 'ports' },
  drive:     { kind: 'bar', max: 100, unit: '%' },
  swr:       { kind: 'swr' },
  device_online: { kind: 'devonline' },
  settling:  { kind: 'spinner' },
  moving:    { kind: 'spinner' },
};

// --- state ----------------------------------------------------------------

const store = {
  slots: new Map(),   // address -> { meta, state, status, cmd, metaParsed }
  order: [],          // insertion order of addresses
  filter: '',
  offlineOnly: false,
  expanded: new Set(),// addresses whose edit/raw sections are open
  ticker: [],         // recent messages [{topic, payload, ts}]
};
const TICKER_MAX = 40;

const el = (id) => document.getElementById(id);
let sse = null;

// --- SSE plumbing ---------------------------------------------------------

function connectSSE() {
  setConn('connecting…');
  sse = new EventSource('/api/stream');
  sse.addEventListener('snapshot', (e) => {
    setConn('live', true);
    applySnapshot(JSON.parse(e.data));
  });
  sse.addEventListener('update', (e) => {
    setConn('live', true);
    applyUpdate(JSON.parse(e.data));
  });
  sse.onopen = () => setConn('live', true);
  sse.onerror = () => {
    setConn('reconnecting…', false);
    sse.close();
    setTimeout(connectSSE, 2000);
  };
}

function setConn(text, ok) {
  const c = el('connstate');
  c.textContent = text;
  c.classList.toggle('ok', ok === true);
  c.classList.toggle('bad', ok === false);
}

// --- store updates --------------------------------------------------------

function parseMeta(metaPlane) {
  if (!metaPlane) return null;
  // The relay pre-decodes object payloads into metaPlane.object; use it directly. The
  // object payload would otherwise arrive as a JS object (not a string) and JSON.parse
  // on it would throw, leaving every card without role/expose/device.
  if (metaPlane.object) return metaPlane.object;
  try { return JSON.parse(metaPlane.payload); } catch { return null; }
}

function applySnapshot(data) {
  store.slots.clear();
  store.order = (data.order || []).slice();
  for (const s of data.slots || []) {
    store.slots.set(s.address, {
      meta: s.meta, state: s.state, status: s.status, cmd: s.cmd,
      metaParsed: parseMeta(s.meta),
    });
  }
  renderAll();
}

function applyUpdate(ev) {
  const addr = ev.address;
  const plane = ev.plane;
  let slot = store.slots.get(addr);
  if (!slot) {
    slot = { meta: null, state: null, status: null, cmd: null, metaParsed: null };
    store.slots.set(addr, slot);
    store.order.push(addr);
  }
  // The server is the sole authority for a clear: it sets ev.cleared and omits the
  // payload for an empty retained publish. Do NOT also gate on !ev.payload — a real
  // update may carry a JSON-falsy payload (null/false/0/"") which is a legitimate
  // stored value, not a clear; treating it as a clear would diverge client state from
  // the server (a snapshot refresh would then show a value the live update just dropped).
  if (plane && ev.cleared) {
    slot[plane] = null;
    if (plane === 'meta') slot.metaParsed = null;
  } else if (plane) {
    // Same Plane shape the snapshot sends: {topic, payload, retained, ts, object}.
    slot[plane] = {
      topic: ev.topic, payload: ev.payload, object: ev.object,
      retained: ev.retained, ts: ev.ts,
    };
    if (plane === 'meta') slot.metaParsed = ev.object ?? parseMeta(slot.meta);
  }
  pushTicker(ev);
  renderAll();
}

function pushTicker(ev) {
  const p = ev.payload;
  // payload arrives as a JS string (e.g. /status "online") or a JS object (e.g. /state);
  // stringify objects so the ticker shows the raw bus content.
  const ps = p == null ? '' : (typeof p === 'string' ? p : JSON.stringify(p));
  store.ticker.unshift({ topic: ev.topic, payload: ps, ts: ev.ts });
  if (store.ticker.length > TICKER_MAX) store.ticker.length = TICKER_MAX;
}

// --- helpers --------------------------------------------------------------

function statusText(slot) {
  // /status is a bare "online"/"offline" string. The relay wraps it as a JSON string,
  // so after JSON.parse it is already unquoted here; strip any residual quotes defensively.
  const raw = slot.status?.payload;
  if (raw == null || raw === '') return '';
  return (typeof raw === 'string' ? raw : String(raw)).replace(/^"|"$/g, '').trim();
}
function isOnline(slot) { return statusText(slot) === 'online'; }

function stateObj(slot) {
  const p = slot.state;
  if (!p) return {};
  if (p.object) return p.object;      // pre-decoded by the relay
  if (!p.payload) return {};
  if (typeof p.payload === 'object') return p.payload;
  try { return JSON.parse(p.payload); } catch { return {}; }
}

// field may be undefined when a state key has no matching expose field-spec (e.g.
// ant-switch's `selected` on a slot with no /meta). Guard every deref so the ports/enum
// widgets fall through to their built-in default lists instead of throwing.
function resolveOptions(field, meta) {
  if (field?.options && field.options.length) return field.options;
  if (field?.options_ref && meta?.capabilities) {
    const v = meta.capabilities[field.options_ref];
    if (Array.isArray(v)) return v;
  }
  return [];
}

// Coerce a string input to the value_type declared by the command descriptor.
function coerce(val, type) {
  switch (type) {
    case 'int':   return parseInt(val, 10);
    case 'float': return parseFloat(val);
    case 'bool':  return val === true || val === 'true' || val === '1';
    default:      return val;
  }
}

// --- API calls ------------------------------------------------------------

async function apiPublish(topic, payload, retain = false, qos = 1) {
  const res = await fetch('/api/publish', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ topic, payload, retain, qos }),
  });
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}
async function apiClear(topic) {
  const res = await fetch('/api/clear', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ topic }),
  });
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}

// --- rendering ------------------------------------------------------------

function renderAll() {
  renderRail();
  renderTicker();
  renderCards();
}

function renderRail() {
  const tree = el('tree');
  tree.innerHTML = '';
  // Group by station: site/station -> [slots]
  const stations = new Map();
  for (const addr of store.order) {
    const parts = addr.split('/');
    const station = parts.length >= 2 ? `${parts[0]}/${parts[1]}` : addr;
    if (!stations.has(station)) stations.set(station, []);
    stations.get(station).push(addr);
  }
  for (const [station, addrs] of stations) {
    const sn = document.createElement('div');
    sn.className = 'node station';
    sn.textContent = station;
    sn.onclick = () => { store.filter = station; el('filter').value = station; renderCards(); };
    tree.appendChild(sn);
    for (const addr of addrs) {
      const slot = store.slots.get(addr);
      const online = isOnline(slot);
      const n = document.createElement('div');
      n.className = 'node slot';
      n.innerHTML = `<span class="${online ? 'live' : 'dead'}">●</span> ${escapeHtml(addr.split('/').slice(2).join('/') || addr)}`;
      n.onclick = () => { store.filter = addr; el('filter').value = addr; renderCards(); };
      tree.appendChild(n);
    }
  }
}

function renderTicker() {
  const t = el('ticker');
  t.innerHTML = store.ticker.map((m) => {
    const p = String(m.payload).slice(0, 120);
    return `<div class="row"><span class="t">${escapeHtml(m.topic)}</span> <span class="p">${escapeHtml(p)}</span></div>`;
  }).join('');
  // Newest entry is unshifted to index 0 on every update; keep the viewport pinned
  // to the top so a stale scrollTop can't leave a fractional row clipped mid-glyph.
  t.scrollTop = 0;
}

function matchesFilter(addr) {
  if (!store.filter) return true;
  return addr.includes(store.filter);
}

function renderCards() {
  const grid = el('cards');
  // Incremental: reuse existing card nodes across re-renders so an open <select> or
  // focused input in the command panel / tools is NOT destroyed when a chatty slot
  // pushes a /state update (e.g. PA at ~10Hz). State-display parts update in place;
  // command/tools DOM is rebuilt only when /meta changes. Without this, opening any
  // chooser snapped it shut within ~100ms as the whole grid was rebuilt.
  if (!grid._byAddr) grid._byAddr = new Map();
  const byAddr = grid._byAddr;
  const present = new Set();
  let any = false;
  for (const addr of store.order) {
    const slot = store.slots.get(addr);
    if (!matchesFilter(addr)) continue;
    if (store.offlineOnly && isOnline(slot)) continue;
    present.add(addr);
    any = true;
    let card = byAddr.get(addr);
    if (!card) {
      card = renderCard(addr, slot);
      byAddr.set(addr, card);
    } else {
      updateCardInPlace(card, addr, slot);
    }
  }
  for (const [addr, card] of [...byAddr]) {
    if (!present.has(addr)) { card.remove(); byAddr.delete(addr); }
  }
  // Reorder ONLY when the visible order changed. Re-appending a card on every render
  // (e.g. while PA pushes /state at ~10Hz) removes+re-inserts the node, which closes any
  // open <select> inside it — the chooser-snap bug that hit pa/ant-select/tuner. Gating
  // on an order signature means zero DOM moves during a steady-state /state burst, so
  // choosers stay open. The minimal insertBefore walk only moves cards out of position.
  const orderSig = store.order.filter((a) => present.has(a)).join('\n');
  if (orderSig !== grid._orderSig) {
    grid._orderSig = orderSig;
    let ref = null;
    for (let i = store.order.length - 1; i >= 0; i--) {
      const addr = store.order[i];
      if (!present.has(addr)) continue;
      const card = byAddr.get(addr);
      if (ref === null) {
        if (grid.lastChild !== card) grid.appendChild(card);
      } else if (ref.previousSibling !== card) {
        grid.insertBefore(card, ref);
      }
      ref = card;
    }
  }
  const empty = grid.querySelector('.empty');
  if (!any && !empty) {
    const e = document.createElement('div');
    e.className = 'muted empty'; e.textContent = 'no slots match'; e.style.padding = '20px';
    grid.replaceChildren(e);
    byAddr.clear();
  } else if (any && empty) {
    empty.remove();
  }
}

function renderCard(addr, slot) {
  const card = document.createElement('div');
  card.className = 'card';
  card.dataset.addr = addr;

  // Skeleton with stable sub-containers; updateCardInPlace fills + refreshes them. The
  // command panel (_cmd) and tools (_tools) are rebuilt ONLY on /meta change, so the
  // choosers/inputs there survive the frequent /state updates (the chooser-snap bug).
  const head = document.createElement('div');
  head.className = 'card-head';
  head.innerHTML = `
    <span class="addr">${escapeHtml(addr)}</span>
    <span class="role"></span>
    <span class="model"></span>
    <span class="pill online">online</span>
    <span class="stale-badge">STALE</span>`;
  card.appendChild(head);
  card._role = head.querySelector('.role');
  card._model = head.querySelector('.model');
  card._statusPill = head.querySelector('.pill');

  card._metaLine = document.createElement('div');
  card._metaLine.className = 'meta-line';
  card.appendChild(card._metaLine);

  card._fields = document.createElement('div');
  card._fields.className = 'fields';
  card.appendChild(card._fields);

  card._cmd = document.createElement('div');
  card.appendChild(card._cmd);

  card._tools = document.createElement('div');
  card.appendChild(card._tools);

  card._metaSig = undefined;
  updateCardInPlace(card, addr, slot);
  return card;
}

// updateCardInPlace refreshes the volatile parts (liveness pill, state fields) every
// call, and the meta-derived parts (role/model, chips, command panel, tools) only when
// /meta changes. /meta.ts is the relay's per-message receive time, so any meta republish
// (including a clear) flips it. Keeping the command panel + tools stable across /state
// updates is what lets an open chooser stay open and the edit-state textarea keep its
// typed content instead of being clobbered every ~100ms.
function updateCardInPlace(card, addr, slot) {
  const online = isOnline(slot);
  const meta = slot.metaParsed || {};
  const role = meta.role || '';

  card.classList.toggle('offline', !online);
  card.classList.toggle('stale', !online);
  card._statusPill.className = `pill ${online ? 'online' : 'offline'}`;
  card._statusPill.textContent = online ? 'online' : 'offline';

  // State display: rebuild each update. Non-interactive (read-only pills/spans), safe.
  renderStateFieldsInto(card._fields, slot, meta);

  const sig = slot.meta ? slot.meta.ts : '';
  if (sig === card._metaSig) return;
  card._metaSig = sig;

  card._role.textContent = role;
  card._model.textContent = meta.device?.model ? ` · ${meta.device.model}` : '';
  card._metaLine.replaceChildren();
  if (meta.link || meta.host || meta.location) {
    const chips = [meta.link, meta.host && `host:${meta.host}`, meta.location && `loc:${meta.location}`]
      .filter(Boolean).map((c) => `<span class="chip">${escapeHtml(c)}</span>`).join('');
    card._metaLine.innerHTML = chips;
  }

  // Command panel (expose-driven + registry fallback).
  card._cmd.replaceChildren();
  const cmdPanel = renderCommandPanel(addr, slot, meta);
  if (cmdPanel) card._cmd.appendChild(cmdPanel);

  // Edit-state + clear + raw.
  card._tools.replaceChildren();
  card._tools.appendChild(renderTools(addr, slot, meta));
}

// renderStateFieldsInto fills an existing container (card._fields) with the schema-aware
// state rows. Rebuilt every /state update — non-interactive, safe. device_online is
// rendered here as a devonline pill (via FIELD_HINTS), no separate chip (no duplication).
function renderStateFieldsInto(wrap, slot, meta) {
  wrap.replaceChildren();
  const st = stateObj(slot);
  const exposeFields = meta.expose?.fields || [];

  // Build a field-spec lookup from expose.
  const byKey = new Map(exposeFields.map((f) => [f.key, f]));

  // Render known fields first in expose order, then any extra state keys not in expose.
  const seen = new Set();
  for (const f of exposeFields) {
    if (!(f.key in st)) continue;
    seen.add(f.key);
    wrap.appendChild(renderField(f.key, st[f.key], f, meta, slot));
  }
  for (const k of Object.keys(st)) {
    if (seen.has(k) || k === 'ts') continue;
    wrap.appendChild(renderField(k, st[k], byKey.get(k), meta, slot));
  }
  if (wrap.children.length === 0) {
    wrap.innerHTML = '<div class="muted">no /state</div>';
  }
}

function renderField(key, value, field, meta, slot) {
  const hint = FIELD_HINTS[key] || {};
  const row = document.createElement('div');
  row.className = 'field';
  const k = document.createElement('div'); k.className = 'k'; k.textContent = field?.name || key;
  const v = document.createElement('div'); v.className = 'v';
  v.appendChild(fieldValueEl(key, value, field, hint, meta, slot));
  row.appendChild(k); row.appendChild(v);
  return row;
}

function fieldValueEl(key, value, field, hint, meta, slot) {
  const frag = document.createDocumentFragment();

  // Special, role/field-aware widgets:
  if (key === 'freq_hz' && typeof value === 'number') {
    frag.appendChild(text(`${value.toLocaleString('en-US')} `));
    frag.appendChild(span('unit', 'Hz'));
    const derived = bandFromFreq(value);
    frag.appendChild(text(' '));
    frag.appendChild(span('pill ' + (derived ? 'on' : 'off'), derived || '—'));
    const st = stateObj(slot);
    if (st.band && derived && st.band !== derived) {
      frag.appendChild(text(' '));
      frag.appendChild(span('pill warn', `bus:${st.band}`));
    }
    return frag;
  }
  if (hint.kind === 'mode' || (key === 'mode')) {
    const known = CANONICAL_MODES.includes(value);
    return span('pill ' + (known ? 'on' : 'warn'), value || '—');
  }
  if (hint.kind === 'tx') {
    return span(`pill ${value === 'tx' ? 'tx' : 'rx'}`, value || 'rx');
  }
  if (hint.kind === 'keyed') {
    const cls = value === 'tx' ? 'tx' : value === 'inhibited' ? 'warn' : 'rx';
    return span(`pill ${cls}`, value || 'rx');
  }
  if (hint.kind === 'fault') {
    const bad = value && value !== 'none';
    return span(`pill ${bad ? 'fault' : 'off'}`, value || 'none');
  }
  if (hint.kind === 'power') {
    // PA power state (on/off). The amp's canonical `power` field is an enum, which
    // the generic enum branch would render as an always-green pill — misleading for
    // "off". Render on=green, off=dim explicitly.
    return span(`pill ${value === 'on' ? 'on' : 'off'}`, value || '—');
  }
  if (hint.kind === 'ports') {
    const opts = resolveOptions(field, meta);
    const wrap = span('ports');
    const list = opts.length ? opts : ['off', 'port1', 'port2', 'port3', 'port4', 'port5', 'port6'];
    for (const p of list) {
      const b = span('port' + (value === p ? ' active' : ''), p);
      wrap.appendChild(b);
    }
    return wrap;
  }
  if (hint.kind === 'bar') {
    const max = hint.max || 100;
    const pct = Math.max(0, Math.min(100, (Number(value) / max) * 100));
    const wrap = document.createElement('div');
    // Build via textContent/DOM, not innerHTML: value is raw /state and could carry
    // markup from a crafted or compromised bridge.
    wrap.appendChild(text(String(value)));
    if (hint.unit) wrap.appendChild(span('unit', hint.unit));
    wrap.appendChild(text(' '));
    const bar = document.createElement('div'); bar.className = 'bar';
    const fill = document.createElement('i'); fill.style.width = pct + '%';
    bar.appendChild(fill); wrap.appendChild(bar);
    return wrap;
  }
  if (hint.kind === 'swr') {
    const bad = Number(value) >= 3;
    return span(`pill ${bad ? 'warn' : 'on'}`, `SWR ${value}`);
  }
  if (hint.kind === 'devonline') {
    return span(`pill ${value ? 'online' : 'offline'}`, value ? 'device online' : 'device offline');
  }
  if (hint.kind === 'spinner') {
    if (value) { const s = span('pill warn', '◆ ' + key); return s; }
    return span('pill off', 'idle');
  }
  if (field?.type === 'boolean' || typeof value === 'boolean') {
    const on = field?.on, off = field?.off;
    const label = value ? (on || 'on') : (off || 'off');
    return span(`pill ${value ? 'on' : 'off'}`, label);
  }
  if (field?.type === 'enum' || (field?.options_ref)) {
    // Enum values like the m5stamp switch's pa/trx use strings 'on'/'off'. Color
    // the pill so state changes are visible instead of always showing green.
    const active = value === 'on' || value === true || value === 1 || value === 'true';
    return span(`pill ${active ? 'on' : 'off'}`, String(value));
  }
  // default: value + unit
  let s = value === null || value === undefined ? '—' : String(value);
  if (field?.unit) s += ` ` ;
  const wrap = document.createElement('div');
  wrap.textContent = s;
  if (field?.unit) { wrap.appendChild(text(' ')); wrap.appendChild(span('unit', field.unit)); }
  return wrap;
}

// --- command panel --------------------------------------------------------

function renderCommandPanel(addr, slot, meta) {
  const role = meta.role || '';
  if (NO_CMD_ROLES.has(role)) return null; // radio/discovery: no /cmd handler

  const wrap = document.createElement('div');
  wrap.className = 'section';
  const h = document.createElement('h4');
  h.textContent = 'Command  /cmd';
  wrap.appendChild(h);

  const grid = document.createElement('div');
  grid.className = 'cmd-grid';

  let added = false;

  // Primary: expose-driven writable fields + actions.
  const expose = meta.expose || {};
  for (const f of expose.fields || []) {
    if (!f.writable || !f.command) continue;
    grid.appendChild(buildSetpointRow(addr, f, meta));
    added = true;
  }
  for (const a of expose.actions || []) {
    grid.appendChild(buildActionRow(addr, a, meta));
    added = true;
  }

  // Fallback: role registry (e.g. reconciler operator hold), then slot-name registry
  // (e.g. ant-switch, which publishes no /meta so there's no role to key on).
  if (!added) {
    const slotName = addr.split('/').pop();
    const reg = ROLE_REGISTRY[role] || SLOT_REGISTRY[slotName];
    if (reg) {
      const topic = `${addr}/${reg.topicSuffix}`;
      for (const c of reg.commands) {
        grid.appendChild(buildRegistryRow(topic, c, slot));
        added = true;
      }
      if (reg.label) h.textContent = `Command  ${reg.label}`;
    }
  }

  if (!added) {
    // Unknown role with a /cmd topic: offer a raw JSON editor.
    grid.appendChild(buildRawCmdRow(addr));
    added = true;
  }

  wrap.appendChild(grid);
  return wrap;
}

function buildSetpointRow(addr, f, meta) {
  const cmd = f.command || {};
  const topic = `${addr}/cmd`;
  const row = document.createElement('div');
  row.className = 'cmd-row';

  const label = document.createElement('label');
  label.textContent = f.name || f.key;
  row.appendChild(label);

  const options = f.type === 'enum' ? resolveOptions(f, meta) : [];
  let input;
  if (f.type === 'enum' && options.length) {
    input = document.createElement('select');
    for (const o of options) {
      const opt = document.createElement('option'); opt.value = o; opt.textContent = o;
      input.appendChild(opt);
    }
  } else if (f.type === 'boolean') {
    input = document.createElement('select');
    for (const o of ['true', 'false']) {
      const opt = document.createElement('option'); opt.value = o; opt.textContent = o;
      input.appendChild(opt);
    }
  } else {
    input = document.createElement('input');
    input.type = 'number';
    if (f.min != null) input.min = f.min;
    if (f.max != null) input.max = f.max;
    if (f.step != null) input.step = f.step;
    input.placeholder = f.unit || '';
  }
  input.value = f.type === 'boolean' ? 'true' : (options[0] || '');
  row.appendChild(input);

  const retain = document.createElement('input');
  retain.type = 'checkbox'; retain.title = 'retain /cmd (model §8: only for self-healing actuators)';
  const rlbl = document.createElement('label'); rlbl.className = 'chk'; rlbl.style.margin = '0';
  rlbl.innerHTML = '<span>retain</span>';
  rlbl.prepend(retain);
  rlbl.style.fontSize = '11px'; rlbl.style.color = 'var(--dim)';

  const send = document.createElement('button');
  send.className = 'send';
  send.textContent = 'send';
  send.onclick = () => {
    let val = input.value;
    if (f.type === 'boolean') val = (val === 'true');
    else val = coerce(val, cmd.value_type || (f.type === 'number' ? 'float' : 'string'));
    const payload = buildPayload(cmd, val);
    sendCommand(send, topic, payload, retain.checked, addr, ackKey(f, val));
  };
  row.appendChild(rlbl);
  row.appendChild(send);
  return row;
}

function buildActionRow(addr, a, meta) {
  const cmd = a.command || {};
  const topic = `${addr}/cmd`;
  const row = document.createElement('div');
  row.className = 'cmd-row';
  const label = document.createElement('label'); label.textContent = a.name || a.key;
  row.appendChild(label);

  // An action with a value_key takes a typed argument (e.g. atr1k `tune` is an
  // enum of mem|full). Render an input for it and send the typed value; an
  // action WITHOUT a value_key is a pure button (retract/stop/fwd/rev) and sends
  // no value. Sending null for a valued action used to arrive as
  // {"action":"tune","value":null} → the bridge unmarshals null to "" and drops
  // it as "unknown mode" — the live "atr not following" bug.
  const hasKey = cmd.value_key && cmd.value_key !== '';
  let input = null;
  let options = [];
  if (hasKey) {
    options = cmd.value_type === 'enum' ? resolveOptions(a, meta) : [];
    if (cmd.value_type === 'enum' && options.length) {
      input = document.createElement('select');
      for (const o of options) {
        const opt = document.createElement('option'); opt.value = o; opt.textContent = o;
        input.appendChild(opt);
      }
    } else if (cmd.value_type === 'bool') {
      input = document.createElement('select');
      for (const o of ['true', 'false']) {
        const opt = document.createElement('option'); opt.value = o; opt.textContent = o;
        input.appendChild(opt);
      }
    } else {
      input = document.createElement('input');
      input.type = (cmd.value_type === 'int' || cmd.value_type === 'float') ? 'number' : 'text';
    }
    input.value = cmd.value_type === 'bool' ? 'true' : (options[0] || '');
    row.appendChild(input);
  }

  const danger = ['retract', 'stop', 'tune'].includes(cmd.action);
  const send = document.createElement('button');
  send.className = 'send' + (danger ? ' danger' : '');
  send.textContent = cmd.action || a.key;
  send.onclick = () => {
    let val = null;
    if (hasKey) {
      val = input.value;
      if (cmd.value_type === 'bool') val = (val === 'true');
      else val = coerce(val, cmd.value_type || 'string');
    }
    const payload = buildPayload(cmd, val);
    const key = hasKey ? `${cmd.action}=${val}` : cmd.action;
    sendCommand(send, topic, payload, false, addr, key);
  };
  row.appendChild(send);
  return row;
}

function buildRegistryRow(topic, c, slot) {
  const st = stateObj(slot);
  const row = document.createElement('div');
  row.className = 'cmd-row';
  const label = document.createElement('label'); label.textContent = c.label;
  row.appendChild(label);

  if (c.kind === 'grid') {
    // Button grid (e.g. ant-switch ports): each click sends immediately — no dropdown to
    // snap shut and no separate send step. Reuses the .ports/.port chip styles.
    const g = document.createElement('div'); g.className = 'ports';
    for (const o of c.options) {
      const b = document.createElement('button');
      b.className = 'port port-btn' + (o === c.current(st) ? ' active' : '');
      b.textContent = o;
      b.onclick = () => sendCommand(b, topic, c.build(o), c.retain, topic.replace('/cmd', ''), c.key);
      g.appendChild(b);
    }
    row.appendChild(g);
    if (c.help) row.title = c.help;
    return row;
  }

  const input = document.createElement('select');
  for (const o of c.options) {
    const opt = document.createElement('option'); opt.value = o; opt.textContent = o;
    if (o === c.current(st)) opt.selected = true;
    input.appendChild(opt);
  }
  row.appendChild(input);
  const send = document.createElement('button');
  send.className = 'send'; send.textContent = 'send';
  send.onclick = () => {
    const val = input.value;
    sendCommand(send, topic, c.build(val), c.retain, topic.replace('/cmd', ''), c.key);
  };
  row.appendChild(send);
  if (c.help) {
    row.title = c.help;
  }
  return row;
}

function buildRawCmdRow(addr) {
  const row = document.createElement('div');
  row.className = 'cmd-row';
  row.innerHTML = `<label>raw /cmd</label>`;
  const ta = document.createElement('textarea');
  ta.className = 'json'; ta.value = '{}';
  const send = document.createElement('button');
  send.className = 'send'; send.textContent = 'send';
  send.onclick = async () => {
    try {
      const payload = JSON.parse(ta.value);
      await apiPublish(`${addr}/cmd`, payload, false);
    } catch (e) { alert('bad JSON: ' + e.message); }
  };
  row.appendChild(ta);
  row.appendChild(send);
  return row;
}

// Build the /cmd JSON payload from an expose command descriptor.
//   action != ""  -> {"action":<action>, <value_key>:<value>}
//   action == ""  -> {<value_key>:<value>}        (value-key-only, e.g. {"select":"port2"})
//   no value_key  -> {"action":<action>}          (button)
function buildPayload(cmd, value) {
  const hasAction = cmd.action && cmd.action !== '';
  const hasKey = cmd.value_key && cmd.value_key !== '';
  if (hasAction && hasKey) return { action: cmd.action, [cmd.value_key]: value };
  if (!hasAction && hasKey) return { [cmd.value_key]: value };
  if (hasAction && !hasKey) return { action: cmd.action };
  return {};
}

function ackKey(f, val) { return `${f.key}=${val}`; }

// Fire-and-observe: send, then watch the slot's /state ts for a change (plane discipline).
async function sendCommand(btn, topic, payload, retain, addr, key) {
  const slot = store.slots.get(addr);
  const before = slot?.state?.ts || '';
  btn.classList.add('pending'); btn.classList.remove('ack', 'timeout');
  try {
    await apiPublish(topic, payload, retain);
  } catch (e) {
    btn.classList.remove('pending'); btn.classList.add('timeout');
    btn.title = e.message;
    return;
  }
  // Watch up to 3s for a /state update on this slot.
  const deadline = Date.now() + 3000;
  const check = () => {
    const now = store.slots.get(addr)?.state?.ts || '';
    if (now && now !== before) {
      btn.classList.remove('pending'); btn.classList.add('ack');
      setTimeout(() => btn.classList.remove('ack'), 1500);
      return;
    }
    if (Date.now() > deadline) {
      btn.classList.remove('pending'); btn.classList.add('timeout');
      return;
    }
    setTimeout(check, 150);
  };
  // Some commands (e.g. retract) won't change /state ts; still show pending cleared after 3s.
  setTimeout(check, 200);
}

// --- tools: edit-state, clear, raw ----------------------------------------

function renderTools(addr, slot, meta) {
  const wrap = document.createElement('div');
  wrap.className = 'section';
  const h = document.createElement('h4');
  const toggle = document.createElement('span');
  toggle.className = 'toggle'; toggle.textContent = '▸ ';
  h.appendChild(toggle);
  h.appendChild(document.createTextNode('tools'));
  wrap.appendChild(h);
  const body = document.createElement('div');
  body.style.display = 'none';
  body.appendChild(buildEditState(addr, slot, meta));
  body.appendChild(buildClearRow(addr, slot, meta));
  body.appendChild(buildRawView(slot));
  wrap.appendChild(body);
  h.onclick = () => {
    const open = body.style.display !== 'none';
    body.style.display = open ? 'none' : 'block';
    toggle.textContent = open ? '▸ ' : '▾ ';
  };
  return wrap;
}

function buildEditState(addr, slot, meta) {
  const wrap = document.createElement('div');
  wrap.innerHTML = `<div class="muted" style="margin-bottom:4px">simulate retained /state (publishes <code>${escapeHtml(addr)}/state</code> retained)</div>`;
  const ta = document.createElement('textarea');
  ta.className = 'json'; ta.value = JSON.stringify(stateObj(slot), null, 2);
  const btn = document.createElement('button');
  btn.className = 'btn'; btn.textContent = 'publish retained /state';
  btn.style.marginTop = '4px';
  btn.onclick = async () => {
    try {
      const payload = JSON.parse(ta.value);
      await apiPublish(`${addr}/state`, payload, true);
    } catch (e) { alert('bad JSON: ' + e.message); }
  };
  wrap.appendChild(ta); wrap.appendChild(btn);
  return wrap;
}

function buildClearRow(addr, slot, meta) {
  const wrap = document.createElement('div');
  wrap.style.marginTop = '8px';
  const planes = ['state', 'meta', 'cmd'];
  const row = document.createElement('div');
  row.className = 'btnrow';
  row.innerHTML = '<span class="muted" style="align-self:center">clear retained:</span> ';
  for (const p of planes) {
    const b = document.createElement('button');
    b.className = 'btn danger'; b.textContent = p;
    b.onclick = () => {
      if (confirm(`Clear retained ${addr}/${p}?`)) apiClear(`${addr}/${p}`).catch((e) => alert(e.message));
    };
    row.appendChild(b);
  }
  wrap.appendChild(row);
  return wrap;
}

function buildRawView(slot) {
  const wrap = document.createElement('div');
  wrap.style.marginTop = '8px';
  wrap.innerHTML = '<div class="muted">raw planes:</div>';
  const pre = document.createElement('div');
  pre.className = 'raw';
  const show = (v) => (v == null ? '' : typeof v === 'string' ? v : JSON.stringify(v));
  const lines = [];
  for (const p of ['meta', 'state', 'status', 'cmd']) {
    const pl = slot[p];
    lines.push(`${p}: ${pl ? show(pl.payload) : '—'}`);
  }
  pre.textContent = lines.join('\n');
  wrap.appendChild(pre);
  return wrap;
}

// --- tiny DOM helpers -----------------------------------------------------

function text(s) { return document.createTextNode(s); }
function span(cls, s) { const e = document.createElement('span'); e.className = cls; e.textContent = s; return e; }
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// --- boot -----------------------------------------------------------------

el('filter').oninput = (e) => { store.filter = e.target.value; renderCards(); };
el('offline-only').onchange = (e) => { store.offlineOnly = e.target.checked; renderCards(); };
connectSSE();