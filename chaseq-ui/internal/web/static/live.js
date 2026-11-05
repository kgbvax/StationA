// Keeps the list current: a fresh fragment whenever the server says the list
// changed. The fragment replaces #app wholesale; focus on a filter chip is
// restored by its data attribute.
(function () {
  let busy = false, again = false, last = '';
  window.chaseqFailed = function (status) {
    last = ''; // the next refresh must replace the strip we are about to overwrite
    const f = document.querySelector('.result');
    if (f) f.innerHTML = '<span class="part" data-status="failed">request failed' + (status ? ' (' + status + ')' : '') + '</span>';
  };
  async function refresh() {
    if (busy) { again = true; return; }
    busy = true;
    try {
      // Every chip kind (one list: a test checks it against list.html and keys.js).
      // VALUED kinds are found again by value, the others are single chips whose value flips.
      const KINDS = ['band', 'mode', 'award', 'source', 'needed', 'lock', 'clear'];
      const VALUED = ['band', 'mode', 'award', 'source'];
      const focused = document.activeElement && document.activeElement.dataset;
      const key = focused && KINDS.find((k) => focused[k] !== undefined) || null;
      const val = key && focused[key];
      const wasChip = !!(document.activeElement && document.activeElement.classList && document.activeElement.classList.contains('chip'));
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
        if (key || wasChip) {
          const el = key && document.querySelector('[data-' + key + ']' + (VALUED.includes(key) ? '[data-' + key + '="' + CSS.escape(val) + '"]' : ''));
          // A chip that vanished (clear after clearing) hands focus to the list,
          // never to the page body, where Enter would act.
          if (el) el.focus(); else if (rows) rows.focus();
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
