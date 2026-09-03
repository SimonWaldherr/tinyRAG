// R3 frontend — plain JS, no build step.

// $ is a querySelector shorthand.
function $(sel) { return document.querySelector(sel); }
// $all is a querySelectorAll shorthand that returns a real array (so .map/.filter etc. work).
function $all(sel) { return Array.from(document.querySelectorAll(sel)); }

// intOrDefault parses an <input type="number"> field to an integer,
// falling back to def for an empty/non-numeric value — unlike `parseInt(v)
// || def`, this doesn't misfire on a field whose valid, deliberately-typed
// value is 0 (e.g. ranking.context_chunks_before, where 0 is a real
// setting distinct from its negative "unlimited" default).
function intOrDefault(value, def) {
  const n = parseInt(value, 10);
  return Number.isNaN(n) ? def : n;
}

// isDryRun reads the Import tab's single "Dry-Run" toggle (see
// tab-import.html) — every import submit handler below reads this once,
// right when it fires the request, rather than each card having its own
// checkbox to remember to tick. Missing element (toggle not in the DOM,
// e.g. a future page reorganization) defaults to false — never silently
// simulate when the control that says so isn't actually present.
function isDryRun() {
  const el = $("#dryRunToggle");
  return !!(el && el.checked);
}

// dryRunPrefix renders a leading "🧪 Simulation: " marker on a result line
// when result.dry_run is set (see baseImportResult in connector.go) — the
// single, consistent way every import result renderer below flags "nothing
// was actually written" instead of leaving that to be inferred from context.
function dryRunPrefix(result) {
  return result && result.dry_run ? "🧪 Simulation — nichts gespeichert: " : "";
}

// wireSelectionControls supplies the identical checkbox behavior used by
// import preview pickers: update their summary after a manual change and let
// their "all" / "none" buttons select only the checkboxes that picker marks
// as selectable. Each importer keeps ownership of its own summary wording.
function wireSelectionControls({ list, selectAll, selectNone, update, checkbox = "input[type=checkbox]" }) {
  const boxes = () => $all(`${list} ${checkbox}`);
  $(list).addEventListener("change", (e) => {
    if (e.target.matches(checkbox)) update();
  });
  $(selectAll).addEventListener("click", () => {
    boxes().forEach(box => { box.checked = true; });
    update();
  });
  $(selectNone).addEventListener("click", () => {
    boxes().forEach(box => { box.checked = false; });
    update();
  });
}

// Hydrate data-i18n* markup immediately with the default locale (see
// i18n.js) — loadSettings() below calls setLocale() again once it knows
// settings.lang, but the page shouldn't sit untranslated until that
// network round-trip finishes.
applyI18n();

// apiRequest wraps fetch with the app's error convention and preserves the
// response headers for the few callers that need transport metadata (notably
// Settings' optimistic-concurrency revision). Most callers should keep using
// api below and receive only the decoded data.
async function apiRequest(path, opts) {
  const res = await fetch(path, opts);
  const text = await res.text();
  let data;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) {
    const message = typeof data === "string" ? data : (data && data.error) || res.statusText;
    // "login required" is requireAdminSession/requireSessionIfLDAP's exact
    // wording (session.go) for either an expired/invalid session cookie
    // (e.g. the server restarted since login — the HMAC signing secret is
    // process-lifetime, not persisted, see session.go's doc comment) or a
    // valid session that simply isn't an admin. Either way the sidebar's
    // cached isLoggedIn/isAdmin is now stale — resync it the same way
    // doLogout() already does, instead of leaving "Abmelden (Name)" shown
    // right next to an action the server just rejected.
    if (ldapEnabled && message === "login required" && (isLoggedIn || isAdmin)) {
      doLogout();
      throw new Error(t("session.expired"));
    }
    const error = new Error(message);
    error.status = res.status;
    error.headers = res.headers;
    throw error;
  }
  return { data, headers: res.headers, status: res.status };
}

// api is the compact form used by the rest of the UI.
async function api(path, opts) {
  return (await apiRequest(path, opts)).data;
}

// escapeHTML escapes text before it's inserted via innerHTML, so
// user/document content can't break markup or inject scripts.
function escapeHTML(s) {
  return String(s)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

// ---- Markdown renderer (assistant messages) -------------------------
// citations[] is passed so [Q1]/[Q2] inline markers can be resolved to
// actual source names and URLs at render time. During streaming (before
// the "done" message with citations arrives) pass [] and leave final
// false — unresolved markers render as a plain placeholder chip, since
// citations just haven't arrived yet. Once "done" arrives, pass the final
// citations *and* final=true: a marker that still doesn't resolve then was
// deliberately dropped by the backend (filterCitations in rank.go — either
// unused by the model or hidden for its source_kind, see settings.
// SourceVisibility) and is removed from the rendered text entirely rather
// than shown as a dangling, unexplained chip.
function renderMarkdown(raw, citations, final) {
  citations = citations || [];
  final = !!final;
  const byMarker = {};
  citations.forEach(c => { if (c.marker) byMarker[c.marker] = c; });
  let s = escapeHTML(raw);

  // Fenced code blocks — stash them so inner content is not further
  // processed. The language tag is preserved as data-lang so
  // enhanceRenderedContent() (run on the final render) can route
  // ```mermaid / ```d3 / ```json / ```xml blocks to their respective
  // renderers, and give every other block a language label + copy button.
  // The (already-HTML-escaped, since we escaped the whole string above)
  // source stays verbatim inside <code>, so a later textContent read
  // recovers the exact original text to feed those renderers.
  const codeBlocks = [];
  s = s.replace(/```([a-zA-Z0-9_+#.-]*)\n?([\s\S]*?)```/g, (_, lang, code) => {
    const attr = lang ? ` data-lang="${escapeHTML(lang.toLowerCase())}"` : "";
    codeBlocks.push(`<pre class="r3-code"${attr}><code>${code.replace(/\n$/, "")}</code></pre>`);
    return `\x00CODE${codeBlocks.length - 1}\x00`;
  });

  // Inline code
  s = s.replace(/`([^`\n]+)`/g, "<code>$1</code>");

  // Inline citations [Q1], [Q2] … → styled chips, linked if citation has URL.
  s = s.replace(/\[Q(\d+)\]/gi, (_, n) => {
    const cit = byMarker[n];
    const label = `Q${n}`;
    if (cit) {
      const title = ` title="${escapeHTML(cit.source_name)}"`;
      if (cit.source_url) {
        return `<a class="inline-cite" href="${escapeHTML(cit.source_url)}" target="_blank" rel="noopener"${title} data-cite="${n}">${label}</a>`;
      }
      return `<span class="inline-cite" data-cite="${n}"${title}>${label}</span>`;
    }
    // No matching citation: during streaming that just means "done" hasn't
    // arrived yet (show a plain placeholder); once final, it means the
    // backend deliberately dropped this source (unused or hidden by
    // source-kind visibility) — drop the marker from view too.
    return final ? "" : `<span class="inline-cite" data-cite="${n}">${label}</span>`;
  });

  // Bold, italic, strikethrough
  s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  s = s.replace(/(?<![*\w])\*([^*\n]+)\*(?!\w)/g, "<em>$1</em>");
  s = s.replace(/(?<![_\w])_([^_\n]+)_(?!\w)/g, "<em>$1</em>");
  s = s.replace(/~~([^~\n]+)~~/g, "<del>$1</del>");

  // Links
  s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g,
    '<a href="$2" target="_blank" rel="noopener">$1</a>');

  // Markdown tables: detect blocks where every line is a pipe-delimited row.
  // We work on the whole string before splitting into paragraphs. The
  // separator row's colons encode per-column alignment (|:--|:-:|--:|).
  s = s.replace(/((^|\n)([ \t]*\|[^\n]+\n?)+)/g, (tableBlock) => {
    const lines = tableBlock.trim().split("\n").map(l => l.trim()).filter(Boolean);
    if (lines.length < 2) return tableBlock;
    const sepIdx = lines.findIndex(l => /^\|[-| :]+\|$/.test(l));
    if (sepIdx !== 1) return tableBlock;
    const parseRow = (line) =>
      line.replace(/^\|/, "").replace(/\|$/, "").split("|").map(c => c.trim());
    const aligns = parseRow(lines[1]).map(spec => {
      const l = spec.startsWith(":"), r = spec.endsWith(":");
      return r && l ? "center" : r ? "right" : l ? "left" : "";
    });
    const alignAttr = (i) => aligns[i] ? ` style="text-align:${aligns[i]}"` : "";
    const headers = parseRow(lines[0]);
    const body = lines.slice(2);
    const ths = headers.map((h, i) => `<th${alignAttr(i)}>${h}</th>`).join("");
    const trs = body.map(l => {
      const cells = parseRow(l);
      return `<tr>${cells.map((c, i) => `<td${alignAttr(i)}>${c}</td>`).join("")}</tr>`;
    }).join("");
    return `<div class="r3-table-wrap"><table><thead><tr>${ths}</tr></thead><tbody>${trs}</tbody></table></div>`;
  });

  // Paragraphs / headings / lists / blockquotes / rules
  const blocks = s.split(/\n{2,}/).map(block => {
    const lines = block.split("\n").filter(l => l.trim() !== "");
    if (!lines.length) return "";
    // Already a table or code placeholder — pass through
    if (lines[0].trim().startsWith("<div class=\"r3-table-wrap\"") || lines[0].startsWith("\x00CODE")) {
      return lines.join("\n");
    }
    // Horizontal rule — a block that is only --- / *** / ___
    if (lines.length === 1 && /^\s*([-*_])\1{2,}\s*$/.test(lines[0])) {
      return "<hr>";
    }
    // Blockquote — every line starts with > (already HTML-escaped to &gt;
    // by the escapeHTML pass at the top, so match that form).
    if (lines.every(l => /^\s*&gt;\s?/.test(l))) {
      return `<blockquote>${lines.map(l => l.replace(/^\s*&gt;\s?/, "")).join("<br>")}</blockquote>`;
    }
    // Task list — every line "- [ ]" / "- [x]"
    if (lines.every(l => /^[-*]\s+\[[ xX]\]\s+/.test(l))) {
      const items = lines.map(l => {
        const checked = /^[-*]\s+\[[xX]\]/.test(l);
        return `<li class="md-task"><input type="checkbox" disabled${checked ? " checked" : ""}> ${l.replace(/^[-*]\s+\[[ xX]\]\s+/, "")}</li>`;
      }).join("");
      return `<ul class="md-task-list">${items}</ul>`;
    }
    // Bullet list
    if (lines.every(l => /^[-*]\s+/.test(l))) {
      return `<ul>${lines.map(l => `<li>${l.replace(/^[-*]\s+/, "")}</li>`).join("")}</ul>`;
    }
    // Ordered list
    if (lines.every(l => /^\d+\.\s+/.test(l))) {
      return `<ol>${lines.map(l => `<li>${l.replace(/^\d+\.\s+/, "")}</li>`).join("")}</ol>`;
    }
    // Mixed prose: honour heading lines (# … ######) inline, everything else
    // accumulates into paragraphs (single newlines become <br>).
    const out = [];
    let para = [];
    const flush = () => { if (para.length) { out.push(`<p>${para.join("<br>")}</p>`); para = []; } };
    for (const l of lines) {
      const h = l.match(/^(#{1,6})\s+(.*)$/);
      if (h) { flush(); out.push(`<h${h[1].length} class="md-h">${h[2].trim()}</h${h[1].length}>`); }
      else { para.push(l); }
    }
    flush();
    return out.join("");
  });
  s = blocks.join("");

  // Restore stashed code blocks
  s = s.replace(/\x00CODE(\d+)\x00/g, (_, i) => codeBlocks[Number(i)]);
  return s;
}

// renderInto sets el's HTML from renderMarkdown, then — only on the FINAL
// render (never mid-stream, where re-running heavy work each token would
// flicker and waste effort) — upgrades any special code blocks in place:
// JSON/XML get syntax highlighting, ```mermaid becomes a diagram, ```d3 a
// sandboxed chart, every other fenced block a language label + copy button.
// Use this instead of assigning innerHTML = renderMarkdown(...) directly so
// the rich rendering reaches every surface (Chat, Agent, saved history, the
// Mail draft preview) uniformly.
function renderInto(el, raw, citations, final) {
  if (!el) return;
  el.innerHTML = raw ? renderMarkdown(raw, citations, final) : "";
  if (final && raw) enhanceRenderedContent(el);
}

// ---- Rich code-block rendering (post-processing the markdown output) ----
// Config: which URLs the browser loads the optional Mermaid / d3 libraries
// from. Seeded with working CDN defaults so rendering works out of the box,
// then overridden by /api/auth/status's "render" block (settings.go's
// renderConfig) — an air-gapped deployment points these at a self-hosted
// copy, or clears one to disable that renderer (its source then just shows
// as a labeled code block). Read lazily at render time, so a changed
// setting takes effect without reload once auth-status has resolved.
window.R3_RENDER = window.R3_RENDER || {
  mermaidUrl: "https://cdn.jsdelivr.net/npm/mermaid@11/+esm",
  d3Url: "https://cdn.jsdelivr.net/npm/d3@7/dist/d3.min.js",
};

function currentRenderTheme() {
  return document.documentElement.getAttribute("data-theme") === "light" ? "light" : "dark";
}

function enhanceRenderedContent(el) {
  if (!el) return;
  el.querySelectorAll("pre.r3-code").forEach(pre => {
    if (pre.dataset.enhanced) return;
    pre.dataset.enhanced = "1";
    const lang = (pre.dataset.lang || "").toLowerCase();
    const codeEl = pre.querySelector("code");
    const src = codeEl ? codeEl.textContent : pre.textContent;
    if (lang === "mermaid") { renderMermaidBlock(pre, src); return; }
    if (lang === "d3" || lang === "d3js") { renderD3Block(pre, src); return; }
    if (codeEl) {
      if (lang === "json") {
        const pretty = formatJSON(src);
        if (pretty !== null) codeEl.innerHTML = highlightJSON(pretty);
      } else if (lang === "xml" || lang === "html" || lang === "svg") {
        codeEl.innerHTML = highlightXML(src);
      }
    }
    decorateCodeBlock(pre, lang || "text", src);
  });
}

// decorateCodeBlock wraps a <pre> with a header carrying the language label
// and a copy button (copies the verbatim source, not the highlighted HTML).
function decorateCodeBlock(pre, lang, src) {
  if (pre.parentNode && pre.parentNode.classList && pre.parentNode.classList.contains("r3-code-wrap")) return;
  const wrap = document.createElement("div");
  wrap.className = "r3-code-wrap";
  const head = document.createElement("div");
  head.className = "r3-code-head";
  const label = document.createElement("span");
  label.className = "r3-code-lang";
  label.textContent = lang;
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "r3-code-copy";
  btn.textContent = t("chat.codeCopy.label");
  btn.addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(src); btn.textContent = t("chat.codeCopy.copied"); setTimeout(() => { btn.textContent = t("chat.codeCopy.label"); }, 1200); }
    catch { btn.textContent = t("chat.codeCopy.error"); }
  });
  head.appendChild(label);
  head.appendChild(btn);
  pre.parentNode.insertBefore(wrap, pre);
  wrap.appendChild(head);
  wrap.appendChild(pre);
}

// formatJSON returns canonical 2-space-indented JSON, or null if the source
// isn't valid JSON (then it's left as a plain code block, unreformatted).
function formatJSON(src) {
  try { return JSON.stringify(JSON.parse(src), null, 2); } catch { return null; }
}

// highlightJSON tokenises canonical JSON (the output of formatJSON) into
// class-tagged spans. Operates on structurally-valid JSON, so the only
// HTML-special characters can be inside string tokens, which are escaped
// per-match; the structural gaps ({ } [ ] : , whitespace) are HTML-safe.
function highlightJSON(text) {
  return text.replace(
    /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g,
    (m, str, colon, kw, num) => {
      if (str !== undefined) {
        const e = escapeHTML(str);
        return colon ? `<span class="tok-key">${e}</span>${colon}` : `<span class="tok-str">${e}</span>`;
      }
      if (kw !== undefined) return `<span class="tok-kw">${kw}</span>`;
      if (num !== undefined) return `<span class="tok-num">${num}</span>`;
      return m;
    }
  );
}

// highlightXML escapes first (so this is XSS-safe regardless of content) then
// tags comments, element names and attributes. Highlight-only: it never
// reindents, so whitespace-significant XML content is preserved exactly.
function highlightXML(src) {
  let e = escapeHTML(src);
  e = e.replace(/&lt;!--[\s\S]*?--&gt;/g, m => `<span class="tok-comment">${m}</span>`);
  e = e.replace(/(&lt;\/?)([\w:.-]+)((?:[^&]|&(?!gt;))*?)(\/?&gt;)/g, (m, open, name, attrs, close) => {
    const hl = attrs.replace(/([\w:.-]+)=(&quot;[\s\S]*?&quot;|&#39;[\s\S]*?&#39;)/g,
      (mm, an, av) => `<span class="tok-attr">${an}</span>=<span class="tok-str">${av}</span>`);
    return `${open}<span class="tok-tag">${name}</span>${hl}${close}`;
  });
  return e;
}

// ---- Mermaid (lazy ES-module import, cached) ----------------------------
let _mermaidPromise = null;
function loadMermaid() {
  if (_mermaidPromise) return _mermaidPromise;
  const url = window.R3_RENDER && window.R3_RENDER.mermaidUrl;
  if (!url) return Promise.reject(new Error("mermaid disabled"));
  _mermaidPromise = import(url).then(mod => {
    const mermaid = (mod && (mod.default || mod.mermaid)) || window.mermaid;
    if (!mermaid || typeof mermaid.render !== "function") throw new Error("mermaid module invalid");
    return mermaid;
  });
  return _mermaidPromise;
}
let _mermaidSeq = 0;
function renderMermaidBlock(pre, code) {
  loadMermaid().then(mermaid => {
    // securityLevel:"strict" makes mermaid sanitise labels and forbid inline
    // scripts/HTML in the diagram — the AI's diagram text is data, not markup.
    mermaid.initialize({ startOnLoad: false, securityLevel: "strict", theme: currentRenderTheme() === "light" ? "default" : "dark" });
    const id = "r3mmd" + (_mermaidSeq++);
    return Promise.resolve(mermaid.render(id, code)).then(res => {
      const svg = typeof res === "string" ? res : (res && res.svg);
      if (!svg) throw new Error("empty render");
      const fig = document.createElement("figure");
      fig.className = "r3-mermaid";
      fig.innerHTML = svg;
      pre.replaceWith(fig);
    });
  }).catch(() => {
    // Loading blocked/offline, or the diagram didn't parse: keep the source
    // visible as a labeled code block with a small note, never a blank gap.
    decorateCodeBlock(pre, "mermaid", code);
    appendCodeNote(pre, "Diagramm konnte nicht gerendert werden — Quelltext oben.");
  });
}

// ---- d3.js (sandboxed iframe — runs AI-authored JS in isolation) --------
// sandbox="allow-scripts" WITHOUT allow-same-origin gives the iframe an
// opaque origin: the code can draw a chart but cannot read cookies/session,
// touch the parent DOM, navigate the top window, or submit forms. The only
// channel back is a postMessage carrying its content height, which the
// listener below uses to size the frame.
function renderD3Block(pre, code) {
  const url = window.R3_RENDER && window.R3_RENDER.d3Url;
  if (!url) { decorateCodeBlock(pre, "d3", code); appendCodeNote(pre, "d3.js-Rendering ist deaktiviert — Quelltext oben."); return; }
  const fg = currentRenderTheme() === "light" ? "#1a1a1a" : "#e6e6e6";
  // Neutralise any literal </script> in the AI code so it can't break out of
  // the injected <script> element (belt-and-braces on top of the sandbox).
  const safeCode = String(code).replace(/<\/script>/gi, "<\\/script>");
  const iframe = document.createElement("iframe");
  iframe.className = "r3-d3";
  iframe.setAttribute("sandbox", "allow-scripts");
  iframe.setAttribute("title", "d3.js-Visualisierung");
  // Keep the verbatim source on the element so the message listener can swap
  // the frame back to a code block if d3 itself fails to load (offline /
  // air-gapped) — matching Mermaid's fallback, so the source is never lost.
  iframe.__r3d3src = String(code);
  iframe.srcdoc = `<!doctype html><html><head><meta charset="utf-8"><style>
      html,body{margin:0;padding:8px;background:transparent;color:${fg};font-family:system-ui,-apple-system,sans-serif;font-size:13px;}
      svg{max-width:100%;height:auto;} .r3d3-err{color:#d33;white-space:pre-wrap;font-family:monospace;font-size:12px;}
    </style></head><body><div id="viz"></div>
    <script src="${escapeHTML(url)}"><\/script>
    <script>(function(){
      function post(extra){try{var m={__r3d3:1,height:document.documentElement.scrollHeight};if(extra)for(var k in extra)m[k]=extra[k];parent.postMessage(m,"*");}catch(e){}}
      function run(){
        if(typeof d3==="undefined"){post({failed:1});return;}
        try{${safeCode}}catch(e){document.body.innerHTML='<div class="r3d3-err">'+String((e&&e.message)||e)+'<\/div>';}
        post();setTimeout(post,300);setTimeout(post,1200);
      }
      if(document.readyState!=="loading")run();else document.addEventListener("DOMContentLoaded",run);
    })();<\/script></body></html>`;
  pre.replaceWith(iframe);
}

// d3FrameFallback replaces a d3 iframe that couldn't load its library with the
// verbatim source as a labeled code block (kept on iframe.__r3d3src) plus a
// note — the same "never lose the source" behavior renderMermaidBlock has.
function d3FrameFallback(iframe) {
  const src = iframe.__r3d3src || "";
  const pre = document.createElement("pre");
  pre.className = "r3-code";
  pre.setAttribute("data-lang", "d3");
  const codeEl = document.createElement("code");
  codeEl.textContent = src;
  pre.appendChild(codeEl);
  iframe.replaceWith(pre);
  decorateCodeBlock(pre, "d3", src);
  appendCodeNote(pre, "Diagramm konnte nicht geladen werden — Quelltext oben.");
}

// One shared listener resizes each d3 iframe to its reported content height
// (clamped), matched to the frame by comparing contentWindow to the message
// source (the frame has an opaque origin, so event.origin is "null").
if (!window.__r3d3Listener) {
  window.__r3d3Listener = true;
  window.addEventListener("message", (ev) => {
    const d = ev.data;
    if (!d || !d.__r3d3) return;
    document.querySelectorAll("iframe.r3-d3").forEach(f => {
      if (f.contentWindow !== ev.source) return;
      if (d.failed) { d3FrameFallback(f); return; }
      const h = Math.max(80, Math.min(1400, Number(d.height) || 0));
      if (h) f.style.height = h + "px";
    });
  });
}

// appendCodeNote drops a small caption under a (fallback) code block.
function appendCodeNote(pre, text) {
  const target = (pre.parentNode && pre.parentNode.classList.contains("r3-code-wrap")) ? pre.parentNode : pre;
  const note = document.createElement("div");
  note.className = "r3-code-note hint";
  note.textContent = text;
  target.appendChild(note);
}

// ---- Theme switcher -------------------------------------------------
// "auto" tracks prefers-color-scheme live — the *choice* ("auto") is what
// gets persisted/highlighted, while data-theme on <html> always holds the
// currently *resolved* dark/light value the stylesheet actually keys off.
const THEMES = ["dark", "light", "ocean", "rubix", "solarized", "win2k", "auto"];
let themeChoice = "rubix";
// resolveTheme turns the user's stored choice into an actual theme name,
// resolving "auto" to "dark"/"light" based on the OS preference at call time.
function resolveTheme(choice) {
  if (choice !== "auto") return choice;
  const prefersDark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
  return prefersDark ? "dark" : "light";
}
// applyTheme sets data-theme on <html> (which the CSS keys off of), persists
// the raw choice (including "auto") to localStorage, and syncs the theme
// menu's toggle (swatch + current name) and option list (active state,
// aria-checked) to match.
function applyTheme(choice) {
  if (!THEMES.includes(choice)) choice = "rubix";
  themeChoice = choice;
  document.documentElement.setAttribute("data-theme", resolveTheme(choice));
  try { localStorage.setItem("r3_theme", choice); } catch {}
  $all(".theme-menu-option").forEach(btn => {
    const active = btn.dataset.t === choice;
    btn.classList.toggle("active", active);
    btn.setAttribute("aria-checked", String(active));
  });
  const toggleSwatch = $("#themeMenuToggleSwatch");
  if (toggleSwatch) toggleSwatch.dataset.theme = resolveTheme(choice);
  const currentName = $("#themeMenuCurrentName");
  if (currentName) currentName.textContent = t(`theme.${choice}`);
}
applyTheme((() => { try { return localStorage.getItem("r3_theme"); } catch {} return null; })() || "rubix");
$all(".theme-menu-option").forEach(btn => btn.addEventListener("click", () => {
  applyTheme(btn.dataset.t);
  closeThemeMenu();
}));
if (window.matchMedia) {
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
    if (themeChoice === "auto") applyTheme("auto");
  });
}

// ---- Theme menu popover open/close ------------------------------------
// Same "toggle, outside click closes, Escape closes" shape as every other
// popover-like control in the app — no shared helper exists yet, so this
// stays self-contained rather than introducing one for a single caller.
function openThemeMenu() {
  $("#themeMenuPopover").hidden = false;
  $("#themeMenuToggle").setAttribute("aria-expanded", "true");
}
function closeThemeMenu() {
  $("#themeMenuPopover").hidden = true;
  $("#themeMenuToggle").setAttribute("aria-expanded", "false");
}
$("#themeMenuToggle").addEventListener("click", () => {
  if ($("#themeMenuPopover").hidden) openThemeMenu(); else closeThemeMenu();
});
document.addEventListener("click", (e) => {
  if (!$("#themeMenuPopover").hidden && !e.target.closest(".theme-menu")) closeThemeMenu();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !$("#themeMenuPopover").hidden) { closeThemeMenu(); $("#themeMenuToggle").focus(); }
});

// ---- Font-size switcher ----------------------------------------------
const FONT_SIZES = ["small", "normal", "large", "larger", "huge"];
// applyFontSize sets data-font-size on <html> (omitted for the "normal"
// default so existing CSS needs no extra selector for it), persists the
// choice, and syncs the sidebar buttons' active/aria-pressed state.
function applyFontSize(size) {
  if (!FONT_SIZES.includes(size)) size = "normal";
  if (size === "normal") {
    document.documentElement.removeAttribute("data-font-size");
  } else {
    document.documentElement.setAttribute("data-font-size", size);
  }
  try { localStorage.setItem("r3_font_size", size); } catch {}
  $all(".font-size-btn").forEach(btn => {
    const active = btn.dataset.fs === size;
    btn.classList.toggle("active", active);
    btn.setAttribute("aria-pressed", String(active));
  });
}
applyFontSize((() => { try { return localStorage.getItem("r3_font_size"); } catch {} return null; })() || "normal");
$all(".font-size-btn").forEach(btn => btn.addEventListener("click", () => applyFontSize(btn.dataset.fs)));

// ---- Language switcher --------------------------------------------------
// The admin's Settings-tab #s_lang picker (loadSettings() further down)
// sets the server-wide *default* language (settings.go's Lang) — this is
// the personal *override* on top of it. Mirrors applyTheme/applyFontSize
// above for the "no session, or LDAP off" case (pure localStorage, no
// server round-trip); when a real session exists it additionally persists
// server-side via userprefs.go/POST /api/account/prefs/set, so it follows
// a logged-in user across devices — the /api/auth/status handler below
// resolves that same override (handlers.go's handleAuthStatus mirrors
// this exact precedence) into its `lang` field on every page load.
const LANG_OVERRIDE_KEY = "r3_lang_override";
function applyLangSwitcherUI(lang) {
  $all(".lang-btn").forEach(btn => {
    const active = btn.dataset.lang === lang;
    btn.classList.toggle("active", active);
    btn.setAttribute("aria-pressed", String(active));
  });
}
// Applied immediately at parse time, before any network round-trip — same
// "avoid a flash of the wrong language before the async fetch resolves"
// reasoning as applyTheme/applyFontSize above.
(() => {
  let stored = null;
  try { stored = localStorage.getItem(LANG_OVERRIDE_KEY); } catch {}
  if (stored) { setLocale(stored); applyLangSwitcherUI(stored); }
})();
$all(".lang-btn").forEach(btn => btn.addEventListener("click", () => {
  const lang = btn.dataset.lang;
  try { localStorage.setItem(LANG_OVERRIDE_KEY, lang); } catch {}
  setLocale(lang);
  applyLangSwitcherUI(lang);
  // setLocale/applyI18n only re-translates static data-i18n markup — content
  // built from a template literal at fetch time (Chat's KB-size hint and
  // empty-state suggestion chips, loadStats() below) stays in whatever
  // language was active when it last rendered otherwise. Both are read-only
  // display, safe to silently re-fetch/re-render on every language switch —
  // unlike e.g. loadPrompts(), which would blow away an admin's unsaved
  // textarea edits, so that one is deliberately NOT refreshed here.
  loadStats();
  // isLoggedIn is declared further down (Admin gating section) but already
  // initialized by the time a user can actually click this — the script
  // has finished its top-level run long before any click handler fires.
  if (isLoggedIn) {
    api("/api/account/prefs/set", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ lang }),
    }).catch(() => { /* best-effort — localStorage already applied it locally */ });
  }
}));

// ---- Collapsible sidebar (desktop) ------------------------------------
// Icon-only mode for people who know the icons and want the width back
// for content — persisted like theme/font size. Desktop only: the CSS
// scopes the collapsed rules to >860px, so the mobile off-canvas drawer
// always opens full-width regardless of this state.
const SIDEBAR_COLLAPSED_KEY = "r3_sidebar_collapsed";
function applySidebarCollapsed(collapsed) {
  document.body.classList.toggle("sidebar-collapsed", collapsed);
  const btn = $("#sidebarCollapse");
  const label = collapsed ? t("sidebar.expand.label") : t("sidebar.collapse.label");
  btn.setAttribute("aria-pressed", String(collapsed));
  btn.setAttribute("aria-label", label);
  btn.title = label;
}
applySidebarCollapsed((() => { try { return localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === "1"; } catch {} return false; })());
$("#sidebarCollapse").addEventListener("click", () => {
  const collapsed = !document.body.classList.contains("sidebar-collapsed");
  try { localStorage.setItem(SIDEBAR_COLLAPSED_KEY, collapsed ? "1" : "0"); } catch {}
  applySidebarCollapsed(collapsed);
});
// Every nav item carries its label as a native tooltip so the icon-only
// mode stays self-explanatory (and hovering helps in expanded mode too).
// Items that already manage a title via data-i18n-title (e.g. Verlauf)
// are left alone.
$all(".nav-item").forEach(b => {
  if (b.title || b.dataset.i18nTitle) return;
  const label = b.querySelector("span");
  if (label) b.title = label.textContent;
});

// ---- Mobile sidebar --------------------------------------------------
(function () {
  const sidebar = $("#sidebar");
  const toggle = $("#menuToggle");
  const backdrop = $("#sidebarBackdrop");
  // setOpen toggles the mobile sidebar drawer and keeps the backdrop and
  // toggle button's aria-expanded state in sync with it.
  function setOpen(open) {
    sidebar.classList.toggle("open", open);
    backdrop.hidden = !open;
    toggle.setAttribute("aria-expanded", String(open));
  }
  toggle.addEventListener("click", () => setOpen(!sidebar.classList.contains("open")));
  backdrop.addEventListener("click", () => setOpen(false));
  // Selecting a nav item on mobile closes the drawer.
  sidebar.addEventListener("click", (e) => {
    if (e.target.closest(".nav-item") && window.matchMedia("(max-width: 860px)").matches) {
      setOpen(false);
    }
  });
})();

// ---- Knowledge base stats (hero + sidebar) ---------------------------
async function loadStats() {
  try {
    const s = await api("/api/stats");
    const fmt = (n) => Number(n).toLocaleString("de-DE");
    $("#kbStats").innerHTML = t("sidebar.kbStats.text", { chunks: `<strong>${fmt(s.chunks)}</strong>`, sources: `<strong>${fmt(s.sources)}</strong>` });
    if (s.chunks > 0) {
      $("#chatEmptyStats").innerHTML = t("chat.emptyStats.withData", { chunks: `<strong>${fmt(s.chunks)}</strong>`, sources: `<strong>${fmt(s.sources)}</strong>` });
    } else {
      $("#chatEmptyStats").textContent = t("chat.emptyStats.empty");
    }
    renderChatSuggestions(s.chunks > 0 ? (s.kinds || []) : []);
  } catch { /* stats are cosmetic — ignore */ }
}

// SUGGESTION_TEMPLATES maps a source_kind (as reported by /api/stats'
// `kinds`, most-populated first — see handleStats, handlers.go) to the
// i18n key of a matching starter question. The three *_attachment kinds
// share one template; unknown kinds are simply skipped.
const SUGGESTION_TEMPLATES = {
  pst_email:           "chat.suggest.pstEmail",
  pst_attachment:      "chat.suggest.emailAttachment",
  imap_attachment:     "chat.suggest.emailAttachment",
  outlook_attachment:  "chat.suggest.emailAttachment",
  jira_issue:          "chat.suggest.jiraIssue",
  freshservice_ticket: "chat.suggest.freshserviceTicket",
  confluence_page:     "chat.suggest.confluencePage",
  teams_message:       "chat.suggest.teamsMessage",
  sharepoint_file:     "chat.suggest.sharepointFile",
  file:                "chat.suggest.documents",
  web:                 "chat.suggest.webPage",
  web_page:            "chat.suggest.webPage",
};
const SUGGESTION_FALLBACKS = ["chat.suggest.fallbackSummary", "chat.suggest.fallbackTopics"];

// renderChatSuggestions fills the empty state's starter-question chips
// from what's actually in the knowledge base — a chip per (deduped)
// source kind, padded with generic fallbacks up to 4, clicking asks the
// question directly. Re-run on every loadStats() call, so a language
// switch re-translates and a first import replaces the fallbacks.
function renderChatSuggestions(kinds) {
  const box = $("#chatSuggestions");
  if (!box) return;
  const keys = [];
  for (const kind of kinds) {
    const key = SUGGESTION_TEMPLATES[kind];
    if (key && !keys.includes(key)) keys.push(key);
    if (keys.length >= 4) break;
  }
  for (const key of SUGGESTION_FALLBACKS) {
    if (keys.length >= 4) break;
    if (kinds.length && !keys.includes(key)) keys.push(key);
  }
  box.innerHTML = "";
  box.hidden = keys.length === 0;
  keys.forEach((key) => {
    const chip = document.createElement("button");
    chip.type = "button";
    chip.className = "suggestion";
    chip.textContent = t(key);
    chip.addEventListener("click", () => askQuestion(chip.textContent));
    box.appendChild(chip);
  });
}
loadStats();

// ---- Access presets (Chat preset dropdown) -----------------------------
// Populates #askPreset from the public, ungated /api/presets endpoint
// (name/display_name only — kinds/tools stay admin-internal, see
// handlePresets in handlers.go). Absent or empty means no presets are
// configured; the dropdown then stays at its single "Standard" option.
async function loadPresets() {
  try {
    const presets = await api("/api/presets");
    const sel = $("#askPreset");
    if (!sel || !Array.isArray(presets) || presets.length === 0) return;
    for (const p of presets) {
      const opt = document.createElement("option");
      opt.value = p.name;
      opt.textContent = p.display_name || p.name;
      sel.appendChild(opt);
    }
  } catch { /* presets are optional — ignore */ }
}
loadPresets();

// ---- Admin gating ---------------------------------------------------
// Two login modes coexist: the original shared-password UI gate
// (localStorage-only, never verified server-side — see handleAdminCheck's
// doc comment) and, once enabled under Einstellungen -> LDAP, real
// Active-Directory login with a server-verified session (see
// ldapauth.go/session.go). /api/auth/status tells the frontend which mode
// is active; legacy mode is untouched when LDAP is off.
//
// In LDAP mode, being logged in and being an admin are now separate:
// login is also how a regular employee identifies themselves for
// department-restricted content and answer personalization (see
// Einstellungen -> "Zugriffskontrolle je Quelltyp"), so any account that
// can bind gets a session. isAdmin (from the server's is_admin claim)
// is the only thing that actually shows admin-only UI; isLoggedIn just
// tracks whether the toggle button should say "Abmelden" at all.
let isAdmin = localStorage.getItem("r3_admin") === "1";
let isLoggedIn = false;
let currentUserLabel = "";
let ldapEnabled = false;
// localAuthEnabled mirrors settings.go's LocalAuth.Enabled — a second,
// independent way a real per-account session can exist (localusers.go),
// alongside (or instead of) LDAP. authTierActive below is what every other
// piece of UI logic should actually check ("is SOME real login system
// active", the same distinction handlers.go's authTierActive draws
// server-side) — ldapEnabled/localAuthEnabled individually only matter for
// LDAP- or local-auth-specific UI (e.g. which login fields to show, or the
// LDAP settings section itself).
let localAuthEnabled = false;
let authTierActive = false;
// smtpEnabled mirrors settings.go's SMTP.Enabled (via /api/auth/status) so
// the "An mich senden" button (Chat and Mail tab) can hide itself when
// email isn't configured, instead of always showing and only failing on
// click (handleChatEmail's "email is not configured" 400).
let smtpEnabled = false;
// currentUserClaims backs the "Mein Konto" modal (docs/UI_HARDENING_PLAN.md)
// with the fuller profile /api/auth/status and the login response already
// carry — currentUserLabel above stays as the compact sidebar-tooltip
// form, this is the same data unflattened for a multi-line display.
let currentUserClaims = null;
// See the dedicated "Chat history" section below for what these drive —
// declared here (rather than there) so the /api/auth/status handler a few
// lines down can assign chatHistoryEnabled before that section's own code
// runs (a `let` can't be referenced before its declaration executes).
let chatHistoryEnabled = false;
// uploadImageMode mirrors uploadConfig.ImageMode's *effective* value
// ("vision" or "ocr", settings.go/chatimages.go's effectiveUploadImageMode)
// — an explicit admin policy, not per-profile guesswork. Set from
// /api/auth/status below, read by updateAttachHints (Chat's image-attach
// UI) to show "wird vom Vision-Modell gelesen" vs. "wird per OCR
// umgewandelt" without needing admin-only /api/settings access.
let uploadImageMode = "ocr";
let currentConversation = null;
// sessionMessages is always maintained (independent of chatHistoryEnabled,
// which only controls localStorage *persistence* across page loads) — it
// gives the current in-page conversation multi-turn memory: each /api/ask
// call sends it as `history` so a follow-up question ("what about last
// month?") has the prior turns to resolve against. Plain {role, content}
// pairs, no citations — the model only needs the text it already said,
// not the source metadata behind it.
let sessionMessages = [];
// Default matches handlers.go's askHistoryMaxDefault — overwritten below
// from /api/auth/status's "history_max_turns" (the server's actual
// resolved appSettings.HistoryMaxTurns) as soon as that resolves, so an
// admin raising the setting takes effect here too instead of the browser
// silently keeping the old cap.
let SESSION_HISTORY_MAX = 12;

// isRegisteredTier reports whether the current visitor counts as
// docs/UI_HARDENING_PLAN.md's "Registriert" tier (any real login, not
// just admin) — with no login system configured at all (LDAP and local
// accounts both off), there's no such tier, so everyone stays at today's
// single "guest" level (true). Purely a UI declutter: the server enforces
// the real gate (requireSessionIfLDAP/
// resolveAskProfile/mssqlToolAllowed in handlers.go) independently, so
// hiding these controls only avoids a guest hitting a confusing 401,
// it isn't itself the security boundary.
function isRegisteredTier() {
  return !authTierActive || isLoggedIn;
}

// applyAdminVisibility shows/hides every .admin-only element (gated by
// isAdmin alone) and flips the toggle button's label/style — in LDAP
// mode that reflects isLoggedIn (any employee), in legacy mode it's the
// original admin-only toggle (isAdmin doubles as isLoggedIn there, same
// as before this feature).
//
// closeBox defaults to true (used by the initial call below, and by
// sign-out/successful-login) but must be false when called from the
// /api/auth/status resolution: that fetch fires unconditionally on every
// page load, racing against the user's own click on #adminToggle. Without
// this flag, a user who opens the login box gets it slammed shut out from
// under them the instant that unrelated status check resolves.
function applyAdminVisibility(closeBox = true) {
  $all(".admin-only").forEach(el => { el.hidden = !isAdmin; });
  const btn = $("#adminToggle");
  const lbl = $("#adminToggleLabel");
  const signedIn = authTierActive ? isLoggedIn : isAdmin;
  if (signedIn) {
    lbl.textContent = currentUserLabel ? `${t("login.signOut")} (${currentUserLabel})` : t("login.signOut");
    btn.classList.add("is-admin");
  } else {
    lbl.textContent = t("login.signIn");
    btn.classList.remove("is-admin");
  }
  if (closeBox) {
    btn.setAttribute("aria-expanded", "false");
    $("#adminLoginBox").hidden = true;
  }
  if (!isAdmin) {
    const activeTab = $(".nav-item.active");
    if (activeTab && activeTab.classList.contains("admin-only")) activateTab($("#navtab-chat"));
  }
  // "An mich senden" on the Mail tab's draft (see #mailDraftSendSelf below)
  // needs a real per-account session (claims.Mail) to have an address to
  // send to — isLoggedIn, not signedIn/isAdmin above, since the legacy
  // (non-LDAP) admin-password gate has no such per-account identity at all.
  // Also needs SMTP actually configured (settings.go's SMTP.Enabled, mirrored
  // here via smtpEnabled) — otherwise the button always showed but every
  // click just failed with "email is not configured" (handleChatEmail).
  const sendSelfBtn = $("#mailDraftSendSelf");
  if (sendSelfBtn) sendSelfBtn.hidden = !isLoggedIn || !smtpEnabled;

  applyAskProfileVisibility();
  updateMyAccountVisibility();
}

// configuredChatProfiles (from /api/auth/status) lists which cloud
// backends actually have a model + API key configured (settings.go's
// configuredChatProfiles) — picking one that isn't hits a confusing
// upstream auth error instead of a clear "not configured" message, so
// applyAskProfileVisibility hides those options instead. Azure additionally
// still needs the "Registriert" tier (docs/UI_HARDENING_PLAN.md) — the
// server enforces both independently (resolveAskProfile, handlers.go),
// this only avoids the guest/silent-fallback/401 surprise client-side.
//
// Declared (and initialized) before the applyAdminVisibility() call below,
// since that call reaches applyAskProfileVisibility() synchronously —
// declaring this after would leave it in the temporal dead zone at that
// point despite `function` hoisting covering applyAskProfileVisibility
// itself.
let configuredChatProfiles = [];
// availableSources/availableTools (settings.go's configuredSourceKinds/
// configuredToolKinds, via /api/auth/status) drive the Help tab's dynamic
// "which sources/tools does this deployment actually have active" sections
// (renderHelpAvailability below) — empty until that fetch resolves.
let availableSources = [];
let availableTools = [];

// SOURCE_CATALOG/TOOL_CATALOG map each backend-reported kind tag
// (settings.go's configuredSourceKinds/configuredToolKinds) to an icon plus
// the i18n keys for its display name/description — the one place a newly
// added connector/tool kind needs a new entry so the Help tab's "available"
// and "not yet active" sections both automatically pick it up.
const SOURCE_CATALOG = {
  sharepoint: { icon: "📁", nameKey: "help.catalog.sharepoint.name", descKey: "help.catalog.sharepoint.desc" },
  onedrive: { icon: "☁️", nameKey: "help.catalog.onedrive.name", descKey: "help.catalog.onedrive.desc" },
  exchange_graph: { icon: "📧", nameKey: "help.catalog.exchange_graph.name", descKey: "help.catalog.exchange_graph.desc" },
  imap: { icon: "📧", nameKey: "help.catalog.imap.name", descKey: "help.catalog.imap.desc" },
  teams: { icon: "💬", nameKey: "help.catalog.teams.name", descKey: "help.catalog.teams.desc" },
  confluence: { icon: "📖", nameKey: "help.catalog.confluence.name", descKey: "help.catalog.confluence.desc" },
  jira: { icon: "🎫", nameKey: "help.catalog.jira.name", descKey: "help.catalog.jira.desc" },
  freshservice: { icon: "🎫", nameKey: "help.catalog.freshservice.name", descKey: "help.catalog.freshservice.desc" },
  folder: { icon: "🗂️", nameKey: "help.catalog.folder.name", descKey: "help.catalog.folder.desc" },
  github: { icon: "🐙", nameKey: "help.catalog.github.name", descKey: "help.catalog.github.desc" },
  sap_s4: { icon: "🏢", nameKey: "help.catalog.sap_s4.name", descKey: "help.catalog.sap_s4.desc" },
};
const TOOL_CATALOG = {
  mssql: { icon: "🗄️", nameKey: "help.catalog.mssql.name", descKey: "help.catalog.mssql.desc" },
  shop: { icon: "🛒", nameKey: "help.catalog.shop.name", descKey: "help.catalog.shop.desc" },
  http: { icon: "🔌", nameKey: "help.catalog.http.name", descKey: "help.catalog.http.desc" },
  rest_connectors: { icon: "🔌", nameKey: "help.catalog.rest_connectors.name", descKey: "help.catalog.rest_connectors.desc" },
  sharepoint_search: { icon: "🔎", nameKey: "help.catalog.sharepoint_search.name", descKey: "help.catalog.sharepoint_search.desc" },
  fetch_url: { icon: "🌐", nameKey: "help.catalog.fetch_url.name", descKey: "help.catalog.fetch_url.desc" },
  web_research: { icon: "🔬", nameKey: "help.catalog.web_research.name", descKey: "help.catalog.web_research.desc" },
  web_search: { icon: "🔍", nameKey: "help.catalog.web_search.name", descKey: "help.catalog.web_search.desc" },
  azure_bing_search: { icon: "🔍", nameKey: "help.catalog.azure_bing_search.name", descKey: "help.catalog.azure_bing_search.desc" },
  subagents: { icon: "🧩", nameKey: "help.catalog.subagents.name", descKey: "help.catalog.subagents.desc" },
};

// helpAvailabilityRow builds one row for the Help tab's dynamic sources/
// tools lists (active or inactive) — icon + name + description, both
// i18n'd via data-i18n so a later language switch (setLocale's applyI18n)
// re-translates them without needing to rebuild the list.
function helpAvailabilityRow(entry) {
  const row = qtEl("div", { class: "help-availability-item" });
  row.appendChild(qtEl("span", { class: "help-availability-icon", text: entry.icon, "aria-hidden": "true" }));
  const body = qtEl("div", { class: "help-availability-body" });
  body.appendChild(qtEl("div", { class: "help-availability-name", "data-i18n": entry.nameKey, text: t(entry.nameKey) }));
  body.appendChild(qtEl("div", { class: "help-availability-desc", "data-i18n": entry.descKey, text: t(entry.descKey) }));
  row.appendChild(body);
  return row;
}

// renderHelpAvailability populates the Help tab's "available sources"/
// "available tools"/"not yet active" blocks from availableSources/
// availableTools (set by the /api/auth/status handler below) — called once
// that fetch resolves. Safe on every page load even before the Help tab is
// ever opened: it just fills hidden markup ahead of time, same as the rest
// of the page's up-front DOM population.
function renderHelpAvailability() {
  const sourcesHost = document.getElementById("helpAvailableSources");
  const toolsHost = document.getElementById("helpAvailableTools");
  const inactiveHost = document.getElementById("helpInactiveModules");
  const noToolsHint = document.getElementById("helpNoToolsHint");
  if (!sourcesHost || !toolsHost || !inactiveHost) return;

  sourcesHost.innerHTML = "";
  toolsHost.innerHTML = "";
  inactiveHost.innerHTML = "";

  const activeSourceSet = new Set(availableSources);
  const activeToolSet = new Set(availableTools);

  Object.entries(SOURCE_CATALOG).forEach(([kind, entry]) => {
    if (activeSourceSet.has(kind)) sourcesHost.appendChild(helpAvailabilityRow(entry));
  });
  Object.entries(TOOL_CATALOG).forEach(([kind, entry]) => {
    if (activeToolSet.has(kind)) toolsHost.appendChild(helpAvailabilityRow(entry));
  });
  if (noToolsHint) noToolsHint.hidden = toolsHost.children.length > 0;

  Object.entries(SOURCE_CATALOG).forEach(([kind, entry]) => {
    if (!activeSourceSet.has(kind)) inactiveHost.appendChild(helpAvailabilityRow(entry));
  });
  Object.entries(TOOL_CATALOG).forEach(([kind, entry]) => {
    if (!activeToolSet.has(kind)) inactiveHost.appendChild(helpAvailabilityRow(entry));
  });
}

const CLOUD_PROFILE_OPTIONS = {
  askProfileAzureOption: "azure",
  askProfileOpenAIOption: "openai",
  askProfileOpenRouterOption: "openrouter",
  askProfileClaudeOption: "claude",
  askProfileGeminiOption: "gemini",
};
function applyAskProfileVisibility() {
  const configured = new Set(configuredChatProfiles);
  Object.entries(CLOUD_PROFILE_OPTIONS).forEach(([id, value]) => {
    const opt = $("#" + id);
    if (!opt) return;
    const hide = !configured.has(value) || (value === "azure" && !isRegisteredTier());
    opt.hidden = hide;
    if (hide && $("#askProfile").value === value) $("#askProfile").value = "";
  });
}
applyAdminVisibility();

api("/api/auth/status").then(status => {
  ldapEnabled = !!status.ldap_enabled;
  localAuthEnabled = !!status.local_auth_enabled;
  authTierActive = ldapEnabled || localAuthEnabled;
  $("#adminUsername").hidden = !authTierActive;
  if (status.history_max_turns > 0) SESSION_HISTORY_MAX = status.history_max_turns;
  smtpEnabled = !!status.smtp_enabled;
  configuredChatProfiles = status.configured_chat_profiles || [];
  applyAskProfileVisibility();
  // Diagram-library sources (settings.go's renderConfig, exposed here so a
  // non-admin chat user learns them too). An empty string means the operator
  // disabled that renderer; only override the built-in CDN default when the
  // server actually sent a value, so a deployment on an older build that
  // doesn't send "render" keeps working rendering out of the box.
  if (status.render) {
    if (status.render.mermaid_url !== undefined) window.R3_RENDER.mermaidUrl = status.render.mermaid_url;
    if (status.render.d3_url !== undefined) window.R3_RENDER.d3Url = status.render.d3_url;
  }
  // /api/auth/status is ungated and fetched unconditionally on every page
  // load, unlike /api/settings (admin-only) — so this, not loadSettings()
  // below, is what actually gets a non-admin/anonymous chat user off
  // i18n.js's hardcoded "de" default and onto the server-configured
  // language. loadSettings() still calls setLocale() too when an admin
  // opens Settings; same value, harmless no-op re-apply.
  //
  // Skipped when this browser already has its own r3_lang_override
  // (applied synchronously at parse time, above) — a choice already made
  // on THIS device shouldn't be silently overwritten by whatever the
  // server resolves to. A fresh browser with no local override yet is
  // exactly the case this is for: it lets a logged-in user's previously
  // saved personal override (userprefs.go) follow them to a new device.
  let hasLocalLangOverride = false;
  try { hasLocalLangOverride = !!localStorage.getItem(LANG_OVERRIDE_KEY); } catch {}
  if (status.lang && !hasLocalLangOverride) {
    setLocale(status.lang);
    applyLangSwitcherUI(status.lang);
  }
  if (authTierActive) {
    // Server-verified session, unlike the legacy mode's localStorage flag
    // (which the server never checks) — trust it over whatever's cached.
    // isLoggedIn/isAdmin are deliberately separate here: any bound AD
    // account gets a session (isLoggedIn), only is_admin unlocks
    // .admin-only UI (see requireAdminSession in handlers.go).
    isLoggedIn = !!status.logged_in;
    isAdmin = !!status.is_admin;
    currentUserLabel = (status.display_name || status.user) ? [status.display_name || status.user, status.department].filter(Boolean).join(" · ") : "";
    currentUserClaims = status.logged_in ? status : null;
    applyAdminVisibility(false);
    // admin_bootstrap_warning (handlers.go's handleAuthStatus): neither
    // ldap.required_group_dn nor ldap.admin_users is configured, so every
    // successful AD bind becomes admin. Previously only a server-log WARN
    // (ldapauth.go) — surfaced here as a one-time toast so an admin
    // actually notices and fixes it. Guarded so a re-resolved status
    // (unlikely, but this promise only fires once per page load anyway)
    // can't re-toast.
    if (isAdmin && status.admin_bootstrap_warning && !bootstrapWarningShown) {
      bootstrapWarningShown = true;
      NovaPop.toast({
        type: "warn",
        message: "LDAP: weder required_group_dn noch admin_users gesetzt — jeder erfolgreiche Login wird aktuell automatisch Admin. Unter Einstellungen -> LDAP einschränken.",
        duration: 10000,
      });
    }
    if (isAdmin) startAdminNotificationPoll();
    // Pre-load personal context (Phase 4) so buildMailSignature can prefer
    // a saved custom signature even if this visit never opens "Mein Konto"
    // — best-effort, same reasoning as every other status-derived fetch
    // here: a failure just leaves myPersonalContext null (the existing
    // AD-derived fallback), never blocks page load.
    if (isLoggedIn) {
      api("/api/account/prefs").then(p => { myPersonalContext = p; }).catch(() => {});
    }
  }
  chatHistoryEnabled = !!status.chat_history_enabled;
  updateHistoryButtonVisibility();
  mailGraphAvailable = !!status.mail_graph_available;
  updateMailGraphPanelVisibility();
  uploadImageMode = status.upload_image_mode === "vision" ? "vision" : "ocr";
  updateAttachHints();
  availableSources = status.available_sources || [];
  availableTools = status.available_tools || [];
  renderHelpAvailability();
}).catch(() => { /* status check is best-effort; legacy mode still works without it */ });

// bootstrapWarningShown/lastAdminNotifId/adminNotifStream back the admin
// toast feed just below (notifications.go's /api/admin/notifications/stream,
// pushed to e.g. on a scheduler job finishing) — declared here since the
// /api/auth/status handler above is what starts the stream. This used to be
// a plain 8-second setInterval poll (GET /api/admin/notifications from every
// open admin tab, regardless of whether anything happened) — replaced with a
// single persistent SSE connection per tab that the server only writes to
// when a notification actually exists, cutting the near-constant "nothing
// new" request/response/JSON round trip (and the matching access-log noise)
// down to one connection that just sits there until it's needed.
let bootstrapWarningShown = false;
let lastAdminNotifId = 0;
let adminNotifStream = null;

// handleAdminNotification shows one novapop.js toast for a single
// notification (notifications.go's adminNotification) and advances
// lastAdminNotifId — shared by the initial connection's catch-up and every
// live event afterward, both delivered through the same EventSource
// "message" event.
function handleAdminNotification(n) {
  const isAlert = n.kind === "scheduler_alert" || n.kind.endsWith("_error");
  NovaPop.toast({
    type: isAlert ? "error" : "success",
    duration: isAlert ? 10000 : 4000,
    message: n.message,
    action: isAlert ? { label: "Jobs öffnen", onClick: () => activateTab($("#navtab-jobs")) } : null,
  });
  lastAdminNotifId = Math.max(lastAdminNotifId, n.id);
}

// Native alert dialogs interrupt keyboard flow and vanish without a record.
// Keep existing call sites concise while routing their messages through the
// accessible NovaPop toast/live-region system instead.
window.alert = (message) => NovaPop.toast({ type: "error", duration: 8000, message: String(message || "") });

// ---- Online/offline connectivity ---------------------------------------
// A dropped connection previously gave no feedback anywhere in the app — a
// stalled /api/ask stream just sat there with the typing indicator, and a
// lost Wi-Fi/VPN link was silent until the next action happened to fail.
// Reuses the same NovaPop toast mechanism already wired for admin
// notifications above, rather than adding a second toast system.
window.addEventListener("offline", () => {
  NovaPop.toast({ type: "error", duration: 6000, message: t("connectivity.offline") });
});
window.addEventListener("online", () => {
  NovaPop.toast({ type: "success", duration: 3000, message: t("connectivity.online") });
});

// startAdminNotificationPoll opens the SSE connection once /api/auth/status
// confirms isAdmin — idempotent (guards against being called more than
// once, e.g. if login happens after an initial non-admin status
// resolution). Kept its original poll-era name since every call site still
// just wants "start receiving admin notifications" — renaming it would only
// churn call sites for no behavioral reason.
function startAdminNotificationPoll() {
  if (adminNotifStream) return;
  adminNotifStream = new EventSource(`/api/admin/notifications/stream?since=${lastAdminNotifId}`);
  adminNotifStream.onmessage = (e) => {
    try { handleAdminNotification(JSON.parse(e.data)); } catch { /* malformed event, ignore */ }
  };
  // Best-effort by design (matching the old poller's silent-retry
  // behavior): EventSource retries the connection automatically on a
  // transient drop, with its own backoff — nothing to do here. If the
  // session itself is gone (logout, expiry), stopAdminNotificationPoll
  // (called from doLogout) closes the stream outright instead of letting
  // it keep retrying against a 401 forever.
  adminNotifStream.onerror = () => {};
}

// stopAdminNotificationPoll closes the SSE connection — called on logout so
// an expired/cleared session doesn't leave EventSource retrying forever
// against a 401, and lets a subsequent login re-open a fresh stream (guarded
// by startAdminNotificationPoll's adminNotifStream null-check above).
function stopAdminNotificationPoll() {
  if (adminNotifStream) { adminNotifStream.close(); adminNotifStream = null; }
}

// doLogout is the one place both the sidebar toggle and the "Mein Konto"
// modal's logout button call — kept in one place so the two entry points
// can't drift (e.g. one clearing currentUserClaims and the other not).
function doLogout() {
  isAdmin = false;
  isLoggedIn = false;
  currentUserLabel = "";
  currentUserClaims = null;
  localStorage.removeItem("r3_admin");
  if (authTierActive) api("/api/auth/logout", { method: "POST" }).catch(() => {});
  applyAdminVisibility();
  updateHistoryButtonVisibility();
  stopAdminNotificationPoll();
  stopOperationsPolling();
}

$("#adminToggle").addEventListener("click", () => {
  const signedIn = authTierActive ? isLoggedIn : isAdmin;
  if (signedIn) {
    doLogout();
    return;
  }
  const box = $("#adminLoginBox");
  box.hidden = !box.hidden;
  $("#adminToggle").setAttribute("aria-expanded", String(!box.hidden));
  if (!box.hidden) $(authTierActive ? "#adminUsername" : "#adminPassword").focus();
});

$("#adminLoginBox").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#adminLoginResult");
  out.className = "result";
  try {
    if (authTierActive) {
      // api() throws on a non-2xx response (see its doc comment) — a
      // failed login is now a real HTTP 401, not a 200 body with
      // {"ok":false}, so a thrown error here is the failure path,
      // handled by the catch block below. On success the body carries
      // is_admin/department alongside user (see handleLDAPLogin).
      const res = await api("/api/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: $("#adminUsername").value, password: $("#adminPassword").value }),
      });
      isLoggedIn = true;
      isAdmin = !!res.is_admin;
      currentUserLabel = [res.display_name || res.user, res.department].filter(Boolean).join(" · ");
      currentUserClaims = res;
    } else {
      const res = await api("/api/admin/check", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password: $("#adminPassword").value }),
      });
      if (!res.ok) { out.className = "result error"; out.textContent = t("login.wrongPassword"); return; }
      isAdmin = true;
      localStorage.setItem("r3_admin", "1");
    }
    $("#adminPassword").value = "";
    $("#adminUsername").value = "";
    out.textContent = "";
    applyAdminVisibility();
    updateHistoryButtonVisibility();
    if (isAdmin) startAdminNotificationPoll();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  }
});

// ---- Tabs -----------------------------------------------------------
// TAB_LOADERS maps a tab's data-tab value to the loader(s) that (re)fetch
// its panel content — run once per activateTab() call rather than an
// if-chain, so adding a tab later means one new entry here, not another
// branch to keep in sync.
const TAB_LOADERS = {
  sources: () => loadSources(),
  // Keep unsaved form edits intact when an admin briefly switches tabs.
  // Settings can still be explicitly reloaded from its own toolbar.
  settings: () => { if (!settingsDirty) loadSettings(); loadAPIKeys(); loadAgentAudit(); loadOperationsStatus(); loadStorageStatus(); },
  chunks: () => loadChunks(0),
  prompts: () => loadPrompts(),
  jobs: () => refreshSchedulerPanel(),
  import: () => loadImportConnectionSelectors(),
  users: () => loadLocalUsers(),
};

// navigateToChunksForSource jumps to the Chunks tab pre-filtered to one
// source (the "Quell-ID enthält" filter does a substring match, chunks.go —
// passing the full source_id effectively narrows to just this source) —
// shared deep-link so "this source keeps getting downvoted" (Jobs tab) or
// "inspect this source's actual chunks" (Sources tab) leads straight there
// instead of requiring a manual copy-paste into the Chunks filter form.
function navigateToChunksForSource(sourceId) {
  $("#cf_source_id").value = sourceId;
  activateTab($("#navtab-chunks"));
}

function activateTab(btn) {
  $all(".nav-item").forEach(b => { b.classList.remove("active"); b.setAttribute("aria-selected", "false"); });
  $all(".panel").forEach(p => p.classList.remove("active"));
  btn.classList.add("active");
  btn.setAttribute("aria-selected", "true");
  $("#tab-" + btn.dataset.tab).classList.add("active");
  TAB_LOADERS[btn.dataset.tab]?.();
  if (btn.dataset.tab === "settings") startOperationsPolling();
  else stopOperationsPolling();
}
// ".nav-action" items (currently just #navtab-history) have no matching
// #tab-<value> panel by design — they trigger something else (a modal)
// instead of switching panels, so they're excluded from this generic
// tab-switching loop and wired individually further down.
$all(".nav-item:not(.nav-action)").forEach(btn => btn.addEventListener("click", () => {
  // Re-clicking the already-active tab would otherwise re-run its loader
  // (e.g. loadChunks(0) silently resetting pagination the user scrolled
  // past) for no benefit — each panel already has its own explicit
  // refresh control (#refreshSources, the chunks filter form, …).
  if (btn.classList.contains("active")) return;
  activateTab(btn);
}));

// ---- Chat message construction --------------------------------------
const ASSISTANT_AVATAR =
  `<svg viewBox="0 0 50 50" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
    <polygon points="13,1 37,1 49,13 49,37 37,49 13,49 1,37 1,13"
      fill="none" stroke="currentColor" stroke-width="3.4" stroke-linejoin="bevel"/>
    <line x1="14" y1="10" x2="43" y2="41" stroke="currentColor" stroke-width="7" stroke-linecap="round"/>
    <line x1="43" y1="10" x2="14" y2="41" stroke="currentColor" stroke-width="7" stroke-linecap="round"/>
  </svg>`;

// ACTION_ICONS: the per-message action row's icon set — inline stroke SVGs
// in the same 20x20/1.7 style as the sidebar's nav icons, replacing the
// old emoji glyphs (📋💾🔄👍👎✉️) that rendered inconsistently across
// platforms and clashed with the rest of the app's line-icon language.
const ACTION_ICONS = {
  copy:   `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="7" y="7" width="9" height="9" rx="1.5"/><path d="M13 7V5a1.5 1.5 0 00-1.5-1.5h-6A1.5 1.5 0 004 5v6a1.5 1.5 0 001.5 1.5H7"/></svg>`,
  check:  `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 10.5l4 4 8-9"/></svg>`,
  alert:  `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10 3L2.5 16.5h15z"/><path d="M10 8.5v3.5M10 14.5h.01"/></svg>`,
  download: `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10 3v9M7 9.5l3 3 3-3M3 14v2a1 1 0 001 1h12a1 1 0 001-1v-2"/></svg>`,
  refresh: `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M16 10a6 6 0 11-1.8-4.3M16 3v3.2h-3.2"/></svg>`,
  thumbUp: `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 9v8H3.5V9zM6 9.5l3.2-6a1.8 1.8 0 011.7 2.2L10.3 8h4.9a1.5 1.5 0 011.5 1.8l-1.2 5.5A2 2 0 0113.5 17H6"/></svg>`,
  thumbDown: `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M14 11V3h2.5v8zM14 10.5l-3.2 6a1.8 1.8 0 01-1.7-2.2L9.7 12H4.8a1.5 1.5 0 01-1.5-1.8l1.2-5.5A2 2 0 016.5 3H14"/></svg>`,
  mail:   `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="5" width="14" height="10" rx="1.5"/><path d="M3 6l7 5 7-5"/></svg>`,
  speak:  `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 8v4h3l4 3V5L6 8H3z"/><path d="M13.3 7.2a3 3 0 010 5.6M15.5 5.2a6 6 0 010 9.6"/></svg>`,
  speakStop: `<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="6" y="6" width="8" height="8" rx="1.2"/></svg>`,
};

// setActionIconState swaps a .copy-btn's icon to a transient ok/error state
// (check/alert + color class) and back after 1.5s — the SVG equivalent of
// the old "📋 → ✅ → 📋" textContent dance.
function setActionIconState(btn, state, restoreIcon) {
  btn.innerHTML = ACTION_ICONS[state === "ok" ? "check" : "alert"];
  btn.classList.add(state === "ok" ? "action-ok" : "action-err");
  setTimeout(() => {
    btn.innerHTML = restoreIcon;
    btn.classList.remove("action-ok", "action-err");
  }, 1500);
}

// hideChatEmpty hides the "empty state" placeholder once the chat has messages.
function hideChatEmpty() {
  const empty = $("#chatEmpty");
  if (empty) empty.hidden = true;
}

// ---- Chat auto-scroll ------------------------------------------------
// Streaming used to force-scroll on every token, which made it impossible
// to scroll up and read an earlier answer while a long one was still
// coming in. Instead: only follow the stream while the user is "pinned"
// near the bottom; once they scroll up, a floating "Nach unten" pill
// (#scrollBottomBtn, tab-chat.html) appears and re-pins on click — the
// ChatGPT/Claude convention.
const CHAT_PIN_THRESHOLD = 120; // px from bottom still counting as "at the bottom"

function chatLogPinned() {
  const log = $("#chatLog");
  return log.scrollHeight - log.scrollTop - log.clientHeight < CHAT_PIN_THRESHOLD;
}

// scrollChatToBottom(force): force=true always jumps (new message sent,
// pill clicked); force=false only follows if the user is already pinned.
// Always instant, overriding .chat-log's scroll-behavior:smooth — a smooth
// animation would leave scrollTop mid-flight when the next token's
// chatLogPinned() check runs, spuriously "unpinning" an actually-following
// reader.
function scrollChatToBottom(force) {
  const log = $("#chatLog");
  if (force || chatLogPinned()) log.scrollTo({ top: log.scrollHeight, behavior: "instant" });
  updateScrollBottomBtn();
}

function updateScrollBottomBtn() {
  const btn = $("#scrollBottomBtn");
  if (btn) btn.hidden = chatLogPinned();
}
$("#chatLog").addEventListener("scroll", updateScrollBottomBtn, { passive: true });
$("#scrollBottomBtn")?.addEventListener("click", () => scrollChatToBottom(true));

// addMessage creates the message <div>. For assistant messages, citations
// are rendered inline ([Q1] markers) and shown as chips below. During
// streaming, citations is [] and a re-render is done when "done" arrives.
function addMessage(role, content, citations, images) {
  hideChatEmpty();
  const log = $("#chatLog");
  const div = document.createElement("div");
  div.className = "msg " + role;
  const roleLabel = role === "user" ? "Du" : "R3";
  const avatar = role === "user" ? "DU" : ASSISTANT_AVATAR;
  div.innerHTML =
    `<div class="avatar" aria-hidden="true">${avatar}</div>` +
    `<div class="msg-body">` +
    `<span class="sr-only">${roleLabel}</span>` +
    `<div class="content"></div>` +
    `<div class="msg-actions"></div>` +
    `</div>`;
  const contentEl = div.querySelector(".content");
  if (role === "assistant") {
    const citArr = citations || [];
    renderInto(contentEl, content, citArr, true);
    addCopyButton(div, () => contentEl.dataset.raw ?? "");
    addDownloadButton(div, () => contentEl.dataset.raw ?? "");
    addSpeakButton(div, () => contentEl.dataset.raw ?? "");
    addRegenerateButton(div);
    addFeedbackButtons(div);
    if (isLoggedIn && smtpEnabled) addSendEmailButton(div);
    contentEl.dataset.raw = content || "";
  } else {
    contentEl.textContent = content || "";
    addMsgImages(contentEl, images);
  }
  if (citations && citations.length) addCitations(div, citations);
  log.appendChild(div);
  scrollChatToBottom(true);
  return div;
}

// addCopyButton adds a "Kopieren" button to a message's action bar that
// copies getText()'s current output (called lazily, so streaming
// responses copy their latest content rather than a stale snapshot).
function addCopyButton(div, getText) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "copy-btn";
  btn.innerHTML = ACTION_ICONS.copy;
  btn.title = t("chat.copyButton.title");
  btn.setAttribute("aria-label", t("chat.copyButton.title"));
  btn.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(getText());
      setActionIconState(btn, "ok", ACTION_ICONS.copy);
    } catch { setActionIconState(btn, "err", ACTION_ICONS.copy); }
  });
  div.querySelector(".msg-actions").appendChild(btn);
}

// addDownloadButton adds a "Speichern" button that saves getText()'s
// current output as a timestamped .md file via a throwaway blob URL.
function addDownloadButton(div, getText) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "copy-btn dl-btn";
  btn.innerHTML = ACTION_ICONS.download;
  btn.title = t("chat.downloadButton.title");
  btn.setAttribute("aria-label", t("chat.downloadButton.ariaLabel"));
  btn.addEventListener("click", () => {
    const md = getText();
    const blob = new Blob([md], { type: "text/markdown; charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `r3-antwort-${new Date().toISOString().slice(0, 16).replace("T", "-")}.md`;
    a.click();
    URL.revokeObjectURL(url);
  });
  div.querySelector(".msg-actions").appendChild(btn);
}

// addRegenerateButton adds the "Neu generieren" button that triggers
// regenerateFrom() (see its comment below) for this assistant message.
function addRegenerateButton(div) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "copy-btn";
  btn.innerHTML = ACTION_ICONS.refresh;
  btn.title = t("chat.regenerate.title");
  btn.setAttribute("aria-label", t("chat.regenerate"));
  btn.addEventListener("click", () => regenerateFrom(div));
  div.querySelector(".msg-actions").appendChild(btn);
}

// addFeedbackButtons adds a thumbs up/down pair to a message's action bar
// (docs/TODO.md C5) — POSTs to /api/feedback with the question/answer/
// citations already rendered, same "read current state at click time, not
// a stale snapshot" reasoning as addCopyButton. Only one vote is allowed
// per answer: both buttons disable themselves once either is clicked,
// simplest UX for a first cut with no "change your vote" flow yet.
function addFeedbackButtons(div) {
  const wrap = document.createElement("span");
  wrap.className = "feedback-buttons";
  const up = document.createElement("button");
  up.type = "button";
  up.className = "copy-btn feedback-btn";
  up.innerHTML = ACTION_ICONS.thumbUp;
  up.title = t("chat.feedback.good");
  up.setAttribute("aria-label", t("chat.feedback.good"));
  const down = document.createElement("button");
  down.type = "button";
  down.className = "copy-btn feedback-btn";
  down.innerHTML = ACTION_ICONS.thumbDown;
  down.title = t("chat.feedback.bad");
  down.setAttribute("aria-label", t("chat.feedback.bad"));

  const vote = async (rating, btn) => {
    const contentEl = div.querySelector(".content");
    const userDiv = div.previousElementSibling;
    const question = (userDiv && userDiv.classList.contains("user")) ? (userDiv.querySelector(".content")?.textContent || "") : "";
    const citations = Array.from(div.querySelectorAll(".citations .citation")).map(el => el.textContent.trim());
    up.disabled = true;
    down.disabled = true;
    try {
      await api("/api/feedback", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ question, answer: contentEl.dataset.raw || "", citations, rating }),
      });
      btn.classList.add("feedback-btn-active");
    } catch {
      up.disabled = false;
      down.disabled = false;
    }
  };
  up.addEventListener("click", () => vote("up", up));
  down.addEventListener("click", () => vote("down", down));
  wrap.appendChild(up);
  wrap.appendChild(down);
  div.querySelector(".msg-actions").appendChild(wrap);
}

// addSendEmailButton adds an "An mich senden" button to a message's action
// bar that emails the current answer to the logged-in user's own AD
// address — resolved server-side from the session (handlers.go's
// handleChatEmail), never sent from here, so this can't be used to relay
// mail to an arbitrary address. Only added when isLoggedIn AND smtpEnabled
// (see addMessage above) — the latter avoids showing a button that would
// always fail with "email is not configured" when SMTP isn't set up.
// Reads the answer/question/citations lazily at click time, same "current
// state, not a stale snapshot" reasoning as addCopyButton.
function addSendEmailButton(div) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "copy-btn";
  btn.innerHTML = ACTION_ICONS.mail;
  btn.title = t("chat.emailButton.title");
  btn.setAttribute("aria-label", t("chat.emailButton.ariaLabel"));
  btn.addEventListener("click", async () => {
    const contentEl = div.querySelector(".content");
    const userDiv = div.previousElementSibling;
    const question = (userDiv && userDiv.classList.contains("user")) ? (userDiv.querySelector(".content")?.textContent || "") : "";
    const citations = Array.from(div.querySelectorAll(".citations .citation")).map(el => el.textContent.trim());
    btn.disabled = true;
    try {
      await api("/api/chat/email", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ question, answer: contentEl.dataset.raw || "", citations }),
      });
      setActionIconState(btn, "ok", ACTION_ICONS.mail);
    } catch {
      setActionIconState(btn, "err", ACTION_ICONS.mail);
    } finally {
      btn.disabled = false;
    }
  });
  div.querySelector(".msg-actions").appendChild(btn);
}

// ---- Speech output (local text-to-speech via the Web Speech API) --------
// Local voice INPUT (wireVoiceInput) already transcribes speech with a
// local Whisper CLI, entirely server-side; speech OUTPUT is the browser's
// own built-in SpeechSynthesis instead — no server round trip, no external
// service, nothing configured or installed, consistent with the app's
// local-first posture on the input side. Support varies by browser/OS
// (voice list, language coverage) — this degrades to a disabled button with
// an explanatory title rather than a broken one when unsupported.

// stripMarkdownForSpeech turns a raw markdown answer into plain, readable
// text for the speech engine: heading/emphasis/inline-code markers,
// fenced-code delimiters, "[Qn]" citation markers (meaningless read aloud —
// the source popup is the visual equivalent) and markdown links'
// "(url)" halves are all dropped, keeping just the words a listener needs.
// Deliberately regex-based, not a full markdown parse — good enough for
// speech, where a stray leftover symbol is heard as a brief pause at worst.
function stripMarkdownForSpeech(md) {
  return (md || "")
    .replace(/\[Q\d+\]/g, "")
    .replace(/```[a-zA-Z0-9]*\n?/g, "")
    .replace(/`([^`]+)`/g, "$1")
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
    .replace(/^#{1,6}\s+/gm, "")
    .replace(/(\*\*|__)(.*?)\1/g, "$2")
    .replace(/(\*|_)(.*?)\1/g, "$2")
    .replace(/^\s*[-*+]\s+/gm, "")
    .replace(/^\s*>\s?/gm, "")
    .replace(/\n{2,}/g, ". ")
    .replace(/\s+/g, " ")
    .trim();
}

// TTS_LANG_BY_LOCALE maps this app's own 4 UI locales to a concrete BCP-47
// tag SpeechSynthesisUtterance.lang understands — currentLocale itself
// (see i18n.js) is only ever one of these 4 short codes.
const TTS_LANG_BY_LOCALE = { de: "de-DE", en: "en-US", fr: "fr-FR", it: "it-IT" };

// currentSpeech tracks the single message currently being read aloud (the
// Web Speech API has no built-in "what's playing" query), so starting a
// second message's speech first cancels the first and resets ITS button —
// only one message reads at a time, matching how a single audio/video
// element would behave.
let currentSpeech = null;

function resetSpeakButton(btn) {
  btn.innerHTML = ACTION_ICONS.speak;
  btn.classList.remove("speaking");
  btn.title = t("chat.speakButton.title");
  btn.setAttribute("aria-label", t("chat.speakButton.title"));
}

// addSpeakButton adds a "vorlesen" button that reads getText()'s current
// output aloud via the browser's local SpeechSynthesis — never sent
// anywhere, same "read live state at click time" pattern as addCopyButton.
// Clicking the same button again (or the browser finishing on its own)
// stops playback; clicking a DIFFERENT message's button switches to it.
function addSpeakButton(div, getText) {
  if (!window.speechSynthesis || !window.SpeechSynthesisUtterance) return; // unsupported browser: no broken button
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "copy-btn speak-btn";
  resetSpeakButton(btn);
  btn.addEventListener("click", () => {
    if (currentSpeech && currentSpeech.btn === btn) {
      window.speechSynthesis.cancel(); // onend/onerror below resets the button
      return;
    }
    window.speechSynthesis.cancel(); // stop any other message currently reading
    const text = stripMarkdownForSpeech(getText());
    if (!text) return;
    const utter = new SpeechSynthesisUtterance(text);
    utter.lang = TTS_LANG_BY_LOCALE[currentLocale] || "de-DE";
    const finish = () => {
      resetSpeakButton(btn);
      if (currentSpeech && currentSpeech.btn === btn) currentSpeech = null;
    };
    utter.addEventListener("end", finish);
    utter.addEventListener("error", finish);
    currentSpeech = { btn };
    btn.innerHTML = ACTION_ICONS.speakStop;
    btn.classList.add("speaking");
    btn.title = t("chat.speakButton.stop");
    btn.setAttribute("aria-label", t("chat.speakButton.stop"));
    window.speechSynthesis.speak(utter);
  });
  div.querySelector(".msg-actions").appendChild(btn);
}

// regenerateFrom re-asks the question that produced assistantDiv's answer.
// Not just a single-message swap: everything from that question onward
// is dropped first (DOM + sessionMessages + the persisted conversation,
// if any), since later turns in the conversation were grounded in the
// answer being replaced and would otherwise reference a question/answer
// pair that no longer matches what's shown.
function regenerateFrom(assistantDiv) {
  if (streamAbort) return; // avoid overlapping with an already-running stream
  const userDiv = assistantDiv.previousElementSibling;
  if (!userDiv || !userDiv.classList.contains("user")) return;
  const question = userDiv.querySelector(".content")?.textContent || "";
  if (!question) return;

  let node = userDiv;
  const toRemove = [];
  while (node) { toRemove.push(node); node = node.nextElementSibling; }
  toRemove.forEach(el => el.remove());

  const cut = sessionMessages.findIndex(m => m.role === "user" && m.content === question);
  sessionMessages = cut >= 0 ? sessionMessages.slice(0, cut) : [];
  if (currentConversation) {
    const cutConv = currentConversation.messages.findIndex(m => m.role === "user" && m.content === question);
    currentConversation.messages = cutConv >= 0 ? currentConversation.messages.slice(0, cutConv) : [];
    persistCurrentConversation();
  }

  askQuestion(question);
}

// buildCitationChip renders one citation as either a real link (when the
// source has a resolvable source_url, e.g. SharePoint/web) or a button that
// opens the in-app source popup (openSourcePopup) — shared by every place a
// citation is shown (chat/agent messages via addCitations, and the Mail
// tab's draft citations) so a citation is always clickable everywhere,
// never a dead label.
function buildCitationChip(c) {
  const when = c.loaded_at ? new Date(c.loaded_at * 1000).toLocaleString() : "";
  const titleText = `Quelle: ${c.source_id}\nLoad: ${c.load_id}\nGeladen: ${when}`;
  if (c.source_url) {
    const a = document.createElement("a");
    a.className = "citation";
    a.href = c.source_url;
    a.target = "_blank";
    a.rel = "noopener";
    a.title = titleText;
    a.textContent = `📎 ${c.source_name}`;
    return a;
  }
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "citation";
  btn.title = titleText;
  btn.textContent = `📎 ${c.source_name}`;
  btn.addEventListener("click", () => openSourcePopup(c.source_id, c.source_name, c.source_kind));
  return btn;
}

// addCitations (re)renders the citation chip bar below an assistant
// message — safe to call repeatedly (e.g. once per stream "done" event)
// since it removes any bar it previously added before adding the new one.
function addCitations(div, citations) {
  // Remove any existing citation bar before re-adding (called on streaming "done")
  const existing = div.querySelector(".citations");
  if (existing) existing.remove();

  const cdiv = document.createElement("div");
  cdiv.className = "citations";
  citations.forEach(c => cdiv.appendChild(buildCitationChip(c)));
  div.querySelector(".msg-body").appendChild(cdiv);
}

// ---- Debug-Modus (Chat/Agent/Mail) -------------------------------------
// The server only ever includes a "debug" field in the /api/ask ("done"/
// "clarify" NDJSON lines, or the format:"json" response) or /api/draft/
// reply response for an admin session (handlers.go's debugModeAllowed) —
// everyone else's response simply has no such field, so this renders
// nothing for them. No client-side name/role check needed or possible:
// the gate is entirely server-side, this only ever displays what the
// server chose to send.
function renderDebugPanel(container, debug) {
  // Remove any previously rendered panel first (mirrors addCitations'
  // existing-removal pattern) — container may hold other content
  // (message text, citations) that must not be touched.
  const existing = container.querySelector(".debug-panel");
  if (existing) existing.remove();
  if (!debug) return;

  const details = document.createElement("details");
  details.className = "debug-panel";
  const summary = document.createElement("summary");
  summary.textContent = t("chat.debugPanel.summary");
  details.appendChild(summary);

  function section(title, bodyEl) {
    const h = document.createElement("h4");
    h.textContent = title;
    details.appendChild(h);
    details.appendChild(bodyEl);
  }
  function pre(text) {
    const p = document.createElement("pre");
    p.className = "debug-pre";
    p.textContent = text;
    return p;
  }

  const metaParts = [];
  if (debug.profile) metaParts.push(`Profil: ${debug.profile}`);
  if (debug.preset) metaParts.push(`Preset: ${debug.preset}`);
  if (debug.preset_kinds && debug.preset_kinds.length) metaParts.push(`Quell-Arten: ${debug.preset_kinds.join(", ")}`);
  if (debug.preset_tools && debug.preset_tools.length) metaParts.push(`Werkzeuge: ${debug.preset_tools.join(", ")}`);
  if (debug.dept_code) metaParts.push(`Abteilungscode: ${debug.dept_code === "*" ? "* (Admin, ungefiltert)" : debug.dept_code}`);
  metaParts.push(debug.selected_skills && debug.selected_skills.length
    ? `Skills: ${debug.selected_skills.join(", ")}`
    : "Skills: keiner passte (nur index.md/agent.md)");
  if (debug.total_ms != null) metaParts.push(`Gesamtdauer: ${debug.total_ms} ms`);
  if (metaParts.length) {
    const meta = document.createElement("p");
    meta.className = "hint debug-meta";
    meta.textContent = metaParts.join(" · ");
    details.appendChild(meta);
  }

  const chunks = debug.retrieved_chunks || [];
  if (chunks.length) {
    const list = document.createElement("ol");
    list.className = "debug-chunk-list";
    chunks.forEach(c => {
      const li = document.createElement("li");
      li.innerHTML = `<strong>${escapeHTML(c.source_name || c.source_id)}</strong> ` +
        `<span class="hint">(${escapeHTML(c.source_kind)}, final=${(c.final_score ?? 0).toFixed(3)} ` +
        `vector=${(c.vector_score ?? 0).toFixed(3)} keyword=${(c.keyword_score ?? 0).toFixed(3)} ` +
        `recency=${(c.recency_score ?? 0).toFixed(3)})</span>`;
      li.appendChild(pre(c.content || ""));
      list.appendChild(li);
    });
    section(`Aus dem RAG geladen (${chunks.length} Treffer)`, list);
  }

  const toolCalls = debug.tool_calls || [];
  if (toolCalls.length) {
    // Group by sub-agent attribution (tc.agent, "" = top-level orchestrator,
    // llm.go's debugToolCall.Agent) so a run involving delegate_subtasks/
    // web_research reads as "which agent made which call", instead of one
    // flat, unattributed, interleaved list — previously the one place
    // sub-agent attribution existed everywhere else (live step timeline,
    // audit log) but not here.
    const groups = new Map(); // agent label ("" = top level) -> tc[]
    toolCalls.forEach(tc => {
      const key = tc.agent || "";
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(tc);
    });
    const wrap = document.createElement("div");
    wrap.className = "debug-tool-groups";
    groups.forEach((calls, agentLabel) => {
      if (agentLabel) {
        const h = document.createElement("h5");
        h.className = "debug-tool-group-agent";
        h.textContent = `🤝 Unter-Agent: ${agentLabel}`;
        wrap.appendChild(h);
      }
      const list = document.createElement("ol");
      list.className = "debug-tool-list";
      calls.forEach(tc => {
        const li = document.createElement("li");
        const status = tc.error ? `Fehler nach ${tc.duration_ms} ms` : `${tc.duration_ms} ms`;
        li.innerHTML = `<strong>${escapeHTML(tc.name)}</strong> <span class="hint">(Runde ${tc.round}, ${escapeHTML(status)})</span>`;
        li.appendChild(pre("Argumente: " + (tc.arguments || "{}")));
        if (tc.error) {
          li.appendChild(pre("Fehler: " + tc.error));
        } else if (tc.result) {
          li.appendChild(pre("Ergebnis: " + tc.result));
        }
        list.appendChild(li);
      });
      wrap.appendChild(list);
    });
    section(`Genutzte Werkzeuge (${toolCalls.length})`, wrap);
  }

  if (debug.messages && debug.messages.length) {
    section("An die KI geschickt (vollständiger Verlauf)", pre(JSON.stringify(debug.messages, null, 2)));
  }
  if (debug.raw_answer) {
    section("Rohe Modell-Antwort", pre(debug.raw_answer));
  }

  container.appendChild(details);
}

// ---- Source content popup --------------------------------------------
// Backs both the chat citation chips above and the Sources-tab rows
// (loadSources below): given a source_id, shows its full extracted text
// (every chunk stitched together server-side, see /api/sources/content)
// plus a download link when the upload that created it kept the original
// file (see the "Original behalten" checkbox on the upload form), and —
// for PST emails, when enabled under Einstellungen → Antwortentwürfe — a
// button to generate a draft reply (see composeDraftReply, draft.go).
let currentPopupSourceId = null;
let sourceModalReturnFocus = null;

// Focusable elements inside the modal, for the Tab-cycle trap below —
// recomputed on open since which of these are hidden (download link,
// draft button/section) changes per source.
function modalFocusables() {
  return $all("#sourceModal button, #sourceModal a[href]").filter(el => !el.hidden && el.offsetParent !== null);
}

// openSourcePopup opens the modal described above for a given source,
// fetching and rendering its content and wiring up the download/draft
// controls for that specific source/kind.
function openSourcePopup(sourceId, sourceName, sourceKind) {
  const modal = $("#sourceModal");
  const title = $("#sourceModalTitle");
  const meta = $("#sourceModalMeta");
  const body = $("#sourceModalBody");
  const dl = $("#sourceModalDownload");
  const draftBtn = $("#sourceModalDraftBtn");
  const draftCopy = $("#sourceModalDraftCopy");
  const draftSection = $("#sourceModalDraft");

  currentPopupSourceId = sourceId;
  sourceModalReturnFocus = document.activeElement;
  title.textContent = sourceName || sourceId;
  // Long subjects (esp. PST emails with an "Anhang: <filename>" suffix
  // appended, see pst.go) can run to a full paragraph — the CSS clamps
  // display to 2 lines, so the native title tooltip is the only way to
  // read the rest without opening the source itself.
  title.title = sourceName || sourceId;
  meta.textContent = sourceId;
  body.textContent = t("modal.loading");
  body.className = "modal-content";
  dl.hidden = true;
  draftBtn.hidden = true;
  draftCopy.hidden = true;
  draftSection.hidden = true;
  $("#sourceModalDraftBody").textContent = "";
  $("#sourceModalDraftCitations").innerHTML = "";
  modal.hidden = false;
  // Move focus into the dialog (its own close button, always present and
  // visible) rather than leaving it on the trigger behind the overlay —
  // otherwise a keyboard/screen-reader user's next Tab press would land
  // back in the page content behind the modal.
  $("#sourceModalClose").focus();

  api(`/api/sources/content?source_id=${encodeURIComponent(sourceId)}`)
    .then(res => {
      body.textContent = res.content && res.content.trim() ? res.content : t("modal.noTextExtracted");
      // Both actions are "Registriert" tier (docs/UI_HARDENING_PLAN.md) —
      // the server enforces this independently via requireSessionIfLDAP
      // (handlers.go), hiding here just avoids a guest hitting a 401.
      if (res.has_original && isRegisteredTier()) {
        dl.href = `/api/sources/original?source_id=${encodeURIComponent(sourceId)}`;
        dl.hidden = false;
      }
      if (res.draft_replies_enabled && sourceKind === "pst_email" && isRegisteredTier()) {
        draftBtn.hidden = false;
      }
    })
    .catch(err => {
      body.textContent = t("common.errorPrefix", { message: err.message });
      body.classList.add("error-text");
    });
}

$("#sourceModalDraftBtn").addEventListener("click", async () => {
  const sourceId = currentPopupSourceId;
  const btn = $("#sourceModalDraftBtn");
  const section = $("#sourceModalDraft");
  const draftBody = $("#sourceModalDraftBody");
  const draftCopy = $("#sourceModalDraftCopy");

  section.hidden = false;
  draftBody.className = "modal-content";
  draftBody.textContent = t("modal.draftGenerating");
  draftCopy.hidden = true;
  // Live step timeline while the draft is generated (search_knowledge_base,
  // MSSQL/Shop/HTTP tools the model or pre-flight router decided to use) —
  // same panel Chat/Agent use, shown right above the draft text. Remove any
  // leftover panel from a previous click first — each click creates a new
  // one, so a stale prior panel would otherwise accumulate in the DOM.
  section.querySelectorAll(".agent-steps").forEach((el) => el.remove());
  const steps = agentStepsPanel();
  draftBody.parentNode.insertBefore(steps.el, draftBody);
  setBusy(btn, true);
  try {
    const res = await requestDraftStream({ source_id: sourceId }, (step) => steps.add(step));
    steps.finish();
    draftBody.textContent = res.reply_text || t("modal.noDraftReceived");
    const citCont = $("#sourceModalDraftCitations");
    citCont.innerHTML = "";
    (res.citations || []).forEach(c => {
      const span = document.createElement("span");
      span.className = "citation";
      span.title = t("modal.sourceTitle", { sourceId: c.source_id });
      span.textContent = `📎 ${c.source_name}`;
      citCont.appendChild(span);
    });
    draftCopy.hidden = false;
    draftCopy.onclick = async () => {
      try {
        await navigator.clipboard.writeText(draftBody.textContent);
        draftCopy.textContent = t("modal.copied");
        setTimeout(() => { draftCopy.textContent = t("modal.copyDraft"); }, 1500);
      } catch { draftCopy.textContent = t("common.error"); }
    };
  } catch (err) {
    steps.finish();
    draftBody.className = "modal-content error-text";
    draftBody.textContent = t("common.errorPrefix", { message: err.message });
  } finally {
    setBusy(btn, false);
  }
});

// closeSourcePopup hides the source modal and restores keyboard focus.
function closeSourcePopup() {
  $("#sourceModal").hidden = true;
  // Restore focus to whatever opened the modal (a citation chip, a
  // sources-table button, ...) so keyboard navigation continues from
  // where the user left it, instead of resetting to the top of the page.
  if (sourceModalReturnFocus && document.body.contains(sourceModalReturnFocus)) {
    sourceModalReturnFocus.focus();
  }
  sourceModalReturnFocus = null;
}

$("#sourceModalClose").addEventListener("click", closeSourcePopup);
$("#sourceModal").addEventListener("click", (e) => {
  if (e.target.id === "sourceModal") closeSourcePopup();
});
document.addEventListener("keydown", (e) => {
  if ($("#sourceModal").hidden) return;
  if (e.key === "Escape") { closeSourcePopup(); return; }
  // Focus trap: Tab/Shift+Tab cycles within the dialog instead of
  // escaping into the page behind the overlay.
  if (e.key === "Tab") {
    const focusables = modalFocusables();
    if (!focusables.length) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }
});

// ---- Mail tab: reply drafts + new-mail composition ----------------------
// Two modes against the same /api/draft/reply endpoint: "reply" sends the
// pasted incoming mail as raw_email (the original Mail-tab behavior),
// "compose" sends the freeform description as brief (composeNewMail,
// draft.go). The result lands in editable Betreff/Body fields — the human
// reviews/edits there, then copies it, downloads it as .eml, or (admins,
// IMAP configured) files it into the mailbox's Drafts folder. R3 itself
// never sends anything.
$("#mailMode").addEventListener("change", () => {
  const compose = $("#mailMode").value === "compose";
  $("#mailInput").placeholder = compose
    ? t("mail.inputPlaceholderCompose")
    : t("mail.inputPlaceholder");
  // A composed-from-scratch mail has no original message to reply to —
  // drop any native-mailbox message association from a previous "Antwort"
  // pass so "In mein Outlook-Postfach speichern" doesn't offer to file a
  // reply against a message that's no longer what's being drafted.
  if (compose && currentMailGraphMessageId) {
    currentMailGraphMessageId = null;
    updateMailDraftSaveGraphVisibility();
  }
});

// ---- Mail tab: native mailbox panel (mail_graph.go) ----------------------
// Only ever shown for a logged-in user explicitly authorized on an
// InteractiveEnabled Exchange connection (mailGraphAvailable, set from
// /api/auth/status — see the api("/api/auth/status") handler below).
// Selecting a message fills #mailInput with a ready-to-use raw_email
// rendering and remembers its Graph id (currentMailGraphMessageId), so the
// EXISTING /api/draft/reply generation flow (generateMailDraft below) needs
// no changes at all beyond the new instructions field — only the final
// "save directly to Outlook" step is genuinely new.
let mailGraphAvailable = false;
let currentMailGraphMessageId = null;
// currentMailboxKey/currentFolder select WHICH mailbox and folder the next
// list/message/save-draft call targets (mail_graph.go's mailbox_key/folder
// request fields) — "" for currentFolder means the chosen connection's own
// configured default (inbox unless an admin set otherwise), unchanged from
// before folder browsing existed. mailGraphOptionsLoaded guards
// loadMailGraphOptions so switching tabs after the initial page-load status
// check doesn't re-fetch it on every visit.
let currentMailboxKey = "";
let currentFolder = "";
let mailGraphOptionsLoaded = false;

function updateMailDraftSaveGraphVisibility() {
  $("#mailDraftSaveGraph").hidden = !(mailGraphAvailable && currentMailGraphMessageId);
}

function updateMailGraphPanelVisibility() {
  $("#mailGraphPanel").hidden = !mailGraphAvailable;
  updateMailDraftSaveGraphVisibility();
  if (mailGraphAvailable && !mailGraphOptionsLoaded) {
    mailGraphOptionsLoaded = true;
    loadMailGraphOptions();
  }
}

// Any manual edit to the input after a native message was loaded means the
// text no longer necessarily matches that message 1:1 — but the reply is
// still logically "to" that same original message, so this deliberately
// does NOT clear currentMailGraphMessageId; only switching to compose mode
// (above) or loading a *different* native message does. Kept here only as
// the doc-comment anchor for that decision — no listener needed.

// loadMailGraphOptions populates the mailbox picker (own mailbox and/or any
// shared/team mailboxes the caller is authorized for, mail_graph.go's
// findInteractiveExchangeOptions) — called once when the panel first
// becomes visible. Switching the selection resets the folder/message list,
// since a folder ID picked in one mailbox's tree is meaningless in another.
async function loadMailGraphOptions() {
  const sel = $("#mailGraphMailboxSelect");
  try {
    const res = await api("/api/mail/graph/options");
    const options = res.options || [];
    sel.innerHTML = "";
    options.forEach(opt => {
      const el = document.createElement("option");
      el.value = opt.key;
      el.textContent = opt.label;
      sel.appendChild(el);
    });
    currentMailboxKey = options[0] ? options[0].key : "";
    // Only one option (the common case: no shared mailbox configured) —
    // showing a single-choice dropdown would just be visual noise.
    sel.parentElement.hidden = options.length <= 1;
  } catch {
    // Best-effort — the panel still works with currentMailboxKey="",
    // which mail_graph.go's requireInteractiveExchangeConn resolves to the
    // caller's first authorized option anyway (backward-compat fallback).
  }
}
$("#mailGraphMailboxSelect").addEventListener("input", () => {
  currentMailboxKey = $("#mailGraphMailboxSelect").value;
  currentFolder = "";
  $("#mailGraphCurrentFolder").textContent = "";
  $("#mailGraphFolderTree").hidden = true;
  $("#mailGraphFolderTree").innerHTML = "";
  $("#mailGraphList").hidden = true;
  $("#mailGraphList").innerHTML = "";
  $("#mailGraphMessageView").hidden = true;
});

// loadMailGraphFolders fetches the chosen mailbox's folder tree
// (exchangeDiscoverTree via handleMailGraphFolders — the same walker
// Settings' admin-only "Struktur erkunden" button uses) and renders it as a
// clickable tree (renderDiscoverNode's onSelect) — picking a folder sets
// currentFolder and immediately reloads the message list from it.
async function loadMailGraphFolders() {
  const btn = $("#mailGraphFoldersBtn");
  const tree = $("#mailGraphFolderTree");
  setBusy(btn, true);
  try {
    const node = await api("/api/mail/graph/folders", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mailbox_key: currentMailboxKey }),
    });
    tree.innerHTML = "";
    tree.hidden = false;
    tree.appendChild(renderDiscoverNode(node, true, (folderNode) => {
      currentFolder = folderNode.path;
      $("#mailGraphCurrentFolder").textContent = folderNode.path ? t("mail.graphPanel.folderPrefix", { name: folderNode.name }) : "";
      loadMailGraphList();
    }));
  } catch (err) {
    tree.innerHTML = "";
    tree.hidden = false;
    tree.appendChild(qtEl("div", { class: "result error", text: err.message }));
  } finally {
    setBusy(btn, false);
  }
}
$("#mailGraphFoldersBtn").addEventListener("click", loadMailGraphFolders);

async function loadMailGraphList() {
  const btn = $("#mailGraphLoadBtn");
  const out = $("#mailGraphListResult");
  const list = $("#mailGraphList");
  out.className = "result";
  out.textContent = t("mail.graphPanel.loadingMailbox");
  setBusy(btn, true);
  try {
    const res = await api("/api/mail/graph/list", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mailbox_key: currentMailboxKey, folder: currentFolder || undefined }),
    });
    const items = res.items || [];
    list.innerHTML = "";
    list.hidden = items.length === 0;
    out.textContent = items.length === 0 ? t("mail.graphPanel.noMessages") : "";
    items.forEach(item => {
      const li = document.createElement("li");
      const itemBtn = document.createElement("button");
      itemBtn.type = "button";
      itemBtn.className = "mail-graph-item";
      const dt = item.received ? new Date(item.received) : null;
      const dateStr = dt ? `${dt.toLocaleDateString()} ${dt.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}` : "";
      itemBtn.innerHTML =
        `<span class="mail-graph-item-date">${escapeHTML(dateStr)}</span> ` +
        `<span class="mail-graph-item-from">${escapeHTML(item.from || "")}</span> ` +
        `<span class="mail-graph-item-subject">${escapeHTML(item.subject || t("mail.graphPanel.noSubject"))}</span>`;
      itemBtn.addEventListener("click", () => selectMailGraphMessage(item.id));
      li.appendChild(itemBtn);
      list.appendChild(li);
    });
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
}
$("#mailGraphLoadBtn").addEventListener("click", loadMailGraphList);

// selectMailGraphMessage fetches one message's full content and fills the
// manual-input card as if it had been pasted — raw_email
// (mailGraphMessageResponse, mail_graph.go) is the same "From/To/Date/
// Subject + body" text emailFields.String() renders, so it flows straight
// into /api/draft/reply's existing raw_email field with no special-casing.
async function selectMailGraphMessage(id) {
  const view = $("#mailGraphMessageView");
  const meta = $("#mailGraphMessageMeta");
  const bodyEl = $("#mailGraphMessageBody");
  const attList = $("#mailGraphMessageAttachments");
  const out = $("#mailGraphListResult");
  out.className = "result";
  out.textContent = t("mail.graphPanel.loadingMessage");
  try {
    const res = await api("/api/mail/graph/message", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id, mailbox_key: currentMailboxKey }),
    });
    out.textContent = "";
    const receivedStr = res.received ? new Date(res.received).toLocaleString() : "";
    meta.textContent = t("mail.graphPanel.messageMeta", {
      from: res.from || "?",
      receivedPart: receivedStr ? " · " + receivedStr : "",
      subject: res.subject || t("mail.graphPanel.noSubject"),
    });
    bodyEl.textContent = res.body || "";
    // Attachments (res.attachments, mailGraphAttachmentInfo) — previously
    // the live mailbox reader showed no trace of an attachment at all;
    // now each one is listed with its extracted-text status so a missing
    // OCR/markitdown result is visible instead of silently absent.
    attList.innerHTML = "";
    attList.hidden = !res.attachments || res.attachments.length === 0;
    for (const att of res.attachments || []) {
      const li = document.createElement("li");
      const status = att.error ? ` — ${att.error}` : (att.text ? ` — ${t("mail.graphPanel.attachmentTextRead")}` : "");
      li.textContent = `📎 ${att.filename}${status}`;
      if (att.error) li.classList.add("result-error-item");
      attList.appendChild(li);
    }
    view.hidden = false;
    $("#mailMode").value = "reply";
    $("#mailInput").placeholder = t("mail.inputPlaceholder");
    $("#mailInput").value = res.raw_email || "";
    currentMailGraphMessageId = id;
    updateMailDraftSaveGraphVisibility();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  }
}

// lastMailDraftRequest remembers the exact request body of the last
// successful draft generation, so "Entwurf neu generieren" can ask for a
// fresh attempt from the *original* input even if the textarea above has
// since been cleared or edited for a next, unrelated draft.
let lastMailDraftRequest = null;

// mailDraftAbort tracks an in-flight draft generation (generateMailDraft
// below) so a second click on the same button — now relabeled/restyled as
// "Generierung abbrechen" while busy, same pattern as Chat's #askSubmit/
// streamAbort — cancels it instead of firing a second, overlapping request.
// Unlike Chat's stream, a draft has no partial body text to preserve on
// cancel (the body only ever arrives as one final "done" message, see
// requestDraftStream/#4's known gap — draft body doesn't token-stream), so
// cancelling just discards everything and returns to the idle state.
let mailDraftAbort = null;

// generateMailDraft is shared by the "Antwortentwurf nur anzeigen" button
// (fresh input from the form) and "Entwurf neu generieren" (replays
// lastMailDraftRequest unchanged) — same request/response handling either
// way, just a different request body.
async function generateMailDraft(body) {
  const btn = $("#mailDraftBtn");
  const out = $("#mailDraftResult");
  // A generation is already in flight and this button is now acting as
  // "Cancel" (see below) — abort it instead of starting a second,
  // overlapping request. Mirrors Chat's #askForm submit handler / streamAbort.
  if (mailDraftAbort) { mailDraftAbort.abort(); return; }
  out.className = "result";
  out.textContent = t("modal.draftGenerating");
  // Live step timeline while the draft is generated — remove any leftover
  // panel from a previous generation first (each call creates a new one).
  const prevSteps = out.parentNode.querySelector(".agent-steps");
  if (prevSteps) prevSteps.remove();
  const steps = agentStepsPanel();
  out.insertAdjacentElement("afterend", steps.el);
  mailDraftAbort = new AbortController();
  btn.classList.add("stop-mode");
  const originalBtnLabel = btn.textContent;
  btn.textContent = t("mail.draftCancel");
  try {
    const res = await requestDraftStream(body, (step) => steps.add(step), mailDraftAbort.signal);
    steps.finish();
    lastMailDraftRequest = body;
    out.textContent = "";
    $("#mailDraftSection").hidden = false;
    $("#mailDraftSubject").value = res.subject || "";
    $("#mailDraftBody").value = res.reply_text || "";

    // Reply mode only: composeNewMail (brief mode) never sets original_mail,
    // so "from"/"subject" stay empty and this context line simply stays
    // hidden — no separate mode check needed here.
    const orig = res.original_mail || {};
    const ctx = $("#mailDraftContext");
    if (orig.from || orig.subject) {
      const parts = [];
      if (orig.from) parts.push(t("mail.replyContext.from", { from: orig.from }));
      if (orig.subject) parts.push(t("mail.replyContext.subject", { subject: orig.subject }));
      ctx.textContent = t("mail.replyContext.prefix", { parts: parts.join(", ") });
      ctx.hidden = false;
      // Prefills "An" from the original sender so it doesn't have to be
      // copied by hand — still just a suggestion, editable/clearable like
      // every other field before anything is downloaded/filed.
      if (orig.from) $("#mailDraftTo").value = orig.from;
    } else {
      ctx.hidden = true;
      ctx.textContent = "";
    }

    const citCont = $("#mailDraftCitations");
    citCont.innerHTML = "";
    (res.citations || []).forEach(c => citCont.appendChild(buildCitationChip(c)));
    renderDebugPanel($("#mailDraftDebug"), res.debug);
    $("#mailDraftFeedbackUp").classList.remove("feedback-btn-active");
    $("#mailDraftFeedbackDown").classList.remove("feedback-btn-active");
    $("#mailDraftFeedbackUp").disabled = false;
    $("#mailDraftFeedbackDown").disabled = false;
  } catch (err) {
    steps.finish();
    if (err.name === "AbortError") {
      out.className = "result";
      out.textContent = t("mail.draftCancelled");
    } else {
      out.className = "result error";
      out.textContent = t("common.errorPrefix", { message: err.message });
    }
  } finally {
    mailDraftAbort = null;
    btn.classList.remove("stop-mode");
    btn.textContent = originalBtnLabel;
  }
}

$("#mailDraftBtn").addEventListener("click", () => {
  // Generation in progress → this click means "Cancel" (button is relabeled
  // while busy, see generateMailDraft) — checked before the empty-input
  // guard below so clearing the input mid-generation still lets it cancel.
  if (mailDraftAbort) { mailDraftAbort.abort(); return; }
  const out = $("#mailDraftResult");
  const input = $("#mailInput").value.trim();
  const compose = $("#mailMode").value === "compose";
  out.className = "result";
  if (!input) { out.textContent = t("mail.emptyInput"); return; }
  // A genuinely new draft (as opposed to "Entwurf neu generieren", which
  // reformulates the SAME source) starts without whatever was attached to
  // a previous, unrelated draft.
  clearMailAttachments();
  // Length/Format/Instructions are optional draft-shape hints
  // (draftReplyRequest, handlers.go) — empty values are omitted so the
  // model keeps its default (no situational note either).
  const shape = {};
  const len = $("#mailLength") && $("#mailLength").value;
  const fmt = $("#mailFormat") && $("#mailFormat").value;
  const instructions = $("#mailInstructions") && $("#mailInstructions").value.trim();
  if (len) shape.length = len;
  if (fmt) shape.format = fmt;
  if (instructions) shape.instructions = instructions;
  generateMailDraft(compose ? { brief: input, ...shape } : { raw_email: input, ...shape });
});

$("#mailDraftRegenerate").addEventListener("click", () => {
  const out = $("#mailDraftResult");
  if (!lastMailDraftRequest) { out.className = "result error"; out.textContent = t("mail.emptyInput"); return; }
  generateMailDraft(lastMailDraftRequest);
});

// "Stil anwenden" — rewrites whatever's currently in the body field (may
// already be human-edited) in the selected tone via /api/draft/restyle
// (draft.go's restyleDraftText). Pure rewrite, no retrieval/tools — the
// facts are already grounded from the original draft; only tone/wording
// changes, so this is much cheaper/faster than a full regenerate.
$("#mailDraftRestyle").addEventListener("click", async () => {
  const btn = $("#mailDraftRestyle");
  const out = $("#mailDraftResult");
  const text = $("#mailDraftBody").value.trim();
  out.className = "result";
  if (!text) { out.textContent = t("mail.emptyInput"); return; }
  out.textContent = t("modal.draftGenerating");
  setBusy(btn, true);
  try {
    const res = await api("/api/draft/restyle", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text, style: $("#mailDraftStyle").value }),
    });
    $("#mailDraftBody").value = res.text || text;
    out.textContent = "";
  } catch (err) {
    out.className = "result error";
    out.textContent = t("common.errorPrefix", { message: err.message });
  } finally {
    setBusy(btn, false);
  }
});

// buildMailSignature renders "Freundliche Grüße" + the logged-in account's
// own name/department + "Rubix GmbH" — from currentUserClaims (the same
// session state "Mein Konto" reads), never anything the model proposed.
// Falls back to bracketed placeholders when nobody's logged in (LDAP off,
// or a guest), so the button stays useful either way — the human filling
// them in is exactly the point, same spirit as the model's own "[Ihr
// Name]"-style placeholders elsewhere.
// buildMailSignature prefers the user's own custom Signature (Mein Konto →
// Persönlicher Kontext, Phase 4) once they've opted in and actually filled
// one in — falls back to the generic AD-derived placeholder otherwise,
// exactly as before this field existed. myPersonalContext is only ever
// populated by openAccountModal's own load, so a session that never opened
// "Mein Konto" this visit simply keeps using the fallback — never a
// network call from here, matching this function's existing "instant,
// synchronous" contract (its one caller expects a string back immediately).
function buildMailSignature() {
  const claims = currentUserClaims || {};
  const p = myPersonalContext;
  if (p && p.use_personal_context && (p.signature || "").trim()) {
    return p.signature.trim();
  }
  const lines = ["Freundliche Grüße", (p && p.display_name) || claims.display_name || claims.user || "[Ihr Name]"];
  if (p && p.department) lines.push(p.department);
  else if (claims.department) lines.push(claims.department);
  lines.push("Rubix GmbH");
  return lines.join("\n");
}

// "Signatur einfügen" — deliberately never something the model itself
// proposes (see draft.go's defaultDraftSystemPrompt: it's explicitly told
// not to invent or copy a sender name/title from retrieved context, since
// that risks naming a real, unrelated person from a past email). Strips
// any existing "Freundliche Grüße"-onward closing first, so re-clicking
// after a regenerate/restyle never piles up duplicate signatures.
$("#mailDraftInsertSignature").addEventListener("click", () => {
  const bodyEl = $("#mailDraftBody");
  const withoutClosing = bodyEl.value.replace(/\n*Freundliche Gr[üu]ße[\s\S]*$/i, "").replace(/\s+$/, "");
  bodyEl.value = withoutClosing + "\n\n" + buildMailSignature();
});

// Mail draft feedback (👍/👎) — same /api/feedback endpoint and JSONL log
// as Chat's per-answer feedback (feedback.go), just fed from the Mail
// tab's own state instead of a .msg element: "question" is whatever was
// actually asked of the model (the brief, or the pasted incoming mail),
// "answer" is the current (possibly human-edited) draft body.
function mailDraftFeedbackQuestion() {
  if (!lastMailDraftRequest) return "";
  return lastMailDraftRequest.brief || lastMailDraftRequest.raw_email || "";
}
async function voteMailDraft(rating, btn, otherBtn) {
  btn.disabled = true;
  otherBtn.disabled = true;
  try {
    await api("/api/feedback", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        question: mailDraftFeedbackQuestion(),
        answer: $("#mailDraftBody").value,
        citations: $all("#mailDraftCitations .citation").map(el => el.textContent.trim()),
        rating,
      }),
    });
    btn.classList.add("feedback-btn-active");
  } catch {
    btn.disabled = false;
    otherBtn.disabled = false;
  }
}
$("#mailDraftFeedbackUp").addEventListener("click", () => voteMailDraft("up", $("#mailDraftFeedbackUp"), $("#mailDraftFeedbackDown")));
$("#mailDraftFeedbackDown").addEventListener("click", () => voteMailDraft("down", $("#mailDraftFeedbackDown"), $("#mailDraftFeedbackUp")));

$("#mailDraftCopy").addEventListener("click", async () => {
  const btn = $("#mailDraftCopy");
  try {
    await navigator.clipboard.writeText($("#mailDraftBody").value);
    btn.textContent = t("modal.copied");
    setTimeout(() => { btn.textContent = t("modal.copyDraft"); }, 1500);
  } catch { btn.textContent = t("common.error"); }
});

// buildEmlText renders the reviewed draft as a minimal RFC 5322 message —
// CRLF line endings, UTF-8 text body — so "Als .eml herunterladen" yields
// a file Outlook & Co. open directly as an editable mail. Header-injection
// safe by construction: To/Subject are folded onto one line (any newline a
// user pasted into the inputs is replaced by a space) before being placed
// in a header.
function buildEmlText(to, subject, body) {
  const clean = s => s.replace(/[\r\n]+/g, " ").trim();
  const lines = [];
  if (clean(to)) lines.push(`To: ${clean(to)}`);
  lines.push(`Subject: ${clean(subject)}`);
  lines.push(`Date: ${new Date().toUTCString()}`);
  lines.push("MIME-Version: 1.0");
  lines.push('Content-Type: text/plain; charset="UTF-8"');
  lines.push("X-Unsent: 1"); // Outlook: open in compose (editable) mode, not read mode
  lines.push("");
  lines.push(body.replace(/\r?\n/g, "\r\n"));
  return lines.join("\r\n");
}

// ---- Mail-tab attachments -----------------------------------------------
// Unlike Chat/Agent's image attach (wireImageAttach above), any file type
// is valid here — a scanned PDF is just as legitimate an attachment as a
// photo — so this is its own, simpler widget: no vision/OCR hint, no
// "used once per question" ephemerality (a draft's attachment should
// survive "Stil anwenden"/edits to the body, only cleared when a
// genuinely new draft is generated — see #mailDraftBtn above). Same
// {filename, data_base64} wire shape as chat images
// (mailAttachmentInput, mail.go).
const mailAttachMaxCount = 5;
const mailAttachMaxBytes = 15 * 1024 * 1024;
let mailAttachItems = []; // {file, base64}

function renderMailAttachList() {
  const list = $("#mailAttachList");
  if (!list) return;
  list.hidden = mailAttachItems.length === 0;
  list.innerHTML = "";
  mailAttachItems.forEach((it, i) => {
    const chip = document.createElement("div");
    chip.className = "attach-chip";
    const name = document.createElement("span");
    name.className = "attach-chip-name";
    name.textContent = it.file.name;
    const rm = document.createElement("button");
    rm.type = "button";
    rm.className = "attach-chip-remove";
    rm.setAttribute("aria-label", t("mail.attach.removeAttachment"));
    rm.title = t("mail.attach.removeAttachment");
    rm.textContent = "×";
    rm.addEventListener("click", () => { mailAttachItems.splice(i, 1); renderMailAttachList(); });
    chip.append(name, rm);
    list.appendChild(chip);
  });
}

function clearMailAttachments() { mailAttachItems = []; renderMailAttachList(); }

// mailAttachmentsPayload() is read by all three actions below
// (save-imap/send-self/.eml) — the same attached files ride along with
// whichever of them the user clicks, in any order, until cleared.
function mailAttachmentsPayload() {
  return mailAttachItems.map(it => ({ filename: it.file.name, data_base64: it.base64 }));
}

$("#mailAttachBtn")?.addEventListener("click", () => $("#mailAttachInput")?.click());
$("#mailAttachInput")?.addEventListener("change", () => {
  const input = $("#mailAttachInput");
  const room = mailAttachMaxCount - mailAttachItems.length;
  const picked = [...input.files].slice(0, Math.max(room, 0));
  if (input.files.length > picked.length) {
    alert(t("mail.attach.tooMany", { max: mailAttachMaxCount }));
  }
  picked.forEach(file => {
    if (file.size > mailAttachMaxBytes) {
      alert(t("mail.attach.tooLarge", { name: file.name, max: mailAttachMaxBytes / (1024 * 1024) }));
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      mailAttachItems.push({ file, base64: (reader.result.split(",")[1] || "") });
      renderMailAttachList();
    };
    reader.readAsDataURL(file);
  });
  input.value = "";
});

// "Als .eml herunterladen": the common attachment-free case stays a pure
// client-side download (buildEmlText, no server round-trip); with at
// least one attachment, building valid MIME by hand in JS isn't worth
// duplicating — /api/draft/eml (handlers.go) renders the same
// buildMultipartEmail bytes the save-imap/send-self paths use and hands
// them back as a download instead.
$("#mailDraftEml").addEventListener("click", async () => {
  const to = $("#mailDraftTo").value, subject = $("#mailDraftSubject").value, body = $("#mailDraftBody").value;
  const attachments = mailAttachmentsPayload();
  if (attachments.length === 0) {
    const eml = buildEmlText(to, subject, body);
    const blob = new Blob([eml], { type: "message/rfc822" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = "entwurf.eml";
    a.click();
    URL.revokeObjectURL(a.href);
    return;
  }
  const out = $("#mailDraftResult");
  try {
    const res = await fetch("/api/draft/eml", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ to, subject, body, attachments }),
    });
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || "Herunterladen fehlgeschlagen");
    const blob = await res.blob();
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = "entwurf.eml";
    a.click();
    URL.revokeObjectURL(a.href);
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  }
});

// "An mich senden" reuses the same /api/chat/email endpoint the chat
// answer's "An mich senden" button uses (handleChatEmail, handlers.go) —
// sends only to the logged-in account's own AD address, never a
// caller-supplied one, and requires SMTP configured. Subject/body double
// up the chat-email request's question/answer fields; citations are read
// back off the rendered chip bar rather than kept as separate state.
$("#mailDraftSendSelf").addEventListener("click", async () => {
  const btn = $("#mailDraftSendSelf");
  const out = $("#mailDraftResult");
  const citations = $all("#mailDraftCitations .citation").map(el => el.textContent.trim());
  out.className = "result";
  setBusy(btn, true);
  try {
    await api("/api/chat/email", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        question: $("#mailDraftSubject").value.trim() || t("mail.draftHeading"),
        answer: $("#mailDraftBody").value,
        citations,
        attachments: mailAttachmentsPayload(),
      }),
    });
    out.textContent = t("mail.sentToSelf");
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

$("#mailDraftSaveImap").addEventListener("click", async () => {
  const btn = $("#mailDraftSaveImap");
  const out = $("#mailDraftResult");
  out.className = "result";
  setBusy(btn, true);
  try {
    const res = await api("/api/draft/save-imap", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        to: $("#mailDraftTo").value.trim(),
        subject: $("#mailDraftSubject").value.trim(),
        body: $("#mailDraftBody").value,
        attachments: mailAttachmentsPayload(),
      }),
    });
    out.textContent = t("mail.draft.savedImap", { mailbox: res.mailbox });
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

// "In mein Outlook-Postfach speichern" — the interactive-per-user analogue
// of "In Postfach-Entwürfe speichern" above: files this reviewed reply
// directly into the CALLER'S OWN Outlook Drafts folder via Microsoft Graph
// (handleMailGraphSaveDraft, mail_graph.go), addressed to whichever message
// currentMailGraphMessageId remembers from selectMailGraphMessage. Only
// ever visible (updateMailDraftSaveGraphVisibility) when that association
// exists — never admin-gated like the IMAP button above, since this acts on
// the logged-in user's own mailbox, not a shared service one.
$("#mailDraftSaveGraph").addEventListener("click", async () => {
  const btn = $("#mailDraftSaveGraph");
  const out = $("#mailDraftResult");
  out.className = "result";
  if (!currentMailGraphMessageId) {
    out.className = "result error";
    out.textContent = t("mail.draft.noMessageSelected");
    return;
  }
  setBusy(btn, true);
  try {
    const res = await api("/api/mail/graph/save-draft", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        original_message_id: currentMailGraphMessageId,
        body: $("#mailDraftBody").value,
        mailbox_key: currentMailboxKey,
      }),
    });
    out.textContent = t("mail.draft.savedGraph", { draftId: res.draft_id });
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

// Re-render inline [Qn] markers after citations arrive (end of stream).
function enrichInlineCites(contentEl, citations) {
  const byMarker = {};
  (citations || []).forEach(c => { if (c.marker) byMarker[c.marker] = c; });
  contentEl.querySelectorAll(".inline-cite[data-cite]").forEach(el => {
    const cit = byMarker[el.dataset.cite];
    if (!cit) return;
    el.title = cit.source_name;
    if (cit.source_url && el.tagName !== "A") {
      const a = document.createElement("a");
      a.className = el.className;
      a.dataset.cite = el.dataset.cite;
      a.href = cit.source_url;
      a.target = "_blank";
      a.rel = "noopener";
      a.title = cit.source_name;
      a.textContent = el.textContent;
      el.replaceWith(a);
    }
  });
}

// ---- New chat ---------------------------------------------------------
// Two entry points share this: the sidebar's always-visible "Neues
// Gespräch" button (#sidebarNewChat — the ChatGPT/Claude convention of a
// primary new-chat action at the top of the rail) and the history
// drawer's header button (#historyNewChat). Both jump to and reset Chat
// (the Agent tier lives inside Chat now, not a separate tab/history —
// see #askTier in tab-chat.html).
function startNewChat() {
  if (!$("#navtab-chat").classList.contains("active")) activateTab($("#navtab-chat"));
  if (streamAbort) streamAbort.abort();
  $all("#chatLog .msg").forEach(el => el.remove());
  const empty = $("#chatEmpty");
  if (empty) empty.hidden = false;
  currentConversation = null; // next question starts (and, if enabled, saves) a fresh entry
  sessionMessages = []; // no more multi-turn memory of the conversation just cleared
  loadStats();
  closeHistoryModal();
  $("#question").focus();
}
$("#historyNewChat").addEventListener("click", startNewChat);
$("#sidebarNewChat").addEventListener("click", startNewChat);

// ---- Chat history (browser-local, or server-side once logged in) -------
// Gated by settings.enable_chat_history (see handleAuthStatus), but
// *where* a conversation lives depends on login state — see
// serverHistoryMode() below and settings.go's EnableChatHistory doc
// comment for the full reasoning:
//   - not logged in (or LDAP disabled entirely): exactly the original
//     behavior — conversations live only in this browser's localStorage,
//     "this device remembers what was asked from it," never synced.
//   - logged in: conversations are stored server-side (chathistory.go),
//     scoped to that AD account, so they follow the person across
//     browsers/devices. The server enforces the per-account isolation
//     (see chatHistoryStore's doc comment) — the client never needs to
//     (and couldn't) request someone else's conversations.
const HISTORY_KEY = "r3_chat_history";
const HISTORY_MAX = 50; // oldest conversations drop off past this to bound localStorage growth

// serverHistoryMode reports whether history for the current visitor is
// server-backed (logged in) rather than the older localStorage-only path.
function serverHistoryMode() {
  return authTierActive && isLoggedIn;
}

// loadHistoryList reads the browser-local stored conversation list,
// tolerating a missing/corrupt/non-array value by falling back to an
// empty list. Local-mode only — see fetchServerHistoryList for the
// server-mode equivalent.
function loadHistoryList() {
  try {
    const list = JSON.parse(localStorage.getItem(HISTORY_KEY) || "[]");
    return Array.isArray(list) ? list : [];
  } catch { return []; }
}

// saveHistoryList writes the conversation list back to localStorage,
// swallowing quota/private-mode errors since history is best-effort.
function saveHistoryList(list) {
  try { localStorage.setItem(HISTORY_KEY, JSON.stringify(list)); } catch { /* quota/private-mode — history is best-effort */ }
}

// fetchServerHistoryList fetches the logged-in caller's own conversation
// list from the server and normalizes it to the same {id, title,
// updatedAt} shape loadHistoryList's entries have, so rendering code
// doesn't need to branch on which mode produced the list.
async function fetchServerHistoryList() {
  try {
    const rows = await api("/api/chat/conversations");
    return (rows || []).map(r => ({ id: r.id, title: r.title, updatedAt: r.updated_at, mode: r.mode || "chat" }));
  } catch { return []; }
}

// persistConversation upserts an in-progress conversation (currentConversation
// — anything shaped like {id, title, mode, updatedAt, messages}) — server-side
// (one row, via its own save endpoint) if logged in, otherwise into the
// browser-local list (replacing any earlier save of it, re-sorted by
// recency, trimmed to HISTORY_MAX). Called after every recorded message,
// fire-and-forget from its callers, so a crash or tab close never loses
// more than the message in flight; best-effort either way, matching the
// pre-existing local-storage semantics. mode ("chat" or "agent") records
// which #askTier was selected when the conversation started — the former
// separate Agent tab used its own currentAgentConversation/mode:"agent"
// for this; now it's just which tier Chat's askQuestion happened to send.
async function persistConversation(conv) {
  if (!chatHistoryEnabled || !conv || !conv.messages.length) return;
  if (serverHistoryMode()) {
    try {
      await api("/api/chat/conversations/save", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          id: conv.id,
          title: conv.title,
          mode: conv.mode,
          messages: conv.messages,
        }),
      });
    } catch { /* best-effort, same as the local-storage path below */ }
    return;
  }
  const list = loadHistoryList().filter(c => c.id !== conv.id);
  list.unshift(conv);
  list.sort((a, b) => b.updatedAt - a.updatedAt);
  saveHistoryList(list.slice(0, HISTORY_MAX));
}

// recordUserMessage starts a new currentConversation on the first question
// of a chat (titled from the question text) or appends to it otherwise,
// then persists it — a no-op when chat history is disabled. The
// conversation id is always generated client-side (both modes) — the
// server accepts it on first save and, for a logged-in caller, binds it
// to that session's account from then on (see chathistory.go's save).
function recordUserMessage(question) {
  if (!chatHistoryEnabled) return;
  if (!currentConversation) {
    currentConversation = {
      id: (window.crypto && crypto.randomUUID) ? crypto.randomUUID() : `local-${Date.now()}`,
      title: question.slice(0, 80),
      updatedAt: Date.now(),
      mode: ($("#askTier") && $("#askTier").value === "agent") ? "agent" : "chat",
      messages: [],
    };
  }
  currentConversation.messages.push({ role: "user", content: question });
  currentConversation.updatedAt = Date.now();
  persistConversation(currentConversation);
}

// recordAssistantMessage appends the assistant's answer (with its
// citations) to currentConversation and persists it.
function recordAssistantMessage(content, citations) {
  if (!chatHistoryEnabled || !currentConversation) return;
  currentConversation.messages.push({ role: "assistant", content, citations: citations || [] });
  currentConversation.updatedAt = Date.now();
  persistConversation(currentConversation);
}

// updateHistoryButtonVisibility shows/hides the sidebar "Verlauf" entry
// point. Once LDAP is enabled, history is a login-gated feature (server-
// side, per account) — an anonymous visitor in that mode sees no history
// entry point at all, rather than falling back to a local-only history
// that would look "lost" the moment they log in. Without LDAP, behavior
// is unchanged: available to everyone whenever the setting is on.
function updateHistoryButtonVisibility() {
  $("#navtab-history").hidden = !chatHistoryEnabled || (authTierActive && !isLoggedIn);
}

// updateMyAccountVisibility shows the "Mein Konto" sidebar entry
// (docs/UI_HARDENING_PLAN.md) only when there's actually an account to
// show — no LDAP configured (nothing to log into) or not logged in both
// hide it, same reasoning as updateHistoryButtonVisibility above.
function updateMyAccountVisibility() {
  const btn = $("#navtab-account");
  if (btn) btn.hidden = !authTierActive || !isLoggedIn;
}

// openAccountModal fills the "Mein Konto" modal from currentUserClaims —
// name/department/title/office, whichever are non-empty — and shows it.
// myPersonalContext caches the logged-in user's own personal-context
// fields (userprefs.go, Phase 4) — loaded whenever the account modal opens,
// and reused by buildMailSignature (below) so "Signatur einfügen" in the
// Mail tab can prefer a custom signature without a network round-trip on
// every click. null until first successfully loaded (never guessed at).
let myPersonalContext = null;

// personalContextFieldMap pairs each modal input's element id with the
// userPrefs (userprefs.go) field it edits — one place driving both
// "populate the form" and "read the form back for saving" below, so the
// two can never drift out of sync with each other.
const personalContextFieldMap = [
  ["accountPersonalDisplayName", "display_name"],
  ["accountPersonalPosition", "position"],
  ["accountPersonalDepartment", "department"],
  ["accountPersonalContactInfo", "contact_info"],
  ["accountPersonalCommunicationStyle", "communication_style"],
  ["accountPersonalSignature", "signature"],
  ["accountPersonalTypicalPhrasing", "typical_phrasing"],
  ["accountPersonalAINotes", "ai_notes"],
];

let accountModalReturnFocus = null;
async function openAccountModal() {
  accountModalReturnFocus = document.activeElement;
  const fields = $("#accountModalFields");
  fields.innerHTML = "";
  const claims = currentUserClaims || {};
  const rows = [
    [t("account.fieldName"), claims.user],
    [t("account.fieldDepartment"), claims.department],
    [t("account.fieldTitle"), claims.title],
    [t("account.fieldOffice"), claims.office],
  ];
  for (const [label, value] of rows) {
    if (!value) continue;
    const dt = document.createElement("dt");
    dt.textContent = label;
    const dd = document.createElement("dd");
    dd.textContent = value;
    fields.appendChild(dt);
    fields.appendChild(dd);
  }

  // Personal-context form: populate from the server (not just the cache)
  // every time the modal opens, so a value saved from another device is
  // always reflected here.
  $("#accountPersonalResult").className = "result";
  $("#accountPersonalResult").textContent = "";
  try {
    const p = await api("/api/account/prefs");
    myPersonalContext = p;
    $("#accountPersonalUse").checked = !!p.use_personal_context;
    personalContextFieldMap.forEach(([elId, key]) => { $("#" + elId).value = p[key] || ""; });
  } catch {
    // Best-effort: an unreachable prefs endpoint leaves the form at
    // whatever it last showed rather than blocking the modal from opening.
  }

  $("#accountModal").hidden = false;
  $("#accountModalClose").focus();
}
// closeAccountModal hides the account modal and restores keyboard focus —
// same pattern as closeHistoryModal.
function closeAccountModal() {
  $("#accountModal").hidden = true;
  if (accountModalReturnFocus && document.body.contains(accountModalReturnFocus)) accountModalReturnFocus.focus();
  accountModalReturnFocus = null;
}

$("#navtab-account").addEventListener("click", openAccountModal);
$("#accountModalClose").addEventListener("click", closeAccountModal);
$("#accountModal").addEventListener("click", (e) => { if (e.target.id === "accountModal") closeAccountModal(); });
$("#accountModalLogout").addEventListener("click", () => { closeAccountModal(); doLogout(); });
// Same Escape-to-close + Tab focus-trap pattern as #historyModal/#sourceModal.
document.addEventListener("keydown", (e) => {
  if ($("#accountModal").hidden) return;
  if (e.key === "Escape") { closeAccountModal(); return; }
  if (e.key === "Tab") {
    const focusables = $all("#accountModal button, #accountModal a[href], #accountModal input, #accountModal select, #accountModal textarea")
      .filter(el => !el.hidden && !el.disabled && el.offsetParent !== null);
    if (!focusables.length) return;
    const first = focusables[0], last = focusables[focusables.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }
});

// "Speichern" in the personal-context section — POSTs every field at once
// to its own endpoint (handleUserPrefsSetPersonalContext, userprefs.go),
// deliberately separate from the language switcher's /api/account/prefs/set
// so the two save actions can never clobber each other's fields (see that
// handler's doc comment).
$("#accountPersonalSave").addEventListener("click", async () => {
  const btn = $("#accountPersonalSave");
  const out = $("#accountPersonalResult");
  out.className = "result";
  const body = { use_personal_context: $("#accountPersonalUse").checked };
  personalContextFieldMap.forEach(([elId, key]) => { body[key] = $("#" + elId).value; });
  setBusy(btn, true);
  try {
    await api("/api/account/prefs/personal", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    myPersonalContext = body;
    out.textContent = t("account.personal.saved");
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

// renderHistoryList rebuilds the history modal's conversation list —
// from the server if logged in, from localStorage otherwise — wiring up
// each row's open/rename/delete controls.
async function renderHistoryList() {
  const list = serverHistoryMode() ? await fetchServerHistoryList() : loadHistoryList();
  const ul = $("#historyList");
  const empty = $("#historyEmpty");
  ul.innerHTML = "";
  ul.hidden = list.length === 0;
  empty.hidden = list.length !== 0;
  list.forEach(conv => {
    const li = document.createElement("li");
    const openBtn = document.createElement("button");
    openBtn.type = "button";
    openBtn.className = "history-item-open";
    openBtn.dataset.id = conv.id;
    const modeKey = conv.mode === "agent" ? "history.modeAgent" : "history.modeChat";
    const modeBadge = `<span class="settings-badge history-item-mode history-item-mode-${conv.mode === "agent" ? "agent" : "chat"}">${escapeHTML(t(modeKey))}</span>`;
    openBtn.innerHTML = modeBadge +
      `<span class="pst-folder-path" title="${escapeHTML(conv.title || "")}">${escapeHTML(conv.title || t("history.untitled"))}</span>`;
    const when = document.createElement("span");
    when.className = "pst-folder-count";
    when.textContent = new Date(conv.updatedAt).toLocaleString();
    const renameBtn = document.createElement("button");
    renameBtn.type = "button";
    renameBtn.className = "history-item-rename ghost-btn";
    renameBtn.dataset.id = conv.id;
    renameBtn.textContent = "✎";
    renameBtn.setAttribute("aria-label", t("history.renameOne"));
    const delBtn = document.createElement("button");
    delBtn.type = "button";
    delBtn.className = "history-item-delete ghost-btn";
    delBtn.dataset.id = conv.id;
    delBtn.textContent = "🗑";
    delBtn.setAttribute("aria-label", t("history.deleteOne"));
    li.append(openBtn, when, renameBtn, delBtn);
    ul.appendChild(li);
  });
}

// renameConversation prompts for a new title for a stored conversation
// and re-renders the list; a cancelled prompt leaves the title untouched.
async function renameConversation(id) {
  if (serverHistoryMode()) {
    const list = await fetchServerHistoryList();
    const conv = list.find(c => c.id === id);
    if (!conv) return;
    const next = prompt(t("history.renamePrompt"), conv.title || "");
    if (next === null) return; // cancelled
    const title = next.trim().slice(0, 80) || t("history.untitled");
    try {
      await api("/api/chat/conversations/rename", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id, title }),
      });
    } catch { /* best-effort */ }
    if (currentConversation && currentConversation.id === id) currentConversation.title = title;
    renderHistoryList();
    return;
  }
  const list = loadHistoryList();
  const conv = list.find(c => c.id === id);
  if (!conv) return;
  const next = prompt(t("history.renamePrompt"), conv.title || "");
  if (next === null) return; // cancelled
  conv.title = next.trim().slice(0, 80) || t("history.untitled");
  saveHistoryList(list);
  if (currentConversation && currentConversation.id === id) currentConversation.title = conv.title;
  renderHistoryList();
}

// openConversation replaces Chat's live log with a saved conversation's
// messages, switching to the Chat tab first (a shared history list can
// reopen an entry regardless of which tab happens to be active right now)
// and aborting any in-flight answer there so the old stream can't keep
// appending to the now-swapped-out log. If the conversation's mode was
// "agent" (the former Agent tab, or a Chat message sent with the Agent
// tier selected), the tier selector is switched to "agent" too, so
// continuing the conversation keeps using the same tier it was started
// with.
async function openConversation(id) {
  let conv = null;
  if (serverHistoryMode()) {
    try {
      const res = await api("/api/chat/conversations/get?id=" + encodeURIComponent(id));
      conv = { id, title: res.title, updatedAt: res.updated_at, mode: res.mode, messages: res.messages || [] };
    } catch { conv = null; }
  } else {
    conv = loadHistoryList().find(c => c.id === id) || null;
  }
  if (!conv) return;
  activateTab($("#navtab-chat"));
  if (streamAbort) streamAbort.abort();
  $all("#chatLog .msg").forEach(el => el.remove());
  (conv.messages || []).forEach(m => addMessage(m.role, m.content, m.citations));
  $("#chatEmpty").hidden = true;
  currentConversation = conv;
  sessionMessages = (conv.messages || []).map(m => ({ role: m.role, content: m.content }));
  if ($("#askTier")) $("#askTier").value = conv.mode === "agent" ? "agent" : "standard";
  updateAgentDemoButtonsVisibility();
  closeHistoryModal();
  loadStats();
}

// deleteConversation removes one saved conversation and clears
// currentConversation if it was the one open, then re-renders the list.
async function deleteConversation(id) {
  if (serverHistoryMode()) {
    try {
      await api("/api/chat/conversations/delete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id }),
      });
    } catch { /* best-effort */ }
  } else {
    saveHistoryList(loadHistoryList().filter(c => c.id !== id));
  }
  if (currentConversation && currentConversation.id === id) currentConversation = null;
  renderHistoryList();
}

let historyModalReturnFocus = null;
// openHistoryModal opens the history modal, remembering the previously
// focused element so closeHistoryModal can restore focus to it.
async function openHistoryModal() {
  historyModalReturnFocus = document.activeElement;
  await renderHistoryList();
  $("#historyModal").hidden = false;
  $("#historyModalClose").focus();
}
// closeHistoryModal hides the history modal and restores keyboard focus.
function closeHistoryModal() {
  $("#historyModal").hidden = true;
  if (historyModalReturnFocus && document.body.contains(historyModalReturnFocus)) historyModalReturnFocus.focus();
  historyModalReturnFocus = null;
}

$("#navtab-history").addEventListener("click", openHistoryModal);
$("#historyModalClose").addEventListener("click", closeHistoryModal);
$("#historyModal").addEventListener("click", (e) => { if (e.target.id === "historyModal") closeHistoryModal(); });
$("#historyList").addEventListener("click", (e) => {
  const openBtn = e.target.closest(".history-item-open");
  if (openBtn) { openConversation(openBtn.dataset.id); return; }
  const renameBtn = e.target.closest(".history-item-rename");
  if (renameBtn) { renameConversation(renameBtn.dataset.id); return; }
  const delBtn = e.target.closest(".history-item-delete");
  if (delBtn && confirm(t("history.confirmDeleteOne"))) deleteConversation(delBtn.dataset.id);
});
$("#historyClearAll").addEventListener("click", async () => {
  if (!confirm(t("history.confirmClearAll"))) return;
  if (serverHistoryMode()) {
    try { await api("/api/chat/conversations/delete-all", { method: "POST" }); } catch { /* best-effort */ }
  } else {
    saveHistoryList([]);
  }
  currentConversation = null;
  renderHistoryList();
});
// Same Escape-to-close + Tab focus-trap pattern as #sourceModal (see its
// keydown handler for the fuller rationale) — a second, near-identical
// listener rather than a shared helper, since each modal's focusable-set
// query is scoped to its own #id.
document.addEventListener("keydown", (e) => {
  if ($("#historyModal").hidden) return;
  if (e.key === "Escape") { closeHistoryModal(); return; }
  if (e.key === "Tab") {
    const focusables = $all("#historyModal button, #historyModal a[href]").filter(el => !el.hidden && el.offsetParent !== null);
    if (!focusables.length) return;
    const first = focusables[0], last = focusables[focusables.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }
});

// ---- Textarea auto-resize / keyboard ---------------------------------
(function () {
  const ta = $("#question");
  // grow auto-expands the question textarea's height to fit its content
  // as the user types, instead of scrolling inside a fixed box.
  function grow() {
    ta.style.height = "auto";
    ta.style.height = ta.scrollHeight + "px";
  }
  ta.addEventListener("input", grow);
  // Enter submits, Shift+Enter inserts a newline (Ctrl/Cmd+Enter also submits).
  ta.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      $("#askForm").requestSubmit();
    }
  });
})();

// ---- Chat: ask form -------------------------------------------------
// /api/ask streams newline-delimited JSON: "token" lines (text chunks),
// one "done" line with citations, or an "error" line on failure.
// While a response streams, the send button becomes a stop button.
let streamAbort = null;

// If an admin has turned on Settings → API-Zugriff → "API-Key erforderlich",
// /api/ask starts rejecting requests with 401 — including the bundled UI's
// own, since there's no special-casing for same-origin requests (see
// handlers.go's requireAPIKey doc comment). r3_api_key in localStorage is
// this UI's own credential for that case: unset by default (nothing to do
// as long as the toggle is off), prompted for once on the first 401 and
// reused after that.
function apiKeyHeaders() {
  const key = localStorage.getItem("r3_api_key");
  return key ? { "X-API-Key": key } : {};
}

// voiceExtensionForMime maps a MediaRecorder mimeType to a matching file
// extension so the uploaded filename actually reflects the blob's real
// container — previously always "r3-voice.webm" regardless of which
// mimeType actually recorded, which would have misled the server's
// extension-based dispatch (whisper.go's transcribeAudio) once a non-webm
// fallback (see VOICE_MIME_CANDIDATES below) was ever picked.
function voiceExtensionForMime(mime) {
  if (mime.includes("mp4")) return ".mp4";
  if (mime.includes("ogg")) return ".ogg";
  return ".webm";
}

// fetchVoiceTranscript keeps voice input on the same optional API-key policy
// as /api/ask. FormData deliberately supplies no Content-Type header so the
// browser can add the multipart boundary itself. Returns {text, language}
// (language is whisper.cpp's own report of what it actually used — "auto"
// or a detected code when no language is configured server-side, otherwise
// the configured code).
async function fetchVoiceTranscript(blob) {
  const doFetch = () => {
    const form = new FormData();
    form.append("audio", blob, "r3-voice" + voiceExtensionForMime(blob.type));
    return fetch("/api/voice/transcribe", { method: "POST", headers: apiKeyHeaders(), body: form });
  };
  let res = await doFetch();
  if (res.status === 401) {
    const key = window.prompt(t("prompt.apiKeyRequired"));
    if (key && key.trim()) {
      localStorage.setItem("r3_api_key", key.trim());
      res = await doFetch();
    }
  }
  const payload = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(payload.error || res.statusText);
  return { text: payload.text || "", language: payload.language || "" };
}

// fetchAskStream POSTs to /api/ask, and on a 401 (missing/invalid API key)
// prompts the user for one, stores it, and retries once before giving up —
// so callers just get a normal response instead of handling auth inline.
async function fetchAskStream(body, signal) {
  const doFetch = () => fetch("/api/ask", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...apiKeyHeaders() },
    body: JSON.stringify(body),
    signal,
  });
  let res = await doFetch();
  if (res.status === 401) {
    const key = window.prompt(t("prompt.apiKeyRequired"));
    if (key && key.trim()) {
      localStorage.setItem("r3_api_key", key.trim());
      res = await doFetch();
    }
  }
  return res;
}

// readNDJSONStream reads a streaming NDJSON response (one JSON object per
// line — /api/ask, /api/draft/reply) and invokes onLine(msg) for each
// decoded line as it arrives. Pure transport plumbing only: Chat and Mail
// each keep their own msg.type switch/DOM updates rather than sharing those
// too (differing DOM ids/state make that riskier than it's worth) — only
// the byte-chunking/line-splitting/JSON.parse mechanics, which are
// identical everywhere, live here once.
async function readNDJSONStream(res, onLine) {
  if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let idx;
    while ((idx = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, idx);
      buf = buf.slice(idx + 1);
      if (!line.trim()) continue;
      onLine(JSON.parse(line));
    }
  }
}

// requestDraftStream POSTs to /api/draft/reply — NDJSON streaming since
// Mail was converted alongside Chat/Agent (see draftStreamMsg's doc comment,
// handlers.go) — and resolves with the finished draftReply once its "done"
// line arrives. onStep, if given, is called for every "step" line (live
// tool use, including the pre-flight tool router) so callers can render an
// agentStepsPanel while the draft is generated. Throws on a network
// failure or a "type":"error" line, same convention as api() elsewhere.
async function requestDraftStream(body, onStep, signal) {
  const res = await fetch("/api/draft/reply", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
    signal,
  });
  let draft = null;
  await readNDJSONStream(res, (msg) => {
    if (msg.type === "step") {
      if (onStep) onStep(msg.step || {});
    } else if (msg.type === "done") {
      draft = msg.draft || {};
    } else if (msg.type === "error") {
      throw new Error(msg.error);
    }
  });
  if (!draft) throw new Error(t("modal.noDraftReceived"));
  return draft;
}

// askQuestion is the one place that actually calls /api/ask — both the
// compose-box submit handler and the "regenerate" button (see below) fan
// into this, so both get identical history-tracking/streaming/error
// handling instead of two near-duplicate copies.
// ---- Image/document attachments (Chat/Agent composers) ------------------
// attachIsImage tells whether a data: URL (from FileReader.readAsDataURL)
// is an actual image, from its "data:<mime>;base64,..." prefix — a PDF/
// Office/text attachment (chatimages.go's decodeAskImages now accepts
// these too, text-extracted server-side rather than shown as a picture)
// has no sensible thumbnail, so attachThumbEl below falls back to a
// plain document-icon badge instead of a broken <img>.
function attachIsImage(dataURL) {
  return /^data:image\//.test(dataURL || "");
}

// attachThumbEl builds either a real <img> preview or a document-icon
// placeholder, same call shape either way — used by both the composer's
// attach-chip preview (wireImageAttach's render()) and a sent message's
// thumbnail row (addMsgImages) so neither has to branch on file type
// itself.
function attachThumbEl(filename, dataURL) {
  if (attachIsImage(dataURL)) {
    const img = document.createElement("img");
    img.src = dataURL;
    img.alt = filename;
    return img;
  }
  const badge = document.createElement("span");
  badge.className = "attach-doc-icon";
  badge.textContent = "📄";
  badge.setAttribute("aria-hidden", "true");
  return badge;
}

// One small widget factory (still parameterized by DOM id rather than
// hardcoded, from when Chat and the now-merged Agent tab each had their own
// composer): an attach button opens a hidden file input, each selected
// image or document renders as a removable thumbnail chip, and getImages()
// hands back the {filename, data_base64} shape askRequest.Images expects
// (chatimages.go) — despite the name, an entry may be an image OR a
// document; see chatimages.go's package comment for how the server tells
// them apart and routes each accordingly. Deliberately NOT persisted
// anywhere — askQuestion calls clear() right after reading getImages() for
// the outgoing request, so an attachment affects only that one turn,
// matching how it's never written into sessionMessages/localStorage
// history either. Client-side count/size checks mirror
// askImageMaxCount/askImageMaxBytes (chatimages.go) purely for fast
// feedback — the server enforces the real limits regardless.
function wireImageAttach({ btnId, inputId, listId }) {
  const btn = $(btnId), input = $(inputId), list = $(listId);
  const maxCount = 4;
  const maxBytes = 8 * 1024 * 1024;
  let items = []; // {file, dataURL}

  function render() {
    if (!list) return;
    list.hidden = items.length === 0;
    list.innerHTML = "";
    items.forEach((it, i) => {
      const chip = document.createElement("div");
      chip.className = "attach-chip";
      const img = attachThumbEl(it.file.name, it.dataURL);
      const name = document.createElement("span");
      name.className = "attach-chip-name";
      name.textContent = it.file.name;
      const rm = document.createElement("button");
      rm.type = "button";
      rm.className = "attach-chip-remove";
      rm.setAttribute("aria-label", t("chat.attach.removeImage"));
      rm.title = t("chat.attach.removeImage");
      rm.textContent = "×";
      rm.addEventListener("click", () => { items.splice(i, 1); render(); });
      chip.append(img, name, rm);
      list.appendChild(chip);
    });
  }

  function updateHint() {
    if (!btn) return;
    // uploadImageMode is a single, explicit admin policy (Einstellungen →
    // LLM-Backends & Routing) — it's the same for every question, not
    // dependent on which profile happens to be selected in the dropdown
    // next to it (a "vision" upload reroutes the whole request to the
    // configured Vision-Backend regardless, see handleAsk).
    btn.title = uploadImageMode === "vision"
      ? t("chat.attach.hintVision")
      : t("chat.attach.hintOcr");
  }

  btn?.addEventListener("click", () => input?.click());
  input?.addEventListener("change", () => {
    const room = maxCount - items.length;
    const picked = [...input.files].slice(0, Math.max(room, 0));
    if (input.files.length > picked.length) {
      alert(t("chat.attach.maxCountAlert", { max: maxCount }));
    }
    picked.forEach(file => {
      if (file.size > maxBytes) {
        alert(t("chat.attach.tooLargeAlert", { name: file.name, max: maxBytes / (1024 * 1024) }));
        return;
      }
      const reader = new FileReader();
      reader.onload = () => { items.push({ file, dataURL: reader.result }); render(); };
      reader.readAsDataURL(file);
    });
    input.value = "";
  });

  return {
    getImages: () => items.map(it => ({ filename: it.file.name, data_base64: (it.dataURL.split(",")[1] || "") })),
    // getThumbnails hands back the same attachments in the shape the sent
    // message bubble renders them in (addMsgImages) — the full data: URL
    // rather than getImages()'s bare base64, since that's what an <img src>
    // needs and it's already sitting in memory from the composer preview.
    getThumbnails: () => items.map(it => ({ filename: it.file.name, dataURL: it.dataURL })),
    clear: () => { items = []; render(); },
    updateHint,
  };
}

// addMsgImages renders a sent message's attached images as a small
// thumbnail row inside its .content — addMessage/addAgentMessage call this
// for "user" messages so an attached photo/scan is still visible after
// sending, not just in the composer beforehand (it was previously read for
// the request and then discarded, per wireImageAttach's ephemeral-by-design
// getImages()+clear() pair — this only adds a *visual* record in the DOM,
// still nothing persisted to sessionMessages/localStorage history).
function addMsgImages(contentEl, images) {
  if (!images || !images.length) return;
  const wrap = document.createElement("div");
  wrap.className = "msg-images";
  images.forEach((img) => {
    const a = document.createElement("a");
    a.href = img.dataURL;
    a.target = "_blank";
    a.rel = "noopener";
    a.title = img.filename;
    a.appendChild(attachThumbEl(img.filename, img.dataURL));
    wrap.appendChild(a);
  });
  contentEl.appendChild(wrap);
}
const chatAttach = wireImageAttach({ btnId: "#chatAttachBtn", inputId: "#chatAttachInput", listId: "#chatAttachList" });
// Called once /api/auth/status resolves and uploadImageMode becomes known.
function updateAttachHints() { chatAttach.updateHint(); }

// Local push-to-talk voice input. MediaRecorder produces a short audio
// blob (WebM/Opus on Chrome/Firefox, MP4/AAC on Safari — see
// VOICE_MIME_CANDIDATES); the server normalizes it with the configured
// FFmpeg binary before invoking the configured Whisper CLI. Nothing is
// retained in browser history or on the server beyond this request. A
// successful transcript is inserted into the question field for review —
// it deliberately does NOT auto-submit, so a Whisper mis-transcription
// (homophone, wrong language, background noise) gets the same glance-before-
// send treatment as anything typed, instead of going straight to the LLM.
//
// VOICE_MIME_CANDIDATES is tried in order via MediaRecorder.isTypeSupported
// — Safari (14.5+, incl. iOS) never supports the webm container at all, so
// trying only webm variants (as this used to) left Safari with no working
// fallback and a MediaRecorder constructor that throws synchronously.
const VOICE_MIME_CANDIDATES = [
  "audio/webm;codecs=opus",
  "audio/webm",
  "audio/mp4;codecs=mp4a.40.2",
  "audio/mp4",
  "audio/ogg;codecs=opus",
];
function pickVoiceMimeType() {
  return VOICE_MIME_CANDIDATES.find(m => window.MediaRecorder?.isTypeSupported?.(m)) || "";
}

// VOICE_MAX_RECORD_SECONDS auto-stops a forgotten-open recording — a client-
// side safety ceiling independent of (and comfortably under) the server's
// own default whisper/ffmpeg timeouts, so a long recording fails fast and
// locally instead of after a full round trip.
const VOICE_MAX_RECORD_SECONDS = 120;

(function wireVoiceInput() {
  const btn = $("#chatVoiceBtn");
  if (!btn) return;
  let recorder = null;
  let stream = null;
  let chunks = [];
  let mime = "";
  let cancelled = false;
  let tickHandle = null;
  let startedAt = 0;

  const setIdle = () => {
    btn.classList.remove("recording");
    btn.disabled = false;
    btn.textContent = "🎙️";
    btn.title = t("chat.voice.idle");
    btn.setAttribute("aria-label", t("chat.voice.idle"));
  };
  const setRecordingLabel = () => {
    const elapsed = Math.floor((Date.now() - startedAt) / 1000);
    const mm = String(Math.floor(elapsed / 60)).padStart(1, "0");
    const ss = String(elapsed % 60).padStart(2, "0");
    btn.textContent = `⏹️ ${mm}:${ss}`;
  };
  const stopTracks = () => {
    const localStream = stream;
    stream = null;
    localStream?.getTracks().forEach(track => track.stop());
  };
  const clearTick = () => {
    if (tickHandle) { clearInterval(tickHandle); tickHandle = null; }
  };

  const cancelRecording = () => {
    if (!recorder || recorder.state !== "recording") return;
    cancelled = true;
    recorder.stop(); // the 'stop' handler below sees `cancelled` and skips transcription
  };

  document.addEventListener("keydown", e => {
    if (e.key === "Escape" && recorder && recorder.state === "recording") cancelRecording();
  });
  // A SPA tab switch never navigates away, so an in-progress recording
  // would otherwise keep the microphone (and the browser's own recording
  // indicator) live indefinitely if the user clicks elsewhere and forgets.
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) cancelRecording();
  });

  btn.addEventListener("click", async () => {
    if (recorder && recorder.state === "recording") {
      recorder.stop();
      return;
    }
    if (!navigator.mediaDevices?.getUserMedia || !window.MediaRecorder) {
      alert(t("chat.voice.notSupported"));
      return;
    }
    mime = pickVoiceMimeType();
    if (!mime) {
      alert(t("chat.voice.notSupported"));
      return;
    }
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      recorder = new MediaRecorder(stream, { mimeType: mime });
      chunks = [];
      cancelled = false;
      recorder.addEventListener("dataavailable", e => { if (e.data.size) chunks.push(e.data); });
      recorder.addEventListener("stop", async () => {
        clearTick();
        stopTracks();
        const wasCancelled = cancelled;
        if (wasCancelled) {
          recorder = null;
          chunks = [];
          setIdle();
          $("#ariaStatus").textContent = t("chat.voice.ariaCancelled");
          return;
        }
        btn.disabled = true;
        btn.textContent = "…";
        $("#ariaStatus").textContent = t("chat.voice.ariaTranscribing");
        try {
          const { text, language } = await fetchVoiceTranscript(new Blob(chunks, { type: mime }));
          if (!text.trim()) throw new Error(t("chat.voice.noTranscript"));
          const ta = $("#question");
          ta.value = text.trim();
          ta.dispatchEvent(new Event("input", { bubbles: true }));
          ta.focus();
          // language is whisper.cpp's own report (configured code, or its
          // auto-detection result — see whisper.go's runWhisperCLI) —
          // surfaced here mainly for screen-reader users, who have no
          // visual way to notice a wrong auto-detected language before
          // the transcript is used.
          $("#ariaStatus").textContent = language
            ? t("chat.voice.ariaDoneLang", { lang: language })
            : t("chat.voice.ariaDone");
        } catch (err) {
          alert(t("chat.voice.failedPrefix") + err.message);
        } finally {
          recorder = null;
          chunks = [];
          setIdle();
        }
      }, { once: true });
      recorder.start();
      startedAt = Date.now();
      btn.classList.add("recording");
      btn.title = t("chat.voice.recording");
      btn.setAttribute("aria-label", t("chat.voice.recording"));
      $("#ariaStatus").textContent = t("chat.voice.ariaRecording");
      setRecordingLabel();
      tickHandle = setInterval(() => {
        setRecordingLabel();
        if ((Date.now() - startedAt) / 1000 >= VOICE_MAX_RECORD_SECONDS && recorder?.state === "recording") {
          recorder.stop();
        }
      }, 500);
    } catch (err) {
      stopTracks();
      recorder = null;
      setIdle();
      alert(t("chat.voice.micErrorPrefix") + err.message);
    }
  });
})();

async function askQuestion(q) {
  const submitBtn = $("#askSubmit");
  if (streamAbort) streamAbort.abort(); // defensive: askQuestion assumes no stream is already in flight

  addMessage("user", q, undefined, chatAttach.getThumbnails());
  recordUserMessage(q);
  // Snapshot history *before* pushing this question — the question itself
  // goes in the request's separate `question` field, not duplicated into
  // `history` too.
  const history = sessionMessages.slice(-SESSION_HISTORY_MAX);
  sessionMessages.push({ role: "user", content: q });

  const assistantDiv = addMessage("assistant", "");
  const contentEl = assistantDiv.querySelector(".content");
  // Live step timeline: previously Agent-tab-only ("Demo" mode), now shown
  // here too since Chat can just as well trigger a tool call (the model's
  // own single-round tool-calling, or the pre-flight tool router if
  // enabled) — an admin/customer question that happens to need e.g. a
  // stock lookup shouldn't leave that invisible just because it's the
  // plain Chat tab.
  const steps = agentStepsPanel();
  assistantDiv.querySelector(".msg-body").insertBefore(steps.el, contentEl);
  const typing = document.createElement("span");
  typing.className = "typing";
  typing.innerHTML = "<span></span><span></span><span></span>";
  typing.setAttribute("aria-hidden", "true"); // purely visual "..." animation; #ariaStatus below carries the actual announcement
  contentEl.appendChild(typing);
  const cursor = document.createElement("span");
  cursor.className = "stream-cursor";
  cursor.setAttribute("aria-hidden", "true");

  streamAbort = new AbortController();
  submitBtn.classList.add("streaming");
  submitBtn.title = t("chat.stop");
  $("#ariaStatus").textContent = t("chat.answerPending");

  const images = chatAttach.getImages();
  chatAttach.clear();
  let full = "";
  // renderInto re-parses the WHOLE accumulated markdown (headings, code
  // blocks, citation markers, …) on every call — cheap for one message, but
  // a fast local model can emit several "token" events per animation frame,
  // so calling it on every single one repeats that parse/DOM-rebuild work
  // far more often than the screen can even repaint. scheduleTokenRender
  // coalesces same-frame tokens into one renderInto call via
  // requestAnimationFrame, while contentEl.dataset.raw (a plain string
  // assignment, not a DOM rebuild) still updates on every token so anything
  // reading it between frames (the speak/copy/download buttons) never sees
  // stale text. renderFrame is cancelled before any of the FINAL renders
  // below (done/clarify/abort/error) so a still-pending scheduled frame
  // can never fire afterward and clobber a citation-resolved or
  // error-state render with a stale streaming one.
  let renderFrame = null;
  function scheduleTokenRender() {
    if (renderFrame !== null) return;
    renderFrame = requestAnimationFrame(() => {
      renderFrame = null;
      renderInto(contentEl, full, [], false);
      contentEl.appendChild(cursor);
      scrollChatToBottom(false);
    });
  }
  function cancelScheduledRender() {
    if (renderFrame !== null) { cancelAnimationFrame(renderFrame); renderFrame = null; }
  }
  try {
    const res = await fetchAskStream(
      { question: q, profile: $("#askProfile").value || undefined, preset: $("#askPreset").value || undefined, tier: ($("#askTier") && $("#askTier").value) || undefined, history, images },
      streamAbort.signal,
    );
    await readNDJSONStream(res, (msg) => {
      if (msg.type === "token") {
        full += msg.text;
        contentEl.dataset.raw = full;
        // Render without citations during streaming (inline cites appear as plain chips)
        scheduleTokenRender();
      } else if (msg.type === "done") {
        cancelScheduledRender();
        cursor.remove();
        // Re-render with the final citations to resolve [Q1] markers —
        // final=true so any marker still unresolved (unused by the
        // model, or hidden for its source_kind) is dropped from view
        // rather than left as a dangling placeholder chip.
        const cits = msg.citations || [];
        renderInto(contentEl, full, cits, true);
        contentEl.dataset.raw = full;
        if (cits.length) {
          addCitations(assistantDiv, cits);
          enrichInlineCites(contentEl, cits);
        }
        renderDebugPanel(assistantDiv.querySelector(".msg-body"), msg.debug);
        steps.finish();
        recordAssistantMessage(full, cits);
        sessionMessages.push({ role: "assistant", content: full });
        $("#ariaStatus").textContent = t("chat.newAnswerReceived");
      } else if (msg.type === "clarify") {
        // Agent tier's ask_clarifying_question tool (agent.go) — was
        // Agent-tab-only ("Demo" mode) before the two tabs merged; Chat's
        // own askQuestion handles it now that the Agent tier lives here.
        cancelScheduledRender();
        cursor.remove();
        renderClarification(assistantDiv, contentEl, msg.clarify || {});
        renderDebugPanel(assistantDiv.querySelector(".msg-body"), msg.debug);
        $("#ariaStatus").textContent = t("chat.newAnswerReceived");
      } else if (msg.type === "step") {
        steps.add(msg.step || {});
        scrollChatToBottom(false);
      } else if (msg.type === "error") {
        throw new Error(msg.error);
      }
    });
  } catch (err) {
    cancelScheduledRender();
    cursor.remove();
    if (err.name === "AbortError") {
      // User stopped the stream — keep the partial answer.
      renderInto(contentEl, full, [], false);
      contentEl.dataset.raw = full;
      const note = document.createElement("p");
      note.className = "hint";
      note.textContent = t("chat.stopped");
      contentEl.appendChild(note);
      $("#ariaStatus").textContent = t("chat.stopped.aria");
    } else {
      contentEl.dataset.raw = full + (full ? "\n\n" : "") + "Fehler: " + err.message;
      contentEl.textContent = contentEl.dataset.raw;
      contentEl.classList.add("error-text");
      $("#ariaStatus").textContent = t("chat.answerError");
    }
  } finally {
    typing.remove();
    streamAbort = null;
    submitBtn.classList.remove("streaming");
    submitBtn.title = t("chat.send");
    $("#question").focus();
  }
}

$("#askForm").addEventListener("submit", (e) => {
  e.preventDefault();
  // Streaming in progress → the submit button acts as "Stop" instead of
  // asking a new question (askQuestion itself also aborts defensively,
  // but this keeps that click from also submitting whatever's still in
  // the textarea).
  if (streamAbort) { streamAbort.abort(); return; }

  const q = $("#question").value.trim();
  if (!q) return;
  const ta = $("#question");
  ta.value = "";
  ta.style.height = "auto";
  askQuestion(q);
});

// ---- Agent tier: clarifying questions, live step timeline, demo buttons --
// The Agent tier (#askTier's "agent" option, see tier.go) used to be its own
// tab hitting /api/ask with mode:"agent" — now it's just one of Chat's
// tiers, so this is the leftover Agent-only rendering askQuestion needs:
// clarifying-question option buttons, the live tool/sub-agent timeline, and
// the admin-configurable demo buttons.

// renderClarification renders the agent's clarifying question
// (ask_clarifying_question, agent.go) as the current assistant message,
// plus one button per offered option (if any) — clicking an option submits
// its exact text as the next question, same as if the user had typed and
// sent it themselves.
function renderClarification(assistantDiv, contentEl, clarify) {
  const question = clarify.question || "";
  contentEl.innerHTML = renderMarkdown(question, []);
  contentEl.dataset.raw = question;
  recordAssistantMessage(question, []);
  sessionMessages.push({ role: "assistant", content: question });

  const options = clarify.options || [];
  if (!options.length) return;
  const wrap = document.createElement("div");
  wrap.className = "clarify-options";
  options.forEach((opt) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "clarify-option-btn";
    btn.textContent = opt;
    btn.addEventListener("click", () => {
      wrap.querySelectorAll("button").forEach((b) => { b.disabled = true; });
      askQuestion(opt);
    });
    wrap.appendChild(btn);
  });
  assistantDiv.querySelector(".msg-body").appendChild(wrap);
}

// iconFor/toolLabel map a tool name to its icon/German label — shared
// between agentStepsPanel's list view and the graphical view's node
// labels (buildStepGraphMermaid) below, so both ways of looking at the
// exact same step stream stay visually consistent.
function iconFor(tool) {
  if (tool === "search_knowledge_base") return "🔎";
  if (tool === "get_source_content") return "📄";
  if (tool === "list_sources") return "🗂";
  if (tool === "search_shop_items") return "🛒";
  if (tool && tool.indexOf("mssql") >= 0) return "🗄";
  if (tool === "fetch_url" || tool === "fetch_page_with_links") return "🌐";
  if (tool === "web_research") return "🕵";
  return "⚙";
}
function toolLabel(tool) {
  if (tool === "search_knowledge_base") return "Wissensbasis durchsuchen";
  if (tool === "get_source_content") return "Volltext einer Quelle laden";
  if (tool === "list_sources") return "Quellen auflisten";
  if (tool === "search_shop_items") return "Shop durchsuchen";
  if (tool === "fetch_url") return "Webseite abrufen";
  if (tool === "fetch_page_with_links") return "Web-Recherche: Seite abrufen";
  if (tool === "web_research") return "Web-Recherche";
  return tool || "Werkzeug";
}

// sanitizeMermaidLabel strips characters that would break a Mermaid
// flowchart node's quoted label syntax (quotes, brackets, newlines) — the
// label text itself is our own short icon+name+status summary, but it can
// embed a truncated tool/agent name that in principle came from a model's
// own tool-call arguments (a sub-agent's own Label), so this isn't purely
// decorative.
function sanitizeMermaidLabel(text) {
  return String(text).replace(/[\r\n]+/g, " ").replace(/["\[\]{}|]/g, "'").slice(0, 90);
}

// buildStepGraphMermaid turns the raw step stream (llm.go's agentStep —
// id/parent_id/agent/tool/type) into a Mermaid flowchart definition: the
// graphical counterpart to agentStepsPanel's flat list, showing at a
// glance which agent started which sub-agent, which tool it called, and
// whether each call is still running / succeeded / failed. Each node's
// running number (#1, #2, ...) matches the same number shown on that
// step's row in the list view (see agentStepsPanel's shared seqOf), so
// switching to the list view for full args/results is a matter of finding
// the same number rather than needing interactive click handling inside
// the rendered SVG.
function buildStepGraphMermaid(steps, seqOf) {
  const nodes = new Map(); // id -> merged step data
  const order = [];
  steps.forEach(s => {
    if (!s.id) return; // defensive: a step from a server predating this field
    if (!nodes.has(s.id)) {
      nodes.set(s.id, Object.assign({}, s));
      order.push(s.id);
    } else {
      const n = nodes.get(s.id);
      // Merge the matching *_end step's fields onto the *_start node
      // without blanking out start-only fields (Args) an end event simply
      // doesn't repeat.
      if (s.result) n.result = s.result;
      if (s.error) n.error = s.error;
      if (s.duration_ms) n.duration_ms = s.duration_ms;
      if (s.type === "tool_end" || s.type === "subagent_end") n.resolved = true;
      if (!n.parent_id && s.parent_id) n.parent_id = s.parent_id;
    }
  });

  const lines = ["flowchart TD", `root(["🧭 Anfrage"])`];
  order.forEach(id => {
    const n = nodes.get(id);
    const nodeId = "n" + id;
    const parent = n.parent_id ? "n" + n.parent_id : "root";
    const isSub = (n.type || "").indexOf("subagent") === 0;
    const icon = isSub ? "🤝" : iconFor(n.tool);
    const name = isSub ? (n.agent || "Unter-Agent") : toolLabel(n.tool);
    const status = !n.resolved ? "⏳" : n.error ? "✗" : "✓";
    const ms = n.duration_ms ? ` ${n.duration_ms}ms` : "";
    const label = sanitizeMermaidLabel(`#${seqOf(id)} ${icon} ${name} ${status}${ms}`);
    const cls = !n.resolved ? "pending" : n.error ? "err" : "ok";
    lines.push(`${nodeId}["${label}"]:::${cls}`);
    lines.push(`${parent} --> ${nodeId}`);
  });
  lines.push("classDef pending fill:#5a4a10,stroke:#d4a017,color:#fff,stroke-width:2px;");
  lines.push("classDef ok fill:#123a24,stroke:#2e8b57,color:#fff,stroke-width:1px;");
  lines.push("classDef err fill:#3a1212,stroke:#c0392b,color:#fff,stroke-width:1px;");
  return lines.join("\n");
}

// renderAgentGraph renders a Mermaid flowchart definition (built by
// buildStepGraphMermaid) into container — the graphical view's counterpart
// to renderMermaidBlock (used for AI-authored diagrams inside chat
// messages), but always fully replacing container's content rather than
// converting a <pre> code block, and meant to be called repeatedly as new
// steps arrive (agentStepsPanel debounces those calls).
let _agentGraphSeq = 0;
function renderAgentGraph(container, code) {
  loadMermaid().then(mermaid => {
    mermaid.initialize({ startOnLoad: false, securityLevel: "strict", theme: currentRenderTheme() === "light" ? "default" : "dark" });
    const id = "r3agraph" + (_agentGraphSeq++);
    return Promise.resolve(mermaid.render(id, code)).then(res => {
      const svg = typeof res === "string" ? res : (res && res.svg);
      if (!svg) throw new Error("empty render");
      container.innerHTML = svg;
    });
  }).catch(() => {
    container.innerHTML = `<p class="hint">${escapeHTML(t("chat.agentSteps.graphUnavailable"))}</p>`;
  });
}

// agentStepsPanel builds the live "what the agent is doing" timeline shown
// above an answer that used tools (any tier, not just Agent — see
// askQuestion's own comment on why). It starts hidden and reveals itself on
// the first step; tool calls show as
// pending rows that resolve to done/error, and delegated sub-agents get
// their own indented, labelled rows. Purely presentational — driven by the
// server's "step" NDJSON events (llm.go's agentStep). Offers two views over
// the same step data: the original flat/nested "Liste" (full args/results
// per row) and a new "Grafisch" Mermaid flowchart (buildStepGraphMermaid)
// showing the agent/sub-agent/tool-call hierarchy at a glance — matching
// #-numbers tie a graph node back to its list row for the full detail.
function agentStepsPanel() {
  const details = document.createElement("details");
  details.className = "agent-steps";
  details.open = true;
  details.hidden = true;
  const summary = document.createElement("summary");
  summary.textContent = t("chat.agentSteps.summary");
  details.appendChild(summary);

  const viewToggle = document.createElement("div");
  viewToggle.className = "agent-steps-view-toggle";
  const listBtn = document.createElement("button");
  listBtn.type = "button";
  listBtn.className = "ghost-btn active";
  listBtn.textContent = t("chat.agentSteps.viewList");
  const graphBtn = document.createElement("button");
  graphBtn.type = "button";
  graphBtn.className = "ghost-btn";
  graphBtn.textContent = t("chat.agentSteps.viewGraph");
  viewToggle.appendChild(listBtn);
  viewToggle.appendChild(graphBtn);
  details.appendChild(viewToggle);

  const list = document.createElement("ol");
  list.className = "agent-steps-list";
  details.appendChild(list);

  const graphHost = document.createElement("div");
  graphHost.className = "agent-steps-graph";
  graphHost.hidden = true;
  details.appendChild(graphHost);

  // seqOf assigns each step ID a stable, first-seen-order running number —
  // shared between the list rows (#N prefix) and the graph nodes (#N in
  // the node label), the one thing tying the two views together.
  const seqByID = {};
  let seqNext = 1;
  function seqOf(id) {
    if (!seqByID[id]) seqByID[id] = seqNext++;
    return seqByID[id];
  }

  const allSteps = []; // every step ever seen, for (re)building the graph
  let currentView = "list";
  let graphTimer = null;
  function renderGraphNow() {
    graphHost.innerHTML = "";
    renderAgentGraph(graphHost, buildStepGraphMermaid(allSteps, seqOf));
  }
  function scheduleGraphRender(immediate) {
    if (currentView !== "graph") return; // no point rendering a hidden view
    if (immediate) {
      if (graphTimer) { clearTimeout(graphTimer); graphTimer = null; }
      renderGraphNow();
      return;
    }
    if (graphTimer) return; // a render is already queued
    graphTimer = setTimeout(() => { graphTimer = null; renderGraphNow(); }, 400);
  }
  function showView(view) {
    currentView = view;
    list.hidden = view !== "list";
    graphHost.hidden = view !== "graph";
    listBtn.classList.toggle("active", view === "list");
    graphBtn.classList.toggle("active", view === "graph");
    if (view === "graph") scheduleGraphRender(true);
  }
  listBtn.addEventListener("click", () => showView("list"));
  graphBtn.addEventListener("click", () => showView("graph"));

  // resultBlock renders a collapsed-by-default "Ergebnis" detail under a
  // resolved (non-error) step — the server already sends step.result
  // (llm.go's tool_end/subagent_end, truncated), this is what actually
  // shows it instead of silently dropping it as before. Only for
  // successes: an error already has its own visible .step-err span.
  function resultBlock(text) {
    if (!text) return "";
    return `<details class="step-result"><summary>Ergebnis</summary><pre>${escapeHTML(text)}</pre></details>`;
  }

  // label wraps the shared toolLabel() with the router-phase prefix: a
  // Phase:"router" (tool_router.go) step is the pre-flight "brauche ich
  // ein Werkzeug?" decision, distinct from the main answer's own tool use
  // — same icon/mechanics, just called out as a pre-check so it doesn't
  // read as if the main answer redundantly used the tool twice.
  function label(step) {
    const l = toolLabel(step.tool);
    return step.phase === "router" ? `Vorprüfung: ${l}` : l;
  }

  // pending is keyed by step.id (llm.go's agentStep.ID) — a "start" and its
  // matching "end" always share one ID (see agentProgress.send's doc
  // comment), so this is an exact match with no collision risk, unlike the
  // old fallback of a "phase|agent|tool" composite key (which broke on two
  // concurrent identical calls, or a router-phase call racing a main-loop
  // call to the same tool). Falls back to the composite key only for a
  // step from a server predating this field (id missing) — keeps this
  // panel working against an older/mixed deployment during a rolling
  // restart instead of just breaking silently.
  const pending = {}; // key -> <li> awaiting its *_end
  function keyOf(step) { return step.id || ((step.phase || "") + "|" + (step.agent || "") + "|" + (step.tool || "")); }

  function add(step) {
    details.hidden = false;
    allSteps.push(step);
    scheduleGraphRender(false);
    const seqPrefix = step.id ? `<span class="step-seq">#${seqOf(step.id)}</span> ` : "";
    if (step.type === "subagent_start") {
      const li = document.createElement("li");
      li.className = "agent-step subagent pending";
      li.innerHTML = `${seqPrefix}<span class="step-ico">🤝</span> <strong>Unter-Agent:</strong> ${escapeHTML(step.agent || "")}` +
        (step.args ? ` <span class="hint">— ${escapeHTML(step.args)}</span>` : "");
      list.appendChild(li);
      pending[keyOf(step)] = li;
    } else if (step.type === "subagent_end") {
      const li = pending[keyOf(step)];
      if (li) {
        li.classList.remove("pending");
        li.classList.add(step.error ? "err" : "ok");
        const ms = step.duration_ms ? ` <span class="hint">(${step.duration_ms} ms)</span>` : "";
        li.innerHTML += `${ms}` + (step.error ? ` <span class="step-err">${escapeHTML(step.error)}</span>` : "") +
          (step.error ? "" : resultBlock(step.result));
        delete pending[keyOf(step)];
      }
    } else if (step.type === "tool_start") {
      const li = document.createElement("li");
      li.className = "agent-step pending" + (step.agent ? " nested" : "");
      li.innerHTML = `${seqPrefix}<span class="step-ico">${iconFor(step.tool)}</span> ${escapeHTML(label(step))}` +
        (step.args ? ` <span class="hint">${escapeHTML(step.args)}</span>` : "") +
        ` <span class="step-spin">…</span>`;
      list.appendChild(li);
      pending[keyOf(step)] = li;
    } else if (step.type === "tool_end") {
      const li = pending[keyOf(step)];
      const target = li || (() => { const n = document.createElement("li"); n.className = "agent-step" + (step.agent ? " nested" : ""); list.appendChild(n); return n; })();
      target.classList.remove("pending");
      target.classList.add(step.error ? "err" : "ok");
      const spin = target.querySelector(".step-spin");
      if (spin) spin.remove();
      const ms = step.duration_ms ? ` <span class="hint">(${step.duration_ms} ms)</span>` : "";
      if (!li) {
        target.innerHTML = `${seqPrefix}<span class="step-ico">${iconFor(step.tool)}</span> ${escapeHTML(label(step))}`;
      }
      target.innerHTML += ms + (step.error ? ` <span class="step-err">${escapeHTML(step.error)}</span>` : " ✓" + resultBlock(step.result));
      delete pending[keyOf(step)];
    }
  }

  function finish() {
    // Any still-pending row (rare: budget cut mid-call) loses its spinner.
    list.querySelectorAll(".agent-step.pending .step-spin").forEach((s) => s.remove());
    list.querySelectorAll(".agent-step.pending").forEach((li) => li.classList.remove("pending"));
    // Collapse the timeline once done so the answer stays the focus — but
    // only if it actually recorded steps; leave a single-step run expanded.
    if (list.children.length > 3) details.open = false;
    scheduleGraphRender(true); // final, unhurried render once the run settles
  }

  return { el: details, add, finish };
}

// ---- Chat: export ---------------------------------------------------
function exportChatAsMarkdown() {
  const msgs = $all("#chatLog .msg");
  if (!msgs.length) return;
  const lines = msgs.map(div => {
    const role = div.classList.contains("user") ? "Du" : "R3";
    const raw = div.querySelector(".content")?.dataset?.raw
      || div.querySelector(".content")?.textContent
      || "";
    const cits = Array.from(div.querySelectorAll(".citation"))
      .map(c => c.textContent.trim()).join(", ");
    return `**${role}:** ${raw}${cits ? `\n\n*Quellen: ${cits}*` : ""}`;
  });
  const md = `# R3 Gespräch\n\n${lines.join("\n\n---\n\n")}\n`;
  const blob = new Blob([md], { type: "text/markdown; charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `r3-gespraech-${new Date().toISOString().slice(0, 10)}.md`;
  a.click();
  URL.revokeObjectURL(url);
}
$("#exportChatComposer").addEventListener("click", exportChatAsMarkdown);

// ---- Busy helper for long-running form submits ------------------------
function setBusy(btn, busy) {
  btn.classList.toggle("busy", busy);
  btn.disabled = busy;
}

// ---- Import: dropzone -------------------------------------------------
(function () {
  const dz = $("#dropzone");
  const input = $("#fileInput");
  const label = $("#dzFiles");
  if (!dz) return;

  // showFiles updates the dropzone label to summarize the currently
  // selected files (truncating the name list once there are more than 4).
  function showFiles() {
    const files = Array.from(input.files || []);
    if (!files.length) { label.textContent = t("import.dropzone.noneSelected"); return; }
    const names = files.slice(0, 4).map(f => f.name).join(", ");
    label.textContent = files.length > 4
      ? t("import.dropzone.moreFiles", { names, count: files.length - 4 })
      : names;
  }

  dz.addEventListener("click", () => input.click());
  dz.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); input.click(); }
  });
  input.addEventListener("change", showFiles);

  ["dragenter", "dragover"].forEach(ev =>
    dz.addEventListener(ev, (e) => { e.preventDefault(); dz.classList.add("dragover"); }));
  ["dragleave", "drop"].forEach(ev =>
    dz.addEventListener(ev, (e) => { e.preventDefault(); dz.classList.remove("dragover"); }));
  dz.addEventListener("drop", (e) => {
    if (e.dataTransfer?.files?.length) {
      input.files = e.dataTransfer.files;
      showFiles();
    }
  });
})();

// ---- Import: files --------------------------------------------------
$("#uploadForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const files = $("#fileInput").files;
  const out = $("#uploadResult");
  const btn = e.target.querySelector("button[type=submit]");
  out.className = "result";
  if (!files.length) { out.textContent = t("import.upload.noFileSelected"); return; }
  const fd = new FormData();
  for (const f of files) fd.append("file", f);
  if ($("#keepOriginal").checked) fd.append("keep_original", "1");
  if (isDryRun()) fd.append("dry_run", "1");
  setBusy(btn, true);
  try {
    const res = await api("/api/upload", { method: "POST", body: fd });
    const prefix = res.some(r => r.dry_run) ? dryRunPrefix({ dry_run: true }) : "";
    out.textContent = prefix + res.map(r => r.error
      ? `✗ ${r.source_name}: ${r.error}`
      : `✓ ${r.source_name}: ${r.skipped ? t("import.common.unchangedSkipped") : r.chunks + " Chunks"}`
    ).join("\n");
    loadStats();
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
  finally { setBusy(btn, false); }
});

// ---- Import: PST ----------------------------------------------------
// Two-phase flow: uploading a .pst first stages it server-side and returns
// its folder list + message counts (previewStaging), so the user can pick
// which folders to actually import before anything is parsed/embedded.
// The second phase (pstImportSubmit) commits by staging_id — the file
// itself is never re-uploaded.
let pstPreviewAbort = null;
let pstStaging = null; // { id, file, folders: [{path, messages}, ...] }
// pstJobId/pstJobPollTimer track the running background import job (see
// handleImportPST, handlers.go) — replaces the old pstAbort
// AbortController, since the import no longer lives on this page's fetch
// at all (see pollPSTJob below). "r3_pst_job_id" in localStorage is what
// makes a page reload (or a browser that was closed and reopened) able to
// find and keep watching an already-running job instead of showing a
// blank Import tab.
let pstJobId = null;
let pstJobPollTimer = null;
const PST_JOB_STORAGE_KEY = "r3_pst_job_id";

// renderPstProgress writes a one-line status update into out during a
// PST import, showing the current folder/message alongside running totals.
function renderPstProgress(out, folder, subject, r, phase) {
  const isIngest = phase === "ingest";
  const action = isIngest ? t("import.pst.actionIngest") : t("import.pst.actionScan");
  const phaseHint = isIngest
    ? t("import.pst.phaseHintIngest")
    : t("import.pst.phaseHintScan");
  out.textContent = `${action}${folder ? " „" + folder + "“" : ""} … ` +
    t("import.pst.progressCounts", { messages: r.messages || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
    (subject ? " " + t("import.common.lastItem", { name: subject }) : "") +
    // Boilerplate counts are provisional this early (see pst.go) but showing
    // them live, updated on every progress tick, is the whole point — no
    // more waiting until a half-hour import finishes to notice something's
    // being over/under-filtered.
    renderBoilerplateSummary(r) + phaseHint;
  const phaseStatus = $("#pstPhaseStatus");
  phaseStatus.hidden = false;
  phaseStatus.dataset.phase = isIngest ? "ingest" : "scan";
  phaseStatus.textContent = isIngest
    ? t("import.pst.phaseStatusIngest")
    : t("import.pst.phaseStatusScan");
}

// renderBoilerplateSummary renders the boilerplate-detection tail of a PST
// import result (see boilerplate.go/pst.go): always a one-line count of
// how many recurring paragraphs were stripped, plus — only when the
// "Debug" checkbox was on for this run, so boilerplate_samples came back —
// each one's occurrence count and a text sample, so an operator can check
// nothing but signatures/disclaimers actually got filtered. Returns ""
// when nothing was detected, so callers can append it unconditionally.
function renderBoilerplateSummary(final) {
  const n = final.boilerplate_blocks || 0;
  if (!n) return "";
  if (!final.boilerplate_samples || !final.boilerplate_samples.length) {
    return "\n\n" + t(n === 1 ? "import.pst.boilerplateSingular" : "import.pst.boilerplatePlural", { n });
  }
  const lines = final.boilerplate_samples.map((s, i) =>
    t("import.pst.boilerplateSampleLine", { index: i + 1, count: s.count, chars: s.chars, sample: s.sample }));
  return "\n\n" + t(n === 1 ? "import.pst.boilerplateSingularWithSamples" : "import.pst.boilerplatePluralWithSamples", { n }) + "\n" + lines.join("\n");
}

// renderPstFolderPicker renders the checkbox list of PST folders found
// during staging, letting the user pick which ones to actually import.
function renderPstFolderPicker() {
  const list = $("#pstFolderList");
  list.innerHTML = "";
  for (const f of pstStaging.folders) {
    const li = document.createElement("li");
    li.dataset.path = f.path;
    li.innerHTML = `
      <label>
        <input type="checkbox" checked data-path="${escapeHTML(f.path)}">
        <span class="pst-folder-path" title="${escapeHTML(f.path)}">${escapeHTML(f.path)}</span>
      </label>
      <span class="pst-folder-count">${f.messages}</span>`;
    list.appendChild(li);
  }
  $("#pstFolderPicker").hidden = false;
  $("#pstFolderFilter").value = "";
  updatePstSelectedSummary();
}

// updatePstSelectedSummary updates the "N of M folders selected" line for
// the PST folder picker, including the total message count selected.
function updatePstSelectedSummary() {
  const boxes = $all("#pstFolderList input[type=checkbox]");
  const checked = boxes.filter(b => b.checked);
  const messages = pstStaging.folders
    .filter(f => checked.some(b => b.dataset.path === f.path))
    .reduce((sum, f) => sum + f.messages, 0);
  const query = $("#pstFolderFilter").value.trim().toLocaleLowerCase();
  let visible = 0;
  $all("#pstFolderList li").forEach(li => {
    const matches = !query || li.dataset.path.toLocaleLowerCase().includes(query);
    li.hidden = !matches;
    if (matches) visible++;
  });
  const visibleNote = query ? t("import.pst.visibleNote", { visible }) : "";
  $("#pstSelectedSummary").textContent =
    t("import.pst.folderSelectedSummary", { checked: checked.length, total: boxes.length, messages, visibleNote });
}

$("#pstFolderList").addEventListener("change", (e) => {
  if (e.target.matches("input[type=checkbox]")) updatePstSelectedSummary();
});
$("#pstFolderFilter").addEventListener("input", updatePstSelectedSummary);
$("#pstSelectAll").addEventListener("click", () => {
  $all("#pstFolderList li:not([hidden]) input[type=checkbox]").forEach(box => { box.checked = true; });
  updatePstSelectedSummary();
});
$("#pstSelectNone").addEventListener("click", () => {
  $all("#pstFolderList li:not([hidden]) input[type=checkbox]").forEach(box => { box.checked = false; });
  updatePstSelectedSummary();
});

$("#pstPreviewForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const files = $("#pstInput").files;
  const out = $("#pstPreviewResult");
  const btn = $("#pstPreviewSubmit");
  const cancelBtn = $("#pstPreviewCancel");
  out.className = "result";
  $("#pstFolderPicker").hidden = true;
  $("#pstPhaseStatus").hidden = true;
  $("#pstResult").textContent = "";
  pstStaging = null;
  if (!files.length) { out.textContent = t("import.pst.noFileSelected"); return; }

  const fd = new FormData();
  fd.append("file", files[0]);
  out.textContent = t("import.pst.loadingPreview");
  setBusy(btn, true);
  cancelBtn.hidden = false;
  pstPreviewAbort = new AbortController();

  try {
    const res = await fetch("/api/import/pst/preview", { method: "POST", body: fd, signal: pstPreviewAbort.signal });
    const text = await res.text();
    if (!res.ok) throw new Error(text || res.statusText);
    const preview = JSON.parse(text);
    applyPstPreviewResponse(preview, out);
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.previewAborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    cancelBtn.hidden = true;
    pstPreviewAbort = null;
  }
});

$("#pstPreviewCancel").addEventListener("click", () => {
  if (pstPreviewAbort) pstPreviewAbort.abort();
});

// applyPstPreviewResponse renders a /api/import/pst/preview(-path) JSON
// response into out and, if it found any non-empty folders, the folder
// picker — shared by both the upload form above and the server-local-path
// form below, since the two endpoints return an identical shape.
function applyPstPreviewResponse(preview, out) {
  pstStaging = { id: preview.staging_id, file: preview.file, folders: preview.folders || [] };
  const walkedNote = (preview.folders_walked && preview.folders_walked > pstStaging.folders.length)
    ? t("import.pst.walkedNote", { total: preview.folders_walked, empty: preview.folders_walked - pstStaging.folders.length })
    : "";
  if (!pstStaging.folders.length) {
    out.textContent = t("import.pst.noFoldersFound", { file: preview.file, walkedNote });
    return;
  }
  out.textContent = t("import.pst.previewSummary", { file: preview.file, folders: pstStaging.folders.length, total: preview.total, walkedNote });
  renderPstFolderPicker();
}

// Server-local-path variant of the PST preview form above — skips the
// browser upload entirely for mailbox exports too large or the network
// too slow to push through reliably; the operator places the file on the
// server's own filesystem first (network share, scp, ...) and gives R3
// that path instead of a <input type=file> selection.
$("#pstPreviewPathForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const path = $("#pstPathInput").value.trim();
  const out = $("#pstPreviewResult");
  const btn = $("#pstPreviewPathSubmit");
  out.className = "result";
  $("#pstFolderPicker").hidden = true;
  $("#pstPhaseStatus").hidden = true;
  $("#pstResult").textContent = "";
  pstStaging = null;
  if (!path) { out.textContent = t("import.pst.noPathGiven"); return; }

  out.textContent = t("import.pst.loadingPreview");
  setBusy(btn, true);
  try {
    const preview = await api("/api/import/pst/preview-path", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path }),
    });
    applyPstPreviewResponse(preview, out);
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

// pollPSTJob polls /api/import/pst/status for pstJobId every 1.5s,
// rendering exactly the way the old NDJSON stream reader used to
// (renderPstProgress for each tick, the same final-summary block once
// done) — the only difference is where the data comes from. Safe to call
// to *resume* watching a job too (see resumePSTJobIfAny below): the first
// tick runs immediately, and only schedules the repeating timer if the
// job isn't already done.
async function pollPSTJob() {
  const out = $("#pstResult");
  const btn = $("#pstImportSubmit");
  const cancelBtn = $("#pstCancel");
  if (pstJobPollTimer) { clearInterval(pstJobPollTimer); pstJobPollTimer = null; }
  setBusy(btn, true);
  cancelBtn.hidden = false;

  const stopWatching = () => {
    if (pstJobPollTimer) { clearInterval(pstJobPollTimer); pstJobPollTimer = null; }
    localStorage.removeItem(PST_JOB_STORAGE_KEY);
    pstJobId = null;
    setBusy(btn, false);
    cancelBtn.hidden = true;
    $("#pstPhaseStatus").hidden = true;
  };

  const tick = async () => {
    let status;
    try {
      status = await api(`/api/import/pst/status?job_id=${encodeURIComponent(pstJobId)}`);
    } catch (err) {
      stopWatching();
      out.className = "result error";
      out.textContent = t("import.pst.statusUnavailable");
      return;
    }
    if (!status.done) {
      renderPstProgress(out, status.folder, status.subject, status.result, status.phase);
      return;
    }
    stopWatching();
    const final = status.result;
    out.textContent = dryRunPrefix(final) + t("import.pst.finalSummary", { file: final.file, folders: final.folders, messages: final.messages, chunks: final.chunks, skipped: final.skipped }) +
      renderBoilerplateSummary(final) +
      (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "") +
      (final.attachment_warnings && final.attachment_warnings.length ? t("import.common.attachmentsSkippedHeading") + final.attachment_warnings.join("\n") : "");
    if (!final.dry_run) loadStats();
  };

  await tick();
  if (pstJobId) pstJobPollTimer = setInterval(tick, 1500);
}

$("#pstImportSubmit").addEventListener("click", async () => {
  const out = $("#pstResult");
  const btn = $("#pstImportSubmit");
  out.className = "result";
  if (!pstStaging) return;
  const folders = $all("#pstFolderList input[type=checkbox]").filter(b => b.checked).map(b => b.dataset.path);
  if (!folders.length) { out.textContent = t("import.pst.noFoldersSelected"); return; }

  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);
  try {
    const res = await api("/api/import/pst", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ staging_id: pstStaging.id, folders, dry_run: isDryRun(), debug: $("#pstDebugToggle").checked }),
    });
    pstJobId = res.job_id;
    localStorage.setItem(PST_JOB_STORAGE_KEY, pstJobId);
    // Staging is single-use server-side (takeStagedPST) — reset the picker
    // now; the import itself runs independently of this page from here on.
    pstStaging = null;
    $("#pstFolderPicker").hidden = true;
    $("#pstInput").value = "";
    pollPSTJob();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
    setBusy(btn, false);
  }
});

$("#pstCancel").addEventListener("click", async () => {
  if (!pstJobId) return;
  try {
    await api("/api/import/pst/cancel", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ job_id: pstJobId }),
    });
  } catch { /* best-effort — the next poll tick reflects whatever actually happened */ }
});

// resumePSTJobIfAny re-attaches to an in-progress PST import job after a
// page reload/reopen — the job itself already kept running server-side
// regardless (see handleImportPST's doc comment), this only restores the
// Import tab's view of it instead of looking blank as if nothing were
// happening. Two paths, in order:
//  1. This browser's own localStorage job_id (fast, no request needed).
//  2. Otherwise, ask the server for every known job (GET
//     /api/import/pst/jobs) and attach to the most recent still-running
//     one — this is what makes a *different* browser/machine/incognito
//     tab (no localStorage entry of its own) able to discover an import
//     someone else started, exactly as handleImportPSTJobs' doc comment
//     promises. A finished job is left alone here: whoever was actually
//     watching it already saw the summary, and surfacing an old result to
//     an unrelated tab that never asked for it would be more confusing
//     than helpful.
async function resumePSTJobIfAny() {
  const savedJobId = localStorage.getItem(PST_JOB_STORAGE_KEY);
  if (savedJobId) {
    pstJobId = savedJobId;
    pollPSTJob();
    return;
  }
  try {
    const jobs = await api("/api/import/pst/jobs");
    const running = (jobs || []).find(j => !j.done);
    if (!running) return;
    pstJobId = running.job_id;
    localStorage.setItem(PST_JOB_STORAGE_KEY, pstJobId);
    pollPSTJob();
  } catch { /* best-effort discovery — a blank Import tab is no worse than before this existed */ }
}
resumePSTJobIfAny();

// ---- Import: SharePoint ----------------------------------------------
// Same preview-then-select-then-stream shape as the PST import above,
// just backed by /api/import/sharepoint/preview (one folder level via
// Microsoft Graph, see sharepoint.go) instead of an uploaded file.
let spAbort = null;
let spStaging = null; // { folder, items: [{name, path, size, is_folder}] }

// currentSpConnection reads the picked SharePoint connection name (empty
// string when only one/zero are configured — requireConn's server-side
// fallback then applies, same as before this selector existed). Shared by
// every SharePoint action in the card (files, Site Pages, share-link
// import below), since they all reach the same Azure AD app + site grant.
function currentSpConnection() {
  return $("#spConnectionSelect")?.value || "";
}

// loadImportConnectionSelectors populates the Import tab's per-connector
// "which configured connection" dropdowns — called once when the Import
// tab is activated (TAB_LOADERS below), since (unlike Settings) nothing
// else on this tab already fetches /api/settings. Hidden entirely when
// 0/1 connections exist for a given type: nothing to choose, same
// reasoning as requireConn's server-side "name required" only once there
// actually is more than one.
async function loadImportConnectionSelectors() {
  try {
    const s = await api("/api/settings");
    const populate = (kind, selectId, wrapId) => {
      const conns = s[kind] || [];
      const sel = $(selectId);
      const wrap = $(wrapId);
      if (!sel || !wrap) return;
      sel.innerHTML = "";
      conns.forEach(c => {
        const opt = document.createElement("option");
        opt.value = c.name || "";
        opt.textContent = c.name || "(unbenannt)";
        sel.appendChild(opt);
      });
      wrap.hidden = conns.length <= 1;
    };
    populate("sharepoint", "#spConnectionSelect", "#spConnectionWrap");
    populate("onedrive", "#onedriveConnectionSelect", "#onedriveConnectionWrap");
    populate("github", "#githubConnectionSelect", "#githubConnectionWrap");
    populate("sap_s4", "#sapS4ConnectionSelect", "#sapS4ConnectionWrap");
  } catch { /* Import tab still works with the single/only connection either way */ }
}

// runConfiguredDeltaSync is the compact common UI for sources which own
// their own cursor (OneDrive/GitHub/SAP). Unlike preview-first importers the
// source API itself decides what changed; the endpoint streams one final
// result object and the Scheduler dashboard offers the same operation later
// as an unattended job.
async function runConfiguredDeltaSync({ buttonId, resultId, endpoint, selectId, label, summarize }) {
  const btn = $(buttonId);
  const out = $(resultId);
  if (!btn || !out) return;
  out.className = "result";
  out.textContent = isDryRun() ? t("import.deltaSync.startingDryRun") : t("import.deltaSync.syncing");
  setBusy(btn, true);
  try {
    const res = await fetch(endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ connection: $(selectId)?.value || "", dry_run: isDryRun() }),
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, idx); buffer = buffer.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") out.textContent = t("import.deltaSync.progress", { label });
        if (msg.type === "done") final = msg.result || {};
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + summarize(final) + (final.errors?.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
}

$("#onedriveSyncSubmit")?.addEventListener("click", () => runConfiguredDeltaSync({
  buttonId: "#onedriveSyncSubmit", resultId: "#onedriveSyncResult", endpoint: "/api/import/onedrive/sync", selectId: "#onedriveConnectionSelect", label: "OneDrive",
  summarize: r => t("import.onedrive.syncSummary", { files: r.files || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }),
}));
$("#githubSyncSubmit")?.addEventListener("click", () => runConfiguredDeltaSync({
  buttonId: "#githubSyncSubmit", resultId: "#githubSyncResult", endpoint: "/api/import/github/sync", selectId: "#githubConnectionSelect", label: "GitHub",
  summarize: r => t("import.github.syncSummary", { issues: r.issues || 0, pullRequests: r.pull_requests || 0, readmes: r.readmes || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }),
}));
$("#sapS4SyncSubmit")?.addEventListener("click", () => runConfiguredDeltaSync({
  buttonId: "#sapS4SyncSubmit", resultId: "#sapS4SyncResult", endpoint: "/api/import/sap-s4/sync", selectId: "#sapS4ConnectionSelect", label: "SAP S/4",
  summarize: r => t("import.sapS4.syncSummary", { records: r.records || 0, deleted: r.deleted || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }),
}));

// fmtBytes formats a byte count as a human-readable B/KB/MB string.
function fmtBytes(n) {
  if (n === null || n === undefined) return "";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

// renderSpFilePicker renders the SharePoint file/folder listing for the
// current staging path, letting the user check files to import or
// navigate into a subfolder (folders themselves aren't selectable).
function renderSpFilePicker() {
  const list = $("#spFileList");
  list.innerHTML = "";
  for (const item of spStaging.items) {
    const li = document.createElement("li");
    if (item.is_folder) {
      li.innerHTML = `
        <label>
          <input type="checkbox" disabled>
          <button type="button" class="link-btn sp-folder-nav" data-path="${escapeHTML(item.path)}">📁 ${escapeHTML(item.name)}</button>
        </label>
        <span class="pst-folder-count">${t("import.sharepoint.folderLabel")}</span>`;
    } else {
      li.innerHTML = `
        <label>
          <input type="checkbox" checked data-path="${escapeHTML(item.path)}">
          <span class="pst-folder-path" title="${escapeHTML(item.path)}">${escapeHTML(item.name)}</span>
        </label>
        <span class="pst-folder-count">${fmtBytes(item.size)}</span>`;
    }
    list.appendChild(li);
  }
  $("#spFilePicker").hidden = false;
  updateSpSelectedSummary();
  $all(".sp-folder-nav").forEach(btn => {
    btn.addEventListener("click", () => {
      $("#spFolderPath").value = btn.dataset.path;
      $("#spPreviewForm").requestSubmit();
    });
  });
}

// updateSpSelectedSummary updates the "N of M files selected" line for
// the SharePoint file picker.
function updateSpSelectedSummary() {
  const boxes = $all("#spFileList input[type=checkbox]:not(:disabled)");
  const checked = boxes.filter(b => b.checked);
  $("#spSelectedSummary").textContent = t("import.sharepoint.filesSelectedSummary", { checked: checked.length, total: boxes.length });
}

wireSelectionControls({
  list: "#spFileList", selectAll: "#spSelectAll", selectNone: "#spSelectNone",
  update: updateSpSelectedSummary, checkbox: "input[type=checkbox]:not(:disabled)",
});

$("#spPreviewForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const folder = $("#spFolderPath").value.trim();
  const out = $("#spPreviewResult");
  const btn = $("#spPreviewSubmit");
  out.className = "result";
  $("#spFilePicker").hidden = true;
  $("#spResult").textContent = "";
  spStaging = null;

  out.textContent = t("import.sharepoint.loadingFolder");
  setBusy(btn, true);
  try {
    const res = await api("/api/import/sharepoint/preview", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ folder, connection: currentSpConnection() }),
    });
    spStaging = { folder: res.folder, items: res.items || [] };
    if (!spStaging.items.length) {
      out.textContent = t("import.sharepoint.noItemsFound");
      return;
    }
    const fileCount = spStaging.items.filter(i => !i.is_folder).length;
    out.textContent = t("import.sharepoint.previewSummary", { fileCount, subfolders: spStaging.items.length - fileCount });
    renderSpFilePicker();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

$("#spImportSubmit").addEventListener("click", async () => {
  const out = $("#spResult");
  const btn = $("#spImportSubmit");
  const cancelBtn = $("#spCancel");
  out.className = "result";
  if (!spStaging) return;
  const files = $all("#spFileList input[type=checkbox]:not(:disabled)").filter(b => b.checked).map(b => b.dataset.path);
  if (!files.length) { out.textContent = t("import.sharepoint.noFilesSelected"); return; }

  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);
  cancelBtn.hidden = false;
  spAbort = new AbortController();

  try {
    const res = await fetch("/api/import/sharepoint", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ folder: spStaging.folder, files, dry_run: isDryRun(), connection: currentSpConnection() }),
      signal: spAbort.signal,
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.sharepoint.importProgress", { files: r.files || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.file_name ? " " + t("import.common.lastItem", { name: msg.file_name }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.sharepoint.finalSummary", { files: final.files, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.aborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    cancelBtn.hidden = true;
    spAbort = null;
  }
});

// spDeltaSyncAbort mirrors spAbort above but for the Delta-Sync button —
// separate so cancelling one doesn't touch the other, though only one is
// realistically ever running at a time from the same card.
let spDeltaSyncAbort = null;

$("#spDeltaSyncSubmit").addEventListener("click", async () => {
  const out = $("#spDeltaSyncResult");
  const btn = $("#spDeltaSyncSubmit");
  out.className = "result";
  out.textContent = t("import.sharepoint.searchingChanges");
  setBusy(btn, true);
  spDeltaSyncAbort = new AbortController();

  try {
    const res = await fetch("/api/import/sharepoint/delta-sync", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ dry_run: isDryRun(), connection: currentSpConnection() }),
      signal: spDeltaSyncAbort.signal,
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.sharepoint.syncProgress", { files: r.files || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.file_name ? " " + t("import.common.lastItem", { name: msg.file_name }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.sharepoint.syncFinalSummary", { files: final.files, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.aborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    spDeltaSyncAbort = null;
  }
});

$("#spCancel").addEventListener("click", () => {
  if (spAbort) spAbort.abort();
});

// ---- Import: SharePoint Site Pages -------------------------------------
// Same preview-then-select-then-stream shape as the file import above,
// backed by /api/import/sharepoint/pages/preview|pages (sharepoint.go's
// spListPages/importSharePointPages) instead of the document-library
// drive API — a Site Page has no size/folder concept, so the picker is
// simpler (flat list, title as the visible label).
let spPagesAbort = null;
let spPagesStaging = null; // { pages: [{id, name, title}] }

function renderSpPagesPicker() {
  const list = $("#spPagesList");
  list.innerHTML = "";
  for (const p of spPagesStaging.pages) {
    const li = document.createElement("li");
    li.innerHTML = `
      <label>
        <input type="checkbox" checked data-id="${escapeHTML(p.id)}">
        <span class="pst-folder-path" title="${escapeHTML(p.name)}">${escapeHTML(p.title || p.name)}</span>
      </label>
      <span class="pst-folder-count">${escapeHTML(p.name)}</span>`;
    list.appendChild(li);
  }
  $("#spPagesPicker").hidden = false;
  updateSpPagesSelectedSummary();
}

function updateSpPagesSelectedSummary() {
  const boxes = $all("#spPagesList input[type=checkbox]");
  const checked = boxes.filter(b => b.checked);
  $("#spPagesSelectedSummary").textContent = t("import.sharepointPages.selectedSummary", { checked: checked.length, total: boxes.length });
}

wireSelectionControls({
  list: "#spPagesList", selectAll: "#spPagesSelectAll", selectNone: "#spPagesSelectNone",
  update: updateSpPagesSelectedSummary, checkbox: "input[type=checkbox]",
});

$("#spPagesPreviewSubmit").addEventListener("click", async () => {
  const out = $("#spPagesPreviewResult");
  const btn = $("#spPagesPreviewSubmit");
  out.className = "result";
  $("#spPagesPicker").hidden = true;
  $("#spPagesResult").textContent = "";
  spPagesStaging = null;

  out.textContent = t("import.sharepointPages.loadingList");
  setBusy(btn, true);
  try {
    const res = await api("/api/import/sharepoint/pages/preview", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ connection: currentSpConnection() }),
    });
    spPagesStaging = { pages: res.pages || [] };
    if (!spPagesStaging.pages.length) {
      out.textContent = t("import.sharepointPages.noPagesFound");
      return;
    }
    out.textContent = t("import.sharepointPages.previewSummary", { count: spPagesStaging.pages.length });
    renderSpPagesPicker();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

$("#spPagesImportSubmit").addEventListener("click", async () => {
  const out = $("#spPagesResult");
  const btn = $("#spPagesImportSubmit");
  const cancelBtn = $("#spPagesCancel");
  out.className = "result";
  if (!spPagesStaging) return;
  const pages = $all("#spPagesList input[type=checkbox]").filter(b => b.checked).map(b => b.dataset.id);
  if (!pages.length) { out.textContent = t("import.sharepointPages.noneSelected"); return; }

  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);
  cancelBtn.hidden = false;
  spPagesAbort = new AbortController();

  try {
    const res = await fetch("/api/import/sharepoint/pages", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ pages, dry_run: isDryRun(), connection: currentSpConnection() }),
      signal: spPagesAbort.signal,
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.sharepointPages.importProgress", { pages: r.pages || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.name ? " " + t("import.common.lastItem", { name: msg.name }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.sharepointPages.finalSummary", { pages: final.pages, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.aborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    cancelBtn.hidden = true;
    spPagesAbort = null;
  }
});

$("#spPagesCancel").addEventListener("click", () => {
  if (spPagesAbort) spPagesAbort.abort();
});

// ---- Import: SharePoint share links -------------------------------------
// No preview/select step (unlike the two pickers above) — each pasted
// link is independently resolved+ingested server-side
// (sharepoint.go's importSharePointShareLinks), one line each.
let spShareLinkAbort = null;

$("#spShareLinkImportSubmit").addEventListener("click", async () => {
  const out = $("#spShareLinkResult");
  const btn = $("#spShareLinkImportSubmit");
  const urls = $("#spShareLinkInput").value.split("\n").map(l => l.trim()).filter(Boolean);
  out.className = "result";
  if (!urls.length) { out.textContent = t("import.sharepointLinks.noneEntered"); return; }

  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);
  spShareLinkAbort = new AbortController();

  try {
    const res = await fetch("/api/import/sharepoint/sharelink", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ urls, dry_run: isDryRun(), connection: currentSpConnection() }),
      signal: spShareLinkAbort.signal,
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.sharepointLinks.importProgress", { links: r.links || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.name ? " " + t("import.common.lastItem", { name: msg.name }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.sharepointLinks.finalSummary", { links: final.links, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) { loadStats(); $("#spShareLinkInput").value = ""; }
    }
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.aborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    spShareLinkAbort = null;
  }
});

// ---- Import: Outlook/Exchange (Graph) ---------------------------------
// Same preview-then-select-then-stream shape again, this time listing
// messages in the configured shared mailbox (see graphmail.go) instead of
// SharePoint files.
let exAbort = null;
let exStaging = null; // { items: [{id, subject, from, received}] }

// renderExMessagePicker renders the checkbox list of Exchange messages
// found during staging, letting the user pick which ones to import.
function renderExMessagePicker() {
  const list = $("#exMessageList");
  list.innerHTML = "";
  for (const item of exStaging.items) {
    const li = document.createElement("li");
    const when = item.received ? new Date(item.received).toLocaleString() : "";
    li.innerHTML = `
      <label>
        <input type="checkbox" checked data-id="${escapeHTML(item.id)}">
        <span class="pst-folder-path" title="${escapeHTML(item.subject)}">${escapeHTML(item.subject || t("import.exchange.noSubject"))} — ${escapeHTML(item.from)}</span>
      </label>
      <span class="pst-folder-count">${escapeHTML(when)}</span>`;
    list.appendChild(li);
  }
  $("#exMessagePicker").hidden = false;
  updateExSelectedSummary();
}

// updateExSelectedSummary updates the "N of M messages selected" line for
// the Exchange message picker.
function updateExSelectedSummary() {
  const boxes = $all("#exMessageList input[type=checkbox]");
  const checked = boxes.filter(b => b.checked);
  $("#exSelectedSummary").textContent = t("import.exchange.selectedSummary", { checked: checked.length, total: boxes.length });
}

wireSelectionControls({
  list: "#exMessageList", selectAll: "#exSelectAll", selectNone: "#exSelectNone",
  update: updateExSelectedSummary,
});

$("#exPreviewForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#exPreviewResult");
  const btn = $("#exPreviewSubmit");
  out.className = "result";
  $("#exMessagePicker").hidden = true;
  $("#exResult").textContent = "";
  exStaging = null;

  out.textContent = t("import.exchange.loadingMessages");
  setBusy(btn, true);
  try {
    const res = await api("/api/import/exchange/preview", { method: "POST" });
    exStaging = { items: res.items || [] };
    if (!exStaging.items.length) {
      out.textContent = t("import.exchange.noMessagesFound");
      return;
    }
    out.textContent = t("import.exchange.previewSummary", { count: exStaging.items.length, mailbox: res.mailbox, folder: res.folder });
    renderExMessagePicker();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

$("#exImportSubmit").addEventListener("click", async () => {
  const out = $("#exResult");
  const btn = $("#exImportSubmit");
  const cancelBtn = $("#exCancel");
  out.className = "result";
  if (!exStaging) return;
  const messageIds = $all("#exMessageList input[type=checkbox]").filter(b => b.checked).map(b => b.dataset.id);
  if (!messageIds.length) { out.textContent = t("import.exchange.noneSelected"); return; }

  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);
  cancelBtn.hidden = false;
  exAbort = new AbortController();

  try {
    const res = await fetch("/api/import/exchange", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ message_ids: messageIds, dry_run: isDryRun() }),
      signal: exAbort.signal,
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.exchange.importProgress", { messages: r.messages || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.subject ? " " + t("import.common.lastItem", { name: msg.subject }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.exchange.finalSummary", { messages: final.messages, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "") +
        (final.attachment_warnings && final.attachment_warnings.length ? t("import.common.attachmentsSkippedHeading") + final.attachment_warnings.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.aborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    cancelBtn.hidden = true;
    exAbort = null;
  }
});

$("#exCancel").addEventListener("click", () => {
  if (exAbort) exAbort.abort();
});

// ---- Import: IMAP (on-prem Exchange / generic mailboxes) --------------
// No preview/select step (see imapmail.go) — every click just fetches
// everything newer than the server-side LastUID, so this is a single
// streamed POST rather than the SharePoint/Exchange two-step flow.
$("#imapImportSubmit").addEventListener("click", async () => {
  const out = $("#imapResult");
  const btn = $("#imapImportSubmit");
  out.className = "result";
  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);

  try {
    const res = await fetch("/api/import/imap", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ dry_run: isDryRun() }),
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.imap.importProgress", { messages: r.messages || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.subject ? " " + t("import.common.lastItem", { name: msg.subject }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.imap.finalSummary", { messages: final.messages, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "") +
        (final.attachment_warnings && final.attachment_warnings.length ? t("import.common.attachmentsSkippedHeading") + final.attachment_warnings.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

// ---- Import: Microsoft Teams channel messages -------------------------
// Same preview-then-select-then-stream shape as Outlook/Exchange, this
// time listing top-level posts in the configured team/channel (see
// teams.go) instead of mailbox messages.
let teamsAbort = null;
let teamsStaging = null; // { items: [{id, subject, from, preview, created}] }

// renderTeamsMessagePicker renders the checkbox list of Teams messages
// found during staging, letting the user pick which ones to import.
function renderTeamsMessagePicker() {
  const list = $("#teamsMessageList");
  list.innerHTML = "";
  for (const item of teamsStaging.items) {
    const li = document.createElement("li");
    const when = item.created ? new Date(item.created).toLocaleString() : "";
    const label = item.subject || item.preview || t("import.teams.noText");
    li.innerHTML = `
      <label>
        <input type="checkbox" checked data-id="${escapeHTML(item.id)}">
        <span class="pst-folder-path" title="${escapeHTML(item.preview)}">${escapeHTML(label)} — ${escapeHTML(item.from)}</span>
      </label>
      <span class="pst-folder-count">${escapeHTML(when)}</span>`;
    list.appendChild(li);
  }
  $("#teamsMessagePicker").hidden = false;
  updateTeamsSelectedSummary();
}

// updateTeamsSelectedSummary updates the "N of M posts selected" line for
// the Teams message picker.
function updateTeamsSelectedSummary() {
  const boxes = $all("#teamsMessageList input[type=checkbox]");
  const checked = boxes.filter(b => b.checked);
  $("#teamsSelectedSummary").textContent = t("import.teams.selectedSummary", { checked: checked.length, total: boxes.length });
}

wireSelectionControls({
  list: "#teamsMessageList", selectAll: "#teamsSelectAll", selectNone: "#teamsSelectNone",
  update: updateTeamsSelectedSummary,
});

$("#teamsPreviewForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#teamsPreviewResult");
  const btn = $("#teamsPreviewSubmit");
  out.className = "result";
  $("#teamsMessagePicker").hidden = true;
  $("#teamsResult").textContent = "";
  teamsStaging = null;

  out.textContent = t("import.teams.loadingPosts");
  setBusy(btn, true);
  try {
    const res = await api("/api/import/teams/preview", { method: "POST" });
    teamsStaging = { items: res.items || [] };
    if (!teamsStaging.items.length) {
      out.textContent = t("import.teams.noPostsFound");
      return;
    }
    out.textContent = t("import.teams.previewSummary", { count: teamsStaging.items.length });
    renderTeamsMessagePicker();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

$("#teamsImportSubmit").addEventListener("click", async () => {
  const out = $("#teamsResult");
  const btn = $("#teamsImportSubmit");
  const cancelBtn = $("#teamsCancel");
  out.className = "result";
  if (!teamsStaging) return;
  const messageIds = $all("#teamsMessageList input[type=checkbox]").filter(b => b.checked).map(b => b.dataset.id);
  if (!messageIds.length) { out.textContent = t("import.teams.noneSelected"); return; }

  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);
  cancelBtn.hidden = false;
  teamsAbort = new AbortController();

  try {
    const res = await fetch("/api/import/teams", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ message_ids: messageIds, dry_run: isDryRun() }),
      signal: teamsAbort.signal,
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.teams.importProgress", { messages: r.messages || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.subject ? " " + t("import.common.lastItem", { name: msg.subject }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.teams.finalSummary", { messages: final.messages, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.aborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    cancelBtn.hidden = true;
    teamsAbort = null;
  }
});

$("#teamsCancel").addEventListener("click", () => {
  if (teamsAbort) teamsAbort.abort();
});

// ---- Import: Confluence pages ------------------------------------------
// Same preview-then-select-then-stream shape as Outlook/Exchange/Teams,
// this time listing pages in the configured space (see confluence.go).
let confAbort = null;
let confStaging = null; // { items: [{id, title, updated}] }

// renderConfPagePicker renders the checkbox list of Confluence pages
// found during staging, letting the user pick which ones to import.
function renderConfPagePicker() {
  const list = $("#confPageList");
  list.innerHTML = "";
  for (const item of confStaging.items) {
    const li = document.createElement("li");
    const when = item.updated ? new Date(item.updated).toLocaleString() : "";
    li.innerHTML = `
      <label>
        <input type="checkbox" checked data-id="${escapeHTML(item.id)}">
        <span class="pst-folder-path" title="${escapeHTML(item.title)}">${escapeHTML(item.title || t("import.confluence.noTitle"))}</span>
      </label>
      <span class="pst-folder-count">${escapeHTML(when)}</span>`;
    list.appendChild(li);
  }
  $("#confPagePicker").hidden = false;
  updateConfSelectedSummary();
}

// updateConfSelectedSummary updates the "N of M pages selected" line for
// the Confluence page picker.
function updateConfSelectedSummary() {
  const boxes = $all("#confPageList input[type=checkbox]");
  const checked = boxes.filter(b => b.checked);
  $("#confSelectedSummary").textContent = t("import.confluence.selectedSummary", { checked: checked.length, total: boxes.length });
}

wireSelectionControls({
  list: "#confPageList", selectAll: "#confSelectAll", selectNone: "#confSelectNone",
  update: updateConfSelectedSummary,
});

$("#confPreviewForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#confPreviewResult");
  const btn = $("#confPreviewSubmit");
  out.className = "result";
  $("#confPagePicker").hidden = true;
  $("#confResult").textContent = "";
  confStaging = null;

  out.textContent = t("import.confluence.loadingPages");
  setBusy(btn, true);
  try {
    const res = await api("/api/import/confluence/preview", { method: "POST" });
    confStaging = { items: res.items || [] };
    if (!confStaging.items.length) {
      out.textContent = t("import.confluence.noPagesFound");
      return;
    }
    out.textContent = t("import.confluence.previewSummary", { count: confStaging.items.length, spaceKey: res.space_key });
    renderConfPagePicker();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

$("#confImportSubmit").addEventListener("click", async () => {
  const out = $("#confResult");
  const btn = $("#confImportSubmit");
  const cancelBtn = $("#confCancel");
  out.className = "result";
  if (!confStaging) return;
  const pageIds = $all("#confPageList input[type=checkbox]").filter(b => b.checked).map(b => b.dataset.id);
  if (!pageIds.length) { out.textContent = t("import.confluence.noneSelected"); return; }

  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);
  cancelBtn.hidden = false;
  confAbort = new AbortController();

  try {
    const res = await fetch("/api/import/confluence", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ page_ids: pageIds, dry_run: isDryRun() }),
      signal: confAbort.signal,
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.confluence.importProgress", { pages: r.pages || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.title ? " " + t("import.common.lastItem", { name: msg.title }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.confluence.finalSummary", { pages: final.pages, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.aborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    cancelBtn.hidden = true;
    confAbort = null;
  }
});

$("#confCancel").addEventListener("click", () => {
  if (confAbort) confAbort.abort();
});

// ---- Import: Jira issues ------------------------------------------------
// Same preview-then-select-then-stream shape as Confluence, this time
// listing issues in the configured project (see jira.go).
let jiraAbort = null;
let jiraStaging = null; // { items: [{key, summary, status, updated}] }

// renderJiraIssuePicker renders the checkbox list of Jira issues found
// during staging, letting the user pick which ones to import.
function renderJiraIssuePicker() {
  const list = $("#jiraIssueList");
  list.innerHTML = "";
  for (const item of jiraStaging.items) {
    const li = document.createElement("li");
    const when = item.updated ? new Date(item.updated).toLocaleString() : "";
    const label = `${item.key}: ${item.summary || t("import.confluence.noTitle")}`;
    li.innerHTML = `
      <label>
        <input type="checkbox" checked data-id="${escapeHTML(item.key)}">
        <span class="pst-folder-path" title="${escapeHTML(label)}">${escapeHTML(label)}</span>
      </label>
      <span class="pst-folder-count">${escapeHTML(item.status || "")} ${escapeHTML(when)}</span>`;
    list.appendChild(li);
  }
  $("#jiraIssuePicker").hidden = false;
  updateJiraSelectedSummary();
}

// updateJiraSelectedSummary updates the "N of M issues selected" line for
// the Jira issue picker.
function updateJiraSelectedSummary() {
  const boxes = $all("#jiraIssueList input[type=checkbox]");
  const checked = boxes.filter(b => b.checked);
  $("#jiraSelectedSummary").textContent = t("import.jira.selectedSummary", { checked: checked.length, total: boxes.length });
}

wireSelectionControls({
  list: "#jiraIssueList", selectAll: "#jiraSelectAll", selectNone: "#jiraSelectNone",
  update: updateJiraSelectedSummary,
});

$("#jiraPreviewForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#jiraPreviewResult");
  const btn = $("#jiraPreviewSubmit");
  out.className = "result";
  $("#jiraIssuePicker").hidden = true;
  $("#jiraResult").textContent = "";
  jiraStaging = null;

  out.textContent = t("import.jira.loadingIssues");
  setBusy(btn, true);
  try {
    const res = await api("/api/import/jira/preview", { method: "POST" });
    jiraStaging = { items: res.items || [] };
    if (!jiraStaging.items.length) {
      out.textContent = t("import.jira.noIssuesFound");
      return;
    }
    out.textContent = t("import.jira.previewSummary", { count: jiraStaging.items.length, projectKey: res.project_key });
    renderJiraIssuePicker();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

$("#jiraImportSubmit").addEventListener("click", async () => {
  const out = $("#jiraResult");
  const btn = $("#jiraImportSubmit");
  const cancelBtn = $("#jiraCancel");
  out.className = "result";
  if (!jiraStaging) return;
  const issueKeys = $all("#jiraIssueList input[type=checkbox]").filter(b => b.checked).map(b => b.dataset.id);
  if (!issueKeys.length) { out.textContent = t("import.jira.noneSelected"); return; }

  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);
  cancelBtn.hidden = false;
  jiraAbort = new AbortController();

  try {
    const res = await fetch("/api/import/jira", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ issue_keys: issueKeys, dry_run: isDryRun() }),
      signal: jiraAbort.signal,
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.jira.importProgress", { issues: r.issues || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.key ? " " + t("import.common.lastItem", { name: msg.key }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.jira.finalSummary", { issues: final.issues, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.aborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    cancelBtn.hidden = true;
    jiraAbort = null;
  }
});

$("#jiraCancel").addEventListener("click", () => {
  if (jiraAbort) jiraAbort.abort();
});

// ---- Import: Freshservice tickets ---------------------------------------
// Same preview-then-select-then-stream shape as Jira, this time listing
// tickets from the configured Freshservice instance (see freshservice.go).
// Ticket ids are numbers, not strings — data-id round-trips through the
// DOM as a string either way, parsed back to int before sending.
let fsAbort = null;
let fsStaging = null; // { items: [{id, subject, status, updated}] }

// renderFsTicketPicker renders the checkbox list of Freshservice tickets
// found during staging, letting the user pick which ones to import.
function renderFsTicketPicker() {
  const list = $("#fsTicketList");
  list.innerHTML = "";
  for (const item of fsStaging.items) {
    const li = document.createElement("li");
    const when = item.updated ? new Date(item.updated).toLocaleString() : "";
    const label = `#${item.id}: ${item.subject || t("import.confluence.noTitle")}`;
    li.innerHTML = `
      <label>
        <input type="checkbox" checked data-id="${escapeHTML(String(item.id))}">
        <span class="pst-folder-path" title="${escapeHTML(label)}">${escapeHTML(label)}</span>
      </label>
      <span class="pst-folder-count">${escapeHTML(item.status || "")} ${escapeHTML(when)}</span>`;
    list.appendChild(li);
  }
  $("#fsTicketPicker").hidden = false;
  updateFsSelectedSummary();
}

// updateFsSelectedSummary updates the "N of M tickets selected" line for
// the Freshservice ticket picker.
function updateFsSelectedSummary() {
  const boxes = $all("#fsTicketList input[type=checkbox]");
  const checked = boxes.filter(b => b.checked);
  $("#fsSelectedSummary").textContent = t("import.freshservice.selectedSummary", { checked: checked.length, total: boxes.length });
}

wireSelectionControls({
  list: "#fsTicketList", selectAll: "#fsSelectAll", selectNone: "#fsSelectNone",
  update: updateFsSelectedSummary,
});

$("#fsPreviewForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#fsPreviewResult");
  const btn = $("#fsPreviewSubmit");
  out.className = "result";
  $("#fsTicketPicker").hidden = true;
  $("#fsResult").textContent = "";
  fsStaging = null;

  out.textContent = t("import.freshservice.loadingTickets");
  setBusy(btn, true);
  try {
    const res = await api("/api/import/freshservice/preview", { method: "POST" });
    fsStaging = { items: res.items || [] };
    if (!fsStaging.items.length) {
      out.textContent = t("import.freshservice.noTicketsFound");
      return;
    }
    out.textContent = t("import.freshservice.previewSummary", { count: fsStaging.items.length });
    renderFsTicketPicker();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

$("#fsImportSubmit").addEventListener("click", async () => {
  const out = $("#fsResult");
  const btn = $("#fsImportSubmit");
  const cancelBtn = $("#fsCancel");
  out.className = "result";
  if (!fsStaging) return;
  const ticketIDs = $all("#fsTicketList input[type=checkbox]").filter(b => b.checked).map(b => parseInt(b.dataset.id, 10));
  if (!ticketIDs.length) { out.textContent = t("import.freshservice.noneSelected"); return; }

  out.textContent = t("import.common.startingImport");
  setBusy(btn, true);
  cancelBtn.hidden = false;
  fsAbort = new AbortController();

  try {
    const res = await fetch("/api/import/freshservice", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ticket_ids: ticketIDs, dry_run: isDryRun() }),
      signal: fsAbort.signal,
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.freshservice.importProgress", { tickets: r.tickets || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.id ? " " + t("import.common.lastItem", { name: "#" + msg.id }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.freshservice.finalSummary", { tickets: final.tickets, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    if (err.name === "AbortError") {
      out.textContent = t("import.aborted");
    } else {
      out.className = "result error";
      out.textContent = err.message;
    }
  } finally {
    setBusy(btn, false);
    cancelBtn.hidden = true;
    fsAbort = null;
  }
});

$("#fsCancel").addEventListener("click", () => {
  if (fsAbort) fsAbort.abort();
});

// ---- Import: generic website URLs --------------------------------------
// No preview/select step (see webimport.go) — the admin already chose
// exactly which URLs to import by typing/pasting them, one per line.
// runWebImport is shared by the "Import starten" (respects the global
// dry-run toggle) and the per-connector "Probelauf (testen)" button
// (forceDry=true — always a no-write probe, so Web is testable without
// touching the knowledge base, the closest equivalent to the other
// connectors' "Verbindung testen").
async function runWebImport(forceDry) {
  const out = $("#webImportResult");
  const btn = forceDry ? $("#webImportTest") : $("#webImportSubmit");
  out.className = "result";

  const urls = $("#webImportUrls").value.split("\n").map(s => s.trim()).filter(Boolean);
  if (!urls.length) { out.textContent = t("import.web.noUrlsEntered"); return; }

  const dry = forceDry || isDryRun();
  const crawl = $("#webImportCrawl").checked;
  out.textContent = dry ? t("import.deltaSync.startingDryRun") : (crawl ? t("import.web.startingCrawl") : t("import.common.startingImport"));
  setBusy(btn, true);

  try {
    const res = await fetch("/api/import/web", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        urls, dry_run: dry,
        ...(crawl ? {
          crawl: true,
          max_depth: intOrDefault($("#webImportMaxDepth").value, 0),
          max_pages: intOrDefault($("#webImportMaxPages").value, 0),
          allow_other_hosts: $("#webImportOtherHosts").checked,
        } : {}),
      }),
    });
    if (!res.ok || !res.body) throw new Error((await res.text()) || res.statusText);

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let final = null;
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        const msg = JSON.parse(line);
        if (msg.type === "progress") {
          const r = msg.result || {};
          out.textContent = t("import.web.importProgress", { pages: r.pages || 0, chunks: r.chunks || 0, skipped: r.skipped || 0 }) +
            (msg.url ? t("import.common.lastItem", { name: msg.url }) : "");
        } else if (msg.type === "done") {
          final = msg.result;
        }
      }
    }
    if (final) {
      out.textContent = dryRunPrefix(final) + t("import.web.finalSummary", { pages: final.pages, chunks: final.chunks, skipped: final.skipped }) +
        (final.errors && final.errors.length ? t("import.common.errorsHeading") + final.errors.join("\n") : "");
      if (!final.dry_run) loadStats();
    }
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
}
$("#webImportForm").addEventListener("submit", (e) => { e.preventDefault(); runWebImport(false); });
$("#webImportTest")?.addEventListener("click", () => runWebImport(true));

// runRSSImport: see runWebImport's doc comment — same shared-with-test pattern.
async function runRSSImport(forceDry) {
  const out = $("#rssImportResult");
  const btn = forceDry ? $("#rssImportTest") : null;
  const url = $("#rssImportUrl").value.trim();
  if (!url) { out.textContent = t("import.rss.noUrlEntered"); return; }
  const dry = forceDry || isDryRun();
  out.className = "result";
  out.textContent = dry ? t("import.rss.dryRunProgress") : t("import.rss.importing");
  if (btn) setBusy(btn, true);
  try {
    const res = await fetch("/api/import/rss", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ url, dry_run: dry }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || t("import.rss.importFailed"));
    out.textContent = dryRunPrefix(data) + t("import.rss.finalSummary", { pages: data.pages || 0, chunks: data.chunks || 0, skipped: data.skipped || 0 }) + (data.errors?.length ? t("import.common.errorsHeading") + data.errors.join("\n") : "");
    if (!data.dry_run) loadStats();
  } catch (err) { out.className = "result error"; out.textContent = err.message; } finally { if (btn) setBusy(btn, false); }
}
$("#rssImportForm").addEventListener("submit", (e) => { e.preventDefault(); runRSSImport(false); });
$("#rssImportTest")?.addEventListener("click", () => runRSSImport(true));

// ---- Import: server folder ------------------------------------------
$("#folderForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const path = $("#folderPath").value.trim();
  const out = $("#folderResult");
  const btn = e.target.querySelector("button[type=submit]");
  out.className = "result";
  if (!path) return;
  setBusy(btn, true);
  try {
    const res = await api("/api/import/folder", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path, dry_run: isDryRun() }),
    });
    const isDry = res.some(r => r.dry_run);
    out.textContent = (isDry ? dryRunPrefix({ dry_run: true }) : "") + res.map(r => r.error
      ? `✗ ${r.source_name}: ${r.error}`
      : `✓ ${r.source_name}: ${r.skipped ? t("import.common.unchangedSkipped") : r.chunks + " Chunks"}`
    ).join("\n");
    if (!isDry) loadStats();
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
  finally { setBusy(btn, false); }
});

// ---- Import: R3 self-source ("yo dawg, I heard you like RAG…") --------
$("#selfSourceForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#selfSourceResult");
  const btn = e.target.querySelector("button[type=submit]");
  out.className = "result";
  setBusy(btn, true);
  try {
    const res = await api("/api/import/self-source", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ dry_run: isDryRun() }),
    });
    const isDry = res.some(r => r.dry_run);
    out.textContent = (isDry ? dryRunPrefix({ dry_run: true }) : "") + res.map(r => r.error
      ? `✗ ${r.source_name}: ${r.error}`
      : `✓ ${r.source_name}: ${r.skipped ? t("import.common.unchangedSkipped") : r.chunks + " Chunks"}`
    ).join("\n");
    if (!isDry) loadStats();
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
  finally { setBusy(btn, false); }
});

// ---- Sources --------------------------------------------------------
function fmtTime(unix) {
  if (!unix) return "–";
  return new Date(unix * 1000).toLocaleString();
}

// pstFileFromSourceID extracts the originating filename from a
// "pst:<file>:<folder>:<message-id>" source_id, used to group one PST
// import's messages together so it can be deleted as a block without
// touching sources from any other PST import.
function pstFileFromSourceID(sourceID) {
  const m = sourceID.match(/^pst:([^:]+):/);
  return m ? m[1] : null;
}

// renderImportBatches renders the per-kind/per-import summary cards above
// the sources table (e.g. total chunks per PST file), grouping the flat
// sources list by source_kind and, for PST, by originating file.
function renderImportBatches(sources) {
  const container = $("#importBatches");
  container.innerHTML = "";
  if (!sources.length) return;

  const kinds = new Map();
  const pstFiles = new Map();
  for (const s of sources) {
    const k = kinds.get(s.source_kind) || { count: 0, chunks: 0 };
    k.count++; k.chunks += s.chunks;
    kinds.set(s.source_kind, k);
    if (s.source_kind === "pst_email") {
      const file = pstFileFromSourceID(s.source_id);
      if (file) {
        const f = pstFiles.get(file) || { count: 0, chunks: 0 };
        f.count++; f.chunks += s.chunks;
        pstFiles.set(file, f);
      }
    }
  }

  for (const [kind, stats] of kinds) {
    const row = document.createElement("div");
    row.className = "import-batch-row";
    row.innerHTML = `
      <span class="batch-label">${escapeHTML(t("sources.batch.kindLabel", { kind }))}</span>
      <span class="batch-meta">${escapeHTML(t("sources.batch.countMeta", { count: stats.count, chunks: stats.chunks }))}</span>
      <button class="small-btn" data-kind="${escapeHTML(kind)}">${escapeHTML(t("sources.batch.deleteAllButton"))}</button>`;
    container.appendChild(row);
  }
  for (const [file, stats] of pstFiles) {
    const row = document.createElement("div");
    row.className = "import-batch-row";
    row.innerHTML = `
      <span class="batch-label">${escapeHTML(t("sources.batch.pstImportLabel", { file }))}</span>
      <span class="batch-meta">${escapeHTML(t("sources.batch.pstCountMeta", { count: stats.count, chunks: stats.chunks }))}</span>
      <button class="small-btn" data-prefix="pst:${escapeHTML(file)}:">${escapeHTML(t("sources.batch.deleteThisImportButton"))}</button>`;
    container.appendChild(row);
  }

  $all("#importBatches button[data-kind]").forEach(btn => {
    btn.addEventListener("click", async () => {
      const kind = btn.dataset.kind;
      if (!confirm(t("confirm.deleteByKind", { kind }))) return;
      try {
        await api("/api/sources/delete-by-kind", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ source_kind: kind }),
        });
        loadSources();
        loadStats();
      } catch (err) {
        alert(err.message);
      }
    });
  });
  $all("#importBatches button[data-prefix]").forEach(btn => {
    btn.addEventListener("click", async () => {
      const prefix = btn.dataset.prefix;
      if (!confirm(t("confirm.deleteByPrefix", { prefix }))) return;
      try {
        await api("/api/sources/delete-by-prefix", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ prefix }),
        });
        loadSources();
        loadStats();
      } catch (err) {
        alert(err.message);
      }
    });
  });
}

// REFRESHABLE_SOURCE_KINDS mirrors sourcerefresh.go's refreshSource
// switch — kept in sync manually since the two can't share a definition
// across the Go/JS boundary; a kind added there without being added here
// just means the row's "Neu laden" button doesn't yet appear for it, not
// a broken request.
const REFRESHABLE_SOURCE_KINDS = new Set(["sharepoint_file", "sharepoint_page", "sharepoint_link"]);

// loadSources fetches and renders the full sources table (plus the
// import-batch summary cards above it) on the Quellen page.
async function loadSources() {
  const tbody = $("#sourcesTable tbody");
  tbody.innerHTML = `<tr><td colspan=6>${escapeHTML(t("modal.loading"))}</td></tr>`;
  try {
    const sources = await api("/api/sources");
    tbody.innerHTML = "";
    renderImportBatches(sources);
    if (!sources.length) tbody.innerHTML = `<tr><td colspan=6>${escapeHTML(t("sources.empty"))}</td></tr>`;
    for (const s of sources) {
      const tr = document.createElement("tr");
      // data-source-* lets applySourceFilter() (below) filter already-
      // rendered rows without needing a separate cached copy of `sources`.
      tr.dataset.sourceId = s.source_id;
      tr.dataset.sourceKind = s.source_kind;
      tr.dataset.sourceName = s.source_name;
      // Source name: external link if a URL mapping matches, otherwise a
      // button that opens the same content popup the chat citations use.
      const nameCell = s.source_url
        ? `<a href="${escapeHTML(s.source_url)}" target="_blank" rel="noopener" title="${escapeHTML(s.source_id)}">${escapeHTML(s.source_name)}</a>`
        : `<button type="button" class="link-btn" data-open-source="${escapeHTML(s.source_id)}" data-open-name="${escapeHTML(s.source_name)}" data-open-kind="${escapeHTML(s.source_kind)}" title="${escapeHTML(s.source_id)}">${escapeHTML(s.source_name)}</button>`;
      // "Neu laden" only for kinds sourcerefresh.go's refreshSource actually
      // knows how to re-fetch (currently the three SharePoint kinds) —
      // showing it for every row would just mean a click-and-fail for e.g.
      // a pst_email, which has no server-kept original to re-download.
      const refreshBtn = REFRESHABLE_SOURCE_KINDS.has(s.source_kind)
        ? `<button class="small-btn" data-refresh-id="${escapeHTML(s.source_id)}" title="${escapeHTML(t("sources.refresh.title"))}">${escapeHTML(t("sources.refresh"))}</button>`
        : "";
	  const aclBtn = `<button class="small-btn" data-acl-id="${escapeHTML(s.source_id)}" data-acl-name="${escapeHTML(s.source_name)}" title="Dokumentzugriff für diese einzelne Quelle festlegen">Zugriff</button>`;
      tr.innerHTML = `
        <td class="name">${nameCell}</td>
        <td>${escapeHTML(s.source_kind)}</td>
        <td>${fmtTime(s.loaded_at)}</td>
        <td>${fmtTime(s.doc_date)}</td>
        <td><button type="button" class="link-btn" data-view-chunks="${escapeHTML(s.source_id)}" title="${escapeHTML(t("sources.viewChunksTitle"))}">${s.chunks}</button></td>
        <td>${refreshBtn}${aclBtn}<button class="small-btn" data-id="${escapeHTML(s.source_id)}">${escapeHTML(t("common.delete"))}</button></td>`;
      tbody.appendChild(tr);
    }
    updateSourceFilterOptions(sources);
    applySourceFilter();
    $all("#sourcesTable button[data-open-source]").forEach(btn => {
      btn.addEventListener("click", () => openSourcePopup(btn.dataset.openSource, btn.dataset.openName, btn.dataset.openKind));
    });
    // Deep-link into the Chunks tab, pre-filtered to this exact source —
    // same navigateToChunksForSource helper the Jobs tab's worst-sources
    // table uses (app.js, near activateTab).
    $all("#sourcesTable button[data-view-chunks]").forEach(btn => {
      btn.addEventListener("click", () => navigateToChunksForSource(btn.dataset.viewChunks));
    });
    $all("#sourcesTable button[data-refresh-id]").forEach(btn => {
      btn.addEventListener("click", async () => {
        const original = btn.textContent;
        btn.disabled = true;
        btn.textContent = t("sources.refresh.loading");
        try {
          const res = await api("/api/sources/refresh", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ source_id: btn.dataset.refreshId }),
          });
          btn.textContent = res.skipped ? t("sources.refresh.unchanged") : t("sources.refresh.updated");
          if (!res.skipped) loadStats();
          setTimeout(() => { btn.textContent = original; btn.disabled = false; }, 2000);
        } catch (err) {
          alert(err.message);
          btn.textContent = original;
          btn.disabled = false;
        }
      });
    });
    $all("#sourcesTable button[data-acl-id]").forEach(btn => {
      btn.addEventListener("click", () => editSourceACL(btn.dataset.aclId, btn.dataset.aclName));
    });
    $all("#sourcesTable button.small-btn[data-id]").forEach(btn => {
      btn.addEventListener("click", async () => {
        if (!confirm(t("confirm.deleteSource"))) return;
        try {
          await api("/api/sources/delete", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ source_id: btn.dataset.id }),
          });
          loadSources();
          loadStats();
        } catch (err) {
          alert(err.message);
        }
      });
    });
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan=6>${escapeHTML(t("common.tableError", { message: err.message }))}</td></tr>`;
  }
}
$("#refreshSources").addEventListener("click", loadSources);

async function editSourceACL(sourceID, sourceName) {
  try {
    const current = await api(`/api/sources/acl?source_id=${encodeURIComponent(sourceID)}`);
    const acl = current.acl || {};
    const result = await NovaPop.popup({
      title: `Zugriff: ${sourceName}`,
      html: `<p>Diese Regel kann den Zugriff auf genau diese Quelle weiter einschränken. Leer lassen bedeutet: die bestehende Regel für den Quelltyp gilt unverändert.</p>
        <label class="np-field">Abteilungen (eine pro Zeile oder mit Komma)<textarea id="sourceAclDepartments" class="np-textarea" rows="4">${escapeHTML((acl.departments || []).join("\n"))}</textarea></label>
        <label class="np-field">Personen (E-Mail oder LDAP-Login, eine pro Zeile oder mit Komma)<textarea id="sourceAclUsers" class="np-textarea" rows="4">${escapeHTML((acl.users || []).join("\n"))}</textarea></label>`,
      buttons: [
        { label: "Abbrechen", action: "cancel" },
        { label: "Speichern", action: "save", accent: true, getData: () => ({
          departments: $("#sourceAclDepartments").value.split(/[\n,]/).map(x => x.trim()).filter(Boolean),
          users: $("#sourceAclUsers").value.split(/[\n,]/).map(x => x.trim()).filter(Boolean),
        }) },
      ],
    });
    if (result.action !== "save") return;
    await api("/api/sources/acl", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ source_id: sourceID, ...result.data }) });
    NovaPop.toast({ type: "success", message: "Dokumentzugriff gespeichert." });
  } catch (err) {
    NovaPop.toast({ type: "error", duration: 8000, message: err.message });
  }
}

// updateSourceFilterOptions rebuilds the "Typ"/"Dateityp" dropdowns from
// whatever source_kinds/extensions are actually present in the currently
// loaded sources (rather than a hardcoded list, since which kinds/
// extensions exist varies a lot by deployment) — preserves the current
// selection across a reload if it's still a valid option.
function updateSourceFilterOptions(sources) {
  const kindSel = $("#sourceFilterKind");
  const extSel = $("#sourceFilterExt");
  const prevKind = kindSel.value;
  const prevExt = extSel.value;

  const kinds = [...new Set(sources.map(s => s.source_kind))].sort();
  const exts = [...new Set(sources.map(s => {
    const m = /\.[a-zA-Z0-9]+$/.exec(s.source_name || "");
    return m ? m[0].toLowerCase() : null;
  }).filter(Boolean))].sort();

  kindSel.innerHTML = `<option value="">Alle Typen</option>` +
    kinds.map(k => `<option value="${escapeHTML(k)}">${escapeHTML(k)}</option>`).join("");
  extSel.innerHTML = `<option value="">Alle Dateitypen</option>` +
    exts.map(e => `<option value="${escapeHTML(e)}">${escapeHTML(e)}</option>`).join("");

  if (kinds.includes(prevKind)) kindSel.value = prevKind;
  if (exts.includes(prevExt)) extSel.value = prevExt;
}

// applySourceFilter shows/hides already-rendered #sourcesTable rows by the
// current Suchbegriff/Typ/Dateityp filter values (mirrors sourceFilter.
// matches in store.go, but client-side so typing/selecting updates
// instantly without a round-trip) and updates the live count + the
// "Gefilterte Quellen löschen" button, which only ever appears once at
// least one filter is actually set — never a one-click "delete
// everything".
function applySourceFilter() {
  const q = $("#sourceFilterQuery").value.trim().toLowerCase();
  const kind = $("#sourceFilterKind").value;
  const ext = $("#sourceFilterExt").value.toLowerCase();
  const active = !!(q || kind || ext);

  const rows = $all("#sourcesTable tbody tr[data-source-id]");
  let visible = 0;
  for (const row of rows) {
    const name = (row.dataset.sourceName || "").toLowerCase();
    const id = (row.dataset.sourceId || "").toLowerCase();
    const rowKind = row.dataset.sourceKind || "";
    const matches = (!kind || rowKind === kind) &&
      (!ext || name.endsWith(ext)) &&
      (!q || name.includes(q) || id.includes(q));
    row.hidden = active && !matches;
    if (matches) visible++;
  }

  const countEl = $("#sourceFilterCount");
  const delBtn = $("#sourceFilterDeleteBtn");
  if (!rows.length) {
    countEl.textContent = "";
    delBtn.hidden = true;
    return;
  }
  countEl.textContent = active ? t("sources.filter.visibleCount", { visible, total: rows.length }) : "";
  delBtn.hidden = !active;
  delBtn.textContent = t("sources.deleteFiltered.button", { count: visible });
  delBtn.disabled = visible === 0;
}
$("#sourceFilterQuery").addEventListener("input", applySourceFilter);
$("#sourceFilterKind").addEventListener("change", applySourceFilter);
$("#sourceFilterExt").addEventListener("change", applySourceFilter);

$("#sourceFilterDeleteBtn").addEventListener("click", async () => {
  const payload = {
    source_kind: $("#sourceFilterKind").value,
    extension: $("#sourceFilterExt").value,
    query: $("#sourceFilterQuery").value.trim(),
  };
  try {
    // Confirm against the SERVER's current match count, not this page's
    // possibly stale row count — a parallel import or a second admin may
    // have changed the store since the table was rendered.
    const preview = await api("/api/sources/delete-by-filter", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...payload, dry_run: true }),
    });
    const n = preview.matched || 0;
    if (!n) {
      alert(t("sources.deleteByFilter.noneMatched"));
      loadSources();
      return;
    }
    if (!confirm(t("confirm.deleteByFilter", { n }))) return;
    await api("/api/sources/delete-by-filter", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    loadSources();
    loadStats();
  } catch (err) {
    alert(err.message);
  }
});

// ---- Users (local accounts, localusers.go) ---------------------------
// editingUserID tracks whether #userForm is in "create" (null) or "edit"
// (the account's id) mode — the same form/submit handler serves both, only
// the request target and a couple of field defaults differ. A password
// typed while editing is optional and, if non-empty, sent as a SEPARATE
// request to /api/admin/users/password after the update succeeds — mirrors
// the server's own split (handlers_local_users.go) so an edit that only
// changes e.g. the display name can never accidentally touch the password.
let editingUserID = null;

async function loadLocalUsers() {
  const tbody = $("#usersTable tbody");
  tbody.innerHTML = `<tr><td colspan=6>${escapeHTML(t("modal.loading"))}</td></tr>`;
  try {
    const res = await api("/api/admin/users");
    const users = res.users || [];
    tbody.innerHTML = "";
    if (!users.length) tbody.innerHTML = `<tr><td colspan=6>${escapeHTML(t("users.empty"))}</td></tr>`;
    const usersByID = new Map(users.map(u => [u.id, u]));
    for (const u of users) {
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td>${escapeHTML(u.username)}</td>
        <td>${escapeHTML(u.display_name || "–")}</td>
        <td>${escapeHTML(u.department || "–")}</td>
        <td>${u.is_admin ? escapeHTML(t("common.yes")) : escapeHTML(t("common.no"))}</td>
        <td>${u.disabled ? escapeHTML(t("users.status.disabled")) : escapeHTML(t("users.status.active"))}</td>
        <td>
          <button type="button" class="small-btn" data-edit-id="${escapeHTML(u.id)}">${escapeHTML(t("users.table.editButton"))}</button>
          <button type="button" class="small-btn" data-delete-id="${escapeHTML(u.id)}" data-delete-name="${escapeHTML(u.username)}">${escapeHTML(t("common.delete"))}</button>
        </td>`;
      tbody.appendChild(tr);
    }
    $all("#usersTable button[data-edit-id]").forEach(btn => {
      btn.addEventListener("click", () => startEditLocalUser(usersByID.get(btn.dataset.editId)));
    });
    $all("#usersTable button[data-delete-id]").forEach(btn => {
      btn.addEventListener("click", async () => {
        if (!confirm(t("users.confirm.delete", { name: btn.dataset.deleteName }))) return;
        try {
          await api("/api/admin/users/delete", {
            method: "POST", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ id: btn.dataset.deleteId }),
          });
          if (editingUserID === btn.dataset.deleteId) resetUserForm();
          loadLocalUsers();
        } catch (err) { alert(err.message); }
      });
    });
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan=6>${escapeHTML(t("common.tableError", { message: err.message }))}</td></tr>`;
  }
}

// startEditLocalUser switches #userForm into edit mode for one account —
// password is deliberately left blank (never pre-filled from the server;
// handlers_local_users.go never returns a hash to begin with), so leaving
// it empty on submit means "keep the current password".
function startEditLocalUser(u) {
  if (!u) return;
  editingUserID = u.id;
  $("#u_id").value = u.id;
  $("#u_username").value = u.username;
  $("#u_password").value = "";
  $("#u_password").required = false;
  $("#u_password").placeholder = t("users.form.password.optionalPlaceholder");
  $("#u_displayName").value = u.display_name || "";
  $("#u_email").value = u.email || "";
  $("#u_department").value = u.department || "";
  $("#u_deptCode").value = u.dept_code || "";
  $("#u_isAdmin").checked = !!u.is_admin;
  $("#u_disabled").checked = !!u.disabled;
  $("#u_disabledLabel").hidden = false;
  $("#userFormHeading").textContent = t("users.form.editTitle", { name: u.username });
  $("#userFormSubmit").textContent = t("users.form.updateButton");
  $("#userFormCancel").hidden = false;
  $("#userFormResult").textContent = "";
  $("#u_username").focus();
}

function resetUserForm() {
  editingUserID = null;
  $("#userForm").reset();
  $("#u_id").value = "";
  $("#u_password").required = false;
  $("#u_password").placeholder = "";
  $("#u_disabledLabel").hidden = true;
  $("#userFormHeading").textContent = t("users.form.createTitle");
  $("#userFormSubmit").textContent = t("users.form.createButton");
  $("#userFormCancel").hidden = true;
}

$("#usersReload")?.addEventListener("click", () => loadLocalUsers());
$("#userFormCancel")?.addEventListener("click", () => { resetUserForm(); $("#userFormResult").textContent = ""; });

$("#userForm")?.addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#userFormResult");
  const btn = $("#userFormSubmit");
  out.className = "result";
  const password = $("#u_password").value;
  setBusy(btn, true);
  try {
    if (editingUserID) {
      await api("/api/admin/users/update", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          id: editingUserID, username: $("#u_username").value, display_name: $("#u_displayName").value,
          email: $("#u_email").value, department: $("#u_department").value, dept_code: $("#u_deptCode").value,
          is_admin: $("#u_isAdmin").checked, disabled: $("#u_disabled").checked,
        }),
      });
      if (password) {
        await api("/api/admin/users/password", {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ id: editingUserID, password }),
        });
      }
      out.textContent = t("users.form.updated");
    } else {
      await api("/api/admin/users/create", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          username: $("#u_username").value, password,
          display_name: $("#u_displayName").value, email: $("#u_email").value,
          department: $("#u_department").value, dept_code: $("#u_deptCode").value,
          is_admin: $("#u_isAdmin").checked,
        }),
      });
      out.textContent = t("users.form.created");
    }
    resetUserForm();
    loadLocalUsers();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

// ---- Chunk viewer ---------------------------------------------------
function chunksQuery(offset) {
  const params = new URLSearchParams();
  const q = $("#cf_q").value.trim();
  const sourceID = $("#cf_source_id").value.trim();
  const sourceKind = $("#cf_source_kind").value;
  const embedModel = $("#cf_embed_model").value.trim();
  if (q) params.set("q", q);
  if (sourceID) params.set("source_id", sourceID);
  if (sourceKind) params.set("source_kind", sourceKind);
  if (embedModel) params.set("embed_model", embedModel);
  params.set("sort", $("#cf_sort").value);
  params.set("order", $("#cf_order").dataset.order);
  params.set("limit", $("#cf_limit").value);
  params.set("offset", String(offset));
  return params.toString();
}

// freshnessCell renders a 0-100% freshness score as a small bar-plus-label,
// clamping out-of-range input so a bad score can't produce a negative or
// over-100% bar width.
function freshnessCell(score) {
  const pct = Math.max(0, Math.min(100, Math.round(score * 100)));
  return `<span class="freshness-bar"><span style="width:${pct}%"></span></span><span class="freshness-val">${pct}%</span>`;
}

// loadChunks fetches and renders one page of the chunk viewer table,
// starting at offset, along with its pager and result-count summary.
async function loadChunks(offset) {
  const tbody = $("#chunksTable tbody");
  tbody.innerHTML = `<tr><td colspan=7>${escapeHTML(t("modal.loading"))}</td></tr>`;
  $("#chunksMeta").textContent = "";
  $("#chunksPager").innerHTML = "";
  try {
    const res = await api("/api/chunks?" + chunksQuery(offset));
    tbody.innerHTML = "";
    if (!res.chunks.length) tbody.innerHTML = `<tr><td colspan=7>${escapeHTML(t("chunks.noResults"))}</td></tr>`;
    for (const c of res.chunks) {
      const tr = document.createElement("tr");
      tr.className = "chunk-row";
      tr.innerHTML = `
        <td class="name" title="${escapeHTML(c.source_id)}">${escapeHTML(c.source_name)}</td>
        <td>${escapeHTML(c.source_kind)}</td>
        <td>${c.chunk_idx}</td>
        <td>${c.chars}</td>
        <td>${fmtTime(c.loaded_at)}</td>
        <td>${fmtTime(c.doc_date)}</td>
        <td>${freshnessCell(c.freshness)}</td>`;
      tr.addEventListener("click", () => toggleChunkDetail(tr, c));
      tbody.appendChild(tr);
    }
    const from = res.total === 0 ? 0 : res.offset + 1;
    const to = res.offset + res.chunks.length;
    let meta = t("chunks.meta.range", { from, to, total: res.total });
    if (res.capped) meta += " " + t("chunks.meta.capped");
    $("#chunksMeta").textContent = meta;

    const pager = $("#chunksPager");
    const prevBtn = document.createElement("button");
    prevBtn.textContent = t("chunks.pager.prev");
    prevBtn.disabled = res.offset <= 0;
    prevBtn.addEventListener("click", () => loadChunks(Math.max(0, res.offset - res.limit)));
    const nextBtn = document.createElement("button");
    nextBtn.textContent = t("chunks.pager.next");
    nextBtn.disabled = res.offset + res.chunks.length >= res.total;
    nextBtn.addEventListener("click", () => loadChunks(res.offset + res.limit));
    pager.appendChild(prevBtn);
    pager.appendChild(nextBtn);
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan=7>${escapeHTML(t("common.tableError", { message: err.message }))}</td></tr>`;
  }
}

// toggleChunkDetail expands/collapses a chunk's detail row below its table
// row; only one detail row is ever open at a time, so opening one closes
// whichever other row was previously expanded.
function toggleChunkDetail(tr, c) {
  const next = tr.nextElementSibling;
  if (next && next.classList.contains("chunk-detail")) { next.remove(); return; }
  $all("#chunksTable tr.chunk-detail").forEach(el => el.remove());
  const detail = document.createElement("tr");
  detail.className = "chunk-detail";
  detail.innerHTML = `<td colspan="7">
    <div class="meta">
      <span>ID: ${c.id}</span>
      <span>Quell-ID: ${escapeHTML(c.source_id)}</span>
      <span>Load-ID: ${escapeHTML(c.load_id)}</span>
      <span>Embedding-Modell: ${escapeHTML(c.embed_model)}</span>
      <span>Chunk-Position: ${c.chunk_idx}</span>
      <span>Aktualität: ${(c.freshness * 100).toFixed(1)}%</span>
    </div>
    <div class="chunk-detail-actions">
      <button type="button" class="link-btn chunk-open-source">${escapeHTML(t("chunks.detail.openSource"))}</button>
      <button type="button" class="link-btn qt-remove chunk-delete-source">${escapeHTML(t("chunks.detail.deleteSource"))}</button>
    </div>
    <div class="content-box"></div>
    <div class="result chunk-detail-result" role="status"></div>
  </td>`;
  detail.querySelector(".content-box").textContent = c.content;
  detail.querySelector(".chunk-open-source").addEventListener("click", () => {
    openSourcePopup(c.source_id, c.source_name, c.source_kind);
  });
  // "Quelle löschen" reuses the exact same /api/sources/delete the Sources
  // tab's own delete button calls (handlers_sources.go) — deletes every
  // chunk of this source, not just this one row, since chunks aren't
  // independently meaningful/re-embeddable outside their source (see
  // replaceSourceChunks' doc comment, store.go): re-importing the source
  // is how a chunk actually gets "re-embedded" in this pipeline.
  detail.querySelector(".chunk-delete-source").addEventListener("click", async () => {
    if (!confirm(t("confirm.deleteSource"))) return;
    const out = detail.querySelector(".chunk-detail-result");
    out.className = "result chunk-detail-result";
    try {
      await api("/api/sources/delete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ source_id: c.source_id }),
      });
      loadChunks(0);
    } catch (err) {
      out.className = "result chunk-detail-result error";
      out.textContent = err.message;
    }
  });
  tr.after(detail);
}

$("#chunksFilterForm").addEventListener("submit", (e) => { e.preventDefault(); loadChunks(0); });
$("#cf_order").addEventListener("click", () => {
  const btn = $("#cf_order");
  const next = btn.dataset.order === "desc" ? "asc" : "desc";
  btn.dataset.order = next;
  btn.textContent = next === "desc" ? t("chunks.order.desc") : t("chunks.order.asc");
  loadChunks(0);
});
// "Aktualisieren" reloads the current page with whatever filters/sort/page
// size are already set — same intent as Jobs'/Sources' own refresh
// buttons, previously missing here (the only way to reload was re-submit
// the filter form, which isn't obvious when nothing about the filter
// itself changed).
$("#chunksRefresh")?.addEventListener("click", () => loadChunks(0));

// Testsuche (chunks.searchTest.*): runs the real rankedSearch (POST
// /api/chunks/search-test, chunks.go) for a sample question and renders
// hits with the same per-hit score breakdown Debug-Modus already uses
// (renderDebugPanel's .debug-chunk-list), so the two stay visually
// consistent.
$("#chunksSearchTestForm")?.addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#chunksSearchTestResult");
  const list = $("#chunksSearchTestList");
  const query = $("#cst_query").value.trim();
  out.className = "result";
  list.innerHTML = "";
  if (!query) { out.textContent = t("chunks.searchTest.emptyQuery"); return; }
  out.textContent = t("chunks.searchTest.searching");
  try {
    const res = await api("/api/chunks/search-test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query }),
    });
    const hits = res.hits || [];
    if (!hits.length) {
      out.textContent = t("chunks.searchTest.noResults");
      return;
    }
    out.textContent = t("chunks.searchTest.resultCount", { count: hits.length });
    hits.forEach(hit => {
      const li = document.createElement("li");
      li.innerHTML = `<strong>${escapeHTML(hit.source_name || hit.source_id)}</strong> ` +
        `<span class="hint">(${escapeHTML(hit.source_kind)}, final=${(hit.final_score ?? 0).toFixed(3)} ` +
        `vector=${(hit.vector_score ?? 0).toFixed(3)} keyword=${(hit.keyword_score ?? 0).toFixed(3)} ` +
        `recency=${(hit.recency_score ?? 0).toFixed(3)})</span>`;
      const pre = document.createElement("pre");
      pre.className = "debug-pre";
      pre.textContent = hit.content || "";
      li.appendChild(pre);
      const jump = document.createElement("button");
      jump.type = "button";
      jump.className = "link-btn";
      jump.textContent = t("chunks.searchTest.showAllChunks");
      jump.addEventListener("click", () => navigateToChunksForSource(hit.source_id));
      li.appendChild(document.createElement("br"));
      li.appendChild(jump);
      list.appendChild(li);
    });
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  }
});

// ---- Prompts --------------------------------------------------------
async function loadPrompts() {
  const err1 = $("#promptsIndexResult"), err2 = $("#promptsSkillResult");
  err1.className = "result"; err1.textContent = "";
  err2.className = "result"; err2.textContent = "";
  try {
    const res = await api("/api/prompts");
    $("#promptsIndex").value = res.index_content || "";
    $("#promptsDraft").value = res.draft_content || "";
    $("#promptsAgent").value = res.agent_content || "";
    $("#promptsIndexDefaultBadge").hidden = !res.index_is_default;
    $("#promptsDraftDefaultBadge").hidden = !res.draft_is_default;
    $("#promptsAgentDefaultBadge").hidden = !res.agent_is_default;
    renderSkillsList(res.skills || []);
  } catch (err) { err1.className = "result error"; err1.textContent = err.message; }
  loadDepartmentRules();
}

// loadDepartmentRules fetches the currently effective department
// classification ruleset (department_rules.json override, or the
// built-in default — see department.go) and shows which one is active.
async function loadDepartmentRules() {
  const out = $("#departmentRulesResult");
  out.className = "result"; out.textContent = "";
  try {
    const res = await api("/api/department-rules");
    $("#departmentRules").value = JSON.stringify(res.rules || [], null, 2);
    $("#departmentRulesSource").textContent = res.overridden
      ? t("prompts.departmentRules.sourceCustom")
      : t("prompts.departmentRules.sourceDefault");
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
}

$("#saveDepartmentRules").addEventListener("click", async () => {
  const out = $("#departmentRulesResult");
  out.className = "result";
  let rules;
  try {
    rules = JSON.parse($("#departmentRules").value);
  } catch (err) {
    out.className = "result error";
    out.textContent = "Ungültiges JSON: " + err.message;
    return;
  }
  try {
    await api("/api/department-rules/save", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ rules }),
    });
    // loadDepartmentRules also writes to #departmentRulesResult (clearing
    // it on entry) — call it first so this success message is the one
    // left standing, not overwritten by it.
    await loadDepartmentRules();
    out.textContent = "Gespeichert.";
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
});

$("#resetDepartmentRules").addEventListener("click", async () => {
  const out = $("#departmentRulesResult");
  out.className = "result";
  try {
    await api("/api/department-rules/reset", { method: "POST" });
    await loadDepartmentRules(); // see saveDepartmentRules above for why this runs before the message is set
    out.textContent = "Auf Standard zurückgesetzt.";
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
});

// renderSkillsList renders the Prompts page's skill table (or an empty-
// state hint) from the current list of skills.
function renderSkillsList(skills) {
  const list = $("#skillsList");
  list.innerHTML = "";
  if (!skills.length) {
    list.innerHTML = `<p class="hint" style="padding:12px 0">${escapeHTML(t("prompts.skills.empty"))}</p>`;
    return;
  }
  const table = document.createElement("div");
  table.className = "skill-table";
  const hdr = document.createElement("div");
  hdr.className = "skill-table-hdr";
  hdr.innerHTML = `<span></span><span data-i18n="prompts.skills.table.name">${escapeHTML(t("prompts.skills.table.name"))}</span><span data-i18n="prompts.skills.table.tags">${escapeHTML(t("prompts.skills.table.tags"))}</span><span data-i18n="prompts.skills.table.file">${escapeHTML(t("prompts.skills.table.file"))}</span><span></span>`;
  table.appendChild(hdr);
  skills.forEach(skill => table.appendChild(buildSkillRow(skill)));
  list.appendChild(table);
}

// buildSkillRow builds one skill's summary row (name, truncated tag chips,
// actions) plus its paired, initially-hidden editor panel (see
// buildSkillEditor) that expands beneath it.
function buildSkillRow(skill) {
  const wrap = document.createElement("div");
  wrap.className = "skill-row-wrap";

  const tags = skill.tags || [];
  const MAX_TAGS = 3;
  const tagChips = tags.slice(0, MAX_TAGS).map(t =>
    `<span class="sk-tag">${escapeHTML(t)}</span>`).join("");
  const more = tags.length > MAX_TAGS
    ? `<span class="sk-tag-more">+${tags.length - MAX_TAGS}</span>` : "";

  const row = document.createElement("button");
  row.type = "button";
  row.className = "skill-row-head";
  row.setAttribute("aria-expanded", "false");
  row.innerHTML =
    `<span class="sk-status-dot${skill.enabled ? " enabled" : ""}"></span>` +
    `<span class="sk-row-name">${escapeHTML(skill.display_name || skill.filename)}</span>` +
    `<span class="sk-row-tags">${tagChips}${more}</span>` +
    `<span class="sk-row-file">${escapeHTML(skill.filename)}</span>` +
    `<span class="sk-chevron" aria-hidden="true">›</span>`;

  const panel = buildSkillEditor(skill);

  const toggle = () => {
    const willOpen = panel.hidden;
    document.querySelectorAll(".skill-editor-panel").forEach(p => { if (p !== panel) p.hidden = true; });
    document.querySelectorAll(".skill-row-head.sk-open").forEach(r => { if (r !== row) { r.classList.remove("sk-open"); r.setAttribute("aria-expanded", "false"); } });
    panel.hidden = !willOpen;
    row.classList.toggle("sk-open", willOpen);
    row.setAttribute("aria-expanded", String(willOpen));
    if (willOpen) panel.querySelector(".sk-display-name").focus();
  };
  row.addEventListener("click", toggle);

  wrap.appendChild(row);
  wrap.appendChild(panel);
  return wrap;
}

// buildSkillEditor builds the editable form for one skill (metadata +
// content), shown expanded by default only for a brand-new skill.
function buildSkillEditor(skill) {
  const panel = document.createElement("div");
  panel.className = "skill-editor-panel";
  panel.hidden = !skill._isNew;
  const filename = skill.filename;

  panel.innerHTML =
    `<div class="skill-editor-inner">` +
    `<div class="skill-meta-row">` +
    `<label class="inline-label sk-toggle"><input type="checkbox" class="sk-enabled"> ${escapeHTML(t("prompts.skills.editor.active"))}</label>` +
    `<input type="text" class="sk-display-name" placeholder="${escapeHTML(t("prompts.skills.editor.displayNamePlaceholder"))}">` +
    `<span class="sk-filename-badge">${escapeHTML(filename)}</span>` +
    `</div>` +
    `<label class="skill-field-label">${escapeHTML(t("prompts.skills.editor.description"))}<input type="text" class="sk-description" placeholder="${escapeHTML(t("prompts.skills.editor.descriptionPlaceholder"))}"></label>` +
    `<label class="skill-field-label">${escapeHTML(t("prompts.skills.editor.tags"))} <span class="hint">${escapeHTML(t("prompts.skills.editor.tagsHint"))}</span>` +
    `<input type="text" class="sk-tags" placeholder="${escapeHTML(t("prompts.skills.editor.tagsPlaceholder"))}"></label>` +
    `<label class="skill-field-label">${escapeHTML(t("prompts.skills.editor.content"))}<textarea class="sk-content prompt-editor" rows="10"></textarea></label>` +
    `<div class="skill-editor-actions">` +
    `<button type="button" class="sk-save">${escapeHTML(t("common.save"))}</button>` +
    (skill._isNew ? "" : `<button type="button" class="sk-delete small-btn">${escapeHTML(t("common.delete"))}</button>`) +
    `<div class="result sk-result" role="status"></div>` +
    `</div></div>`;

  panel.querySelector(".sk-enabled").checked = !!skill.enabled;
  panel.querySelector(".sk-display-name").value = skill.display_name || "";
  panel.querySelector(".sk-description").value = skill.description || "";
  panel.querySelector(".sk-tags").value = (skill.tags || []).join(", ");
  panel.querySelector(".sk-content").value = skill.content || "";

  const result = panel.querySelector(".sk-result");

  panel.querySelector(".sk-save").addEventListener("click", async () => {
    result.className = "result sk-result";
    result.textContent = "";
    const newTags = panel.querySelector(".sk-tags").value.split(",").map(t => t.trim()).filter(Boolean);
    try {
      await api("/api/prompts/skill", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          filename,
          display_name: panel.querySelector(".sk-display-name").value.trim(),
          description: panel.querySelector(".sk-description").value.trim(),
          enabled: panel.querySelector(".sk-enabled").checked,
          tags: newTags,
          content: panel.querySelector(".sk-content").value,
        }),
      });
      result.textContent = "Gespeichert.";
      // Sync row visuals without full reload
      const wrap = panel.closest(".skill-row-wrap");
      if (wrap) {
        const dot = wrap.querySelector(".sk-status-dot");
        const nameEl = wrap.querySelector(".sk-row-name");
        const tagsEl = wrap.querySelector(".sk-row-tags");
        if (dot) dot.className = "sk-status-dot" + (panel.querySelector(".sk-enabled").checked ? " enabled" : "");
        if (nameEl) nameEl.textContent = panel.querySelector(".sk-display-name").value.trim() || filename;
        if (tagsEl) {
          const MAX_TAGS = 3;
          tagsEl.innerHTML = newTags.slice(0, MAX_TAGS).map(t => `<span class="sk-tag">${escapeHTML(t)}</span>`).join("") +
            (newTags.length > MAX_TAGS ? `<span class="sk-tag-more">+${newTags.length - MAX_TAGS}</span>` : "");
        }
      }
      skill._isNew = false;
    } catch (err) { result.className = "result sk-result error"; result.textContent = err.message; }
  });

  const delBtn = panel.querySelector(".sk-delete");
  if (delBtn) {
    delBtn.addEventListener("click", async () => {
      if (!confirm(t("confirm.deleteSkill", { filename }))) return;
      try {
        await api("/api/prompts/skill/delete", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ filename }),
        });
        panel.closest(".skill-row-wrap")?.remove();
      } catch (err) { result.className = "result sk-result error"; result.textContent = err.message; }
    });
  }
  return panel;
}

$("#addSkill").addEventListener("click", () => {
  const name = prompt(t("prompt.newSkillFilename"));
  if (!name) return;
  if (!/^skill_[a-zA-Z0-9_-]+\.md$/.test(name)) {
    alert(t("alert.invalidSkillFilename"));
    return;
  }
  const newSkill = { filename: name, enabled: false, tags: [], content: "", description: "", _isNew: true };
  const newRow = buildSkillRow(newSkill);
  const table = document.querySelector("#skillsList .skill-table");
  if (table) {
    const hdr = table.querySelector(".skill-table-hdr");
    table.insertBefore(newRow, hdr ? hdr.nextSibling : table.firstChild);
  } else {
    $("#skillsList").appendChild(newRow);
  }
  newRow.querySelector(".skill-row-head").click();
});

// Skill tester (prompts.skills.tester.*): tries a sample question against
// whatever skills are currently SAVED on disk (POST /api/prompts/skill-test,
// prompts_admin.go) — reuses the exact selectSkills logic buildSystemPrompt
// runs at request time, so this can never show a result runtime behavior
// wouldn't actually produce. Note it only sees saved skills: an unsaved
// edit in an open editor panel above isn't reflected until "Speichern".
$("#skillTestButton").addEventListener("click", async () => {
  const out = $("#skillTestResult");
  const question = $("#skillTestQuestion").value.trim();
  out.className = "result";
  if (!question) { out.textContent = t("prompts.skills.tester.emptyQuestion"); return; }
  out.textContent = t("prompts.skills.tester.testing");
  try {
    const res = await api("/api/prompts/skill-test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ question }),
    });
    const selected = res.selected || [];
    out.textContent = selected.length
      ? t("prompts.skills.tester.matched", { skills: selected.join(", ") })
      : t("prompts.skills.tester.noneMatched");
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
});

// refreshPromptDefaultBadges re-fetches just enough to keep the "using
// built-in default" badges (prompts.usingBuiltinDefault) correct after a
// save — can't simply hide the just-saved badge optimistically, since
// saving empty/whitespace-only content still reads back as the default
// (readIndexPrompt/readDraftPrompt/readAgentPrompt, skills.go/draft.go),
// so the badge may legitimately still be showing afterward.
async function refreshPromptDefaultBadges() {
  try {
    const res = await api("/api/prompts");
    $("#promptsIndexDefaultBadge").hidden = !res.index_is_default;
    $("#promptsDraftDefaultBadge").hidden = !res.draft_is_default;
    $("#promptsAgentDefaultBadge").hidden = !res.agent_is_default;
  } catch { /* best-effort — a stale badge is harmless, unlike a failed save */ }
}

$("#savePromptsIndex").addEventListener("click", async () => {
  const out = $("#promptsIndexResult");
  out.className = "result";
  try {
    await api("/api/prompts/index", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: $("#promptsIndex").value }),
    });
    out.textContent = "Gespeichert.";
    refreshPromptDefaultBadges();
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
});

$("#savePromptsDraft").addEventListener("click", async () => {
  const out = $("#promptsDraftResult");
  out.className = "result";
  try {
    await api("/api/prompts/draft", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: $("#promptsDraft").value }),
    });
    out.textContent = "Gespeichert.";
    refreshPromptDefaultBadges();
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
});

$("#savePromptsAgent").addEventListener("click", async () => {
  const out = $("#promptsAgentResult");
  out.className = "result";
  try {
    await api("/api/prompts/agent", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: $("#promptsAgent").value }),
    });
    out.textContent = "Gespeichert.";
    refreshPromptDefaultBadges();
  } catch (err) { out.className = "result error"; out.textContent = err.message; }
});

// ---- Settings -------------------------------------------------------

// URL mappings are stored as "Pfad → URL" lines in the textarea.
function parseURLMappings(text) {
  return text.split("\n")
    .map(l => l.trim())
    .filter(Boolean)
    .map(l => {
      // "→" is the documented separator (see the textarea's placeholder/
      // tooltip), but a hand-typed "->" is an easy, natural mistake on a
      // keyboard that has no arrow key — accepting it too means that line
      // still saves as a real mapping instead of silently vanishing.
      let sep = l.indexOf("→");
      let sepLen = 1;
      if (sep < 0) {
        sep = l.indexOf("->");
        sepLen = 2;
      }
      if (sep < 0) return null;
      return { prefix: l.slice(0, sep).trim(), url_prefix: l.slice(sep + sepLen).trim() };
    })
    .filter(Boolean);
}

// formatURLMappings renders URL-prefix mappings as one "prefix → url_prefix"
// line per mapping, for display in a settings textarea.
function formatURLMappings(mappings) {
  if (!mappings || !mappings.length) return "";
  return mappings.map(m => `${m.prefix} → ${m.url_prefix}`).join("\n");
}

// Source-visibility settings are stored as "source_kind = true/false" lines
// in the textarea — same lightweight line-based format as the URL mappings
// above. A kind left out of the map defaults to visible on the server side.
function parseSourceVisibility(text) {
  const out = {};
  text.split("\n").map(l => l.trim()).filter(Boolean).forEach(l => {
    const sep = l.indexOf("=");
    if (sep < 0) return;
    const kind = l.slice(0, sep).trim();
    if (!kind) return;
    const val = l.slice(sep + 1).trim().toLowerCase();
    out[kind] = val !== "false" && val !== "0" && val !== "nein";
  });
  return out;
}

// formatSourceVisibility renders the source_visibility map as one
// "source_kind = true/false" line per entry, for display in a settings
// textarea.
function formatSourceVisibility(map) {
  if (!map) return "";
  return Object.keys(map).sort().map(k => `${k} = ${map[k]}`).join("\n");
}

// parseSourceAccess/formatSourceAccess mirror parseSourceVisibility/
// formatSourceVisibility above for source_access — a "source_kind =
// Dept1,Dept2" line per entry instead of a boolean, since a source_kind
// here maps to a *list* of department codes (see settings.go's
// SourceAccess/department.go's classifyDepartment).
function parseSourceAccess(text) {
  const out = {};
  text.split("\n").map(l => l.trim()).filter(Boolean).forEach(l => {
    const sep = l.indexOf("=");
    if (sep < 0) return;
    const kind = l.slice(0, sep).trim();
    if (!kind) return;
    const codes = l.slice(sep + 1).split(",").map(c => c.trim()).filter(Boolean);
    if (codes.length) out[kind] = codes;
  });
  return out;
}
function formatSourceAccess(map) {
  if (!map) return "";
  return Object.keys(map).sort().map(k => `${k} = ${(map[k] || []).join(",")}`).join("\n");
}

// ---- Query-template structured editor (SQL + HTTP) ------------------
// A form-based editor over the same JSON the server stores, so an admin
// enters allowed queries field-by-field instead of hand-writing JSON. The
// underlying <textarea> stays the single source of truth the save path
// reads (loadSettings/saveSettings are unchanged) — this editor renders
// from it and writes back to it on every change; a collapsible "Als JSON
// bearbeiten" escape hatch remains for power users. The fields the model
// sees (description, per-parameter description/example, result hint) are
// grouped and labelled as such, since getting them right is what lets the
// model call a query correctly (see connector.go's queryTemplateToolSchema).
const QT_PARAM_TYPES = ["string", "integer", "number", "boolean", "date"];

const QT_CFG = {
  sql: {
    editor: "#mssqlTemplatesEditor", ta: "#s_mssql_query_templates",
    bodyKey: "sql", bodyLabel: "SQL (nur SELECT, Parameter als {name})",
    bodyRows: 3, bodyPh: "SELECT TOP 50 Bestellnr, Kunde, Betrag FROM Orders WHERE CustomerID = {customer_id} ORDER BY Datum DESC",
    http: false,
  },
  http: {
    editor: "#httpTemplatesEditor", ta: "#s_http_templates",
    bodyKey: "url_template", bodyLabel: "URL-Vorlage (Parameter als {name})",
    bodyRows: 2, bodyPh: "https://rubix.freshservice.com/api/v2/tickets/{ticket_id}",
    http: true,
  },
};

function qtEl(tag, props, kids) {
  const e = document.createElement(tag);
  if (props) for (const k in props) {
    if (k === "class") e.className = props[k];
    else if (k === "text") e.textContent = props[k];
    else if (k === "html") e.innerHTML = props[k];
    else e.setAttribute(k, props[k]);
  }
  (kids || []).forEach(c => c && e.appendChild(c));
  return e;
}

// qtField builds a labelled control bound to obj[key]; onChange also
// re-serializes the whole array back into the textarea.
function qtField(labelText, title, control) {
  const l = qtEl("label", { title: title || "" }, [document.createTextNode(labelText + " ")]);
  l.appendChild(control);
  return l;
}

// connCardMenu builds a self-contained "⋮" dropdown (kebab button + popover)
// for one connector card's header — the uniform Entfernen/Exportieren/
// Importieren/Duplizieren/Testen menu every connCard now offers instead of
// a single bare "Verbindung entfernen" button. actions is an ordered list
// of {label, onClick, danger?}; danger-flagged entries (only "Verbindung
// entfernen" today) render in --danger, matching the old standalone
// button's .qt-remove color. Each call creates its own independent
// popover/state — safe to call once per card, N times per settings page.
function connCardMenu(actions) {
  const wrap = qtEl("div", { class: "conn-card-menu" });
  const toggle = qtEl("button", {
    type: "button", class: "conn-card-menu-toggle", "aria-haspopup": "menu", "aria-expanded": "false",
    title: "Weitere Aktionen für diese Verbindung", "aria-label": "Weitere Aktionen für diese Verbindung",
    html: "&#8942;",
  });
  const popover = qtEl("div", { class: "conn-card-menu-popover", role: "menu", hidden: "" });

  const close = () => { popover.hidden = true; toggle.setAttribute("aria-expanded", "false"); };
  const open = () => { popover.hidden = false; toggle.setAttribute("aria-expanded", "true"); };

  actions.forEach(a => {
    const item = qtEl("button", {
      type: "button", role: "menuitem",
      class: "conn-card-menu-option" + (a.danger ? " conn-card-menu-option-danger" : ""),
      text: a.label,
    });
    item.addEventListener("click", () => { close(); a.onClick(); });
    popover.appendChild(item);
  });

  toggle.addEventListener("click", (e) => {
    e.stopPropagation();
    if (popover.hidden) open(); else close();
  });
  // Outside-click/Escape-to-close are registered on document (the click can
  // land anywhere on the page), so each is written to self-unregister once
  // wrap is no longer attached — every connector-list action (add/remove/
  // duplicate/import) rebuilds the whole card list from scratch
  // (renderConnEditor's host.innerHTML = ""), so without this a menu's
  // listeners would otherwise outlive its own DOM and accumulate forever
  // over a settings session instead of being garbage-collected with it.
  const onDocClick = (e) => {
    if (!document.body.contains(wrap)) { document.removeEventListener("click", onDocClick); return; }
    if (!popover.hidden && !wrap.contains(e.target)) close();
  };
  const onDocKeydown = (e) => {
    if (!document.body.contains(wrap)) { document.removeEventListener("keydown", onDocKeydown); return; }
    if (e.key === "Escape" && !popover.hidden) { close(); toggle.focus(); }
  };
  document.addEventListener("click", onDocClick);
  document.addEventListener("keydown", onDocKeydown);

  wrap.appendChild(toggle);
  wrap.appendChild(popover);
  return wrap;
}

function qtReadArray(cfg) {
  try {
    const v = JSON.parse(document.querySelector(cfg.ta).value || "[]");
    return Array.isArray(v) ? v : [];
  } catch { return []; }
}

function qtSync(kind, arr) {
  document.querySelector(QT_CFG[kind].ta).value = JSON.stringify(arr, null, 2);
  if (typeof markSettingsDirty === "function") markSettingsDirty(true);
}

function renderQTEditor(kind) {
  const cfg = QT_CFG[kind];
  const host = document.querySelector(cfg.editor);
  if (!host) return;
  const arr = qtReadArray(cfg);
  host.innerHTML = "";
  if (!arr.length) {
    host.appendChild(qtEl("p", { class: "hint", text: "Noch keine Vorlage angelegt. Unten hinzufügen." }));
  }
  arr.forEach((t, i) => host.appendChild(qtCard(kind, arr, t, i)));
}

function qtCard(kind, arr, t, idx) {
  const cfg = QT_CFG[kind];
  t.parameters = t.parameters || [];
  const rerender = () => renderQTEditor(kind);
  const sync = () => qtSync(kind, arr);

  const bind = (control, obj, key) => {
    control.addEventListener("input", () => { obj[key] = control.type === "checkbox" ? control.checked : control.value; sync(); });
    return control;
  };
  const input = (val, ph) => { const i = qtEl("input"); i.value = val || ""; if (ph) i.placeholder = ph; return i; };
  const textarea = (val, rows, ph) => { const a = qtEl("textarea", { class: "prompt-editor", rows: String(rows || 2) }); a.value = val || ""; if (ph) a.placeholder = ph; return a; };

  // Header: name + enabled + remove
  const nameI = bind(input(t.name, "eindeutiger_werkzeugname"), t, "name");
  const enabledC = qtEl("input", { type: "checkbox" }); enabledC.checked = t.enabled !== false;
  enabledC.addEventListener("input", () => { t.enabled = enabledC.checked; sync(); });
  const rm = qtEl("button", { type: "button", class: "ghost-btn qt-remove", text: "Vorlage entfernen" });
  rm.addEventListener("click", () => { arr.splice(idx, 1); qtSync(kind, arr); rerender(); });
  const header = qtEl("div", { class: "qt-card-head" }, [
    qtField("Werkzeugname", "Der eindeutige Funktionsname, unter dem das Modell dieses Werkzeug aufruft (a–z, 0–9, _). Kein Leerzeichen.", nameI),
    qtField("aktiv", "Nur aktive Vorlagen werden dem Modell als Werkzeug angeboten.", enabledC),
    rm,
  ]);

  // Model-facing block
  const descA = bind(textarea(t.description, 2, "Wofür ist diese Abfrage da? Wann soll das Modell sie nutzen (und wann nicht)? Diesen Text liest das Modell wortwörtlich."), t, "description");
  const bodyA = bind(textarea(t[cfg.bodyKey], cfg.bodyRows, cfg.bodyPh), t, cfg.bodyKey);
  const resultA = bind(input(t.result_hint, "z. B. Eine Zeile je offener Bestellung: Bestellnr., Kunde, Betrag in EUR, Datum."), t, "result_hint");

  const body = qtEl("div", { class: "qt-card-body" }, [
    qtField("Beschreibung fürs Modell", "Sieht das Modell wortwörtlich — entscheidet, ob es die Abfrage im richtigen Moment nutzt.", descA),
    qtField(cfg.bodyLabel, "Sieht das Modell NIE — nur der Server führt das aus. Jeder Parameter unten muss hier vorkommen.", bodyA),
  ]);

  if (cfg.http) {
    const authSel = qtEl("select");
    // Built-in Basic-auth sources plus every configured generic REST connector
    // (settings.go's restConnectorConfig, edited in connState.rest_connectors).
    const authOptions = ["none", "confluence", "jira", "freshservice"];
    (connState.rest_connectors || []).forEach(rc => {
      const n = (rc.name || "").trim();
      if (n && !authOptions.includes(n)) authOptions.push(n);
    });
    // Keep an already-selected but now-unknown source in the list so editing
    // an unrelated field can't silently drop it.
    if (t.auth_source && !authOptions.includes(t.auth_source)) authOptions.push(t.auth_source);
    authOptions.forEach(o => { const op = qtEl("option", { value: o, text: o }); if ((t.auth_source || "none") === o) op.selected = true; authSel.appendChild(op); });
    authSel.addEventListener("input", () => { t.auth_source = authSel.value; sync(); });
    body.appendChild(qtField("Zugangsdaten von (auth_source)", "Welcher konfigurierte Konnektor liefert Zugangsdaten und pinnt den erlaubten Host: eine der eingebauten Quellen oder ein eigener REST-Connector (unten unter „Generische REST-Connectoren“). Die URL muss denselben Host haben.", authSel));
    body.appendChild(qtField("Antwort auf ein Feld einschränken (optional)", "Statt der vollen Antwort nur ein bestimmtes Feld davon ans Modell weitergeben — z. B. 'tickets' für ein oberstes Feld, oder 'tickets.0.status', um gezielt in verschachtelte Daten hineinzugreifen (Punkt-Schreibweise, Zahl = Position in einer Liste). Leer = volle Antwort wird übergeben.", bind(input(t.response_json_path, "z. B. tickets oder tickets.0.status"), t, "response_json_path")));
    const insecureC = qtEl("input", { type: "checkbox" }); insecureC.checked = !!t.insecure_skip_verify;
    insecureC.addEventListener("input", () => { t.insecure_skip_verify = insecureC.checked; sync(); });
    body.appendChild(qtField("TLS-Zertifikat ungeprüft akzeptieren", "Nur für interne Endpunkte mit selbstsigniertem oder intern ausgestelltem Zertifikat (z. B. ein On-Prem-SAP-se16-Gateway) — sonst z. B. \"tls: failed to verify certificate: x509: certificate signed by unknown authority\". Bei einem Endpunkt mit echtem, öffentlich vertrauenswürdigem Zertifikat deaktiviert lassen.", insecureC));
  }
  body.appendChild(qtField("Rückgabe-Hinweis fürs Modell (optional)", "Was kommt zurück (welche Spalten/Felder, in welcher Einheit)? Sieht das Modell wortwörtlich, damit es weiß, was es erwarten kann.", resultA));

  // "Vorlage testen" (MSSQL only): actually runs the SQL against the
  // (possibly not-yet-saved) connection config above, using each
  // parameter's Example value as input — proves the template really
  // executes and returns rows, rather than only the save-time syntax/
  // parameter-reference check validateSQLQueryTemplates already does.
  // Same request/response shape as wireConnTest's static buttons above,
  // just per-card instead of per-fixed-id since there's one card per
  // template.
  if (!cfg.http) {
    const testBtn = qtEl("button", { type: "button", class: "ghost-btn", text: "Vorlage testen" });
    testBtn.title = "Führt diese Vorlage einmal aus — mit den obigen (noch nicht gespeicherten) Verbindungsdaten und den Beispielwerten der Parameter unten — und zeigt echte Ergebniszeilen oder den Fehler an.";
    const testOut = qtEl("div", { class: "result", role: "status" });
    testBtn.addEventListener("click", async () => {
      testOut.className = "result";
      testOut.textContent = "Teste Vorlage …";
      setBusy(testBtn, true);
      try {
        const res = await api("/api/settings/test/mssql-template", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            config: {
              host: $("#s_mssql_host").value.trim(),
              port: parseInt($("#s_mssql_port").value, 10) || 1433,
              database: $("#s_mssql_database").value.trim(),
              username: $("#s_mssql_username").value.trim(),
              password_env: $("#s_mssql_password_env").value.trim(),
              password: $("#s_mssql_password").value.trim(),
              trust_server_certificate: $("#s_mssql_trust_cert").checked,
            },
            template: t,
          }),
        });
        testOut.className = res.ok ? "result" : "result error";
        testOut.textContent = (res.ok ? "✓ " : "✗ ") + res.detail;
      } catch (err) {
        testOut.className = "result error";
        testOut.textContent = err.message;
      } finally {
        setBusy(testBtn, false);
      }
    });
    body.appendChild(qtEl("div", { class: "qt-test" }, [testBtn, testOut]));
  } else {
    // HTTP template test: run the real GET (httpTemplateToolExecutor) with
    // each parameter's example, against the REST connectors as currently
    // edited (connState.rest_connectors) so a not-yet-saved SAP/se16
    // connector can be exercised too — mirrors the SQL branch above.
    const testBtn = qtEl("button", { type: "button", class: "ghost-btn", text: "Vorlage testen" });
    testBtn.title = "Ruft diese Vorlage einmal live auf — mit den Beispielwerten der Parameter unten und den (noch nicht gespeicherten) REST-Connectoren — und zeigt die echte Antwort oder den Fehler an.";
    const testOut = qtEl("div", { class: "result", role: "status" });
    testBtn.addEventListener("click", async () => {
      testOut.className = "result";
      testOut.textContent = "Teste Vorlage …";
      setBusy(testBtn, true);
      try {
        const res = await api("/api/settings/test/http-template", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ template: t, rest_connectors: connState.rest_connectors || [] }),
        });
        testOut.className = res.ok ? "result" : "result error";
        testOut.textContent = (res.ok ? "✓ " : "✗ ") + res.detail;
      } catch (err) {
        testOut.className = "result error";
        testOut.textContent = err.message;
      } finally {
        setBusy(testBtn, false);
      }
    });
    body.appendChild(qtEl("div", { class: "qt-test" }, [testBtn, testOut]));
  }

  // Parameters
  const paramsHost = qtEl("div", { class: "qt-params" });
  const renderParams = () => {
    paramsHost.innerHTML = "";
    paramsHost.appendChild(qtEl("div", { class: "qt-params-title", text: "Parameter" }));
    (t.parameters || []).forEach((p, pi) => {
      const pName = bind(input(p.name, "param_name"), p, "name");
      const typeSel = qtEl("select");
      QT_PARAM_TYPES.forEach(o => { const op = qtEl("option", { value: o, text: o }); if ((p.type || "string") === o) op.selected = true; typeSel.appendChild(op); });
      typeSel.addEventListener("input", () => { p.type = typeSel.value; sync(); });
      const reqC = qtEl("input", { type: "checkbox" }); reqC.checked = !!p.required;
      reqC.addEventListener("input", () => { p.required = reqC.checked; sync(); });
      const pDesc = bind(input(p.description, "Beschreibung"), p, "description");
      const pEx = bind(input(p.example, "Beispielwert"), p, "example");
      const pOpts = qtEl("input");
      pOpts.value = (p.options || []).join(", ");
      pOpts.placeholder = "z. B. likp, vbak, kna1, mbew, mara";
      pOpts.addEventListener("input", () => {
        p.options = pOpts.value.split(",").map(s => s.trim()).filter(Boolean);
        sync();
      });
      const pRm = qtEl("button", { type: "button", class: "ghost-btn qt-remove", text: "×" });
      pRm.addEventListener("click", () => { t.parameters.splice(pi, 1); sync(); renderParams(); });
      paramsHost.appendChild(qtEl("div", { class: "qt-param-row" }, [
        qtField("Name", "Muss oben im SQL/URL als Platzhalter vorkommen.", pName),
        qtField("Typ", "Welche Art von Wert dieser Parameter annimmt: string (Text), integer (ganze Zahl), number (Zahl mit Dezimalstellen), boolean (ja/nein) oder date (Datum, Format JJJJ-MM-TT). R3 prüft und wandelt den vom Modell gelieferten Wert entsprechend um, bevor die Abfrage/URL damit ausgeführt wird — ein falsch gewählter Typ kann dazu führen, dass eine an sich gültige Modell-Eingabe abgelehnt wird.", typeSel),
        qtField("Pflicht", "Wenn angehakt, muss das Modell für diesen Parameter zwingend einen Wert mitliefern, sonst kann es das Werkzeug gar nicht erst aufrufen. Unangehakt lassen für optionale Filter, die die Abfrage auch ohne Angabe sinnvoll ausführen kann.", reqC),
        qtField("Beschreibung", "Sieht das Modell.", pDesc),
        qtField("Beispiel", "Sieht das Modell — Beispielwert für das Format.", pEx),
        qtField("Erlaubte Werte (optional, kommagetrennt)", "Schränkt diesen Parameter auf eine feste Werteliste ein — wird dem Modell als Auswahl (JSON-Schema-enum) angeboten UND serverseitig vor der Ausführung geprüft, unabhängig davon, was das Modell tatsächlich schickt. Leer = jeder Wert des gewählten Typs, wie bisher.", pOpts),
        pRm,
      ]));
    });
    const addP = qtEl("button", { type: "button", class: "ghost-btn", text: "+ Parameter" });
    addP.addEventListener("click", () => { t.parameters.push({ name: "", type: "string", required: false }); sync(); renderParams(); });
    paramsHost.appendChild(addP);
  };
  renderParams();

  return qtEl("div", { class: "qt-card" }, [header, body, paramsHost]);
}

function wireQTEditor(kind) {
  const cfg = QT_CFG[kind];
  const addId = kind === "sql" ? "#mssqlTemplateAdd" : "#httpTemplateAdd";
  const fromJsonId = kind === "sql" ? "#mssqlTemplateFromJson" : "#httpTemplateFromJson";
  document.querySelector(addId)?.addEventListener("click", () => {
    const arr = qtReadArray(cfg);
    const t = { name: "", description: "", enabled: true, parameters: [] };
    // A usable, safe starting point makes the common "one filtered lookup"
    // template quicker to create than an empty card, while still requiring
    // the admin to choose names/table/columns deliberately.
    t[cfg.bodyKey] = kind === "sql" ? "SELECT TOP 50 id, name FROM dbo.example WHERE id = {id}" : "";
    if (kind === "sql") t.parameters = [{ name: "id", type: "string", description: "ID des gesuchten Datensatzes", required: true, example: "4711" }];
    if (cfg.http) t.auth_source = "none";
    arr.push(t);
    qtSync(kind, arr);
    renderQTEditor(kind);
  });
  // "JSON übernehmen" re-renders the form from a hand-edited textarea.
  document.querySelector(fromJsonId)?.addEventListener("click", () => renderQTEditor(kind));
}
wireQTEditor("sql");
wireQTEditor("http");

// ---- Multi-connection connector editor (SharePoint/Exchange-Graph/IMAP/
// Teams/Confluence/Jira/Freshservice/Folder/OneDrive/GitHub/SAP S/4) ------
// Each of these connector types holds a *list* of named connections
// (connruntime.go) instead of one fixed config, so e.g. several Exchange
// mailboxes can be configured side by side. Reuses the same
// "list of N named things, add/remove per card, one array as the single
// source of truth, per-card live-test button" shape as the query-template
// editor above (QT_CFG/qtCard) — CONN_CFG declares each kind's field
// schema instead of just a body key/label, and each card's "Verbindung
// testen" POSTs the card's own (possibly not-yet-saved) object straight to
// that kind's existing /api/settings/test/<kind> endpoint (conntest.go),
// same contract the old static per-field test buttons used.
//
// connState[kind] is the live in-memory array the cards read from and
// mutate directly (no intermediate JSON textarea, unlike QT_CFG) —
// loadSettings deep-copies it in from the server, saveSettings sends it
// back as-is.
let connState = {};

const CONN_CFG = {
  sharepoint: {
    editor: "#s_sp_editor", addBtn: "#s_sp_add", testEndpoint: "/api/settings/test/sharepoint",
    discoverEndpoint: "/api/import/sharepoint/discover",
    namePh: "z. B. vertrieb-site",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "tenant_id", label: "Tenant-ID", ph: "z.B. 11111111-2222-3333-4444-555555555555", title: "Die Azure-AD-Tenant-ID (GUID) dieses Microsoft-365-Mandanten — im Azure-Portal unter 'Azure Active Directory' → Übersicht als 'Mandanten-ID' zu finden. Für alle Verbindungen desselben Unternehmens identisch." },
      { key: "client_id", label: "Client-ID (App-Registrierung)", title: "Die Anwendungs-ID (Client-ID) der für diese Verbindung angelegten Azure-AD-App-Registrierung — im Azure-Portal unter 'App-Registrierungen' → die betreffende App → Übersicht." },
      { key: "client_secret_env", label: "Client-Secret (env-Variable)", ph: "SHAREPOINT_CLIENT_SECRET", title: "Name der Umgebungsvariable, die den geheimen Client-Schlüssel dieser App-Registrierung enthält — empfohlen statt der direkten Eingabe rechts, damit der Schlüssel nicht im Klartext in settings.json landet." },
      { key: "client_secret", label: "Client-Secret (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Der geheime Client-Schlüssel im Klartext, nur zur schnellen lokalen Entwicklung gedacht. In einem geteilten Deployment stattdessen die env-Variable links nutzen." },
      { key: "site_url", label: "Site-URL", ph: "https://rubix.sharepoint.com/sites/Vertrieb", title: "Die vollständige Adresse der SharePoint-Site, deren Dokumentbibliothek importiert werden soll — nicht die URL eines einzelnen Dokuments/Ordners darin." },
      { key: "live_search_enabled", label: "Live-Suche für Chat/Agent/Mail freigeben (search_sharepoint-Tool — durchsucht diese Site live über Graph, unabhängig vom Import)", type: "checkbox", title: "Erlaubt dem Chat/Agent zusätzlich zum regulären Import eine sofortige Stichwortsuche direkt in dieser SharePoint-Site, unabhängig davon, ob/wann zuletzt importiert wurde — nützlich bei sehr aktuellen Dokumenten, kostet aber bei jeder Nutzung eine zusätzliche Anfrage an Microsoft Graph." },
    ],
    make: () => ({ name: "", enabled: true, tenant_id: "", client_id: "", client_secret: "", client_secret_env: "", site_url: "", sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0, live_search_enabled: false }),
  },
  onedrive: {
    editor: "#s_onedrive_editor", addBtn: "#s_onedrive_add", testEndpoint: "/api/settings/test/onedrive",
    namePh: "z. B. vertrieb-one-drive",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "tenant_id", label: "Tenant-ID", ph: "z.B. 11111111-2222-3333-4444-555555555555", title: "Azure-AD-Mandanten-ID der App-Registrierung." },
      { key: "client_id", label: "Client-ID (App-Registrierung)", title: "Anwendungs-ID der Azure-AD-App-Registrierung." },
      { key: "client_secret_env", label: "Client-Secret (env-Variable)", ph: "ONEDRIVE_CLIENT_SECRET", title: "Name der Umgebungsvariable mit dem geheimen Client-Schlüssel; empfohlen statt einer Klartextablage." },
      { key: "client_secret", label: "Client-Secret (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Nur für lokale Entwicklung; im Deployment die Umgebungsvariable verwenden." },
      { key: "drive_id", label: "Drive-ID", ph: "b!…", title: "Unveränderliche Microsoft-Graph-Drive-ID. Nicht den Namen, keine E-Mail-Adresse und nicht /me eintragen – die ID begrenzt, welchen Drive diese Verbindung anspricht." },
      { key: "folder_path", label: "Optionaler Ordnerpfad", ph: "z. B. Freigegebene Dokumente/Vertrieb", title: "Leer = gesamter Drive. Ein Pfad begrenzt die Delta-Synchronisierung auf diesen Unterordner." },
    ],
    make: () => ({ name: "", enabled: true, tenant_id: "", client_id: "", client_secret: "", client_secret_env: "", drive_id: "", folder_path: "", sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0 }),
  },
  exchange_graph: {
    editor: "#s_ex_editor", addBtn: "#s_ex_add", testEndpoint: "/api/settings/test/exchange",
    discoverEndpoint: "/api/import/exchange/discover",
    namePh: "z. B. vertrieb-postfach",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "tenant_id", label: "Tenant-ID", title: "Die Azure-AD-Tenant-ID (GUID) dieses Microsoft-365-Mandanten — im Azure-Portal unter 'Azure Active Directory' → Übersicht als 'Mandanten-ID' zu finden." },
      { key: "client_id", label: "Client-ID (App-Registrierung)", title: "Die Anwendungs-ID (Client-ID) der für diese Verbindung angelegten Azure-AD-App-Registrierung — im Azure-Portal unter 'App-Registrierungen' → die betreffende App → Übersicht. Kann dieselbe App-Registrierung wie SharePoint sein." },
      { key: "client_secret_env", label: "Client-Secret (env-Variable)", ph: "EXCHANGE_CLIENT_SECRET", title: "Name der Umgebungsvariable, die den geheimen Client-Schlüssel dieser App-Registrierung enthält — empfohlen statt der direkten Eingabe rechts, damit der Schlüssel nicht im Klartext in settings.json landet." },
      { key: "client_secret", label: "Client-Secret (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Der geheime Client-Schlüssel im Klartext, nur zur schnellen lokalen Entwicklung gedacht. In einem geteilten Deployment stattdessen die env-Variable links nutzen." },
      { key: "mailbox", label: "Postfach", ph: "vertrieb@rubix.com", title: "Die E-Mail-Adresse des Postfachs, das importiert werden soll — muss ein Postfach sein, auf das die App-Registrierung per 'Mail.Read'-Berechtigung Zugriff hat, nicht ein beliebiges privates Postfach ohne diese Freigabe." },
      { key: "folder", label: "Ordner", ph: "inbox", title: "Welcher Ordner des Postfachs importiert wird — 'inbox' für den normalen Posteingang, oder ein anderer Ordnername/eine Ordner-ID (siehe 'Struktur erkunden' unten)." },
    ],
    // Auto-draft controls (two opt-in gates + rule list, exchangeAutoDraftSection)
    // and the interactive per-user mailbox access controls
    // (exchangeInteractiveSection, mail_graph.go) render below the standard
    // fields, in that order.
    customSection: (item, touch, rerender) => {
      const wrap = qtEl("div", { class: "qt-card-extra" });
      wrap.appendChild(exchangeAutoDraftSection(item, touch, rerender));
      wrap.appendChild(exchangeInteractiveSection(item, touch, rerender));
      return wrap;
    },
    make: () => ({ name: "", enabled: true, tenant_id: "", client_id: "", client_secret: "", client_secret_env: "", mailbox: "", folder: "inbox", sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0, enable_draft_replies: false, enable_auto_draft_rules: false, auto_draft_rules: [], interactive_enabled: false, interactive_shared: false, allowed_users: [], allowed_groups: [] }),
  },
  imap: {
    editor: "#s_imap_editor", addBtn: "#s_imap_add", testEndpoint: "/api/settings/test/imap",
    namePh: "z. B. support-postfach",
    cycle: { key: "poll_interval_seconds", label: "Auto-Abruf-Intervall (Sekunden)", ph: "0 = kein Auto-Abruf" },
    fields: [
      { key: "host", label: "Server", ph: "mail.rubix.com", title: "Adresse des Mailservers — beim E-Mail-Anbieter oder der eigenen IT erfragen, falls unbekannt." },
      { key: "port", label: "Port", type: "number", ph: "993", title: "Port des IMAP-Servers — 993 für IMAPS (von Anfang an verschlüsselt, Standard bei den meisten Anbietern), 143 für unverschlüsseltes IMAP mit optionalem STARTTLS." },
      { key: "use_tls", label: "TLS verwenden", type: "checkbox", title: "Verbindung von Anfang an verschlüsselt aufbauen (TLS/SSL). Bei Port 993 fast immer aktiviert lassen; bei Port 143 nur deaktivieren, wenn der Server kein STARTTLS anbietet." },
      { key: "username", label: "Benutzername", title: "Anmeldename für das Postfach — oft die volle E-Mail-Adresse, je nach Mailserver aber auch ein separater Login-Name." },
      { key: "password_env", label: "Passwort (env-Variable)", ph: "IMAP_PASSWORD", title: "Name der Umgebungsvariable mit dem Postfach-Passwort — empfohlen statt direkter Eingabe rechts, damit das Passwort nicht im Klartext in settings.json landet." },
      { key: "password", label: "Passwort (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Das Passwort im Klartext, nur zur schnellen lokalen Entwicklung gedacht. In einem geteilten Deployment stattdessen die env-Variable links nutzen." },
      { key: "mailbox", label: "Postfach/Ordner", ph: "INBOX", title: "Welcher Ordner des Postfachs abgerufen wird — meist 'INBOX' für den normalen Posteingang." },
      { key: "drafts_mailbox", label: "Entwürfe-Ordner", ph: "Drafts", title: "Name des Entwürfe-Ordners dieses Postfachs, z. B. 'Drafts' oder 'Entwürfe' — wird nur gebraucht, wenn ein Antwortentwurf direkt im Postfach abgelegt werden soll, statt ihn nur herunterzuladen/zu kopieren." },
    ],
    make: () => ({ name: "", enabled: true, host: "", port: 993, use_tls: true, username: "", password: "", password_env: "", mailbox: "INBOX", poll_interval_seconds: 0, drafts_mailbox: "", max_items_per_run: 0, timeout_seconds: 0 }),
  },
  teams: {
    editor: "#s_teams_editor", addBtn: "#s_teams_add", testEndpoint: "/api/settings/test/teams",
    namePh: "z. B. it-support-kanal",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "tenant_id", label: "Tenant-ID", title: "Die Azure-AD-Tenant-ID (GUID) dieses Microsoft-365-Mandanten — im Azure-Portal unter 'Azure Active Directory' → Übersicht als 'Mandanten-ID' zu finden." },
      { key: "client_id", label: "Client-ID (App-Registrierung)", title: "Die Anwendungs-ID (Client-ID) der für diese Verbindung angelegten Azure-AD-App-Registrierung — im Azure-Portal unter 'App-Registrierungen' → die betreffende App → Übersicht." },
      { key: "client_secret_env", label: "Client-Secret (env-Variable)", ph: "TEAMS_CLIENT_SECRET", title: "Name der Umgebungsvariable, die den geheimen Client-Schlüssel dieser App-Registrierung enthält — empfohlen statt der direkten Eingabe rechts, damit der Schlüssel nicht im Klartext in settings.json landet." },
      { key: "client_secret", label: "Client-Secret (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Der geheime Client-Schlüssel im Klartext, nur zur schnellen lokalen Entwicklung gedacht. In einem geteilten Deployment stattdessen die env-Variable links nutzen." },
      { key: "team_id", label: "Team-ID", title: "Die Team-ID (GUID) — aus der Teams-Web-URL des Kanals ablesen: 'Kanal-Link kopieren' in Teams liefert eine URL, in der die Team-ID im groupId-Parameter steht (siehe Erklärung oben im Hinweistext)." },
      { key: "channel_id", label: "Kanal-ID", title: "Die Kanal-ID — ebenfalls aus dem kopierten Kanal-Link ablesen: der Teil vor %40thread.tacv2 inklusive des vorangestellten 19%3a (siehe Erklärung oben im Hinweistext)." },
    ],
    make: () => ({ name: "", enabled: true, tenant_id: "", client_id: "", client_secret: "", client_secret_env: "", team_id: "", channel_id: "", sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0 }),
  },
  confluence: {
    editor: "#s_conf_editor", addBtn: "#s_conf_add", testEndpoint: "/api/settings/test/confluence",
    namePh: "z. B. vertrieb-space",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "base_url", label: "Basis-URL", ph: "https://rubix.atlassian.net/wiki", title: "Die Basis-Adresse des Confluence-Wikis — bei Atlassian Cloud endet sie auf '/wiki'." },
      { key: "email", label: "E-Mail", title: "Die E-Mail-Adresse des Atlassian-Kontos, mit dem R3 auf Confluence zugreift." },
      { key: "api_token_env", label: "API-Token (env-Variable)", ph: "CONFLUENCE_API_TOKEN", title: "Name der Umgebungsvariable mit dem Atlassian-API-Token dieses Kontos (Atlassian-Konto → Sicherheit → API-Tokens) — empfohlen statt direkter Eingabe rechts, damit das Token nicht im Klartext in settings.json landet." },
      { key: "api_token", label: "API-Token (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Das API-Token im Klartext, nur zur schnellen lokalen Entwicklung gedacht. In einem geteilten Deployment stattdessen die env-Variable links nutzen." },
      { key: "space_key", label: "Space-Key", ph: "VERTRIEB", title: "Kürzel des zu importierenden Confluence-Space, z. B. 'VERTRIEB' — in der Confluence-URL des Space oder unter dessen Space-Einstellungen zu finden." },
    ],
    make: () => ({ name: "", enabled: true, base_url: "", email: "", api_token: "", api_token_env: "", space_key: "", sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0 }),
  },
  jira: {
    editor: "#s_jira_editor", addBtn: "#s_jira_add", testEndpoint: "/api/settings/test/jira",
    namePh: "z. B. ops-projekt",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "base_url", label: "Basis-URL", ph: "https://rubix.atlassian.net", title: "Die Basis-Adresse der Jira-Instanz." },
      { key: "email", label: "E-Mail", title: "Die E-Mail-Adresse des Atlassian-Kontos, mit dem R3 auf Jira zugreift." },
      { key: "api_token_env", label: "API-Token (env-Variable)", ph: "JIRA_API_TOKEN", title: "Name der Umgebungsvariable mit dem Atlassian-API-Token dieses Kontos (Atlassian-Konto → Sicherheit → API-Tokens) — derselbe Token wie für Confluence funktioniert, falls der Account beides lesen darf. Empfohlen statt direkter Eingabe rechts, damit das Token nicht im Klartext in settings.json landet." },
      { key: "api_token", label: "API-Token (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Das API-Token im Klartext, nur zur schnellen lokalen Entwicklung gedacht. In einem geteilten Deployment stattdessen die env-Variable links nutzen." },
      { key: "project_key", label: "Projekt-Key", ph: "OPS", title: "Kürzel des zu importierenden Jira-Projekts, z. B. 'OPS' — steht vor jeder Ticketnummer dieses Projekts, z. B. OPS-123." },
    ],
    make: () => ({ name: "", enabled: true, base_url: "", email: "", api_token: "", api_token_env: "", project_key: "", sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0 }),
  },
  freshservice: {
    editor: "#s_fs_editor", addBtn: "#s_fs_add", testEndpoint: "/api/settings/test/freshservice",
    namePh: "z. B. haupt-instanz",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "base_url", label: "Basis-URL", ph: "https://rubix.freshservice.com", title: "Die Basis-Adresse der Freshservice-Instanz." },
      { key: "api_key_env", label: "API-Key (env-Variable)", ph: "FRESHSERVICE_API_KEY", title: "Name der Umgebungsvariable mit dem Freshservice-API-Key (im Freshservice-Profil unter 'API-Key' zu finden) — empfohlen statt direkter Eingabe rechts, damit der Schlüssel nicht im Klartext in settings.json landet." },
      { key: "api_key", label: "API-Key (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Der API-Key im Klartext, nur zur schnellen lokalen Entwicklung gedacht. In einem geteilten Deployment stattdessen die env-Variable links nutzen." },
    ],
    make: () => ({ name: "", enabled: true, base_url: "", api_key: "", api_key_env: "", sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0 }),
  },
  github: {
    editor: "#s_github_editor", addBtn: "#s_github_add", testEndpoint: "/api/settings/test/github",
    namePh: "z. B. produkt-dokumentation",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "base_url", label: "GitHub API-Basis-URL", ph: "https://api.github.com", title: "Für GitHub.com leer lassen oder https://api.github.com setzen. Für GitHub Enterprise z. B. https://github.firma.de/api/v3. Ausschließlich HTTPS." },
      { key: "owner", label: "Organisation / Owner", ph: "rubix", title: "GitHub-Organisation oder Benutzer, dem das Repository gehört." },
      { key: "repository", label: "Repository", ph: "handbuch", title: "Repository-Name ohne Owner-Präfix." },
      { key: "token_env", label: "Token (env-Variable)", ph: "GITHUB_RAG_TOKEN", title: "Feingranularer GitHub-Token mit rein lesendem Zugriff auf genau dieses Repository. Als Umgebungsvariable hinterlegen." },
      { key: "token", label: "Token (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Nur für lokale Entwicklung; im Deployment die Umgebungsvariable verwenden." },
      { key: "include_readme", label: "README importieren", type: "checkbox", title: "Importiert die zentrale README-Datei als eigene Quelle." },
      { key: "include_issues", label: "Issues importieren", type: "checkbox", title: "Importiert offene und geschlossene Issues." },
      { key: "include_pull_requests", label: "Pull Requests importieren", type: "checkbox", title: "Importiert offene und geschlossene Pull Requests." },
    ],
    make: () => ({ name: "", enabled: true, base_url: "https://api.github.com", owner: "", repository: "", token: "", token_env: "", include_readme: true, include_issues: true, include_pull_requests: true, sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0 }),
  },
  sap_s4: {
    editor: "#s_sap_s4_editor", addBtn: "#s_sap_s4_add", testEndpoint: "/api/settings/test/sap-s4",
    namePh: "z. B. business-partner",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "base_url", label: "SAP-Basis-URL", ph: "https://s4.firma.de", title: "HTTPS-Basisadresse des S/4-Systems bzw. API-Gateways. Interne Zertifikate müssen von der Laufzeit als vertrauenswürdig erkannt werden." },
      { key: "auth_type", label: "Authentifizierung", type: "select", options: [["basic", "Basic (Benutzer/Passwort)"], ["bearer", "Bearer-Token"], ["header", "Header / API-Key"], ["none", "keine"]], title: "Wähle ausschließlich die Authentifizierung, die der freigegebene OData-Service verlangt." },
      { key: "username", label: "Benutzername (bei Basic)", ph: "R3_ODATA", title: "Technischer, nur lesender SAP-Benutzer. Nur bei Basic-Auth relevant." },
      { key: "password_env", label: "Passwort (env-Variable)", ph: "SAP_S4_PASSWORD", title: "Umgebungsvariable mit dem Basic-Passwort; statt Klartext in settings.json verwenden." },
      { key: "password", label: "Passwort (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Nur für lokale Entwicklung." },
      { key: "token_env", label: "Token (env-Variable)", ph: "SAP_S4_TOKEN", title: "Für Bearer- oder Header-Authentifizierung." },
      { key: "token", label: "Token (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Nur für lokale Entwicklung." },
      { key: "header_name", label: "Header-Name (bei Header-Auth)", ph: "X-API-Key", title: "Leer verwendet Authorization. Nur bei Header-Auth relevant." },
      { key: "headers", label: "Zusätzliche Header (JSON, optional)", type: "jsontext", ph: '{ "sap-client": "100" }', title: "Nicht geheime, statische Zusatzheader, z. B. sap-client. Authorization wird aus Sicherheitsgründen ignoriert und kommt nur aus den Zugangsdaten oben." },
      { key: "entity_path", label: "OData-Entity-Pfad", ph: "/sap/opu/odata/sap/API_BUSINESS_PARTNER/A_BusinessPartner", title: "Relativer Entity-Set-Pfad ohne Query-Parameter. R3 sendet nur GET und baut $select/$top selbst." },
      { key: "id_field", label: "ID-Feld", ph: "BusinessPartner", title: "Ein eindeutiges, einfaches OData-Feld; dient als stabile R3-Quell-ID." },
      { key: "title_field", label: "Titel-Feld (optional)", ph: "OrganizationBPName1", title: "Einfaches Feld für die Quellenbezeichnung; leer = ID." },
      { key: "content_fields", label: "Inhaltsfelder (kommasepariert)", type: "csv", ph: "OrganizationBPName1, CityName, Country", title: "Nur diese Felder werden per $select abgerufen und in die Wissensbasis geschrieben. Mindestens eines erforderlich." },
      { key: "updated_at_field", label: "Aktualisiert-am-Feld (optional)", ph: "LastChangeDateTime", title: "Wird als Dokumentdatum angezeigt, sofern es RFC3339/ISO-8601 enthält." },
    ],
    make: () => ({ name: "", enabled: true, base_url: "", auth_type: "basic", username: "", password: "", password_env: "", token: "", token_env: "", header_name: "", headers: {}, entity_path: "", id_field: "", title_field: "", content_fields: [], updated_at_field: "", sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0 }),
  },
  folder: {
    editor: "#s_folder_editor", addBtn: "#s_folder_add", testEndpoint: "/api/settings/test/folder",
    discoverEndpoint: "/api/import/folder/discover",
    namePh: "z. B. vertrieb-freigabe",
    cycle: { key: "sync_interval_minutes", label: "Auto-Sync-Intervall (Minuten)", ph: "0 = kein Auto-Sync" },
    fields: [
      { key: "path", label: "Ordnerpfad auf dem Server", ph: "z.B. C:\\Freigaben\\Vertrieb", title: "Vollständiger Pfad zu dem Ordner, der (rekursiv, inkl. Unterordner) importiert werden soll. Muss für den R3-Serverprozess selbst lesbar sein — ein lokaler Pfad auf demselben Rechner, oder eine dort bereits eingebundene Netzwerkfreigabe. R3 bindet selbst keine Netzlaufwerke ein." },
    ],
    make: () => ({ name: "", enabled: true, path: "", sync_interval_minutes: 0, max_items_per_run: 0, timeout_seconds: 0 }),
  },
  // Generic REST connector (settings.go's restConnectorConfig): a live-only
  // backend HTTP query templates borrow credentials + a pinned host from via
  // their auth_source. liveOnly hides the import runtime knobs; noTest hides
  // the standalone connection probe (there's no generic endpoint to hit — the
  // per-template "Vorlage testen" button is the real test). The kind key is
  // "rest_connectors" so loadSettings' generic connState loop maps it straight
  // onto s.rest_connectors.
  rest_connectors: {
    editor: "#s_rest_editor", addBtn: "#s_rest_add",
    liveOnly: true, noTest: true,
    namePh: "z. B. sap-logistik",
    fields: [
      { key: "base_url", label: "Basis-URL", ph: "https://logistic.rubix-intern.de", title: "Nur dieser Host darf von Vorlagen angesprochen werden, die diesen Connector nutzen (SSRF-Schutz). Muss https sein. Die Vorlage trägt die volle URL — dies fixiert nur den erlaubten Host." },
      { key: "auth_type", label: "Authentifizierung", type: "select", title: "Wie sich R3 gegenüber diesem System authentifiziert.", options: [["none", "keine"], ["basic", "Basic (Benutzer/Passwort)"], ["bearer", "Bearer-Token"], ["header", "Header / API-Key"]] },
      { key: "username", label: "Benutzername (bei Basic)", ph: "nur bei Basic-Auth", title: "Nur relevant, wenn oben bei Authentifizierung 'Basic (Benutzer/Passwort)' gewählt ist — wird als HTTP-Basic-Anmeldedaten mitgeschickt. Bei jeder anderen Authentifizierungs-Art wirkungslos." },
      { key: "password_env", label: "Passwort (env-Variable, bei Basic)", ph: "z. B. SAP_SE16_PASSWORD", title: "Name der Umgebungsvariable mit dem Basic-Auth-Passwort — empfohlen statt direkter Eingabe rechts, damit das Passwort nicht im Klartext in settings.json landet. Nur relevant bei Authentifizierung 'Basic'." },
      { key: "password", label: "Passwort (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Das Passwort im Klartext, nur zur schnellen lokalen Entwicklung gedacht. In einem geteilten Deployment stattdessen die env-Variable links nutzen." },
      { key: "token_env", label: "Token (env-Variable, bei Bearer/Header)", ph: "z. B. SAP_SE16_TOKEN", title: "Name der Umgebungsvariable mit dem Token — je nach gewählter Authentifizierung entweder als Bearer-Token oder im unten benannten Header verschickt. Empfohlen statt direkter Eingabe rechts. Nur relevant bei Authentifizierung 'Bearer-Token' oder 'Header / API-Key'." },
      { key: "token", label: "Token (direkt, nur Dev)", type: "password", ph: "optional, besser: env-Variable", title: "Das Token im Klartext, nur zur schnellen lokalen Entwicklung gedacht. In einem geteilten Deployment stattdessen die env-Variable links nutzen." },
      { key: "header_name", label: "Header-Name (bei Header-Auth)", ph: "z. B. X-API-Key", title: "Der Name des HTTP-Headers, in den der Token-Wert eingetragen wird (z. B. X-API-Key, apikey, Ocp-Apim-Subscription-Key). Leer = 'Authorization' wird verwendet. Nur relevant bei Authentifizierung 'Header / API-Key'." },
      { key: "headers", label: "Zusätzliche Header (JSON, optional)", type: "jsontext", ph: '{ "X-System": "ZITEC" }', title: "Statische Header bei jedem Aufruf. KEINE Geheimnisse hier ablegen — Header werden im Browser im Klartext angezeigt; Zugangsdaten gehören in Passwort/Token oben." },
    ],
    customSection: (item, touch) => accessControlSection(item, touch),
    make: () => ({ name: "", enabled: true, base_url: "", auth_type: "none", username: "", password: "", password_env: "", token: "", token_env: "", header_name: "", headers: {}, access_control: {} }),
  },
};

// connSecretFieldKeys returns the field keys cfg declares as type:"password"
// — the direct-secret inputs (client_secret, password, token, api_key,
// api_token; NOT their *_env siblings, which merely name an environment
// variable and carry no secret themselves) — the single generic rule
// connCardExport uses so no per-kind field-name list needs maintaining
// separately from CONN_CFG's own field definitions.
function connSecretFieldKeys(cfg) {
  return cfg.fields.filter(f => f.type === "password").map(f => f.key);
}

// connCardExport downloads item as a standalone .json file: every field
// EXCEPT direct-secret ones (connSecretFieldKeys — password/token/
// client_secret/... fields are blanked; their *_env counterparts, which
// only ever hold an environment-variable NAME, are kept), plus a
// _r3_connector_kind marker connCardImport checks on the way back in so an
// export from one connector kind can't be imported into a differently-
// shaped card by mistake.
function connCardExport(kind, cfg, item) {
  const out = JSON.parse(JSON.stringify(item));
  connSecretFieldKeys(cfg).forEach(k => { if (k in out) out[k] = ""; });
  out._r3_connector_kind = kind;
  const blob = new Blob([JSON.stringify(out, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = qtEl("a", { href: url, download: `r3-${kind}-${(item.name || "verbindung").replace(/[^a-zA-Z0-9_-]+/g, "-")}.json` });
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// connCardImport opens the OS file picker for a single .json file
// (previously produced by connCardExport, or hand-written in the same
// shape) and overwrites every one of item's own keys from it in place —
// the imported file's _r3_connector_kind must match kind, or the import is
// refused outright rather than silently populating fields that don't exist
// on this connector kind's own field list.
function connCardImport(kind, item, touch, rerender) {
  const fileInput = qtEl("input", { type: "file", accept: "application/json,.json" });
  fileInput.style.display = "none";
  fileInput.addEventListener("change", async () => {
    const file = fileInput.files && fileInput.files[0];
    fileInput.remove();
    if (!file) return;
    try {
      const parsed = JSON.parse(await file.text());
      if (parsed._r3_connector_kind && parsed._r3_connector_kind !== kind) {
        NovaPop.toast({ type: "error", message: `Import abgelehnt: Datei stammt von einem anderen Verbindungstyp (${parsed._r3_connector_kind}).` });
        return;
      }
      delete parsed._r3_connector_kind;
      Object.assign(item, parsed);
      touch();
      rerender();
      NovaPop.toast({ type: "success", message: "Verbindung importiert." });
    } catch (err) {
      NovaPop.toast({ type: "error", message: "Import fehlgeschlagen: " + err.message });
    }
  });
  document.body.appendChild(fileInput);
  fileInput.click();
}

// connCard builds one connection's editable card: a header (Name, aktiv,
// and a "⋮" menu — Entfernen/Exportieren/Importieren/Duplizieren/Testen,
// see connCardMenu), the kind-specific fields, the three shared runtime
// knobs (Zyklus/Limit/Timeout, connruntime.go), and (for testable kinds) a
// result area the menu's "Verbindung testen" writes into — mirrors qtCard's
// shape above, but binds straight to the object in connState[kind]
// (mutating it in place) instead of round-tripping through a JSON
// textarea, since there's no equivalent "raw JSON escape hatch" for these
// yet.
function connCard(kind, arr, item, idx) {
  const cfg = CONN_CFG[kind];
  const rerender = () => renderConnEditor(kind);
  const touch = () => { if (typeof markSettingsDirty === "function") markSettingsDirty(true); };

  const bind = (control, key, isCheckbox, isNumber) => {
    control.addEventListener("input", () => {
      item[key] = isCheckbox ? control.checked : isNumber ? (parseInt(control.value, 10) || 0) : control.value;
      touch();
    });
    return control;
  };
  const input = (val, ph, type) => {
    const i = qtEl("input", type ? { type } : null);
    i.value = val == null ? "" : val;
    if (ph) i.placeholder = ph;
    return i;
  };
  const checkbox = (val) => { const c = qtEl("input", { type: "checkbox" }); c.checked = !!val; return c; };

  const nameI = bind(input(item.name, cfg.namePh), "name");
  const enabledC = bind(checkbox(item.enabled), "enabled", true);

  const body = qtEl("div", { class: "qt-card-body" });
  cfg.fields.forEach(f => {
    let ctl;
    if (f.type === "checkbox") {
      ctl = bind(checkbox(item[f.key]), f.key, true);
    } else if (f.type === "select") {
      // A fixed-choice dropdown (e.g. a REST connector's auth_type). Options
      // are [value, label] pairs; the first is the default when unset.
      ctl = qtEl("select");
      (f.options || []).forEach(([v, l]) => {
        const op = qtEl("option", { value: v, text: l });
        if ((item[f.key] || (f.options[0] && f.options[0][0])) === v) op.selected = true;
        ctl.appendChild(op);
      });
      ctl.addEventListener("input", () => { item[f.key] = ctl.value; touch(); });
    } else if (f.type === "jsontext") {
      // A small object field edited as JSON (e.g. static extra headers). The
      // in-memory item[f.key] stays an object; invalid JSON marks the field
      // and simply isn't committed until valid, so a half-typed entry never
      // corrupts the saved value.
      ctl = qtEl("textarea", { class: "prompt-editor", rows: "2" });
      ctl.value = item[f.key] && Object.keys(item[f.key]).length ? JSON.stringify(item[f.key], null, 2) : "";
      if (f.ph) ctl.placeholder = f.ph;
      ctl.addEventListener("input", () => {
        const raw = ctl.value.trim();
        if (raw === "") { item[f.key] = {}; ctl.classList.remove("input-error"); touch(); return; }
        try {
          const parsed = JSON.parse(raw);
          if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
            item[f.key] = parsed; ctl.classList.remove("input-error"); touch();
          } else { ctl.classList.add("input-error"); }
        } catch { ctl.classList.add("input-error"); }
      });
    } else if (f.type === "csv") {
      ctl = input(Array.isArray(item[f.key]) ? item[f.key].join(", ") : item[f.key], f.ph);
      ctl.addEventListener("input", () => {
        item[f.key] = ctl.value.split(",").map(v => v.trim()).filter(Boolean);
        touch();
      });
    } else {
      ctl = bind(input(item[f.key], f.ph, f.type === "password" ? "password" : f.type === "number" ? "number" : undefined), f.key, false, f.type === "number");
    }
    body.appendChild(qtField(f.label, f.title || "", ctl));
  });

  // Runtime knobs (Zyklus/Limit/Timeout) only make sense for IMPORT
  // connectors that run scheduled jobs — a live REST connector (liveOnly) is
  // queried on demand at answer time and has none of them.
  if (!cfg.liveOnly) {
    body.appendChild(qtField(cfg.cycle.label, "0 = kein automatischer Lauf, nur manuell im Import-Tab. Die letzten Auto-Sync-Läufe erscheinen im Panel unter Freshservice.", bind(input(item[cfg.cycle.key], cfg.cycle.ph, "number"), cfg.cycle.key, false, true)));
    body.appendChild(qtField("Limit (Elemente pro Lauf)", "Überschreibt für diese Verbindung das globale Limit unter Import → Drosselung & Limits. 0 = globaler Standard.", bind(input(item.max_items_per_run, "0 = globaler Standard", "number"), "max_items_per_run", false, true)));
    body.appendChild(qtField("Timeout (Sekunden)", "Wie lange ein einzelner Lauf dieser Verbindung höchstens dauern darf, bevor er abgebrochen wird. 0 = eingebauter Standard.", bind(input(item.timeout_seconds, "0 = Standard", "number"), "timeout_seconds", false, true)));
  }

  // Connector-specific extra block (currently only exchange_graph's
  // auto-draft controls) — rendered after the shared fields, before the
  // test/discover buttons.
  if (typeof cfg.customSection === "function") {
    const extra = cfg.customSection(item, touch, rerender);
    if (extra) body.appendChild(extra);
  }

  // A live REST connector (noTest) has no standalone connection probe — it's
  // only meaningful in the context of an HTTP query template's URL, so the
  // per-template "Vorlage testen" button (qtCard) is where it's exercised.
  // The trigger itself lives in the header's "⋮" menu below (runTest);
  // testOut just stays here in the body as the result area, closer to the
  // fields it's reporting on than the header would be.
  let testOut = null;
  let testing = false;
  const runTest = async () => {
    if (!testOut || testing) return;
    testing = true;
    testOut.className = "result";
    testOut.textContent = "Teste Verbindung …";
    try {
      const res = await api(cfg.testEndpoint, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(item) });
      testOut.className = res.ok ? "result" : "result error";
      testOut.textContent = (res.ok ? "✓ " : "✗ ") + res.detail;
      NovaPop.toast({ type: res.ok ? "success" : "error", message: res.detail });
    } catch (err) {
      testOut.className = "result error";
      testOut.textContent = err.message;
      NovaPop.toast({ type: "error", message: err.message });
    } finally {
      testing = false;
    }
  };
  if (!cfg.noTest) {
    testOut = qtEl("div", { class: "result", role: "status" });
    body.appendChild(qtEl("div", { class: "qt-test" }, [testOut]));
  }

  // Discover: recursive, read-only structure preview — only for the
  // connector kinds that declare cfg.discoverEndpoint (sharepoint, folder,
  // exchange_graph). Same "POST the current unsaved item straight to the
  // endpoint" contract as the test above; kept as its own standalone
  // button (not folded into the "⋮" menu) since its result is a whole tree
  // view rendered inline, not a one-line status.
  if (cfg.discoverEndpoint) {
    const discBtn = qtEl("button", { type: "button", class: "ghost-btn", text: "Struktur erkunden" });
    discBtn.title = "Zeigt rekursiv, wie die Ordner-/Postfach-Struktur der Gegenstelle aussieht (mit den obigen, noch nicht gespeicherten Werten) — hilft zu entscheiden, was importiert werden soll, importiert selbst aber nichts.";
    const discOut = qtEl("div", { class: "discover-tree" });
    discBtn.addEventListener("click", async () => {
      setBusy(discBtn, true);
      discOut.innerHTML = "";
      try {
        const res = await api(cfg.discoverEndpoint, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(item) });
        discOut.appendChild(renderDiscoverNode(res, true));
      } catch (err) {
        discOut.appendChild(qtEl("div", { class: "result error", text: err.message }));
      } finally {
        setBusy(discBtn, false);
      }
    });
    body.appendChild(qtEl("div", { class: "qt-test" }, [discBtn]));
    body.appendChild(discOut);
  }

  // Header menu (connCardMenu): built last so it can reference runTest —
  // Entfernen stays danger-colored, matching the old standalone button;
  // Testen is omitted entirely for a noTest kind (there is nothing for it
  // to run), same condition the old standalone button used.
  const menuActions = [];
  if (!cfg.noTest) menuActions.push({ label: "Verbindung testen", onClick: runTest });
  menuActions.push(
    { label: "Verbindung duplizieren", onClick: () => {
      const copy = JSON.parse(JSON.stringify(item));
      copy.enabled = false;
      arr.splice(idx + 1, 0, copy);
      touch();
      rerender();
      NovaPop.toast({ type: "success", message: "Verbindung dupliziert." });
    } },
    { label: "Verbindung exportieren", onClick: () => {
      connCardExport(kind, cfg, item);
      NovaPop.toast({ type: "success", message: "Verbindung exportiert." });
    } },
    { label: "Verbindung importieren", onClick: () => connCardImport(kind, item, touch, rerender) },
    { label: "Verbindung entfernen", danger: true, onClick: () => {
      arr.splice(idx, 1);
      touch();
      rerender();
      NovaPop.toast({ type: "success", message: "Verbindung entfernt." });
    } },
  );

  const header = qtEl("div", { class: "qt-card-head" }, [
    qtField("Name", "Eindeutiger Name dieser Verbindung — nur intern (Zuordnung im Import-Tab, Scheduler-Log), nicht bei der Gegenstelle sichtbar.", nameI),
    qtField("aktiv", "Nur aktive Verbindungen werden im Import-Tab angeboten bzw. automatisch synchronisiert.", enabledC),
    connCardMenu(menuActions),
  ]);

  return qtEl("div", { class: "qt-card" }, [header, body]);
}

// exchangeAutoDraftSection renders the per-mailbox auto-draft controls for an
// Exchange (Graph) connection card: the two opt-in gates (which mirror
// exchangeGraphConfig.EnableDraftReplies / EnableAutoDraftRules, autodraft.go)
// and the rule list the unattended draft engine matches against. It mutates
// the item in place — the same contract connCard's own fields use — so the
// values round-trip through load/saveSettings with no extra wiring, and the
// item's auto_drafted_ids dedup cursor is preserved untouched. HARD invariant,
// restated in the UI copy: this only ever CREATES a draft; R3 has no send path.
function exchangeAutoDraftSection(item, touch, rerender) {
  const wrap = qtEl("div", { class: "conn-subsection autodraft-section" });
  wrap.appendChild(qtEl("h4", { text: "Automatische Antwort-Entwürfe" }));
  wrap.appendChild(qtEl("p", { class: "hint", text: "Nur Entwürfe: R3 legt Vorschläge im Entwürfe-Ordner ab und versendet nie etwas — ein Mensch prüft und sendet jeden Entwurf selbst. Beide Schalter sind standardmäßig aus." }));

  const draftC = qtEl("input", { type: "checkbox" });
  draftC.checked = !!item.enable_draft_replies;
  draftC.addEventListener("input", () => { item.enable_draft_replies = draftC.checked; touch(); });
  wrap.appendChild(qtField("Entwürfe für dieses Postfach erlauben", "Erlaubt R3, für dieses Postfach per Microsoft Graph einen Antwort-Entwurf im Entwürfe-Ordner abzulegen. Ohne diesen Haken bleibt die Verbindung rein lesend (nur Import).", draftC));

  const autoC = qtEl("input", { type: "checkbox" });
  autoC.checked = !!item.enable_auto_draft_rules;
  autoC.addEventListener("input", () => { item.enable_auto_draft_rules = autoC.checked; touch(); });
  wrap.appendChild(qtField("Regel-gesteuert automatisch entwerfen", "Prüft bei jedem Auto-Sync neue Nachrichten gegen die Regeln unten und legt bei einem Treffer selbstständig einen Entwurf an. Braucht zusätzlich den Haken darüber; ohne passende Regel passiert nichts.", autoC));

  const rulesBox = qtEl("div", { class: "autodraft-rules" });
  const rules = Array.isArray(item.auto_draft_rules) ? item.auto_draft_rules : (item.auto_draft_rules = []);
  if (!rules.length) {
    rulesBox.appendChild(qtEl("p", { class: "hint", text: "Noch keine Regeln — ohne mindestens eine (aktive, passende) Regel wird nie automatisch entworfen." }));
  }
  rules.forEach((rule, ri) => {
    const fieldSel = qtEl("select");
    [["from", "Absender"], ["subject", "Betreff"]].forEach(([v, l]) => {
      const o = qtEl("option", { value: v, text: l });
      if ((rule.pattern_field || "from") === v) o.selected = true;
      fieldSel.appendChild(o);
    });
    fieldSel.addEventListener("input", () => { rule.pattern_field = fieldSel.value; touch(); });
    const patI = qtEl("input");
    patI.value = rule.pattern || "";
    patI.placeholder = "z. B. rubix.com";
    patI.addEventListener("input", () => { rule.pattern = patI.value; touch(); });
    const negC = qtEl("input", { type: "checkbox" });
    negC.checked = !!rule.negate;
    negC.addEventListener("input", () => { rule.negate = negC.checked; touch(); });
    const enC = qtEl("input", { type: "checkbox" });
    enC.checked = rule.enabled !== false;
    enC.addEventListener("input", () => { rule.enabled = enC.checked; touch(); });
    const rm = qtEl("button", { type: "button", class: "ghost-btn qt-remove", text: "Entfernen" });
    rm.addEventListener("click", () => { rules.splice(ri, 1); touch(); rerender(); });
    rulesBox.appendChild(qtEl("div", { class: "autodraft-rule" }, [
      qtField("Feld", "Welcher Teil der Nachricht geprüft wird.", fieldSel),
      qtField("enthält", "Text, der (ohne Groß-/Kleinschreibung) im gewählten Feld vorkommen muss.", patI),
      qtField("negieren", "Trifft zu, wenn der Text NICHT vorkommt — z. B. Absender NICHT von rubix.com (externe Anfragen).", negC),
      qtField("aktiv", "Regel aktiv? Deaktivieren lässt sie stehen, ohne sie zu löschen.", enC),
      rm,
    ]));
  });
  wrap.appendChild(rulesBox);
  const add = qtEl("button", { type: "button", class: "ghost-btn", text: "+ Regel hinzufügen" });
  add.addEventListener("click", () => { rules.push({ pattern_field: "from", pattern: "", negate: false, enabled: true }); touch(); rerender(); });
  wrap.appendChild(add);
  return wrap;
}

// exchangeInteractiveSection renders the per-user interactive-mailbox-
// access controls for an Exchange (Graph) connection card: the opt-in gate
// (exchangeGraphConfig.InteractiveEnabled) and the allow-list of AD
// accounts (email or login name, one per line — same textarea/one-per-line
// convention as ldapConfig.AdminUsers) permitted to browse and draft-reply
// to THEIR OWN mailbox through this connection in the Mail tab
// (mail_graph.go) — entirely independent of the fixed Mailbox/Folder above,
// which stays the admin's own import/auto-draft target. A user not listed
// here keeps the pre-existing manual copy-paste Mail-tab workflow,
// unaffected.
function exchangeInteractiveSection(item, touch, rerender) {
  const wrap = qtEl("div", { class: "conn-subsection" });
  wrap.appendChild(qtEl("h4", { text: "Interaktiver Postfach-Zugriff (Mail-Tab)" }));
  wrap.appendChild(qtEl("p", { class: "hint", text: "Lässt ausgewählte Benutzer im Mail-Tab ihr EIGENES Postfach durchsuchen und Antwortentwürfe direkt dort ablegen — unabhängig vom oben konfigurierten Postfach (das bleibt für Import/automatische Entwürfe zuständig). Nicht gelistete Benutzer nutzen weiterhin den bisherigen Copy-and-Paste-Ablauf." }));

  const enabledC = qtEl("input", { type: "checkbox" });
  enabledC.checked = !!item.interactive_enabled;
  enabledC.addEventListener("input", () => { item.interactive_enabled = enabledC.checked; touch(); });
  wrap.appendChild(qtField("Interaktiven Postfach-Zugriff erlauben", "Aktiviert diese Verbindung für den unten gelisteten Benutzerkreis im Mail-Tab.", enabledC));

  const sharedC = qtEl("input", { type: "checkbox" });
  sharedC.checked = !!item.interactive_shared;
  sharedC.addEventListener("input", () => { item.interactive_shared = sharedC.checked; touch(); });
  wrap.appendChild(qtField("Gemeinschaftliches Postfach (statt eigenes Postfach)", "Standardmäßig AUS: berechtigte Benutzer sehen im Mail-Tab dann ihr EIGENES Postfach. Aktiviert lässt sie stattdessen das oben unter „Postfach“ konfigurierte gemeinsame/Team-Postfach (z. B. ein Funktionspostfach wie test.mechatronics.ki@rubix.com) durchsuchen — jeder berechtigte Benutzer sieht dabei dieselbe, gemeinsame Mailbox, nicht seine eigene. Für „sowohl eigenes als auch ein Team-Postfach anbieten“ zwei separate Verbindungen anlegen: eine mit, eine ohne dieses Häkchen.", sharedC));

  const usersA = qtEl("textarea", { class: "prompt-editor", rows: "3" });
  usersA.placeholder = "j.doe@rubix.com\nweitere.person@rubix.com";
  usersA.value = (item.allowed_users || []).join("\n");
  usersA.addEventListener("input", () => {
    item.allowed_users = usersA.value.split("\n").map(s => s.trim()).filter(Boolean);
    touch();
  });
  wrap.appendChild(qtField("Berechtigte Benutzer (E-Mail oder Login, eine Zeile je Benutzer)", "Nur hier gelistete, angemeldete Benutzer dürfen ihr eigenes Postfach interaktiv nutzen. Leer = niemand — bewusst eine Freigabe-, keine Sperr-Liste.", usersA));

  const groupsA = qtEl("textarea", { class: "prompt-editor", rows: "2" });
  groupsA.placeholder = "CN=Vertrieb-Mail,OU=Gruppen,DC=rubix,DC=com";
  groupsA.value = (item.allowed_groups || []).join("\n");
  groupsA.addEventListener("input", () => {
    item.allowed_groups = groupsA.value.split("\n").map(s => s.trim()).filter(Boolean);
    touch();
  });
  wrap.appendChild(qtField("Berechtigte AD-Gruppen (Gruppen-DN, eine Zeile je Gruppe)", "Zusätzlich zu den Benutzern oben — wer Mitglied einer hier gelisteten AD-Gruppe ist, ist ebenfalls berechtigt. Leer = keine zusätzliche Gruppenfreigabe.", groupsA));

  return wrap;
}

// accessControlSection renders the generalized allowed_users/allowed_groups
// allow-list (settings.go's accessControl) for a connector whose live tool
// has no per-identity restriction otherwise (currently: generic REST
// connectors — MSSQL/Shop have their own equivalent static fields in
// tab-settings.html, since they're singular settings, not a list of cards).
// Unlike exchangeInteractiveSection's AllowedUsers, an empty accessControl
// here means UNRESTRICTED, not "nobody" — see accessControl's Go doc
// comment for why.
function accessControlSection(item, touch) {
  const wrap = qtEl("div", { class: "conn-subsection" });
  wrap.appendChild(qtEl("h4", { text: "Zugriffsbeschränkung (optional)" }));
  wrap.appendChild(qtEl("p", { class: "hint", text: "Schränkt ein, welche angemeldeten Benutzer/AD-Gruppen die über diesen Connector laufenden Werkzeuge nutzen dürfen. Leer = keine zusätzliche Einschränkung (Standard, unverändertes Verhalten für jeden, der das Werkzeug ohnehin sehen würde)." }));

  if (!item.access_control) item.access_control = {};

  const usersA = qtEl("textarea", { class: "prompt-editor", rows: "2" });
  usersA.placeholder = "alice@rubix.com\nbob@rubix.com";
  usersA.value = (item.access_control.allowed_users || []).join("\n");
  usersA.addEventListener("input", () => {
    item.access_control.allowed_users = usersA.value.split("\n").map(s => s.trim()).filter(Boolean);
    touch();
  });
  wrap.appendChild(qtField("Berechtigte Benutzer (E-Mail oder Login, eine Zeile je Benutzer)", "Nur hier gelistete Benutzer (oder Mitglieder einer unten gelisteten AD-Gruppe) dürfen die über diesen Connector laufenden Werkzeuge nutzen. Leer = keine zusätzliche Einschränkung — jeder, der das Werkzeug ohnehin sehen würde, darf es weiter nutzen.", usersA));

  const groupsA = qtEl("textarea", { class: "prompt-editor", rows: "2" });
  groupsA.placeholder = "CN=SAP-Nutzer,OU=Gruppen,DC=rubix,DC=com";
  groupsA.value = (item.access_control.allowed_groups || []).join("\n");
  groupsA.addEventListener("input", () => {
    item.access_control.allowed_groups = groupsA.value.split("\n").map(s => s.trim()).filter(Boolean);
    touch();
  });
  wrap.appendChild(qtField("Berechtigte AD-Gruppen (Gruppen-DN, eine Zeile je Gruppe)", "Zusätzlich zu den Benutzern oben — wer Mitglied einer hier gelisteten AD-Gruppe ist, darf die über diesen Connector bereitgestellten Werkzeuge ebenfalls nutzen, unabhängig davon, ob die Person auch einzeln oben gelistet ist. Leer = keine zusätzliche Gruppenfreigabe.", groupsA));

  return wrap;
}

// ---- OpenAI-compatible API: named endpoints editor ------------------------
// One card per settings.openai_api.endpoints entry (settings.go's
// openAIEndpointConfig) — same "list of named cards, one array as the
// single source of truth, mutate in place" shape as connCard/qtCard above,
// just with its own small field set (no runtime knobs, no per-card test
// button — a bare LLM-passthrough endpoint has nothing standalone to test;
// an endpoint with tools is exercised the same way any other chat client
// would use it).
let openaiEndpointsState = [];

const OPENAI_PROFILE_OPTIONS = [
  ["", "wie angefragt / Standard-Chat-Profil"],
  ["local", "Lokal"], ["azure", "Azure"], ["openai", "OpenAI"],
  ["openrouter", "OpenRouter"], ["claude", "Claude"], ["gemini", "Gemini"],
];

function renderOpenAIEndpointsEditor() {
  const host = $("#openaiEndpointsEditor");
  if (!host) return;
  host.innerHTML = "";
  if (!openaiEndpointsState.length) {
    host.appendChild(qtEl("p", { class: "hint", text: "Noch kein Endpoint angelegt. Unten hinzufügen — ohne mindestens einen aktivierten Endpoint startet die API auch bei aktiviertem Port nicht." }));
  }
  openaiEndpointsState.forEach((ep, i) => host.appendChild(openaiEndpointCard(openaiEndpointsState, ep, i)));
}

function openaiEndpointCard(arr, ep, idx) {
  const touch = () => { if (typeof markSettingsDirty === "function") markSettingsDirty(true); };
  const bind = (control, key, isCheckbox, isNumber) => {
    control.addEventListener("input", () => {
      ep[key] = isCheckbox ? control.checked : isNumber ? (parseInt(control.value, 10) || 0) : control.value;
      touch();
    });
    return control;
  };
  const input = (val, ph) => { const i = qtEl("input"); i.value = val == null ? "" : val; if (ph) i.placeholder = ph; return i; };
  const checkbox = (val) => { const c = qtEl("input", { type: "checkbox" }); c.checked = !!val; return c; };

  const nameI = bind(input(ep.name, "leer = unpräfigierter Root-Pfad"), "name");
  const enabledC = bind(checkbox(ep.enabled), "enabled", true);
  const rm = qtEl("button", { type: "button", class: "ghost-btn qt-remove", text: "Endpoint entfernen" });
  rm.addEventListener("click", () => { arr.splice(idx, 1); touch(); renderOpenAIEndpointsEditor(); });
  const header = qtEl("div", { class: "qt-card-head" }, [
    qtField("Name (URL-Pfad)", "Wird zum URL-Pfad-Präfix: /<name>/v1/chat/completions und /<name>/v1/models. Leer = unpräfigiert bei /v1/chat/completions (praktisch für einen einzigen Endpoint, und wie R3 sich vor mehreren Endpoints bereits verhielt). Muss eindeutig sein; höchstens ein Endpoint darf leer bleiben.", nameI),
    qtField("aktiv", "Nur aktive Endpoints werden auf dem Port oben tatsächlich bereitgestellt.", enabledC),
    rm,
  ]);

  const body = qtEl("div", { class: "qt-card-body" });

  body.appendChild(qtField("Wissensbasis (RAG) nutzen", "Fügt R3s eigene Ranked-Retrieval-Ergebnisse als Kontext in den System-Prompt ein, bevor geantwortet wird — wie der Chat-Tab. Aus lassen, wenn dieser Endpoint nur das reine, konfigurierte Standard-LLM ohne R3-Wissen sein soll (\"einfach nur das Default-LLM nutzen\").", bind(checkbox(ep.enable_rag), "enable_rag", true)));

  body.appendChild(qtField("Werkzeuge (Live-Tools) anbieten", "Bietet die unten aktivierten Live-Werkzeuge (MSSQL, Shop, HTTP-Vorlagen/REST-Connectoren) als Function-Calling-Werkzeuge an und löst Aufrufe serverseitig auf — dieselbe Funktion (buildLiveTools), die auch Chat/Agent/Mail nutzen, also ohne separate Pflege.", bind(checkbox(ep.enable_tools), "enable_tools", true)));

  body.appendChild(qtField("Max. Werkzeug-Runden", "Nur wirksam, wenn Werkzeuge oben aktiv sind. 0/leer = 1 Runde (Standard, wie bisher). Höher erlaubt mehrstufige, vom Server selbst aufgelöste Werkzeugnutzung wie beim Agent-Tab — nützlich, wenn der aufrufende Client selbst keine eigene Tool-Schleife mitbringt.", bind(input(ep.max_tool_rounds, "0 = 1 Runde"), "max_tool_rounds", false, true)));

  const profileSel = qtEl("select");
  OPENAI_PROFILE_OPTIONS.forEach(([v, l]) => { const o = qtEl("option", { value: v, text: l }); if ((ep.profile || "") === v) o.selected = true; profileSel.appendChild(o); });
  profileSel.addEventListener("input", () => { ep.profile = profileSel.value; touch(); });
  body.appendChild(qtField("Backend fest vorgeben", "Zwingt diesen Endpoint auf ein bestimmtes LLM-Backend, unabhängig davon, was der Aufrufer im \"model\"-Feld sendet — z. B. damit ein Partner-Werkzeug nie versehentlich ein anderes (teureres/nicht freigegebenes) Backend erreicht. Leer = das vom Aufrufer gesendete \"model\"-Feld entscheidet, sonst das Standard-Chat-Profil.", profileSel));

  body.appendChild(qtField("Zugriffs-Preset", "Name eines oben unter \"Zugriffs-Presets\" definierten Presets — schränkt ein, welche Quelltypen (bei RAG) und Werkzeug-Kategorien (bei Werkzeugen) dieser Endpoint nutzen darf. Admin-fest, keine Wahl durch den Aufrufer. Leer = keine zusätzliche Einschränkung über die Abteilungs-Zugriffskontrolle hinaus. Ohne RAG/Werkzeuge oben wirkungslos.", bind(input(ep.preset, "z. B. intern"), "preset")));

  return qtEl("div", { class: "qt-card" }, [header, body]);
}

$("#openaiEndpointAdd")?.addEventListener("click", () => {
  openaiEndpointsState.push({ name: "", enabled: true, enable_rag: false, enable_tools: false, max_tool_rounds: 0, profile: "", preset: "" });
  if (typeof markSettingsDirty === "function") markSettingsDirty(true);
  renderOpenAIEndpointsEditor();
});

// renderDiscoverNode recursively renders one discoverNode (discover.go) as
// a nested <details>/<div> tree — shared by SharePoint/Folder/Exchange's
// (read-only) Discover buttons AND the Mail tab's interactive folder picker
// (loadMailGraphFolders below), since all four consume the same wire shape.
// Root open by default, everything nested below collapsed, the simplest
// predictable default for a tree whose depth varies per connector.
// onSelect, when passed, makes every node's label clickable (calling
// onSelect(node) with that node) — omitted (undefined) for the admin
// Discover buttons, which stay purely read-only exactly as before this
// parameter existed.
function renderDiscoverNode(node, isRoot, onSelect) {
  const countText = node.file_count ? `${node.file_count} Datei(en)` : node.item_count ? `${node.item_count} Element(e)` : "";
  const labelParts = [document.createTextNode((node.name || "/") + " ")];
  if (countText) labelParts.push(qtEl("span", { class: "discover-node-count", text: countText }));
  if (node.truncated) labelParts.push(qtEl("span", { class: "discover-node-truncated", text: " (gekürzt)" }));
  if (node.error) labelParts.push(qtEl("span", { class: "discover-node-error", text: " ⚠ " + node.error }));
  const label = qtEl("span", { class: "discover-node-label" }, labelParts);
  if (onSelect) {
    label.classList.add("discover-node-selectable");
    label.tabIndex = 0;
    label.setAttribute("role", "button");
    const select = (e) => { e.stopPropagation(); onSelect(node); };
    label.addEventListener("click", select);
    label.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); select(e); } });
  }

  if (!node.children || !node.children.length) {
    return qtEl("div", { class: "discover-leaf" }, [label]);
  }
  const details = qtEl("details", { class: "discover-node" });
  if (isRoot) details.open = true;
  const summary = qtEl("summary", null, [label]);
  details.appendChild(summary);
  const kids = qtEl("div", { class: "discover-children" });
  node.children.forEach(c => kids.appendChild(renderDiscoverNode(c, false, onSelect)));
  details.appendChild(kids);
  return details;
}

function renderConnEditor(kind) {
  const cfg = CONN_CFG[kind];
  const host = document.querySelector(cfg.editor);
  if (!host) return;
  const arr = connState[kind] || (connState[kind] = []);
  host.innerHTML = "";
  if (!arr.length) {
    host.appendChild(qtEl("p", { class: "hint", text: "Noch keine Verbindung angelegt. Unten hinzufügen." }));
  }
  arr.forEach((item, i) => host.appendChild(connCard(kind, arr, item, i)));
}

function wireConnEditor(kind) {
  const cfg = CONN_CFG[kind];
  document.querySelector(cfg.addBtn)?.addEventListener("click", () => {
    const arr = connState[kind] || (connState[kind] = []);
    arr.push(cfg.make());
    if (typeof markSettingsDirty === "function") markSettingsDirty(true);
    renderConnEditor(kind);
  });
}
Object.keys(CONN_CFG).forEach(wireConnEditor);

// The server returns this in X-R3-Settings-Revision. It is intentionally
// kept outside the JSON body so old settings exports and API clients remain
// schema-compatible; browser saves send it back to avoid stale whole-form
// writes clobbering another administrator's newer changes.
let settingsRevision = null;

// loadSettings fetches current server settings and populates every field
// on the Einstellungen page (and applies the active locale) from them.
async function loadSettings() {
  try {
    const response = await apiRequest("/api/settings");
    const s = response.data;
    settingsRevision = response.headers.get("X-R3-Settings-Revision");
    // s.lang is the admin's raw default (unlike /api/auth/status's `lang`,
    // which already resolves a personal override on top of it) — #s_lang
    // itself should always show that raw default, but applying it to the
    // page's active locale must respect the same "don't clobber a local
    // override" guard as the auth-status handler above, or an admin who
    // also happens to have their own language override would have it
    // silently reset every time this (admin-only) endpoint is fetched.
    let hasLocalLangOverride = false;
    try { hasLocalLangOverride = !!localStorage.getItem(LANG_OVERRIDE_KEY); } catch {}
    if (!hasLocalLangOverride) setLocale(s.lang || "de");
    $("#s_lang").value = s.lang || "de";
    $("#s_prompts_dir").value = s.prompts_dir || "";
    $("#s_user_prefs_path").value = s.user_prefs_path || "";
    const renderCfg = s.render || {};
    $("#s_render_mermaid_url").value = renderCfg.mermaid_url || "";
    $("#s_render_d3_url").value = renderCfg.d3_url || "";
    // setLocale re-stamps every data-i18n element — the login button's
    // label is state-derived ("Abmelden (Name)" while signed in), not
    // static text, so re-derive it or a signed-in user's button would
    // fall back to "Anmelden" every time settings (re)load.
    applyAdminVisibility(false);
    const local = (s.profiles && s.profiles.local) || {};
    const azure = (s.profiles && s.profiles.azure) || {};
    $("#local_base_url").value = local.base_url || "";
    $("#local_chat_model").value = local.chat_model || "";
    $("#local_embed_model").value = local.embed_model || "";
    $("#local_api_key").value = local.api_key || "";
    $("#azure_base_url").value = azure.base_url || "";
    $("#azure_api_version").value = azure.api_version || "2024-10-21";
    $("#azure_chat_model").value = azure.chat_model || "";
    $("#azure_embed_model").value = azure.embed_model || "";
    $("#azure_api_key_env").value = azure.api_key_env || "";
    $("#azure_api_key").value = azure.api_key || "";
    const openai = (s.profiles && s.profiles.openai) || {};
    $("#openai_base_url").value = openai.base_url || "https://api.openai.com/v1";
    $("#openai_chat_model").value = openai.chat_model || "gpt-4o-mini";
    $("#openai_api_key_env").value = openai.api_key_env || "OPENAI_API_KEY";
    $("#openai_api_key").value = openai.api_key || "";
    const openrouter = (s.profiles && s.profiles.openrouter) || {};
    $("#openrouter_base_url").value = openrouter.base_url || "https://openrouter.ai/api/v1";
    $("#openrouter_chat_model").value = openrouter.chat_model || "openai/gpt-4o-mini";
    $("#openrouter_api_key_env").value = openrouter.api_key_env || "OPENROUTER_API_KEY";
    $("#openrouter_api_key").value = openrouter.api_key || "";
    const claude = (s.profiles && s.profiles.claude) || {};
    $("#claude_base_url").value = claude.base_url || "https://api.anthropic.com";
    $("#claude_chat_model").value = claude.chat_model || "claude-3-5-sonnet-20241022";
    $("#claude_api_key_env").value = claude.api_key_env || "ANTHROPIC_API_KEY";
    $("#claude_api_key").value = claude.api_key || "";
    const gemini = (s.profiles && s.profiles.gemini) || {};
    $("#gemini_base_url").value = gemini.base_url || "https://generativelanguage.googleapis.com";
    $("#gemini_chat_model").value = gemini.chat_model || "gemini-2.0-flash";
    $("#gemini_api_key_env").value = gemini.api_key_env || "GEMINI_API_KEY";
    $("#gemini_api_key").value = gemini.api_key || "";
    // Embeddings are intentionally local-only, even when older settings.json
    // files still contain a cloud profile name.
    $("#s_embed_profile").value = "local";
    $("#s_chat_profile").value = s.chat_profile || "local";
    const upload = s.upload || {};
    $("#s_upload_image_mode").value = upload.image_mode === "vision" ? "vision" : "ocr";
    $("#s_upload_vision_profile").value = upload.vision_profile || "azure";
    $("#s_upload_vision_max_dim").value = upload.vision_max_dim || 0;
    $("#s_upload_vision_jpeg_quality").value = upload.vision_jpeg_quality || 0;
    $("#s_upload_max_attachment_mb").value = upload.max_attachment_mb || 0;
    $("#s_upload_max_prompt_chars").value = upload.max_prompt_chars || 0;
    $("#s_streaming").checked = !s.disable_streaming;
    $("#s_k").value = s.k || 5;
    $("#s_history_max_turns").value = s.history_max_turns || "";
    $("#s_chunk_size").value = s.chunk_size || 800;
    const r = s.ranking || {};
    $("#s_w_vector").value = r.vector_weight ?? 0.7;
    $("#s_w_keyword").value = r.keyword_weight ?? 0.2;
    $("#s_w_recency").value = r.recency_weight ?? 0.1;
    $("#s_recency_halflife").value = r.recency_half_life_days ?? 180;
    $("#s_candidate_limit").value = r.candidate_limit || 0;
    $("#s_min_similarity").value = r.min_vector_similarity || 0;
    $("#s_min_final_score").value = r.min_final_score || 0;
    $("#s_agent_mode_min_final_score").value = r.agent_mode_min_final_score || 0;
    $("#s_max_sources").value = r.max_sources || 0;
    $("#s_max_hits_per_source").value = r.max_hits_per_source || 0;
    $("#s_context_before").value = r.context_chunks_before ?? -1;
    $("#s_context_after").value = r.context_chunks_after ?? -1;
    $("#s_max_primary_chars").value = r.max_primary_content_chars || 0;
    $("#s_max_sibling_chars").value = r.max_sibling_chars || 0;
    $("#s_max_family_siblings").value = r.max_family_siblings || 0;
    const storage = s.storage || {};
    $("#s_storage_backend").value = storage.backend || "tinysql";
    $("#s_storage_mode").value = storage.mode || "disk";
    $("#s_storage_path").value = storage.path || "";
    $("#s_storage_max_mem_mb").value = storage.max_memory_mb || 0;
    $("#s_redact_pii").checked = !!s.redact_pii;
    $("#s_allow_shell").checked = !!s.allow_shell_exec;
    $("#s_markitdown_bin").value = (s.import && s.import.markitdown_bin) || "markitdown";
    $("#s_markitdown_docintel_endpoint").value = (s.import && s.import.markitdown_docintel_endpoint) || "";
    $("#s_ffmpeg_bin").value = (s.import && s.import.ffmpeg_bin) || "ffmpeg";
    $("#s_sevenzip_bin").value = (s.import && s.import.sevenzip_bin) || "7z";
    $("#s_tesseract_bin").value = (s.import && s.import.tesseract_bin) || "";
    $("#s_tesseract_lang").value = (s.import && s.import.tesseract_lang) || "";
    $("#s_whisper_bin").value = (s.import && s.import.whisper_bin) || "whisper-cli";
    $("#s_whisper_model").value = (s.import && s.import.whisper_model) || "";
    $("#s_whisper_language").value = (s.import && s.import.whisper_language) || "de";
    $("#s_whisper_timeout").value = (s.import && s.import.whisper_timeout_seconds) || "";
    $("#s_whisper_threads").value = (s.import && s.import.whisper_threads) || 0;
    $("#s_whisper_beam_size").value = (s.import && s.import.whisper_beam_size) || 0;
    $("#s_whisper_flash_attn").checked = !!(s.import && s.import.whisper_flash_attn);
    $("#s_whisper_vad").checked = !!(s.import && s.import.whisper_vad);
    $("#s_whisper_vad_model").value = (s.import && s.import.whisper_vad_model) || "";
    $("#s_whisper_max_concurrent").value = (s.import && s.import.whisper_max_concurrent) || 0;
    $("#s_import_originals_dir").value = (s.import && s.import.originals_dir) || "";
    $("#s_import_max_file_mb").value = (s.import && s.import.max_file_mb) || "";
    $("#s_import_max_items").value = (s.import && s.import.max_items_per_run) || "";
    $("#s_import_request_delay").value = (s.import && s.import.request_delay_ms) || "";
    $("#s_import_preview_limit").value = (s.import && s.import.preview_limit) || "";
    $("#s_import_graph_max_retries").value = (s.import && s.import.graph_max_retries) || "";
    $("#s_import_connector_max_retries").value = (s.import && s.import.connector_max_retries) || "";
    $("#s_import_rest_timeout").value = (s.import && s.import.rest_connector_timeout_seconds) || "";
    $("#s_import_embed_batch_size").value = (s.import && s.import.embed_batch_size) || "";
    $("#s_teams_max_replies").value = (s.import && s.import.teams_max_replies_per_thread) || 0;
    $("#s_allow_internal_fetch").checked = !!(s.import && s.import.allow_internal_fetch);
    $("#s_url_mappings").value = formatURLMappings(s.url_mappings);
    $("#s_source_visibility").value = formatSourceVisibility(s.source_visibility);
    $("#s_source_access").value = formatSourceAccess(s.source_access);
    $("#s_personalize_answers").checked = !!s.personalize_answers;
    $("#s_presets").value = JSON.stringify(s.presets || [], null, 2);
    $("#s_draft_replies_enabled").checked = !!s.enable_draft_replies;
    $("#s_draft_chat_profile").value = s.draft_chat_profile || "";
    $("#s_mail_default_preset").value = s.draft_preset || "";
    $("#s_draft_max_tool_rounds").value = s.draft_max_tool_rounds || "";
    $("#s_chat_history_enabled").checked = !!s.enable_chat_history;
    $("#s_chat_history_path").value = s.chat_history_path || "";
    $("#s_token_usage_path").value = s.token_usage_path || "";
    const ldap = s.ldap || {};
    $("#s_ldap_enabled").checked = !!ldap.enabled;
    $("#s_ldap_url").value = ldap.url || "";
    $("#s_ldap_base_dn").value = ldap.base_dn || "";
    $("#s_ldap_domain_prefix").value = ldap.domain_prefix || "";
    $("#s_ldap_required_group_dn").value = ldap.required_group_dn || "";
    $("#s_admin_password_env").value = s.admin_password_env || "";
    $("#s_ldap_admin_users").value = (ldap.admin_users || []).join("\n");
    $("#s_ldap_guest_azure_policy").value = ldap.guest_azure_profile_policy || "fallback";
    const localAuth = s.local_auth || {};
    $("#s_localauth_enabled").checked = !!localAuth.enabled;
    $("#s_localauth_min_pw").value = localAuth.min_password_length || "";
    $("#s_localauth_cost").value = localAuth.bcrypt_cost || "";
    const apiCfg = s.api || {};
    $("#s_api_require_key").checked = !!apiCfg.require_api_key;
    $("#s_api_guest_rate_limit").value = apiCfg.guest_ask_rate_limit_per_minute || 0;
    $("#s_api_guest_voice_rate_limit").value = apiCfg.guest_voice_rate_limit_per_minute || 0;
    const openaiCfg = s.openai_api || {};
    $("#s_openai_api_enabled").checked = !!openaiCfg.enabled;
    $("#s_openai_api_port").value = openaiCfg.port || 0;
    openaiEndpointsState = JSON.parse(JSON.stringify(openaiCfg.endpoints || []));
    renderOpenAIEndpointsEditor();
    // sharepoint/onedrive/exchange_graph/imap/teams/confluence/jira/freshservice,
    // github and sap_s4 are
    // arrays of named connections (multiple mailboxes/sites/etc. per type)
    // — connState is the live in-memory copy the card editor
    // (renderConnEditor/connCard below) reads from and mutates directly;
    // saveSettings sends it back as-is.
    Object.keys(CONN_CFG).forEach(kind => {
      connState[kind] = JSON.parse(JSON.stringify(s[kind] || []));
      renderConnEditor(kind);
    });
    const smtp = s.smtp || {};
    $("#s_smtp_enabled").checked = !!smtp.enabled;
    $("#s_smtp_host").value = smtp.host || "";
    $("#s_smtp_port").value = smtp.port || 25;
    $("#s_smtp_username").value = smtp.username || "";
    $("#s_smtp_password_env").value = smtp.password_env || "";
    $("#s_smtp_password").value = smtp.password || "";
    $("#s_smtp_from").value = smtp.from || "";
    const mssql = s.mssql || {};
    $("#s_mssql_enabled").checked = !!mssql.enabled;
    $("#s_mssql_host").value = mssql.host || "";
    $("#s_mssql_port").value = mssql.port || 1433;
    $("#s_mssql_database").value = mssql.database || "";
    $("#s_mssql_username").value = mssql.username || "";
    $("#s_mssql_password_env").value = mssql.password_env || "";
    $("#s_mssql_password").value = mssql.password || "";
    $("#s_mssql_trust_cert").checked = mssql.trust_server_certificate !== false;
    $("#s_mssql_max_rows").value = mssql.max_rows || 200;
    $("#s_mssql_timeout").value = mssql.timeout_seconds || 10;
    $("#s_mssql_allow_generic").checked = mssql.allow_generic_query === true;
    $("#s_mssql_mask_columns").value = (mssql.mask_columns || []).join(", ");
    const mssqlAC = mssql.access_control || {};
    $("#s_mssql_access_users").value = (mssqlAC.allowed_users || []).join(", ");
    $("#s_mssql_access_groups").value = (mssqlAC.allowed_groups || []).join("\n");
    $("#s_mssql_query_templates").value = JSON.stringify(mssql.query_templates || [], null, 2);
    renderQTEditor("sql");
    const shop = s.shop || {};
    $("#s_shop_enabled").checked = !!shop.enabled;
    $("#s_shop_base_url").value = shop.base_url || "";
    $("#s_shop_username").value = shop.username || "";
    $("#s_shop_password_env").value = shop.password_env || "";
    $("#s_shop_password").value = shop.password || "";
    $("#s_shop_client_id").value = shop.client_id || "";
    $("#s_shop_client_secret_env").value = shop.client_secret_env || "";
    $("#s_shop_client_secret").value = shop.client_secret || "";
    $("#s_shop_max_results").value = shop.max_results || 10;
    $("#s_shop_timeout").value = shop.timeout_seconds || 10;
    $("#s_shop_max_retries").value = shop.max_retries || "";
    const shopAC = shop.access_control || {};
    $("#s_shop_access_users").value = (shopAC.allowed_users || []).join(", ");
    $("#s_shop_access_groups").value = (shopAC.allowed_groups || []).join("\n");
    $("#s_http_templates").value = JSON.stringify(s.http_templates || [], null, 2);
    renderQTEditor("http");

    const agent = s.agent || {};
    $("#s_agent_max_rounds").value = agent.max_tool_rounds || "";
    $("#s_agent_allow_code").checked = agent.allow_code_execution === true;
    $("#s_agent_allow_web_fetch").checked = agent.allow_web_fetch === true;
    $("#s_agent_allow_web_research").checked = agent.allow_web_research === true;
    $("#s_agent_web_research_rounds").value = agent.web_research_rounds || "";
    $("#s_agent_web_research_timeout").value = agent.web_research_timeout_seconds || "";
    $("#s_agent_allow_web_search").checked = agent.allow_web_search === true;
    $("#s_agent_web_search_api_key_env").value = agent.web_search_api_key_env || "";
    $("#s_agent_web_search_api_key").value = agent.web_search_api_key || "";
    $("#s_agent_web_search_max_results").value = agent.web_search_max_results || "";
    $("#s_agent_web_search_timeout").value = agent.web_search_timeout_seconds || "";
    $("#s_agent_allow_azure_bing_search").checked = agent.allow_azure_bing_search === true;
    $("#s_agent_azure_bing_search_timeout").value = agent.azure_bing_search_timeout_seconds || "";
    $("#s_agent_subagents_disabled").checked = agent.subagents_disabled === true;
    $("#s_agent_max_subtasks").value = agent.max_subtasks || "";
    $("#s_agent_subagent_rounds").value = agent.subagent_rounds || "";
    $("#s_agent_max_concurrency").value = agent.max_concurrency || "";
    $("#s_agent_default_preset").value = agent.default_preset || "";
    $("#s_agent_context_compaction_disabled").checked = agent.context_compaction_disabled === true;
    $("#s_agent_context_compaction_threshold").value = agent.context_compaction_threshold_chars || "";
    $("#s_agent_context_compaction_keep_rounds").value = agent.context_compaction_keep_rounds || "";
    $("#s_agent_search_result_chars").value = agent.search_result_chars || "";
    $("#s_agent_source_content_chars").value = agent.source_content_chars || "";

    const toolRouter = s.tool_router || {};
    $("#s_tool_router_enabled").checked = toolRouter.enabled === true;
    $("#s_tool_router_profile").value = toolRouter.profile || "";

    const queryRewrite = s.query_rewrite || {};
    $("#s_query_rewrite_enabled").checked = queryRewrite.enabled === true;
    $("#s_query_rewrite_profile").value = queryRewrite.profile || "";

    // Every "_enabled" checkbox above was just set via .checked = ..., which
    // doesn't fire "change" — refresh the Aktiv/Aus badges explicitly so
    // they reflect what was actually just loaded, not their on-page-load
    // default (see settingsBadgeUpdaters' doc comment).
    refreshSettingsBadges();
    markSettingsDirty(false);
    setSettingsSaveState("");
    return true;
  } catch (err) {
    $("#settingsResult").className = "result error";
    $("#settingsResult").textContent = err.message;
    return false;
  }
}

$("#saveSettings").addEventListener("click", async () => {
  const out = $("#settingsResult");
  out.className = "result";
  let mssqlQueryTemplates;
  try {
    mssqlQueryTemplates = JSON.parse($("#s_mssql_query_templates").value || "[]");
  } catch (err) {
    out.className = "result error";
    out.textContent = "SQL-Abfrage-Vorlagen: ungültiges JSON — " + err.message;
    return;
  }
  let httpTemplates;
  try {
    httpTemplates = JSON.parse($("#s_http_templates").value || "[]");
  } catch (err) {
    out.className = "result error";
    out.textContent = "HTTP-Abfrage-Vorlagen: ungültiges JSON — " + err.message;
    return;
  }
  let presets;
  try {
    presets = JSON.parse($("#s_presets").value || "[]");
  } catch (err) {
    out.className = "result error";
    out.textContent = "Zugriffs-Presets: ungültiges JSON — " + err.message;
    return;
  }
  const body = {
    lang: $("#s_lang").value,
    prompts_dir: $("#s_prompts_dir").value.trim(),
    user_prefs_path: $("#s_user_prefs_path").value.trim(),
    admin_password_env: $("#s_admin_password_env").value.trim(),
    chat_history_path: $("#s_chat_history_path").value.trim(),
    token_usage_path: $("#s_token_usage_path").value.trim(),
    render: {
      mermaid_url: $("#s_render_mermaid_url").value.trim(),
      d3_url: $("#s_render_d3_url").value.trim(),
    },
    profiles: {
      local: {
        provider: "local",
        base_url: $("#local_base_url").value.trim(),
        chat_model: $("#local_chat_model").value.trim(),
        embed_model: $("#local_embed_model").value.trim(),
        api_key: $("#local_api_key").value.trim(),
      },
      azure: {
        provider: "azure",
        base_url: $("#azure_base_url").value.trim(),
        api_version: $("#azure_api_version").value.trim(),
        chat_model: $("#azure_chat_model").value.trim(),
        embed_model: $("#azure_embed_model").value.trim(),
        api_key_env: $("#azure_api_key_env").value.trim(),
        api_key: $("#azure_api_key").value.trim(),
      },
      openai: {
        provider: "openai",
        base_url: $("#openai_base_url").value.trim(),
        chat_model: $("#openai_chat_model").value.trim(),
        api_key_env: $("#openai_api_key_env").value.trim(),
        api_key: $("#openai_api_key").value.trim(),
      },
      openrouter: {
        provider: "openrouter",
        base_url: $("#openrouter_base_url").value.trim(),
        chat_model: $("#openrouter_chat_model").value.trim(),
        api_key_env: $("#openrouter_api_key_env").value.trim(),
        api_key: $("#openrouter_api_key").value.trim(),
      },
      claude: {
        provider: "claude",
        base_url: $("#claude_base_url").value.trim(),
        chat_model: $("#claude_chat_model").value.trim(),
        api_key_env: $("#claude_api_key_env").value.trim(),
        api_key: $("#claude_api_key").value.trim(),
      },
      gemini: {
        provider: "gemini",
        base_url: $("#gemini_base_url").value.trim(),
        chat_model: $("#gemini_chat_model").value.trim(),
        api_key_env: $("#gemini_api_key_env").value.trim(),
        api_key: $("#gemini_api_key").value.trim(),
      },
    },
    embed_profile: "local",
    chat_profile: $("#s_chat_profile").value,
    upload: {
      image_mode: $("#s_upload_image_mode").value,
      vision_profile: $("#s_upload_vision_profile").value,
      vision_max_dim: parseInt($("#s_upload_vision_max_dim").value, 10) || 0,
      vision_jpeg_quality: parseInt($("#s_upload_vision_jpeg_quality").value, 10) || 0,
      max_attachment_mb: parseInt($("#s_upload_max_attachment_mb").value, 10) || 0,
      max_prompt_chars: parseInt($("#s_upload_max_prompt_chars").value, 10) || 0,
    },
    disable_streaming: !$("#s_streaming").checked,
    k: parseInt($("#s_k").value, 10) || 5,
    history_max_turns: intOrDefault($("#s_history_max_turns").value, 0),
    chunk_size: parseInt($("#s_chunk_size").value, 10) || 800,
    storage: {
      backend: $("#s_storage_backend").value,
      mode: $("#s_storage_mode").value,
      path: $("#s_storage_path").value.trim(),
      max_memory_mb: parseInt($("#s_storage_max_mem_mb").value, 10) || 0,
    },
    redact_pii: $("#s_redact_pii").checked,
    allow_shell_exec: $("#s_allow_shell").checked,
    presets: presets,
    enable_draft_replies: $("#s_draft_replies_enabled").checked,
    draft_chat_profile: $("#s_draft_chat_profile").value,
    draft_preset: $("#s_mail_default_preset").value.trim(),
    draft_max_tool_rounds: intOrDefault($("#s_draft_max_tool_rounds").value, 0),
    enable_chat_history: $("#s_chat_history_enabled").checked,
    ldap: {
      enabled: $("#s_ldap_enabled").checked,
      url: $("#s_ldap_url").value.trim(),
      base_dn: $("#s_ldap_base_dn").value.trim(),
      domain_prefix: $("#s_ldap_domain_prefix").value.trim(),
      required_group_dn: $("#s_ldap_required_group_dn").value.trim(),
      admin_users: $("#s_ldap_admin_users").value.split("\n").map(l => l.trim()).filter(Boolean),
      guest_azure_profile_policy: $("#s_ldap_guest_azure_policy").value,
    },
    local_auth: {
      enabled: $("#s_localauth_enabled").checked,
      min_password_length: intOrDefault($("#s_localauth_min_pw").value, 0),
      bcrypt_cost: intOrDefault($("#s_localauth_cost").value, 0),
    },
    api: {
      require_api_key: $("#s_api_require_key").checked,
      guest_ask_rate_limit_per_minute: parseInt($("#s_api_guest_rate_limit").value, 10) || 0,
      guest_voice_rate_limit_per_minute: parseInt($("#s_api_guest_voice_rate_limit").value, 10) || 0,
    },
    openai_api: {
      enabled: $("#s_openai_api_enabled").checked,
      port: parseInt($("#s_openai_api_port").value, 10) || 0,
      endpoints: openaiEndpointsState,
    },
    // Each of these is a list of named connections (connruntime.go),
    // edited directly in connState by the card editor (renderConnEditor/
    // connCard below) — sent back as-is.
    sharepoint: connState.sharepoint,
    onedrive: connState.onedrive,
    exchange_graph: connState.exchange_graph,
    imap: connState.imap,
    teams: connState.teams,
    confluence: connState.confluence,
    jira: connState.jira,
    freshservice: connState.freshservice,
    folder: connState.folder,
    github: connState.github,
    sap_s4: connState.sap_s4,
    smtp: {
      enabled: $("#s_smtp_enabled").checked,
      host: $("#s_smtp_host").value.trim(),
      port: parseInt($("#s_smtp_port").value, 10) || 25,
      username: $("#s_smtp_username").value.trim(),
      password: $("#s_smtp_password").value.trim(),
      password_env: $("#s_smtp_password_env").value.trim(),
      from: $("#s_smtp_from").value.trim(),
    },
    mssql: {
      enabled: $("#s_mssql_enabled").checked,
      host: $("#s_mssql_host").value.trim(),
      port: parseInt($("#s_mssql_port").value, 10) || 1433,
      database: $("#s_mssql_database").value.trim(),
      username: $("#s_mssql_username").value.trim(),
      password_env: $("#s_mssql_password_env").value.trim(),
      password: $("#s_mssql_password").value.trim(),
      trust_server_certificate: $("#s_mssql_trust_cert").checked,
      max_rows: parseInt($("#s_mssql_max_rows").value, 10) || 200,
      timeout_seconds: parseInt($("#s_mssql_timeout").value, 10) || 10,
      allow_generic_query: $("#s_mssql_allow_generic").checked,
      mask_columns: $("#s_mssql_mask_columns").value.split(",").map(s => s.trim()).filter(Boolean),
      query_templates: mssqlQueryTemplates,
      access_control: {
        allowed_users: $("#s_mssql_access_users").value.split(",").map(s => s.trim()).filter(Boolean),
        allowed_groups: $("#s_mssql_access_groups").value.split("\n").map(s => s.trim()).filter(Boolean),
      },
    },
    shop: {
      enabled: $("#s_shop_enabled").checked,
      base_url: $("#s_shop_base_url").value.trim(),
      username: $("#s_shop_username").value.trim(),
      password_env: $("#s_shop_password_env").value.trim(),
      password: $("#s_shop_password").value.trim(),
      client_id: $("#s_shop_client_id").value.trim(),
      client_secret_env: $("#s_shop_client_secret_env").value.trim(),
      client_secret: $("#s_shop_client_secret").value.trim(),
      max_results: parseInt($("#s_shop_max_results").value, 10) || 10,
      timeout_seconds: parseInt($("#s_shop_timeout").value, 10) || 10,
      max_retries: intOrDefault($("#s_shop_max_retries").value, 0),
      access_control: {
        allowed_users: $("#s_shop_access_users").value.split(",").map(s => s.trim()).filter(Boolean),
        allowed_groups: $("#s_shop_access_groups").value.split("\n").map(s => s.trim()).filter(Boolean),
      },
    },
    http_templates: httpTemplates,
    // Generic REST connectors (settings.go's restConnectorConfig), edited in
    // connState by the same card editor as the import connectors above.
    rest_connectors: connState.rest_connectors,
    agent: {
      allow_code_execution: $("#s_agent_allow_code").checked,
      allow_web_fetch: $("#s_agent_allow_web_fetch").checked,
      allow_web_research: $("#s_agent_allow_web_research").checked,
      web_research_rounds: intOrDefault($("#s_agent_web_research_rounds").value, 0),
      web_research_timeout_seconds: intOrDefault($("#s_agent_web_research_timeout").value, 0),
      allow_web_search: $("#s_agent_allow_web_search").checked,
      web_search_api_key_env: $("#s_agent_web_search_api_key_env").value.trim(),
      web_search_api_key: $("#s_agent_web_search_api_key").value.trim(),
      web_search_max_results: intOrDefault($("#s_agent_web_search_max_results").value, 0),
      web_search_timeout_seconds: intOrDefault($("#s_agent_web_search_timeout").value, 0),
      allow_azure_bing_search: $("#s_agent_allow_azure_bing_search").checked,
      azure_bing_search_timeout_seconds: intOrDefault($("#s_agent_azure_bing_search_timeout").value, 0),
      subagents_disabled: $("#s_agent_subagents_disabled").checked,
      max_subtasks: intOrDefault($("#s_agent_max_subtasks").value, 0),
      subagent_rounds: intOrDefault($("#s_agent_subagent_rounds").value, 0),
      max_concurrency: intOrDefault($("#s_agent_max_concurrency").value, 0),
      max_tool_rounds: parseInt($("#s_agent_max_rounds").value, 10) || 0,
      default_preset: $("#s_agent_default_preset").value.trim(),
      context_compaction_disabled: $("#s_agent_context_compaction_disabled").checked,
      context_compaction_threshold_chars: intOrDefault($("#s_agent_context_compaction_threshold").value, 0),
      context_compaction_keep_rounds: intOrDefault($("#s_agent_context_compaction_keep_rounds").value, 0),
      search_result_chars: intOrDefault($("#s_agent_search_result_chars").value, 0),
      source_content_chars: intOrDefault($("#s_agent_source_content_chars").value, 0),
    },
    tool_router: {
      enabled: $("#s_tool_router_enabled").checked,
      profile: $("#s_tool_router_profile").value,
    },
    query_rewrite: {
      enabled: $("#s_query_rewrite_enabled").checked,
      profile: $("#s_query_rewrite_profile").value,
    },
    ranking: {
      vector_weight: parseFloat($("#s_w_vector").value) || 0,
      keyword_weight: parseFloat($("#s_w_keyword").value) || 0,
      recency_weight: parseFloat($("#s_w_recency").value) || 0,
      recency_half_life_days: parseFloat($("#s_recency_halflife").value) || 0,
      candidate_limit: intOrDefault($("#s_candidate_limit").value, 0),
      min_vector_similarity: parseFloat($("#s_min_similarity").value) || 0,
      min_final_score: parseFloat($("#s_min_final_score").value) || 0,
      agent_mode_min_final_score: parseFloat($("#s_agent_mode_min_final_score").value) || 0,
      max_sources: intOrDefault($("#s_max_sources").value, 0),
      max_hits_per_source: intOrDefault($("#s_max_hits_per_source").value, 0),
      context_chunks_before: intOrDefault($("#s_context_before").value, -1),
      context_chunks_after: intOrDefault($("#s_context_after").value, -1),
      max_primary_content_chars: intOrDefault($("#s_max_primary_chars").value, 0),
      max_sibling_chars: intOrDefault($("#s_max_sibling_chars").value, 0),
      max_family_siblings: intOrDefault($("#s_max_family_siblings").value, 0),
    },
    import: {
      markitdown_bin: $("#s_markitdown_bin").value.trim() || "markitdown",
      markitdown_docintel_endpoint: $("#s_markitdown_docintel_endpoint").value.trim(),
      ffmpeg_bin: $("#s_ffmpeg_bin").value.trim() || "ffmpeg",
      sevenzip_bin: $("#s_sevenzip_bin").value.trim() || "7z",
      originals_dir: $("#s_import_originals_dir").value.trim(),
      max_file_mb: intOrDefault($("#s_import_max_file_mb").value, 0),
      max_items_per_run: intOrDefault($("#s_import_max_items").value, 0),
      request_delay_ms: intOrDefault($("#s_import_request_delay").value, 0),
      preview_limit: intOrDefault($("#s_import_preview_limit").value, 0),
      graph_max_retries: intOrDefault($("#s_import_graph_max_retries").value, 0),
      connector_max_retries: intOrDefault($("#s_import_connector_max_retries").value, 0),
      rest_connector_timeout_seconds: intOrDefault($("#s_import_rest_timeout").value, 0),
      embed_batch_size: intOrDefault($("#s_import_embed_batch_size").value, 0),
      teams_max_replies_per_thread: intOrDefault($("#s_teams_max_replies").value, 0),
      tesseract_bin: $("#s_tesseract_bin").value.trim(),
      tesseract_lang: $("#s_tesseract_lang").value.trim(),
      whisper_bin: $("#s_whisper_bin").value.trim() || "whisper-cli",
      whisper_model: $("#s_whisper_model").value.trim(),
      whisper_language: $("#s_whisper_language").value.trim(),
      whisper_timeout_seconds: intOrDefault($("#s_whisper_timeout").value, 0),
      whisper_threads: intOrDefault($("#s_whisper_threads").value, 0),
      whisper_beam_size: intOrDefault($("#s_whisper_beam_size").value, 0),
      whisper_flash_attn: $("#s_whisper_flash_attn").checked,
      whisper_vad: $("#s_whisper_vad").checked,
      whisper_vad_model: $("#s_whisper_vad_model").value.trim(),
      whisper_max_concurrent: intOrDefault($("#s_whisper_max_concurrent").value, 0),
      allow_internal_fetch: $("#s_allow_internal_fetch").checked,
    },
    url_mappings: parseURLMappings($("#s_url_mappings").value),
    source_visibility: parseSourceVisibility($("#s_source_visibility").value),
    source_access: parseSourceAccess($("#s_source_access").value),
    personalize_answers: $("#s_personalize_answers").checked,
  };
  const saveBtn = $("#saveSettings");
  setBusy(saveBtn, true);
  try {
    const response = await apiRequest("/api/settings", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(settingsRevision !== null ? { "X-R3-Settings-Revision": settingsRevision } : {}),
      },
      body: JSON.stringify(body),
    });
    settingsRevision = response.headers.get("X-R3-Settings-Revision") || settingsRevision;
    out.textContent = t("settings.save.saved");
    // Apply a changed UI language to this admin's own session immediately
    // — without this, only the *next* page load (or another visitor's)
    // would pick it up, which read like the language picker didn't work
    // at all right after saving it.
    setLocale($("#s_lang").value);
    // Reset the unsaved-changes indicator (defined further down; hoisted
    // function declaration, so callable from here).
    markSettingsDirty(false);
    setSettingsSaveState(t("settings.save.savedAt", { time: new Date().toLocaleTimeString() }));
    loadSettingsHistory();
  } catch (err) {
    out.className = "result error";
    if (err.status === 409) {
      out.textContent = t("settings.conflict.message");
      setSettingsSaveState(t("settings.conflict.state"), "error");
      NovaPop.toast({
        type: "warn", duration: 10000, message: t("settings.conflict.message"),
        action: { label: t("settings.reload.label"), onClick: () => loadSettings() },
      });
    } else {
      out.textContent = err.message;
      setSettingsSaveState(t("settings.save.error"), "error");
    }
  } finally {
    setBusy(saveBtn, false);
  }
});

// exportSettings: a plain same-origin navigation, not fetch+blob — the
// browser sends the admin session cookie automatically and
// handleSettingsExport's Content-Disposition header does the rest (normal
// browser "Save As", no extra client-side plumbing needed).
$("#exportSettings").addEventListener("click", () => {
  window.location.href = "/api/settings/export";
});

// importSettingsBtn/importSettingsFile: "Import Settings" is a plain file
// picker (no drag-and-drop, unlike the Import tab's uploads — this is an
// occasional admin action, not a workflow) that re-POSTs the exported file
// verbatim to the same endpoint a normal form save uses. That's deliberate:
// every merge rule already in handleSettings (skip a blank/masked secret,
// keep the current one — see settings.go's mergeProfile/mergeRESTConn etc.)
// applies unchanged, so a file exported via exportableSettings (credentials
// blanked) can never wipe out real credentials already configured here.
// ?source=import only changes how the resulting Änderungshistorie entry is
// labeled (settings_history.go) — the save logic itself is identical.
$("#importSettingsBtn").addEventListener("click", () => {
  $("#importSettingsFile").click();
});
$("#importSettingsFile").addEventListener("change", async () => {
  const input = $("#importSettingsFile");
  const file = input.files && input.files[0];
  if (!file) return;
  const out = $("#settingsResult");
  out.className = "result";
  try {
    const text = await file.text();
    JSON.parse(text); // validate before sending — a clearer error than the server's 400
    const response = await apiRequest("/api/settings?source=import", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(settingsRevision !== null ? { "X-R3-Settings-Revision": settingsRevision } : {}),
      },
      body: text,
    });
    settingsRevision = response.headers.get("X-R3-Settings-Revision") || settingsRevision;
    out.textContent = "Importiert.";
    await loadSettings();
    await loadSettingsHistory();
  } catch (err) {
    out.className = "result error";
    out.textContent = "Import fehlgeschlagen: " + err.message;
  } finally {
    input.value = "";
  }
});

// ---- Connector "Verbindung testen" buttons ----------------------------
// Every external interface in Settings gets a debug button that tests the
// form's current (not-yet-saved) values against the real backend — see
// conntest.go's /api/settings/test/* endpoints, which all return the same
// {ok, detail} shape regardless of connector. HTTP-based connectors
// additionally return `exchanges`: the raw request/response dumps
// (secrets already redacted server-side, see conntrace.go), rendered
// behind a click-to-expand toggle with a per-block Text/Hex switch.

// hexDumpText renders a string's UTF-8 bytes as a classic
// offset/hex/ASCII dump — 16 bytes per line, non-printables as ".".
function hexDumpText(str) {
  const bytes = new TextEncoder().encode(str);
  const lines = [];
  for (let off = 0; off < bytes.length; off += 16) {
    const chunk = bytes.slice(off, off + 16);
    const hex = Array.from(chunk).map(b => b.toString(16).padStart(2, "0")).join(" ");
    const ascii = Array.from(chunk).map(b => (b >= 32 && b < 127) ? String.fromCharCode(b) : ".").join("");
    lines.push(off.toString(16).padStart(8, "0") + "  " + hex.padEnd(47, " ") + "  " + ascii);
  }
  return lines.join("\n");
}

// renderConnExchanges appends the collapsed raw-exchange viewer below a
// test-result line. Only ever renders what the server chose to send —
// redaction happened there, this is pure display.
function renderConnExchanges(container, exchanges) {
  if (!exchanges || !exchanges.length) return;
  const details = document.createElement("details");
  details.className = "conn-exchanges";
  const summary = document.createElement("summary");
  summary.textContent = `Details anzeigen — ${exchanges.length} Request/Response (Secrets redigiert)`;
  details.appendChild(summary);
  exchanges.forEach((ex, i) => {
    const h = document.createElement("h5");
    h.textContent = `${i + 1}. ${ex.label || ""}`;
    details.appendChild(h);
    [["Request", ex.request], ["Response", ex.response], ["Netzwerkfehler", ex.error]].forEach(([title, text]) => {
      if (!text) return;
      const wrap = document.createElement("div");
      wrap.className = "conn-exchange-block";
      const bar = document.createElement("div");
      bar.className = "conn-exchange-bar";
      const label = document.createElement("span");
      label.className = "hint";
      label.textContent = title;
      const toggle = document.createElement("button");
      toggle.type = "button";
      toggle.className = "ghost-btn conn-hex-toggle";
      toggle.textContent = "Hex-Ansicht";
      toggle.title = "Zwischen Text- und Hexdump-Darstellung umschalten";
      const pre = document.createElement("pre");
      pre.className = "debug-pre";
      pre.textContent = text;
      let hex = false;
      toggle.addEventListener("click", () => {
        hex = !hex;
        pre.textContent = hex ? hexDumpText(text) : text;
        toggle.textContent = hex ? "Text-Ansicht" : "Hex-Ansicht";
      });
      bar.appendChild(label);
      bar.appendChild(toggle);
      wrap.appendChild(bar);
      wrap.appendChild(pre);
      details.appendChild(wrap);
    });
  });
  container.appendChild(details);
}

function wireConnTest(btnId, resultId, endpoint, buildPayload) {
  const btn = $(btnId);
  const out = $(resultId);
  if (!btn || !out) return;
  btn.addEventListener("click", async () => {
    out.className = "result";
    out.textContent = "Teste Verbindung …";
    setBusy(btn, true);
    try {
      const res = await api(endpoint, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(buildPayload()),
      });
      out.className = res.ok ? "result" : "result error";
      out.textContent = (res.ok ? "✓ " : "✗ ") + res.detail;
      renderConnExchanges(out, res.exchanges);
    } catch (err) {
      out.className = "result error";
      out.textContent = err.message;
    } finally {
      setBusy(btn, false);
    }
  });
}

wireConnTest("#llmLocalTest", "#llmLocalTestResult", "/api/settings/test/llm", () => ({
  provider: "local",
  base_url: $("#local_base_url").value.trim(),
  chat_model: $("#local_chat_model").value.trim(),
  embed_model: $("#local_embed_model").value.trim(),
  api_key: $("#local_api_key").value.trim(),
}));

// "Modelle laden" — queries the local backend's GET /v1/models (see
// llm.go's listModels) and fills the shared <datalist> both the
// Chat-Modell and Embedding-Modell inputs point at via list=, so an admin
// can pick from a dropdown instead of typing an exact model id blind.
$("#llmLocalListModels").addEventListener("click", async () => {
  const btn = $("#llmLocalListModels");
  const out = $("#llmLocalTestResult");
  out.className = "result";
  out.textContent = "Lade Modellliste …";
  setBusy(btn, true);
  try {
    const res = await api("/api/settings/test/llm-models", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        provider: "local",
        base_url: $("#local_base_url").value.trim(),
        api_key: $("#local_api_key").value.trim(),
      }),
    });
    const list = $("#localModelList");
    list.innerHTML = (res.models || []).map(m => `<option value="${escapeHTML(m)}">`).join("");
    out.className = res.ok ? "result" : "result error";
    out.textContent = (res.ok ? "✓ " : "✗ ") + res.detail;
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
});

wireConnTest("#llmAzureTest", "#llmAzureTestResult", "/api/settings/test/llm", () => ({
  provider: "azure",
  base_url: $("#azure_base_url").value.trim(),
  api_version: $("#azure_api_version").value.trim(),
  chat_model: $("#azure_chat_model").value.trim(),
  embed_model: $("#azure_embed_model").value.trim(),
  api_key_env: $("#azure_api_key_env").value.trim(),
  api_key: $("#azure_api_key").value.trim(),
}));

function llmChatTestPayload(provider) {
  return {
    provider,
    base_url: $("#" + provider + "_base_url").value.trim(),
    chat_model: $("#" + provider + "_chat_model").value.trim(),
    api_key_env: $("#" + provider + "_api_key_env").value.trim(),
    api_key: $("#" + provider + "_api_key").value.trim(),
  };
}

[["openai", "OpenAI"], ["openrouter", "OpenRouter"], ["claude", "Claude"], ["gemini", "Gemini"]].forEach(([provider, label]) => {
  wireConnTest("#llm" + label + "Test", "#llm" + label + "TestResult",
    "/api/settings/test/llm", () => llmChatTestPayload(provider));
});

// sharepoint/exchange_graph/imap/teams/confluence/jira/freshservice no
// longer have a single static "Verbindung testen" button — each named
// connection's card (connCard, below) has its own, since there can be any
// number of them.

wireConnTest("#smtpTest", "#smtpTestResult", "/api/settings/test/smtp", () => ({
  host: $("#s_smtp_host").value.trim(),
  port: parseInt($("#s_smtp_port").value, 10) || 25,
  username: $("#s_smtp_username").value.trim(),
  password: $("#s_smtp_password").value.trim(),
  password_env: $("#s_smtp_password_env").value.trim(),
  from: $("#s_smtp_from").value.trim(),
  test_recipient: $("#s_smtp_test_recipient").value.trim(),
}));

wireConnTest("#mssqlTest", "#mssqlTestResult", "/api/settings/test/mssql", () => ({
  host: $("#s_mssql_host").value.trim(),
  port: parseInt($("#s_mssql_port").value, 10) || 1433,
  database: $("#s_mssql_database").value.trim(),
  username: $("#s_mssql_username").value.trim(),
  password_env: $("#s_mssql_password_env").value.trim(),
  password: $("#s_mssql_password").value.trim(),
  trust_server_certificate: $("#s_mssql_trust_cert").checked,
}));

wireConnTest("#shopTest", "#shopTestResult", "/api/settings/test/shop", () => ({
  base_url: $("#s_shop_base_url").value.trim(),
  username: $("#s_shop_username").value.trim(),
  password_env: $("#s_shop_password_env").value.trim(),
  password: $("#s_shop_password").value.trim(),
  client_id: $("#s_shop_client_id").value.trim(),
  client_secret_env: $("#s_shop_client_secret_env").value.trim(),
  client_secret: $("#s_shop_client_secret").value.trim(),
  timeout_seconds: parseInt($("#s_shop_timeout").value, 10) || 10,
}));

wireConnTest("#shopLoginTest", "#shopLoginTestResult", "/api/settings/test/shop-login", () => ({
  base_url: $("#s_shop_base_url").value.trim(),
  username: $("#s_shop_username").value.trim(),
  password_env: $("#s_shop_password_env").value.trim(),
  password: $("#s_shop_password").value.trim(),
  client_id: $("#s_shop_client_id").value.trim(),
  client_secret_env: $("#s_shop_client_secret_env").value.trim(),
  client_secret: $("#s_shop_client_secret").value.trim(),
  timeout_seconds: parseInt($("#s_shop_timeout").value, 10) || 10,
}));

wireConnTest("#ldapTest", "#ldapTestResult", "/api/settings/test/ldap", () => ({
  url: $("#s_ldap_url").value.trim(),
  base_dn: $("#s_ldap_base_dn").value.trim(),
  domain_prefix: $("#s_ldap_domain_prefix").value.trim(),
  required_group_dn: $("#s_ldap_required_group_dn").value.trim(),
  admin_users: $("#s_ldap_admin_users").value.split("\n").map(l => l.trim()).filter(Boolean),
  test_username: $("#s_ldap_test_username").value.trim(),
  test_password: $("#s_ldap_test_password").value,
}));

// ---- Scheduler dashboard (Settings → Hintergrund-Scheduler) ------------
// One row per enabled connection across all import connectors (see
// scheduler.go's /api/scheduler/status): schedule, live run state, last
// result, and the three per-job actions — Jetzt ausführen (ad-hoc run),
// Abbrechen (cancel a running job), Pausieren/Fortsetzen (suspend the
// automatic rhythm without forgetting the interval). Auto-refreshes while
// the panel is open so a launched/cancelled run's state change is visible
// without hammering the server when nobody is looking.

// schedJobLabel renders the scheduler's internal job key ("imap-sync:foo")
// as "imap · foo" for the table.
function schedJobLabel(st) {
  return `${st.kind} · ${st.connection}`;
}

function schedIntervalLabel(secs) {
  if (!secs || secs <= 0) return t("jobs.sched.interval.manualOnly");
  if (secs % 3600 === 0) return t("jobs.sched.interval.everyHours", { n: secs / 3600 });
  if (secs % 60 === 0) return t("jobs.sched.interval.everyMinutes", { n: secs / 60 });
  return t("jobs.sched.interval.everySeconds", { n: secs });
}

// schedAction POSTs one dashboard action and refreshes the tables —
// errors land in the status cell of the affected row via the shared
// refresh rather than an alert box.
async function schedAction(endpoint, body, btn) {
  setBusy(btn, true);
  try {
    await api(endpoint, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  } catch (err) {
    const out = $("#schedStatusTable");
    if (out) out.insertAdjacentHTML("afterbegin", `<p class="result error">${escapeHTML(err.message)}</p>`);
  } finally {
    setBusy(btn, false);
    // A just-launched run needs a beat before it shows as running; a
    // cancel needs one before the import loop notices its context.
    setTimeout(loadSchedulerStatus, 400);
    setTimeout(loadSchedulerHistory, 1200);
  }
}

async function loadSchedulerStatus() {
  const host = $("#schedStatusTable");
  if (!host) return;
  try {
    const jobs = (await api("/api/scheduler/status")) || [];
    if (!jobs.length) {
      host.innerHTML = `<p class="hint">${escapeHTML(t("jobs.sched.noJobs"))}</p>`;
      return;
    }
    const table = qtEl("table");
    table.appendChild(qtEl("thead", null, [qtEl("tr", null, [
      qtEl("th", { text: t("jobs.sched.table.job"), "data-i18n": "jobs.sched.table.job" }),
      qtEl("th", { text: t("jobs.sched.table.interval"), "data-i18n": "jobs.sched.table.interval" }),
      qtEl("th", { text: t("jobs.sched.table.status"), "data-i18n": "jobs.sched.table.status" }),
      qtEl("th", { text: t("jobs.sched.table.lastRun"), "data-i18n": "jobs.sched.table.lastRun" }),
      qtEl("th", { text: t("jobs.sched.table.actions"), "data-i18n": "jobs.sched.table.actions" }),
    ])]));
    const tbody = qtEl("tbody");
    jobs.forEach(st => {
      const status = [];
      if (st.running) {
        const since = new Date(st.running_since * 1000).toLocaleTimeString();
        status.push(t("jobs.sched.status.runningSince", { time: since }));
      } else if (st.paused) {
        status.push(t("jobs.sched.status.paused"));
      } else if (st.interval_seconds > 0) {
        status.push(st.next_run ? t("jobs.sched.status.waitingNextRun", { time: new Date(st.next_run * 1000).toLocaleTimeString() }) : t("jobs.sched.status.dueNextTick"));
      } else {
        status.push(t("jobs.sched.status.noAutoSync"));
      }
      let last = t("jobs.sched.lastRun.none");
      let lastTitle = "";
      if (st.last_run) {
        const r = st.last_run;
        last = t("jobs.sched.lastRun.summary", { icon: r.ok ? "✓" : "✗", time: new Date(r.started_at * 1000).toLocaleString(), ms: r.duration_ms, trigger: r.trigger || t("jobs.sched.trigger.auto") });
        lastTitle = r.detail || "";
      }
      const actions = qtEl("td");
      const runBtn = qtEl("button", { type: "button", class: "ghost-btn", text: t("jobs.sched.runButton"), title: t("jobs.sched.runButton.title") });
      runBtn.disabled = st.running;
      runBtn.addEventListener("click", () => schedAction("/api/scheduler/run", { job: st.job }, runBtn));
      actions.appendChild(runBtn);
      if (st.running) {
        const cancelBtn = qtEl("button", { type: "button", class: "ghost-btn qt-remove", text: t("jobs.sched.cancelButton"), title: t("jobs.sched.cancelButton.title") });
        cancelBtn.addEventListener("click", () => schedAction("/api/scheduler/cancel", { job: st.job }, cancelBtn));
        actions.appendChild(cancelBtn);
      }
      const pauseBtn = qtEl("button", { type: "button", class: "ghost-btn", text: st.paused ? t("jobs.sched.resumeButton") : t("jobs.sched.pauseButton"), title: st.paused ? t("jobs.sched.resumeButton.title") : t("jobs.sched.pauseButton.title") });
      pauseBtn.addEventListener("click", () => schedAction("/api/scheduler/pause", { job: st.job, paused: !st.paused }, pauseBtn));
      actions.appendChild(pauseBtn);
      if (st.kind === "sharepoint" || st.kind === "imap") {
        const resetBtn = qtEl("button", { type: "button", class: "ghost-btn qt-remove", text: t("jobs.sched.resetCursorButton"), title: t("jobs.sched.resetCursorButton.title") });
        resetBtn.addEventListener("click", () => {
          if (!confirm(t("jobs.sched.resetCursorButton.confirm"))) return;
          schedAction("/api/scheduler/reset-cursor", { job: st.job }, resetBtn);
        });
        actions.appendChild(resetBtn);
      }

      const lastTd = qtEl("td", { text: last, title: lastTitle });
      tbody.appendChild(qtEl("tr", null, [
        qtEl("td", { text: schedJobLabel(st), title: st.job }),
        qtEl("td", { text: schedIntervalLabel(st.interval_seconds) }),
        qtEl("td", { text: status.join(", ") }),
        lastTd,
        actions,
      ]));
    });
    table.appendChild(tbody);
    host.innerHTML = "";
    host.appendChild(table);
  } catch (err) {
    host.innerHTML = `<p class="hint">${escapeHTML(err.message)}</p>`;
  }
}

// lastSchedulerHistoryRuns caches the last-fetched history so the job
// filter (#schedHistoryJobFilter) can re-render instantly on selection
// without a round-trip — the full list is already small (schedulerHistory
// is capped at 50 entries server-side, scheduler.go).
let lastSchedulerHistoryRuns = [];

// renderSchedulerHistory renders lastSchedulerHistoryRuns, restricted to
// the job selected in #schedHistoryJobFilter (empty value = every job) —
// shared by the initial load and the filter's own change handler.
function renderSchedulerHistory() {
  const list = $("#schedHistoryList");
  if (!list) return;
  const filter = $("#schedHistoryJobFilter")?.value || "";
  const runs = filter ? lastSchedulerHistoryRuns.filter(r => r.job === filter) : lastSchedulerHistoryRuns;
  if (!runs.length) {
    const emptyKey = filter ? "jobs.history.filtered.empty" : "jobs.sched.history.empty";
    list.innerHTML = `<li class="hint">${escapeHTML(t(emptyKey))}</li>`;
    return;
  }
  list.innerHTML = runs.map(r => {
    const when = new Date(r.started_at * 1000).toLocaleString();
    const status = r.ok ? "✓" : "✗";
    const trigger = r.trigger === "manuell" ? t("jobs.sched.history.trigger.manual") : "";
    return `<li>
      <span class="pst-folder-path" title="${escapeHTML(r.detail)}">${status} ${escapeHTML(when)} · ${escapeHTML(r.job)}${trigger} — ${escapeHTML(r.detail)}</span>
      <span class="pst-folder-count">${r.duration_ms} ms</span>
    </li>`;
  }).join("");
}

async function loadSchedulerHistory() {
  const list = $("#schedHistoryList");
  const filterSel = $("#schedHistoryJobFilter");
  if (!list) return;
  try {
    lastSchedulerHistoryRuns = (await api("/api/scheduler/history")) || [];
    if (filterSel) {
      // Rebuild the option list from whatever jobs actually appear in the
      // fetched history, preserving the current selection if it's still
      // present — a job that never ran yet has nothing to filter to
      // anyway, so it's fine that it won't appear until its first run.
      const current = filterSel.value;
      const jobs = [...new Set(lastSchedulerHistoryRuns.map(r => r.job))].sort();
      filterSel.innerHTML = `<option value="">${escapeHTML(t("jobs.history.filterAll"))}</option>` +
        jobs.map(j => `<option value="${escapeHTML(j)}">${escapeHTML(j)}</option>`).join("");
      if (jobs.includes(current)) filterSel.value = current;
    }
    renderSchedulerHistory();
  } catch (err) {
    list.innerHTML = `<li class="hint">${escapeHTML(err.message)}</li>`;
  }
}
$("#schedHistoryJobFilter")?.addEventListener("change", renderSchedulerHistory);

async function loadSchedulerAlerts() {
  const host = $("#schedulerAlerts");
  if (!host) return;
  try {
    const alerts = (await api("/api/scheduler/alerts")) || [];
    const active = alerts.filter(a => !a.resolved_at);
    if (!active.length) {
      host.innerHTML = `<p class="hint">✓ Keine aktiven Scheduler-Warnungen.</p>`;
      return;
    }
    host.innerHTML = active.map(a => {
      const when = new Date(a.created_at * 1000).toLocaleString();
      const ack = a.acknowledged ? `<span class="hint">zur Kenntnis genommen</span>` :
        `<button type="button" class="ghost-btn" data-scheduler-alert-ack="${a.id}">Zur Kenntnis nehmen</button>`;
      return `<article class="scheduler-alert ${a.acknowledged ? "scheduler-alert-ack" : ""}">
        <strong>${escapeHTML(a.job)}</strong><span>${escapeHTML(when)}</span>
        <p>${escapeHTML(a.message)}</p>${ack}
      </article>`;
    }).join("");
    $all("[data-scheduler-alert-ack]").forEach(btn => btn.addEventListener("click", async () => {
      setBusy(btn, true);
      try {
        await api("/api/scheduler/alerts/ack", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id: Number(btn.dataset.schedulerAlertAck) }) });
        NovaPop.toast({ type: "success", message: "Warnung zur Kenntnis genommen." });
        loadSchedulerAlerts();
      } catch (err) {
        NovaPop.toast({ type: "error", duration: 8000, message: err.message });
      } finally { setBusy(btn, false); }
    }));
  } catch (err) {
    host.innerHTML = `<p class="result error">${escapeHTML(err.message)}</p>`;
  }
}

// loadFeedbackStats renders the Jobs tab's feedback-analysis card
// (GET /api/feedback/stats, feedback.go) — computed fresh from the
// feedback log on every call, same "no client-side caching, just re-fetch"
// approach as loadSchedulerStatus/loadSchedulerHistory above.
// feedbackStatsQuery builds the from/to/user query string for
// /api/feedback/stats from the filter form's current values — a date
// input's "YYYY-MM-DD" is turned into the start (00:00:00) / end
// (23:59:59) of that local day so the filter is inclusive of the whole
// day the admin picked, not just its midnight instant.
function feedbackStatsQuery() {
  const params = new URLSearchParams();
  const from = $("#fb_from")?.value;
  const to = $("#fb_to")?.value;
  const user = $("#fb_user")?.value.trim();
  if (from) params.set("from", String(Math.floor(new Date(from + "T00:00:00").getTime() / 1000)));
  if (to) params.set("to", String(Math.floor(new Date(to + "T23:59:59").getTime() / 1000)));
  if (user) params.set("user", user);
  return params.toString();
}

async function loadFeedbackStats() {
  const summary = $("#feedbackStatsSummary");
  const worstHost = $("#feedbackStatsWorstSources");
  const recentList = $("#feedbackStatsRecentDownvotes");
  if (!summary || !worstHost || !recentList) return;
  try {
    const stats = await api("/api/feedback/stats?" + feedbackStatsQuery());
    if (!stats.total) {
      summary.textContent = t("jobs.feedback.empty");
      worstHost.innerHTML = "";
      recentList.innerHTML = "";
      return;
    }
    const downPct = (stats.down_rate * 100).toFixed(1);
    summary.textContent = t("jobs.feedback.summary", { total: stats.total, up: stats.up, down: stats.down, pct: downPct });

    if (!stats.worst_sources || !stats.worst_sources.length) {
      worstHost.innerHTML = `<p class="hint">${escapeHTML(t("jobs.feedback.worstSources.empty"))}</p>`;
    } else {
      const table = qtEl("table");
      table.appendChild(qtEl("thead", null, [qtEl("tr", null, [
        qtEl("th", { text: t("jobs.feedback.worstSources.source") }),
        qtEl("th", { text: "👍", title: t("jobs.feedback.worstSources.upLabel"), "aria-label": t("jobs.feedback.worstSources.upLabel") }),
        qtEl("th", { text: "👎", title: t("jobs.feedback.worstSources.downLabel"), "aria-label": t("jobs.feedback.worstSources.downLabel") }),
      ])]));
      const tbody = qtEl("tbody");
      stats.worst_sources.forEach(s => {
        // Clicking the source id jumps to its actual chunks (Chunks tab,
        // pre-filtered) instead of leaving an admin to copy-paste the id
        // by hand into that tab's filter form.
        const link = qtEl("button", {
          type: "button",
          class: "link-btn",
          text: s.source_id,
          title: t("jobs.feedback.worstSources.sourceLinkTitle"),
        });
        link.addEventListener("click", () => navigateToChunksForSource(s.source_id));
        tbody.appendChild(qtEl("tr", null, [
          qtEl("td", null, [link]),
          qtEl("td", { text: String(s.up) }),
          qtEl("td", { text: String(s.down) }),
        ]));
      });
      table.appendChild(tbody);
      worstHost.innerHTML = "";
      worstHost.appendChild(table);
    }

    if (!stats.recent_downvotes || !stats.recent_downvotes.length) {
      recentList.innerHTML = `<li class="hint">${escapeHTML(t("jobs.feedback.recentDownvotes.empty"))}</li>`;
    } else {
      recentList.innerHTML = stats.recent_downvotes.map(r => {
        const when = new Date(r.time * 1000).toLocaleString();
        const cites = (r.citations || []).join(", ");
        // r.answer is the full bad-answer text (feedback.go) — the whole
        // point of this list (raw material for an eval set) needs the
        // actual text, not just the question that triggered it. Older log
        // lines written before Answer existed have only answer_hash, so
        // fall back to a placeholder rather than showing nothing.
        const answerText = r.answer || t("jobs.feedback.recentDownvotes.noAnswerText");
        return `<li>
          <span class="pst-folder-path" title="${escapeHTML(cites)}">${escapeHTML(when)} · ${escapeHTML(r.user || "anonym")} — ${escapeHTML(r.question)}</span>
          <p class="hint feedback-answer-preview" title="${escapeHTML(answerText)}">${escapeHTML(answerText)}</p>
        </li>`;
      }).join("");
    }
  } catch (err) {
    summary.textContent = err.message;
  }
}
$("#feedbackStatsFilterForm")?.addEventListener("submit", (e) => { e.preventDefault(); loadFeedbackStats(); });
$("#feedbackStatsFilterReset")?.addEventListener("click", () => {
  $("#fb_from").value = "";
  $("#fb_to").value = "";
  $("#fb_user").value = "";
  loadFeedbackStats();
});

function refreshSchedulerPanel() {
  loadSchedulerStatus();
  loadSchedulerHistory();
  loadSchedulerAlerts();
  loadFeedbackStats();
}
$("#jobsReload")?.addEventListener("click", refreshSchedulerPanel);
// Poll while the Jobs tab is actually the visible one — 10s keeps a
// running job's status/duration reasonably live without polling forever
// in the background once the admin switches to a different tab.
setInterval(() => {
  const tab = $("#tab-jobs");
  if (tab && tab.classList.contains("active") && document.visibilityState === "visible") {
    loadSchedulerStatus();
    loadSchedulerAlerts();
  }
}, 10000);

// ---- Settings: expand/collapse all accordions --------------------------
// Purely a convenience toggle over the native <details open> attribute —
// no state persisted, since which sections were open/closed isn't
// meaningful once the tab is left (unlike e.g. sidebar-collapsed).
$("#settingsExpandAll")?.addEventListener("click", () => {
  $all("#tab-settings .settings-accordion").forEach((d) => { d.open = true; });
});
$("#settingsCollapseAll")?.addEventListener("click", () => {
  $all("#tab-settings .settings-accordion").forEach((d) => { d.open = false; });
});

// ---- Settings: quick-jump nav -------------------------------------------
// A plain <a href="#id"> would rely on the browser's own fragment-scroll
// reaching into .panel.active's nested scroll container (overflow-y: auto,
// not the document root) — not guaranteed across engines. scrollIntoView
// walks every scrollable ancestor itself, so this works regardless.
$all(".settings-jumpnav a").forEach((a) => {
  a.addEventListener("click", (e) => {
    const target = document.querySelector(a.getAttribute("href"));
    if (!target) return;
    e.preventDefault();
    target.scrollIntoView({ behavior: "smooth", block: "start" });
  });
});

// ---- Settings: Aktiv/Aus-Badges je Abschnitt ----------------------------
// Each accordion whose card is governed by an "…aktivieren" checkbox
// (id ending in "_enabled") gets a live badge in its summary showing the
// checkbox's CURRENT (possibly unsaved) state — so the activation state
// of every connector is visible at a glance without opening the cards.
// s_chat_history_enabled is excluded: it toggles one feature inside the
// Chat card, not the card itself.
//
// settingsBadgeUpdaters collects each card's update() function so
// refreshSettingsBadges() (called by loadSettings once the real saved
// values are in) can re-run them without touching the DOM again —
// initSettingsBadges itself only runs once, at page load, before
// loadSettings' async /api/settings fetch has resolved. Setting a
// checkbox's .checked property in JS (as loadSettings does for every
// field) does NOT fire a "change" event, so without this, every badge
// stayed frozen on whatever the checkbox's on-page-load state was
// (unchecked ⇒ permanently "Aus") until an admin happened to click that
// exact checkbox themselves — which is exactly the bug this fixes: a
// connector configured and enabled on a previous save still showed "Aus"
// on every subsequent page load.
let settingsBadgeUpdaters = [];
function initSettingsBadges() {
  const excluded = new Set(["s_chat_history_enabled"]);
  settingsBadgeUpdaters = [];
  $all("#tab-settings .settings-accordion").forEach((card) => {
    const cb = Array.from(card.querySelectorAll('input[type="checkbox"]'))
      .find(c => c.id.endsWith("_enabled") && !excluded.has(c.id));
    if (!cb) return;
    const summary = card.querySelector("summary h2");
    if (!summary) return;
    const badge = document.createElement("span");
    badge.className = "settings-badge";
    const update = () => {
      badge.textContent = cb.checked ? t("settings.badge.active") : t("settings.badge.off");
      badge.classList.toggle("settings-badge-on", cb.checked);
    };
    update();
    cb.addEventListener("change", update);
    summary.appendChild(badge);
    settingsBadgeUpdaters.push(update);
  });
}
initSettingsBadges();

// refreshSettingsBadges re-evaluates every badge's checkbox — see
// settingsBadgeUpdaters' doc comment above for why this is needed at all
// (loadSettings sets .checked without firing "change").
function refreshSettingsBadges() {
  settingsBadgeUpdaters.forEach(update => update());
}

// ---- Settings: Suchfilter ------------------------------------------------
// Filters the accordion cards by their full text (titles, labels, hints,
// tooltips) — matching cards open up, the rest hide. Clearing the field
// restores everything (closed, matching the default view).
$("#settingsSearch")?.addEventListener("input", () => {
  const q = $("#settingsSearch").value.trim().toLowerCase();
  $all("#tab-settings .settings-accordion").forEach((card) => {
    if (!q) {
      card.hidden = false;
      return;
    }
    const text = (card.textContent + " " +
      Array.from(card.querySelectorAll("[title]")).map(el => el.title).join(" ")).toLowerCase();
    const match = text.includes(q);
    card.hidden = !match;
    if (match) card.open = true;
  });
  // Group headings with no visible card left disappear too.
  $all("#tab-settings .settings-group").forEach((group) => {
    const anyVisible = Array.from(group.querySelectorAll(".settings-accordion")).some(c => !c.hidden);
    group.hidden = !anyVisible;
  });
  // Jumping to a group makes no sense once search has already reduced the
  // view to just the matching cards.
  const jumpnav = $("#tab-settings .settings-jumpnav");
  if (jumpnav) jumpnav.hidden = !!q;
});

// ---- Settings: Ungespeichert-Indikator ----------------------------------
// Any edit inside the settings panel marks the save button until the next
// successful save (loadSettings/save reset it) — so it's always visible
// whether the current form state differs from what's on the server.
let settingsDirty = false;
function setSettingsSaveState(message, kind = "") {
  const state = $("#settingsSaveState");
  if (!state) return;
  state.textContent = message || "";
  state.classList.toggle("settings-save-state-error", kind === "error");
}

function markSettingsDirty(dirty) {
  settingsDirty = dirty;
  const btn = $("#saveSettings");
  if (!btn) return;
  btn.classList.toggle("save-dirty", dirty);
  btn.textContent = dirty ? t("settings.save.dirty") : t("settings.save.label");
  if (dirty) setSettingsSaveState(t("settings.save.unsaved"));
}

function handleSettingsEdit(e) {
  // The search box filters the view, it doesn't change any setting.
  if (e.target && e.target.id === "settingsSearch") return;
  if (!settingsDirty) markSettingsDirty(true);
}
$("#tab-settings")?.addEventListener("input", handleSettingsEdit);
$("#tab-settings")?.addEventListener("change", handleSettingsEdit);

$("#settingsReload")?.addEventListener("click", async () => {
  if (settingsDirty && !confirm(t("settings.reload.confirm"))) return;
  if (await loadSettings()) NovaPop.toast({ type: "success", message: t("settings.reload.done") });
});

// Ctrl/Cmd+S is a natural shortcut for a long administrative form. It only
// applies while Settings is visible, so it never steals the browser shortcut
// from Chat/source content or unrelated tabs.
document.addEventListener("keydown", (e) => {
  const settingsTab = $("#tab-settings");
  if (!(e.ctrlKey || e.metaKey) || e.key.toLowerCase() !== "s" || !settingsTab?.classList.contains("active")) return;
  e.preventDefault();
  if (settingsDirty && !$("#saveSettings")?.disabled) $("#saveSettings")?.click();
});

// Browser close/reload is the one navigation that cannot preserve this form
// in place. Let the browser show its native, familiar leave-page warning
// whenever an administrator would otherwise lose unsaved edits.
window.addEventListener("beforeunload", (e) => {
  if (!settingsDirty) return;
  e.preventDefault();
  e.returnValue = "";
});

// ---- Settings: Änderungshistorie ----------------------------------------
// Mirrors loadSchedulerHistory/loadAgentAudit: fetches the persisted
// field-level change log (settings_history.go) — secrets arrive already
// masked server-side (secret:true, no values).
async function loadSettingsHistory() {
  const list = $("#settingsHistoryList");
  if (!list) return;
  list.innerHTML = `<li class="hint">Lade …</li>`;
  try {
    const entries = (await api("/api/settings/history")) || [];
    if (!entries.length) {
      list.innerHTML = `<li class="hint">Noch keine aufgezeichneten Änderungen — die Historie beginnt mit dem nächsten „Einstellungen speichern“.</li>`;
      return;
    }
    list.innerHTML = entries.map(e => {
      const when = new Date(e.time * 1000).toLocaleString();
      const changes = (e.changes || []).map(c => {
        if (c.secret) {
          return `<li><code>${escapeHTML(c.path)}</code> <span class="hint">(geändert, Wert nicht protokolliert)</span></li>`;
        }
        return `<li><code>${escapeHTML(c.path)}</code>: <s>${escapeHTML(c.old || "—")}</s> → <strong>${escapeHTML(c.new || "—")}</strong></li>`;
      }).join("");
      const sourceBadge = e.source === "import" ? ` · <span class="hint">Import</span>` : "";
      return `<li class="settings-history-entry">
        <div><strong>${escapeHTML(when)}</strong> · ${escapeHTML(e.actor)} · ${(e.changes || []).length} Feld(er)${sourceBadge}</div>
        <ul class="settings-history-changes">${changes}</ul>
      </li>`;
    }).join("");
  } catch (err) {
    list.innerHTML = `<li class="hint">${escapeHTML(err.message)}</li>`;
  }
}
$("#settingsHistoryReload")?.addEventListener("click", loadSettingsHistory);
// Load automatically the first time the card is opened, so a click on the
// summary is enough — the button stays for refreshing.
$("#settingsHistoryCard")?.addEventListener("toggle", () => {
  if ($("#settingsHistoryCard").open && !$("#settingsHistoryList").children.length) loadSettingsHistory();
});

// ---- Token-usage chart (Settings → Protokoll → Token-Nutzung) ----------
// Mirrors loadSettingsHistory's fetch/render/error shape. The chart itself
// is plain flexbox divs (see .tokusage-* in style.css) — no canvas/SVG/
// charting library, consistent with the rest of this codebase's UI.
const TOKUSAGE_PROVIDER_LABELS = {
  local: "Lokal", azure: "Azure", openai: "OpenAI",
  openrouter: "OpenRouter", claude: "Claude", gemini: "Gemini",
};
const TOKUSAGE_PROVIDER_ORDER = ["local", "azure", "openai", "openrouter", "claude", "gemini"];

function tokUsageFmt(n) { return (n || 0).toLocaleString("de-DE"); }

function renderTokUsageChart(byProvider) {
  const chart = $("#tokenUsageChart");
  if (!chart) return;
  chart.innerHTML = "";
  // Fixed categorical order (the dataviz skill's "never cycled" rule) —
  // sort the server's rows into TOKUSAGE_PROVIDER_ORDER rather than
  // whatever order the SQL aggregation happened to return; skip a
  // provider with no calls in range instead of drawing an empty bar.
  const byName = Object.fromEntries(byProvider.map(r => [r.provider, r]));
  const rows = TOKUSAGE_PROVIDER_ORDER.map(p => byName[p]).filter(Boolean);
  if (!rows.length) return; // :empty CSS rule shows the "no data" hint
  const maxTotal = Math.max(...rows.map(r => r.prompt_tokens + r.completion_tokens), 1);
  rows.forEach(r => {
    const total = r.prompt_tokens + r.completion_tokens;
    const promptPct = (r.prompt_tokens / maxTotal) * 100;
    const completionPct = (r.completion_tokens / maxTotal) * 100;
    const label = TOKUSAGE_PROVIDER_LABELS[r.provider] || r.provider;
    // Prompt-cache read tokens (Azure/OpenAI automatic prefix cache and
    // Claude's cache_control) are recorded DISJOINT from prompt_tokens
    // (llm.go's upstreamChatUsage.tokenEvent, llm_claude.go), so the true
    // input total is prompt (fresh, full price) + cache-read (discounted).
    // Surface the share served from cache — the concrete payoff of the
    // Agent tier's multi-round prompt reuse on these backends. Only shown
    // when the backend actually reported cache reads (0 for local servers
    // that don't cache, so no misleading "0% cached" noise there).
    const cacheRead = r.cache_read_tokens || 0;
    let title = `${label}: ${tokUsageFmt(r.prompt_tokens)} Prompt + ${tokUsageFmt(r.completion_tokens)} Completion = ${tokUsageFmt(total)} Token(s), ${tokUsageFmt(r.calls)} Aufruf(e)`;
    if (cacheRead > 0) {
      const inputTotal = r.prompt_tokens + cacheRead;
      const hitPct = Math.round((cacheRead / inputTotal) * 100);
      title += ` — Eingabe zu ${hitPct}% aus dem Prompt-Cache (${tokUsageFmt(cacheRead)} Token vergünstigt)`;
    }
    const bar = qtEl("div", {
      class: "tokusage-bar",
      title,
    }, [
      qtEl("div", { class: "tokusage-seg-completion", style: `height:${completionPct}%` }),
      qtEl("div", { class: "tokusage-seg-prompt", style: `height:${promptPct}%` }),
    ]);
    const col = qtEl("div", { class: "tokusage-bar-col" }, [
      qtEl("div", { class: "tokusage-bar-total", text: tokUsageFmt(total) }),
      bar,
      qtEl("div", { class: "tokusage-bar-label", text: label }),
    ]);
    chart.appendChild(col);
  });
}

async function loadTokenUsage() {
  const chart = $("#tokenUsageChart");
  const tbody = $("#tokenUsageTableBody");
  if (!chart || !tbody) return;
  const days = $("#tokenUsageRange")?.value || "30";
  chart.innerHTML = "";
  tbody.innerHTML = `<tr><td colspan="5" class="hint">Lade …</td></tr>`;
  try {
    const res = await api(`/api/token-usage?days=${encodeURIComponent(days)}`);
    renderTokUsageChart(res.by_provider || []);
    const userRows = res.by_user_provider || [];
    if (!userRows.length) {
      tbody.innerHTML = `<tr><td colspan="5" class="hint">Noch keine Token-Nutzung im gewählten Zeitraum aufgezeichnet.</td></tr>`;
      return;
    }
    tbody.innerHTML = userRows.map(r => `<tr>
      <td>${escapeHTML(r.user)}</td>
      <td>${escapeHTML(TOKUSAGE_PROVIDER_LABELS[r.provider] || r.provider)}</td>
      <td>${tokUsageFmt(r.prompt_tokens)}</td>
      <td>${tokUsageFmt(r.completion_tokens)}</td>
      <td>${tokUsageFmt(r.calls)}</td>
    </tr>`).join("");
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="5" class="hint">${escapeHTML(err.message)}</td></tr>`;
  }
}
$("#tokenUsageReload")?.addEventListener("click", loadTokenUsage);
$("#tokenUsageRange")?.addEventListener("change", loadTokenUsage);
$("#tokenUsageCard")?.addEventListener("toggle", () => {
  if ($("#tokenUsageCard").open && !$("#tokenUsageTableBody").children.length) loadTokenUsage();
});

// ---- Agent tool audit (Settings → Agent) -------------------------------
// Mirrors loadSchedulerHistory: the last agent tool executions from the
// server's in-memory ring (GET /api/agent/audit, agent.go) — who ran
// which tool, how long it took, and a preview of args/result on hover.
async function loadAgentAudit() {
  const list = $("#agentAuditList");
  if (!list) return;
  list.innerHTML = `<li class="hint">Lade …</li>`;
  try {
    const runs = (await api("/api/agent/audit")) || [];
    if (!runs.length) {
      list.innerHTML = `<li class="hint">Noch keine Werkzeug-Aufrufe seit dem letzten Serverstart.</li>`;
      return;
    }
    list.innerHTML = runs.map(r => {
      const when = new Date(r.time * 1000).toLocaleString();
      const status = r.ok ? "✓" : "✗";
      const hover = `Argumente: ${r.args}\n\nErgebnis: ${r.result}`;
      // Attribute the call to its sub-agent when one ran it, so the
      // orchestration tree is visible instead of a flat list.
      const who = r.agent ? `${escapeHTML(r.user)} · 🤝 ${escapeHTML(r.agent)}` : escapeHTML(r.user);
      return `<li${r.agent ? ' class="agent-audit-sub"' : ""}>
        <span class="pst-folder-path" title="${escapeHTML(hover)}">${status} ${escapeHTML(when)} — ${who} → ${escapeHTML(r.tool)}</span>
        <span class="pst-folder-count">${r.duration_ms} ms</span>
      </li>`;
    }).join("");
  } catch (err) {
    list.innerHTML = `<li class="hint">${escapeHTML(err.message)}</li>`;
  }
}
$("#agentAuditReload")?.addEventListener("click", loadAgentAudit);
$("#agentAuditClear")?.addEventListener("click", async () => {
  try {
    await api("/api/agent/audit", { method: "POST" });
    loadAgentAudit();
  } catch (err) {
    const list = $("#agentAuditList");
    if (list) list.innerHTML = `<li class="hint">${escapeHTML(err.message)}</li>`;
  }
});

// ---- Live operations (Settings → Agent) --------------------------------
// This is intentionally separate from the audit list above: it is a live,
// bounded snapshot of currently present LDAP users and Agent-tier work, not a
// transcript of questions or tool payloads. The endpoint itself is admin-only.
let operationsPollTimer = null;

function formatOperationElapsed(ms) {
  const seconds = Math.max(0, Math.floor((ms || 0) / 1000));
  if (seconds < 60) return `${seconds} s`;
  const minutes = Math.floor(seconds / 60);
  return `${minutes} min ${seconds % 60} s`;
}

function operationsKPI(label, value) {
  return `<span class="operations-kpi">${escapeHTML(label)}: <strong>${escapeHTML(String(value || 0))}</strong></span>`;
}

async function loadOperationsStatus() {
  const summary = $("#operationsSummary");
  const usersList = $("#operationsUsers");
  const agentsList = $("#operationsAgents");
  if (!summary || !usersList || !agentsList) return;
  try {
    const status = await api("/api/admin/operations");
    const sessions = status.sessions || {};
    const agents = status.agents || {};
    summary.innerHTML = [
      operationsKPI(t("settings.operations.signedIn"), sessions.signed_in_users),
      operationsKPI(t("settings.operations.active"), sessions.active_users),
      operationsKPI(t("settings.operations.agentRuns"), agents.active_runs),
      operationsKPI(t("settings.operations.subagents"), agents.active_subagents),
      operationsKPI(t("settings.operations.tools"), agents.active_tool_calls),
    ].join("");

    const users = sessions.users || [];
    if (!users.length) {
      usersList.innerHTML = `<li class="hint">${escapeHTML(t("settings.operations.noUsers"))}</li>`;
    } else {
      usersList.innerHTML = users.map(user => {
        const name = user.display_name || user.user || t("settings.operations.unknownUser");
        const identity = [user.account_name, user.mail].filter(Boolean).join(" · ");
        const organization = [user.department, user.title, user.office, user.company].filter(Boolean).join(" · ");
        const lastSeen = user.last_seen_at ? new Date(user.last_seen_at * 1000).toLocaleString() : t("settings.operations.neverSeen");
        const state = user.active ? t("settings.operations.activeNow") : t("settings.operations.idle");
        const role = user.is_admin ? t("settings.operations.admin") : t("settings.operations.user");
        const sessionsText = t("settings.operations.sessionCount", { count: user.sessions || 1 });
        return `<li>
          <div class="operations-primary"><strong>${escapeHTML(name)}</strong><span class="operations-status${user.active ? " is-active" : ""}">${escapeHTML(state)}</span></div>
          <div class="operations-meta">${escapeHTML([identity, organization, role, sessionsText].filter(Boolean).join(" · "))}</div>
          <div class="operations-meta">${escapeHTML(t("settings.operations.lastSeen", { time: lastSeen }))}</div>
        </li>`;
      }).join("");
    }

    const runs = agents.runs || [];
    if (!runs.length) {
      agentsList.innerHTML = `<li class="hint">${escapeHTML(t("settings.operations.noAgents"))}</li>`;
    } else {
      agentsList.innerHTML = runs.map(run => {
        const tools = (run.active_tools || []).join(", ");
        const detail = [
          t("settings.operations.profile", { profile: run.profile || "-" }),
          t("settings.operations.elapsed", { time: formatOperationElapsed(run.elapsed_ms) }),
          `${t("settings.operations.subagents")}: ${run.active_subagents || 0}`,
          `${t("settings.operations.tools")}: ${run.active_tool_calls || 0}`,
        ];
        return `<li>
          <div class="operations-primary"><strong>${escapeHTML(run.user || t("settings.operations.unknownUser"))}</strong><span class="operations-status is-active">${escapeHTML(t("settings.operations.running"))}</span></div>
          <div class="operations-meta">${escapeHTML(detail.join(" · "))}</div>
          ${tools ? `<div class="operations-meta">${escapeHTML(tools)}</div>` : ""}
        </li>`;
      }).join("");
    }
  } catch (err) {
    summary.textContent = err.message;
    usersList.innerHTML = "";
    agentsList.innerHTML = "";
  }
}

function startOperationsPolling() {
  if (operationsPollTimer || !isAdmin) return;
  operationsPollTimer = setInterval(() => {
    if (!isAdmin || !$("#tab-settings")?.classList.contains("active")) {
      stopOperationsPolling();
      return;
    }
    loadOperationsStatus();
  }, 10000);
}

function stopOperationsPolling() {
  if (operationsPollTimer) {
    clearInterval(operationsPollTimer);
    operationsPollTimer = null;
  }
}

$("#operationsReload")?.addEventListener("click", loadOperationsStatus);

// ---- Storage status (Settings → Speicher) -----------------------------
// Renders the live tinySQL/sqlite status from /api/admin/storage next to
// the editable (restart-required) fields above, so an admin can compare
// "currently running with" against "will run with after the next restart"
// instead of guessing. See handlers_storage.go's storageStats/oversized
// doc comments for what each number means and vectorstore.go's
// storageSettings doc comment for why these fields aren't live.
async function loadStorageStatus() {
  const box = $("#storageStatusBox");
  if (!box) return;
  try {
    const st = await api("/api/admin/storage");
    if (st.supported === false) {
      box.className = "result";
      box.textContent = "Für das aktuell laufende Backend nicht verfügbar (nur für tinysql).";
      return;
    }
    const pct = (st.cache_hit_rate * 100).toFixed(1);
    const lines = [
      `Aktuell aktiv (seit letztem Start): Backend „${st.backend}“, Modus „${st.mode}“, Pfad „${st.path}“, ${st.chunks} Chunks`,
      `Speicher genutzt/Budget: ${st.memory_used_mb}/${st.memory_limit_mb} MB · Festplatte: ${st.disk_used_mb} MB · Cache-Trefferquote: ${pct}% · Ladevorgänge: ${st.load_count} · Verdrängungen: ${st.eviction_count}`,
    ];
    const vc = st.vector_result_cache;
    if (vc && vc.enabled) {
      const vcPct = vc.hits + vc.misses > 0 ? ((vc.hits / (vc.hits + vc.misses)) * 100).toFixed(1) : "–";
      lines.push(`Vektor-Ergebnis-Cache: ${vc.entries} Einträge · Trefferquote: ${vcPct}% (${vc.hits} Treffer / ${vc.misses} Fehltreffer) · Verdrängungen: ${vc.evictions} · Speicherbedarf: ~${Math.round(vc.approx_bytes / 1024)} KB (Heap gesamt: ${vc.heap_alloc_mb} MB)`);
    }
    if (st.oversized) {
      lines.push("Chunks-Tabelle größer als das Speicherbudget — Vektor-/Volltextindex werden bei jeder Suche neu aufgebaut, Antworten werden dadurch spürbar langsamer. Speicherbudget oben erhöhen oder Speichermodus auf „disk“ stellen (danach Neustart erforderlich).");
    }
    box.className = st.oversized ? "result error" : "result";
    box.innerHTML = lines.map(escapeHTML).join("<br>");
    renderStorageCacheQueries(vc && vc.recent_queries);
  } catch (err) {
    box.className = "result error";
    box.textContent = err.message;
  }
}

// renderStorageCacheQueries fills the "recent VEC_SEARCH queries" drill-down
// (newest first, per toVectorQueryEvents in vectorstore_tinysql.go) — hidden
// entirely when analytics has nothing yet (disabled, or no search since
// start/last AnalyticsWindow rollover) rather than showing an empty table.
function renderStorageCacheQueries(events) {
  const detail = $("#storageCacheDetail");
  if (!detail) return;
  if (!events || !events.length) {
    detail.hidden = true;
    return;
  }
  detail.hidden = false;
  const body = detail.querySelector("tbody");
  body.innerHTML = events.map(e => `
    <tr>
      <td>${new Date(e.at * 1000).toLocaleTimeString()}</td>
      <td>${escapeHTML(e.table)}</td>
      <td>${escapeHTML(e.column)}</td>
      <td>${escapeHTML(e.metric)}</td>
      <td>${escapeHTML(e.index)}</td>
      <td>${e.k}</td>
      <td>${e.cache_hit ? "Treffer" : "Fehltreffer"}</td>
      <td>${e.duration_ms} ms</td>
    </tr>`).join("");
}

// ---- API keys (Settings → API-Zugriff) --------------------------------
// Managed through their own endpoints (/api/apikeys), not the generic
// settings blob — see handlers.go's maskedSettings doc comment for why:
// key creation needs the server to generate/return a plaintext value
// exactly once, which doesn't fit the "load form, edit, POST back" shape
// the rest of the Settings tab uses.
async function loadAPIKeys() {
  try {
    const keys = await api("/api/apikeys");
    renderAPIKeyTable(keys || []);
  } catch (err) {
    $("#apiKeyTableBody").innerHTML = `<tr><td colspan="6" class="hint">${escapeHTML(err.message)}</td></tr>`;
  }
}

// renderAPIKeyTable renders the API-key management table (or an empty-
// state row) from the current list of keys.
function renderAPIKeyTable(keys) {
  const body = $("#apiKeyTableBody");
  if (!keys.length) {
    body.innerHTML = `<tr><td colspan="6" class="hint">Noch keine API-Keys erstellt.</td></tr>`;
    return;
  }
  body.innerHTML = keys.map(k => `
    <tr>
      <td>${escapeHTML(k.name)}</td>
      <td><code>${escapeHTML(k.prefix)}…</code></td>
      <td>${k.created_at ? new Date(k.created_at * 1000).toLocaleString() : ""}</td>
      <td>${k.last_used_at ? new Date(k.last_used_at * 1000).toLocaleString() : "nie"}</td>
      <td>${k.enabled ? "aktiv" : "widerrufen"}</td>
      <td>${k.enabled ? `<button type="button" class="ghost-btn" data-revoke="${escapeHTML(k.id)}">Widerrufen</button>` : ""}</td>
    </tr>`).join("");
}

$("#apiKeyTableBody").addEventListener("click", async (e) => {
  const id = e.target.dataset && e.target.dataset.revoke;
  if (!id) return;
  if (!confirm(t("confirm.revokeApiKey"))) return;
  try {
    await api("/api/apikeys/revoke", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id }),
    });
    loadAPIKeys();
  } catch (err) {
    alert(err.message);
  }
});

$("#apiKeyCreateForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("#apiKeyCreateResult");
  const nameInput = $("#apiKeyName");
  out.className = "result";
  try {
    const res = await api("/api/apikeys", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: nameInput.value.trim() }),
    });
    nameInput.value = "";
    out.innerHTML = `Key erstellt — <strong>jetzt kopieren, wird nicht wieder angezeigt:</strong><br><code>${escapeHTML(res.key)}</code>`;
    loadAPIKeys();
  } catch (err) {
    out.className = "result error";
    out.textContent = err.message;
  }
});

loadSettings();
