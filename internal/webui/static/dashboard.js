/* dashboard.js — the page you land on once setup is done. */

let booted = false;
let statusSig = '';        // fingerprint: table and log reload only when this changes
let connSig = '';          // avoids rebuilding the connection cards every tick
let bannerSig = '';
let eventLimit = 20;

init();

async function init() {
  qs('#sync-now').addEventListener('click', onSyncNow);
  qs('#log-more').addEventListener('click', onToggleLogSize);
  show(qs('#app'), true);

  await loadStatus();
  await Promise.all([loadMeasurements(), loadEvents()]);
  booted = true;
  poll(loadStatus, 5000);
}

function refreshAll() {
  loadStatus();
  loadMeasurements();
  loadEvents();
}

/* ── status ── */
async function loadStatus() {
  let s;
  try {
    s = await api('/api/status');
  } catch (err) {
    if (booted) toast(err.message, 'err');
    return;
  }
  // Default the sub-objects so one absent field never blanks the page.
  s.withings = s.withings || {};
  s.garmin = s.garmin || {};
  s.sync = s.sync || {};
  s.backfill = s.backfill || {};
  s.update = s.update || {};
  renderStatus(s);

  const sig = [s.sync.last_at, s.sync.running, s.backfill.running, s.backfill.done].join('|');
  if (sig !== statusSig) {
    statusSig = sig;
    if (booted) { loadMeasurements(); loadEvents(); }
  }
}

function renderStatus(s) {
  renderFooter(s);
  renderBanners(s);
  renderConnections(s);
  renderSync(s);
}

function renderFooter(s) {
  qs('#version').textContent = 'scalebridge-sync ' + (s.version ? 'v' + s.version : '');
  const pill = qs('#update-pill');
  clear(pill);
  if (s.update && s.update.newer) {
    const label = s.update.latest ? 'v' + s.update.latest + ' available' : 'Update available';
    if (s.update.url) {
      pill.appendChild(el('a', { class: 'pill pill-violet', href: s.update.url, target: '_blank', rel: 'noopener noreferrer' }, [icon('download'), label]));
    } else {
      pill.appendChild(el('span', { class: 'pill pill-violet' }, [icon('download'), label]));
    }
  }
  if (s.config_path) qs('#config-note').textContent = 'Config file: ' + s.config_path;
}

function renderBanners(s) {
  const sig = [s.withings.reconnect_required, s.garmin.reconnect_required].join('|');
  if (sig === bannerSig) return;
  bannerSig = sig;

  const host = qs('#banners');
  clear(host);

  if (s.garmin.reconnect_required) {
    host.appendChild(reconnectBanner(
      'Garmin needs signing in again',
      'Garmin invalidated the stored session, so nothing is being uploaded. Signing in once puts it right — measurements already waiting are kept and go up on the next run.',
      'Reconnect Garmin',
      function () { openGarminModal(refreshAll); }
    ));
  }
  if (s.withings.reconnect_required) {
    host.appendChild(reconnectBanner(
      'Withings needs authorizing again',
      'The Withings authorization expired or was revoked, so no new measurements are coming in. Approving access once restores it.',
      'Reconnect Withings',
      function () { openWithingsModal(refreshAll); }
    ));
  }
}

function reconnectBanner(title, body, actionLabel, onClick) {
  const btn = el('button', { class: 'btn btn-danger-solid btn-sm', type: 'button', text: actionLabel });
  btn.addEventListener('click', onClick);
  return el('div', { class: 'banner banner-red' }, [
    icon('alert', 'banner-icon'),
    el('div', { class: 'grow stack-xs' }, [
      el('div', { class: 'strong', text: title }),
      el('div', { text: body }),
      el('div', { class: 'row' }, btn),
    ]),
  ]);
}

function renderConnections(s) {
  const sig = [
    s.withings.creds_set, s.withings.connected, s.withings.reconnect_required, s.withings.user_id,
    s.garmin.connected, s.garmin.reconnect_required, s.garmin.email,
  ].join('|');
  if (sig === connSig) return;
  connSig = sig;

  /* Withings */
  const w = s.withings;
  qs('#withings-ident').textContent = w.user_id ? 'Withings user #' + w.user_id : (w.creds_set ? 'Credentials saved, not authorized' : 'Not set up yet');
  setPill(qs('#withings-pill'), connState(w, w.creds_set));
  const wActions = qs('#withings-actions');
  clear(wActions);
  if (!w.creds_set) {
    wActions.appendChild(el('a', { class: 'btn btn-accent btn-sm', href: '/setup' }, ['Finish setup', icon('arrow')]));
  } else if (!w.connected || w.reconnect_required) {
    wActions.appendChild(actionBtn(w.reconnect_required ? 'Reconnect' : 'Connect', 'btn-accent', function () { openWithingsModal(refreshAll); }));
  }
  if (w.connected || w.reconnect_required) {
    wActions.appendChild(actionBtn('Disconnect', 'btn-danger', disconnectWithings));
  }

  /* Garmin */
  const g = s.garmin;
  qs('#garmin-ident').textContent = g.email || 'No account linked';
  setPill(qs('#garmin-pill'), connState(g, true));
  const gActions = qs('#garmin-actions');
  clear(gActions);
  if (!g.connected || g.reconnect_required) {
    gActions.appendChild(actionBtn(g.reconnect_required ? 'Reconnect' : 'Connect', 'btn-accent', function () { openGarminModal(refreshAll); }));
  }
  if (g.connected || g.reconnect_required) {
    gActions.appendChild(actionBtn('Disconnect', 'btn-danger', disconnectGarmin));
  }
}

function connState(conn, configured) {
  if (!configured) return ['gray', 'Not set up'];
  if (conn.reconnect_required) return ['red', 'Reconnect needed'];
  if (conn.connected) return ['green', 'Connected'];
  return ['amber', 'Not connected'];
}

function setPill(host, state) {
  clear(host);
  host.appendChild(statusPill(state[0], state[1]));
}

function actionBtn(label, kind, onClick) {
  const btn = el('button', { class: 'btn btn-sm ' + kind, type: 'button', text: label });
  btn.addEventListener('click', onClick);
  return btn;
}

async function disconnectWithings() {
  const ok = await confirmModal({
    title: 'Disconnect Withings?',
    message: 'Stored Withings tokens are cleared, so no new measurements arrive until you authorize again. Your Client ID and Secret stay saved and nothing already synced is removed.',
    confirmText: 'Disconnect', danger: true,
  });
  if (!ok) return;
  try {
    await api('/api/withings/disconnect', { method: 'POST' });
    toast('Withings disconnected', 'ok');
    connSig = '';
    refreshAll();
  } catch (err) { toast(err.message, 'err'); }
}

async function disconnectGarmin() {
  const ok = await confirmModal({
    title: 'Disconnect Garmin?',
    message: 'Stored Garmin tokens are cleared, so nothing is uploaded until you sign in again. Measurements already in Garmin Connect are untouched.',
    confirmText: 'Disconnect', danger: true,
  });
  if (!ok) return;
  try {
    await api('/api/garmin/disconnect', { method: 'POST' });
    toast('Garmin disconnected', 'ok');
    connSig = '';
    refreshAll();
  } catch (err) { toast(err.message, 'err'); }
}

/* ── sync card ── */
function renderSync(s) {
  const sync = s.sync || {};
  const pill = qs('#sync-pill');
  clear(pill);
  if (sync.running) pill.appendChild(el('span', { class: 'pill pill-blue' }, [el('span', { class: 'dot pulse' }), 'Syncing']));
  else if (sync.last_error) pill.appendChild(statusPill('red', 'Last run failed'));
  else if (sync.last_at) pill.appendChild(statusPill('green', 'Healthy'));
  else pill.appendChild(statusPill('gray', 'Not run yet'));

  const last = qs('#sync-last');
  last.textContent = relTime(sync.last_at);
  last.setAttribute('title', absTime(sync.last_at));

  const result = qs('#sync-result');
  clear(result);
  if (sync.last_error) {
    result.appendChild(el('span', { class: 'hint-err', text: sync.last_error }));
  } else if (sync.last_at) {
    const fetched = sync.last_fetched || 0;
    const uploaded = sync.last_uploaded || 0;
    result.textContent = fetched === 0 && uploaded === 0
      ? 'Nothing new'
      : plural(fetched, 'measurement') + ' fetched, ' + uploaded + ' uploaded';
  } else {
    result.textContent = '—';
  }

  const next = qs('#sync-next');
  next.textContent = sync.next_at ? relTime(sync.next_at) : 'not scheduled';
  next.setAttribute('title', sync.next_at ? absTime(sync.next_at) : '');

  qs('#sync-interval').textContent = intervalLabel(sync.interval_minutes);

  /* backfill progress */
  const bf = s.backfill || {};
  const block = qs('#backfill-block');
  if (bf.running) {
    const total = bf.total || 0;
    const done = bf.done || 0;
    qs('#backfill-label').textContent = total
      ? 'Backfilling history — ' + done + ' of ' + total
      : 'Backfilling history…';
    qs('#backfill-fill').style.width = (total ? Math.min(100, Math.round((done / total) * 100)) : 5) + '%';
    show(block, true);
  } else {
    show(block, false);
  }

  /* the Sync now button follows the server, not the click */
  const btn = qs('#sync-now');
  if (sync.running && !btn.busyOn) { setBusy(btn, true, 'Syncing…'); btn.busyOn = true; }
  else if (!sync.running && btn.busyOn) { setBusy(btn, false); btn.busyOn = false; }

  qs('#sync-note').textContent = sync.running
    ? 'Running now.'
    : (sync.next_at ? 'Next automatic run ' + relTime(sync.next_at) + '.' : '');
}

function intervalLabel(minutes) {
  const m = Number(minutes);
  if (!m) return '—';
  if (m % 60 === 0) return plural(m / 60, 'hour');
  return plural(m, 'minute');
}

async function onSyncNow() {
  const btn = qs('#sync-now');
  setBusy(btn, true, 'Syncing…');
  btn.busyOn = true;
  try {
    await api('/api/sync/now', { method: 'POST' });
    toast('Sync started', 'ok');
  } catch (err) {
    if (err.code === 'already_running') toast('A sync is already running', 'warn');
    else { toast(err.detail || 'Could not start a sync: ' + err.code, 'err'); }
  }
  loadStatus();
}

/* ── measurements ── */
async function loadMeasurements() {
  let data;
  try {
    data = await api('/api/measurements?limit=50');
  } catch (err) {
    return;
  }
  const items = data.items || [];
  const body = qs('#measurements-body');
  clear(body);
  show(qs('#measurements-empty'), items.length === 0);
  qs('#measurements-count').textContent = items.length ? plural(items.length, 'row') : '';

  for (const m of items) {
    const failed = !m.synced && m.sync_error;
    const tr = el('tr', failed ? { title: m.sync_error } : {}, [
      el('td', { text: absTime(m.measured_at) }),
      el('td', { class: 'num', text: num(m.weight_kg, 1, ' kg') }),
      el('td', { class: 'num col-tertiary', text: num(m.body_fat_pct, 1, '%') }),
      el('td', { class: 'num col-secondary', text: num(m.muscle_kg, 1, ' kg') }),
      el('td', {}, syncedPill(m)),
    ]);
    body.appendChild(tr);
  }
}

function syncedPill(m) {
  if (m.synced) return statusPill('green', 'Synced');
  if (m.sync_error) {
    const pill = statusPill('red', 'Failed');
    pill.setAttribute('title', m.sync_error);
    return pill;
  }
  return statusPill('amber', 'Pending');
}

/* ── activity log ── */
async function loadEvents() {
  let data;
  try {
    data = await api('/api/events?limit=' + eventLimit);
  } catch (err) {
    return;
  }
  const items = data.items || [];
  const list = qs('#log-list');
  clear(list);
  show(qs('#log-empty'), items.length === 0);

  for (const e of items) {
    list.appendChild(el('li', { class: 'lv-' + (e.level || 'info') }, [
      el('span', { class: 'log-dot' }),
      el('span', { class: 'log-at', text: clockTime(e.at), title: absTime(e.at) }),
      el('span', { class: 'log-msg grow', text: e.message || '' }),
    ]));
  }
}

function onToggleLogSize() {
  eventLimit = eventLimit === 20 ? 100 : 20;
  qs('#log-more').textContent = eventLimit === 20 ? 'Show more' : 'Show less';
  loadEvents();
}
