// Keeps the list current: a fresh fragment whenever the server says the list
// changed. The fragment replaces #app wholesale; focus on a filter chip is
// restored by its data attribute.
(function () {
  let busy = false, again = false, last = '';
  window.chaseqFailed = function (status) {
    const f = document.querySelector('.result');
    if (f) f.innerHTML = '<span class="part" data-status="failed">request failed' + (status ? ' (' + status + ')' : '') + '</span>';
  };
  async function refresh() {
    if (busy) { again = true; return; }
    busy = true;
    try {
      const focused = document.activeElement && document.activeElement.dataset;
      const key = focused && (focused.band ? 'band' : focused.needed !== undefined ? 'needed' : null);
      const val = key && focused[key];
      const r = await fetch('/list', { cache: 'no-store' });
      if (r.ok) {
        const text = await r.text();
        if (text === last) return; // nothing changed: keep focus, scroll and any press in flight
        last = text;
        const old = document.getElementById('rows');
        const scroll = old ? old.scrollTop : 0, listFocused = old && document.activeElement === old;
        document.getElementById('app').innerHTML = text;
        const rows = document.getElementById('rows');
        if (rows) { rows.scrollTop = scroll; if (listFocused) rows.focus(); }
        const hi = document.querySelector('.row.hi');
        if (hi) hi.scrollIntoView({ block: 'nearest' });
        if (key) {
          const el = document.querySelector('[data-' + key + ']' + (key === 'band' ? '[data-band="' + val + '"]' : ''));
          if (el) el.focus();
        }
      }
    } finally {
      busy = false;
      if (again) { again = false; refresh(); }
    }
  }
  window.chaseqRefresh = refresh;
  const es = new EventSource('/events');
  es.addEventListener('list_changed', refresh);
  es.addEventListener('hello', refresh);
})();
