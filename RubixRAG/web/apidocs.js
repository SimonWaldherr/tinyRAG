// R3 API-Dokumentation — a small, dependency-free OpenAPI/Swagger-style
// viewer for /api/openapi.json, in the same "plain JS, no build step"
// spirit as the main app (web/app.js). Deliberately not the reference
// swagger-ui bundle: that's ~2 MB of vendored third-party JS for an API
// with four endpoints, and pulling it from a CDN would make this page
// depend on internet egress R3 itself doesn't otherwise need. This covers
// the same core job — browse endpoints, see request/response shapes, fire
// a real "try it out" request — against the exact same standard OpenAPI
// 3.0 document, which can still be pasted into the real Swagger Editor via
// the link in apidocs.html if that specific tool is wanted.

function $(sel) { return document.querySelector(sel); }
function escapeHTML(s) {
  return String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// resolveSchema follows a single $ref and flattens allOf (used by
// CreateAPIKeyResponse = APIKeyPublic + {key}) into one plain schema
// object, so the renderer below never has to special-case either. Only
// resolves one level of $ref per call by design — nested $refs inside
// properties are resolved lazily, at render time, so a schema referenced
// from many places isn't eagerly expanded into a huge duplicated tree.
function resolveSchema(schema, spec) {
  if (!schema) return {};
  if (schema.$ref) {
    const name = schema.$ref.split("/").pop();
    return spec.components?.schemas?.[name] || {};
  }
  if (schema.allOf) {
    const merged = { type: "object", properties: {}, required: [] };
    for (const part of schema.allOf) {
      const resolved = resolveSchema(part, spec);
      Object.assign(merged.properties, resolved.properties || {});
      merged.required.push(...(resolved.required || []));
    }
    return merged;
  }
  return schema;
}

// schemaTypeLabel renders a short, human-readable type description for
// one property's schema, e.g. "array of SourceInfo" or "string (enum: local, azure)".
function schemaTypeLabel(schema, spec) {
  if (schema.$ref) return schema.$ref.split("/").pop();
  if (schema.allOf) return schema.allOf.map(s => schemaTypeLabel(s, spec)).join(" & ");
  if (schema.type === "array") return `array of ${schemaTypeLabel(schema.items || {}, spec)}`;
  let label = schema.type || "object";
  if (schema.format) label += ` (${schema.format})`;
  if (schema.enum) label += ` (enum: ${schema.enum.join(", ")})`;
  return label;
}

// renderSchemaTable renders an object schema's properties as a table —
// name, type, required, description — resolving one level of $ref per
// property so nested object shapes (e.g. AskJSONResponse.citations[].source_id)
// are still visible without a click-through.
function renderSchemaTable(schemaRef, spec) {
  const schema = resolveSchema(schemaRef, spec);
  if (schema.type === "array") {
    return `<p class="hint">Array von: ${escapeHTML(schemaTypeLabel(schema.items || {}, spec))}</p>` +
      renderSchemaTable(schema.items || {}, spec);
  }
  const props = schema.properties || {};
  const required = new Set(schema.required || []);
  const rows = Object.entries(props).map(([name, propSchema]) => {
    const resolved = propSchema.$ref ? resolveSchema(propSchema, spec) : propSchema;
    return `<tr>
      <td><code>${escapeHTML(name)}</code></td>
      <td>${escapeHTML(schemaTypeLabel(propSchema, spec))}</td>
      <td>${required.has(name) ? "ja" : ""}</td>
      <td>${escapeHTML(resolved.description || propSchema.description || "")}</td>
    </tr>`;
  });
  if (!rows.length) return "";
  return `<div class="table-scroll"><table class="schema-table">
    <thead><tr><th>Feld</th><th>Typ</th><th>Pflicht</th><th>Beschreibung</th></tr></thead>
    <tbody>${rows.join("")}</tbody>
  </table></div>`;
}

// exampleFromSchema builds a plausible JSON example for a request body,
// so the "Try it out" textarea starts pre-filled with something valid
// (or close to it) instead of an empty "{}" the caller has to guess the
// shape of. depth guards against a pathological self-referential schema
// recursing forever; nothing in this API's spec is anywhere near that
// deep, this is just a safety backstop.
function exampleFromSchema(schemaRef, spec, depth) {
  depth = depth || 0;
  if (depth > 6) return null;
  const schema = resolveSchema(schemaRef, spec);
  if (schema.enum) return schema.enum[0];
  switch (schema.type) {
    case "string": return "";
    case "integer": case "number": return 0;
    case "boolean": return false;
    case "array": return [];
    case "object": default: {
      if (!schema.properties) return {};
      const out = {};
      const required = new Set(schema.required || []);
      for (const [name, propSchema] of Object.entries(schema.properties)) {
        if (!required.has(name)) continue; // keep the example minimal — only what's actually required
        out[name] = exampleFromSchema(propSchema, spec, depth + 1);
      }
      return out;
    }
  }
}

// securityBadges renders one badge per named security scheme this
// operation accepts (falling back to the spec-level default), so it's
// visible at a glance whether an endpoint is open, API-key-gated or
// admin-session-only — the same three-way distinction docs/API.md
// documents in prose.
function securityBadges(operation, spec) {
  const reqs = operation.security || spec.security || [];
  const names = new Set();
  reqs.forEach(req => Object.keys(req).forEach(n => names.add(n)));
  if (names.size === 0 || (names.size === 1 && [...names][0] === undefined)) return "";
  const labels = { ApiKeyHeader: "🔑 API-Key (Header)", ApiKeyBearer: "🔑 API-Key (Bearer)", AdminSession: "🔒 Admin-Sitzung" };
  const badges = [...names].filter(Boolean).map(n => `<span class="security-badge">${escapeHTML(labels[n] || n)}</span>`);
  if (!badges.length) return "";
  if (reqs.some(r => Object.keys(r).length === 0)) {
    badges.push(`<span class="security-badge">ohne Auth ebenfalls erlaubt</span>`);
  }
  return `<div class="security-badges">${badges.join("")}</div>`;
}

// runTryItOut fires the actual HTTP request a "Try it out" panel was
// built for and renders the raw response — status line plus body, pretty-
// printed if it parses as JSON, shown as-is otherwise (covers /api/ask's
// default NDJSON stream, which is many JSON lines, not one JSON value).
async function runTryItOut(method, path, bodyText, apiKey, resultEl) {
  resultEl.innerHTML = `<p class="hint">Sende Anfrage…</p>`;
  const headers = {};
  let body;
  if (bodyText && bodyText.trim()) {
    headers["Content-Type"] = "application/json";
    body = bodyText;
  }
  if (apiKey) headers["X-API-Key"] = apiKey;
  try {
    const res = await fetch(path, { method, headers, body, credentials: "same-origin" });
    const text = await res.text();
    let pretty = text;
    try { pretty = JSON.stringify(JSON.parse(text), null, 2); } catch { /* not a single JSON value (e.g. NDJSON) — show raw */ }
    const statusClass = res.status >= 500 ? "status-5xx" : res.status >= 400 ? "status-4xx" : "status-2xx";
    resultEl.innerHTML = `<p><span class="status-code ${statusClass}">${res.status} ${escapeHTML(res.statusText)}</span></p>
      <pre>${escapeHTML(pretty || "(leerer Antwortkörper)")}</pre>`;
  } catch (err) {
    resultEl.innerHTML = `<p class="status-code status-5xx">Anfrage fehlgeschlagen: ${escapeHTML(err.message)}</p>`;
  }
}

// renderEndpoint builds one collapsible <details> card for a single
// path+method operation — summary line always visible, everything else
// (description, security, request/response schemas, try-it-out) revealed
// on click, so a four-endpoint API doesn't need a separate nav sidebar.
function renderEndpoint(path, method, operation, spec) {
  const el = document.createElement("details");
  el.className = "endpoint";

  const requestSchemaRef = operation.requestBody?.content?.["application/json"]?.schema;
  const responses = operation.responses || {};

  const responseBlocks = Object.entries(responses).map(([code, resp]) => {
    const resolvedResp = resp.$ref ? resolveSchema(resp, spec) : resp;
    const jsonSchema = resolvedResp.content?.["application/json"]?.schema;
    const ndjsonSchema = resolvedResp.content?.["application/x-ndjson"]?.schema;
    const statusClass = code >= "500" ? "status-5xx" : code >= "400" ? "status-4xx" : "status-2xx";
    return `<div class="response-block">
      <p><span class="status-code ${statusClass}">${escapeHTML(code)}</span> — ${escapeHTML(resolvedResp.description || "")}</p>
      ${jsonSchema ? renderSchemaTable(jsonSchema, spec) : ""}
      ${ndjsonSchema ? `<p class="hint">${escapeHTML(ndjsonSchema.description || "")}</p>` : ""}
    </div>`;
  }).join("");

  const exampleBody = requestSchemaRef ? JSON.stringify(exampleFromSchema(requestSchemaRef, spec), null, 2) : "";
  const tryOutId = `try-${method}-${path}`.replace(/[^a-zA-Z0-9]/g, "-");

  el.innerHTML = `
    <summary>
      <span class="method-badge method-${method}">${method.toUpperCase()}</span>
      <span class="endpoint-path">${escapeHTML(path)}</span>
      <span class="endpoint-summary">${escapeHTML(operation.summary || "")}</span>
    </summary>
    <div class="endpoint-body">
      <p>${escapeHTML(operation.description || "")}</p>
      ${securityBadges(operation, spec)}
      ${requestSchemaRef ? `<h3>Request Body</h3>${renderSchemaTable(requestSchemaRef, spec)}` : ""}
      <h3>Antworten</h3>
      ${responseBlocks}
      <h3>Ausprobieren</h3>
      <textarea class="try-body" id="${tryOutId}-body" ${requestSchemaRef ? "" : "disabled placeholder=\"Kein Request Body\""}>${escapeHTML(exampleBody)}</textarea>
      <div class="try-controls">
        <button type="button" id="${tryOutId}-send">Senden</button>
      </div>
      <div class="try-result" id="${tryOutId}-result"></div>
    </div>`;

  el.querySelector(`#${tryOutId}-send`).addEventListener("click", () => {
    const bodyText = requestSchemaRef ? el.querySelector(`#${tryOutId}-body`).value : "";
    const apiKey = $("#apiKeyInput").value.trim();
    runTryItOut(method, path, bodyText, apiKey, el.querySelector(`#${tryOutId}-result`));
  });

  return el;
}

async function init() {
  let spec;
  try {
    const res = await fetch("/api/openapi.json");
    if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
    spec = await res.json();
  } catch (err) {
    $("#loadError").hidden = false;
    $("#loadError").textContent = `OpenAPI-Spec konnte nicht geladen werden: ${err.message}`;
    return;
  }

  $("#apiTitle").textContent = spec.info?.title || "API-Dokumentation";
  $("#apiDescription").textContent = spec.info?.description || "";
  const editorLink = $("#externalEditorLink");
  editorLink.href = "https://editor.swagger.io/?url=" + encodeURIComponent(location.origin + "/api/openapi.json");

  const container = $("#endpoints");
  for (const [path, methods] of Object.entries(spec.paths || {})) {
    for (const [method, operation] of Object.entries(methods)) {
      container.appendChild(renderEndpoint(path, method, operation, spec));
    }
  }
}

init();
