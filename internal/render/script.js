// beadline roadmap: optional enhancements. The page is complete without them.
(() => {
  'use strict';
  const doc = document;
  const root = doc.documentElement;
  const controls = doc.getElementById('controls');
  if (!controls) return;
  controls.hidden = false;

  // Preferences live in localStorage when it is available; nothing breaks
  // when it is not (file://, private windows, blocked storage).
  const store = {
    get(k) { try { return localStorage.getItem('beadline.' + k); } catch (e) { return null; } },
    set(k, v) { try { localStorage.setItem('beadline.' + k, v); } catch (e) { /* keep defaults */ } },
  };

  // Theme: auto follows the system; light and dark override it.
  const themeBtn = doc.getElementById('theme');
  const themes = ['auto', 'light', 'dark'];
  const setTheme = (t) => {
    if (!themes.includes(t)) t = 'auto';
    if (t === 'auto') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', t);
    themeBtn.dataset.value = t;
    themeBtn.textContent = 'Theme: ' + t;
  };
  setTheme(store.get('theme'));
  themeBtn.addEventListener('click', () => {
    const t = themes[(themes.indexOf(themeBtn.dataset.value) + 1) % themes.length];
    setTheme(t);
    store.set('theme', t);
  });

  // View: the timeline or the table.
  const views = { timeline: doc.getElementById('timeline'), table: doc.getElementById('table') };
  const viewBtns = controls.querySelectorAll('[data-view]');
  const setView = (v) => {
    if (!views[v]) v = 'timeline';
    for (const k in views) views[k].hidden = k !== v;
    viewBtns.forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.view === v)));
  };
  setView(store.get('view'));
  viewBtns.forEach((b) => b.addEventListener('click', () => {
    setView(b.dataset.view);
    store.set('view', b.dataset.view);
  }));

  // Repo filter: hide rows of other repos and re-stack the lanes left.
  // Goal rows belong to every repo their members are in.
  const svg = doc.querySelector('svg.tl');
  const filter = doc.getElementById('repo-filter');
  const inRepo = (el, repo) => !repo || (el.dataset.repos || '').split(' ').includes(repo);
  const setRepo = (repo) => {
    if (![...filter.options].some((o) => o.value === repo)) repo = '';
    filter.value = repo;
    if (svg) {
      const n = (k) => Number(svg.dataset[k]);
      const head = n('laneHead');
      const rowH = n('rowH');
      const gap = n('gap');
      let y = n('axisH');
      let shown = 0;
      svg.querySelectorAll('g.lane').forEach((lane) => {
        let rows = 0;
        lane.querySelectorAll('g.row').forEach((row) => {
          const show = inRepo(row, repo);
          row.style.display = show ? '' : 'none';
          if (show) row.setAttribute('transform', `translate(0 ${head + rowH * rows++})`);
        });
        const show = !repo || rows > 0 || lane.dataset.repo === repo;
        lane.style.display = show ? '' : 'none';
        if (!show) return;
        const h = head + Math.max(rows, 1) * rowH;
        const bg = lane.querySelector('rect.lane-bg');
        bg.setAttribute('height', h);
        bg.classList.toggle('alt', shown++ % 2 === 1);
        lane.setAttribute('transform', `translate(0 ${y})`);
        y += h + gap;
      });
      const h = y - gap + 12;
      svg.setAttribute('viewBox', `0 0 ${svg.viewBox.baseVal.width} ${h}`);
      svg.setAttribute('height', h);
      svg.querySelectorAll('line.grid, line.now').forEach((l) => l.setAttribute('y2', h));
    }
    doc.querySelectorAll('#table tbody').forEach((tb) => {
      let rows = 0;
      tb.querySelectorAll('tr[data-id]').forEach((tr) => {
        tr.hidden = !inRepo(tr, repo);
        if (!tr.hidden) rows++;
      });
      tb.hidden = !!repo && rows === 0 && tb.dataset.repo !== repo;
    });
  };
  setRepo(store.get('repo') || '');
  filter.addEventListener('change', () => {
    setRepo(filter.value);
    store.set('repo', filter.value);
  });

  // Popover: the row's details, on hover and on keyboard focus. It replaces
  // the native SVG tooltip, which would otherwise show as well.
  const pop = doc.getElementById('pop');
  if (!svg || !pop) return;
  const place = (x, y) => {
    const maxX = window.scrollX + root.clientWidth - pop.offsetWidth - 8;
    pop.style.left = Math.max(window.scrollX + 8, Math.min(x + 14, maxX)) + 'px';
    pop.style.top = (y + 16) + 'px';
  };
  const show = (lines, x, y) => {
    pop.replaceChildren(...lines.map((line) => {
      const d = doc.createElement('div');
      d.textContent = line;
      return d;
    }));
    pop.hidden = false;
    place(x, y);
  };
  const hide = () => { pop.hidden = true; };
  svg.querySelectorAll('g.row').forEach((row) => {
    const title = row.querySelector('title');
    if (!title) return;
    const lines = title.textContent.split('\n');
    title.remove();
    row.addEventListener('mouseenter', (e) => show(lines, e.pageX, e.pageY));
    row.addEventListener('mousemove', (e) => place(e.pageX, e.pageY));
    row.addEventListener('mouseleave', hide);
    row.addEventListener('focus', () => {
      const r = row.getBoundingClientRect();
      show(lines, window.scrollX + r.left + r.width / 3, window.scrollY + r.top + r.height / 2);
    });
    row.addEventListener('blur', hide);
  });
  doc.addEventListener('keydown', (e) => { if (e.key === 'Escape') hide(); });
})();
