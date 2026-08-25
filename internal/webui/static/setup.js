/* setup.js — the six-step wizard. Steps toggle on location.hash (Back works for free); /api/setup/state picks the resume step. */

const STEP_COUNT = 6;
const APP_NAME_SUGGESTION = 'ScaleBridge Sync (personal)';
const APP_DESC_SUGGESTION = 'Personal sync of my own Withings measurements to Garmin Connect.';
const INSTALL_COMMAND = 'scalebridge-sync install';

let setupState = { steps: {} };
let finished = false;          // setup completed during this visit
let subDone = {};              // which of the 2a–2e sub-steps the user ticked off

let withings, garmin, creds;
let wConnectedPanel, gConnectedPanel;

/* ── boot ── */
init();

async function init() {
  buildStaticCopyBoxes();
  buildComponents();

  try {
    setupState = await api('/api/setup/state');
  } catch (err) {
    toast(err.message, 'err');
  }
  applyState();

  show(qs('#wizard'), true);
  if (!parseHash()) {
    history.replaceState(null, '', '#step-' + resumeStep());
  }
  window.addEventListener('hashchange', render);
  render();
}

function buildStaticCopyBoxes() {
  qs('#copy-app-name').appendChild(copyBox(APP_NAME_SUGGESTION));
  qs('#copy-app-desc').appendChild(copyBox(APP_DESC_SUGGESTION));
  qs('#copy-origin').appendChild(copyBox(window.location.origin));
  qs('#copy-install').appendChild(copyBox(INSTALL_COMMAND));
}

function buildComponents() {
  creds = createWithingsCredsForm({
    submitLabel: 'Save and continue',
    onSaved: function () {
      markSubDone('d');
      openSub('e');
      refreshState();
    },
  });
  qs('#creds-mount').appendChild(creds.node);

  withings = createWithingsConnect({
    onConnected: function () { goTo(4); },
    onFixCredentials: function () { goTo(2); openSub('d'); },
  });
  wConnectedPanel = buildWithingsConnected();
  const wMount = qs('#withings-mount');
  wMount.appendChild(wConnectedPanel);
  wMount.appendChild(withings.node);

  garmin = createGarminFlow({
    onComplete: async function () {
      toast('Garmin connected', 'ok');
      await refreshState();
      goTo(5);
    },
  });
  gConnectedPanel = buildGarminConnected();
  const gMount = qs('#garmin-mount');
  gMount.appendChild(gConnectedPanel);
  gMount.appendChild(garmin.node);

  qs('#backfill-next').addEventListener('click', submitBackfill);
  qs('#finish-btn').addEventListener('click', submitComplete);
  document.addEventListener('click', onDelegatedClick);
}

/* ── navigation ── */
function parseHash() {
  const m = /^#step-([1-6])$/.exec(window.location.hash);
  return m ? Number(m[1]) : 0;
}

function currentStep() { return parseHash() || 1; }

function goTo(n) {
  const target = '#step-' + Math.min(STEP_COUNT, Math.max(1, n));
  if (window.location.hash === target) render();
  else window.location.hash = target;
}

function resumeStep() {
  const st = setupState.steps || {};
  if (!st.withings_creds) return setupState.client_id ? 2 : 1;
  if (!st.withings_oauth) return 3;
  if (!st.garmin) return 4;
  if (!st.backfill) return 5;
  return 6;
}

async function render() {
  const n = currentStep();
  for (let i = 1; i <= STEP_COUNT; i++) {
    qs('#step-' + i).classList.toggle('active', i === n);
  }
  updateRail(n);
  window.scrollTo(0, 0);
  if (n !== 3) withings.stop();
  if (n === 2) openInitialSub();

  await refreshState();
  if (currentStep() !== n) return;   // navigated away while the state was in flight
  if (n === 3) syncWithingsView();
  if (n === 4) syncGarminView();
}

function updateRail(current) {
  const st = setupState.steps || {};
  const doneFlags = [
    current > 1 || !!st.withings_creds,
    !!st.withings_creds,
    !!st.withings_oauth,
    !!st.garmin,
    !!st.backfill,
    finished,
  ];
  qsa('#rail li').forEach(function (li, idx) {
    const n = idx + 1;
    const isDone = doneFlags[idx] && n !== current;
    li.classList.toggle('done', isDone);
    li.classList.toggle('current', n === current);
    const mark = qs('.rail-mark', li);
    clear(mark);
    mark.appendChild(isDone ? icon('check') : document.createTextNode(String(n)));
  });
}

async function refreshState() {
  try {
    setupState = await api('/api/setup/state');
  } catch (err) {
    return;
  }
  applyDynamicText();
  updateRail(currentStep());
}

/* ── state → DOM ── */
function applyState() {
  applyDynamicText();
  creds.load(setupState);
  if (setupState.steps && setupState.steps.withings_creds) {
    markSubDone('a'); markSubDone('b'); markSubDone('c'); markSubDone('d');
  }
}

function applyDynamicText() {
  const url = setupState.callback_url || (window.location.origin + '/callback');
  const port = setupState.port || window.location.port || '8723';

  const holder = qs('#copy-callback');
  if (holder.dataset.url !== url) {
    holder.dataset.url = url;
    clear(holder);
    holder.appendChild(copyBox(url, true));
  }
  qs('#cb-port').textContent = ':' + port;
  qs('#mock-callback').textContent = url;
}

/* ── step 2: accordion ── */
function onDelegatedClick(e) {
  const target = e.target;
  if (!target || !target.closest) return;

  const jump = target.closest('[data-goto]');
  if (jump) { goTo(Number(jump.dataset.goto)); return; }

  const next = target.closest('[data-sub-next]');
  if (next) {
    const item = next.closest('.acc-item');
    if (item) markSubDone(item.dataset.sub);
    openSub(next.dataset.subNext);
    return;
  }

  const head = target.closest('.acc-head');
  if (head) {
    const item = head.closest('.acc-item');
    openSub(item.classList.contains('open') ? null : item.dataset.sub);
  }
}

function openSub(sub) {
  qsa('#acc .acc-item').forEach(function (item) {
    item.classList.toggle('open', item.dataset.sub === sub);
  });
  if (sub === 'd') creds.focus();
}

function markSubDone(sub) {
  subDone[sub] = true;
  const item = qs('#acc .acc-item[data-sub="' + sub + '"]');
  if (item) {
    item.classList.add('done');
    const badge = qs('.acc-num', item);
    clear(badge);
    badge.appendChild(icon('check'));
  }
}

function openInitialSub() {
  if (qs('#acc .acc-item.open')) return;   // the user already picked one
  const order = ['a', 'b', 'c', 'd', 'e'];
  for (const sub of order) {
    if (!subDone[sub]) { openSub(sub); return; }
  }
  openSub('e');
}

/* ── step 3: Withings ── */
function buildWithingsConnected() {
  const label = el('span', { class: 'grow', text: 'Withings is connected.' });
  const cont = el('button', { class: 'btn btn-accent', type: 'button' }, ['Continue', icon('arrow')]);
  cont.addEventListener('click', function () { goTo(4); });
  const swap = el('button', { class: 'btn btn-ghost', type: 'button', text: 'Use a different Withings account' });
  swap.addEventListener('click', async function () {
    const ok = await confirmModal({
      title: 'Disconnect Withings?',
      message: 'This clears the stored Withings tokens so you can approve a different account. Your Client ID and Secret are kept, and no measurements are deleted.',
      confirmText: 'Disconnect',
      danger: true,
    });
    if (!ok) return;
    try {
      await api('/api/withings/disconnect', { method: 'POST' });
      withings.reset();
      await refreshState();
      syncWithingsView();
    } catch (err) { toast(err.message, 'err'); }
  });

  return el('div', { class: 'stack-sm hidden' }, [
    el('div', { class: 'ok-block' }, [icon('check'), label]),
    el('div', { class: 'row row-wrap' }, [cont, swap]),
  ]);
}

async function syncWithingsView() {
  const connected = !!(setupState.steps && setupState.steps.withings_oauth);
  show(wConnectedPanel, connected);
  show(withings.node, !connected);
  if (connected) {
    withings.stop();
    const label = qs('.ok-block .grow', wConnectedPanel);
    try {
      const status = await api('/api/status');
      if (status.withings && status.withings.user_id) {
        label.textContent = 'Connected as Withings user #' + status.withings.user_id + '.';
      }
    } catch (err) { /* the identity line is a nicety */ }
  } else {
    withings.start();
  }
}

/* ── step 4: Garmin ── */
function buildGarminConnected() {
  const label = el('span', { class: 'grow', text: 'Garmin is connected.' });
  const cont = el('button', { class: 'btn btn-accent', type: 'button' }, ['Continue', icon('arrow')]);
  cont.addEventListener('click', function () { goTo(5); });
  const swap = el('button', { class: 'btn btn-ghost', type: 'button', text: 'Sign in as someone else' });

  const panel = el('div', { class: 'stack-sm hidden' }, [
    el('div', { class: 'ok-block' }, [icon('check'), label]),
    el('div', { class: 'row row-wrap' }, [cont, swap]),
  ]);
  swap.addEventListener('click', function () {
    garmin.reset();
    show(panel, false);
    show(garmin.node, true);
    garmin.focus();
  });
  return panel;
}

async function syncGarminView() {
  const connected = !!(setupState.steps && setupState.steps.garmin);
  show(gConnectedPanel, connected);
  show(garmin.node, !connected);
  if (!connected) { garmin.focus(); return; }
  const label = qs('.ok-block .grow', gConnectedPanel);
  try {
    const status = await api('/api/status');
    if (status.garmin && status.garmin.email) {
      label.textContent = 'Connected as ' + status.garmin.email + '.';
    }
  } catch (err) { /* nicety */ }
}

/* ── step 5: backfill ── */
async function submitBackfill() {
  const btn = qs('#backfill-next');
  const picked = qs('#backfill-form input[name="backfill"]:checked');
  const choice = picked ? picked.value : '30d';
  setBusy(btn, true, 'Saving…');
  try {
    await api('/api/setup/backfill', { method: 'POST', body: { choice: choice } });
    await refreshState();
    goTo(6);
  } catch (err) {
    toast(err.detail || 'Could not save that choice: ' + err.code, 'err');
  } finally {
    setBusy(btn, false);
  }
}

/* ── step 6: schedule + done ── */
async function submitComplete() {
  const btn = qs('#finish-btn');
  const picked = qs('#schedule-form input[name="interval"]:checked');
  const minutes = Number(picked ? picked.value : 15);
  setBusy(btn, true, 'Starting…');
  try {
    await api('/api/setup/complete', { method: 'POST', body: { interval_minutes: minutes } });
    finished = true;
    show(qs('#schedule-panel'), false);
    show(qs('#done-panel'), true);
    updateRail(6);
    window.scrollTo(0, 0);
  } catch (err) {
    toast(err.detail || 'Could not finish setup: ' + err.code, 'err');
  } finally {
    setBusy(btn, false);
  }
}
