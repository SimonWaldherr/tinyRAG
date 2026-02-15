const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

function escHtml(s){
  return s.replace(/[&<>"']/g, (c) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

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
      return marked.parse(md, {renderer, breaks:true});
    }catch(e){
      // fallback on parse error
      return escHtml(md).replace(/\n/g, '<br>');
    }
  }
  return escHtml(md).replace(/\n/g, '<br>');
}

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

function renderBubbleContent(el, content){
  el.dataset.raw = content;
  const html = renderMarkdown(content);
  el.innerHTML = html;
  // If marked.js isn't loaded, use pre-wrap for plain text fallback
  el.classList.toggle('plain', !window.marked);
  renderMermaidIn(el);
}

function timeShort(iso){
  try{
    const d = new Date(iso);
    return d.toLocaleString(undefined, {hour:'2-digit', minute:'2-digit', year:'2-digit', month:'2-digit', day:'2-digit'});
  }catch(e){ return ''; }
}

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

function setStatus(el, msg, cls){
  el.textContent = msg;
  el.className = 'tool-status' + (cls ? ' '+cls : '');
}

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
    debug: 'Debug',
    debug_description: 'Zeigt die RAG-Kontextdaten an, die das System für die Antwort verwendet',
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
    personas: 'Personas',
    appearance: 'Erscheinungsbild',
    language: 'Sprache / Language',
    theme: 'Theme',
    endpoint_note: 'Hinweis: Das Endpoint-Feld erwartet die Basis-URL ohne <code>/v1</code>. Falls du <code>/v1</code> einfügst, wird es automatisch entfernt.',
    allow_nanogo: 'Erlaube Ausführung von nanoGo (interpretiertes Go)',
    nanogo_hint: 'Empfohlen: nur aktivieren, wenn du Ausführungen aus vertrauenswürdigen Quellen zulassen willst.',
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
    persona_prompt_placeholder: 'Instruktionen, Tonalität, Stil…'
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
    debug: 'Debug',
    debug_description: 'Shows RAG context data that the system uses for the response',
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
    personas: 'Personas',
    appearance: 'Appearance',
    language: 'Language',
    theme: 'Theme',
    endpoint_note: 'Note: The endpoint field expects the base URL without <code>/v1</code>. If you enter <code>/v1</code>, it will be automatically removed.',
    allow_nanogo: 'Allow execution of nanoGo (interpreted Go)',
    nanogo_hint: 'Recommended: only enable if you want to allow executions from trusted sources.',
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
    persona_prompt_placeholder: 'Instructions, tone, style…'
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
let debugMode = false;
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

function msgElement(role, content, timeIso){
  const msg = document.createElement('div');
  msg.className = `msg ${role}`;
  const bubble = document.createElement('div');
  bubble.className = 'bubble';
  renderBubbleContent(bubble, content);
  const meta = document.createElement('div');
  meta.className = 'meta';
  meta.textContent = `${role === 'user' ? 'Du' : 'Assistant'} · ${timeShort(timeIso)}`;
  msg.appendChild(bubble);
  msg.appendChild(meta);
  return msg;
}

function addMessage(role, content, timeIso){
  const wrap = $('#chatMessages');
  $('#chatEmpty').style.display = 'none';
  wrap.appendChild(msgElement(role, content, timeIso || new Date().toISOString()));
  wrap.scrollTop = wrap.scrollHeight;
}

function replaceAssistantLast(text){
  const msgs = $$('#chatMessages .msg.assistant .bubble');
  if(msgs.length === 0) return;
  const bubble = msgs[msgs.length-1];
  renderBubbleContent(bubble, text);
  bubble.classList.remove('typing');
  const wrap = $('#chatMessages');
  wrap.scrollTop = wrap.scrollHeight;
}

async function refreshStats(){
  try{
    const stats = await apiGet('/api/stats');
    $('#chunkCount').textContent = stats.chunks ?? '-';
    // Also refresh sources list when open
    await refreshSources(stats.sources || []);
  }catch(e){}
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
          $('#chatMessages').innerHTML = `<div class="empty-state" id="chatEmpty"><div class="icon">💬</div><p>Stelle eine Frage an deine Wissensbasis.<br>Die Antwort basiert auf den gespeicherten Dokumenten.</p></div>`;
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
  $('#chatMessages').innerHTML = `<div class="empty-state" id="chatEmpty" style="display:none"></div>`;
  if(!c.messages || !c.messages.length){
    $('#chatEmpty').style.display = '';
  }else{
    c.messages.forEach(m => addMessage(m.role, m.content, m.time));
  }
  await refreshChats();
  showTab('sidebar','chats');
  showTab('main','chat');
}

async function newChat(){
  const c = await apiPost('/api/chats/new', {persona_id: currentPersonaId});
  currentChatId = c.id;
  $('#chatMessages').innerHTML = `<div class="empty-state" id="chatEmpty"><div class="icon">💬</div><p>Stelle eine Frage an deine Wissensbasis.<br>Die Antwort basiert auf den gespeicherten Dokumenten.</p></div>`;
  await refreshChats();
  showTab('main','chat');
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
  $('#wikiLang').value = s.lang || 'de';
  $('#setBaseUrl').value = s.base_url || 'http://localhost:1234';
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

  await loadCustomApis();
  await loadPersonas();
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
    const r = await apiPost('/api/llm/list-models', {base_url: base});
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

  }catch(e){
    setStatus($('#endpointStatus'), 'Fehler: '+(e.message||String(e)), 'err');
  }
}

async function saveSettings(force=false){
  const base = $('#setBaseUrl').value.trim();
  const chat = $('#setChatModel').value;
  const emb = $('#setEmbedModel').value;
  const allowNano = $('#allowNanoGo') ? !!$('#allowNanoGo').checked : false;
  if(!base || !chat || !emb){
    setStatus($('#saveStatus'), 'Bitte Endpoint und Modelle wählen.', 'err');
    return;
  }
  setStatus($('#saveStatus'), 'Speichere…', '');
  try{
    await apiPost('/api/settings', {base_url: base, chat_model: chat, embed_model: emb, force, allow_nanogo: allowNano});
    setStatus($('#saveStatus'), 'Gespeichert. Einstellungen aktiv.', 'ok');
    closeModal();
  }catch(e){
    if(e.status === 409 && e.payload && e.payload.requires_force){
      setStatus($('#saveStatus'), e.payload.message + ' (Nochmal klicken zum Bestätigen)', 'warn');
      // Second click confirms
      $('#btnSaveSettings').onclick = ()=>saveSettings(true);
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

async function addPersona(){
  const name = $('#newPersonaName').value.trim();
  const prompt = $('#newPersonaPrompt').value.trim();
  if(!name){
    alert('Bitte Namen angeben');
    return;
  }
  await apiPost('/api/personas', {name, prompt});
  $('#newPersonaName').value = '';
  $('#newPersonaPrompt').value = '';
  await loadPersonas();
}

// Tool suggestion UI (adapted from original tinyRAG)
var toolIcons={wikipedia:'\u{1F4D6}',duckduckgo:'\u{1F50E}',wiktionary:'\u{1F4DD}',stackoverflow:'\u{1F4BB}',websearch:'\u{1F50D}'};
var toolLabels={wikipedia:'Wikipedia-Suche',duckduckgo:'DuckDuckGo Websuche',wiktionary:'Wiktionary (Wörterbuch)',stackoverflow:'StackOverflow-Suche',websearch:'Websuche'};
var cachedCustomAPIs=[];
var cachedPersonas=[];

function renderToolSuggestion(tr, chatEl, originalQuestion){
  const div = document.createElement('div');
  div.className = 'tool-suggestion';
  const builtinTools = ['wikipedia','duckduckgo','wiktionary','stackoverflow','websearch'];
  let h = '<div class="tool-header"><span class="tool-icon">\u{1F50D}</span> Zusätzliche Informationen benötigt</div>';
  h += '<div class="tool-desc">Das Modell hat nicht genügend Kontext. Suchbegriff anpassen und eine Quelle wählen:</div>';
  h += '<input class="tool-query-edit" type="text" value="'+escHtml(tr.query)+'">';
  h += '<div class="tool-actions">';
  h += '<span class="tool-actions-label">Suchen mit:</span>';
  for(let i=0;i<builtinTools.length;i++){
    const t = builtinTools[i];
    const icon = toolIcons[t]||'\u{1F527}';
    const label = toolLabels[t]||t;
    const suggested = (t===tr.tool) ? ' suggested' : '';
    h += '<button class="tool-btn'+suggested+'" data-tool="'+t+'">'+icon+' '+label+'</button>';
  }
  // custom APIs
  for(let j=0;j<cachedCustomAPIs.length;j++){
    const api = cachedCustomAPIs[j];
    const suggested2 = (api.id===tr.tool)?' suggested':'';
    h += '<button class="tool-btn'+suggested2+'" data-tool="'+escHtml(api.id)+'">\u{1F310} '+escHtml(api.name)+'</button>';
  }
  h += '<button class="btn-reject">\u274C Ablehnen</button>';
  h += '</div>';
  h += '<div class="tool-status"></div>';
  div.innerHTML = h;
  div.dataset.originalQuestion = originalQuestion;
  // attach handlers
  div.querySelectorAll('.tool-btn').forEach(b=>{
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
  const actions = card.querySelector('.tool-actions'); if(actions) actions.style.display = 'none';
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
  card.querySelectorAll('.tool-btn,.btn-reject').forEach(b=>b.disabled=true);
  const input = card.querySelector('.tool-query-edit'); if(input) input.disabled = true;
  status.innerHTML = '<span class="spinner"></span>' + (toolIcons[tool]||'') + ' ' + escHtml(toolLabels[tool]||tool) + ': Suche läuft…';

  fetch('/api/tool/execute',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({tool:tool,query:query})}).then(async function(resp){
    if(!resp.ok){
      const t = await resp.text();
      status.innerHTML = '<span style="color:var(--red)">Fehler: '+escHtml(t)+'</span>';
      card.querySelectorAll('.tool-btn,.btn-reject').forEach(b=>b.disabled=false);
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
    card.querySelectorAll('.tool-btn,.btn-reject').forEach(b=>b.disabled=false);
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
  if(!q) return;
  $('#chatQ').value = '';
  autosize($('#chatQ'));
  lastDebugData = null;
  addMessage('user', q, new Date().toISOString());

  // placeholder assistant msg
  addMessage('assistant', '🔄 Wird bearbeitet...', new Date().toISOString());
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
  let hasError = false;

  try{
    const resp = await fetch('/api/ask', {
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify({question:q, chat_id: currentChatId, debug: debugMode, persona_id: currentPersonaId})
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
            // refresh chats sidebar
            refreshChats();
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
                // Strip [TOOL_REQUEST] markers, then render final markdown
                const cleaned = raw.replace(/\[TOOL_REQUEST\]\s*\{[^}]*\}\s*\[\/TOOL_REQUEST\]/g,'').trim();
                renderBubbleContent(bubble, cleaned);
              }
            }
          }catch(e){}
          break;
        }
        try{
          const tok = JSON.parse(dataStr);
          if(typeof tok === 'string'){
            acc += tok;
            replaceAssistantLast(acc);
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
      div.innerHTML = `<div class="score">Score: ${Number(r.score).toFixed(4)}</div><div>${escHtml(r.content)}</div>`;
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
    const r = await apiPost('/api/add-wiki', {article, lang});
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
    const r = await apiPost('/api/add-url', {url});
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
    const r = await apiPost('/api/add-text', {title, text});
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
    const r = await apiPost('/api/add-folder', {path, recursive});
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
  }catch(e){
    applyTranslations(navigator.language || 'de');
  }

  await loadPersonas();

  if(window.mermaid){
    mermaid.initialize({startOnLoad:false, theme:'dark', securityLevel:'strict'});
  }
  // Tabs
  $$('.main-tab').forEach(b => {
    b.addEventListener('click', ()=>showTab('main', b.dataset.mainTab));
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

  // Chat
  $('#chatBtn').addEventListener('click', askChat);
  const personaSelect = $('#personaSelect');
  if(personaSelect){
    personaSelect.addEventListener('change', (e)=>{
      currentPersonaId = e.target.value;
    });
  }
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
    });
  }

  $('#btnDiscover').addEventListener('click', discoverEndpoints);
  $('#btnTestEndpoint').addEventListener('click', testEndpointAndLoadModels);
  $('#btnSaveSettings').addEventListener('click', ()=>saveSettings(false));
  $('#btnAddCustomApi').addEventListener('click', addCustomApi);
  $('#btnAddPersona').addEventListener('click', addPersona);

  await refreshStats();
  await refreshChats();
});
