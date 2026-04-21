// Small DOM helpers: query single / multiple elements
const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

// Maximum characters passed to the TTS endpoint to avoid overly long audio.
const MAX_TTS_LENGTH = 4096;

// ---------------------------------------------------------------------------
// Utility helpers
// - Keep small, well-tested helpers near the top for quick inspection.
// - These are intentionally simple and synchronous to avoid side-effects.
// ---------------------------------------------------------------------------

/**
 * Escape text for safe insertion into HTML.
 * Prevents simple XSS when rendering user-provided content.
 * This function intentionally only handles a small set of characters.
 */
function escHtml(s){
  return s.replace(/[&<>"']/g, (c) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

/**
 * Render Markdown to HTML using marked if available.
 * - When `marked` is present we sanitize any raw HTML tokens.
 * - On parsing errors or if `marked` is absent we fall back to a safe
 *   plain-text rendering with newlines converted to <br>.
 */
function renderMarkdown(md){
  if(!md) return '';
  if(window.marked){
    try{
      const renderer = new marked.Renderer();
      const origHtml = renderer.html ? renderer.html.bind(renderer) : null;
      renderer.html = function(token){
        // block raw HTML injection – escape it
        const raw = (typeof token === 'string') ? token : (token && token.raw ? token.raw : String(token));
        return escHtml(raw);
      };

      // Set options for syntax highlighting if highlight.js is present
      const options = { renderer, breaks: true, gfm: true };
      if(window.hljs){
        options.highlight = (code, lang) => {
          if(lang && hljs.getLanguage(lang)){
            return hljs.highlight(code, { language: lang }).value;
          }
          return hljs.highlightAuto(code).value;
        };
      }
      return marked.parse(md, options);
    }catch(e){
      // fallback on parse error
      return escHtml(md).replace(/\n/g, '<br>');
    }
  }
  return escHtml(md).replace(/\n/g, '<br>');
}

/**
 * Render any Mermaid diagrams found inside a container element.
 * This is async because Mermaid's render API is async; errors are shown
 * inline so the rest of the page still loads.
 */
async function renderMermaidIn(el){
  if(!window.mermaid) return;
  const nodes = el.querySelectorAll('code.language-mermaid, code.lang-mermaid, code.mermaid');
  for(const code of nodes){
    const src = code.textContent;
    const parent = code.closest('pre') || code;
    const holder = document.createElement('div');
    holder.className = 'mermaid-render';
    parent.replaceWith(holder);
    try{
      const {svg} = await mermaid.render('mmd-'+Math.random().toString(16).slice(2), src);
      holder.innerHTML = svg;
    }catch(err){
      holder.textContent = 'Mermaid-Fehler: '+(err?.message||err);
      holder.classList.add('tool-status','err');
    }
  }
}

/**
 * Populate a chat bubble element with rendered content.
 * - Stores the raw content on `data-raw` for debugging/inspection.
 * - Uses Markdown rendering and attempts to render Mermaid diagrams.
 */
function renderBubbleContent(el, content){
  el.dataset.raw = content;
  const html = renderMarkdown(content);
  el.innerHTML = html;
  // If marked.js isn't loaded, use pre-wrap for plain text fallback
  el.classList.toggle('plain', !window.marked);
  renderMermaidIn(el);
  
  // Syntax highlighting for code blocks if highlight.js is present
  if(window.hljs){
    el.querySelectorAll('pre code').forEach((block) => {
      hljs.highlightElement(block);
    });
    // Add copy buttons to code blocks
    el.querySelectorAll('pre').forEach(pre => {
      if(pre.querySelector('.code-copy-btn')) return;
      const btn = document.createElement('button');
      btn.className = 'code-copy-btn';
      btn.innerHTML = '📋';
      btn.title = 'Code kopieren';
      btn.onclick = () => {
        const code = pre.querySelector('code')?.innerText || pre.innerText;
        copyToClipboard(code, btn);
      };
      pre.appendChild(btn);
    });
  }
}

function createThinkingPanel(thinking){
  const details = document.createElement('details');
  details.className = 'thinking-panel';
  const summary = document.createElement('summary');
  summary.textContent = 'Reasoning anzeigen';
  const body = document.createElement('div');
  body.className = 'thinking-body';
  renderBubbleContent(body, thinking || '');
  details.appendChild(summary);
  details.appendChild(body);
  return details;
}

/**
 * Copy text to clipboard using the modern API.
 * Shows a temporary success state on the triggering element if provided.
 */
async function copyToClipboard(text, el){
  try{
    if(!text) return;
    await navigator.clipboard.writeText(text);
    if(el){
      const old = el.innerHTML;
      el.innerHTML = '✓';
      el.classList.add('success');
      setTimeout(() => { 
        el.innerHTML = old; 
        el.classList.remove('success');
      }, 2000);
    }
  }catch(e){
    console.error('Copy failed', e);
  }
}

// Short human-friendly timestamp for compact chat headers
function timeShort(iso){
  try{
    const d = new Date(iso);
    return d.toLocaleString(undefined, {hour:'2-digit', minute:'2-digit', year:'2-digit', month:'2-digit', day:'2-digit'});
  }catch(e){ return ''; }
}

/**
 * Simple API helpers for GET/POST
 * - `apiGet` expects a JSON response and throws on non-2xx.
 * - `apiPost` returns parsed JSON or text payload; on non-2xx it throws
 *   an Error object with `.status` and `.payload` for richer handling
 *   (useful for detecting `409` server responses with JSON bodies).
 */
async function apiGet(path){
  const r = await fetch(path);
  if(!r.ok) throw new Error(await r.text());
  return await r.json();
}
async function apiPost(path, body){
  const r = await fetch(path, {
    method:'POST',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify(body||{})
  });
  // Some endpoints return 409 with JSON
  const ct = r.headers.get('content-type')||'';
  const isJson = ct.includes('application/json');
  const payload = isJson ? await r.json().catch(()=>null) : await r.text();
  if(!r.ok){
    const msg = (payload && payload.message) ? payload.message : (typeof payload === 'string' ? payload : JSON.stringify(payload));
    const err = new Error(msg || ('HTTP '+r.status));
    err.status = r.status;
    err.payload = payload;
    throw err;
  }
  return payload;
}

// Set a short status message into an element.
// `cls` can be 'ok' or 'err' to apply simple coloring via CSS.
function setStatus(el, msg, cls){
  el.textContent = msg;
  el.className = 'tool-status' + (cls ? ' '+cls : '');
}

// Toggle a loading state on a button or element.
// Accepts either a selector string or an element reference.
function setLoading(selectorOrEl, on){
  let el = (typeof selectorOrEl === 'string') ? document.querySelector(selectorOrEl) : selectorOrEl;
  if(!el) return;
  if(on){
    el.disabled = true;
    el.classList.add('loading');
  }else{
    el.disabled = false;
    el.classList.remove('loading');
  }
}

/**
 * Helper for quick-start suggestions in empty chat state.
 * Fills the chat input and triggers the request.
 */
window.fillChat = function(txt){
  const q = $('#chatQ');
  if(q){
    q.value = txt;
    autosize(q);
    askChat();
  }
};

function buildEmptyStateHtml(){
  return `
    <div class="empty-state" id="chatEmpty">
      <div class="icon" aria-hidden="true">💬</div>
      <p data-i18n="chat_empty_state">${t('chat_empty_state')}</p>
      <div class="chat-suggestions">
        <button class="suggestion-btn" onclick="fillChat('${t('sugg_summarize_val')}')">📝 <span>${t('sugg_summarize')}</span></button>
        <button class="suggestion-btn" onclick="fillChat('${t('sugg_newest_val')}')">🆕 <span>${t('sugg_newest')}</span></button>
        <button class="suggestion-btn" onclick="fillChat('${t('sugg_analyze_val')}')">🔍 <span>${t('sugg_analyze')}</span></button>
      </div>
    </div>`;
}

// resolveProviderName maps a base URL to a human-friendly provider name.
// It uses URL hostname parsing where possible to avoid substring-matching
// against arbitrary host prefixes/suffixes.
function resolveProviderName(url){
  if(!url) return '';
  // Parse the URL once, normalising bare host:port strings first.
  let parsed = null;
  try{
    const raw = url.trim();
    parsed = new URL(raw.includes('://') ? raw : 'https://'+raw);
  }catch(e){
    // Unparseable – treat as remote.
    return 'Remote LLM';
  }
  const hostname = parsed.hostname.toLowerCase();
  const port = parsed.port;
  if(hostname === 'api.openai.com' || hostname.endsWith('.openai.com')) return 'OpenAI';
  if(hostname === 'api.anthropic.com' || hostname.endsWith('.anthropic.com')) return 'Anthropic';
  if(hostname.endsWith('.googleapis.com') || hostname === 'generativelanguage.googleapis.com') return 'Google Gemini';
  if(hostname === 'api.mistral.ai' || hostname.endsWith('.mistral.ai')) return 'Mistral AI';
  if(hostname === 'api.groq.com' || hostname.endsWith('.groq.com')) return 'Groq';
  if(hostname === 'api.deepseek.com' || hostname.endsWith('.deepseek.com')) return 'DeepSeek';
  if(hostname.endsWith('.together.xyz') || hostname.endsWith('.together.ai')) return 'Together AI';
  if(hostname === 'api.x.ai' || hostname.endsWith('.x.ai')) return 'xAI';
  if(hostname === 'api.cohere.com' || hostname.endsWith('.cohere.com') || hostname === 'api.cohere.ai') return 'Cohere';
  if(hostname === 'openrouter.ai' || hostname.endsWith('.openrouter.ai')) return 'OpenRouter';
  if(hostname === 'localhost' || hostname === '127.0.0.1'){
    if(port === '11434') return 'Ollama';
    if(port === '1234') return 'LM Studio';
    return 'Local LLM';
  }
  return 'Remote LLM';
}

function setSidebarOpen(open){
  const sidebar = $('#appSidebar');
  const toggle = $('#sidebarToggle');
  if(!sidebar || !toggle) return;
  sidebar.classList.toggle('open', !!open);
  toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
  document.body.classList.toggle('sidebar-open', !!open);
}

function closeSidebarIfMobile(){
  if(window.innerWidth <= 960) setSidebarOpen(false);
}

function currentModeSummary(){
  const modes = [];
  if(autoSearchMode) modes.push('Auto-Search');
  if(deepMode) modes.push('Deep Research');
  if(offlineMode) modes.push('Offline');
  return modes.length ? modes.join(' · ') : 'Standard';
}

function roleLabelForUI(role){
  const map = {
    it: t('role_it'),
    logistik: t('role_logistics'),
    vertrieb: t('role_sales'),
    hr: t('role_hr')
  };
  return map[role] || String(role || 'IT');
}

function applyRolePermissionsUI(perms){
  currentRolePermissions = perms || null;
  const p = currentRolePermissions || {can_web_fetch: true, can_bulk_ingest: true};

  const setDisabled = (sel, disabled, title)=>{
    const el = $(sel);
    if(!el) return;
    el.disabled = !!disabled;
    if(disabled && title){
      el.title = title;
    }else if(!disabled){
      el.removeAttribute('title');
    }
  };

  const noWeb = !p.can_web_fetch;
  const noBulk = !p.can_bulk_ingest;
  setDisabled('#wikiBtn', noWeb, 'Für diese Rolle nicht erlaubt');
  setDisabled('#urlBtn', noWeb, 'Für diese Rolle nicht erlaubt');
  setDisabled('#folderBtn', noBulk, 'Für diese Rolle nicht erlaubt');
  setDisabled('#fileInput', noBulk, 'Für diese Rolle nicht erlaubt');
}

function updateWorkspaceStrip(){
  const providerEl = $('#providerPill strong');
  const personaEl = $('#personaPill strong');
  const roleEl = $('#rolePill strong');
  const modeEl = $('#modePill strong');
  if(providerEl){
    providerEl.textContent = $('#openaiBadge strong')?.textContent?.trim() || 'LLM';
  }
  if(personaEl){
    const persona = cachedPersonas.find(p => p.id === currentPersonaId);
    personaEl.textContent = persona?.name || $('#personaSelect')?.selectedOptions?.[0]?.textContent || 'Standard';
  }
  if(roleEl){
    roleEl.textContent = roleLabelForUI(currentRole);
  }
  if(modeEl){
    modeEl.textContent = currentModeSummary();
  }
}

async function setActiveRole(role){
  try{
    const resp = await apiPost('/api/settings/role', {role});
    currentRole = resp.role || normalizeRoleValue(role);
    applyRolePermissionsUI(resp.permissions || null);
    const sel = $('#roleSelect');
    if(sel) sel.value = currentRole;
    syncIngestRoleScopesWithActiveRole();
    updateWorkspaceStrip();
  }catch(err){
    const sel = $('#roleSelect');
    if(sel) sel.value = currentRole;
    alert('Rollenwechsel fehlgeschlagen: ' + (err.message || String(err)));
  }
}

function normalizeRoleValue(role){
  const v = String(role || '').trim().toLowerCase();
  if(v === 'logistics') return 'logistik';
  if(v === 'sales') return 'vertrieb';
  if(v === 'human-resources' || v === 'human_resources') return 'hr';
  if(['it','logistik','vertrieb','hr'].includes(v)) return v;
  return 'it';
}

function fillChatWithSearchHit(content){
  const q = $('#chatQ');
  if(!q) return;
  const excerpt = (content || '').trim().slice(0, 500);
  q.value = `Nutze diesen Treffer als Kontext und beantworte meine Frage präzise:\n\n${excerpt}`;
  autosize(q);
  showTab('main', 'chat');
  q.focus();
}

function stripToolRequestText(raw){
  if(!raw) return '';
  // Strip legacy [TOOL_REQUEST]...[/TOOL_REQUEST] blocks
  const start = raw.indexOf('[TOOL_REQUEST]');
  if(start !== -1){
    const endMarker = '[/TOOL_REQUEST]';
    const end = raw.indexOf(endMarker, start);
    if(end === -1){
      raw = raw.slice(0, start).trim();
    } else {
      raw = (raw.slice(0, start) + raw.slice(end + endMarker.length)).trim();
    }
  }
  // Strip XML <tool ...>...</tool> blocks
  raw = stripXMLToolBlocks(raw);
  return raw.trim();
}

/**
 * Strip all <tool name="...">...</tool> XML blocks from text.
 * Used before final markdown rendering so the raw XML is not shown to users.
 */
function stripXMLToolBlocks(raw){
  if(!raw) return '';
  return raw.replace(/<tool\s[^>]*>[\s\S]*?<\/tool>/gi, '').trim();
}

// Maximum characters of source code to show in tool cards.
const MAX_TOOL_SOURCE_PREVIEW = 80;

/**
 * Replace <tool name="...">...</tool> XML blocks with styled status cards.
 * Extracted blocks are replaced with formatted HTML before markdown rendering.
 */
function replaceXMLToolBlocksWithCards(raw){
  if(!raw) return raw;
  return raw.replace(/<tool\s+name="([^"]*)">([\s\S]*?)<\/tool>/gi, (match, toolName, inner) => {
    // Extract the query/url/source content
    let content = '';
    const queryMatch = inner.match(/<query>([\s\S]*?)<\/query>/i);
    const urlMatch = inner.match(/<url>([\s\S]*?)<\/url>/i);
    const sourceMatch = inner.match(/<source>([\s\S]*?)<\/source>/i);
    if(queryMatch) content = queryMatch[1].trim();
    else if(urlMatch) content = urlMatch[1].trim();
    else if(sourceMatch) content = sourceMatch[1].trim().slice(0, MAX_TOOL_SOURCE_PREVIEW)+'…';
    const icon = toolIconFor(toolName);
    return `\n\n<div class="xml-tool-card" data-tool="${escHtml(toolName)}" data-status="running">` +
      `<span class="tool-icon">${icon}</span>` +
      `<strong>${escHtml(toolName)}</strong>` +
      (content ? ` <span class="tool-query">${escHtml(content)}</span>` : '') +
      `<span class="tool-status-badge">⟳</span></div>\n\n`;
  });
}

function toolIconFor(name){
  const icons = {
    rag_knowledge:'🔍', local_search:'🔍', vector_query:'🔍',
    url_fetch:'🌐', wikipedia:'📖', websearch:'🌐',
    duckduckgo:'🦆', stackoverflow:'💬', github:'🐙',
    nanogo:'⚙️', tinygo:'⚙️', calculate:'🧮',
    sql_query:'🗄️', datetime:'🕐', news:'📰',
    wikidata:'🧩', wiktionary:'📚', shell:'💻',
  };
  return icons[name] || '🔧';
}

function stripInternalThinking(raw){
  if(!raw) return '';
  let cleaned = raw.replace(/<think>[\s\S]*?<\/think>/gi, '').replace(/\[THINK\][\s\S]*?\[\/THINK\]/g, '');
  cleaned = cleaned.replace(/(<think>|\[THINK\])[\s\S]*$/i, '');
  return cleaned.trim();
}

// Read the ingest embed-model selector UI and return the effective model string.
// Priority: select value -> custom input when 'custom' selected -> empty string
function getIngestEmbedModel(){
  const sel = document.querySelector('#ingestEmbedSelect');
  const custom = document.querySelector('#ingestEmbedCustom');
  if(!sel) return custom ? custom.value.trim() : '';
  const v = sel.value;
  if(v === 'custom') return custom ? custom.value.trim() : '';
  return v || '';
}

function getIngestRoleScopes(){
  const checks = Array.from(document.querySelectorAll('.ingest-role-scope'));
  const selected = checks.filter(c => c.checked).map(c => normalizeRoleValue(c.value));
  if(selected.length) return Array.from(new Set(selected));
  return [normalizeRoleValue(currentRole || 'it')];
}

function syncIngestRoleScopesWithActiveRole(force=false){
  const checks = Array.from(document.querySelectorAll('.ingest-role-scope'));
  if(!checks.length) return;
  const target = normalizeRoleValue(currentRole || 'it');
  if(force){
    checks.forEach(c => { c.checked = normalizeRoleValue(c.value) === target; });
    return;
  }
  const anyChecked = checks.some(c => c.checked);
  if(anyChecked) return;
  checks.forEach(c => { c.checked = normalizeRoleValue(c.value) === target; });
}

// --- i18n simple helper
const _translations = {
  de: {
    loading: 'Lade…',
    scrape: 'Scrape…',
    saving: 'Speichere…',
    importing: 'Importiere…',
    uploading: 'Upload…',
    ok_chunks: (chunks, total) => `OK: ${chunks} Chunks hinzugefügt. Total: ${total}`,
    not_found_intro: 'Nicht gefunden. Meintest du:',
    error_prefix: 'Fehler: ',
    assistant_typing: 'Assistent denkt nach',
    // New translations for UI elements
    skip_to_main: 'Zum Hauptinhalt springen',
    chunks_in_knowledge_base: 'Chunks in der Wissensbasis',
    new_chat: '+ Neuer Chat',
    chats: 'Chats',
    sources: 'Quellen',
    chat: 'Chat',
    search: 'Suche',
    ingest: 'Daten hinzufügen',
    persona: 'Persona',
    role: 'Rolle',
    role_it: 'IT',
    role_logistics: 'Logistik',
    role_sales: 'Vertrieb',
    role_hr: 'HR',
    import_roles: 'Import-Sichtbarkeit (Rollen)',
    import_roles_hint: 'Keine Auswahl = aktive Rolle. Mehrfachauswahl erlaubt (wichtig für Massenupload).',
    debug: 'Debug',
    debug_description: 'Zeigt die RAG-Kontextdaten an, die das System für die Antwort verwendet',
    auto_search: 'Auto-Search',
    deep_research: 'Deep Research',
    offline_mode: 'Offline',
    chat_disclaimer: 'tinyRAG kann Fehler machen. Wichtige Informationen prüfen.',
    settings: 'Einstellungen',
    chat_empty_state: 'Stelle eine Frage an deine Wissensbasis.<br>Die Antwort basiert auf den gespeicherten Dokumenten.',
    chat_input_label: 'Ihre Frage eingeben',
    chat_input_placeholder: 'Frage eingeben…',
    send: 'Senden',
    search_label: 'Semantische Suche in den Chunks',
    search_placeholder: 'Suchbegriff…',
    search_button: 'Suchen',
    wikipedia: 'Wikipedia',
    url: 'URL',
    text: 'Text',
    upload: 'Upload',
    folder: 'Ordner',
    wiki_label: 'Wikipedia-Artikel laden',
    wiki_placeholder: 'z.B. Sonnensystem',
    wiki_lang_label: 'Wikipedia-Sprachcode',
    load: 'Laden',
    url_label: 'Beliebige Webseite scrapen',
    url_placeholder: 'https://example.com/page',
    scrape: 'Scrapen',
    text_title_label: 'Titel (optional)',
    text_title_placeholder: 'Mein Dokument',
    text_content_label: 'Text',
    text_content_placeholder: 'Text hier einfügen…',
    save_embed: 'Speichern & Einbetten',
    upload_label: 'Textdatei hochladen',
    drop_zone_text: 'Datei hierher ziehen oder klicken',
    folder_label: 'Ordner importieren (alle Textdateien)',
    folder_hint: 'Absoluter Pfad zum Ordner auf dem Server. Unterstützte Dateien: .txt, .md, .csv, .json, .xml, .html, .log u.a.',
    folder_placeholder: '/pfad/zum/ordner',
    recursive: 'Rekursiv',
    import: 'Importieren',
    settings_title: 'Einstellungen',
    general: 'Allgemein',
    llm_backend: 'LLM Backend',
    custom_apis: 'Custom APIs',
    modules: 'Module',
    modules_hint: 'Module verbinden externe Quellen mit tinyRAG. Testen zeigt Rohdaten, Ingest uebernimmt normalisierten Text in die Wissensbasis.',
    personas: 'Personas',
    appearance: 'Erscheinungsbild',
    language: 'Sprache / Language',
    usage_profile: 'Nutzungsprofil',
    usage_profile_personal: 'Persönlich',
    usage_profile_commercial: 'Gewerblich',
    response_language_mode: 'Antwortsprache',
    response_language_auto: 'Automatisch (Sprache der Anfrage)',
    response_language_settings: 'Fix nach Einstellungssprache',
    response_language_hint: 'Auto eignet sich für europaweite Teams mit mehreren Sprachen.',
    theme: 'Theme',
    endpoint_note: 'Hinweis: Das Endpoint-Feld erwartet die Basis-URL ohne <code>/v1</code>. Falls du <code>/v1</code> einfügst, wird es automatisch entfernt.',
    allow_nanogo: 'Erlaube Ausführung von nanoGo (interpretiertes Go)',
    nanogo_hint: 'Empfohlen: nur aktivieren, wenn du Ausführungen aus vertrauenswürdigen Quellen zulassen willst.',
    redact_pii: 'PII beim Ingest maskieren (E-Mail, Telefon, IBAN, Kartenmuster)',
    redact_pii_hint: 'Empfohlen für gewerbliche Nutzung und sensible Daten (DSGVO).',
    api_endpoint: 'API Endpoint (OpenAI-kompatibel)',
    auto_discovery: 'Auto-Discovery',
    test_load_models: 'Test & Modelle laden',
    chat_model: 'Chat-Modell',
    embedding_model: 'Embedding-Modell',
    save: 'Speichern',
    add_new_api: 'Neue API hinzufügen',
    api_name: 'Name',
    api_name_placeholder: 'z.B. StackOverflow',
    api_template: 'URL-Template (mit $q)',
    api_template_placeholder: 'https://example.com/search?q=$q',
    api_description: 'Beschreibung (optional)',
    api_desc_placeholder: 'Wofür ist die Quelle gut?',
    add: 'Hinzufügen',
    new_persona: 'Neue Persona',
    persona_name: 'Name',
    persona_name_placeholder: 'z.B. Sachlicher Modus',
    persona_prompt: 'Pre-Prompt',
    persona_prompt_placeholder: 'Instruktionen, Tonalität, Stil…',
    maintenance: 'Wartung',
    clear_chunks: 'Wissensbasis leeren',
    clear_chunks_info: 'Lösche alle Chunks in der Wissensbasis. Diese Aktion kann nicht rückgängig gemacht werden.',
    clear_confirm: 'Wirklich ALLES löschen? Das kann nicht rückgängig gemacht werden.',
    cleared: 'Alle Chunks gelöscht.',
    copy: 'Kopieren',
    sugg_summarize: 'Zusammenfassen',
    sugg_summarize_val: 'Fasse die wichtigsten Infos aus meiner Wissensbasis zusammen.',
    sugg_newest: 'Was ist neu?',
    sugg_newest_val: 'Welche Dokumente wurden zuletzt hinzugefügt und was ist deren Inhalt?',
    sugg_analyze: 'Analyse',
    sugg_analyze_val: 'Hilf mir bei der Analyse der vorhandenen Daten.',
    upload_label: 'Datei hochladen (Text, PDF, DOCX, ODT, PPTX, XLSX)',
    folder_hint: 'Absoluter Pfad zum Ordner auf dem Server. Unterstützte Dateien: .txt, .md, .csv, .json, .xml, .html, .log, .pdf, .docx, .odt u.a.',
    stt_listening: 'Höre zu…',
    stt_error: 'Spracheingabe fehlgeschlagen',
    tts_speak: 'Vorlesen',
    tts_stop: 'Stopp',
    attach_image: 'Bild anhängen',
    remove_image: 'Bild entfernen',
    vision_mode: 'Bildanalyse',
    image_label: '[Bild]',
    processing: 'Wird bearbeitet…',
    error: 'Fehler'
  },
  en: {
    loading: 'Loading…',
    scrape: 'Scraping…',
    saving: 'Saving…',
    importing: 'Importing…',
    uploading: 'Uploading…',
    ok_chunks: (chunks, total) => `OK: ${chunks} chunks added. Total: ${total}`,
    not_found_intro: 'Not found. Did you mean:',
    error_prefix: 'Error: ',
    assistant_typing: 'Assistant is thinking',
    // New translations for UI elements
    skip_to_main: 'Skip to main content',
    chunks_in_knowledge_base: 'Chunks in knowledge base',
    new_chat: '+ New Chat',
    chats: 'Chats',
    sources: 'Sources',
    chat: 'Chat',
    search: 'Search',
    ingest: 'Add Data',
    persona: 'Persona',
    role: 'Role',
    role_it: 'IT',
    role_logistics: 'Logistics',
    role_sales: 'Sales',
    role_hr: 'HR',
    import_roles: 'Import Visibility (Roles)',
    import_roles_hint: 'No selection = active role. Multi-select supported (important for bulk upload).',
    debug: 'Debug',
    debug_description: 'Shows RAG context data that the system uses for the response',
    auto_search: 'Auto-Search',
    deep_research: 'Deep Research',
    offline_mode: 'Offline',
    chat_disclaimer: 'tinyRAG can make mistakes. Check important info.',
    settings: 'Settings',
    chat_empty_state: 'Ask a question about your knowledge base.<br>The answer is based on stored documents.',
    chat_input_label: 'Enter your question',
    chat_input_placeholder: 'Enter question…',
    send: 'Send',
    search_label: 'Semantic search in chunks',
    search_placeholder: 'Search term…',
    search_button: 'Search',
    wikipedia: 'Wikipedia',
    url: 'URL',
    text: 'Text',
    upload: 'Upload',
    folder: 'Folder',
    wiki_label: 'Load Wikipedia article',
    wiki_placeholder: 'e.g., Solar System',
    wiki_lang_label: 'Wikipedia language code',
    load: 'Load',
    url_label: 'Scrape any webpage',
    url_placeholder: 'https://example.com/page',
    scrape: 'Scrape',
    text_title_label: 'Title (optional)',
    text_title_placeholder: 'My Document',
    text_content_label: 'Text',
    text_content_placeholder: 'Paste text here…',
    save_embed: 'Save & Embed',
    upload_label: 'Upload text file',
    drop_zone_text: 'Drag file here or click',
    folder_label: 'Import folder (all text files)',
    folder_hint: 'Absolute path to folder on server. Supported files: .txt, .md, .csv, .json, .xml, .html, .log, etc.',
    folder_placeholder: '/path/to/folder',
    recursive: 'Recursive',
    import: 'Import',
    settings_title: 'Settings',
    general: 'General',
    llm_backend: 'LLM Backend',
    custom_apis: 'Custom APIs',
    modules: 'Modules',
    modules_hint: 'Modules connect external sources to tinyRAG. Test previews raw data, ingest writes normalized text into the knowledge base.',
    personas: 'Personas',
    appearance: 'Appearance',
    language: 'Language',
    usage_profile: 'Usage Profile',
    usage_profile_personal: 'Personal',
    usage_profile_commercial: 'Commercial',
    response_language_mode: 'Response Language',
    response_language_auto: 'Automatic (language of the request)',
    response_language_settings: 'Fixed by selected UI language',
    response_language_hint: 'Auto mode is recommended for multilingual European teams.',
    theme: 'Theme',
    endpoint_note: 'Note: The endpoint field expects the base URL without <code>/v1</code>. If you enter <code>/v1</code>, it will be automatically removed.',
    allow_nanogo: 'Allow execution of nanoGo (interpreted Go)',
    nanogo_hint: 'Recommended: only enable if you want to allow executions from trusted sources.',
    redact_pii: 'Redact PII on ingest (email, phone, IBAN, card patterns)',
    redact_pii_hint: 'Recommended for commercial usage and sensitive data (GDPR).',
    api_endpoint: 'API Endpoint (OpenAI-compatible)',
    auto_discovery: 'Auto-Discovery',
    test_load_models: 'Test & Load Models',
    chat_model: 'Chat Model',
    embedding_model: 'Embedding Model',
    save: 'Save',
    add_new_api: 'Add New API',
    api_name: 'Name',
    api_name_placeholder: 'e.g., StackOverflow',
    api_template: 'URL Template (with $q)',
    api_template_placeholder: 'https://example.com/search?q=$q',
    api_description: 'Description (optional)',
    api_desc_placeholder: 'What is the source good for?',
    add: 'Add',
    new_persona: 'New Persona',
    persona_name: 'Name',
    persona_name_placeholder: 'e.g., Formal Mode',
    persona_prompt: 'Pre-Prompt',
    persona_prompt_placeholder: 'Instructions, tone, style…',
    maintenance: 'Maintenance',
    clear_chunks: 'Clear Knowledge Base',
    clear_chunks_info: 'Delete all chunks in the knowledge base. This action cannot be undone.',
    clear_confirm: 'Really delete EVERYTHING? This cannot be undone.',
    cleared: 'All chunks cleared.',
    copy: 'Copy',
    sugg_summarize: 'Summarize',
    sugg_summarize_val: 'Summarize the most important info from my knowledge base.',
    sugg_newest: 'What\'s new?',
    sugg_newest_val: 'What documents were added recently and what is their content?',
    sugg_analyze: 'Analyze',
    sugg_analyze_val: 'Help me analyze the existing data.',
    upload_label: 'Upload file (Text, PDF, DOCX, ODT, PPTX, XLSX)',
    folder_hint: 'Absolute path to folder on server. Supported files: .txt, .md, .csv, .json, .xml, .html, .log, .pdf, .docx, .odt, etc.',
    stt_listening: 'Listening…',
    stt_error: 'Voice input failed',
    tts_speak: 'Read aloud',
    tts_stop: 'Stop',
    attach_image: 'Attach image',
    remove_image: 'Remove image',
    vision_mode: 'Image analysis',
    image_label: '[image]',
    processing: 'Processing…',
    error: 'Error'
  }
};

let uiLang = 'de';
function t(key, ...args){
  const tr = (_translations[uiLang] || _translations.de)[key];
  if(typeof tr === 'function') return tr(...args);
  return tr || key;
}

function applyTranslations(lang){
  uiLang = (lang||'de').split('-')[0];
  
  // Update HTML lang attribute
  document.documentElement.setAttribute('lang', uiLang);
  
  // Apply data-i18n translations
  document.querySelectorAll('[data-i18n]').forEach(el => {
    const key = el.getAttribute('data-i18n');
    const text = t(key);
    if(text !== key){
      el.innerHTML = text;
    }
  });
  
  // Apply data-i18n-placeholder translations
  document.querySelectorAll('[data-i18n-placeholder]').forEach(el => {
    const key = el.getAttribute('data-i18n-placeholder');
    const text = t(key);
    if(text !== key){
      el.placeholder = text;
    }
  });
  
  // Update language selector
  const langSelect = document.getElementById('langSelect');
  if(langSelect){
    langSelect.value = uiLang;
  }
}

function autosize(el){
  if(!el) return;
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 200) + 'px';
}

// ═══════ Theme system ═══════
const THEMES = ['dark','light','nord','solarized','monokai','dracula'];
let currentTheme = 'dark';

function applyTheme(id){
  if(!THEMES.includes(id)) id = 'dark';
  currentTheme = id;
  document.body.setAttribute('data-theme', id);
  // Update meta color-scheme for browser chrome
  const meta = document.querySelector('meta[name="color-scheme"]');
  if(meta) meta.content = (id === 'light') ? 'light' : 'dark';
  // Update theme cards active state and ARIA attributes
  $$('.theme-card').forEach(c => {
    const isActive = c.dataset.themeId === id;
    c.classList.toggle('active', isActive);
    c.setAttribute('aria-checked', isActive ? 'true' : 'false');
    c.setAttribute('tabindex', isActive ? '0' : '-1');
  });
}

async function setTheme(id){
  applyTheme(id);
  try{ await apiPost('/api/settings/theme', {theme: id}); }catch(e){}
}

// ═══════ Settings tabs ═══════
function showSettingsTab(name){
  $$('.settings-tab').forEach(b => {
    const isActive = b.dataset.settingsTab === name;
    b.classList.toggle('active', isActive);
    b.setAttribute('aria-selected', isActive ? 'true' : 'false');
    b.setAttribute('tabindex', isActive ? '0' : '-1');
  });
  $$('.settings-panel').forEach(p => {
    p.classList.toggle('active', p.id === 'settings-'+name);
  });
}

function onEnter(el, handler){
  if(!el) return;
  el.addEventListener('keydown', (e)=>{
    if(e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.metaKey){
      e.preventDefault();
      handler();
    }
  });
}

function showTab(group, name){
  // group: 'main' or 'sidebar' or 'ingest'
  if(group === 'main'){
    $$('.main-tab').forEach(b => {
      const isActive = b.dataset.mainTab === name;
      b.classList.toggle('active', isActive);
      b.setAttribute('aria-selected', isActive ? 'true' : 'false');
      b.setAttribute('tabindex', isActive ? '0' : '-1');
    });
    $$('.main-panel').forEach(p => p.classList.toggle('active', p.id === 'panel-'+name));
  }else if(group === 'sidebar'){
    $$('.sidebar-tab').forEach(b => {
      const isActive = b.dataset.sidebarTab === name;
      b.classList.toggle('active', isActive);
      b.setAttribute('aria-selected', isActive ? 'true' : 'false');
      b.setAttribute('tabindex', isActive ? '0' : '-1');
    });
    $('#sidebar-chats').style.display = (name === 'chats') ? '' : 'none';
    $('#sidebar-sources').style.display = (name === 'sources') ? '' : 'none';
  }else if(group === 'ingest'){
    $$('.ingest-tabs .sidebar-tab').forEach(b => {
      const isActive = b.dataset.ingestTab === name;
      b.classList.toggle('active', isActive);
      b.setAttribute('aria-selected', isActive ? 'true' : 'false');
      b.setAttribute('tabindex', isActive ? '0' : '-1');
    });
    $('#ingest-wiki').style.display = (name === 'wiki') ? '' : 'none';
    $('#ingest-url').style.display = (name === 'url') ? '' : 'none';
    $('#ingest-text').style.display = (name === 'text') ? '' : 'none';
    $('#ingest-upload').style.display = (name === 'upload') ? '' : 'none';
    $('#ingest-folder').style.display = (name === 'folder') ? '' : 'none';
  }
}

// Handle keyboard navigation for tab lists (ARIA best practices)
function handleTabKeydown(e, selector, group){
  if(!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) return;
  
  e.preventDefault();
  const tabs = Array.from(document.querySelectorAll(selector));
  const currentIndex = tabs.indexOf(e.target);
  let newIndex = currentIndex;
  
  if(e.key === 'ArrowLeft'){
    newIndex = currentIndex > 0 ? currentIndex - 1 : tabs.length - 1;
  }else if(e.key === 'ArrowRight'){
    newIndex = currentIndex < tabs.length - 1 ? currentIndex + 1 : 0;
  }else if(e.key === 'Home'){
    newIndex = 0;
  }else if(e.key === 'End'){
    newIndex = tabs.length - 1;
  }
  
  const newTab = tabs[newIndex];
  if(newTab){
    newTab.focus();
    // Activate the tab
    const dataAttr = group === 'main' ? 'mainTab' : group === 'sidebar' ? 'sidebarTab' : 'ingestTab';
    const tabName = newTab.dataset[dataAttr];
    if(tabName) showTab(group, tabName);
  }
}

let currentChatId = '';
let currentPersonaId = '';
let currentRole = 'it';
let currentRolePermissions = null;
let debugMode = false;
let deepMode = false;
let offlineMode = false;
let autoSearchMode = true;
let typingBubble = null;
let lastDebugData = null;

// ═══════ Debug panel rendering ═══════
function renderDebugPanel(data){
  const panel = document.createElement('div');
  panel.className = 'debug-panel';

  // Header with toggle
  const header = document.createElement('div');
  header.className = 'debug-header';
  header.innerHTML = `<span class="debug-icon">🔍</span> <span>Debug · <code>${escHtml(data.request_id||'')}</code></span>`;
  const toggle = document.createElement('button');
  toggle.className = 'debug-toggle-btn';
  toggle.textContent = '▼';
  header.appendChild(toggle);
  panel.appendChild(header);

  const body = document.createElement('div');
  body.className = 'debug-body';

  // ── Overview section ──
  const modes = [];
  if(data.deep) modes.push('Deep Research');
  if(data.offline) modes.push('Offline');
  if(data.auto_search) modes.push('Auto-Search');
  const modeStr = modes.length ? modes.join(', ') : 'Standard';

  body.innerHTML += `<div class="debug-section">
    <div class="debug-section-title">⚙️ Anfrage</div>
    <div class="debug-grid">
      <div class="debug-kv"><span class="debug-k">Modus</span><span class="debug-v">${escHtml(modeStr)}</span></div>
      <div class="debug-kv"><span class="debug-k">Frage</span><span class="debug-v">${escHtml(data.question||'')}</span></div>
      <div class="debug-kv"><span class="debug-k">Request-ID</span><span class="debug-v"><code>${escHtml(data.request_id||'')}</code></span></div>
    </div>
  </div>`;

  // ── Model section ──
  const m = data.models || {};
  body.innerHTML += `<div class="debug-section">
    <div class="debug-section-title">🤖 Modelle</div>
    <div class="debug-grid">
      <div class="debug-kv"><span class="debug-k">Endpoint</span><span class="debug-v"><code>${escHtml(m.base_url||'?')}</code></span></div>
      <div class="debug-kv"><span class="debug-k">Chat-Modell</span><span class="debug-v">${escHtml(m.chat_model||'?')}</span></div>
      <div class="debug-kv"><span class="debug-k">Embedding-Modell</span><span class="debug-v">${escHtml(m.embed_model||'?')}</span></div>
    </div>
  </div>`;

  // ── Persona section ──
  if(data.persona_name){
    body.innerHTML += `<div class="debug-section">
      <div class="debug-section-title">🎭 Persona</div>
      <div class="debug-grid">
        <div class="debug-kv"><span class="debug-k">Name</span><span class="debug-v">${escHtml(data.persona_name)}</span></div>
        <div class="debug-kv"><span class="debug-k">Pre-Prompt</span><span class="debug-v">${data.persona_prompt_chars||0} Zeichen</span></div>
      </div>
    </div>`;
  }

  // ── RAG / Retrieval section ──
  const ret = data.retrieval || {};
  body.innerHTML += `<div class="debug-section">
    <div class="debug-section-title">📊 RAG-Retrieval</div>
    <div class="debug-grid">
      <div class="debug-kv"><span class="debug-k">Top-K</span><span class="debug-v">${data.used_k||'?'} (Basis: ${data.base_k||'?'})</span></div>
      <div class="debug-kv"><span class="debug-k">Chunk-Größe</span><span class="debug-v">${data.chunk_size||'?'} Zeichen</span></div>
      <div class="debug-kv"><span class="debug-k">Chunks gesamt</span><span class="debug-v">${data.total_chunks||0}</span></div>
      <div class="debug-kv"><span class="debug-k">Kontext</span><span class="debug-v">${data.context_chars||0} Zeichen</span></div>
      <div class="debug-kv"><span class="debug-k">System-Prompt</span><span class="debug-v">${data.system_prompt_chars||0} Zeichen</span></div>
      <div class="debug-kv"><span class="debug-k">History</span><span class="debug-v">${data.history_messages||0} Nachrichten</span></div>
      <div class="debug-kv"><span class="debug-k">Embedding</span><span class="debug-v">${ret.embed_ms!=null ? ret.embed_ms+'ms' : '?'}</span></div>
      <div class="debug-kv"><span class="debug-k">Vektor-Suche</span><span class="debug-v">${ret.search_ms!=null ? ret.search_ms+'ms' : '?'}</span></div>
      <div class="debug-kv"><span class="debug-k">Storage</span><span class="debug-v">${escHtml(data.storage_mode||'?')} · <code>${escHtml(data.db_path||'')}</code></span></div>
    </div>
  </div>`;

  // ── Chunks section ──
  const chunks = (ret.chunks || []);
  if(chunks.length){
    let chunksHtml = `<div class="debug-section"><div class="debug-section-title">📄 Verwendete Chunks (${chunks.length})</div><div class="debug-chunks">`;
    chunks.forEach((c, i) => {
      const scoreLabel = c.is_neighbor ? '<span class="debug-badge neighbor">Nachbar</span>' : `<span class="debug-badge score">Score: ${Number(c.score).toFixed(4)}</span>`;
      const preview = (c.content||'').slice(0, 200) + ((c.content||'').length > 200 ? '…' : '');
      chunksHtml += `<details class="debug-chunk">
        <summary>
          <span class="debug-chunk-meta">#${i+1} · ${escHtml(c.article||'?')} [${c.chunk_idx}] ${scoreLabel}</span>
        </summary>
        <div class="debug-chunk-content">${escHtml(preview)}</div>
      </details>`;
    });
    chunksHtml += '</div></div>';
    body.innerHTML += chunksHtml;
  }

  panel.appendChild(body);

  // Toggle collapse
  let collapsed = false;
  toggle.addEventListener('click', (e)=>{
    e.stopPropagation();
    collapsed = !collapsed;
    body.style.display = collapsed ? 'none' : '';
    toggle.textContent = collapsed ? '▶' : '▼';
  });
  header.addEventListener('click', ()=> toggle.click());

  return panel;
}

function msgElement(role, content, timeIso, model, modelMeta, thinking){
  const msg = document.createElement('div');
  msg.className = `msg ${role}`;
  const bubble = document.createElement('div');
  bubble.className = 'bubble';

  renderBubbleContent(bubble, content);

  // If model info is present, add it to the bubble as a title or badge
  if(role === 'assistant' && model){
    let title = 'Model: ' + model;
    if(modelMeta){
      if(modelMeta.base_url) title += '\nEndpoint: ' + modelMeta.base_url;
      // Add other meta if present
      for(const [k,v] of Object.entries(modelMeta)){
        if(k !== 'base_url' && k !== 'chat_model') title += `\n${k}: ${v}`;
      }
    }
    bubble.title = title;
  }

  const meta = document.createElement('div');
  meta.className = 'meta';
  let metaText = `${role === 'user' ? 'Du' : 'Assistant'} · ${timeShort(timeIso)}`;
  if(role === 'assistant' && model){
    metaText += ` · ${model}`;
  }
  meta.textContent = metaText;
  msg.appendChild(bubble);
  if(role === 'assistant' && thinking && thinking.trim()){
    msg.appendChild(createThinkingPanel(thinking));
  }
  msg.appendChild(meta);

  if(role === 'assistant'){
    const actions = document.createElement('div');
    actions.className = 'msg-actions';
    const copyBtn = document.createElement('button');
    copyBtn.className = 'icon-btn copy-btn';
    copyBtn.innerHTML = '📋';
    copyBtn.title = t('copy') || 'Kopieren';
    copyBtn.addEventListener('click', () => {
      const raw = bubble.dataset.raw || content;
      copyToClipboard(raw, copyBtn);
    });
    actions.appendChild(copyBtn);

    // TTS speak button
    const speakBtn = document.createElement('button');
    speakBtn.className = 'icon-btn speak-btn';
    speakBtn.innerHTML = '🔊';
    speakBtn.title = t('tts_speak') || 'Vorlesen';
    speakBtn.addEventListener('click', () => {
      const raw = bubble.dataset.raw || content;
      if(speakBtn.classList.contains('speaking')){
        stopTTS();
        speakBtn.classList.remove('speaking');
        speakBtn.innerHTML = '🔊';
      } else {
        // Stop any other active TTS first
        $$('.speak-btn.speaking').forEach(b => {
          b.classList.remove('speaking');
          b.innerHTML = '🔊';
        });
        speakBtn.classList.add('speaking');
        speakBtn.innerHTML = '⏹';
        speakText(raw, ()=>{ speakBtn.classList.remove('speaking'); speakBtn.innerHTML = '🔊'; });
      }
    });
    actions.appendChild(speakBtn);

    msg.appendChild(actions);
  }

  return msg;
}

function addMessage(role, content, timeIso, model, modelMeta, thinking){
  const wrap = $('#chatMessages');
  $('#chatEmpty').style.display = 'none';
  wrap.appendChild(msgElement(role, content, timeIso || new Date().toISOString(), model, modelMeta, thinking));
  wrap.scrollTop = wrap.scrollHeight;
}

function replaceAssistantLast(text){
  const msgs = $$('#chatMessages .msg.assistant .bubble');
  if(msgs.length === 0) return;
  const bubble = msgs[msgs.length-1];
  renderBubbleContent(bubble, stripInternalThinking(text));
  bubble.classList.remove('typing');
  const wrap = $('#chatMessages');
  wrap.scrollTop = wrap.scrollHeight;
}

/**
 * Replace assistant bubble content with pre-rendered HTML (for inline tool cards).
 * Saves existing tool cards, renders markdown on the non-tool text, then
 * re-appends the cards so their status badges are preserved across re-renders.
 */
function replaceAssistantLastHTML(htmlContent){
  const msgs = $$('#chatMessages .msg.assistant .bubble');
  if(msgs.length === 0) return;
  const bubble = msgs[msgs.length-1];
  // Save existing tool status cards before overwriting innerHTML
  const existingCards = Array.from(bubble.querySelectorAll('.xml-tool-card'))
    .map(c => c.outerHTML);
  // Render markdown on the text with XML blocks stripped
  const clean = stripInternalThinking(stripXMLToolBlocks(htmlContent));
  bubble.dataset.raw = htmlContent;
  bubble.innerHTML = renderMarkdown(clean);
  bubble.classList.toggle('plain', !window.marked);
  // Re-append saved tool cards
  if(existingCards.length){
    const cardContainer = document.createElement('div');
    cardContainer.className = 'xml-tool-cards-restored';
    cardContainer.innerHTML = existingCards.join('');
    bubble.appendChild(cardContainer);
  }
  // Re-apply syntax highlighting
  if(window.hljs) bubble.querySelectorAll('pre code').forEach(b => hljs.highlightElement(b));
  const wrap = $('#chatMessages');
  wrap.scrollTop = wrap.scrollHeight;
}

/**
 * Update the status badge of an XML tool card by tool ID.
 * Status values: 'running' | 'done' | 'error'
 */
function updateXMLToolCard(id, toolName, query, status){
  const icon = status === 'done' ? '✓' : status === 'error' ? '✗' : '⟳';
  // Look for card by data-tool attribute in all bubbles
  const bubbles = $$('#chatMessages .msg.assistant .bubble');
  if(!bubbles.length) return;
  const bubble = bubbles[bubbles.length-1];
  const cards = bubble.querySelectorAll('.xml-tool-card');
  cards.forEach(card => {
    if(card.dataset.tool === toolName){
      const badge = card.querySelector('.tool-status-badge');
      if(badge) badge.textContent = icon;
      card.dataset.status = status;
      card.classList.toggle('tool-done', status === 'done');
      card.classList.toggle('tool-error', status === 'error');
    }
  });
}

/**
 * Ensure a tool status card exists in the current bubble.
 * If no card for this toolName exists, creates one and appends it.
 */
function ensureToolCard(id, toolName, query){
  const bubbles = $$('#chatMessages .msg.assistant .bubble');
  if(!bubbles.length) return;
  const bubble = bubbles[bubbles.length-1];
  // Check if a card already exists for this tool name
  const existing = Array.from(bubble.querySelectorAll('.xml-tool-card'))
    .find(c => c.dataset.tool === toolName);
  if(existing) return;
  // Insert a new card
  const icon = toolIconFor(toolName);
  const card = document.createElement('div');
  card.className = 'xml-tool-card';
  card.dataset.tool = toolName;
  card.dataset.status = 'running';
  card.dataset.id = id || '';
  card.innerHTML = `<span class="tool-icon">${icon}</span>` +
    `<strong>${escHtml(toolName)}</strong>` +
    (query ? ` <span class="tool-query">${escHtml(String(query).slice(0, MAX_TOOL_SOURCE_PREVIEW))}</span>` : '') +
    `<span class="tool-status-badge">⟳</span>`;
  bubble.appendChild(card);
  const wrap = $('#chatMessages');
  wrap.scrollTop = wrap.scrollHeight;
}

function setAssistantLastThinking(thinking){
  const msgs = $$('#chatMessages .msg.assistant');
  if(msgs.length === 0) return;
  const msg = msgs[msgs.length-1];
  const existing = msg.querySelector('.thinking-panel');
  if(existing) existing.remove();
  if(thinking && thinking.trim()){
    const meta = msg.querySelector('.meta');
    const panel = createThinkingPanel(thinking);
    if(meta) msg.insertBefore(panel, meta);
    else msg.appendChild(panel);
  }
}

async function refreshStats(){
  try{
    const stats = await apiGet('/api/stats');
    $('#chunkCount').textContent = stats.chunks ?? '-';
    // Also refresh sources list when open
    await refreshSources(stats.sources || []);
    const searchMeta = $('#searchMeta');
    if(searchMeta){
      searchMeta.textContent = `Top-K 8 · ${stats.chunks ?? 0} Chunks`;
    }
    updateWorkspaceStrip();
  }catch(e){}
}

async function clearChunks(){
  const status = $('#clearStatus');
  if(!confirm(t('clear_confirm'))) return;
  setLoading('#clearChunksBtn', true);
  setStatus(status, t('loading'));
  try{
    await fetch('/api/chunks/clear', {method:'POST'});
    setStatus(status, t('cleared'), 'ok');
    setTimeout(()=>setStatus(status,''), 3000);
    await refreshStats();
  }catch(e){
    setStatus(status, t('error_prefix')+(e.message||String(e)), 'err');
  }finally{
    setLoading('#clearChunksBtn', false);
  }
}

async function refreshChats(){
  const list = await apiGet('/api/chats');
  const box = $('#sidebar-chats');
  if(!list.length){
    box.innerHTML = `<p class="muted">Noch keine Chats.</p>`;
    return;
  }
  box.innerHTML = '';
  list.forEach(c => {
    const div = document.createElement('div');
    div.className = 'item' + (c.id === currentChatId ? ' active':'');
    div.innerHTML = `
      <div>
        <div class="title">${escHtml(c.title || 'Neuer Chat')}</div>
        <div class="meta">${timeShort(c.updated || c.created)}</div>
      </div>
      <div class="right">
        <button class="icon-btn danger" title="Chat löschen">🗑</button>
      </div>`;
    div.addEventListener('click', async (ev) => {
      // Click on delete?
      if(ev.target && ev.target.classList.contains('danger')){
        ev.stopPropagation();
        if(!confirm('Diesen Chat löschen?')) return;
        await fetch('/api/chat/'+encodeURIComponent(c.id), {method:'DELETE'});
        if(currentChatId === c.id){
          currentChatId = '';
          $('#chatMessages').innerHTML = buildEmptyStateHtml();
          updateWorkspaceStrip();
        }
        await refreshChats();
        return;
      }
      await loadChat(c.id);
    });
    box.appendChild(div);
  });
}

async function loadChat(id){
  const c = await apiGet('/api/chat/'+encodeURIComponent(id));
  currentChatId = c.id;
  if(c.persona_id){
    currentPersonaId = c.persona_id;
    const sel = $('#personaSelect'); if(sel) sel.value = currentPersonaId;
  }
  
  $('#chatMessages').innerHTML = buildEmptyStateHtml();
  if(!c.messages || !c.messages.length){
    $('#chatEmpty').style.display = '';
  }else{
    $('#chatEmpty').style.display = 'none';
    c.messages.forEach(m => addMessage(m.role, m.content, m.time, m.model, m.model_meta, m.thinking));
  }
  await refreshChats();
  showTab('sidebar','chats');
  showTab('main','chat');
  closeSidebarIfMobile();
  updateWorkspaceStrip();
}

async function newChat(){
  const c = await apiPost('/api/chats/new', {persona_id: currentPersonaId});
  currentChatId = c.id;
  $('#chatMessages').innerHTML = buildEmptyStateHtml();
  await refreshChats();
  showTab('main','chat');
  closeSidebarIfMobile();
  updateWorkspaceStrip();
}

async function refreshSources(src){
  const box = $('#sidebar-sources');
  if(!src || !src.length){
    box.innerHTML = `<p class="muted">Noch keine Quellen.</p>`;
    return;
  }
  box.innerHTML = '';
  src.forEach(s => {
    const div = document.createElement('div');
    div.className = 'item';
    div.innerHTML = `
      <div>
        <div class="title">${escHtml(s.article)}</div>
        <div class="meta">${s.chunks} Chunks</div>
      </div>
      <div class="right">
        <button class="icon-btn danger" title="Quelle löschen">🗑</button>
      </div>`;
    div.addEventListener('click', async (ev) => {
      if(ev.target && ev.target.classList.contains('danger')){
        ev.stopPropagation();
        if(!confirm('Diese Quelle komplett löschen?\n\n'+s.article)) return;
        await apiPost('/api/sources/delete', {article: s.article});
        await refreshStats();
      }
    });
    box.appendChild(div);
  });
}

async function initSettingsUI(){
  // Load persisted settings
  const s = await apiGet('/api/settings');
  currentRole = s.active_role || currentRole || 'it';
  applyRolePermissionsUI(s.role_permissions || null);
  syncIngestRoleScopesWithActiveRole(true);
  $('#wikiLang').value = s.lang || 'de';
  const langSel = $('#langSelect');
  if(langSel){
    const pref = (s.lang || 'de').split('-')[0];
    if(Array.from(langSel.options).some(o => o.value === pref)){
      langSel.value = pref;
    }
  }
  if($('#setUsageProfile')) $('#setUsageProfile').value = s.usage_profile || 'personal';
  if($('#setResponseLanguageMode')) $('#setResponseLanguageMode').value = s.response_language_mode || 'auto';
  if($('#setRedactPII')) $('#setRedactPII').checked = !!s.redact_pii;
  const roleSel = $('#roleSelect');
  if(roleSel){
    roleSel.value = currentRole;
  }
  $('#setBaseUrl').value = s.base_url || 'http://localhost:1234';
  // Support separate chat/embed bases
  $('#setChatBase').value = s.chat_base || s.base_url || 'http://localhost:1234';
  $('#setEmbedBase').value = s.embed_base || s.base_url || 'http://localhost:1234';
  // Show/hide advanced endpoints toggle
  const useSep = $('#useSeparateEndpoints');
  const adv = $('#advancedEndpoints');
  if(useSep && adv){
    // If chat_base/embed_base differ from base_url, enable advanced view
    const isAdvanced = (s.chat_base && s.chat_base !== s.base_url) || (s.embed_base && s.embed_base !== s.base_url);
    useSep.checked = !!isAdvanced;
    adv.style.display = isAdvanced ? '' : 'none';
    useSep.addEventListener('change', ()=>{ adv.style.display = useSep.checked ? '' : 'none'; });
  }
  // nanoGo toggle
  const nanoChk = $('#allowNanoGo');
  if(nanoChk) nanoChk.checked = !!s.allow_nanogo;

  // Apply theme from settings
  if(s.theme) applyTheme(s.theme);

  // Fill selects with current values (will be replaced after "Test")
  const chatSel = $('#setChatModel');
  const embSel = $('#setEmbedModel');
  chatSel.innerHTML = `<option>${escHtml(s.chat_model||'')}</option>`;
  embSel.innerHTML = `<option>${escHtml(s.embed_model||'')}</option>`;
  $('#chatHint').textContent = '';
  $('#embedHint').textContent = '';
  $('#endpointStatus').textContent = '';

  // OpenAI key presence (do not expose actual key)
  const keyInp = $('#setOpenAIKey');
  if(keyInp){
    if(s.openai_key_present){
      keyInp.placeholder = 'Configured (hidden)';
      keyInp.value = '';
    } else {
      keyInp.placeholder = 'sk-...';
      keyInp.value = '';
    }
  }

  // Wire quick "Use OpenAI" button: fill base URL and enable simple setup
  const btnUse = $('#btnUseOpenAI');
  const btnClearKey = $('#btnClearOpenAI');
  const hint = $('#openaiHint');
  if(btnUse){
    btnUse.addEventListener('click', ()=>{
      // Set base URL to OpenAI host
      $('#setBaseUrl').value = 'https://api.openai.com';
      // Disable advanced endpoints (simple mode)
      if($('#useSeparateEndpoints')){ $('#useSeparateEndpoints').checked = false; const adv = $('#advancedEndpoints'); if(adv) adv.style.display='none'; }
      if(keyInp && keyInp.value.trim() === '' && s.openai_key_present){
        // If key already present, inform user
        if(hint) hint.textContent = 'OpenAI-Key ist gespeichert. Nach Speichern wird OpenAI verwendet.';
      } else if(keyInp && keyInp.value.trim() !== ''){
        if(hint) hint.textContent = 'OpenAI-Key eingefügt. Klick auf Speichern, um OpenAI zu aktivieren.';
      } else {
        if(hint) hint.textContent = 'Trage deinen OpenAI-Key ein und klicke auf Speichern.';
      }
    });
  }
  if(btnClearKey){
    btnClearKey.onclick = async ()=>{
      try{
        await apiPost('/api/settings', {
          base_url: $('#setBaseUrl').value.trim(),
          chat_base: ($('#useSeparateEndpoints') && $('#useSeparateEndpoints').checked) ? $('#setChatBase').value.trim() : '',
          embed_base: ($('#useSeparateEndpoints') && $('#useSeparateEndpoints').checked) ? $('#setEmbedBase').value.trim() : '',
          chat_model: $('#setChatModel').value,
          embed_model: $('#setEmbedModel').value,
          active_role: currentRole || 'it',
          usage_profile: $('#setUsageProfile') ? $('#setUsageProfile').value : 'personal',
          response_language_mode: $('#setResponseLanguageMode') ? $('#setResponseLanguageMode').value : 'auto',
          redact_pii: $('#setRedactPII') ? !!$('#setRedactPII').checked : false,
          allow_nanogo: $('#allowNanoGo') ? !!$('#allowNanoGo').checked : false,
          openai_api_key_clear: true
        });
        if(keyInp){
          keyInp.value = '';
          keyInp.placeholder = 'sk-...';
        }
        if(hint) hint.textContent = 'OpenAI-Key gelöscht.';
      }catch(e){
        if(hint) hint.textContent = 'Fehler beim Löschen des Keys: ' + (e.message||String(e));
      }
    };
  }

  await loadCustomApis();
  await loadModules();
  await loadPersonas();
  await loadAdmin();
  await loadBranding(s);
  await loadAuthSettings();

  // Update OpenAI badge visibility in main UI
  updateOpenAIBadge(s);
  // Initialize LLM provider switcher in the header
  initLLMSwitcher(s);

  // Initialize ingest embed-model input with current setting
  const ingestSelect = $('#ingestEmbedSelect');
  const ingestCustom = $('#ingestEmbedCustom');
  if(ingestSelect){
    // Ensure default option present
    ingestSelect.innerHTML = '<option value="">(Default / use settings)</option><option value="custom">-- Custom --</option>';
    if(s.embed_model){
      // if a configured embed model exists, add and select it
      const opt = document.createElement('option'); opt.value = s.embed_model; opt.textContent = s.embed_model;
      ingestSelect.appendChild(opt);
      ingestSelect.value = s.embed_model;
    }
    ingestSelect.addEventListener('change', ()=>{ if(ingestCustom) ingestCustom.style.display = ingestSelect.value==='custom' ? '' : 'none'; });
  }
  if(ingestCustom){ ingestCustom.style.display = 'none'; ingestCustom.value = ''; }

  // ingest model helper is global (defined above)

  // Wire clear-all-chunks button
  const btnClearAll = $('#btnClearAllChunks');
  if(btnClearAll){
    btnClearAll.addEventListener('click', async ()=>{
      if(!confirm('Alle Chunks löschen? Dieser Vorgang kann nicht rückgängig gemacht werden.')) return;
      setStatus($('#saveStatus'), 'Lösche alle Chunks…', '');
      try{
        const r = await apiPost('/api/chunks/clear', {confirm: true});
        setStatus($('#saveStatus'), 'Alle Chunks gelöscht. Total: '+(r.total||0), 'ok');
        await refreshStats();
      }catch(e){
        setStatus($('#saveStatus'), 'Fehler beim Löschen: '+(e.message||String(e)), 'err');
      }
    });
  }
}

function updateOpenAIBadge(s){
  const badge = $('#openaiBadge');
  if(!badge) return;
  const chatBase = (s && (s.chat_base || s.base_url)) ? (s.chat_base || s.base_url) : '';
  const embedBase = (s && (s.embed_base || s.base_url)) ? (s.embed_base || s.base_url) : '';
  const chatName = resolveProviderName(chatBase);
  const embedName = resolveProviderName(embedBase);
  const anyBase = (chatBase || embedBase || '').toLowerCase();
  const usingOpenAI = !!(s && s.openai_key_present && anyBase && anyBase.includes('openai.com'));

  if(usingOpenAI){
    badge.style.display = '';
    badge.innerHTML = '<span>Using</span><strong>OpenAI</strong>';
    badge.title = 'OpenAI';
    updateWorkspaceStrip();
    return;
  }

  // If we have local/remote providers, show a badge with primary name and hover with both
  if(chatName || embedName){
    const primary = chatName || embedName || 'LLM';
    badge.style.display = '';
    badge.innerHTML = '<span>Using</span><strong>' + escHtml(primary) + '</strong>';
    const parts = [];
    if(chatName) parts.push('Chat: ' + chatName);
    if(embedName) parts.push('Embed: ' + embedName);
    badge.title = parts.join('\n');
    updateWorkspaceStrip();
    return;
  }

  badge.style.display = 'none';
  updateWorkspaceStrip();
}

function isLocalLLMBase(base){
  if(!base) return false;
  const low = String(base).toLowerCase();
  return low.includes('localhost') || low.includes('127.0.0.1') || low.includes('[::1]');
}

function buildQuickChatModelOptions(resp, currentChatModel){
  const out = [];
  const seen = new Set();
  const add = (value, label) => {
    if(!value || seen.has(value)) return;
    seen.add(value);
    out.push({value, label});
  };

  (resp.recommend_chat || []).forEach(m => add(m, m + ' (recommended)'));
  (resp.models || []).forEach(m => {
    const low = String(m).toLowerCase();
    const looksEmbed = low.includes('embed') || low.includes('embedding');
    if(looksEmbed && m !== currentChatModel) return;
    add(m, m);
  });
  return out;
}

async function applyQuickLocalModel(baseUrl, chatModel, currentSettings){
  const payload = {
    base_url: currentSettings.base_url || baseUrl,
    chat_base: currentSettings.chat_base || baseUrl,
    embed_base: currentSettings.embed_base || currentSettings.base_url || baseUrl,
    chat_model: chatModel,
    embed_model: currentSettings.embed_model,
    active_role: currentRole || currentSettings.active_role || 'it'
  };
  await apiPost('/api/settings', payload);
}

async function refreshQuickModelSwitcher(s){
  const modelSel = $('#llmModelSelect');
  const applyBtn = $('#llmApplyBtn');
  if(!modelSel || !applyBtn) return;

  const baseUrl = (s && (s.chat_base || s.base_url)) ? (s.chat_base || s.base_url) : '';
  const currentChatModel = s?.chat_model || '';
  if(!isLocalLLMBase(baseUrl)){
    modelSel.style.display = 'none';
    applyBtn.style.display = 'none';
    modelSel.innerHTML = '<option value="">(Use recommended)</option>';
    return;
  }

  try{
    const resp = await apiPost('/api/llm/list-models', {base_url: baseUrl});
    const opts = buildQuickChatModelOptions(resp, currentChatModel);
    if(!opts.length){
      modelSel.style.display = 'none';
      applyBtn.style.display = 'none';
      return;
    }

    modelSel.innerHTML = '';
    opts.forEach(o => {
      const op = document.createElement('option');
      op.value = o.value;
      op.textContent = o.label;
      modelSel.appendChild(op);
    });

    const hasCurrent = opts.some(o => o.value === currentChatModel);
    modelSel.value = hasCurrent ? currentChatModel : opts[0].value;
    modelSel.style.display = '';
    applyBtn.style.display = '';
    applyBtn.textContent = 'Switch';
    applyBtn.disabled = (modelSel.value === currentChatModel);
    modelSel.title = 'Lokales Chat-Modell';
    applyBtn.title = 'Ausgewähltes lokales Modell aktivieren';

    modelSel.onchange = ()=>{
      applyBtn.disabled = (modelSel.value === currentChatModel);
    };

    applyBtn.onclick = async ()=>{
      if(!modelSel.value || modelSel.value === currentChatModel) return;
      setLoading(applyBtn, true);
      try{
        await applyQuickLocalModel(baseUrl, modelSel.value, s);
        const s2 = await apiGet('/api/settings');
        updateOpenAIBadge(s2);
        await refreshQuickModelSwitcher(s2);
        await loadPersonas();
        await refreshStats();
      }catch(err){
        alert('Fehler beim Wechseln des lokalen Modells: '+(err.message||String(err)));
      }finally{
        setLoading(applyBtn, false);
      }
    };
  }catch(err){
    modelSel.style.display = 'none';
    applyBtn.style.display = 'none';
    console.error('Failed to refresh quick model switcher', err);
  }
}

// Initialize the LLM provider switcher UI and wire selection actions.
async function initLLMSwitcher(s){
  const sel = $('#llmSwitcher');
  if(!sel) return;
  // Determine active base (chat preferred) and map to a switcher value using
  // resolveProviderName so we share the same hostname-safe logic.
  const base = (s && (s.chat_base || s.embed_base || s.base_url)) ? (s.chat_base || s.embed_base || s.base_url) : '';
  const provName = resolveProviderName(base);
  const provToVal = {
    'OpenAI': 'openai', 'LM Studio': 'lmstudio', 'Ollama': 'ollama',
    'Groq': 'groq', 'Anthropic': 'anthropic', 'Mistral AI': 'mistral',
    'DeepSeek': 'deepseek', 'OpenRouter': 'openrouter',
  };
  sel.value = provToVal[provName] || 'custom';

  await refreshQuickModelSwitcher(s);

  if(sel.dataset.bound === 'true') return;
  sel.dataset.bound = 'true';

  sel.addEventListener('change', async (e)=>{
    const v = e.target.value;
    if(v === 'custom'){
      // Open settings for custom entry
      openModal();
      return;
    }
    // Map selection to base URL
    const map = {
      openai: 'https://api.openai.com',
      lmstudio: 'http://localhost:1234',
      ollama: 'http://localhost:11434',
      groq: 'https://api.groq.com/openai',
      anthropic: 'https://api.anthropic.com',
      mistral: 'https://api.mistral.ai',
      deepseek: 'https://api.deepseek.com',
      openrouter: 'https://openrouter.ai/api',
    };
    const baseUrl = map[v];
    if(!baseUrl) return;
    // Probe and populate a compact model selector so the user can pick a chat model before applying
    const modelSel = $('#llmModelSelect');
    const applyBtn = $('#llmApplyBtn');
    setLoading(sel, true);
    try{
      const resp = await apiPost('/api/llm/list-models', {base_url: baseUrl});
      // Build option list: prefer recommend_chat first, then unique models
      const opts = [];
      const seen = {};
      if(resp.recommend_chat && resp.recommend_chat.length){
        resp.recommend_chat.forEach(m=>{ if(!seen[m]){ opts.push({label: m+' (recommended)', value: m}); seen[m]=true } });
      }
      if(resp.models && resp.models.length){
        resp.models.forEach(m=>{ if(!seen[m]){ opts.push({label: m, value: m}); seen[m]=true } });
      }
      if(opts.length === 0){
        alert('Keine Modelle gefunden für '+baseUrl+'. Öffne Einstellungen, um manuell zu konfigurieren.');
        openModal();
        return;
      }
      // Populate select
      modelSel.innerHTML = '';
      const defOpt = document.createElement('option'); defOpt.value = ''; defOpt.textContent = '(Use recommended)'; modelSel.appendChild(defOpt);
      opts.forEach(o=>{ const op = document.createElement('option'); op.value = o.value; op.textContent = o.label; modelSel.appendChild(op); });
      // Choose first recommended if present
      if(resp.recommend_chat && resp.recommend_chat.length){ modelSel.value = resp.recommend_chat[0]; }
      else { modelSel.value = opts[0].value; }
      modelSel.style.display = '';
      applyBtn.style.display = '';

      // Wire apply button: uses selected model (or recommended if empty)
      applyBtn.onclick = async ()=>{
        setLoading(applyBtn, true);
        try{
          const selected = modelSel.value || ((resp.recommend_chat && resp.recommend_chat.length)?resp.recommend_chat[0]:opts[0].value);
          const embedModel = (resp.recommend_embed && resp.recommend_embed.length) ? resp.recommend_embed[0] : selected;
          await apiPost('/api/settings', {chat_base: baseUrl, embed_base: baseUrl, chat_model: selected, embed_model: embedModel});
          const s2 = await apiGet('/api/settings');
          updateOpenAIBadge(s2);
          await refreshQuickModelSwitcher(s2);
          await loadPersonas();
          await refreshStats();
          alert('LLM gewechselt und Modell angewendet: '+selected);
          // Auto-hide selector after success
          modelSel.style.display = 'none';
          applyBtn.style.display = 'none';
        }catch(err){
          alert('Fehler beim Anwenden des Modells: '+(err.message||String(err)));
        }finally{
          setLoading(applyBtn, false);
        }
      };

    }catch(err){
      alert('Fehler beim Wechseln des LLM: '+(err.message||String(err)));
    }finally{
      setLoading(sel, false);
    }
  });

  // Hide model selector if user clicks elsewhere or re-opens settings
  document.addEventListener('click', (e)=>{
    const modelSel = $('#llmModelSelect');
    const applyBtn = $('#llmApplyBtn');
    if(!modelSel || !applyBtn) return;
    // keep visible as long as user interacts with switcher or selector
    // No-op: selector visibility is managed by change handler and apply button
  });
}

let modalFocusTrap = null;

function openModal(){
  const modal = $('#settingsModal');
  modal.classList.add('open');
  modal.setAttribute('aria-hidden','false');
  
  // Store the element that triggered the modal
  modalFocusTrap = document.activeElement;
  
  // Focus the first focusable element in the modal
  setTimeout(() => {
    const focusableElements = modal.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])');
    if(focusableElements.length > 0){
      focusableElements[0].focus();
    }
  }, 100);
  
  // Trap focus in modal
  modal.addEventListener('keydown', trapFocusInModal);
}

function closeModal(){
  const modal = $('#settingsModal');
  modal.classList.remove('open');
  modal.setAttribute('aria-hidden','true');
  
  // Remove focus trap
  modal.removeEventListener('keydown', trapFocusInModal);
  
  // Return focus to the element that opened the modal
  if(modalFocusTrap){
    modalFocusTrap.focus();
    modalFocusTrap = null;
  }
}

function showLLMMissingModal(msg){
  // Avoid duplicate
  if($('#llmMissingModal')) return;
  const modal = document.createElement('div');
  modal.id = 'llmMissingModal';
  modal.className = 'overlay-modal';
  modal.innerHTML = `
    <div class="overlay-box">
      <h2>LLM API nicht erreichbar</h2>
      <p>${escHtml(msg || 'Keine LLM-API konfiguriert oder erreichbar. Bitte Einstellungen öffnen und einen Endpoint/API-Key hinterlegen.')}</p>
      <div class="actions">
        <button id="llmOpenSettings" class="tool-btn">Einstellungen öffnen</button>
        <button id="llmDismiss" class="tool-btn">Schließen</button>
      </div>
    </div>`;
  document.body.appendChild(modal);
  $('#llmOpenSettings').addEventListener('click', async ()=>{
    closeLLMMissingModal();
    openModal();
    await initSettingsUI();
  });
  $('#llmDismiss').addEventListener('click', ()=>closeLLMMissingModal());
}
function closeLLMMissingModal(){
  const m = $('#llmMissingModal'); if(m) m.remove();
}

function trapFocusInModal(e){
  if(e.key !== 'Tab') return;
  
  const modal = $('#settingsModal');
  const focusableElements = Array.from(modal.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'));
  const firstElement = focusableElements[0];
  const lastElement = focusableElements[focusableElements.length - 1];
  
  if(e.shiftKey){
    // Shift + Tab
    if(document.activeElement === firstElement){
      e.preventDefault();
      lastElement.focus();
    }
  }else{
    // Tab
    if(document.activeElement === lastElement){
      e.preventDefault();
      firstElement.focus();
    }
  }
}

async function discoverEndpoints(){
  const box = $('#discoverBox');
  box.style.display = '';
  box.innerHTML = '<p class="muted">Suche lokale Endpoints…</p>';
  try{
    const d = await apiGet('/api/discover');
    if(!d.candidates || !d.candidates.length){
      box.innerHTML = '<p class="muted">Keine Kandidaten gefunden.</p>';
      return;
    }
    box.innerHTML = '';
    d.candidates.forEach(c => {
      const div = document.createElement('div');
      div.className = 'discover-candidate';
      const badge = c.ok ? `<span class="badge ok">OK</span>` : `<span class="badge err">Fehler</span>`;
      const rec = c.ok ? ( (c.recommend_chat?.[0]||'') + (c.recommend_embed?.[0] ? ' · '+c.recommend_embed[0] : '') ) : '';
      div.innerHTML = `
        <div class="left">
          <div class="title">${escHtml(c.provider_hint)} · ${escHtml(c.base_url)}</div>
          <div class="models">${c.ok ? escHtml('Modelle: '+(c.models?.length||0)+' · Vorschlag: '+rec) : escHtml(c.error||'')}</div>
        </div>
        <div class="right">
          ${badge}
          <button class="tool-btn">Übernehmen</button>
        </div>
      `;
      div.querySelector('button').addEventListener('click', ()=>{
        $('#setBaseUrl').value = c.base_url;
        setStatus($('#endpointStatus'), 'Endpoint übernommen. Jetzt „Test & Modelle laden“ klicken.', 'ok');
      });
      box.appendChild(div);
    });
  }catch(e){
    box.innerHTML = `<p class="tool-status err">${escHtml(e.message||String(e))}</p>`;
  }
}

async function testEndpointAndLoadModels(){
  const base = $('#setBaseUrl').value.trim();
  if(!base){
    setStatus($('#endpointStatus'), 'Bitte Endpoint eingeben.', 'err');
    return;
  }
  setStatus($('#endpointStatus'), 'Teste Endpoint…', '');
  $('#discoverBox').style.display = 'none';

  try{
    const maybeKey = $('#setOpenAIKey') ? $('#setOpenAIKey').value.trim() : '';
    const r = await apiPost('/api/llm/list-models', {base_url: base, openai_api_key: maybeKey});
    setStatus($('#endpointStatus'), `OK (${r.provider_hint}). ${r.models.length} Modelle gefunden.`, 'ok');

    // Fill selects
    const chatSel = $('#setChatModel');
    const embSel = $('#setEmbedModel');
    const curChat = chatSel.value;
    const curEmb = embSel.value;

    chatSel.innerHTML = '';
    embSel.innerHTML = '';

    r.models.forEach(m=>{
      const opt1 = document.createElement('option');
      opt1.value = m; opt1.textContent = m;
      chatSel.appendChild(opt1);

      const opt2 = document.createElement('option');
      opt2.value = m; opt2.textContent = m;
      embSel.appendChild(opt2);
    });

    // Keep selection if possible; else pick recommended; else first
    const pick = (sel, cur, rec) => {
      if(cur && Array.from(sel.options).some(o=>o.value===cur)){
        sel.value = cur; return;
      }
      if(rec && rec.length && Array.from(sel.options).some(o=>o.value===rec[0])){
        sel.value = rec[0]; return;
      }
      sel.selectedIndex = 0;
    };
    pick(chatSel, curChat, r.recommend_chat);
    pick(embSel, curEmb, r.recommend_embed);

    $('#chatHint').textContent = r.recommend_chat?.length ? ('Vorschläge: '+r.recommend_chat.join(', ')) : '';
    $('#embedHint').textContent = r.recommend_embed?.length ? ('Vorschläge: '+r.recommend_embed.join(', ')) : 'Tipp: wähle ein Embedding-Modell (oft mit „embed“ im Namen).';

    // Populate ingest embed select as well
    const ingestSel = $('#ingestEmbedSelect');
    const ingestCustom = $('#ingestEmbedCustom');
    if(ingestSel){
      // keep the default and custom options; add model options
      const prev = ingestSel.value;
      const baseHtml = '<option value="">(Default / use settings)</option><option value="custom">-- Custom --</option>';
      ingestSel.innerHTML = baseHtml;
      r.models.forEach(m=>{ const o = document.createElement('option'); o.value = m; o.textContent = m; ingestSel.appendChild(o); });
      // try to select recommend_embed or previous
      if(prev && Array.from(ingestSel.options).some(o=>o.value===prev)) ingestSel.value = prev;
      else if(r.recommend_embed && r.recommend_embed.length && Array.from(ingestSel.options).some(o=>o.value===r.recommend_embed[0])) ingestSel.value = r.recommend_embed[0];
    }
    if(ingestCustom) ingestCustom.style.display = (ingestSel && ingestSel.value==='custom') ? '' : 'none';
    if(ingestSel){ ingestSel.addEventListener('change', ()=>{ if(ingestCustom) ingestCustom.style.display = ingestSel.value==='custom' ? '' : 'none'; }); }

  }catch(e){
    setStatus($('#endpointStatus'), 'Fehler: '+(e.message||String(e)), 'err');
  }
}

async function saveSettings(force=false){
  const base = $('#setBaseUrl').value.trim();
  const useSep = $('#useSeparateEndpoints') ? !!$('#useSeparateEndpoints').checked : false;
  const chatBase = useSep ? $('#setChatBase').value.trim() : '';
  const embedBase = useSep ? $('#setEmbedBase').value.trim() : '';
  const chat = $('#setChatModel').value;
  const emb = $('#setEmbedModel').value;
  const openaiKey = $('#setOpenAIKey') ? $('#setOpenAIKey').value.trim() : '';
  const allowNano = $('#allowNanoGo') ? !!$('#allowNanoGo').checked : false;
  const usageProfile = $('#setUsageProfile') ? $('#setUsageProfile').value : 'personal';
  const responseLanguageMode = $('#setResponseLanguageMode') ? $('#setResponseLanguageMode').value : 'auto';
  const redactPII = $('#setRedactPII') ? !!$('#setRedactPII').checked : false;
  if(!chat || !emb){
    setStatus($('#saveStatus'), 'Bitte Chat- und Embedding-Modell wählen.', 'err');
    return;
  }
  setStatus($('#saveStatus'), 'Speichere…', '');
  try{
    // Only send chat_base/embed_base when advanced mode is used; otherwise server will use base_url
    const payload = {
      base_url: base,
      chat_model: chat,
      embed_model: emb,
      openai_api_key: openaiKey,
      active_role: currentRole || 'it',
      force,
      allow_nanogo: allowNano,
      usage_profile: usageProfile,
      response_language_mode: responseLanguageMode,
      redact_pii: redactPII
    };
    if(useSep){ payload.chat_base = chatBase; payload.embed_base = embedBase; }
    await apiPost('/api/settings', payload);
    setStatus($('#saveStatus'), 'Gespeichert. Einstellungen aktiv.', 'ok');
    // If an OpenAI key was provided and base points to OpenAI, make it explicit to the user
    if(openaiKey){
      const baseHost = (base || '').toLowerCase();
      if(baseHost.includes('openai.com')){
        setStatus($('#saveStatus'), 'OpenAI konfiguriert — Chat & Embeddings werden OpenAI verwenden.', 'ok');
      }
    }
    closeModal();
  }catch(e){
    if(e.status === 409 && e.payload && e.payload.requires_force){
      // Prompt the user explicitly instead of requiring a hidden second click
      const msg = (e.payload && e.payload.message) ? e.payload.message : 'Änderung erfordert Bestätigung.';
      const proceed = confirm(msg + '\n\nFortfahren und die Wissensbasis ggf. neu einbetten/DB leeren?');
      if(proceed){
        try{
          await saveSettings(true);
        }catch(err){
          // nested call will surface errors into UI
        }
      }else{
        setStatus($('#saveStatus'), 'Abgebrochen.', '');
      }
      return;
    }
    setStatus($('#saveStatus'), 'Fehler: '+(e.message||String(e)), 'err');
  }finally{
    // reset handler
    setTimeout(()=>{ $('#btnSaveSettings').onclick = ()=>saveSettings(false); }, 0);
  }
}

async function loadCustomApis(){
  const list = await apiGet('/api/settings/apis');
  const box = $('#apiList');
  cachedCustomAPIs = list || [];
  if(!list.length){
    box.innerHTML = '<p class="muted">Noch keine Custom APIs.</p>';
    return;
  }
  box.innerHTML = '';
  list.forEach(a=>{
    const div = document.createElement('div');
    div.className = 'api-item';
    div.innerHTML = `
      <div>
        <div class="name">${escHtml(a.name)}</div>
        <div class="desc">${escHtml(a.desc||'')}</div>
        <div class="tmpl">${escHtml(a.template)}</div>
      </div>
      <div class="actions">
        <button class="tool-btn danger">Löschen</button>
      </div>
    `;
    div.querySelector('button').addEventListener('click', async ()=>{
      if(!confirm('Custom API löschen?\n\n'+a.name)) return;
      await apiPost('/api/settings/apis/delete', {id: a.id});
      await loadCustomApis();
    });
    box.appendChild(div);
  });
}

async function loadModules(){
  const list = await apiGet('/api/modules');
  cachedModules = list || [];
  const box = $('#moduleList');
  if(!box) return;
  if(!cachedModules.length){
    box.innerHTML = '<p class="muted">Noch keine Module.</p>';
    return;
  }
  box.innerHTML = '';
  cachedModules.forEach(m => {
    const div = document.createElement('div');
    div.className = 'api-item module-item';
    const actionLabel = m.kind === 'sql' ? 'Query override (optional)' : m.kind === 'mail' ? 'Limit (optional)' : 'Pfad / Unterordner';
    const actionPlaceholder = m.kind === 'sql' ? 'SELECT * FROM ...' : m.kind === 'mail' ? '5' : '.';
    const jsonCfg = JSON.stringify(m.config || {}, null, 2);
    div.innerHTML = `
      <div style="width:100%">
        <div class="name" style="display:flex;justify-content:space-between;gap:12px;align-items:center">
          <span>${escHtml(m.name)}</span>
          <label class="inline-check" style="margin:0">
            <input type="checkbox" class="module-enabled" ${m.enabled ? 'checked' : ''}>
            <span>Enabled</span>
          </label>
        </div>
        <div class="desc">${escHtml(m.kind)} · ${escHtml(m.description || '')}</div>
        <label style="margin-top:10px">Config JSON</label>
        <textarea class="module-config" rows="10">${escHtml(jsonCfg)}</textarea>
        <label style="margin-top:10px">${escHtml(actionLabel)}</label>
        <input class="module-arg" placeholder="${escHtml(actionPlaceholder)}" value="${m.kind === 'http-folder' ? escHtml((m.config||{}).default_list_path || '.') : ''}">
        <div class="actions-row" style="margin-top:10px">
          <button class="tool-btn module-save">Speichern</button>
          <button class="tool-btn module-test">Testen</button>
          <button class="tool-btn suggested module-ingest">Ingest</button>
          ${m.kind === 'http-folder' ? '<button class="tool-btn module-upload">Upload</button><input type="file" class="module-file" style="display:none">' : ''}
        </div>
        <div class="tool-status module-status" style="margin-top:8px"></div>
        <pre class="module-output" style="white-space:pre-wrap;margin-top:8px"></pre>
      </div>
    `;

    const cfgEl = div.querySelector('.module-config');
    const argEl = div.querySelector('.module-arg');
    const enabledEl = div.querySelector('.module-enabled');
    const statusEl = div.querySelector('.module-status');
    const outEl = div.querySelector('.module-output');

    div.querySelector('.module-save').addEventListener('click', async ()=>{
      try{
        const config = JSON.parse(cfgEl.value || '{}');
        await apiPost('/api/modules/save', {
          id: m.id,
          name: m.name,
          kind: m.kind,
          description: m.description,
          enabled: enabledEl.checked,
          config
        });
        setStatus(statusEl, 'Gespeichert', 'ok');
        await loadModules();
      }catch(e){
        setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err');
      }
    });

    div.querySelector('.module-test').addEventListener('click', async ()=>{
      try{
        setStatus(statusEl, 'Teste…', '');
        const payload = {id: m.id, action: m.kind === 'http-folder' ? 'list' : 'query', arg: argEl.value.trim()};
        if(m.kind === 'mail') payload.limit = Number(argEl.value.trim() || 0);
        const res = await apiPost('/api/modules/run', payload);
        setStatus(statusEl, res.summary || 'OK', 'ok');
        outEl.textContent = res.text || JSON.stringify(res.meta || {}, null, 2);
      }catch(e){
        setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err');
        outEl.textContent = '';
      }
    });

    div.querySelector('.module-ingest').addEventListener('click', async ()=>{
      try{
        setStatus(statusEl, 'Ingest läuft…', '');
        const payload = {id: m.id, action: m.kind === 'http-folder' ? 'ingest' : 'query', arg: argEl.value.trim(), ingest: true};
        if(m.kind === 'mail') payload.limit = Number(argEl.value.trim() || 0);
        const res = await apiPost('/api/modules/run', payload);
        setStatus(statusEl, res.summary || 'Ingest OK', 'ok');
        outEl.textContent = res.text || JSON.stringify(res.meta || {}, null, 2);
        await refreshStats();
      }catch(e){
        setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err');
      }
    });

    if(m.kind === 'http-folder'){
      const fileInput = div.querySelector('.module-file');
      div.querySelector('.module-upload').addEventListener('click', ()=>fileInput.click());
      fileInput.addEventListener('change', async ()=>{
        const file = fileInput.files?.[0];
        if(!file) return;
        const form = new FormData();
        form.append('file', file);
        setStatus(statusEl, 'Upload…', '');
        try{
          const resp = await fetch('/api/modules/upload?id='+encodeURIComponent(m.id)+'&target='+encodeURIComponent(argEl.value.trim() || '.'), {method:'POST', body: form});
          const payload = await resp.json();
          if(!resp.ok) throw new Error(payload.error || JSON.stringify(payload));
          setStatus(statusEl, 'Upload OK', 'ok');
          outEl.textContent = JSON.stringify(payload, null, 2) + '\n\nDownload URL:\n/api/modules/download?id=' + encodeURIComponent(m.id) + '&path=' + encodeURIComponent(file.name);
          await refreshStats();
        }catch(e){
          setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err');
        }finally{
          fileInput.value = '';
        }
      });
    }

    box.appendChild(div);
  });
}

async function loadPersonas(){
  const list = await apiGet('/api/personas');
  cachedPersonas = list || [];

  // Update selector
  const sel = $('#personaSelect');
  if(sel){
    sel.innerHTML = '';
    cachedPersonas.forEach(p=>{
      const opt = document.createElement('option');
      opt.value = p.id;
      opt.textContent = p.name;
      sel.appendChild(opt);
    });
    if(!currentPersonaId && cachedPersonas.length){
      currentPersonaId = cachedPersonas[0].id;
    }
    const has = cachedPersonas.some(p=>p.id === currentPersonaId);
    if(!has && cachedPersonas.length){
      currentPersonaId = cachedPersonas[0].id;
    }
    if(currentPersonaId) sel.value = currentPersonaId;
  }
  updateWorkspaceStrip();

  // Render list in settings
  const box = $('#personaList');
  if(box){
    if(!cachedPersonas.length){
      box.innerHTML = '<p class="muted">Noch keine Personas.</p>';
    } else {
      box.innerHTML = '';
      cachedPersonas.forEach(p=>{
        const div = document.createElement('div');
        div.className = 'api-item';
        const snippet = (p.prompt||'').split('\n').slice(0,2).join(' ');
        div.innerHTML = `
          <div>
            <div class="name">${escHtml(p.name)}</div>
            <div class="desc">${escHtml(snippet)}</div>
          </div>
          <div class="actions">
            <button class="tool-btn danger">Löschen</button>
          </div>
        `;
        div.querySelector('button').addEventListener('click', async ()=>{
          if(!confirm('Persona löschen?\n\n'+p.name)) return;
          await apiPost('/api/personas/delete', {id: p.id});
          if(currentPersonaId === p.id) currentPersonaId = '';
          await loadPersonas();
        });
        box.appendChild(div);
      });
    }
  }
}

function showIssuedAPIKey(name, key){
  if(!key) return;
  window.prompt('API-Key fuer "' + name + '". Jetzt kopieren; spaeter ist er nicht mehr sichtbar.', key);
}

async function loadAdmin(){
  await Promise.all([loadAdminUsers(), loadAdminRoutes()]);
}

async function loadAdminUsers(){
  const box = $('#adminUserList');
  if(!box) return;
  const list = await apiGet('/api/admin/users');
  if(!list.length){
    box.innerHTML = '<p class="muted">Noch keine API-User.</p>';
    return;
  }
  box.innerHTML = '';
  list.forEach(u => {
    const div = document.createElement('div');
    div.className = 'api-item';
    div.innerHTML = `
      <div style="width:100%">
        <div class="name" style="display:flex;justify-content:space-between;gap:12px;align-items:center">
          <input class="admin-user-name" value="${escHtml(u.name || '')}" aria-label="API user name">
          <label class="inline-check" style="margin:0">
            <input type="checkbox" class="admin-user-enabled" ${u.enabled ? 'checked' : ''}>
            <span>Enabled</span>
          </label>
        </div>
        <div class="desc">ID: ${escHtml(u.id)} · Rolle: ${escHtml(u.role || 'it')} · Key endet auf: ${escHtml(u.api_key_last4 || '----')}</div>
        <div class="desc">Erstellt: ${escHtml(u.created_at || '')}</div>
        <div class="actions-row" style="margin-top:10px">
          <select class="admin-user-role" aria-label="API user role">
            <option value="it" ${u.role === 'it' ? 'selected' : ''}>IT</option>
            <option value="logistik" ${u.role === 'logistik' ? 'selected' : ''}>Logistik</option>
            <option value="vertrieb" ${u.role === 'vertrieb' ? 'selected' : ''}>Vertrieb</option>
            <option value="hr" ${u.role === 'hr' ? 'selected' : ''}>HR</option>
          </select>
          <button class="tool-btn admin-user-save">Speichern</button>
          <button class="tool-btn suggested admin-user-regen">Key neu erzeugen</button>
          <button class="tool-btn danger admin-user-delete">Löschen</button>
        </div>
        <div class="tool-status admin-user-status" style="margin-top:8px"></div>
      </div>
    `;
    const nameEl = div.querySelector('.admin-user-name');
    const roleEl = div.querySelector('.admin-user-role');
    const enabledEl = div.querySelector('.admin-user-enabled');
    const statusEl = div.querySelector('.admin-user-status');
    div.querySelector('.admin-user-save').addEventListener('click', async ()=>{
      try{
        await apiPost('/api/admin/users/save', {
          id: u.id,
          name: nameEl.value.trim(),
          role: roleEl.value,
          enabled: !!enabledEl.checked
        });
        setStatus(statusEl, 'Gespeichert', 'ok');
        await loadAdminUsers();
      }catch(e){
        setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err');
      }
    });
    div.querySelector('.admin-user-regen').addEventListener('click', async ()=>{
      if(!confirm('API-Key für '+(nameEl.value.trim() || u.name)+' neu erzeugen? Der alte Key wird sofort ungültig.')) return;
      try{
        const res = await apiPost('/api/admin/users/regenerate', {id: u.id});
        setStatus(statusEl, 'API-Key neu erzeugt', 'ok');
        showIssuedAPIKey(res.user?.name || nameEl.value.trim() || u.name, res.api_key || '');
        await loadAdminUsers();
      }catch(e){
        setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err');
      }
    });
    div.querySelector('.admin-user-delete').addEventListener('click', async ()=>{
      if(!confirm('API-User löschen?\n\n'+(nameEl.value.trim() || u.name))) return;
      try{
        await apiPost('/api/admin/users/delete', {id: u.id});
        await loadAdminUsers();
      }catch(e){
        setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err');
      }
    });
    box.appendChild(div);
  });
}

async function loadAdminRoutes(){
  const box = $('#adminRouteList');
  if(!box) return;
  const list = await apiGet('/api/admin/routes');
  if(!list.length){
    box.innerHTML = '<p class="muted">Keine Route-Policies vorhanden.</p>';
    return;
  }
  box.innerHTML = '';
  list.forEach(route => {
    const div = document.createElement('div');
    div.className = 'api-item';
    div.innerHTML = `
      <div style="width:100%">
        <div class="name">${escHtml(route.path)}</div>
        <div class="desc">${escHtml(route.description || '')}</div>
        <div class="actions-row" style="margin-top:10px">
          <label class="inline-check" style="margin:0">
            <input type="checkbox" class="admin-route-enabled" ${route.enabled ? 'checked' : ''}>
            <span>Enabled</span>
          </label>
          <label class="inline-check" style="margin:0">
            <input type="checkbox" class="admin-route-public" ${route.public ? 'checked' : ''}>
            <span>Public</span>
          </label>
          <button class="tool-btn admin-route-save">Speichern</button>
        </div>
        <div class="tool-status admin-route-status" style="margin-top:8px"></div>
      </div>
    `;
    const enabledEl = div.querySelector('.admin-route-enabled');
    const publicEl = div.querySelector('.admin-route-public');
    const statusEl = div.querySelector('.admin-route-status');
    div.querySelector('.admin-route-save').addEventListener('click', async ()=>{
      try{
        await apiPost('/api/admin/routes/save', {
          path: route.path,
          enabled: !!enabledEl.checked,
          public: !!publicEl.checked
        });
        setStatus(statusEl, 'Gespeichert', 'ok');
        await loadAdminRoutes();
      }catch(e){
        setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err');
      }
    });
    box.appendChild(div);
  });
}

async function addAdminUser(){
  const name = $('#newAdminUserName') ? $('#newAdminUserName').value.trim() : '';
  const role = $('#newAdminUserRole') ? $('#newAdminUserRole').value : 'it';
  if(!name){
    alert('Bitte Namen angeben');
    return;
  }
  setLoading('#btnAddAdminUser', true);
  try{
    const res = await apiPost('/api/admin/users/create', {name, role});
    if($('#newAdminUserName')) $('#newAdminUserName').value = '';
    if($('#newAdminUserRole')) $('#newAdminUserRole').value = 'it';
    showIssuedAPIKey(res.user?.name || name, res.api_key || '');
    await loadAdminUsers();
  }catch(e){
    alert('Fehler beim Anlegen: '+(e.message||String(e)));
  }finally{
    setLoading('#btnAddAdminUser', false);
  }
}

async function addPersona(){
  const name = $('#newPersonaName').value.trim();
  const prompt = $('#newPersonaPrompt').value.trim();
  if(!name){
    alert('Bitte Namen angeben');
    return;
  }
  setLoading('#btnAddPersona', true);
  try{
    await apiPost('/api/personas', {name, prompt});
    $('#newPersonaName').value = '';
    $('#newPersonaPrompt').value = '';
    await loadPersonas();
  }catch(e){
    alert('Fehler beim Hinzufügen: '+(e.message||String(e)));
  }finally{
    setLoading('#btnAddPersona', false);
  }
}

// Insert a curated set of sample personas to help new users.
async function addSamplePersonas(){
  const samples = [
    {name: 'Formal', prompt: 'You are a formal assistant. Use a polite, professional tone and concise sentences.'},
    {name: 'Friendly', prompt: 'You are friendly and approachable. Use an informal tone and give helpful examples where useful.'},
    {name: 'Expert', prompt: 'You are an expert in the relevant field. Provide detailed, technical answers and cite assumptions when needed.'},
    {name: 'Concise', prompt: 'Answer briefly and directly. Prioritize short, precise responses with bulleted lists for clarity.'},
    {name: 'Developer', prompt: 'You are a coding assistant. Provide code snippets and explanations to help solve programming problems. Use best practices and comment your code.'},
  ];
  if(!confirm('Beispiel-Personas hinzufügen?')) return;
  setLoading('#btnAddSamplePersonas', true);
  try{
    for(const p of samples){
      await apiPost('/api/personas', {name: p.name, prompt: p.prompt});
    }
    await loadPersonas();
    alert('Beispiel-Personas hinzugefügt.');
  }catch(e){
    alert('Fehler beim Hinzufügen der Beispiel-Personas: '+(e.message||String(e)));
  }finally{
    setLoading('#btnAddSamplePersonas', false);
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// Branding / white-label support
// ─────────────────────────────────────────────────────────────────────────────

// applyBranding applies branding from settings to the live UI.
function applyBranding(s){
  const appName = (s && s.app_name) ? s.app_name : 'tinyRAG';
  // Page title
  document.title = appName;
  const titleEl = document.getElementById('pageTitle');
  if(titleEl) titleEl.textContent = appName;
  // Sidebar heading
  const heading = document.getElementById('appNameHeading');
  if(heading) heading.textContent = appName;
  // Logo
  const logo = document.getElementById('appLogoImg');
  if(logo){
    if(s && s.app_logo_url){
      logo.src = s.app_logo_url;
      logo.style.display = '';
    } else {
      logo.style.display = 'none';
    }
  }
  // Custom CSS injection
  const cssEl = document.getElementById('customCSS');
  if(cssEl && s && s.custom_css) cssEl.textContent = s.custom_css;
}

// loadBranding fills the branding settings form and applies live branding.
async function loadBranding(s){
  // s is the already-fetched settings object (optional, will reload if null)
  if(!s){
    try{ s = await apiGet('/api/settings'); }catch(e){ return; }
  }
  applyBranding(s);
  if($('#brandAppName')) $('#brandAppName').value = s.app_name || '';
  if($('#brandLogoURL')) $('#brandLogoURL').value = s.app_logo_url || '';
  if($('#brandCustomCSS')) $('#brandCustomCSS').value = s.custom_css || '';
}

// saveBranding POSTs branding fields to /api/admin/branding.
async function saveBranding(){
  const payload = {
    app_name:    ($('#brandAppName')    || {}).value || '',
    app_logo_url:($('#brandLogoURL')   || {}).value || '',
    custom_css:  ($('#brandCustomCSS') || {}).value || '',
  };
  const st = $('#brandingStatus');
  try{
    await apiPost('/api/admin/branding', payload);
    applyBranding(payload);
    setStatus(st, 'Gespeichert', 'ok');
  }catch(e){
    setStatus(st, 'Fehler: '+(e.message||String(e)), 'err');
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// Web-UI Authentication management
// ─────────────────────────────────────────────────────────────────────────────

// initSessionUI checks the /api/auth/me endpoint and shows/hides the user bar.
async function initSessionUI(){
  try{
    const me = await apiGet('/api/auth/me');
    const bar = $('#userBar');
    if(bar && me.web_ui_auth){
      bar.style.display = 'flex';
      const nameEl = $('#userBarName');
      if(nameEl) nameEl.textContent = me.username + (me.role === 'admin' ? ' (admin)' : '');
    }
  }catch(e){
    // 401 means auth is enabled but no session — /login redirect handled by server
  }
}

// loadAuthSettings fetches auth config and fills the Auth tab form.
async function loadAuthSettings(){
  try{
    const cfg = await apiGet('/api/admin/auth/get');
    if($('#authEnabled'))     $('#authEnabled').checked          = !!cfg.web_ui_auth;
    if($('#authSessionTTL'))  $('#authSessionTTL').value         = cfg.session_ttl_seconds || 86400;
    if($('#ldapEnabled'))     $('#ldapEnabled').checked          = !!cfg.ldap_enabled;
    if($('#ldapServer'))      $('#ldapServer').value             = cfg.ldap_server || '';
    if($('#ldapPort'))        $('#ldapPort').value               = cfg.ldap_port || 389;
    if($('#ldapUseTLS'))      $('#ldapUseTLS').checked           = !!cfg.ldap_use_tls;
    if($('#ldapStartTLS'))    $('#ldapStartTLS').checked         = !!cfg.ldap_start_tls;
    if($('#ldapBaseDN'))      $('#ldapBaseDN').value             = cfg.ldap_base_dn || '';
    if($('#ldapBindDN'))      $('#ldapBindDN').value             = cfg.ldap_bind_dn || '';
    if($('#ldapUserAttr'))    $('#ldapUserAttr').value           = cfg.ldap_user_attr || 'uid';
    if($('#ldapFilter'))      $('#ldapFilter').value             = cfg.ldap_filter || '';
    if($('#ldapAdminGroup'))  $('#ldapAdminGroup').value         = cfg.ldap_admin_group || '';
    if($('#ldapBindPass') && cfg.ldap_bind_pass_set){
      $('#ldapBindPass').placeholder = '(configured, leave blank to keep)';
    }
    await loadWebUsers();
  }catch(e){
    // Might be 401 if not admin — silently ignore
  }
}

// saveAuthSettings saves web UI auth + session TTL.
async function saveAuthSettings(){
  const st = $('#authStatus');
  const payload = {
    web_ui_auth:         !!($('#authEnabled')    || {}).checked,
    session_ttl_seconds: parseInt(($('#authSessionTTL') || {}).value || '86400', 10),
  };
  try{
    await apiPost('/api/admin/auth', payload);
    setStatus(st, 'Gespeichert', 'ok');
  }catch(e){
    setStatus(st, 'Fehler: '+(e.message||String(e)), 'err');
  }
}

// saveLDAPSettings saves LDAP configuration.
async function saveLDAPSettings(){
  const st = $('#ldapStatus');
  const payload = {
    ldap_enabled:    !!($('#ldapEnabled')    || {}).checked,
    ldap_server:     ($('#ldapServer')       || {}).value || '',
    ldap_port:       parseInt(($('#ldapPort') || {}).value || '389', 10),
    ldap_use_tls:    !!($('#ldapUseTLS')     || {}).checked,
    ldap_start_tls:  !!($('#ldapStartTLS')   || {}).checked,
    ldap_base_dn:    ($('#ldapBaseDN')        || {}).value || '',
    ldap_bind_dn:    ($('#ldapBindDN')        || {}).value || '',
    ldap_bind_pass:  ($('#ldapBindPass')      || {}).value || '',
    ldap_user_attr:  ($('#ldapUserAttr')      || {}).value || 'uid',
    ldap_filter:     ($('#ldapFilter')        || {}).value || '',
    ldap_admin_group:($('#ldapAdminGroup')    || {}).value || '',
  };
  try{
    await apiPost('/api/admin/auth', payload);
    setStatus(st, 'LDAP-Konfiguration gespeichert', 'ok');
  }catch(e){
    setStatus(st, 'Fehler: '+(e.message||String(e)), 'err');
  }
}

// loadWebUsers fetches and renders local web UI users.
async function loadWebUsers(){
  const box = $('#webUserList');
  if(!box) return;
  let list;
  try{
    list = await apiGet('/api/admin/webusers');
  }catch(e){
    box.innerHTML = '<p class="muted">Nicht verfügbar (Admin-Session erforderlich)</p>';
    return;
  }
  if(!list.length){
    box.innerHTML = '<p class="muted">Keine lokalen Web-UI-User vorhanden.</p>';
    return;
  }
  box.innerHTML = '';
  list.forEach(u => {
    const div = document.createElement('div');
    div.className = 'api-item';
    div.innerHTML = `
      <div style="width:100%">
        <div class="name" style="display:flex;justify-content:space-between;align-items:center;gap:12px">
          <span>${escHtml(u.username)}</span>
          <label class="inline-check" style="margin:0">
            <input type="checkbox" class="wu-enabled" ${u.enabled ? 'checked' : ''}>
            <span>Enabled</span>
          </label>
        </div>
        <div class="desc">ID: ${escHtml(u.id)} · Rolle: ${escHtml(u.role)} · Erstellt: ${escHtml(u.created_at || '')}</div>
        <div class="actions-row" style="margin-top:8px">
          <select class="wu-role" aria-label="User role">
            <option value="viewer" ${u.role === 'viewer' ? 'selected' : ''}>Viewer</option>
            <option value="admin"  ${u.role === 'admin'  ? 'selected' : ''}>Admin</option>
          </select>
          <input type="password" class="wu-pass" placeholder="New password (min 8 chars)" style="width:180px">
          <button class="tool-btn wu-save">Speichern</button>
          <button class="tool-btn wu-chpass">Passwort setzen</button>
          <button class="tool-btn danger wu-delete">Löschen</button>
        </div>
        <div class="tool-status wu-status" style="margin-top:6px"></div>
      </div>`;
    const roleEl    = div.querySelector('.wu-role');
    const enabledEl = div.querySelector('.wu-enabled');
    const passEl    = div.querySelector('.wu-pass');
    const statusEl  = div.querySelector('.wu-status');
    div.querySelector('.wu-save').addEventListener('click', async ()=>{
      try{
        await apiPost('/api/admin/webusers/save', {id: u.id, role: roleEl.value, enabled: !!enabledEl.checked});
        setStatus(statusEl, 'Gespeichert', 'ok');
        await loadWebUsers();
      }catch(e){ setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err'); }
    });
    div.querySelector('.wu-chpass').addEventListener('click', async ()=>{
      const pw = passEl.value;
      if(pw.length < 8){ setStatus(statusEl, 'Passwort muss mind. 8 Zeichen haben', 'err'); return; }
      try{
        await apiPost('/api/admin/webusers/password', {id: u.id, password: pw});
        passEl.value = '';
        setStatus(statusEl, 'Passwort geändert', 'ok');
      }catch(e){ setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err'); }
    });
    div.querySelector('.wu-delete').addEventListener('click', async ()=>{
      if(!confirm('User "'+u.username+'" löschen?')) return;
      try{
        await apiPost('/api/admin/webusers/delete', {id: u.id});
        await loadWebUsers();
      }catch(e){ setStatus(statusEl, 'Fehler: '+(e.message||String(e)), 'err'); }
    });
    box.appendChild(div);
  });
}

// addWebUser creates a new local web UI user.
async function addWebUser(){
  const name = ($('#newWebUserName') || {}).value?.trim() || '';
  const pass = ($('#newWebUserPass') || {}).value || '';
  const role = ($('#newWebUserRole') || {}).value || 'viewer';
  const st   = $('#webUserStatus');
  if(!name || !pass){ setStatus(st, 'Username und Passwort erforderlich', 'err'); return; }
  if(pass.length < 8){ setStatus(st, 'Passwort muss mind. 8 Zeichen haben', 'err'); return; }
  try{
    await apiPost('/api/admin/webusers/create', {username: name, password: pass, role});
    if($('#newWebUserName')) $('#newWebUserName').value = '';
    if($('#newWebUserPass')) $('#newWebUserPass').value = '';
    setStatus(st, 'User angelegt', 'ok');
    await loadWebUsers();
  }catch(e){ setStatus(st, 'Fehler: '+(e.message||String(e)), 'err'); }
}

// logoutUser clears the session and redirects to /login.
async function logoutUser(){
  try{ await fetch('/api/auth/logout', {method:'POST'}); }catch(e){}
  window.location.href = '/login';
}

// Tool suggestion UI (adapted from original tinyRAG)
var toolIcons={wikipedia:'\u{1F4D6}',duckduckgo:'\u{1F50E}',wiktionary:'\u{1F4DD}',stackoverflow:'\u{1F4BB}',websearch:'\u{1F50D}',news:'\u{1F4F0}',calculate:'\u{1F522}',nanogo:'\u{1F680}',vector_query:'\u{1F9E0}',sql_query:'\u{1F5C4}'};
var toolLabels={wikipedia:'Wikipedia',duckduckgo:'DuckDuckGo',wiktionary:'Wiktionary',stackoverflow:'StackOverflow',websearch:'Websuche',news:'News',calculate:'Rechnen',nanogo:'Go-Code',vector_query:'Vektorsuche',sql_query:'SQL-Abfrage'};
var cachedCustomAPIs=[];
var cachedModules=[];
var cachedPersonas=[];

const TOOL_META = {
  wikipedia: {group:'research'},
  duckduckgo: {group:'research'},
  wiktionary: {group:'reference'},
  stackoverflow: {group:'code'},
  websearch: {group:'research'},
  news: {group:'research'},
  calculate: {group:'compute'},
  nanogo: {group:'compute'},
  shell: {group:'danger'},
  tinygo: {group:'danger'}
};

function toolButtonHtml(toolName, label, icon, extraClass=''){
  return '<button class="tool-btn'+extraClass+'" data-tool="'+escHtml(toolName)+'">'+icon+' '+escHtml(label)+'</button>';
}

function renderToolSuggestion(tr, chatEl, originalQuestion){
  const div = document.createElement('div');
  div.className = 'tool-suggestion';
  const suggestedTool = tr.tool || 'websearch';
  const suggestedIcon = toolIcons[suggestedTool] || '\u{1F527}';
  const suggestedLabel = toolLabels[suggestedTool] || suggestedTool;
  const suggestedGroup = TOOL_META[suggestedTool]?.group || 'research';

  const builtinTools = ['websearch','wikipedia','duckduckgo','news','stackoverflow','wiktionary','calculate','nanogo','shell','tinygo'];
  const preferredAlternatives = builtinTools.filter(t => t !== suggestedTool && TOOL_META[t]?.group === suggestedGroup && TOOL_META[t]?.group !== 'danger').slice(0, 2);
  const secondaryAlternatives = builtinTools.filter(t => t !== suggestedTool && !preferredAlternatives.includes(t) && TOOL_META[t]?.group !== 'danger');
  const advancedTools = builtinTools.filter(t => TOOL_META[t]?.group === 'danger');

  let h = '<div class="tool-header"><div><span class="tool-icon">\u{1F50D}</span> Zusätzliche Recherche vorgeschlagen</div><span class="tool-badge">Empfohlen: '+escHtml(suggestedLabel)+'</span></div>';
  h += '<div class="tool-desc">Der lokale Kontext reicht vermutlich noch nicht aus. Passe die Anfrage an oder starte direkt die empfohlene Aktion.</div>';
  h += '<label class="tool-query-label">Anfrage</label>';
  h += '<textarea class="tool-query-edit" rows="2">'+escHtml(tr.query)+'</textarea>';
  h += '<div class="tool-primary-actions">';
  h += '<button class="btn-primary tool-run-primary" data-tool="'+escHtml(suggestedTool)+'">'+suggestedIcon+' '+escHtml(suggestedLabel)+' ausführen</button>';
  preferredAlternatives.forEach(t => {
    h += toolButtonHtml(t, toolLabels[t] || t, toolIcons[t] || '\u{1F527}');
  });
  h += '<button class="btn-reject">Nicht jetzt</button>';
  h += '</div>';

  h += '<details class="tool-more">';
  h += '<summary>Weitere Tools</summary>';
  h += '<div class="tool-actions">';
  secondaryAlternatives.forEach(t => {
    h += toolButtonHtml(t, toolLabels[t] || t, toolIcons[t] || '\u{1F527}', t === suggestedTool ? ' suggested' : '');
  });
  cachedCustomAPIs.forEach(api => {
    const suggested2 = (api.id===suggestedTool)?' suggested':'';
    h += toolButtonHtml(api.id, api.name, '\u{1F310}', suggested2);
  });
  cachedModules.forEach(mod => {
    if(!mod.enabled) return;
    const toolName = 'module:'+mod.id;
    const suggested3 = (toolName===suggestedTool)?' suggested':'';
    h += toolButtonHtml(toolName, mod.name, '\u{1F9E9}', suggested3);
  });
  if(advancedTools.length){
    h += '<div class="tool-advanced-label">Erweiterte Tools</div>';
    advancedTools.forEach(t => {
      h += toolButtonHtml(t, toolLabels[t] || t, toolIcons[t] || '\u{1F527}', ' danger');
    });
  }
  h += '</div></details>';
  h += '<div class="tool-status"></div>';
  div.innerHTML = h;
  div.dataset.originalQuestion = originalQuestion;
  // attach handlers
  div.querySelectorAll('.tool-btn,.tool-run-primary').forEach(b=>{
    b.addEventListener('click', (ev)=>{ executeToolFromCard(ev.currentTarget); });
  });
  const rej = div.querySelector('.btn-reject');
  if(rej) rej.addEventListener('click', ()=>dismissToolCard(rej));
  chatEl.appendChild(div);
  chatEl.scrollTop = chatEl.scrollHeight;
}

function dismissToolCard(btn){
  const card = btn.closest('.tool-suggestion');
  if(!card) return;
  card.querySelector('.tool-status').textContent = 'Abgelehnt';
  const primary = card.querySelector('.tool-primary-actions'); if(primary) primary.style.display = 'none';
  const more = card.querySelector('.tool-more'); if(more) more.style.display = 'none';
}

function executeToolFromCard(btn){
  const card = btn.closest('.tool-suggestion');
  if(!card) return;
  const tool = btn.dataset.tool;
  const query = (card.querySelector('.tool-query-edit')||{value:''}).value.trim();
  const originalQuestion = card.dataset.originalQuestion;
  if(!query) return;
  executeToolAndReask(btn, tool, query, originalQuestion);
}

function executeToolAndReask(btn, tool, query, originalQuestion){
  const card = btn.closest('.tool-suggestion');
  if(!card) return;
  const status = card.querySelector('.tool-status');
  // disable buttons
  card.querySelectorAll('.tool-btn,.tool-run-primary,.btn-reject').forEach(b=>b.disabled=true);
  const input = card.querySelector('.tool-query-edit'); if(input) input.disabled = true;
  status.innerHTML = '<span class="spinner"></span>' + (toolIcons[tool]||'') + ' ' + escHtml(toolLabels[tool]||tool) + ': Suche läuft…';

  fetch('/api/tool/execute',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({tool:tool,query:query})}).then(async function(resp){
    if(!resp.ok){
      const t = await resp.text();
      status.innerHTML = '<span style="color:var(--red)">Fehler: '+escHtml(t)+'</span>';
      card.querySelectorAll('.tool-btn,.tool-run-primary,.btn-reject').forEach(b=>b.disabled=false);
      if(input) input.disabled=false;
      return;
    }
    const d = await resp.json();
    status.innerHTML = '<span style="color:#22c55e">✓ '+escHtml(d.source)+': '+d.chars+' Zeichen, '+d.chunks+' Chunks geladen</span>';
    const actions = card.querySelector('.tool-actions'); if(actions) actions.style.display = 'none';
    refreshStats();
    // auto re-ask after short delay
    setTimeout(function(){
      $('#chatQ').value = originalQuestion || '';
      askChat();
    }, 500);
  }).catch(function(e){
    status.innerHTML = '<span style="color:var(--red)">Fehler: '+escHtml(e.message||String(e))+'</span>';
    card.querySelectorAll('.tool-btn,.tool-run-primary,.btn-reject').forEach(b=>b.disabled=false);
    if(input) input.disabled=false;
  });
}

async function addCustomApi(){
  const name = $('#newApiName').value.trim();
  const template = $('#newApiTemplate').value.trim();
  const desc = $('#newApiDesc').value.trim();
  if(!name || !template){
    alert('Bitte Name und URL-Template ausfüllen.');
    return;
  }
  try{
    await apiPost('/api/settings/apis', {name, template, desc});
    $('#newApiName').value = '';
    $('#newApiTemplate').value = '';
    $('#newApiDesc').value = '';
    await loadCustomApis();
  }catch(e){
    alert('Fehler: '+(e.message||String(e)));
  }
}

// Tool request from model
async function executeTool(tool, query){
  // show in chat that tool is being executed
  addMessage('assistant', `🔎 Tool wird ausgeführt: ${tool}("${query}")`, new Date().toISOString());
  try{
    const r = await apiPost('/api/tool/execute', {tool, query});
    addMessage('assistant', `✅ Tool fertig. Quelle: ${r.source} · ${r.chunks} Chunks hinzugefügt.`, new Date().toISOString());
    await refreshStats();
  }catch(e){
    addMessage('assistant', `❌ Tool-Fehler: ${e.message||String(e)}`, new Date().toISOString());
  }
}

async function askChat(){
  const q = $('#chatQ').value.trim();
  if(!q && !attachedImageBase64) return;
  $('#chatQ').value = '';
  autosize($('#chatQ'));
  lastDebugData = null;

  // If an image is attached, use the vision endpoint
  if(attachedImageBase64){
    const imageB64  = attachedImageBase64;
    const imageMime = attachedImageMime;
    clearAttachedImage();
    const userLabel = q || t('image_label');
    addMessage('user', userLabel, new Date().toISOString());
    await askVision(q, imageB64, imageMime);
    return;
  }

  addMessage('user', q, new Date().toISOString());

  // placeholder assistant msg
  addMessage('assistant', '🔄 ' + t('processing'), new Date().toISOString());
  // mark last assistant bubble as typing
  try{
    const bubbles = $$('#chatMessages .msg.assistant .bubble');
    if(bubbles.length){
      typingBubble = bubbles[bubbles.length-1];
      typingBubble.classList.add('typing');
      typingBubble.setAttribute('aria-live','polite');
      typingBubble.textContent = t('assistant_typing');
    }
  }catch(e){}
  let acc = '';
  let reasoningAcc = '';
  let hasError = false;

  try{
    const payload = {
      question:q,
      chat_id: currentChatId,
      debug: debugMode,
      deep: deepMode,
      offline: offlineMode,
      auto_search: autoSearchMode,
      persona_id: currentPersonaId,
      active_role: currentRole
    };
    const resp = await fetch('/api/ask', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify(payload)
    });

    if(!resp.ok){
      const t = await resp.text();
      replaceAssistantLast('Fehler: '+t);
      return;
    }

    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = '';

    while(true){
      const {value, done} = await reader.read();
      if(done) break;
      buf += dec.decode(value, {stream:true});

      // process SSE events
      let idx;
      while((idx = buf.indexOf('\n\n')) >= 0){
        const raw = buf.slice(0, idx);
        buf = buf.slice(idx+2);

        const lines = raw.split('\n').filter(Boolean);
        let event = 'message';
        let dataLines = [];
        for(const line of lines){
          if(line.startsWith('event:')){
            event = line.slice(6).trim();
          }else if(line.startsWith('data:')){
            dataLines.push(line.slice(5).trim());
          }
        }
        const dataStr = dataLines.join('\n');
        if(event === 'meta'){
          try{
            const meta = JSON.parse(dataStr);
            if(meta.chat_id) currentChatId = meta.chat_id;
            if(meta.persona_id){
              currentPersonaId = meta.persona_id;
              const sel = $('#personaSelect');
              if(sel) sel.value = currentPersonaId;
            }
            if(meta.active_role){
              currentRole = normalizeRoleValue(meta.active_role);
              const roleSel = $('#roleSelect');
              if(roleSel) roleSel.value = currentRole;
            }
            if(meta.role_permissions){
              applyRolePermissionsUI(meta.role_permissions);
            }
            // refresh chats sidebar
            refreshChats();
            updateWorkspaceStrip();
          }catch(e){}
          continue;
        }
        if(event === 'debug'){
          // Render debug panel in the chat
          try{
            lastDebugData = JSON.parse(dataStr);
            console.debug('RAG debug:', lastDebugData);
            // Insert debug panel right before the current assistant bubble
            const msgs = document.querySelectorAll('#chatMessages .msg.assistant');
            if(msgs.length){
              const lastMsg = msgs[msgs.length-1];
              const existing = lastMsg.querySelector('.debug-panel');
              if(existing) existing.remove();
              lastMsg.appendChild(renderDebugPanel(lastDebugData));
              const wrap = $('#chatMessages');
              wrap.scrollTop = wrap.scrollHeight;
            }
          }catch(e){ console.error('debug parse error', e); }
          continue;
        }
        if(event === 'tool_request'){
          try{
            const tr = JSON.parse(dataStr);
            if(tr.tool && tr.query){
              // Render tool suggestion card as a separate UI element below the response
              renderToolSuggestion(tr, $('#chatMessages'), q);
            }
          }catch(e){}
          continue;
        }
        if(event === 'tool_start'){
          try{
            const ts = JSON.parse(dataStr);
            if(ts.id && ts.tool){
              // First try to update an existing inline XML card
              updateXMLToolCard(ts.id, ts.tool, ts.query, 'running');
              // If no card exists yet, insert one into the current bubble
              ensureToolCard(ts.id, ts.tool, ts.query);
            }
          }catch(e){}
          continue;
        }
        if(event === 'tool_result'){
          try{
            const tr = JSON.parse(dataStr);
            if(tr && tr.id){
              // Update the inline XML card status
              updateXMLToolCard(tr.id, tr.tool, tr.query, tr.error ? 'error' : 'done');
            } else if(tr && tr.source && tr.output){
              // Legacy path
              acc = '';
              replaceAssistantLast(`🔎 ${tr.tool || 'Tool'} wurde ausgeführt. Antwort wird mit den neuen Informationen überarbeitet…`);
              const bubbles = $$('#chatMessages .msg.assistant .bubble');
              if(bubbles.length){
                typingBubble = bubbles[bubbles.length-1];
                typingBubble.classList.add('typing');
              }
            }
          }catch(e){}
          continue;
        }
        if(event === 'route'){
          // Debug: routing decision
          try{ console.debug('Route:', JSON.parse(dataStr)); }catch(e){}
          continue;
        }
        if(event === 'reasoning'){
          try{
            const reasoning = JSON.parse(dataStr);
            if(typeof reasoning === 'string'){
              reasoningAcc = reasoning;
              setAssistantLastThinking(reasoningAcc);
            }
          }catch(e){}
          continue;
        }

        // default data stream
        if(dataStr === '[DONE]'){
          // Final render: use accumulated raw markdown (acc) to preserve formatting.
          // Previously used bubble.textContent which strips HTML and loses markdown.
          try{
            const msgs = document.querySelectorAll('#chatMessages .msg.assistant .bubble');
            if(msgs && msgs.length){
              const bubble = msgs[msgs.length-1];
              const raw = acc || bubble.dataset.raw || '';
              if(!raw.trim()){
                bubble.textContent = '❌ Keine Antwort vom LLM erhalten';
                hasError = true;
              }else{
                // Save existing tool status cards before re-rendering
                const existingCards = Array.from(bubble.querySelectorAll('.xml-tool-card'))
                  .map(c => c.outerHTML);
                // Strip tool XML blocks and internal thinking before final markdown render
                const cleaned = stripInternalThinking(stripToolRequestText(raw));
                renderBubbleContent(bubble, cleaned);
                // Re-append saved tool cards so status badges are preserved
                if(existingCards.length){
                  const cardContainer = document.createElement('div');
                  cardContainer.className = 'xml-tool-cards-restored';
                  cardContainer.innerHTML = existingCards.join('');
                  bubble.appendChild(cardContainer);
                }
              }
            }
          }catch(e){}
          break;
        }
        try{
          const tok = JSON.parse(dataStr);
          if(typeof tok === 'string'){
            acc += tok;
            // Render XML tool blocks as cards during streaming
            const rendered = replaceXMLToolBlocksWithCards(acc);
            replaceAssistantLastHTML(rendered);
          }
        }catch(e){
          // Check if it's an error message (starts with ⚠️ or doesn't parse as JSON)
          if(dataStr.startsWith('⚠️') || dataStr.startsWith('Fehler')){
            acc += dataStr;
            replaceAssistantLast(acc);
            hasError = true;
          }
        }
      }
    }
  }catch(e){
    replaceAssistantLast('Fehler: '+(e.message||String(e)));
  }
}

async function runSearch(){
  const q = $('#searchQ').value.trim();
  if(!q) return;
  $('#searchResults').innerHTML = `<p class="muted">Suche…</p>`;
  const searchMeta = $('#searchMeta');
  if(searchMeta) searchMeta.textContent = `Top-K 8 · ${q}`;
  try{
    const res = await apiPost('/api/search', {query:q, k: 8});
    if(!res.length){
      $('#searchResults').innerHTML = `<p class="muted">Keine Treffer.</p>`;
      return;
    }
    $('#searchResults').innerHTML = '';
    res.forEach(r=>{
      const div = document.createElement('div');
      div.className = 'result';
      div.innerHTML = `
        <div class="result-head">
          <div class="score">Score: ${Number(r.score).toFixed(4)}</div>
          <span class="panel-badge">semantischer Treffer</span>
        </div>
        <div class="result-content">${escHtml(r.content)}</div>
        <div class="result-actions">
          <button class="result-action primary">Im Chat nutzen</button>
          <button class="result-action">Kopieren</button>
        </div>`;
      const [useBtn, copyBtn] = div.querySelectorAll('.result-action');
      useBtn.addEventListener('click', ()=>fillChatWithSearchHit(r.content));
      copyBtn.addEventListener('click', ()=>copyToClipboard(r.content, copyBtn));
      $('#searchResults').appendChild(div);
    });
  }catch(e){
    $('#searchResults').innerHTML = `<p class="tool-status err">${escHtml(e.message||String(e))}</p>`;
  }
}

async function addWiki(opts={}){
  const preserveSuggestions = !!opts.preserveSuggestions;
  const article = (opts.article ?? $('#wikiArticle').value).trim();
  const lang = $('#wikiLang').value.trim() || 'de';
  if(!article) return;

  if(!preserveSuggestions){
    const suggBox = $('#wikiSuggestions');
    if(suggBox) suggBox.innerHTML = '';
  }

  setLoading('#wikiBtn', true);
  setStatus($('#wikiStatus'), t('loading'), '');
  try{
    const embedModel = getIngestEmbedModel();
    const roles = getIngestRoleScopes();
    const r = await apiPost('/api/add-wiki', {article, lang, embed_model: embedModel, roles});
    if(r.not_found){
      const box = $('#wikiStatus');
      box.className = 'tool-status warn';
      box.innerHTML = '<div>'+t('not_found_intro')+'</div>';
      const suggBox = $('#wikiSuggestions');
      if(suggBox){
        suggBox.innerHTML = '';
        const list = document.createElement('ul');
        list.className = 'wiki-suggestions';
        (r.results||[]).forEach(item => {
          const li = document.createElement('li');
          const a = document.createElement('a');
          a.href = '#';
          a.textContent = item.title || String(item);
          a.addEventListener('click', async (e) => {
            e.preventDefault();
            $('#wikiArticle').value = item.title || item;
            // re-run the addWiki flow for the selected suggestion, but keep the list visible
            await addWiki({article: item.title || item, preserveSuggestions: true});
          });
          li.appendChild(a);
          // optional snippet hint
          if(item.snippet){
            const hint = document.createElement('div');
            hint.className = 'muted';
            hint.textContent = item.snippet.replace(/<[^>]+>/g, '');
            li.appendChild(hint);
          }
          list.appendChild(li);
        });
        suggBox.appendChild(list);
      }
      return;
    }
    setStatus($('#wikiStatus'), t('ok_chunks', r.chunks, r.total), 'ok');
    if(!preserveSuggestions) $('#wikiArticle').value = '';
    await refreshStats();
  }catch(e){
    setStatus($('#wikiStatus'), t('error_prefix') + (e.message||String(e)), 'err');
  }finally{
    setLoading('#wikiBtn', false);
  }
}

async function addURL(){
  const url = $('#scrapeUrl').value.trim();
  if(!url) return;
  setLoading('#urlBtn', true);
  setStatus($('#urlStatus'), t('scrape'), '');
  try{
    const embedModel = getIngestEmbedModel();
    const roles = getIngestRoleScopes();
    const r = await apiPost('/api/add-url', {url, embed_model: embedModel, roles});
    setStatus($('#urlStatus'), t('ok_chunks', r.chunks, r.total), 'ok');
    $('#scrapeUrl').value = '';
    await refreshStats();
  }catch(e){
    setStatus($('#urlStatus'), t('error_prefix') + (e.message||String(e)), 'err');
  }finally{
    setLoading('#urlBtn', false);
  }
}

async function addText(){
  const title = $('#textTitle').value.trim();
  const text = $('#textContent').value;
  if(!text.trim()) return;
  setLoading('#textBtn', true);
  setStatus($('#textStatus'), t('saving'), '');
  try{
    const embedModel = getIngestEmbedModel();
    const roles = getIngestRoleScopes();
    const r = await apiPost('/api/add-text', {title, text, embed_model: embedModel, roles});
    setStatus($('#textStatus'), t('ok_chunks', r.chunks, r.total), 'ok');
    $('#textTitle').value = '';
    $('#textContent').value = '';
    await refreshStats();
  }catch(e){
    setStatus($('#textStatus'), t('error_prefix') + (e.message||String(e)), 'err');
  }finally{
    setLoading('#textBtn', false);
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// Speech-to-Text (STT) — microphone input
// ─────────────────────────────────────────────────────────────────────────────

let sttActive = false;
let sttRecognition = null;
let sttMediaRecorder = null;
let sttAudioChunks = [];

function startSTT(onResult, onEnd){
  const SpeechRecognition = window.SpeechRecognition || window.webkitSpeechRecognition;
  if(SpeechRecognition){
    sttRecognition = new SpeechRecognition();
    sttRecognition.continuous = false;
    sttRecognition.interimResults = false;
    sttRecognition.lang = uiLang === 'de' ? 'de-DE' : 'en-US';
    sttRecognition.onresult = (e) => {
      const text = Array.from(e.results).map(r=>r[0].transcript).join('');
      onResult(text);
    };
    sttRecognition.onerror = () => { onEnd && onEnd(); };
    sttRecognition.onend  = () => { sttActive = false; onEnd && onEnd(); };
    sttRecognition.start();
    sttActive = true;
    return;
  }
  if(!navigator.mediaDevices){ onEnd && onEnd(); return; }
  navigator.mediaDevices.getUserMedia({audio:true}).then(stream => {
    sttAudioChunks = [];
    sttMediaRecorder = new MediaRecorder(stream);
    sttMediaRecorder.ondataavailable = e => sttAudioChunks.push(e.data);
    sttMediaRecorder.onstop = async () => {
      stream.getTracks().forEach(t=>t.stop());
      const blob = new Blob(sttAudioChunks, {type:'audio/webm'});
      const form = new FormData();
      form.append('file', blob, 'audio.webm');
      try{
        const resp = await fetch('/api/stt', {method:'POST', body:form});
        if(resp.ok){ const data = await resp.json(); if(data.text) onResult(data.text); }
      }catch(e){ console.error('STT error', e); }
      onEnd && onEnd();
    };
    sttMediaRecorder.start();
    sttActive = true;
  }).catch(() => { onEnd && onEnd(); });
}

function stopSTT(){
  sttActive = false;
  if(sttRecognition){ try{ sttRecognition.stop(); }catch(e){} sttRecognition = null; }
  if(sttMediaRecorder && sttMediaRecorder.state !== 'inactive'){
    try{ sttMediaRecorder.stop(); }catch(e){}
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// Text-to-Speech (TTS)
// ─────────────────────────────────────────────────────────────────────────────

let ttsAudio = null;

function stopTTS(){
  if(window.speechSynthesis) window.speechSynthesis.cancel();
  if(ttsAudio){ ttsAudio.pause(); ttsAudio.src=''; ttsAudio=null; }
}

function speakText(text, onDone){
  const clean = text
    .replace(/[#*_`~>|]/g,'')
    .replace(/!\[.*?\]\(.*?\)/g,'')
    .replace(/\[.*?\]\(.*?\)/g, m => m.replace(/\[|\]|\(.*?\)/g,''))
    .replace(/\s+/g,' ').trim().slice(0, MAX_TTS_LENGTH);
  if(window.speechSynthesis){
    const u = new SpeechSynthesisUtterance(clean);
    u.lang = uiLang === 'de' ? 'de-DE' : 'en-US';
    u.onend  = () => onDone && onDone();
    u.onerror = () => onDone && onDone();
    window.speechSynthesis.speak(u);
    return;
  }
  fetch('/api/tts', {
    method:'POST', headers:{'Content-Type':'application/json'},
    body: JSON.stringify({text: clean, voice:'alloy', model:'tts-1'})
  }).then(async resp => {
    if(!resp.ok){ onDone && onDone(); return; }
    const blob = await resp.blob();
    const url  = URL.createObjectURL(blob);
    ttsAudio = new Audio(url);
    ttsAudio.onended = () => { URL.revokeObjectURL(url); ttsAudio=null; onDone && onDone(); };
    ttsAudio.onerror = () => { URL.revokeObjectURL(url); ttsAudio=null; onDone && onDone(); };
    ttsAudio.play();
  }).catch(() => onDone && onDone());
}

// ─────────────────────────────────────────────────────────────────────────────
// Image attachment for multimodal / vision chat
// ─────────────────────────────────────────────────────────────────────────────

let attachedImageBase64 = '';
let attachedImageMime   = '';
let attachedImageName   = '';

function clearAttachedImage(){
  attachedImageBase64 = '';
  attachedImageMime   = '';
  attachedImageName   = '';
  const bar = $('#imagePreviewBar');
  if(bar) bar.style.display = 'none';
  const inp = $('#imageAttachInput');
  if(inp) inp.value = '';
}

function setAttachedImage(file){
  if(!file) return;
  attachedImageMime = file.type || 'image/jpeg';
  attachedImageName = file.name;
  const reader = new FileReader();
  reader.onload = (e) => {
    const dataUrl = e.target.result;
    const comma = dataUrl.indexOf(',');
    attachedImageBase64 = comma >= 0 ? dataUrl.slice(comma+1) : dataUrl;
    const bar  = $('#imagePreviewBar');
    const thumb = $('#imagePreviewThumb');
    const name  = $('#imagePreviewName');
    if(bar && thumb && name){
      thumb.src = dataUrl;
      name.textContent = file.name;
      bar.style.display = 'flex';
    }
  };
  reader.readAsDataURL(file);
}

async function askVision(question, imageBase64, mimeType){
  addMessage('assistant', '🔄 ' + t('processing'), new Date().toISOString());
  const bubbles = $$('#chatMessages .msg.assistant .bubble');
  if(bubbles.length){
    const b = bubbles[bubbles.length-1];
    b.classList.add('typing');
    b.textContent = t('assistant_typing');
  }
  let acc = '';
  try{
    const resp = await fetch('/api/vision', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({question, image_base64: imageBase64, mime_type: mimeType,
        chat_id: currentChatId, persona_id: currentPersonaId})
    });
    if(!resp.ok){ replaceAssistantLast(t('error')+': '+(await resp.text())); return; }
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    while(true){
      const {value, done} = await reader.read();
      if(done) break;
      buf += dec.decode(value, {stream:true});
      let idx;
      while((idx = buf.indexOf('\n\n')) >= 0){
        const raw = buf.slice(0, idx); buf = buf.slice(idx+2);
        const lines = raw.split('\n').filter(Boolean);
        let event = 'message', dataLines = [];
        for(const line of lines){
          if(line.startsWith('event:')) event = line.slice(6).trim();
          else if(line.startsWith('data:')) dataLines.push(line.slice(5).trim());
        }
        const dataStr = dataLines.join('\n');
        if(event === 'meta'){
          try{ const m = JSON.parse(dataStr); if(m.chat_id) currentChatId = m.chat_id; refreshChats(); }catch(e){}
          continue;
        }
        if(event === 'reasoning'){
          try{ const r = JSON.parse(dataStr); if(typeof r==='string') setAssistantLastThinking(r); }catch(e){}
          continue;
        }
        if(dataStr === '[DONE]'){
          try{
            const msgs2 = document.querySelectorAll('#chatMessages .msg.assistant .bubble');
            if(msgs2.length){ renderBubbleContent(msgs2[msgs2.length-1], stripInternalThinking(acc || '')); }
          }catch(e){}
          break;
        }
        try{ const tok = JSON.parse(dataStr); if(typeof tok==='string'){ acc+=tok; replaceAssistantLast(acc); } }catch(e){}
      }
    }
  }catch(e){ replaceAssistantLast(t('error')+': '+(e.message||String(e))); }
}

function initUpload(){
  const dz = $('#dropZone');
  const inp = $('#fileInput');

  dz.addEventListener('click', ()=>inp.click());

  // Add keyboard support for drop zone
  dz.addEventListener('keydown', (e)=>{
    if(e.key === 'Enter' || e.key === ' '){
      e.preventDefault();
      inp.click();
    }
  });

  function handleFile(file){
    if(!file) return;
    setStatus($('#uploadStatus'), t('uploading'), '');
    const form = new FormData();
    form.append('file', file);
    const embedModel = getIngestEmbedModel();
    if(embedModel) form.append('embed_model', embedModel);
    const roles = getIngestRoleScopes();
    if(roles.length) form.append('roles', roles.join(','));
    inp.disabled = true;
    fetch('/api/upload', {method:'POST', body: form})
      .then(async r=>{
        const ct = r.headers.get('content-type')||'';
        const isJson = ct.includes('application/json');
        const payload = isJson ? await r.json().catch(()=>null) : await r.text();
        if(!r.ok){
          throw new Error(typeof payload==='string' ? payload : JSON.stringify(payload));
        }
        setStatus($('#uploadStatus'), t('ok_chunks', payload.chunks, payload.total), 'ok');
        refreshStats();
      })
        .catch(e=>setStatus($('#uploadStatus'), t('error_prefix') + (e.message||String(e)), 'err'))
        .finally(()=>{ inp.disabled = false; });
  }

  inp.addEventListener('change', ()=>handleFile(inp.files[0]));

  dz.addEventListener('dragover', (e)=>{e.preventDefault(); dz.style.background='rgba(255,255,255,.04)';});
  dz.addEventListener('dragleave', ()=>{dz.style.background='';});
  dz.addEventListener('drop', (e)=>{
    e.preventDefault();
    dz.style.background='';
    const f = e.dataTransfer.files?.[0];
    handleFile(f);
  });
}

async function addFolder(){
  const path = $('#folderPath').value.trim();
  if(!path) return;
  const recursive = $('#folderRecursive').checked;
  setLoading('#folderBtn', true);
  setStatus($('#folderStatus'), t('importing'), '');
  try{
    const embedModel = getIngestEmbedModel();
    const roles = getIngestRoleScopes();
    const r = await apiPost('/api/add-folder', {path, recursive, embed_model: embedModel, roles});
    let msg = `OK: ${r.files} Dateien · ${r.total_chunks} Chunks · Total: ${r.total}`;
    if(r.errors && r.errors.length){
      msg += ` · Fehler: ${r.errors.length}`;
    }
    setStatus($('#folderStatus'), msg, r.errors?.length ? 'warn' : 'ok');
    await refreshStats();
  }catch(e){
    setStatus($('#folderStatus'), t('error_prefix') + (e.message||String(e)), 'err');
  }finally{
    setLoading('#folderBtn', false);
  }
}

// Main init
window.addEventListener('DOMContentLoaded', async ()=>{
  // load settings to determine UI language + theme
  try{
    const s = await apiGet('/api/settings');
    if(s && s.lang) applyTranslations(s.lang);
    else applyTranslations(navigator.language || 'de');
    if(s && s.theme) applyTheme(s.theme);
    currentRole = normalizeRoleValue(s?.active_role || currentRole);
    applyRolePermissionsUI(s?.role_permissions || null);
    const roleSel = $('#roleSelect');
    if(roleSel) roleSel.value = currentRole;
    syncIngestRoleScopesWithActiveRole(true);
    updateOpenAIBadge(s);
    initLLMSwitcher(s);
    updateWorkspaceStrip();
  }catch(e){
    applyTranslations(navigator.language || 'de');
  }

  await loadPersonas();

  if(window.mermaid){
    mermaid.initialize({startOnLoad:false, theme:'dark', securityLevel:'strict'});
  }
  // Tabs
  $$('.main-tab').forEach(b => {
    b.addEventListener('click', ()=>{ showTab('main', b.dataset.mainTab); closeSidebarIfMobile(); });
    b.addEventListener('keydown', (e)=>handleTabKeydown(e, '.main-tab', 'main'));
  });
  $$('.sidebar-tab').forEach(b => {
    b.addEventListener('click', ()=>showTab('sidebar', b.dataset.sidebarTab));
    b.addEventListener('keydown', (e)=>handleTabKeydown(e, '.sidebar-tab:not(.ingest-tabs .sidebar-tab)', 'sidebar'));
  });
  $$('.ingest-tabs .sidebar-tab').forEach(b => {
    b.addEventListener('click', ()=>showTab('ingest', b.dataset.ingestTab));
    b.addEventListener('keydown', (e)=>handleTabKeydown(e, '.ingest-tabs .sidebar-tab', 'ingest'));
  });

  $('#debugMode').addEventListener('change', (e)=>{ debugMode = e.target.checked; });
  const deepToggle = $('#deepMode');
  if(deepToggle){
    deepToggle.addEventListener('change', (e)=>{ deepMode = e.target.checked; updateWorkspaceStrip(); });
  }
  const offlineToggle = $('#offlineMode');
  if(offlineToggle){
    offlineToggle.addEventListener('change', (e)=>{ offlineMode = e.target.checked; updateWorkspaceStrip(); });
  }
  const autoSearchToggle = $('#autoSearchMode');
  if(autoSearchToggle){
    autoSearchMode = autoSearchToggle.checked;
    autoSearchToggle.addEventListener('change', (e)=>{ autoSearchMode = e.target.checked; updateWorkspaceStrip(); });
  }

  // Chat
  $('#chatBtn').addEventListener('click', askChat);

  // STT microphone button
  const sttBtn = $('#sttBtn');
  if(sttBtn){
    sttBtn.addEventListener('click', ()=>{
      if(sttActive){
        stopSTT();
        sttBtn.classList.remove('recording');
        sttBtn.setAttribute('aria-pressed','false');
        sttBtn.textContent = '🎤';
      } else {
        sttBtn.classList.add('recording');
        sttBtn.setAttribute('aria-pressed','true');
        sttBtn.textContent = '⏹';
        startSTT(
          (text) => {
            const box = $('#chatQ');
            if(box){ box.value += (box.value ? ' ' : '') + text; autosize(box); }
          },
          () => {
            sttBtn.classList.remove('recording');
            sttBtn.setAttribute('aria-pressed','false');
            sttBtn.textContent = '🎤';
          }
        );
      }
    });
  }

  // Image attachment button
  const imageAttachBtn = $('#imageAttachBtn');
  const imageAttachInput = $('#imageAttachInput');
  if(imageAttachBtn && imageAttachInput){
    imageAttachBtn.addEventListener('click', ()=> imageAttachInput.click());
    imageAttachInput.addEventListener('change', (e)=>{
      const file = e.target.files?.[0];
      if(file) setAttachedImage(file);
    });
    const removeBtn = $('#imagePreviewRemove');
    if(removeBtn) removeBtn.addEventListener('click', clearAttachedImage);
  }
  // Allow pasting images directly into the chat textarea
  const chatQEl = $('#chatQ');
  if(chatQEl){
    chatQEl.addEventListener('paste', (e)=>{
      const items = e.clipboardData?.items;
      if(!items) return;
      for(const item of items){
        if(item.type.startsWith('image/')){
          e.preventDefault();
          setAttachedImage(item.getAsFile());
          break;
        }
      }
    });
  }

  const personaSelect = $('#personaSelect');
  if(personaSelect){
    personaSelect.addEventListener('change', (e)=>{
      currentPersonaId = e.target.value;
      updateWorkspaceStrip();
    });
  }
  const roleSelect = $('#roleSelect');
  if(roleSelect){
    roleSelect.addEventListener('change', (e)=>{
      const nextRole = normalizeRoleValue(e.target.value);
      setActiveRole(nextRole);
    });
  }
  document.querySelectorAll('.ingest-role-scope').forEach(cb => {
    cb.addEventListener('change', ()=>syncIngestRoleScopesWithActiveRole());
  });
  const sidebarToggle = $('#sidebarToggle');
  if(sidebarToggle){
    sidebarToggle.addEventListener('click', ()=>{
      const sidebar = $('#appSidebar');
      setSidebarOpen(!sidebar?.classList.contains('open'));
    });
  }
  window.addEventListener('resize', ()=>{
    if(window.innerWidth > 960) setSidebarOpen(false);
  });
  const chatBox = $('#chatQ');
  if(chatBox){
    chatBox.addEventListener('input', ()=>autosize(chatBox));
    chatBox.addEventListener('keydown', (e)=>{
      if(e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.metaKey){
        e.preventDefault();
        askChat();
      }
    });
    chatBox.focus();
    autosize(chatBox);
  }
  const chatInput = $('#chatQ'); if(chatInput) chatInput.focus();

  // Search
  $('#searchBtn').addEventListener('click', runSearch);
  $('#searchQ').addEventListener('keydown', (e)=>{ if(e.key === 'Enter') runSearch(); });

  // Ingest
  $('#wikiBtn').addEventListener('click', addWiki);
  $('#urlBtn').addEventListener('click', addURL);
  $('#textBtn').addEventListener('click', addText);
  $('#folderBtn').addEventListener('click', addFolder);
  if($('#clearChunksBtn')){
    $('#clearChunksBtn').addEventListener('click', clearChunks);
  }
  onEnter($('#wikiArticle'), addWiki);
  onEnter($('#wikiLang'), addWiki);
  onEnter($('#scrapeUrl'), addURL);
  onEnter($('#textTitle'), addText);
  // Allow Ctrl/Cmd+Enter on textarea to send
  const txtArea = $('#textContent');
  if(txtArea){
    txtArea.addEventListener('keydown', (e)=>{
      if((e.ctrlKey || e.metaKey) && e.key === 'Enter'){
        e.preventDefault();
        addText();
      }
    });
  }
  onEnter($('#folderPath'), addFolder);
  initUpload();

  // Sidebar new chat
  $('#newChatBtn').addEventListener('click', newChat);

  // Settings modal
  $('#settingsBtn').addEventListener('click', async ()=>{
    openModal();
    await initSettingsUI();
  });
  $('#settingsClose').addEventListener('click', closeModal);
  $('#settingsModal').addEventListener('click', (e)=>{ if(e.target.id === 'settingsModal') closeModal(); });

  // Settings tabs
  $$('.settings-tab').forEach(b => {
    b.addEventListener('click', ()=>showSettingsTab(b.dataset.settingsTab));
    b.addEventListener('keydown', (e)=>{
      if(!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) return;
      e.preventDefault();
      const tabs = $$('.settings-tab');
      const currentIndex = tabs.indexOf(e.target);
      let newIndex = currentIndex;
      if(e.key === 'ArrowLeft'){
        newIndex = currentIndex > 0 ? currentIndex - 1 : tabs.length - 1;
      }else if(e.key === 'ArrowRight'){
        newIndex = currentIndex < tabs.length - 1 ? currentIndex + 1 : 0;
      }else if(e.key === 'Home'){
        newIndex = 0;
      }else if(e.key === 'End'){
        newIndex = tabs.length - 1;
      }
      const newTab = tabs[newIndex];
      if(newTab){
        newTab.focus();
        showSettingsTab(newTab.dataset.settingsTab);
      }
    });
  });

  // Theme cards
  $$('.theme-card').forEach(c => {
    c.addEventListener('click', ()=>setTheme(c.dataset.themeId));
    // Keyboard navigation for theme cards (radio group)
    c.addEventListener('keydown', (e)=>{
      if(e.key === 'Enter' || e.key === ' '){
        e.preventDefault();
        setTheme(c.dataset.themeId);
      }
    });
  });

  // Language selector
  const langSelect = $('#langSelect');
  if(langSelect){
    langSelect.addEventListener('change', async (e)=>{
      const newLang = e.target.value;
      applyTranslations(newLang);
      // Save language preference to settings
      try{
        await apiPost('/api/settings/lang', {lang: newLang});
      }catch(err){
        console.error('Failed to save language:', err);
      }
      updateWorkspaceStrip();
    });
  }

  $('#btnDiscover').addEventListener('click', discoverEndpoints);
  $('#btnTestEndpoint').addEventListener('click', testEndpointAndLoadModels);
  $('#btnSaveSettings').addEventListener('click', ()=>saveSettings(false));
  $('#btnAddCustomApi').addEventListener('click', addCustomApi);
  $('#btnAddPersona').addEventListener('click', addPersona);
  const btnAddAdminUser = $('#btnAddAdminUser'); if(btnAddAdminUser) btnAddAdminUser.addEventListener('click', addAdminUser);
  const btnSamples = $('#btnAddSamplePersonas'); if(btnSamples) btnSamples.addEventListener('click', addSamplePersonas);

  // Branding
  const btnSaveBranding = $('#btnSaveBranding');
  if(btnSaveBranding) btnSaveBranding.addEventListener('click', saveBranding);

  // Auth & Security tab
  const btnSaveAuth = $('#btnSaveAuth');
  if(btnSaveAuth) btnSaveAuth.addEventListener('click', saveAuthSettings);
  const btnSaveLDAP = $('#btnSaveLDAP');
  if(btnSaveLDAP) btnSaveLDAP.addEventListener('click', saveLDAPSettings);
  const btnAddWebUser = $('#btnAddWebUser');
  if(btnAddWebUser) btnAddWebUser.addEventListener('click', addWebUser);

  // Logout button (sidebar)
  const logoutBtn = $('#logoutBtn');
  if(logoutBtn) logoutBtn.addEventListener('click', logoutUser);

  // Check session state and show user bar if applicable
  await initSessionUI();

  await refreshStats();
  // Check backend LLM status and show popup if not available
  try{
    const st = await apiGet('/api/llm/status');
    if(!st.ok){
      showLLMMissingModal(st.message || 'LLM endpoint nicht erreichbar');
    }
  }catch(e){
    showLLMMissingModal(e.message||String(e));
  }
  await refreshChats();
  updateWorkspaceStrip();
});
