// Keyboard: Up/Down move the highlight, Enter acts, s Skip, u Undo, f focuses
// the filters. Up/Down/Enter/s/u act on the list only while the list has
// focus, never while a filter chip does (a stray Skip changes every view).
(function () {
  const H = { 'X-Chaseq': '1' };
  const post = (url) => fetch(url, { method: 'POST', headers: H }).then((r) => {
    // A refused request (400/403) carries no report: say so in the result strip.
    if (!r.ok && r.status !== 409) window.chaseqFailed && window.chaseqFailed(r.status);
    return window.chaseqRefresh && window.chaseqRefresh();
  }, () => window.chaseqFailed && window.chaseqFailed(0));
  const onChip = () => document.activeElement && document.activeElement.classList.contains('chip');
  const chips = () => Array.from(document.querySelectorAll('.filters .chip'));

  document.addEventListener('keydown', (e) => {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    // A held key must not queue the same action thirty times a second.
    if (e.repeat && (e.key === 'Enter' || e.key === 's' || e.key === 'u' || e.key === ' ')) { e.preventDefault(); return; }
    if (onChip()) {
      const list = chips(), i = list.indexOf(document.activeElement);
      if (e.key === 'ArrowRight') { list[Math.min(i + 1, list.length - 1)].focus(); e.preventDefault(); }
      else if (e.key === 'ArrowLeft') { list[Math.max(i - 1, 0)].focus(); e.preventDefault(); }
      else if (e.key === ' ' || e.key === 'Enter') { toggle(document.activeElement, e.shiftKey); e.preventDefault(); }
      else if (e.key === 'Escape') { document.getElementById('rows').focus(); e.preventDefault(); }
      return;
    }
    switch (e.key) {
      case 'ArrowDown': post('/move?d=1'); e.preventDefault(); break;
      case 'ArrowUp': post('/move?d=-1'); e.preventDefault(); break;
      case 'Enter': post('/act'); e.preventDefault(); break;
      case 's': post('/skip'); break;
      case 'u': post('/undo'); break;
      case 'f': { const c = chips()[0]; if (c) c.focus(); e.preventDefault(); break; }
    }
  });

  function toggle(el, add) {
    const d = el.dataset;
    if (d.lock !== undefined) return post('/filters?lock=' + d.lock);
    if (d.band) return el.getAttribute('aria-disabled') === 'true' ? undefined : post('/filters?band=' + encodeURIComponent(d.band));
    if (d.mode) return post('/filters?mode=' + encodeURIComponent(d.mode) + (add ? '' : '&only=1'));
    if (d.award) return post('/filters?award=' + encodeURIComponent(d.award));
    if (d.source) return post('/filters?source=' + encodeURIComponent(d.source));
    if (d.needed !== undefined) return post('/filters?needed=' + d.needed);
    if (d.clear) return post('/filters?clear=1');
  }

  document.addEventListener('click', (e) => {
    const chip = e.target.closest('.chip');
    // Safari does not focus a clicked button; focus it so the re-render restores focus to the chip
    // and a following Enter cannot reach /act (tuning the FLEX).
    if (chip) { chip.focus(); toggle(chip, e.shiftKey || e.ctrlKey || e.metaKey); return; }
    const row = e.target.closest('.row');
    if (row) post('/act?row=' + row.dataset.row);
  });
})();
