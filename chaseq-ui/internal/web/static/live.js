// Keeps the list current: a fresh fragment whenever the server says the list
// changed. The fragment replaces #app wholesale; focus on a filter chip is
// restored by its data attribute.
(function () {
  let busy = false, again = false;
  async function refresh() {
    if (busy) { again = true; return; }
    busy = true;
    try {
      const focused = document.activeElement && document.activeElement.dataset;
      const key = focused && (focused.band ? 'band' : focused.needed !== undefined ? 'needed' : null);
      const val = key && focused[key];
      const r = await fetch('/list', { cache: 'no-store' });
      if (r.ok) {
        document.getElementById('app').innerHTML = await r.text();
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
