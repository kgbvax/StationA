// Keyboard: Up/Down move the highlight, Enter acts, s Skip, u Undo, f focuses
// the filters. Up/Down/Enter/s/u act on the list only while the list has
// focus, never while a filter chip does (a stray Skip changes every view).
(function () {
  const H = { 'X-Chaseq': '1' };
  const post = (url) => fetch(url, { method: 'POST', headers: H }).then(() => window.chaseqRefresh && window.chaseqRefresh());
  const onChip = () => document.activeElement && document.activeElement.classList.contains('chip');
  const chips = () => Array.from(document.querySelectorAll('.filters .chip'));

  document.addEventListener('keydown', (e) => {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    if (onChip()) {
      const list = chips(), i = list.indexOf(document.activeElement);
      if (e.key === 'ArrowRight') { list[Math.min(i + 1, list.length - 1)].focus(); e.preventDefault(); }
      else if (e.key === 'ArrowLeft') { list[Math.max(i - 1, 0)].focus(); e.preventDefault(); }
      else if (e.key === ' ' || e.key === 'Enter') { toggle(document.activeElement); e.preventDefault(); }
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

  function toggle(el) {
    const d = el.dataset;
    if (d.band) return post('/filters?band=' + encodeURIComponent(d.band));
    if (d.needed !== undefined) return post('/filters?needed=' + d.needed);
    if (d.clear) return post('/filters?clear=1');
  }

  document.addEventListener('click', (e) => {
    const chip = e.target.closest('.chip');
    if (chip) { toggle(chip); return; }
    const row = e.target.closest('.row');
    if (row) post('/act?row=' + row.dataset.row);
  });
})();
