/* ui.js — shared helpers. Classic script: everything here is a global, loaded before the page scripts. */

/* ── fetch wrapper ── */
async function api(path, opts) {
  opts = opts || {};
  const method = (opts.method || 'GET').toUpperCase();
  const headers = { Accept: 'application/json' };
  let body;
  if (method !== 'GET') {
    // The server's CSRF check rejects any non-GET without this header.
    headers['X-ScaleBridge-Local'] = '1';
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(opts.body === undefined ? {} : opts.body);
  }

  let res;
  try {
    res = await fetch(path, { method: method, headers: headers, body: body, cache: 'no-store' });
  } catch (e) {
    const err = new Error('Cannot reach scalebridge-sync. Is it still running on this computer?');
    err.code = 'network';
    err.status = 0;
    throw err;
  }

  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch (e) { data = null; }
  }

  if (!res.ok) {
    const code = (data && data.error) || 'http_' + res.status;
    const detail = (data && data.detail) || '';
    const err = new Error(detail || code);
    err.code = code;
    err.detail = detail;
    err.status = res.status;
    throw err;
  }
  return data || {};
}

/* ── DOM builder ── */
function el(tag, attrs, children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const k in attrs) {
      const v = attrs[k];
      if (v === null || v === undefined || v === false) continue;
      if (k === 'class') node.className = v;
      else if (k === 'text') node.textContent = v;
      else if (k === 'dataset') { for (const d in v) node.dataset[d] = v[d]; }
      else if (k.length > 2 && k.slice(0, 2) === 'on') node.addEventListener(k.slice(2), v);
      else if (v === true) node.setAttribute(k, '');
      else node.setAttribute(k, v);
    }
  }
  appendKids(node, children);
  return node;
}

function appendKids(node, kids) {
  if (kids === null || kids === undefined || kids === false) return;
  if (Array.isArray(kids)) { for (const k of kids) appendKids(node, k); return; }
  node.appendChild(kids instanceof Node ? kids : document.createTextNode(String(kids)));
}

function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }
function qs(sel, root) { return (root || document).querySelector(sel); }
function qsa(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }
function show(node, on) { node.classList.toggle('hidden', !on); }

/* ── inline icons: stroked 24×24 paths in currentColor, so no image requests ── */
const ICONS = {
  check:    ['M20 6 9 17l-5-5'],
  close:    ['M18 6 6 18', 'M6 6l12 12'],
  copy:     ['M9 9h10a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H9a1 1 0 0 1-1-1V10a1 1 0 0 1 1-1z', 'M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1'],
  refresh:  ['M21 12a9 9 0 1 1-2.64-6.36', 'M21 3v6h-6'],
  external: ['M14 3h7v7', 'M21 3 11 13', 'M19 14v6a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1h6'],
  alert:    ['M12 2a10 10 0 1 0 0 20 10 10 0 1 0 0-20', 'M12 7v6', 'M12 16.5v.5'],
  info:     ['M12 2a10 10 0 1 0 0 20 10 10 0 1 0 0-20', 'M12 11v6', 'M12 7.5v.5'],
  eye:      ['M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7-10-7-10-7z', 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z'],
  eyeOff:   ['M3 3l18 18', 'M10.6 10.6A3 3 0 0 0 12 15a3 3 0 0 0 2.4-1.2', 'M6.3 6.4C3.8 8 2 12 2 12s3.6 7 10 7c2 0 3.7-.7 5.1-1.6', 'M9.9 5.2A9.6 9.6 0 0 1 12 5c6.4 0 10 7 10 7a18 18 0 0 1-3 3.9'],
  chevron:  ['M9 6l6 6-6 6'],
  arrow:    ['M5 12h14', 'M13 5l7 7-7 7'],
  back:     ['M19 12H5', 'M11 19l-7-7 7-7'],
  clock:    ['M12 2a10 10 0 1 0 0 20 10 10 0 1 0 0-20', 'M12 6.5V12l3.5 2'],
  link:     ['M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7', 'M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7'],
  plug:     ['M9 2v6', 'M15 2v6', 'M6 8h12v3a6 6 0 0 1-12 0z', 'M12 17v5'],
  trash:    ['M4 7h16', 'M10 11v6', 'M14 11v6', 'M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13', 'M9 7V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v3'],
  scale:    ['M4 20h16', 'M12 4v16', 'M4 8h16', 'M4 8 1.5 14a3.5 3.5 0 0 0 5 0z', 'M20 8l2.5 6a3.5 3.5 0 0 1-5 0z'],
  download: ['M12 3v12', 'M7 11l5 5 5-5', 'M4 21h16'],
  shield:   ['M12 3l8 3v6c0 5-3.4 8.2-8 9-4.6-.8-8-4-8-9V6z', 'M9 12l2 2 4-4'],
};

function icon(name, cls) {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  if (cls) svg.setAttribute('class', cls);
  const paths = ICONS[name] || [];
  for (const d of paths) {
    const p = document.createElementNS(NS, 'path');
    p.setAttribute('d', d);
    svg.appendChild(p);
  }
  return svg;
}

/* ── toasts ── */
function toast(msg, kind) {
  let host = qs('.toast-host');
  if (!host) { host = el('div', { class: 'toast-host' }); document.body.appendChild(host); }
  const node = el('div', { class: 'toast toast-' + (kind || 'info'), role: 'status', text: msg });
  host.appendChild(node);
  const life = kind === 'err' ? 7000 : 3800;
  setTimeout(function () {
    node.style.opacity = '0';
    setTimeout(function () { if (node.parentNode) node.parentNode.removeChild(node); }, 250);
  }, life);
}

/* ── time ── */
function relTime(iso) {
  const t = iso ? Date.parse(iso) : NaN;
  if (isNaN(t)) return 'never';
  const diff = Date.now() - t;
  const future = diff < 0;
  const secs = Math.round(Math.abs(diff) / 1000);
  if (secs < 45) return future ? 'in a few seconds' : 'just now';
  let span;
  if (secs < 5400) span = plural(Math.round(secs / 60), 'minute');
  else if (secs < 172800) span = plural(Math.round(secs / 3600), 'hour');
  else span = plural(Math.round(secs / 86400), 'day');
  return future ? 'in ' + span : span + ' ago';
}

function plural(n, word) { return n + ' ' + word + (n === 1 ? '' : 's'); }

function absTime(iso) {
  const t = iso ? Date.parse(iso) : NaN;
  if (isNaN(t)) return '—';
  const d = new Date(t);
  return d.toLocaleString(undefined, {
    year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  });
}

function clockTime(iso) {
  const t = iso ? Date.parse(iso) : NaN;
  if (isNaN(t)) return '—';
  return new Date(t).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

/* ── polling: pauses while the tab is hidden, never overlaps two calls ── */
function poll(fn, ms) {
  let timer = null, stopped = false, inFlight = false;

  function schedule() {
    clearTimeout(timer);
    if (!stopped) timer = setTimeout(tick, ms);
  }
  async function tick() {
    if (stopped) return;
    if (document.hidden || inFlight) { schedule(); return; }
    inFlight = true;
    try { await fn(); } catch (e) { /* fn owns its own error reporting */ }
    inFlight = false;
    schedule();
  }
  function onVisible() { if (!document.hidden && !stopped) { clearTimeout(timer); tick(); } }

  document.addEventListener('visibilitychange', onVisible);
  tick();
  return {
    stop: function () { stopped = true; clearTimeout(timer); document.removeEventListener('visibilitychange', onVisible); },
    kick: function () { clearTimeout(timer); tick(); },
  };
}

/* ── clipboard ── */
async function copyText(text) {
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (e) { /* fall through to the textarea trick */ }
  try {
    const ta = el('textarea', { 'aria-hidden': 'true', tabindex: '-1' });
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand('copy');
    document.body.removeChild(ta);
    return ok;
  } catch (e) { return false; }
}

/* A <code> block with a Copy button; `loud` is the big violet variant. */
function copyBox(text, loud) {
  const label = el('span', { text: 'Copy' });
  const btn = el('button', { class: 'copy-btn', type: 'button' }, [icon('copy'), label]);
  btn.addEventListener('click', async function () {
    const ok = await copyText(text);
    label.textContent = ok ? 'Copied' : 'Press Ctrl+C';
    if (ok) toast('Copied to clipboard', 'ok');
    setTimeout(function () { label.textContent = 'Copy'; }, 1800);
  });
  return el('div', { class: 'copybox' + (loud ? ' copybox-loud' : '') }, [
    el('code', { text: text }), btn,
  ]);
}

/* ── modals ── */
function openModal(node, opts) {
  opts = opts || {};
  const backdrop = el('div', { class: 'modal-backdrop' }, node);
  function close() {
    document.removeEventListener('keydown', onKey);
    if (backdrop.parentNode) backdrop.parentNode.removeChild(backdrop);
    // Restore scrolling only when no other modal is still stacked on top.
    if (!qs('.modal-backdrop')) document.body.style.overflow = '';
    if (opts.onClose) opts.onClose();
  }
  function onKey(e) { if (e.key === 'Escape') close(); }
  backdrop.addEventListener('mousedown', function (e) { if (e.target === backdrop && !opts.sticky) close(); });
  document.addEventListener('keydown', onKey);
  document.body.style.overflow = 'hidden';
  document.body.appendChild(backdrop);
  const focusable = qs('input, button, select', node);
  if (focusable) focusable.focus();
  return { close: close, node: node };
}

function modalShell(title, subtitle, opts) {
  opts = opts || {};
  const body = el('div', { class: 'modal-body' });
  const foot = el('div', { class: 'modal-foot' });
  const closeBtn = el('button', { class: 'modal-close', type: 'button', 'aria-label': 'Close' }, icon('close'));
  const box = el('div', { class: 'modal' + (opts.wide ? ' modal-wide' : ''), role: 'dialog', 'aria-modal': 'true' }, [
    el('div', { class: 'modal-head' }, [
      el('div', { class: 'grow' }, [
        el('div', { class: 'title', text: title }),
        subtitle ? el('div', { class: 'caption', text: subtitle }) : null,
      ]),
      closeBtn,
    ]),
    body, foot,
  ]);
  return { box: box, body: body, foot: foot, closeBtn: closeBtn };
}

/* Yes/no confirmation; resolves true when the user confirms. */
function confirmModal(o) {
  return new Promise(function (resolve) {
    const shell = modalShell(o.title, null);
    let answered = false;
    appendKids(shell.body, el('p', { class: 'body-sm', text: o.message }));
    const cancel = el('button', { class: 'btn btn-secondary', type: 'button', text: o.cancelText || 'Cancel' });
    const ok = el('button', { class: 'btn ' + (o.danger ? 'btn-danger-solid' : 'btn-accent'), type: 'button', text: o.confirmText || 'Confirm' });
    appendKids(shell.foot, [cancel, ok]);
    const m = openModal(shell.box, { onClose: function () { if (!answered) { answered = true; resolve(false); } } });
    shell.closeBtn.addEventListener('click', m.close);
    cancel.addEventListener('click', m.close);
    ok.addEventListener('click', function () { answered = true; resolve(true); m.close(); });
    ok.focus();
  });
}

/* ── buttons ── */
/* Swap a button into a spinner + label while an async action runs. */
function setBusy(btn, busy, busyLabel) {
  if (busy) {
    if (!btn.idleNodes) btn.idleNodes = Array.prototype.slice.call(btn.childNodes);
    btn.disabled = true;
    clear(btn);
    appendKids(btn, [el('span', { class: 'spinner' }), busyLabel || 'Working…']);
  } else {
    btn.disabled = false;
    clear(btn);
    appendKids(btn, btn.idleNodes || []);
  }
}

/* ── formatting ── */
function num(v, digits, suffix) {
  if (v === null || v === undefined || v === '') return '—';
  const n = Number(v);
  if (isNaN(n)) return '—';
  return n.toFixed(digits === undefined ? 1 : digits) + (suffix || '');
}

function statusPill(kind, label) {
  return el('span', { class: 'pill pill-' + kind }, [el('span', { class: 'dot' }), label]);
}
