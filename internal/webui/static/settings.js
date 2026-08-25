/* settings.js — everything you can change after setup. Every change applies at once except the port. */

const MIN_PORT = 1024;
const MAX_PORT = 65535;

let settings = { interval_minutes: 15, port: 8723, update_check: false };
let setupState = { steps: {} };
let creds;

init();

async function init() {
  creds = createWithingsCredsForm({
    submitLabel: 'Save credentials',
    onSaved: function () { loadSetupState(); loadStatus(); },
  });
  qs('#creds-mount').appendChild(creds.node);

  qs('#interval-select').addEventListener('change', onIntervalChange);
  qs('#update-check').addEventListener('change', onUpdateToggle);
  qs('#update-now').addEventListener('click', onCheckNow);
  qs('#port-change').addEventListener('click', openPortModal);
  qsa('[data-backfill]').forEach(function (btn) {
    btn.addEventListener('click', function () { runBackfill(btn); });
  });

  show(qs('#app'), true);
  await Promise.all([loadSettings(), loadSetupState(), loadStatus()]);

  if (window.location.hash === '#withings') {
    qs('#creds-disclose').open = true;
    qs('#withings').scrollIntoView();
  }
}

/* ── loaders ── */
async function loadSettings() {
  try {
    settings = await api('/api/settings');
  } catch (err) {
    toast(err.message, 'err');
    return;
  }
  const select = qs('#interval-select');
  const wanted = String(settings.interval_minutes);
  if (!qs('option[value="' + wanted + '"]', select)) {
    select.appendChild(el('option', { value: wanted, text: intervalOptionLabel(settings.interval_minutes) }));
  }
  select.value = wanted;
  qs('#update-check').checked = !!settings.update_check;
  qs('#port-current').textContent = 'localhost:' + settings.port;
}

async function loadSetupState() {
  try {
    setupState = await api('/api/setup/state');
  } catch (err) {
    return;
  }
  const url = setupState.callback_url || (window.location.origin + '/callback');
  const holder = qs('#copy-callback');
  clear(holder);
  holder.appendChild(copyBox(url));
  creds.load(setupState);
}

async function loadStatus() {
  let s;
  try {
    s = await api('/api/status');
  } catch (err) {
    return;
  }
  s.withings = s.withings || {};
  s.garmin = s.garmin || {};

  qs('#version').textContent = 'scalebridge-sync ' + (s.version ? 'v' + s.version : '');
  qs('#about-version').textContent = s.version || '—';
  if (s.config_path) qs('#about-config').textContent = s.config_path;

  renderConnection(s.withings, s.withings.creds_set, {
    pill: '#withings-pill', ident: '#withings-ident', actions: '#withings-actions',
    identText: s.withings.user_id ? 'Connected as Withings user #' + s.withings.user_id + '.' : 'No Withings account is authorized yet.',
    connect: function () { openWithingsModal(reloadAll); },
    disconnect: disconnectWithings,
  });

  renderConnection(s.garmin, true, {
    pill: '#garmin-pill', ident: '#garmin-ident', actions: '#garmin-actions',
    identText: s.garmin.email ? 'Signed in as ' + s.garmin.email + '.' : 'No Garmin account is signed in.',
    connect: function () { openGarminModal(reloadAll); },
    disconnect: disconnectGarmin,
  });
}

function reloadAll() { loadStatus(); loadSetupState(); }

function renderConnection(conn, configured, o) {
  qs(o.ident).textContent = o.identText;

  const pill = qs(o.pill);
  clear(pill);
  if (!configured) pill.appendChild(statusPill('gray', 'Not set up'));
  else if (conn.reconnect_required) pill.appendChild(statusPill('red', 'Reconnect needed'));
  else if (conn.connected) pill.appendChild(statusPill('green', 'Connected'));
  else pill.appendChild(statusPill('amber', 'Not connected'));

  const actions = qs(o.actions);
  clear(actions);
  if (configured && (!conn.connected || conn.reconnect_required)) {
    const btn = el('button', { class: 'btn btn-accent btn-sm', type: 'button', text: conn.reconnect_required ? 'Reconnect' : 'Connect' });
    btn.addEventListener('click', o.connect);
    actions.appendChild(btn);
  }
  if (conn.connected || conn.reconnect_required) {
    const btn = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: 'Disconnect' });
    btn.addEventListener('click', o.disconnect);
    actions.appendChild(btn);
  }
}

/* ── schedule ── */
function intervalOptionLabel(minutes) {
  const m = Number(minutes);
  if (m % 60 === 0) return 'Every ' + (m / 60 === 1 ? 'hour' : m / 60 + ' hours');
  return 'Every ' + m + ' minutes';
}

async function onIntervalChange() {
  const select = qs('#interval-select');
  const minutes = Number(select.value);
  select.disabled = true;
  try {
    const res = await api('/api/settings', { method: 'PUT', body: { interval_minutes: minutes } });
    settings.interval_minutes = minutes;
    toast('Now checking ' + intervalOptionLabel(minutes).toLowerCase(), 'ok');
    if (res.restart_required) show(qs('#restart-banner'), true);
  } catch (err) {
    toast(err.detail || 'Could not save the schedule: ' + err.code, 'err');
    select.value = String(settings.interval_minutes);
  } finally {
    select.disabled = false;
  }
}

/* ── backfill ── */
async function runBackfill(btn) {
  const range = btn.dataset.backfill;
  setBusy(btn, true, 'Starting…');
  try {
    await api('/api/sync/backfill', { method: 'POST', body: { range: range } });
    toast('History import started — watch the dashboard for progress', 'ok');
    qs('#backfill-note').textContent = 'Import running. Progress shows on the dashboard.';
  } catch (err) {
    if (err.code === 'already_running') toast('A sync or import is already running', 'warn');
    else toast(err.detail || 'Could not start the import: ' + err.code, 'err');
  } finally {
    setBusy(btn, false);
  }
}

/* ── connections ── */
async function disconnectWithings() {
  const ok = await confirmModal({
    title: 'Disconnect Withings?',
    message: 'Stored Withings tokens are cleared, so no new measurements arrive until you authorize again. Your Client ID and Secret stay saved.',
    confirmText: 'Disconnect', danger: true,
  });
  if (!ok) return;
  try {
    await api('/api/withings/disconnect', { method: 'POST' });
    toast('Withings disconnected', 'ok');
    reloadAll();
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
    reloadAll();
  } catch (err) { toast(err.message, 'err'); }
}

/* ── updates ── */
async function onUpdateToggle() {
  const box = qs('#update-check');
  const on = box.checked;
  box.disabled = true;
  try {
    await api('/api/settings', { method: 'PUT', body: { update_check: on } });
    settings.update_check = on;
    toast(on ? 'Update checks on' : 'Update checks off', 'ok');
  } catch (err) {
    box.checked = !on;
    toast(err.detail || 'Could not save that: ' + err.code, 'err');
  } finally {
    box.disabled = false;
  }
}

async function onCheckNow() {
  const btn = qs('#update-now');
  const out = qs('#update-result');
  clear(out);
  setBusy(btn, true, 'Checking…');
  try {
    const res = await api('/api/update/check', { method: 'POST' });
    if (res.newer) {
      appendKids(out, [
        'Version ' + res.latest + ' is available. ',
        res.url ? el('a', { href: res.url, target: '_blank', rel: 'noopener noreferrer', text: 'Release notes' }) : null,
      ]);
    } else {
      out.textContent = 'You are on the latest version (' + (res.current || '—') + ').';
    }
  } catch (err) {
    out.textContent = err.code === 'github_unreachable'
      ? 'Could not reach GitHub. Nothing to worry about — try again later.'
      : (err.detail || 'Check failed: ' + err.code);
  } finally {
    setBusy(btn, false);
  }
}

/* ── port change: two steps, because Withings must know the new callback URL first ── */
function openPortModal() {
  const shell = modalShell('Change the port', 'Two steps — the portal first, then this app', { wide: true });

  const portInput = el('input', { class: 'input mono', type: 'number', min: String(MIN_PORT), max: String(MAX_PORT), step: '1' });
  portInput.value = String(settings.port);
  const urlHolder = el('div');
  const hint = el('div', { class: 'hint hidden' });

  function futureUrl() { return 'http://localhost:' + (portInput.value || '') + '/callback'; }
  function repaintUrl() {
    clear(urlHolder);
    urlHolder.appendChild(copyBox(futureUrl(), true));
  }
  portInput.addEventListener('input', function () { show(hint, false); repaintUrl(); });
  repaintUrl();

  const stepOne = el('div', { class: 'stack-sm' }, [
    el('div', { class: 'banner banner-amber' }, [
      icon('alert', 'banner-icon'),
      el('div', {}, [
        el('span', { class: 'strong', text: 'Withings must know the new address first. ' }),
        'Change the Callback URI on your Withings application to the URL below, save it there, and only then apply the change here. Doing it the other way round leaves the connection broken until the portal catches up.',
      ]),
    ]),
    el('label', { class: 'field' }, [el('span', { class: 'label', text: 'New port' }), portInput]),
    el('div', { class: 'stack-xs' }, [
      el('span', { class: 'label', text: 'The callback URL to register' }),
      urlHolder,
    ]),
    hint,
    el('div', { class: 'row row-wrap' }, [
      el('a', { class: 'btn btn-secondary btn-sm', href: WITHINGS_DASHBOARD, target: '_blank', rel: 'noopener noreferrer' }, ['Open the Withings portal', icon('external')]),
    ]),
  ]);

  const stepTwo = el('div', { class: 'stack-sm hidden' }, [
    el('p', { class: 'body-sm' }, [
      'Once your Withings application shows ',
      el('span', { class: 'mono strong', id: 'port-confirm-url' }),
      ' as its callback URL, apply the change. The app keeps serving on the current port until it restarts, so this page will not move under you.',
    ]),
    el('div', { class: 'banner banner-plain' }, el('div', { text: 'Nothing else changes: your credentials, tokens and history stay exactly as they are.' })),
  ]);

  appendKids(shell.body, [stepOne, stepTwo]);

  const cancel = el('button', { class: 'btn btn-secondary', type: 'button', text: 'Cancel' });
  const next = el('button', { class: 'btn btn-accent', type: 'button', text: 'Next' });
  const back = el('button', { class: 'btn btn-secondary hidden', type: 'button', text: 'Back' });
  const apply = el('button', { class: 'btn btn-danger-solid hidden', type: 'button', text: "I've updated the portal — apply" });
  appendKids(shell.foot, [cancel, back, next, apply]);

  const m = openModal(shell.box, { sticky: true });
  shell.closeBtn.addEventListener('click', m.close);
  cancel.addEventListener('click', m.close);

  next.addEventListener('click', function () {
    const port = Number(portInput.value);
    if (!Number.isInteger(port) || port < MIN_PORT || port > MAX_PORT) {
      hint.textContent = 'Pick a whole number between ' + MIN_PORT + ' and ' + MAX_PORT + '.';
      hint.className = 'hint hint-err';
      show(hint, true);
      portInput.focus();
      return;
    }
    if (port === settings.port) {
      hint.textContent = 'That is already the port in use.';
      hint.className = 'hint hint-warn';
      show(hint, true);
      return;
    }
    qs('#port-confirm-url', stepTwo).textContent = futureUrl();
    show(stepOne, false); show(stepTwo, true);
    show(next, false); show(cancel, false);
    show(back, true); show(apply, true);
  });

  back.addEventListener('click', function () {
    show(stepTwo, false); show(stepOne, true);
    show(apply, false); show(back, false);
    show(next, true); show(cancel, true);
  });

  apply.addEventListener('click', async function () {
    const port = Number(portInput.value);
    setBusy(apply, true, 'Applying…');
    try {
      const res = await api('/api/settings', { method: 'PUT', body: { port: port } });
      settings.port = port;
      qs('#port-current').textContent = 'localhost:' + port;
      m.close();
      if (res.restart_required !== false) show(qs('#restart-banner'), true);
      toast('Port saved — restart to use it', 'warn');
      loadSetupState();
    } catch (err) {
      toast(err.detail || 'Could not change the port: ' + err.code, 'err');
      setBusy(apply, false);
    }
  });
}
