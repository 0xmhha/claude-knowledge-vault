// kvault dashboard — vanilla ES2020, no build step.
// Pairs with index.html (data-field nodes + form IDs) and the
// dashboard HTTP API. SSE on /api/events keeps the stats pill live;
// the rest is request/response.
'use strict';

const PILL_LABELS = { ok: 'ok', warn: 'warn', error: 'err', unknown: '…' };

// T-D.5 surrounding-turns window. ±N around the focused turn.
const SURROUNDING_RADIUS = 3;

window.addEventListener('DOMContentLoaded', () => {
  bindForms();
  bindResultsClick();
  bindResultsKeyboard();
  bindDrawer();
  loadInitialStats();
  connectEvents();
});

// ─── network helpers ──────────────────────────────────────────────────

async function fetchJSON(method, url, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(url, opts);
  const text = await resp.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch (_) { /* non-JSON OK */ }
  }
  if (!resp.ok) {
    const msg = (data && (data.error || data.message)) || `HTTP ${resp.status}`;
    const err = new Error(msg);
    err.status = resp.status;
    err.body = data;
    throw err;
  }
  return data;
}

// ─── initial load ─────────────────────────────────────────────────────

async function loadInitialStats() {
  try {
    const stats = await fetchJSON('GET', '/api/stats');
    renderStats(stats);
  } catch (err) {
    setPill('error', 'stats fetch failed');
    appendActivity('error', `stats: ${err.message}`);
  }
}

// ─── form bindings ────────────────────────────────────────────────────

function bindForms() {
  document.getElementById('search-form').addEventListener('submit', onSearch);
  document.getElementById('index-form').addEventListener('submit', onIndex);
  document.getElementById('purge-form').addEventListener('submit', onPurge);

  // Re-submit search when a filter changes — only if there's already
  // a query, so changing filters on a fresh load doesn't trigger
  // an empty fetch.
  for (const id of ['filter-source', 'filter-since', 'filter-role', 'filter-limit']) {
    document.getElementById(id).addEventListener('change', () => {
      if (document.getElementById('search-input').value.trim() !== '') {
        document.getElementById('search-form').requestSubmit();
      }
    });
  }
}

async function onSearch(ev) {
  ev.preventDefault();
  const q = document.getElementById('search-input').value.trim();
  if (q === '') return;
  const params = new URLSearchParams({ query: q });
  const limit = document.getElementById('filter-limit').value;
  const source = document.getElementById('filter-source').value;
  const since = document.getElementById('filter-since').value.trim();
  const role = document.getElementById('filter-role').value;
  if (limit) params.set('limit', limit);
  if (source) params.set('source', source);
  if (since) params.set('since', since);
  if (role) params.set('role', role);

  const form = ev.currentTarget;
  setBusy(form, true);
  try {
    const results = await fetchJSON('GET', `/api/search?${params}`);
    renderResults(results || []);
  } catch (err) {
    appendActivity('error', `search failed: ${err.message}`);
    setResultsCount(0);
    clearResults(`error: ${err.message}`);
  } finally {
    setBusy(form, false);
  }
}

async function onIndex(ev) {
  ev.preventDefault();
  const force = document.getElementById('index-force').checked;
  const form = ev.currentTarget;
  setBusy(form, true);
  try {
    const res = await fetchJSON('POST', '/api/index', { force });
    appendActivity('info',
      `index: ${res.filesIndexed}/${res.filesScanned} files, ${res.turnsInserted} turns, ${res.chunksInserted} chunks`);
    // Refresh stats immediately so the pill catches up before SSE.
    renderStats(await fetchJSON('GET', '/api/stats'));
  } catch (err) {
    appendActivity('error', `index failed: ${err.message}`);
  } finally {
    setBusy(form, false);
  }
}

async function onPurge(ev) {
  ev.preventDefault();
  const confirm = document.getElementById('purge-confirm').checked;
  if (!confirm) {
    appendActivity('error', 'purge: tick the confirm checkbox first');
    return;
  }
  const form = ev.currentTarget;
  setBusy(form, true);
  try {
    await fetchJSON('POST', '/api/purge', { confirm: true });
    appendActivity('info', 'purged: all rows dropped');
    document.getElementById('purge-confirm').checked = false;
    clearResults('(vault purged — re-index to repopulate)');
    setResultsCount(0);
    renderStats(await fetchJSON('GET', '/api/stats'));
  } catch (err) {
    appendActivity('error', `purge failed: ${err.message}`);
  } finally {
    setBusy(form, false);
  }
}

// ─── result list ──────────────────────────────────────────────────────

function renderResults(list) {
  const ul = document.getElementById('results-list');
  ul.replaceChildren();
  setResultsCount(list.length);

  const fallback = document.getElementById('results-fallback');
  const usingTrigram = list.length > 0 && list[0].source === 'trigram';
  fallback.hidden = !usingTrigram;

  if (list.length === 0) {
    const li = document.createElement('li');
    li.className = 'muted';
    li.textContent = '(no hits)';
    ul.appendChild(li);
    return;
  }
  for (const r of list) {
    ul.appendChild(buildResultItem(r));
  }
}

function clearResults(message) {
  const ul = document.getElementById('results-list');
  ul.replaceChildren();
  const li = document.createElement('li');
  li.className = 'muted';
  li.textContent = message;
  ul.appendChild(li);
  document.getElementById('results-fallback').hidden = true;
}

function setResultsCount(n) {
  const el = document.getElementById('results-count');
  el.textContent = n === 0 ? '' : `· ${n} hit${n === 1 ? '' : 's'}`;
}

function buildResultItem(r) {
  const li = document.createElement('li');
  li.className = 'result' + (r.source === 'trigram' ? ' source-trigram' : '');
  li.tabIndex = 0; // focusable for keyboard nav
  li.dataset.sessionId = r.session_id;
  li.dataset.turnIndex = String(r.turn_index);

  const meta = document.createElement('span');
  meta.className = 'meta';
  const when = r.ts ? new Date(r.ts).toISOString().slice(0, 16).replace('T', ' ') : '';
  meta.textContent = `${when} · ${r.role || '?'} · turn ${r.turn_index}`;
  li.appendChild(meta);

  const score = document.createElement('span');
  score.className = 'score';
  score.textContent = `score ${formatScore(r.score)}`;
  li.appendChild(score);

  if (r.title) {
    const title = document.createElement('span');
    title.className = 'path';
    title.textContent = r.title;
    li.appendChild(title);
  }

  const snip = document.createElement('span');
  snip.className = 'snippet';
  snip.textContent = r.snippet || '';
  li.appendChild(snip);

  return li;
}

function formatScore(s) {
  if (typeof s !== 'number' || !isFinite(s)) return '—';
  return s.toFixed(2);
}

// ─── result click → drawer ────────────────────────────────────────────

function bindResultsClick() {
  document.getElementById('results-list').addEventListener('click', async (ev) => {
    const li = ev.target.closest('li.result');
    if (!li) return;
    const sid = li.dataset.sessionId;
    const idx = li.dataset.turnIndex;
    if (!sid || idx == null) return;
    await openDrawer(sid, parseInt(idx, 10));
  });
}

// Keyboard navigation on the result list: ↑ / ↓ move focus between
// results, ⏎ opens the drawer for the focused row. We intentionally
// only fire when focus is already on a .result (or on body), so the
// search box still takes ⏎ for submit.
function bindResultsKeyboard() {
  const ul = document.getElementById('results-list');
  document.addEventListener('keydown', (ev) => {
    const focused = document.activeElement;
    const onResult = focused && focused.classList && focused.classList.contains('result');
    const items = Array.from(ul.querySelectorAll('li.result'));
    if (items.length === 0) return;

    if (ev.key === 'ArrowDown') {
      if (!onResult) { items[0].focus(); ev.preventDefault(); return; }
      const i = items.indexOf(focused);
      if (i < items.length - 1) {
        items[i + 1].focus();
        ev.preventDefault();
      }
    } else if (ev.key === 'ArrowUp') {
      if (!onResult) return;
      const i = items.indexOf(focused);
      if (i > 0) {
        items[i - 1].focus();
        ev.preventDefault();
      }
    } else if (ev.key === 'Enter' && onResult) {
      const sid = focused.dataset.sessionId;
      const idx = focused.dataset.turnIndex;
      if (sid && idx != null) {
        openDrawer(sid, parseInt(idx, 10));
        ev.preventDefault();
      }
    }
  });
}

function bindDrawer() {
  const drawer = document.getElementById('turn-drawer');
  document.getElementById('turn-close-btn').addEventListener('click', () => closeDrawer());
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && !drawer.hidden) closeDrawer();
  });
  // Backdrop click — anything outside the drawer (when open) closes
  // it. Listen on document at capture so a single click reliably
  // hits before any other listener.
  document.addEventListener('mousedown', (ev) => {
    if (drawer.hidden) return;
    if (drawer.contains(ev.target)) return;
    // Result clicks reopen the drawer immediately after closing, so
    // ignore those — closeDrawer + openDrawer fires the same frame.
    if (ev.target.closest && ev.target.closest('li.result')) return;
    closeDrawer();
  }, true);
}

async function openDrawer(sessionID, turnIdx) {
  const drawer = document.getElementById('turn-drawer');
  try {
    const t = await fetchJSON('GET',
      `/api/turn?session_id=${encodeURIComponent(sessionID)}&turn_index=${turnIdx}`);
    setField('session_id', t.session_id);
    setField('turn_index', String(t.turn_index));
    setField('role', t.role || '—');
    setField('ts', t.ts ? new Date(t.ts).toISOString() : '—');
    setField('title', t.title || '—');
    document.getElementById('turn-content').textContent = t.content || '';
    renderDrawerToolbar(t);
    drawer.hidden = false;
    drawer.setAttribute('aria-hidden', 'false');
    // Move focus into the drawer so Esc / Tab behave sensibly.
    document.getElementById('turn-close-btn').focus();
  } catch (err) {
    appendActivity('error', `open turn ${sessionID}/${turnIdx}: ${err.message}`);
  }
}

function closeDrawer() {
  const drawer = document.getElementById('turn-drawer');
  drawer.hidden = true;
  drawer.setAttribute('aria-hidden', 'true');
  // Drop any surrounding-turns expansion so the next open starts clean.
  const surr = document.getElementById('turn-surrounding');
  if (surr) surr.remove();
}

// renderDrawerToolbar injects a small action bar above the content
// area: Copy (clipboard), Show ±N (load surrounding turns), and a
// file-path display. The bar is created fresh each open so it
// captures the current turn's identifiers via closure.
function renderDrawerToolbar(t) {
  const meta = document.getElementById('turn-meta');
  // Drop any prior toolbar (Show ±N could have been clicked before).
  const oldBar = document.getElementById('turn-toolbar');
  if (oldBar) oldBar.remove();
  const oldPath = document.getElementById('turn-filepath');
  if (oldPath) oldPath.remove();

  const bar = document.createElement('div');
  bar.id = 'turn-toolbar';
  bar.className = 'row';

  const copyBtn = document.createElement('button');
  copyBtn.type = 'button';
  copyBtn.textContent = 'Copy content';
  copyBtn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(t.content || '');
      appendActivity('info', `copied turn ${t.session_id}/${t.turn_index}`);
    } catch (err) {
      appendActivity('error', `copy failed: ${err.message}`);
    }
  });
  bar.appendChild(copyBtn);

  const surrBtn = document.createElement('button');
  surrBtn.type = 'button';
  surrBtn.textContent = `Show ±${SURROUNDING_RADIUS} turns`;
  surrBtn.addEventListener('click', () => loadSurrounding(t.session_id, t.turn_index, surrBtn));
  bar.appendChild(surrBtn);

  meta.parentNode.insertBefore(bar, document.getElementById('turn-content'));

  if (t.file_path) {
    const pathRow = document.createElement('p');
    pathRow.id = 'turn-filepath';
    pathRow.className = 'muted';
    const label = document.createTextNode('source: ');
    const code = document.createElement('code');
    code.textContent = t.file_path;
    pathRow.appendChild(label);
    pathRow.appendChild(code);
    meta.parentNode.insertBefore(pathRow, document.getElementById('turn-content'));
  }
}

// loadSurrounding fetches ±N turns around the focused one (skipping
// the focused index itself) and renders them above + below the
// main content. Missing turns (404) are skipped silently — the
// edges of a session don't have neighbours in both directions.
async function loadSurrounding(sessionID, focusIdx, btn) {
  btn.disabled = true;
  const oldSurr = document.getElementById('turn-surrounding');
  if (oldSurr) oldSurr.remove();

  const wrap = document.createElement('div');
  wrap.id = 'turn-surrounding';

  const before = [];
  const after = [];
  for (let d = 1; d <= SURROUNDING_RADIUS; d++) {
    before.push(fetchTurnSafe(sessionID, focusIdx - d));
    after.push(fetchTurnSafe(sessionID, focusIdx + d));
  }
  const beforeTurns = (await Promise.all(before)).filter(Boolean).reverse();
  const afterTurns = (await Promise.all(after)).filter(Boolean);

  if (beforeTurns.length > 0) {
    const h = document.createElement('h3');
    h.textContent = `Before (${beforeTurns.length})`;
    wrap.appendChild(h);
    for (const t of beforeTurns) wrap.appendChild(surroundingBlock(t));
  }
  if (afterTurns.length > 0) {
    const h = document.createElement('h3');
    h.textContent = `After (${afterTurns.length})`;
    wrap.appendChild(h);
    for (const t of afterTurns) wrap.appendChild(surroundingBlock(t));
  }
  if (beforeTurns.length === 0 && afterTurns.length === 0) {
    const p = document.createElement('p');
    p.className = 'muted';
    p.textContent = '(no neighbouring turns)';
    wrap.appendChild(p);
  }
  document.getElementById('turn-content').after(wrap);
}

async function fetchTurnSafe(sessionID, idx) {
  if (idx < 0) return null;
  try {
    return await fetchJSON('GET',
      `/api/turn?session_id=${encodeURIComponent(sessionID)}&turn_index=${idx}`);
  } catch (err) {
    if (err.status === 404) return null;
    appendActivity('error', `surrounding turn ${idx}: ${err.message}`);
    return null;
  }
}

function surroundingBlock(t) {
  const div = document.createElement('div');
  div.className = 'surrounding-turn';
  const head = document.createElement('p');
  head.className = 'muted';
  head.textContent = `turn ${t.turn_index} · ${t.role || '?'}`;
  const pre = document.createElement('pre');
  pre.className = 'turn-content';
  pre.textContent = t.content || '';
  div.appendChild(head);
  div.appendChild(pre);
  return div;
}

// ─── SSE ──────────────────────────────────────────────────────────────

function connectEvents() {
  const es = new EventSource('/api/events');
  es.addEventListener('stats', (ev) => {
    try { renderStats(JSON.parse(ev.data)); } catch (_) { /* malformed */ }
  });
  es.addEventListener('error', (ev) => {
    if (ev.data) {
      let msg = ev.data;
      try { msg = JSON.parse(ev.data); } catch (_) { /* string */ }
      appendActivity('error', `sse: ${msg}`);
    }
    // EventSource handles reconnect with backoff — no manual retry.
  });
}

// ─── rendering helpers ────────────────────────────────────────────────

function renderStats(stats) {
  if (!stats) return;
  const sessions = stats.Sessions ?? stats.sessions ?? 0;
  const turns = stats.Turns ?? stats.turns ?? 0;
  const chunks = stats.Chunks ?? stats.chunks ?? 0;
  document.getElementById('stats-summary').textContent =
    `${sessions} session${sessions === 1 ? '' : 's'} · ${turns} turns · ${chunks} chunks`;
  if (sessions === 0) {
    setPill('unknown', 'empty vault');
  } else {
    setPill('ok', 'ready');
  }
}

function setField(name, value) {
  const el = document.querySelector(`[data-field="${name}"]`);
  if (el) el.textContent = value;
}

function setPill(state, summary) {
  const pill = document.getElementById('stats-pill');
  pill.className = `pill pill-${state}`;
  pill.textContent = PILL_LABELS[state] || state;
  if (summary !== undefined) {
    document.getElementById('stats-summary').textContent = summary;
  }
}

function setBusy(form, busy) {
  for (const btn of form.querySelectorAll('button')) {
    btn.disabled = busy;
  }
}

function appendActivity(level, text) {
  const log = document.getElementById('activity-log');
  const first = log.firstElementChild;
  if (first && first.classList.contains('muted')) first.remove();

  const li = document.createElement('li');
  if (level === 'error') li.classList.add('error');
  const stamp = document.createElement('time');
  stamp.textContent = new Date().toLocaleTimeString();
  li.appendChild(stamp);
  li.appendChild(document.createTextNode(text));
  log.insertBefore(li, log.firstChild);

  // Cap at 100 entries so a long-running session stays flat.
  while (log.childElementCount > 100) log.removeChild(log.lastChild);
}
