/* withings-connect.js — consent happens in a second tab; this one polls /api/setup/state to see how it went. */

const WITHINGS_PORTAL = 'https://developer.withings.com/';
const WITHINGS_DASHBOARD = 'https://developer.withings.com/dashboard/';
// Nothing back after this long is almost always a mismatched redirect URL, so say so.
const OAUTH_SLOW_MS = 45000;

function createWithingsConnect(opts) {
  opts = opts || {};
  const onConnected = opts.onConnected || function () {};
  const connectLabel = opts.connectLabel || 'Connect Withings';

  let poller = null;
  let callbackUrl = '';
  let attemptAt = 0;          // when the user last clicked Connect
  let shownError = '';        // last_oauth_error value we have already surfaced
  let slowShown = false;
  let finished = false;

  /* ── pieces ── */
  const connectBtn = el('a', {
    class: 'btn btn-accent btn-lg', href: '/api/withings/connect',
    target: '_blank', rel: 'noopener',
  }, [connectLabel, icon('external')]);

  const startRow = el('div', { class: 'row row-wrap' }, connectBtn);

  const reopenBtn = el('a', {
    class: 'btn btn-ghost btn-sm', href: '/api/withings/connect',
    target: '_blank', rel: 'noopener', text: "Didn't open? Try the link again",
  });

  const waitBlock = el('div', { class: 'wait-block hidden' }, [
    el('span', { class: 'spinner spinner-lg' }),
    el('div', { class: 'stack-xs' }, [
      el('div', { class: 'title', text: 'Waiting for Withings…' }),
      el('div', { class: 'body-sm', text: 'A new tab opened. Sign in with your normal Withings account and approve access — then come back to this tab. It updates on its own.' }),
      reopenBtn,
    ]),
  ]);

  const okBlock = el('div', { class: 'ok-block hidden' }, [icon('check'), el('span', { class: 'grow' })]);

  const errBanner = el('div', { class: 'banner banner-red hidden' });
  const errActions = el('div', { class: 'row row-wrap hidden' });

  /* ── troubleshooting ── */
  const troubleUrl = el('div');
  const retryLink = el('a', { class: 'btn btn-accent btn-sm', href: '/api/withings/connect', target: '_blank', rel: 'noopener' }, ['Try again', icon('external')]);

  const troubleItem1 = el('details', { class: 'disclose' }, [
    el('summary', { text: "Withings sent me back with a redirect / callback URL error" }),
    el('div', { class: 'disclose-body stack-sm' }, [
      el('p', { text: 'This is the one that catches almost everyone. Withings only redirects back to the exact string registered on your developer application — one character off and it refuses. Open your application in the portal and compare it, character for character, with this:' }),
      troubleUrl,
      el('div', { class: 'row row-wrap' }, [
        el('a', { class: 'btn btn-secondary btn-sm', href: WITHINGS_DASHBOARD, target: '_blank', rel: 'noopener noreferrer' }, ['Open the Withings portal', icon('external')]),
        retryLink,
      ]),
    ]),
  ]);

  const troubleItem2 = el('details', { class: 'disclose' }, [
    el('summary', { text: 'No new tab opened at all' }),
    el('div', { class: 'disclose-body', text: 'Your browser probably blocked the pop-up. Look for a blocked-pop-up icon in the address bar and allow it, then use the button again. The link is an ordinary link — right-click and "open in new tab" works too.' }),
  ]);

  const troubleItem3 = el('details', { class: 'disclose' }, [
    el('summary', { text: 'The other tab shows an error page' }),
    el('div', { class: 'disclose-body', text: 'Read the message in that tab — it is coming from this app and says which part of the exchange failed. Then close it and try again here. Nothing was saved, so retrying is free.' }),
  ]);

  const troubleTitle = el('div', { class: 'title' });
  const troubleLede = el('p', { class: 'body-sm' });
  const troubleCard = el('div', { class: 'card card-pad stack-sm hidden' }, [
    troubleTitle, troubleLede, troubleItem1, troubleItem2, troubleItem3,
  ]);

  function showTrouble(afterError) {
    troubleTitle.textContent = afterError ? 'Still stuck?' : 'Nothing came back yet';
    troubleLede.textContent = afterError
      ? 'These are the three things that usually explain it.'
      : 'Withings normally returns within a few seconds. When it does not, it is almost always one of these three.';
    show(troubleCard, true);
  }

  const node = el('div', { class: 'stack-sm' }, [
    startRow, waitBlock, okBlock, errBanner, errActions, troubleCard,
  ]);

  /* ── behaviour ── */
  connectBtn.addEventListener('click', beginAttempt);
  reopenBtn.addEventListener('click', beginAttempt);
  retryLink.addEventListener('click', beginAttempt);

  function beginAttempt() {
    attemptAt = Date.now();
    slowShown = false;
    finished = false;
    // Forget the last error too, or the same failure twice running looks like silence.
    shownError = '';
    hideAll();
    show(waitBlock, true);
    if (poller) poller.kick();
  }

  function hideAll() {
    show(errBanner, false);
    show(errActions, false);
    show(troubleCard, false);
    show(okBlock, false);
    clear(errActions);
  }

  async function tick() {
    let state;
    try {
      state = await api('/api/setup/state');
    } catch (e) {
      return; // transient; the next tick will pick it up
    }

    if (state.callback_url && state.callback_url !== callbackUrl) {
      callbackUrl = state.callback_url;
      clear(troubleUrl);
      troubleUrl.appendChild(copyBox(callbackUrl, true));
    }

    if (finished) return;

    if (state.steps && state.steps.withings_oauth) {
      finished = true;
      await succeed();
      return;
    }

    const oauthErr = state.last_oauth_error || '';
    const oauthDetail = state.last_oauth_detail || state.detail || '';
    if (oauthErr && oauthErr !== shownError) {
      shownError = oauthErr;
      failWith(oauthErr, oauthDetail);
      return;
    }

    if (attemptAt && !slowShown && Date.now() - attemptAt > OAUTH_SLOW_MS) {
      slowShown = true;
      show(waitBlock, false);
      if (oauthErr) failWith(oauthErr, oauthDetail);
      else { showTrouble(false); troubleItem1.open = true; }
    }
  }

  async function succeed() {
    hideAll();
    show(waitBlock, false);
    let who = '';
    try {
      const status = await api('/api/status');
      if (status.withings && status.withings.user_id) who = String(status.withings.user_id);
    } catch (e) { /* the identity is a nicety, not a requirement */ }
    const label = who ? 'Connected as Withings user #' + who : 'Connected to Withings';
    clear(okBlock);
    appendKids(okBlock, [icon('check'), el('span', { class: 'grow', text: label })]);
    show(okBlock, true);
    show(startRow, false);
    setTimeout(function () { onConnected(who); }, 1500);
  }

  function failWith(code, detail) {
    show(waitBlock, false);
    show(troubleCard, false);
    clear(errActions);

    let msg, actions = [];
    if (code === 'exchange_failed') {
      msg = 'Withings rejected the exchange — this usually means the Client Secret is wrong. Copy it from the portal again (it is easy to grab a stray space or the Client ID by mistake).';
      const fix = el('button', { class: 'btn btn-accent', type: 'button', text: 'Re-enter the Client Secret' });
      fix.addEventListener('click', function () {
        if (opts.onFixCredentials) opts.onFixCredentials();
        else window.location.href = '/setup#step-2';
      });
      actions.push(fix);
    } else if (code === 'denied') {
      msg = 'The connection was cancelled — Withings reported that access was not granted. Nothing was saved.';
    } else if (code === 'state_mismatch') {
      msg = 'That authorization link expired (they are good for ten minutes and can only be used once). Start a fresh one.';
    } else {
      msg = 'Withings returned an error we did not expect' + (detail ? ': ' + detail : ' (' + code + ')') + '.';
    }
    if (detail && code === 'exchange_failed') msg += ' Withings said: ' + detail;

    clear(errBanner);
    appendKids(errBanner, [icon('alert', 'banner-icon'), el('div', { text: msg })]);
    show(errBanner, true);

    const again = el('a', { class: 'btn btn-secondary', href: '/api/withings/connect', target: '_blank', rel: 'noopener' }, ['Try again', icon('external')]);
    again.addEventListener('click', beginAttempt);
    actions.push(again);
    appendKids(errActions, actions);
    show(errActions, true);

    showTrouble(true);
    troubleItem1.open = code !== 'exchange_failed';
    show(startRow, false);
  }

  return {
    node: node,
    start: function () {
      if (poller) return;
      poller = poll(tick, 2000);
    },
    stop: function () {
      if (poller) { poller.stop(); poller = null; }
    },
    reset: function () {
      finished = false;
      attemptAt = 0;
      slowShown = false;
      hideAll();
      show(waitBlock, false);
      show(startRow, true);
    },
  };
}

/* ── credentials form, shared by wizard sub-step 2d and Settings ── */
/* The stored secret is never readable back, so every save has to send a freshly pasted one. */
const HEX64 = /^[0-9a-f]{64}$/;

function createWithingsCredsForm(opts) {
  opts = opts || {};
  const onSaved = opts.onSaved || function () {};

  let secretStored = false;
  let replacing = false;
  let softOverride = false;

  const idInput = el('input', {
    class: 'input mono', type: 'text', spellcheck: 'false', autocomplete: 'off',
    autocapitalize: 'off', placeholder: '64 characters, letters and digits',
  });
  const secretInput = el('input', {
    class: 'input mono', type: 'password', spellcheck: 'false', autocomplete: 'off',
    autocapitalize: 'off', placeholder: 'Paste the Client Secret',
  });

  const eyeBtn = el('button', { class: 'eye', type: 'button', 'aria-label': 'Show the Client Secret' }, icon('eye'));
  eyeBtn.addEventListener('click', function () {
    const showing = secretInput.type === 'text';
    secretInput.type = showing ? 'password' : 'text';
    clear(eyeBtn);
    eyeBtn.appendChild(icon(showing ? 'eye' : 'eyeOff'));
    eyeBtn.setAttribute('aria-label', showing ? 'Show the Client Secret' : 'Hide the Client Secret');
  });

  const savedRow = el('div', { class: 'row hidden' }, [
    el('span', { class: 'pill pill-green' }, [el('span', { class: 'dot' }), 'Secret saved']),
    el('span', { class: 'caption grow', text: 'Stored on this computer and never shown again.' }),
  ]);
  const replaceBtn = el('button', { class: 'btn btn-secondary btn-sm hidden', type: 'button', text: 'Replace' });
  savedRow.appendChild(replaceBtn);

  const secretField = el('label', { class: 'field' }, [
    el('span', { class: 'label', text: 'Client Secret' }),
    el('div', { class: 'input-group' }, [secretInput, eyeBtn]),
  ]);

  replaceBtn.addEventListener('click', function () {
    replacing = true;
    show(savedRow, false);
    show(secretField, true);
    secretInput.value = '';
    secretInput.focus();
  });

  const hint = el('div', { class: 'hint hidden' });
  const useAnywayBtn = el('button', { class: 'btn btn-secondary btn-sm hidden', type: 'button', text: 'Use anyway' });
  useAnywayBtn.addEventListener('click', function () { softOverride = true; submit(); });

  const warnNote = el('div', { class: 'banner banner-amber hidden' });
  const saveBtn = el('button', { class: 'btn btn-accent', type: 'submit', text: opts.submitLabel || 'Save credentials' });

  const form = el('form', { class: 'stack-sm', novalidate: true }, [
    el('label', { class: 'field' }, [el('span', { class: 'label', text: 'Client ID' }), idInput]),
    savedRow,
    secretField,
    hint,
    warnNote,
    el('div', { class: 'row row-wrap' }, [saveBtn, useAnywayBtn]),
  ]);

  /* Values pasted from the portal routinely carry a leading or trailing space. */
  function trimmer(input) {
    input.addEventListener('paste', function () { setTimeout(function () { input.value = input.value.trim(); }, 0); });
    input.addEventListener('blur', function () { input.value = input.value.trim(); });
    input.addEventListener('input', function () { clearProblems(); });
  }
  trimmer(idInput);
  trimmer(secretInput);

  function clearProblems() {
    show(hint, false);
    show(useAnywayBtn, false);
    show(warnNote, false);
    idInput.classList.remove('input-err', 'input-warn');
    secretInput.classList.remove('input-err', 'input-warn');
  }

  function problem(text, isErr, offenders) {
    clear(hint);
    hint.textContent = text;
    hint.className = 'hint ' + (isErr ? 'hint-err' : 'hint-warn');
    show(hint, true);
    for (const o of offenders || []) o.classList.add(isErr ? 'input-err' : 'input-warn');
  }

  form.addEventListener('submit', function (e) { e.preventDefault(); submit(); });

  async function submit() {
    const clientId = idInput.value.trim();
    const clientSecret = secretInput.value.trim();
    clearProblems();

    if (!clientId) { problem('Paste the Client ID from your Withings application.', true, [idInput]); idInput.focus(); return; }
    if (secretStored && !replacing) {
      problem('Paste the Client Secret too. This app never reads it back out of storage, so a save has to send the whole pair.', true, [secretInput]);
      show(savedRow, false); show(secretField, true); replacing = true; secretInput.focus();
      return;
    }
    if (!clientSecret) { problem('Paste the Client Secret from your Withings application.', true, [secretInput]); secretInput.focus(); return; }
    if (clientId === clientSecret) {
      problem('You pasted the same value twice — the Client ID and Client Secret are two different strings on the portal page.', true, [idInput, secretInput]);
      return;
    }
    if (!softOverride) {
      const bad = [];
      if (!HEX64.test(clientId)) bad.push(idInput);
      if (!HEX64.test(clientSecret)) bad.push(secretInput);
      if (bad.length) {
        const subject = bad.length === 2
          ? 'The Client ID and Client Secret do not look like Withings keys'
          : 'The Client ' + (bad[0] === idInput ? 'ID does' : 'Secret does') + ' not look like a Withings key';
        problem(subject + ' — they are normally 64 lowercase letters and digits. Check you copied the whole string, with nothing extra on either end.', false, bad);
        show(useAnywayBtn, true);
        return;
      }
    }

    setBusy(saveBtn, true, 'Saving…');
    try {
      await api('/api/setup/withings-credentials', {
        method: 'PUT',
        body: { client_id: clientId, client_secret: clientSecret },
      });
      secretStored = true;
      replacing = false;
      softOverride = false;
      secretInput.value = '';
      show(secretField, false);
      show(savedRow, true);
      show(replaceBtn, true);
      toast('Withings credentials saved', 'ok');
      onSaved(clientId);
    } catch (err) {
      problem(err.detail || 'Could not save: ' + err.code, true, []);
    } finally {
      setBusy(saveBtn, false);
    }
  }

  /* Populates the form from an /api/setup/state payload. */
  function load(state) {
    if (state.client_id) idInput.value = state.client_id;
    secretStored = !!state.client_secret_set;
    replacing = false;
    softOverride = false;
    clearProblems();
    show(savedRow, secretStored);
    show(replaceBtn, secretStored);
    show(secretField, !secretStored);
    if (secretStored && opts.replaceWarning) {
      clear(warnNote);
      appendKids(warnNote, [icon('alert', 'banner-icon'), el('div', { text: opts.replaceWarning })]);
      show(warnNote, true);
    }
  }

  return { node: form, load: load, focus: function () { idInput.focus(); } };
}

/* The same flow in a modal, for the dashboard and Settings. */
function openWithingsModal(onDone) {
  const shell = modalShell('Reconnect Withings', 'Approval happens in a second tab');
  const flow = createWithingsConnect({
    connectLabel: 'Open Withings',
    onConnected: function () {
      m.close();
      toast('Withings reconnected', 'ok');
      if (onDone) onDone();
    },
    onFixCredentials: function () {
      m.close();
      window.location.href = '/settings#withings';
    },
  });
  appendKids(shell.body, [
    el('p', { class: 'body-sm', text: 'Withings needs your approval again. The consent page opens in a new tab; leave this window open and it will notice when you are done.' }),
    flow.node,
  ]);
  const m = openModal(shell.box, { onClose: function () { flow.stop(); } });
  shell.closeBtn.addEventListener('click', m.close);
  flow.start();
  return m;
}
