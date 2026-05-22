// kvault dashboard — vanilla ES2020, no build step.
// Pairs with index.html (data-field nodes + form IDs) and the
// dashboard HTTP API. SSE on /api/events keeps the stats pill live;
// the rest is request/response.
'use strict';

const PILL_LABELS = { ok: 'ok', warn: 'warn', error: 'err', unknown: '…' };

window.addEventListener('DOMContentLoaded', () => {
  bindForms();
  bindResultsClick();
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

function bindDrawer() {
  const drawer = document.getElementById('turn-drawer');
  document.getElementById('turn-close-btn').addEventListener('click', () => closeDrawer());
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && !drawer.hidden) closeDrawer();
  });
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
    drawer.hidden = false;
    drawer.setAttribute('aria-hidden', 'false');
  } catch (err) {
    appendActivity('error', `open turn ${sessionID}/${turnIdx}: ${err.message}`);
  }
}

function closeDrawer() {
  const drawer = document.getElementById('turn-drawer');
  drawer.hidden = true;
  drawer.setAttribute('aria-hidden', 'true');
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
