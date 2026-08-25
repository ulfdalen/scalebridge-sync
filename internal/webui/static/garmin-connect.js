/* garmin-connect.js — Garmin sign-in + MFA, used inline by wizard step 4 and in a modal elsewhere. */

const GARMIN_ISSUES_URL = 'https://github.com/ulfdalen/scalebridge-sync/issues';

function createGarminFlow(opts) {
  opts = opts || {};
  const onComplete = opts.onComplete || function () {};

  let mfaToken = '';
  let mfaMethod = 'email';
  let busy = false;

  /* ── step 1: credentials ── */
  const emailInput = el('input', { class: 'input', type: 'email', id: 'gc-email', autocomplete: 'username', required: true, placeholder: 'you@example.com' });
  const passInput = el('input', { class: 'input', type: 'password', id: 'gc-password', autocomplete: 'current-password', required: true });
  const loginError = el('div', { class: 'banner banner-red hidden' });
  const loginBtn = el('button', { class: 'btn btn-accent btn-block btn-lg', type: 'submit' }, ['Sign in to Garmin', icon('arrow')]);

  const loginForm = el('form', { class: 'stack-sm', novalidate: true }, [
    el('div', { class: 'field' }, [el('label', { class: 'label', for: 'gc-email', text: 'Garmin Connect email' }), emailInput]),
    el('div', { class: 'field' }, [el('label', { class: 'label', for: 'gc-password', text: 'Password' }), passInput]),
    loginError,
    el('div', {}, loginBtn),
    el('p', { class: 'caption row row-top' }, [
      icon('shield'),
      el('span', { text: 'Your password goes directly from this app to Garmin and is never written to disk — only the resulting session tokens are stored, on this computer.' }),
    ]),
  ]);

  /* ── step 2: six-digit MFA ── */
  const digits = [];
  const digitRow = el('div', { class: 'mfa-row' });
  for (let i = 0; i < 6; i++) {
    const input = el('input', {
      class: 'digit', type: 'text', inputmode: 'numeric', autocomplete: 'one-time-code',
      maxlength: '1', 'aria-label': 'Digit ' + (i + 1) + ' of 6',
    });
    input.addEventListener('input', function () { onDigitInput(i, input.value); });
    input.addEventListener('keydown', function (e) { onDigitKey(i, e); });
    input.addEventListener('paste', function (e) { onDigitPaste(i, e); });
    input.addEventListener('focus', function () { input.select(); });
    digits.push(input);
    digitRow.appendChild(input);
  }

  const mfaLede = el('p', { class: 'body-sm' });
  const mfaError = el('div', { class: 'banner banner-red hidden' });
  const mfaBusy = el('div', { class: 'row row-center hidden' }, [el('span', { class: 'spinner' }), el('span', { class: 'body-sm', text: 'Verifying…' })]);
  const restartBtn = el('button', { class: 'btn btn-ghost btn-sm', type: 'button' }, [icon('back'), 'Use a different account']);
  restartBtn.addEventListener('click', function () { reset(); });

  const mfaPanel = el('div', { class: 'stack-sm hidden' }, [
    el('div', { class: 'display-3', text: 'Enter your 6-digit code' }),
    mfaLede,
    digitRow,
    mfaError,
    mfaBusy,
    el('div', { class: 'row' }, restartBtn),
  ]);

  const node = el('div', { class: 'stack-sm' }, [loginForm, mfaPanel]);

  /* ── submit handlers ── */
  loginForm.addEventListener('submit', async function (e) {
    e.preventDefault();
    if (busy) return;
    const email = emailInput.value.trim();
    const password = passInput.value;
    if (!email || !password) { bannerError(loginError, 'Fill in both fields.'); return; }

    show(loginError, false);
    busy = true;
    setBusy(loginBtn, true, 'Signing in…');
    try {
      const res = await api('/api/garmin/login', { method: 'POST', body: { email: email, password: password } });
      if (res.mfa_required) {
        mfaToken = res.mfa_token || '';
        mfaMethod = res.mfa_method || 'email';
        toMfa();
      } else {
        passInput.value = '';
        onComplete();
      }
    } catch (err) {
      bannerError(loginError, humanizeGarmin(err, 'login'));
    } finally {
      busy = false;
      setBusy(loginBtn, false);
    }
  });

  async function submitMfa(code) {
    if (busy) return;
    show(mfaError, false);
    busy = true;
    show(mfaBusy, true);
    setDigitsDisabled(true);
    let retry = false;
    try {
      await api('/api/garmin/verify-mfa', { method: 'POST', body: { mfa_token: mfaToken, code: code } });
      passInput.value = '';
      onComplete();
    } catch (err) {
      if (err.code === 'mfa_expired') {
        reset();
        bannerError(loginError, 'That code expired. Sign in again to get a fresh one.');
        return;
      }
      bannerError(mfaError, humanizeGarmin(err, 'mfa'));
      clearDigits();
      retry = true;
    } finally {
      busy = false;
      show(mfaBusy, false);
      setDigitsDisabled(false);
      // Refocus after the boxes are enabled again: focusing a disabled input does nothing.
      if (retry) digits[0].focus();
    }
  }

  /* ── digit boxes: typing advances, a pasted code spreads across them, a full code submits itself ── */
  function onDigitInput(i, raw) {
    const only = String(raw).replace(/\D/g, '');
    if (only.length > 1) { spread(i, only); return; }
    digits[i].value = only.slice(-1);
    if (digits[i].value && i < 5) digits[i + 1].focus();
    if (i === 5 && allFilled()) submitMfa(currentCode());
  }

  function onDigitKey(i, e) {
    if (e.key === 'Backspace' && !digits[i].value && i > 0) {
      e.preventDefault();
      digits[i - 1].value = '';
      digits[i - 1].focus();
    } else if (e.key === 'ArrowLeft' && i > 0) { e.preventDefault(); digits[i - 1].focus(); }
    else if (e.key === 'ArrowRight' && i < 5) { e.preventDefault(); digits[i + 1].focus(); }
    else if (e.key === 'Enter' && allFilled()) { e.preventDefault(); submitMfa(currentCode()); }
  }

  function onDigitPaste(i, e) {
    const txt = (e.clipboardData || window.clipboardData).getData('text') || '';
    const only = txt.replace(/\D/g, '').slice(0, 6 - i);
    if (!only) return;
    e.preventDefault();
    spread(i, only);
  }

  function spread(start, chars) {
    let idx = start;
    for (const c of chars) {
      if (idx > 5) break;
      digits[idx].value = c;
      idx++;
    }
    if (allFilled()) {
      digits[Math.min(5, idx - 1)].blur();
      submitMfa(currentCode());
    } else {
      digits[Math.min(5, idx)].focus();
    }
  }

  function currentCode() { return digits.map(function (d) { return d.value; }).join(''); }
  function allFilled() { return digits.every(function (d) { return d.value !== ''; }); }
  function clearDigits() { for (const d of digits) d.value = ''; }
  function setDigitsDisabled(on) { for (const d of digits) d.disabled = on; }

  /* ── view switching ── */
  function toMfa() {
    clear(mfaLede);
    appendKids(mfaLede, [
      'Garmin sent a code via ',
      el('b', { class: 'mono strong', text: mfaMethod }),
      '. It is valid for a few minutes.',
    ]);
    show(mfaError, false);
    clearDigits();
    show(loginForm, false);
    show(mfaPanel, true);
    digits[0].focus();
  }

  function reset() {
    mfaToken = '';
    busy = false;
    clearDigits();
    setDigitsDisabled(false);
    passInput.value = '';
    show(loginError, false);
    show(mfaError, false);
    show(mfaBusy, false);
    show(mfaPanel, false);
    show(loginForm, true);
    setBusy(loginBtn, false);
  }

  return {
    node: node,
    reset: reset,
    focus: function () { (loginForm.classList.contains('hidden') ? digits[0] : emailInput).focus(); },
  };
}

/* The same flow in a modal, for the dashboard banner and Settings. */
function openGarminModal(onDone) {
  const shell = modalShell('Connect Garmin', 'Sign in once — tokens live on this computer');
  const flow = createGarminFlow({
    onComplete: function () {
      m.close();
      toast('Garmin connected', 'ok');
      if (onDone) onDone();
    },
  });
  appendKids(shell.body, flow.node);
  const m = openModal(shell.box);
  shell.closeBtn.addEventListener('click', m.close);
  flow.focus();
  return m;
}

/* ── error copy ── */
function bannerError(banner, content) {
  clear(banner);
  appendKids(banner, [icon('alert', 'banner-icon'), el('div', {}, content)]);
  banner.classList.remove('hidden');
}

function humanizeGarmin(err, stage) {
  switch (err.code) {
    case 'invalid_credentials':
      return 'Garmin rejected that email/password.';
    case 'invalid_mfa_code':
      return "That code didn't match — check for a newer email and try again.";
    case 'mfa_expired':
      return 'That code expired. Sign in again to get a fresh one.';
    case 'garmin_unreachable':
      return 'Garmin is unreachable right now. Try again in a minute.';
    case 'missing_fields':
      return 'Fill in both fields.';
    case 'network':
      return err.message;
    default:
      break;
  }
  if (err.status === 502 || err.status === 503 || err.status === 504) {
    return 'Garmin is unreachable right now. Try again in a minute.';
  }
  const label = stage === 'mfa' ? 'Verification failed' : 'Sign-in failed';
  return el('span', {}, [
    label + ': ' + (err.detail || err.code) + '. If this keeps happening, ',
    el('a', { href: GARMIN_ISSUES_URL, target: '_blank', rel: 'noopener noreferrer', text: 'open an issue on GitHub' }),
    '.',
  ]);
}
