/*! NovaPop v0.1.0 — popup + confirm + prompt + lightbox + tooltip + toast (no deps) */
(function (root, factory) {
  if (typeof define === 'function' && define.amd) define([], factory);
  else if (typeof module === 'object' && module.exports) module.exports = factory();
  else root.NovaPop = factory();
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  // ------------------------------
  // Utilities
  // ------------------------------
  const isStr = v => typeof v === 'string';
  const isFn  = v => typeof v === 'function';
  const isEl  = v => v instanceof Node;
  const clamp = (v, min, max) => Math.max(min, Math.min(max, v));
  const uid = (p='np') => `${p}-${Math.random().toString(36).slice(2,9)}`;
  const create = (tag, cls, attrs = {}) => {
    const el = document.createElement(tag);
    if (cls) el.className = cls;
    for (const k in attrs) el.setAttribute(k, attrs[k]);
    return el;
  };
  const focusableSel = [
    'a[href]', 'area[href]', 'input:not([disabled]):not([type="hidden"])',
    'select:not([disabled])', 'textarea:not([disabled])',
    'button:not([disabled])', 'iframe', 'audio[controls]', 'video[controls]',
    '[contenteditable]', '[tabindex]:not([tabindex="-1"])'
  ].join(',');
  const tabbables = root =>
    Array.from(root.querySelectorAll(focusableSel))
      .filter(el => !el.hasAttribute('inert') && !el.closest('[inert]') && (el.offsetParent !== null || el.getClientRects().length));

  // Simple event helper
  const on  = (el, ev, fn, opts) => el.addEventListener(ev, fn, opts);
  const off = (el, ev, fn, opts) => el.removeEventListener(ev, fn, opts);

  // Style injection once
  let stylesInjected = false;
  function injectStyles() {
    if (stylesInjected) return;
    stylesInjected = true;
    const css = `
:root {
  --np-bg: #111; --np-fg: #fff; --np-muted: #a3a3a3;
  --np-backdrop: rgba(0,0,0,.6); --np-accent:#4f46e5;
  --np-radius: 14px; --np-shadow: 0 20px 40px rgba(0,0,0,.25), 0 0 1px rgba(0,0,0,.4);
  --np-anim: 220ms; --np-z: 10000; --np-tooltip-bg:#111; --np-tooltip-fg:#fff;
  --np-toast-bg:#111; --np-toast-fg:#fff; --np-danger:#ef4444; --np-success:#16a34a; --np-warning:#f59e0b;
}
.np-overlay{position:fixed;inset:0;background:var(--np-backdrop);opacity:0;transition:opacity var(--np-anim) ease;z-index:var(--np-z);}
.np-overlay.np-show{opacity:1;}
.np-modal{position:fixed;inset:0;display:flex;align-items:center;justify-content:center;z-index:calc(var(--np-z) + 1);outline:0;}
.np-card{max-width:min(92vw,720px);max-height:86vh;overflow:auto;background:var(--np-bg);color:var(--np-fg);border-radius:var(--np-radius);
  box-shadow:var(--np-shadow);transform:translateY(12px) scale(.98);opacity:0;transition:all var(--np-anim) cubic-bezier(.2,.7,.2,1); position:relative}
.np-card.np-show{opacity:1;transform:translateY(0) scale(1);}
.np-head{padding:16px 20px 8px;font-weight:700;font-size:18px;display:flex;gap:10px;align-items:center}
.np-title{margin:0;font:inherit}
.np-body{padding:0 20px 18px;line-height:1.6;color:#e5e7eb;}
.np-actions{display:flex;gap:12px;justify-content:flex-end;padding:14px 16px;border-top:1px solid rgba(255,255,255,.08);backdrop-filter:blur(4px);position:sticky;bottom:0;background:linear-gradient(180deg, rgba(17,17,17,.7), rgba(17,17,17,.9));}
.np-btn{appearance:none;cursor:pointer;border:0;border-radius:10px;padding:10px 14px;font-weight:600;background:#27272a;color:#e7e7e7;transition:transform .12s ease, background .12s ease;user-select:none}
.np-btn:hover{transform:translateY(-1px)}
.np-btn:focus-visible{outline:2px solid var(--np-accent);outline-offset:2px}
.np-btn.np-accent{background:var(--np-accent);color:#fff}
.np-btn.np-danger{background:var(--np-danger)}
.np-close{position:absolute;top:10px;right:10px;width:36px;height:36px;border-radius:50%;border:0;background:#0009;color:#fff;display:grid;place-items:center;font-size:18px;cursor:pointer}
.np-close:hover{background:#000c}

/* Timer bar */
.np-timer{position:absolute;left:0;right:0;top:0;height:3px;background:linear-gradient(90deg, rgba(255,255,255,.2), rgba(255,255,255,.6));transform-origin:left center;transform:scaleX(1);transition:transform linear;}

.np-field{display:grid;gap:6px;margin:12px 0}
.np-field label{font-size:12px;color:var(--np-muted)}
.np-input, .np-select, .np-textarea{width:100%;box-sizing:border-box;border-radius:10px;border:1px solid rgba(255,255,255,.12);background:#0b0b0b;color:#fff;padding:10px 12px}
.np-hint{font-size:12px;color:var(--np-muted)}
.np-row{display:flex;gap:10px;flex-wrap:wrap}
.np-check{display:flex;align-items:center;gap:8px}

.np-lightbox{position:fixed;inset:0;display:grid;grid-template-rows:1fr auto;z-index:calc(var(--np-z) + 2);color:#fff}
.np-stage{position:relative;display:grid;place-items:center;overflow:hidden}
.np-img, .np-vid, .np-html{max-width:94vw;max-height:82vh;border-radius:12px;box-shadow:var(--np-shadow)}
.np-figure{display:grid;justify-items:center}
.np-caption{padding:10px 16px;color:#e5e5e5;text-align:center;min-height:2em}
.np-arrow{position:absolute;top:50%;transform:translateY(-50%);width:44px;height:44px;border-radius:999px;border:0;background:#000a;color:#fff;cursor:pointer}
.np-arrow:hover{background:#000c}
.np-prev{left:14px}.np-next{right:14px}

/* Tooltip */
.np-tooltip{position:fixed;z-index:calc(var(--np-z) + 3);background:var(--np-tooltip-bg);color:var(--np-tooltip-fg);padding:8px 10px;border-radius:8px;font-size:13px;line-height:1.3;box-shadow:0 8px 24px rgba(0,0,0,.35);opacity:0;transform:translateY(4px);pointer-events:none;transition:opacity 120ms ease, transform 120ms ease;max-width:320px}
.np-tooltip.np-show{opacity:1;transform:translateY(0)}
.np-tip-arrow{position:absolute;width:10px;height:10px;transform:rotate(45deg);background:var(--np-tooltip-bg)}

/* Anim helpers */
.np-anim-fade .np-card{transform:none;opacity:0}.np-anim-fade .np-card.np-show{opacity:1}
.np-anim-scale .np-card{transform:scale(.94);opacity:0}.np-anim-scale .np-card.np-show{transform:scale(1);opacity:1}
.np-anim-slide .np-card{transform:translateY(16px);opacity:0}.np-anim-slide .np-card.np-show{transform:translateY(0);opacity:1}

/* Scroll lock helper class */
body.np-locked{overflow:hidden;touch-action:none}

/* Toasts */
.np-toasts{position:fixed;top:16px;right:16px;display:flex;flex-direction:column;gap:10px;z-index:calc(var(--np-z) + 4);pointer-events:none}
.np-toast{pointer-events:auto;display:grid;grid-template-columns:auto 1fr auto;gap:10px;align-items:center;min-width:260px;max-width:min(92vw,420px);background:var(--np-toast-bg);color:var(--np-toast-fg);border-radius:12px;padding:10px 12px;box-shadow:var(--np-shadow);opacity:0;transform:translateY(-6px);transition:opacity 150ms ease, transform 150ms ease}
.np-toast.np-show{opacity:1;transform:translateY(0)}
.np-toast .np-x{cursor:pointer;border:0;background:transparent;color:inherit;font-size:18px}
.np-toast .np-icon{width:18px;height:18px;border-radius:4px}
.np-toast.success{outline:2px solid var(--np-success)}
.np-toast.error{outline:2px solid var(--np-danger)}
.np-toast.warn{outline:2px solid var(--np-warning)}
    `;
    const style = create('style');
    style.id = 'novapop-styles';
    style.textContent = css;
    document.head.appendChild(style);
  }

  // Global live-region for SR
  let liveRegion;
  function ensureLiveRegion() {
    if (liveRegion) return;
    liveRegion = create('div');
    liveRegion.setAttribute('aria-live', 'polite');
    liveRegion.setAttribute('aria-atomic', 'true');
    liveRegion.style.cssText = 'position:absolute;clip:rect(0 0 0 0);clip-path:inset(50%);height:1px;width:1px;overflow:hidden;white-space:nowrap;';
    document.body.appendChild(liveRegion);
  }

  // Scroll lock
  let lockCount = 0;
  function lockScroll() { if (!lockCount++) document.body.classList.add('np-locked'); }
  function unlockScroll() { if (lockCount) lockCount--; if (!lockCount) document.body.classList.remove('np-locked'); }

  // ------------------------------
  // Audio (beep or pooled URL) + global controls
  // ------------------------------
  let audioCtx = null;
  let audioVolume = 1;
  let audioMuted = false;
  const pool = new Map(); // url -> HTMLAudioElement

  function setAudio({ volume, mute } = {}) {
    if (typeof volume === 'number') audioVolume = clamp(volume, 0, 1);
    if (typeof mute === 'boolean') audioMuted = mute;
    pool.forEach(a => { a.volume = audioMuted ? 0 : audioVolume; });
  }

  function playSound(opt) {
    if (!opt || audioMuted) return;
    if (isStr(opt)) {
      if (/^https?:/i.test(opt) || /\.((mp3|wav|ogg))$/i.test(opt)) return playUrl(opt);
      if (opt === 'beep' || opt === 'ding') return beep();
    }
    if (opt && opt.url) return playUrl(opt.url);
    const cfg = Object.assign({ freq: 784, type: 'sine', duration: .12, vol: .2 }, (typeof opt === 'object' ? opt : {}));
    beep(cfg);
  }
  function playUrl(url) {
    try {
      let a = pool.get(url);
      if (!a) { a = new Audio(url); a.preload = 'auto'; a.volume = audioVolume; pool.set(url, a); }
      a.currentTime = 0; a.play().catch(()=>{});
    } catch(e) {}
  }
  function beep({ freq = 784, type = 'sine', duration = .12, vol = .2 } = {}) {
    try {
      audioCtx = audioCtx || new (window.AudioContext || window.webkitAudioContext)();
      const o = audioCtx.createOscillator();
      const g = audioCtx.createGain();
      o.type = type; o.frequency.value = freq;
      g.gain.value = (audioMuted ? 0 : audioVolume) * vol;
      o.connect(g); g.connect(audioCtx.destination);
      o.start(); setTimeout(() => { try { o.stop(); } catch(_e){} }, duration * 1000);
    } catch (e) {}
  }

  // Focus trap
  function makeTrap(modalEl) {
    function onKey(e) {
      if (e.key !== 'Tab') return;
      const t = tabbables(modalEl);
      if (!t.length) return;
      const first = t[0], last = t[t.length - 1];
      if (e.shiftKey && document.activeElement === first) { last.focus(); e.preventDefault(); }
      else if (!e.shiftKey && document.activeElement === last) { first.focus(); e.preventDefault(); }
    }
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }

  // Stacking z-index
  let stack = 0;
  function nextZ() { return 10000 + (++stack) * 10; }
  function popZ() { stack = Math.max(0, stack - 1); }

  // Track instances for destroyAll
  const instances = new Set();
  const toasts = new Set();

  // Lifecycle helper
  function runHook(h, arg) { try { isFn(h) && h(arg); } catch(e) {} }

  // ------------------------------
  // Popup / Confirm / Prompt
  // ------------------------------
  function popup(opts = {}) {
    injectStyles(); ensureLiveRegion();

    const {
      title = '',
      content = '',
      html,               // Node or HTML
      buttons = [{ label: 'OK', action: 'ok', accent: true }],
      showCloseButton = true,
      closeOnEsc = true,
      closeOnBackdrop = true,
      sound = null,       // 'beep' | URL | {url}| {freq,type,...}
      animation = 'scale',// 'scale' | 'fade' | 'slide'
      timer = 0,          // ms, 0 = no auto-close
      timerProgress = true,
      onOpen, onShown, beforeClose, onClose, // lifecycle hooks
      themeClass = ''     // add CSS class (e.g., 'np-dark')
    } = opts;

    const z = nextZ();
    const overlay = create('div', 'np-overlay'); overlay.style.zIndex = z;
    const modal = create('div', `np-modal np-anim-${animation || 'scale'} ${themeClass || ''}`, {
      role: 'dialog', 'aria-modal': 'true'
    }); modal.style.zIndex = z + 1;

    const card  = create('div', 'np-card');
    const head  = create('div', 'np-head');
    const titleId = uid('np-title');
    const h = create('h2', 'np-title', { id: titleId }); h.textContent = title || '';
    card.setAttribute('aria-labelledby', titleId);
    head.appendChild(h);

    const body  = create('div', 'np-body');
    if (isEl(html)) body.appendChild(html);
    else body.innerHTML = html ?? (isStr(content) ? content : '');

    const actions = create('div', 'np-actions');

    const result = { action: null, data: null };
    let resolved = false;
    let closeTimer = 0;

    const api = {
      el: card,
      close(payload) { doClose(payload); },
      onClose(fn) { onCloseQueue.push(fn); return api; }
    };
    const onCloseQueue = [];

    buttons.forEach(b => {
      const btn = create('button', 'np-btn' + (b.accent ? ' np-accent' : '') + (b.danger ? ' np-danger' : ''));
      btn.type = 'button'; btn.textContent = b.label || 'OK';
      btn.addEventListener('click', async () => {
        result.action = b.action || 'ok';
        result.data = isFn(b.getData) ? b.getData() : null;
        if (isFn(b.onClick)) { const maybe = b.onClick(result, api); if (maybe && isFn(maybe.then)) await maybe; }
        if (!b.keepOpen) doClose(result);
      });
      actions.appendChild(btn);
    });

    if (showCloseButton) {
      const x = create('button', 'np-close', { 'aria-label': 'Close' });
      x.innerHTML = '✕';
      on(x, 'click', () => { result.action = 'close'; doClose(result); });
      card.appendChild(x);
    }

    // Timer progress
    let timerBar;
    if (timer > 0 && timerProgress) {
      timerBar = create('div', 'np-timer');
      card.appendChild(timerBar);
    }

    card.append(head, body, actions);
    document.body.appendChild(overlay);
    document.body.appendChild(modal);
    modal.appendChild(card);

    // Show
    requestAnimationFrame(() => { overlay.classList.add('np-show'); card.classList.add('np-show'); });
    lockScroll();
    playSound(sound);
    runHook(onOpen, { element: card, options: opts });
    ensureAnnounce(title);

    let untrap = makeTrap(card);
    if (closeOnBackdrop) on(overlay, 'click', () => { result.action = 'backdrop'; doClose(result); });
    function onKey(e) {
      if (e.key === 'Escape' && closeOnEsc) { result.action = 'esc'; doClose(result); }
    }
    document.addEventListener('keydown', onKey, true);

    // Focus first button (or first focusable)
    setTimeout(() => {
      const t = tabbables(card);
      (t[0] || card).focus?.();
      runHook(onShown, { element: card, options: opts });
    }, 30);

    // Auto-close timer
    if (timer > 0) {
      timerBar && (timerBar.style.transitionDuration = `${timer}ms`);
      timerBar && (timerBar.style.transform = 'scaleX(1)');
      // next frame to trigger transition
      requestAnimationFrame(() => { timerBar && (timerBar.style.transform = 'scaleX(0)'); });
      closeTimer = setTimeout(() => { result.action = 'timer'; doClose(result); }, timer);
    }

    instances.add(api);

    function doClose(payload) {
      if (resolved) return;
      if (beforeClose && beforeClose(payload) === false) return; // veto
      resolved = true;
      clearTimeout(closeTimer);
      overlay.classList.remove('np-show'); card.classList.remove('np-show');
      document.removeEventListener('keydown', onKey, true);
      untrap && untrap(); unlockScroll();
      setTimeout(() => { overlay.remove(); modal.remove(); popZ(); instances.delete(api); }, 220);
      onCloseQueue.forEach(fn => { try { fn(payload); } catch(e){} });
      runHook(onClose, payload);
      _resolve(payload);
    }

    let _resolve;
    const p = new Promise(r => (_resolve = r));
    p.close = api.close; p.onClose = api.onClose; p.el = card;
    return p;
  }

  function confirm(opts = {}) {
    const {
      title = 'Are you sure?',
      content = '',
      okText = 'Confirm',
      cancelText = 'Cancel',
      danger = false,
      sound = null,
      closeOnEsc = true,
      closeOnBackdrop = true,
      animation = 'scale',
      timer = 0,
      onOpen, onShown, beforeClose, onClose
    } = opts;

    return popup({
      title, html: `<div>${content || ''}</div>`,
      animation, sound, closeOnEsc, closeOnBackdrop, timer,
      onOpen, onShown, beforeClose, onClose,
      buttons: [
        { label: cancelText, action: 'cancel' },
        { label: okText, action: 'ok', accent: !danger, danger }
      ]
    }).then(res => res.action === 'ok');
  }

  // Prompt: rich inputs -> data object or null
  function prompt(opts = {}) {
    const {
      title = 'Input',
      inputs = [ { type:'text', name:'value', label:'Value', required:false, placeholder:'' } ],
      okText = 'OK',
      cancelText = 'Cancel',
      validate, // (data) => string|false (error message) or true/void
      sound = null,
      animation = 'scale',
      closeOnEsc = true,
      closeOnBackdrop = false,
      timer = 0,
      onOpen, onShown, beforeClose, onClose
    } = opts;

    const form = create('form');
    const fields = inputs.map(makeField);
    fields.forEach(f => form.appendChild(f.wrap));

    const err = create('div', 'np-hint');
    err.style.color = 'var(--np-danger)'; err.style.display = 'none';
    form.appendChild(err);

    function collect() {
      const data = {};
      for (const f of fields) {
        const el = f.input;
        const name = f.name;
        if (f.type === 'checkbox' && f.multiple) {
          data[name] = Array.from(f.wrap.querySelectorAll('input[type="checkbox"]'))
            .filter(c => c.checked).map(c => c.value);
        } else if (f.type === 'checkbox') {
          data[name] = !!el.checked;
        } else if (f.type === 'file') {
          data[name] = el.files ? Array.from(el.files) : [];
        } else {
          data[name] = el.value;
        }
      }
      return data;
    }
    function onOk(_r, api) {
      const data = collect();
      if (isFn(validate)) {
        const v = validate(data);
        if (v === false || isStr(v)) {
          err.textContent = isStr(v) ? v : 'Please check your input.'; err.style.display = '';
          return api.close({ action: 'stay' }); // keep open via below guard
        }
      }
    }

    const p = popup({
      title,
      html: form,
      animation, sound, closeOnEsc, closeOnBackdrop, timer,
      onOpen, onShown, beforeClose, onClose,
      buttons: [
        { label: cancelText, action: 'cancel' },
        { label: okText, action: 'ok', accent: true, onClick: (_r, api) => onOk(_r, api) }
      ]
    });
    // Override to keep open on validation error
    const origClose = p.close;
    p.close = (payload) => {
      if (payload && payload.action === 'stay') return; // validation blocked
      origClose(payload);
    };
    return p.then(res => res.action === 'ok' ? collect() : null);
  }

  function makeField(f) {
    const {
      type = 'text', name = uid('field'), label = '', value = '',
      placeholder = '', options = [], required = false, multiple = false, min, max, step
    } = f;
    const wrap = create('div', 'np-field');
    if (label) {
      const lab = create('label'); lab.textContent = label; lab.setAttribute('for', name); wrap.appendChild(lab);
    }
    let input;
    if (type === 'textarea') {
      input = create('textarea', 'np-textarea', { name, id: name, placeholder }); input.value = value || '';
    } else if (type === 'select') {
      input = create('select', 'np-select', { name, id: name, ...(multiple ? { multiple: '' } : {}) });
      for (const o of options) {
        const opt = create('option'); opt.value = o.value ?? o; opt.textContent = o.label ?? o.value ?? o;
        if (Array.isArray(value) ? value.includes(opt.value) : value === opt.value) opt.selected = true;
        input.appendChild(opt);
      }
    } else if (type === 'radio') {
      input = create('div', 'np-row'); // group
      options.forEach((o, idx) => {
        const id = uid('r');
        const wrapC = create('label', 'np-check', { for: id });
        const r = create('input'); r.type = 'radio'; r.name = name; r.id = id; r.value = o.value ?? o;
        if (value === r.value) r.checked = true;
        const txt = document.createTextNode(o.label ?? o.value ?? o);
        wrapC.append(r, txt);
        input.appendChild(wrapC);
      });
    } else if (type === 'checkbox' && multiple) {
      input = create('div', 'np-row');
      (options || []).forEach(o => {
        const id = uid('c');
        const w = create('label', 'np-check', { for: id });
        const c = create('input'); c.type = 'checkbox'; c.id = id; c.value = o.value ?? o;
        if (Array.isArray(value) && value.includes(c.value)) c.checked = true;
        const txt = document.createTextNode(o.label ?? o.value ?? o);
        w.append(c, txt); input.appendChild(w);
      });
    } else {
      input = create('input', 'np-input', { type, name, id: name, placeholder });
      if (value != null) input.value = value;
      if (min != null) input.min = String(min);
      if (max != null) input.max = String(max);
      if (step != null) input.step = String(step);
    }
    if (required && input instanceof HTMLElement) input.setAttribute('required','');
    wrap.appendChild(input);
    return { wrap, input, name, type, multiple };
  }

  // ------------------------------
  // Lightbox
  // ------------------------------
  function lightbox(opts = {}) {
    injectStyles(); ensureLiveRegion();
    const {
      items = [],               // [{src, type:'image'|'video'|'html', caption}]
      startIndex = 0,
      sound = null,
      closeOnEsc = true,
      closeOnBackdrop = true,
      onOpen, onShown, beforeClose, onClose
    } = opts;

    if (!items.length) throw new Error('NovaPop.lightbox: items[] required');

    const z = nextZ();
    const overlay = create('div', 'np-overlay'); overlay.style.zIndex = z;
    const wrap = create('div', 'np-lightbox', { role: 'dialog', 'aria-modal': 'true' }); wrap.style.zIndex = z + 1;

    const stage = create('div', 'np-stage');
    const figure = create('figure', 'np-figure');
    const caption = create('figcaption', 'np-caption');
    const closeBtn = create('button', 'np-close', { 'aria-label': 'Close' }); closeBtn.innerHTML = '✕';
    const prev = create('button', 'np-arrow np-prev', { 'aria-label': 'Previous' }); prev.textContent = '‹';
    const next = create('button', 'np-arrow np-next', { 'aria-label': 'Next' }); next.textContent = '›';

    wrap.appendChild(stage); wrap.appendChild(closeBtn);
    stage.appendChild(prev); stage.appendChild(next); stage.appendChild(figure);
    figure.appendChild(caption);

    document.body.appendChild(overlay);
    document.body.appendChild(wrap);
    requestAnimationFrame(() => overlay.classList.add('np-show'));
    lockScroll(); playSound(sound);
    runHook(onOpen, { element: wrap, options: opts });

    let i = clamp(startIndex, 0, items.length - 1);

    function render(index) {
      figure.querySelectorAll('.np-img,.np-vid,.np-html').forEach(n => n.remove());
      const it = items[index];
      let el;
      if (!it || it.type === 'image' || (!it.type && /\.(png|jpg|jpeg|webp|gif|avif|svg)(\?|$)/i.test(it.src))) {
        el = create('img', 'np-img', { alt: it.caption || 'Image', draggable: 'false' });
        el.src = it.src;
      } else if (it.type === 'video') {
        el = create('video', 'np-vid', { controls: '', playsinline: '' });
        const src = create('source'); src.src = it.src; el.appendChild(src);
      } else {
        el = create('div', 'np-html'); el.innerHTML = it.src || it.html || '';
      }
      figure.insertBefore(el, caption);
      caption.textContent = it.caption || '';
      ensureAnnounce(it.caption || 'Item ' + (index + 1));

      // preload neighbors
      [index - 1, index + 1].forEach(pi => {
        const pre = items[pi]; if (pre && (pre.type === 'image' || (!pre.type && /\.(png|jpg|jpeg|webp|gif|avif|svg)/i.test(pre.src)))) {
          const im = new Image(); im.src = pre.src;
        }
      });
    }

    function close(payload) {
      if (beforeClose && beforeClose(payload) === false) return; // veto
      overlay.classList.remove('np-show');
      unlockScroll();
      setTimeout(() => { overlay.remove(); wrap.remove(); popZ(); instances.delete(api); }, 200);
      document.removeEventListener('keydown', onKey, true);
      window.removeEventListener('resize', placeNav);
      runHook(onClose, payload);
    }
    function onKey(e) {
      if (e.key === 'Escape' && closeOnEsc) close({ action:'esc' });
      else if (e.key === 'ArrowRight') go(1);
      else if (e.key === 'ArrowLeft') go(-1);
    }
    function go(step) {
      i = (i + step + items.length) % items.length;
      render(i);
      playSound(sound);
    }

    // Touch swipe
    let sx = 0, dx = 0;
    stage.addEventListener('touchstart', e => { sx = e.touches[0].clientX; dx = 0; }, { passive: true });
    stage.addEventListener('touchmove', e => { dx = e.touches[0].clientX - sx; }, { passive: true });
    stage.addEventListener('touchend', () => { if (Math.abs(dx) > 48) go(dx < 0 ? 1 : -1); sx = 0; dx = 0; });

    // Nav + close
    on(prev,'click', () => go(-1));
    on(next,'click', () => go(1));
    on(closeBtn,'click', () => close({ action:'close' }));
    if (closeOnBackdrop) on(overlay,'click', () => close({ action:'backdrop' }));
    document.addEventListener('keydown', onKey, true);

    render(i); runHook(onShown, { element: wrap, options: opts });

    function placeNav() {
      const h = window.innerHeight;
      const mid = Math.round(h / 2);
      prev.style.top = next.style.top = mid + 'px';
    }
    placeNav(); window.addEventListener('resize', placeNav);

    const api = { next: () => go(1), prev: () => go(-1), close, el: wrap };
    instances.add(api);
    return api;
  }

  // ------------------------------
  // Tooltip (interactive, followCursor, delegate)
  // ------------------------------
  const TipMgr = (() => {
    let tipEl, inner, arrow;
    let cfg = null;
    let currentTarget = null;
    let followPos = { x:0, y:0 };
    function ensure() {
      injectStyles();
      if (tipEl) return;
      tipEl = create('div', 'np-tooltip', { role: 'tooltip', id: 'np-tooltip' });
      inner = create('div', 'np-tip-inner');
      arrow = create('div', 'np-tip-arrow');
      tipEl.appendChild(inner); tipEl.appendChild(arrow);
      document.body.appendChild(tipEl);
    }
    function placeFor(target, opts) {
      const { placement = 'auto', offset = 10, followCursor = false } = opts;
      const vw = window.innerWidth, vh = window.innerHeight;
      const sizes = tipEl.getBoundingClientRect();
      let x = 0, y = 0, side = placement;

      if (followCursor) {
        x = followPos.x + 12; y = followPos.y + 12;
      } else {
        const r = target.getBoundingClientRect();
        if (placement === 'auto') {
          const space = { top: r.top, bottom: vh - r.bottom, left: r.left, right: vw - r.right };
          side = Object.entries(space).sort((a,b)=>b[1]-a[1])[0][0];
        }
        if (side === 'top')    { x = r.left + r.width/2 - sizes.width/2; y = r.top - sizes.height - offset; }
        if (side === 'bottom') { x = r.left + r.width/2 - sizes.width/2; y = r.bottom + offset; }
        if (side === 'left')   { x = r.left - sizes.width - offset; y = r.top + r.height/2 - sizes.height/2; }
        if (side === 'right')  { x = r.right + offset; y = r.top + r.height/2 - sizes.height/2; }
      }
      if (x < 6) x = 6; if (x + sizes.width > vw - 6) x = vw - sizes.width - 6;
      if (y < 6) y = 6; if (y + sizes.height > vh - 6) y = vh - sizes.height - 6;
      tipEl.style.left = (x + (window.scrollX||0)) + 'px';
      tipEl.style.top  = (y + (window.scrollY||0)) + 'px';
    }
    function show(target, opts) {
      ensure();
      cfg = Object.assign({ placement: 'auto', trigger: 'hover', delay: 60, theme: 'dark', interactive: false, followCursor: false }, opts);
      inner.textContent = (cfg.content ?? '').toString();
      tipEl.style.setProperty('--np-tooltip-bg', cfg.theme === 'light' ? '#fff' : (cfg.bg || '#111'));
      tipEl.style.setProperty('--np-tooltip-fg', cfg.theme === 'light' ? '#111' : (cfg.fg || '#fff'));
      tipEl.style.pointerEvents = cfg.interactive ? 'auto' : 'none';
      placeFor(target, cfg);
      tipEl.classList.add('np-show');
      currentTarget = target;
      // a11y: link and announce
      try {
        const old = target.getAttribute('aria-describedby') || '';
        if (!old.includes('np-tooltip')) target.setAttribute('aria-describedby', (old ? old + ' ' : '') + 'np-tooltip');
        ensureAnnounce(cfg.content);
      } catch(_e){}
    }
    function hide() {
      tipEl?.classList.remove('np-show');
      currentTarget = null; cfg = null;
    }
    function attach(el, opts = {}) {
      ensure();
      const c = Object.assign({ placement: 'auto', trigger: 'hover', delay: 80, theme: 'dark', interactive: false, followCursor: false }, opts);
      let enterT = 0, leaveT = 0;
      function _show(e) {
        clearTimeout(leaveT);
        if (c.followCursor && e) followPos = { x: e.clientX, y: e.clientY };
        enterT = setTimeout(() => show(el, c), c.delay);
      }
      function _hide() {
        clearTimeout(enterT);
        leaveT = setTimeout(() => hide(), 60);
      }
      const onMove = (e) => {
        if (c.followCursor) { followPos = { x: e.clientX, y: e.clientY }; placeFor(el, c); }
        else placeFor(el, c);
      };
      if (c.trigger === 'click') {
        const click = (e) => { e.stopPropagation(); show(el, c); };
        const offDoc = (e) => { if (!tipEl.contains(e.target) && e.target !== el) hide(); };
        on(el,'click',click); on(document,'click',offDoc);
        return { destroy(){ off(el,'click',click); off(document,'click',offDoc); hide(); }, update(n){ Object.assign(c,n); } };
      } else if (c.trigger === 'manual') {
        return { show:()=>show(el,c), hide, update(n){ Object.assign(c,n); placeFor(el,c); }, destroy(){ hide(); } };
      } else { // hover/focus
        on(el,'mouseenter',_show); on(el,'mouseleave',_hide);
        on(el,'focus',_show); on(el,'blur',_hide);
        on(el,'mousemove',onMove);
        if (c.interactive) { // keep open when hovering tooltip
          const overTip = () => clearTimeout(leaveT);
          const leaveTip = () => _hide();
          on(tipEl,'mouseenter',overTip); on(tipEl,'mouseleave',leaveTip);
          return { destroy(){ ['mouseenter','mouseleave','focus','blur','mousemove'].forEach(ev=>off(el,ev,({mouseenter:_show,mouseleave:_hide,focus:_show,blur:_hide,mousemove:onMove}[ev]))); off(tipEl,'mouseenter',overTip); off(tipEl,'mouseleave',leaveTip); hide(); }, update(n){ Object.assign(c,n); } };
        }
        return { destroy(){ ['mouseenter','mouseleave','focus','blur','mousemove'].forEach(ev=>off(el,ev,({mouseenter:_show,mouseleave:_hide,focus:_show,blur:_hide,mousemove:onMove}[ev]))); hide(); }, update(n){ Object.assign(c,n); } };
      }
    }
    // Event delegation: container + selector
    function delegate(container, selector, opts = {}) {
      ensure();
      const c = Object.assign({ placement:'auto', trigger:'hover' }, opts);
      function matchFrom(el) { return el.closest(selector); }
      let detach1, detach2, detach3;
      detach1 = on.bind(null, container, 'mouseover', (e) => {
        const t = matchFrom(e.target); if (!t) return;
        const inst = attach(t, c); t._npTipInst = inst;
      });
      detach2 = on.bind(null, container, 'mouseout', (e) => {
        const t = matchFrom(e.target); if (!t || !t._npTipInst) return;
        t._npTipInst.destroy(); delete t._npTipInst;
      });
      detach3 = on.bind(null, container, 'click', (e) => {
        if (c.trigger !== 'click') return;
        const t = matchFrom(e.target); if (!t) return;
        const inst = attach(t, c); t._npTipInst = inst;
      });
      return { destroy(){ container.removeEventListener('mouseover', detach1); container.removeEventListener('mouseout', detach2); container.removeEventListener('click', detach3); } };
    }
    return { attach, show, hide, delegate };
  })();

  // a11y announce
  function ensureAnnounce(text) { ensureLiveRegion(); liveRegion.textContent = String(text || ''); }

  // ------------------------------
  // Toasts
  // ------------------------------
  let toastWrap;
  function ensureToastWrap() {
    if (!toastWrap) { toastWrap = create('div', 'np-toasts'); document.body.appendChild(toastWrap); }
  }
  function toast(opts = {}) {
    injectStyles(); ensureToastWrap();
    const {
      message = '',
      type = 'info', // 'success'|'error'|'warn'|'info'
      duration = 3500,
      action, // {label, onClick}
      sound = null
    } = opts;

    const el = create('div', `np-toast ${type}`);
    const icon = create('div', 'np-icon');
    icon.style.background = (type==='success')?'var(--np-success)':(type==='error')?'var(--np-danger)':(type==='warn')?'var(--np-warning)':'#6b7280';
    const txt = create('div'); txt.textContent = message;
    const x = create('button','np-x',{ 'aria-label':'Dismiss' }); x.textContent = '✕';
    el.append(icon, txt);
    if (action && action.label) {
      const btn = create('button', 'np-btn np-accent'); btn.textContent = action.label;
      on(btn,'click', () => { try { action.onClick?.(); } catch(_e){} close(); });
      el.appendChild(btn);
    }
    el.appendChild(x);
    toastWrap.appendChild(el);
    requestAnimationFrame(()=>el.classList.add('np-show'));
    playSound(sound);
    ensureAnnounce(message);

    let t = 0;
    function close(){ el.classList.remove('np-show'); setTimeout(()=>{ el.remove(); toasts.delete(api); }, 150); }
    on(x,'click', close);
    if (duration > 0) t = setTimeout(close, duration);

    const api = { close, el };
    toasts.add(api);
    return api;
  }

  // ------------------------------
  // Queue helper
  // ------------------------------
  async function queue(items = []) {
    const results = [];
    for (const it of items) {
      const res = await popup(it);
      results.push(res);
      if (res.action !== 'ok' && it.breakOnCancel) break;
    }
    return results;
  }

  // ------------------------------
  // Housekeeping
  // ------------------------------
  function destroyAll() {
    // Close modals/lightboxes
    Array.from(instances).forEach(inst => {
      try { inst.close?.({ action:'destroyAll' }); } catch(_e){}
    });
    // Remove stray toasts
    Array.from(toasts).forEach(t => { try { t.close?.(); } catch(_e){} });
  }

  // Public API
  const NovaPop = {
    // modals
    popup,
    confirm,
    prompt,
    lightbox,
    // tooltips
    tooltip: TipMgr.attach,
    tipShow: TipMgr.show,
    tipHide: TipMgr.hide,
    tipDelegate: TipMgr.delegate,
    // toasts
    toast,
    // queue + housekeeping
    queue,
    destroyAll,
    // audio
    playSound,
    audio: setAudio,
    // version
    version: '0.1.0'
  };

  return NovaPop;
});
