/*
 * Copyright (C) 2026 GorillaHacker <gorillahacker@yandex.ru> https://t.me/gorillahacker
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

let createRepeaterTabWithRequest = null;
let createAutomatorTabWithRequest = null;

document.addEventListener("DOMContentLoaded", () => {
  const projectEl = document.querySelector("[data-project-id]");
  if (projectEl) {
    const projectId = projectEl.getAttribute("data-project-id");
    setupEncodingSettings(projectId);
    setupTabs(projectId);
    setupEncodeDecodePopup();
    setupTargets(projectId);
    setupProxy(projectId);
    setupProjectSettings(projectId);
    setupModules(projectId);
    setupInterceptor(projectId);
    setupRepeater(projectId);
    setupAutomator(projectId);
    document.querySelectorAll(".code-view .code").forEach((el) => setupCodeHighlightSync(el));
  }
});

let encodingReq = "utf-8";
let encodingResp = "utf-8";

function getEncoding(kind) {
  return kind === "req" ? encodingReq : encodingResp;
}

function setEncoding(kind, value) {
  if (kind === "req") {
    encodingReq = value;
  } else {
    encodingResp = value;
  }
}

function base64ToBytes(b64) {
  if (!b64) return null;
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

function decodeBase64(b64, encoding) {
  if (!b64) return null;
  try {
    const bytes = base64ToBytes(b64);
    if (!bytes) return null;
    return new TextDecoder(encoding).decode(bytes);
  } catch (err) {
    return null;
  }
}

function findHeaderSplit(bytes) {
  for (let i = 0; i < bytes.length - 3; i++) {
    if (bytes[i] === 13 && bytes[i + 1] === 10 && bytes[i + 2] === 13 && bytes[i + 3] === 10) {
      return i;
    }
  }
  return -1;
}

function parseHeaders(headerText) {
  const lines = headerText.split("\r\n");
  const headers = {};
  for (let i = 1; i < lines.length; i++) {
    const line = lines[i];
    const idx = line.indexOf(":");
    if (idx === -1) continue;
    const key = line.slice(0, idx).trim().toLowerCase();
    const value = line.slice(idx + 1).trim();
    headers[key] = value;
  }
  return headers;
}

function dechunkBody(bytes) {
  let offset = 0;
  const chunks = [];
  while (offset < bytes.length) {
    const lineEnd = indexOfCrlf(bytes, offset);
    if (lineEnd === -1) break;
    const sizeLine = bytesToAscii(bytes.slice(offset, lineEnd)).trim();
    const size = parseInt(sizeLine, 16);
    if (!Number.isFinite(size) || size <= 0) break;
    offset = lineEnd + 2;
    const chunk = bytes.slice(offset, offset + size);
    chunks.push(chunk);
    offset += size + 2;
  }
  return concatBytes(chunks);
}

function indexOfCrlf(bytes, start) {
  for (let i = start; i < bytes.length - 1; i++) {
    if (bytes[i] === 13 && bytes[i + 1] === 10) return i;
  }
  return -1;
}

function bytesToAscii(bytes) {
  let out = "";
  for (let i = 0; i < bytes.length; i++) {
    out += String.fromCharCode(bytes[i]);
  }
  return out;
}

function concatBytes(chunks) {
  const total = chunks.reduce((sum, c) => sum + c.length, 0);
  const out = new Uint8Array(total);
  let offset = 0;
  chunks.forEach((c) => {
    out.set(c, offset);
    offset += c.length;
  });
  return out;
}

async function decompressBytes(bytes, encoding) {
  if (!bytes || !encoding) return bytes;
  const enc = encoding.toLowerCase();
  if (!["gzip", "deflate", "br"].includes(enc)) return bytes;
  if (typeof DecompressionStream === "undefined") return bytes;
  try {
    const stream = new DecompressionStream(enc);
    const decompressed = new Response(new Blob([bytes]).stream().pipeThrough(stream));
    const buffer = await decompressed.arrayBuffer();
    return new Uint8Array(buffer);
  } catch (err) {
    return bytes;
  }
}

async function decodeResponsePretty(b64, encoding) {
  const data = await decodeResponseData(b64, encoding);
  if (!data) return null;
  return `${data.headerText}\r\n\r\n${data.bodyText}`;
}

function hasReplacementChars(text) {
  return text.includes("\uFFFD");
}

function parseCharset(contentType) {
  if (!contentType) return "";
  const match = contentType.match(/charset=([^;]+)/i);
  return match ? match[1].trim().toLowerCase() : "";
}

function decodeWithFallbacks(bytes, preferred) {
  const candidates = [preferred, "utf-8", "windows-1251", "koi8-r", "iso-8859-1"].filter(Boolean);
  for (const enc of candidates) {
    try {
      const text = new TextDecoder(enc).decode(bytes);
      if (!hasReplacementChars(text) || enc === preferred) {
        return text;
      }
    } catch (err) {
      continue;
    }
  }
  return new TextDecoder("utf-8").decode(bytes);
}

async function decodeResponseData(b64, encoding) {
  const bytes = base64ToBytes(b64);
  if (!bytes) return null;
  const split = findHeaderSplit(bytes);
  if (split === -1) return null;
  const headerBytes = bytes.slice(0, split);
  const bodyBytes = bytes.slice(split + 4);
  const headerText = new TextDecoder("iso-8859-1").decode(headerBytes);
  const headers = parseHeaders(headerText);
  let body = bodyBytes;
  if (headers["transfer-encoding"] && headers["transfer-encoding"].toLowerCase().includes("chunked")) {
    body = dechunkBody(body);
  }
  if (headers["content-encoding"]) {
    body = await decompressBytes(body, headers["content-encoding"]);
  }
  let bodyText = "";
  if (encoding === "universal") {
    const charset = parseCharset(headers["content-type"] || "");
    const preferred = charset || "utf-8";
    bodyText = decodeWithFallbacks(body, preferred);
  } else {
    bodyText = new TextDecoder(encoding).decode(body);
  }
  return { headerText, headers, bodyText };
}

function escapeHtml(value) {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function wrapClass(text, cls) {
  return `<span class="${cls}">${escapeHtml(text)}</span>`;
}

function normalizeLineEndings(text) {
  return text.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
}

function highlightHttp(text, isRequest) {
  if (!text || typeof text !== "string") return escapeHtml(text || "");
  const normalized = normalizeLineEndings(text);
  const idx = normalized.indexOf("\n\n");
  const head = idx >= 0 ? normalized.slice(0, idx) : normalized;
  const body = idx >= 0 ? normalized.slice(idx + 2) : "";
  const lines = head.split("\n");
  let out = "";
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (i === 0) {
      if (isRequest && line.includes("?")) {
        const q = line.indexOf("?");
        const sp = line.indexOf(" ", q);
        const path = line.slice(0, q + 1);
        const query = sp >= 0 ? line.slice(q + 1, sp) : line.slice(q + 1);
        const rest = sp >= 0 ? line.slice(sp) : "";
        out += escapeHtml(path);
        const params = query.split("&");
        for (let j = 0; j < params.length; j++) {
          if (j > 0) out += "&";
          const eq = params[j].indexOf("=");
          if (eq >= 0) {
            out += wrapClass(params[j].slice(0, eq), "hl-param-name");
            out += "=";
            out += wrapClass(escapeHtml(params[j].slice(eq + 1)), "hl-param-value");
          } else {
            out += escapeHtml(params[j]);
          }
        }
        out += escapeHtml(rest) + "\n";
      } else {
        out += escapeHtml(line) + "\n";
      }
      continue;
    }
    const colon = line.indexOf(":");
    if (colon >= 0) {
      out += wrapClass(line.slice(0, colon), "hl-header");
      out += ":";
      const val = line.slice(colon + 1);
      const key = line.slice(0, colon).trim().toLowerCase();
      if (key === "cookie") {
        const parts = val.split(";");
        for (let j = 0; j < parts.length; j++) {
          if (j > 0) out += "; ";
          const eq = parts[j].trim().indexOf("=");
          const part = parts[j].trim();
          if (eq >= 0) {
            out += wrapClass(part.slice(0, eq), "hl-param-name");
            out += "=";
            out += wrapClass(escapeHtml(part.slice(eq + 1)), "hl-param-value");
          } else {
            out += escapeHtml(part);
          }
        }
        out += "\n";
      } else {
        out += escapeHtml(val) + "\n";
      }
    } else {
      out += escapeHtml(line) + "\n";
    }
  }
  out += "\n";
  if (body) {
    const ct = head.toLowerCase();
    const isJson = /content-type:\s*[^\n]*application\/json/i.test(ct);
    const isForm = /content-type:\s*[^\n]*application\/x-www-form-urlencoded/i.test(ct);
    if (isJson && /^\s*[\{\[]/.test(body)) {
      out += highlightJson(body);
    } else if (isForm) {
      const params = body.split("&");
      for (let j = 0; j < params.length; j++) {
        if (j > 0) out += "&";
        const eq = params[j].indexOf("=");
        if (eq >= 0) {
          out += wrapClass(params[j].slice(0, eq), "hl-param-name");
          out += "=";
          out += wrapClass(escapeHtml(params[j].slice(eq + 1)), "hl-param-value");
        } else {
          out += escapeHtml(params[j]);
        }
      }
    } else {
      out += escapeHtml(body);
    }
  }
  return out;
}

function highlightJson(text) {
  const out = [];
  let i = 0;
  const len = text.length;
  while (i < len) {
    const c = text[i];
    if (c === '"') {
      let j = i + 1;
      while (j < len && text[j] !== '"') {
        if (text[j] === "\\") j += 2;
        else j++;
      }
      const key = text.slice(i, j + 1);
      const after = text.slice(j + 1);
      const colon = after.match(/^\s*:/);
      if (colon && /^\s*:\s*"/.test(after)) {
        out.push(wrapClass(key, "hl-json-key"));
        const strMatch = after.match(/^\s*:\s*"/);
        let k = j + 1 + (strMatch ? strMatch[0].length : 0);
        if (k < len && text[k] === '"') {
          k++;
          while (k < len && text[k] !== '"') {
            if (text[k] === "\\") k += 2;
            else k++;
          }
          k++;
          out.push(wrapClass(text.slice(j + 1, k), "hl-json-value"));
          i = k;
        } else {
          const num = after.match(/^\s*:\s*(-?\d+\.?\d*|true|false|null)/);
          if (num) {
            out.push(wrapClass(text.slice(j + 1, j + 1 + num[0].length), "hl-json-value"));
            i = j + 1 + num[0].length;
          } else {
            out.push(escapeHtml(text.slice(j + 1, j + 2)));
            i = j + 2;
          }
        }
      } else if (colon && /^\s*:\s*(-?\d+\.?\d*|true|false|null)/.test(after)) {
        out.push(wrapClass(key, "hl-json-key"));
        const num = after.match(/^\s*:\s*(-?\d+\.?\d*|true|false|null)/);
        if (num) {
          out.push(wrapClass(text.slice(j + 1, j + 1 + num[0].length), "hl-json-value"));
          i = j + 1 + num[0].length;
        } else {
          out.push(escapeHtml(key));
          i = j + 1;
        }
      } else if (colon && /^\s*:\s*[\{\[]/.test(after)) {
        out.push(wrapClass(key, "hl-json-key"));
        out.push(escapeHtml(after.slice(0, after.search(/\S/))));
        i = j + 1 + after.search(/\S/);
        const stack = [text[i] === "{" ? "}" : "]"];
        let depth = 1;
        let pos = i + 1;
        while (depth > 0 && pos < len) {
          const ch = text[pos];
          if (ch === '"') {
            pos++;
            while (pos < len && text[pos] !== '"') {
              if (text[pos] === "\\") pos += 2;
              else pos++;
            }
            pos++;
          } else if (ch === "{" || ch === "[") {
            stack.push(ch === "{" ? "}" : "]");
            depth++;
            pos++;
          } else if (ch === "}" || ch === "]") {
            stack.pop();
            depth--;
            pos++;
          } else pos++;
        }
        out.push(highlightJson(text.slice(i, pos)));
        i = pos;
      } else {
        out.push(wrapClass(key, "hl-json-value"));
        i = j + 1;
      }
    } else {
      out.push(escapeHtml(c));
      i++;
    }
  }
  return out.join("");
}

async function renderResponseFrame(frame, b64, encoding) {
  if (!frame) return;
  const data = await decodeResponseData(b64, encoding);
  if (!data) {
    frame.srcdoc = "<pre>Нет данных для рендера.</pre>";
    return;
  }
  const contentType = (data.headers["content-type"] || "").toLowerCase();
  const body = data.bodyText || "";
  if (contentType.includes("text/html") || contentType.includes("application/xhtml+xml")) {
    frame.srcdoc = body;
  } else if (contentType.startsWith("text/") || contentType.includes("json") || contentType.includes("xml")) {
    frame.srcdoc = `<pre>${escapeHtml(body)}</pre>`;
  } else {
    frame.srcdoc = "<pre>Render доступен только для текстовых ответов (HTML/JSON/XML).</pre>";
  }
}

function setupEncodingSettings(projectId) {
  const reqSelect = document.getElementById("encoding-req");
  const respSelect = document.getElementById("encoding-resp");
  if (!reqSelect || !respSelect) return;

  fetch(`/api/projects/settings?project_id=${projectId}`)
    .then((res) => res.json())
    .then((data) => {
      encodingReq = data.encoding_req || "utf-8";
      encodingResp = data.encoding_resp || "utf-8";
      reqSelect.value = encodingReq;
      respSelect.value = encodingResp;
      refreshEncodingViews();
    })
    .catch(() => {});

  reqSelect.addEventListener("change", async () => {
    setEncoding("req", reqSelect.value);
    try {
      await fetch(`/api/projects/settings?project_id=${projectId}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ encoding_req: encodingReq, encoding_resp: encodingResp }),
      });
    } catch (err) {}
    refreshEncodingViews();
  });
  respSelect.addEventListener("change", async () => {
    setEncoding("resp", respSelect.value);
    try {
      await fetch(`/api/projects/settings?project_id=${projectId}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ encoding_req: encodingReq, encoding_resp: encodingResp }),
      });
    } catch (err) {}
    refreshEncodingViews();
  });
}

function refreshEncodingViews() {
  const reqView = document.getElementById("req-view");
  const respView = document.getElementById("resp-view");
  const respRender = document.getElementById("resp-render");
  const repeaterResp = document.getElementById("repeater-resp");
  const autoReq = document.getElementById("automator-req-view");
  const autoResp = document.getElementById("automator-resp-view");
  const targetsReq = document.getElementById("targets-req");
  const targetsResp = document.getElementById("targets-resp");
  const proxyRoot = document.getElementById("tab-proxy");
  if (reqView) renderView(reqView, "req", getActiveDetailMode("req", proxyRoot));
  if (respView) renderView(respView, "resp", getActiveDetailMode("resp", proxyRoot));
  if (respRender && respRender.classList.contains("active")) {
    renderResponseFrame(respRender, respView.dataset.b64 || "", getEncoding("resp"));
  }
  const repeaterRoot = document.getElementById("tab-repeater");
  const repeaterRespRender = document.getElementById("repeater-resp-render");
  if (repeaterResp && repeaterRoot) {
    if (repeaterRespRender && repeaterRespRender.classList.contains("active")) {
      renderResponseFrame(repeaterRespRender, repeaterResp.dataset.b64 || "", getEncoding("resp"));
    } else {
      renderView(repeaterResp, "resp", getActiveDetailMode("resp", repeaterRoot));
    }
  }
  if (autoReq) renderSimpleView(autoReq, "req");
  if (autoResp) renderSimpleView(autoResp, "resp");
  if (targetsReq) renderSimpleView(targetsReq, "req");
  if (targetsResp) renderSimpleView(targetsResp, "resp");
}

function getDisplayForView(view) {
  if (!view || !view.parentElement) return null;
  return view.parentElement.querySelector(".code-display");
}

function resizeCodeInner(view) {
  const inner = view.parentElement;
  const scroll = inner?.parentElement;
  const codeView = view.closest(".code-view");
  if (!inner?.classList.contains("code-inner") || !scroll?.classList.contains("code-scroll")) return;
  const minH = scroll.clientHeight || codeView?.clientHeight || 374;
  const isEditableOverlay = codeView?.classList.contains("code-highlight-mode") && !codeView.classList.contains("code-view-pretty");
  if (isEditableOverlay) {
    const contentH = Math.max(minH, view.scrollHeight);
    inner.style.height = contentH + "px";
  } else {
    inner.style.height = minH + "px";
  }
}

const READ_ONLY_PRETTY_IDS = new Set([
  "targets-req", "targets-resp", "req-view", "resp-view",
  "repeater-resp", "automator-modal-req-view", "automator-modal-resp-view"
]);

function isReadOnlyPrettyView(view) {
  return view?.id && READ_ONLY_PRETTY_IDS.has(view.id);
}

function updateCodeDisplay(view, text, isRequest, useHighlight) {
  if (!view) return;
  const codeScroll = view.closest(".code-scroll");
  const codeView = view.closest(".code-view");
  const wasEditableOverlay = codeView?.classList.contains("code-highlight-mode") && !codeView.classList.contains("code-view-pretty");
  const savedScroll = wasEditableOverlay && codeScroll ? codeScroll.scrollTop : view.scrollTop;
  view.value = text || "";
  const display = getDisplayForView(view);
  if (!display) return;
  if (useHighlight && text) {
    display.innerHTML = highlightHttp(text, isRequest);
    view.classList.add("code-highlight-mode");
    if (codeView) {
      codeView.classList.add("code-highlight-mode");
      if (isReadOnlyPrettyView(view)) {
        display.classList.add("code-display-pretty");
        codeView.classList.add("code-view-pretty");
      } else {
        display.classList.remove("code-display-pretty");
        codeView.classList.remove("code-view-pretty");
      }
    }
    if (!isReadOnlyPrettyView(view) && codeScroll) {
      requestAnimationFrame(() => {
        codeScroll.scrollTop = savedScroll;
      });
    }
  } else {
    display.textContent = text || "";
    display.classList.remove("code-display-pretty");
    view.classList.remove("code-highlight-mode");
    if (codeView) {
      codeView.classList.remove("code-highlight-mode");
      codeView.classList.remove("code-view-pretty");
    }
    if (wasEditableOverlay && codeScroll) {
      view.scrollTop = savedScroll;
    }
  }
  resizeCodeInner(view);
  requestAnimationFrame(() => {
    resizeCodeInner(view);
    setTimeout(() => resizeCodeInner(view), 50);
  });
}

function isEditableOverlayView(view) {
  const codeView = view?.closest(".code-view");
  return codeView?.classList.contains("code-highlight-mode") && !codeView.classList.contains("code-view-pretty");
}

function setupCodeHighlightSync(view) {
  const display = getDisplayForView(view);
  if (!display || !view) return;
  const codeScroll = view.closest(".code-scroll");
  const codeInner = view.parentElement;
  const codeView = view.closest(".code-view");
  if (codeScroll && codeInner?.classList.contains("code-inner")) {
    const syncLayout = () => {
      codeInner.style.width = codeScroll.clientWidth + "px";
      resizeCodeInner(view);
      if (codeView?.classList.contains("code-view-pretty")) {
        display.style.minHeight = display.scrollHeight + "px";
      } else if (isEditableOverlayView(view)) {
        display.style.minHeight = view.scrollHeight + "px";
        display.style.width = view.clientWidth + "px";
      } else {
        display.style.minHeight = view.scrollHeight + "px";
        display.style.width = view.clientWidth + "px";
      }
    };
    const syncDisplayScroll = () => {
      if (!isEditableOverlayView(view)) {
        display.style.transform = `translate(${-view.scrollLeft}px, ${-view.scrollTop}px)`;
      }
    };
    syncLayout();
    syncDisplayScroll();
    new ResizeObserver(() => {
      syncLayout();
      syncDisplayScroll();
    }).observe(codeScroll);
    view.addEventListener("scroll", syncDisplayScroll);
    view.addEventListener("wheel", () => {
      requestAnimationFrame(syncDisplayScroll);
    }, { passive: true });
  }
  view.addEventListener("input", () => {
    if (view.classList.contains("code-highlight-mode")) {
      const isRequest = /^(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS|CONNECT)\s/i.test(view.value);
      display.innerHTML = highlightHttp(view.value, isRequest);
    }
    resizeCodeInner(view);
  });
  resizeCodeInner(view);
}

async function renderView(view, kind, mode) {
  if (!view) return;
  const display = getDisplayForView(view);
  if (mode === "hex") {
    const hex = view.dataset.hex || "";
    view.value = hex;
    if (display) {
      display.textContent = hex;
      display.classList.remove("code-display-pretty");
      view.classList.remove("code-highlight-mode");
      const codeView = view.closest(".code-view");
      if (codeView) codeView.classList.remove("code-view-pretty");
    }
    resizeCodeInner(view);
    return;
  }
  let text = "";
  if (kind === "resp" && mode === "pretty") {
    const pretty = await decodeResponsePretty(view.dataset.b64, getEncoding("resp"));
    if (pretty !== null) {
      text = pretty;
    }
  }
  if (!text) {
    const encoding = getEncoding(kind);
    const decoded = decodeBase64(view.dataset.b64, encoding);
    text = decoded !== null ? decoded : view.dataset.raw || "";
  }
  const isRequest = kind === "req";
  updateCodeDisplay(view, text, isRequest, mode === "pretty");
}

async function renderSimpleView(view, kind) {
  const encoding = getEncoding(kind);
  let text = "";
  if (kind === "resp") {
    const pretty = await decodeResponsePretty(view.dataset.b64, encoding);
    if (pretty !== null) {
      text = pretty;
    }
  }
  if (!text) {
    const decoded = decodeBase64(view.dataset.b64, encoding);
    text = decoded !== null ? decoded : view.dataset.raw || "";
  }
  const isRequest = kind === "req";
  updateCodeDisplay(view, text, isRequest, true);
}

function scrollTextareaToPosition(textarea, from) {
  const style = getComputedStyle(textarea);
  const mirror = document.createElement("div");
  mirror.style.position = "absolute";
  mirror.style.visibility = "hidden";
  mirror.style.pointerEvents = "none";
  mirror.style.zIndex = "-1";
  mirror.style.whiteSpace = "pre-wrap";
  mirror.style.wordWrap = "break-word";
  mirror.style.overflow = "hidden";
  mirror.style.top = "0";
  mirror.style.left = "-99999px";
  mirror.style.width = `${textarea.clientWidth}px`;
  mirror.style.fontFamily = style.fontFamily;
  mirror.style.fontSize = style.fontSize;
  mirror.style.fontWeight = style.fontWeight;
  mirror.style.fontStyle = style.fontStyle;
  mirror.style.letterSpacing = style.letterSpacing;
  mirror.style.textTransform = style.textTransform;
  mirror.style.textIndent = style.textIndent;
  mirror.style.lineHeight = style.lineHeight;
  mirror.style.padding = style.padding;
  mirror.style.border = style.border;
  mirror.style.boxSizing = style.boxSizing;
  const before = textarea.value.slice(0, from);
  const markerChar = textarea.value.slice(from, from + 1) || " ";
  mirror.textContent = before;
  const marker = document.createElement("span");
  marker.textContent = markerChar;
  mirror.appendChild(marker);
  document.body.appendChild(mirror);
  const markerTop = marker.offsetTop;
  const markerLeft = marker.offsetLeft;
  document.body.removeChild(mirror);
  const codeScroll = textarea.closest(".code-scroll");
  const isEditableOverlay = codeScroll && textarea.closest(".code-view")?.classList.contains("code-highlight-mode") && !textarea.closest(".code-view")?.classList.contains("code-view-pretty");
  if (isEditableOverlay && codeScroll) {
    codeScroll.scrollTop = Math.max(0, markerTop - codeScroll.clientHeight / 2);
    codeScroll.scrollLeft = Math.max(0, markerLeft - codeScroll.clientWidth / 2);
  } else {
    textarea.scrollTop = Math.max(0, markerTop - textarea.clientHeight / 2);
    textarea.scrollLeft = Math.max(0, markerLeft - textarea.clientWidth / 2);
    const display = textarea.parentElement?.querySelector(".code-display");
    if (display) {
      display.style.transform = `translate(${-textarea.scrollLeft}px, ${-textarea.scrollTop}px)`;
    }
  }
}

function scrollToPosition(element, text, from) {
  if (!element || from < 0 || element.clientWidth <= 0) return;
  const style = getComputedStyle(element);
  const mirror = document.createElement("div");
  mirror.style.cssText = "position:absolute;visibility:hidden;pointer-events:none;z-index:-1;top:0;left:-99999px;overflow:hidden;box-sizing:border-box;";
  mirror.style.whiteSpace = style.whiteSpace || "pre-wrap";
  mirror.style.wordWrap = style.wordWrap || "break-word";
  mirror.style.width = `${element.clientWidth}px`;
  mirror.style.fontFamily = style.fontFamily;
  mirror.style.fontSize = style.fontSize;
  mirror.style.lineHeight = style.lineHeight;
  mirror.style.padding = style.padding;
  const before = (text || "").slice(0, from);
  mirror.textContent = before || "\u200b";
  document.body.appendChild(mirror);
  const markerTop = mirror.offsetHeight;
  document.body.removeChild(mirror);
  element.scrollTop = Math.max(0, markerTop - element.clientHeight / 2);
}

function getNodeAndOffsetAtOffset(element, charOffset) {
  const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT, null, false);
  let count = 0;
  let node = null;
  let offset = 0;
  while (walker.nextNode()) {
    const textNode = walker.currentNode;
    const len = textNode.textContent.length;
    if (count + len >= charOffset) {
      return { node: textNode, offset: charOffset - count };
    }
    count += len;
  }
  const last = walker.currentNode;
  return last ? { node: last, offset: last.textContent.length } : { node: element, offset: 0 };
}

function focusTextareaMatch(textarea, start, end) {
  if (!textarea) return;
  const len = textarea.value.length;
  const safeStart = Math.max(0, Math.min(start, len));
  const safeEnd = Math.max(safeStart, Math.min(end, len));
  const display = getDisplayForView(textarea);
  const codeView = textarea.closest(".code-view");
  const isReadOnly = isReadOnlyPrettyView(textarea) && codeView?.classList.contains("code-view-pretty");
  const applyFocus = () => {
    if (isReadOnly && display) {
      scrollToPosition(display, textarea.value, safeStart);
      try {
        const startPos = getNodeAndOffsetAtOffset(display, safeStart);
        const endPos = getNodeAndOffsetAtOffset(display, safeEnd);
        if (startPos.node && endPos.node) {
          const range = document.createRange();
          range.setStart(startPos.node, startPos.offset);
          range.setEnd(endPos.node, endPos.offset);
          const sel = window.getSelection();
          sel.removeAllRanges();
          sel.addRange(range);
          if (display.tabIndex !== -1) display.tabIndex = -1;
          display.focus();
        }
      } catch (_) {}
    } else {
      textarea.focus();
      textarea.setSelectionRange(safeStart, safeEnd, "forward");
      scrollTextareaToPosition(textarea, safeStart);
    }
  };
  requestAnimationFrame(applyFocus);
  setTimeout(applyFocus, 0);
}

function getActiveDetailMode(kind, root) {
  const selector = kind === "req" ? ".detail-tabs .tab.active[data-detail^='req']" : ".detail-tabs .tab.active[data-detail^='resp']";
  const scope = root || document;
  const active = scope.querySelector(selector);
  if (active && active.dataset.detail.endsWith("hex")) {
    return "hex";
  }
  if (active && active.dataset.detail.endsWith("pretty")) {
    return "pretty";
  }
  return "raw";
}

function setupTabs(projectId) {
  document.querySelectorAll(".tabs .tab").forEach((tab) => {
    tab.addEventListener("click", () => {
      document.querySelectorAll(".tabs .tab").forEach((t) => t.classList.remove("active"));
      document.querySelectorAll(".tab-panel").forEach((panel) => panel.classList.remove("active"));
      tab.classList.add("active");
      const target = document.getElementById(`tab-${tab.dataset.tab}`);
      if (target) target.classList.add("active");
      if (projectId) {
        localStorage.setItem(`abp_active_tab_${projectId}`, tab.dataset.tab);
      }
    });
  });

  document.querySelectorAll(".detail-tabs .tab").forEach((tab) => {
    tab.addEventListener("click", () => {
      const group = tab.parentElement;
      group.querySelectorAll(".tab").forEach((t) => t.classList.remove("active"));
      tab.classList.add("active");
    });
  });

  if (projectId) {
    const saved = localStorage.getItem(`abp_active_tab_${projectId}`);
    if (saved) {
      const savedTab = document.querySelector(`.tabs .tab[data-tab="${saved}"]`);
      if (savedTab) {
        savedTab.click();
      }
    }
  }
}

function setupEncodeDecodePopup() {
  const popup = document.getElementById("encode-decode-popup");
  const actionSel = document.getElementById("encode-decode-action");
  const formatSel = document.getElementById("encode-decode-format");
  const resultEl = document.getElementById("encode-decode-result");
  if (!popup || !actionSel || !formatSel || !resultEl) return;

  let lastSelectedText = "";
  const MAX_RESULT_DISPLAY = 500;

  function decodeUrl(s) {
    try {
      return decodeURIComponent(s.replace(/\+/g, " "));
    } catch (_) {
      return null;
    }
  }

  function encodeUrl(s) {
    try {
      return encodeURIComponent(s);
    } catch (_) {
      return null;
    }
  }

  function decodeHtml(s) {
    const el = document.createElement("textarea");
    el.innerHTML = s;
    return el.value;
  }

  function encodeHtml(s) {
    return s
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function decodeBase64Str(s) {
    try {
      return atob(s.replace(/\s/g, ""));
    } catch (_) {
      return null;
    }
  }

  function encodeBase64Str(s) {
    try {
      return btoa(s);
    } catch (_) {
      return null;
    }
  }

  function decodeHex(s) {
    const clean = s.replace(/\s/g, "").replace(/^0x/i, "");
    if (clean.length % 2) return null;
    try {
      const bytes = [];
      for (let i = 0; i < clean.length; i += 2) {
        bytes.push(parseInt(clean.substr(i, 2), 16));
      }
      return new TextDecoder().decode(new Uint8Array(bytes));
    } catch (_) {
      return null;
    }
  }

  function encodeHex(s) {
    const bytes = new TextEncoder().encode(s);
    return Array.from(bytes)
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("");
  }

  function decodeOctal(s) {
    const clean = s.replace(/\s/g, "");
    try {
      const bytes = [];
      for (let i = 0; i < clean.length; i += 3) {
        const chunk = clean.substr(i, 3);
        if (chunk.length < 3) break;
        bytes.push(parseInt(chunk, 8));
      }
      return new TextDecoder().decode(new Uint8Array(bytes));
    } catch (_) {
      return null;
    }
  }

  function encodeOctal(s) {
    const bytes = new TextEncoder().encode(s);
    return Array.from(bytes)
      .map((b) => b.toString(8).padStart(3, "0"))
      .join("");
  }

  function decodeBinary(s) {
    const clean = s.replace(/\s/g, "");
    if (clean.length % 8) return null;
    try {
      const bytes = [];
      for (let i = 0; i < clean.length; i += 8) {
        bytes.push(parseInt(clean.substr(i, 8), 2));
      }
      return new TextDecoder().decode(new Uint8Array(bytes));
    } catch (_) {
      return null;
    }
  }

  function encodeBinary(s) {
    const bytes = new TextEncoder().encode(s);
    return Array.from(bytes)
      .map((b) => b.toString(2).padStart(8, "0"))
      .join("");
  }

  function decodeAscii(s) {
    const clean = s.replace(/\s/g, "");
    if (clean.length % 2) return null;
    try {
      const bytes = [];
      for (let i = 0; i < clean.length; i += 2) {
        bytes.push(parseInt(clean.substr(i, 2), 16));
      }
      return new TextDecoder().decode(new Uint8Array(bytes));
    } catch (_) {
      return null;
    }
  }

  function encodeAscii(s) {
    const bytes = new TextEncoder().encode(s);
    return Array.from(bytes)
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("");
  }

  async function decompressGzip(bytes) {
    try {
      const ds = new DecompressionStream("gzip");
      const blob = new Blob([bytes]);
      const stream = blob.stream().pipeThrough(ds);
      const result = await new Response(stream).arrayBuffer();
      return new TextDecoder().decode(new Uint8Array(result));
    } catch (_) {
      return null;
    }
  }

  async function compressGzip(s) {
    try {
      const cs = new CompressionStream("gzip");
      const blob = new Blob([new TextEncoder().encode(s)]);
      const stream = blob.stream().pipeThrough(cs);
      const result = await new Response(stream).arrayBuffer();
      return Array.from(new Uint8Array(result))
        .map((b) => b.toString(16).padStart(2, "0"))
        .join("");
    } catch (_) {
      return null;
    }
  }

  function computeResult() {
    const action = actionSel.value;
    const format = formatSel.value;
    const text = lastSelectedText;
    if (!text) {
      resultEl.textContent = "";
      resultEl.dataset.fullResult = "";
      return;
    }
    let result = null;
    if (action === "decode") {
      switch (format) {
        case "url":
          result = decodeUrl(text);
          break;
        case "html":
          result = decodeHtml(text);
          break;
        case "base64":
          result = decodeBase64Str(text);
          break;
        case "ascii":
          result = decodeAscii(text);
          break;
        case "hex":
          result = decodeHex(text);
          break;
        case "octal":
          result = decodeOctal(text);
          break;
        case "binary":
          result = decodeBinary(text);
          break;
        case "gzip":
          (async () => {
            try {
              const clean = text.replace(/\s/g, "");
              let bytes;
              if (/^[0-9a-fA-F]+$/.test(clean)) {
                bytes = new Uint8Array(clean.match(/.{1,2}/g).map((b) => parseInt(b, 16)));
              } else {
                const binary = atob(clean);
                bytes = new Uint8Array(binary.length);
                for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
              }
              resultEl.textContent = "…";
              const r = await decompressGzip(bytes);
              const full = r || "(ошибка декодирования)";
              resultEl.dataset.fullResult = full;
              resultEl.textContent =
                full.length > MAX_RESULT_DISPLAY
                  ? full.slice(0, MAX_RESULT_DISPLAY) + "…"
                  : full;
            } catch (_) {
              resultEl.dataset.fullResult = "(ошибка)";
              resultEl.textContent = "(ошибка декодирования)";
            }
          })();
          return;
        default:
          result = text;
      }
    } else {
      switch (format) {
        case "url":
          result = encodeUrl(text);
          break;
        case "html":
          result = encodeHtml(text);
          break;
        case "base64":
          result = encodeBase64Str(text);
          break;
        case "ascii":
          result = encodeAscii(text);
          break;
        case "hex":
          result = encodeHex(text);
          break;
        case "octal":
          result = encodeOctal(text);
          break;
        case "binary":
          result = encodeBinary(text);
          break;
        case "gzip":
          (async () => {
            resultEl.textContent = "…";
            const r = await compressGzip(text);
            const full = r || "(ошибка кодирования)";
            resultEl.dataset.fullResult = full;
            resultEl.textContent =
              full.length > MAX_RESULT_DISPLAY
                ? full.slice(0, MAX_RESULT_DISPLAY) + "…"
                : full;
          })();
          return;
        default:
          result = text;
      }
    }
    const full = result !== null ? String(result) : "(ошибка)";
    resultEl.dataset.fullResult = full;
    resultEl.textContent =
      full.length > MAX_RESULT_DISPLAY
        ? full.slice(0, MAX_RESULT_DISPLAY) + "…"
        : full;
  }

  actionSel.addEventListener("change", computeResult);
  formatSel.addEventListener("change", computeResult);

  resultEl.addEventListener("click", () => {
    const full = resultEl.dataset.fullResult || "";
    if (full && full !== "(ошибка)" && full !== "(ошибка декодирования)" && full !== "(ошибка кодирования)") {
      navigator.clipboard.writeText(full).then(() => {
        const origDisplay = resultEl.textContent;
        resultEl.textContent = "Скопировано";
        setTimeout(() => {
          resultEl.textContent = origDisplay;
        }, 600);
      }).catch(() => {});
    }
  });

  document.addEventListener("click", (e) => {
    if (!popup.contains(e.target)) {
      popup.classList.add("hidden");
    }
  });

  function showPopupAt(e, selectedText) {
    lastSelectedText = selectedText;
    popup.classList.remove("hidden");
    popup.style.left = e.clientX + "px";
    popup.style.top = e.clientY + "px";
    requestAnimationFrame(() => {
      const rect = popup.getBoundingClientRect();
      if (rect.right > window.innerWidth) popup.style.left = window.innerWidth - rect.width + "px";
      if (rect.bottom > window.innerHeight) popup.style.top = window.innerHeight - rect.height + "px";
    });
    computeResult();
  }

  document.querySelectorAll(".code-view").forEach((codeView) => {
    codeView.addEventListener(
      "contextmenu",
      (e) => {
        const sel = window.getSelection();
        const selectedText = (sel && sel.toString ? sel.toString() : "").trim();
        if (selectedText) {
          e.preventDefault();
          e.stopPropagation();
          showPopupAt(e, selectedText);
        }
      },
      true
    );
  });
}

function setupTargets(projectId) {
  const targetsRoot = document.getElementById("tab-targets");
  const domainList = document.getElementById("targets-domain-list");
  const targetsTable = document.getElementById("targets-table-body");
  const targetsReq = document.getElementById("targets-req");
  const targetsResp = document.getElementById("targets-resp");
  const targetsRespRender = document.getElementById("targets-resp-render");
  const searchInput = document.getElementById("targets-search");
  const searchScope = document.getElementById("targets-search-scope");
  const searchPrev = document.getElementById("targets-search-prev");
  const searchNext = document.getElementById("targets-search-next");
  const searchFocus = document.getElementById("targets-search-focus");
  if (!domainList || !targetsTable || !targetsReq || !targetsResp) return;

  let selectedId = null;
  let selectedHost = "";
  let selectedPath = "";
  let selectedPathType = "";
  let targetsSortCol = "time";
  let targetsSortDir = "desc";
  let lastRequests = [];
  let openHosts = new Set();
  let openPaths = new Set();
  let searchMatches = [];
  let searchIndex = 0;

  const reqCodeView = targetsReq ? targetsReq.closest(".code-view") : null;
  const respCodeView = targetsResp ? targetsResp.closest(".code-view") : null;
  function syncTargetsHeights(h) {
    const height = Math.max(374, h);
    if (reqCodeView) reqCodeView.style.height = height + "px";
    if (respCodeView) respCodeView.style.height = height + "px";
    requestAnimationFrame(() => {
      resizeCodeInner(targetsReq);
      resizeCodeInner(targetsResp);
    });
  }
  const targetsRo = new ResizeObserver((entries) => {
    for (const e of entries) {
      const el = e.target;
      if (el === targetsReq || el === targetsResp) syncTargetsHeights(el.offsetHeight);
    }
  });
  targetsRo.observe(targetsReq);
  targetsRo.observe(targetsResp);

  function buildTree(paths) {
    const root = { children: {} };
    (paths || []).forEach((path) => {
      const clean = path || "/";
      const parts = clean.split("/").filter(Boolean);
      const isDirectory = clean.endsWith("/");
      if (parts.length === 0) {
        root.children["/"] = root.children["/"] || { children: {}, leaf: true };
        root.children["/"].leaf = true;
        return;
      }
      let node = root;
      parts.forEach((part, idx) => {
        if (!node.children[part]) {
          node.children[part] = { children: {} };
        }
        node = node.children[part];
        const isLast = idx === parts.length - 1;
        if (isLast) {
          node.leaf = !isDirectory;
        }
      });
    });
    return root;
  }

  function renderTree(node, host, basePath) {
    const keys = Object.keys(node.children || {});
    if (keys.length === 0) return null;
    keys.sort();
    const ul = document.createElement("ul");
    keys.forEach((key) => {
      const child = node.children[key];
      const li = document.createElement("li");
      const hasChildren = child && Object.keys(child.children || {}).length > 0;
      const isDirectory = child && child.leaf === false;
      const currentPath = basePath ? `${basePath}/${key}` : `/${key}`;
      if (hasChildren || isDirectory) {
        const details = document.createElement("details");
        details.dataset.path = `${host}${currentPath}`;
        const summary = document.createElement("summary");
        const icon = document.createElement("span");
        icon.className = "tree-icon";
        icon.textContent = "📁";
        const label = document.createElement("span");
        label.textContent = key;
        summary.appendChild(icon);
        summary.appendChild(label);
        details.appendChild(summary);
        if (openPaths.has(details.dataset.path)) {
          details.open = true;
        }
        summary.addEventListener("click", (event) => {
          event.preventDefault();
          event.stopPropagation();
          selectedHost = host;
          selectedPath = currentPath;
          selectedPathType = "dir";
          if (details.open) {
            openPaths.delete(details.dataset.path);
            details.open = false;
          } else {
            openPaths.add(details.dataset.path);
            details.open = true;
          }
          renderRequests(lastRequests);
        });
        const nested = renderTree(child, host, currentPath);
        if (nested) details.appendChild(nested);
        li.appendChild(details);
      } else {
        const icon = document.createElement("span");
        icon.className = "tree-icon";
        icon.textContent = "📄";
        const label = document.createElement("span");
        label.textContent = key;
        li.classList.add("tree-leaf");
        li.appendChild(icon);
        li.appendChild(label);
        li.addEventListener("click", (event) => {
          event.preventDefault();
          event.stopPropagation();
          selectedHost = host;
          selectedPath = currentPath;
          selectedPathType = "file";
          renderRequests(lastRequests);
        });
      }
      ul.appendChild(li);
    });
    return ul;
  }

  function renderDomains(domains) {
    domainList.innerHTML = "";
    (domains || []).forEach((domain) => {
      const details = document.createElement("details");
      details.dataset.host = domain.host;
      const summary = document.createElement("summary");
      const badge = document.createElement("span");
      badge.className = `domain-badge${domain.has_tls ? " secure" : ""}`;
      badge.textContent = domain.has_tls ? "TLS" : "HTTP";
      const name = document.createElement("span");
      name.textContent = domain.host;
      summary.appendChild(badge);
      summary.appendChild(name);
      details.appendChild(summary);
      if (selectedHost === domain.host || openHosts.has(domain.host)) {
        details.open = true;
        summary.classList.add("active");
      }
      summary.addEventListener("click", (event) => {
        event.preventDefault();
        event.stopPropagation();
        selectedHost = domain.host;
        selectedPath = "";
        selectedPathType = "";
        if (details.open) {
          openHosts.delete(domain.host);
          details.open = false;
          summary.classList.remove("active");
        } else {
          openHosts.add(domain.host);
          details.open = true;
          domainList.querySelectorAll("summary").forEach((el) => el.classList.remove("active"));
          summary.classList.add("active");
        }
        renderRequests(lastRequests);
      });
      const tree = buildTree(domain.paths || []);
      const list = renderTree(tree, domain.host, "");
      if (list) details.appendChild(list);
      domainList.appendChild(details);
    });
  }

  function setTargetsRespPretty() {
    if (!targetsRoot) return;
    const prettyTab = targetsRoot.querySelector(".detail-tabs .tab[data-detail='targets-resp-pretty']");
    if (prettyTab) {
      prettyTab.parentElement.querySelectorAll(".tab").forEach((t) => t.classList.remove("active"));
      prettyTab.classList.add("active");
    }
    if (targetsRespRender) {
      targetsRespRender.classList.remove("active");
    }
    if (targetsResp.parentElement) targetsResp.parentElement.style.display = "";
  }

  async function fillTargetsFromRequest(it) {
    if (!it) return;
    try {
      const detailRes = await fetch(`/api/projects/history/detail?project_id=${projectId}&id=${it.id}`);
      const detail = await detailRes.json();
      setTargetsRespPretty();
      targetsReq.dataset.raw = detail.req_raw;
      targetsReq.dataset.hex = detail.req_hex;
      targetsReq.dataset.b64 = detail.req_b64 || "";
      targetsResp.dataset.raw = detail.resp_raw;
      targetsResp.dataset.hex = detail.resp_hex;
      targetsResp.dataset.b64 = detail.resp_b64 || "";
      await renderSimpleView(targetsReq, "req");
      await renderSimpleView(targetsResp, "resp");
      refreshTargetMatches();
      if (searchFocus && searchFocus.checked) {
        findNextTargetMatch();
      }
    } catch (err) {}
  }

  function parseRequestURL(urlStr) {
    if (!urlStr) return { host: "", path: "" };
    try {
      const u = new URL(urlStr);
      return { host: u.hostname || u.host, path: u.pathname || "/" };
    } catch (err) {
      try {
        const u = new URL(`http://${urlStr}`);
        return { host: u.hostname || u.host, path: u.pathname || "/" };
      } catch (err2) {
        return { host: urlStr, path: "/" };
      }
    }
  }

  function getPathname(urlStr) {
    return parseRequestURL(urlStr).path || "";
  }

  function pathMatches(it) {
    if (!selectedHost && !selectedPath) return true;
    let hostMatch = true;
    if (selectedHost) {
      hostMatch = parseRequestURL(it.url).host === selectedHost;
    }
    if (!hostMatch) return false;
    if (!selectedPath) return true;
    const path = getPathname(it.url);
    if (selectedPathType === "file") {
      return path === selectedPath;
    }
    return path === selectedPath || path.startsWith(selectedPath + "/");
  }

  function getTargetsSortValue(it, col) {
    const fullRequest = `${it.method || ""} ${it.url || ""}`.toLowerCase();
    switch (col) {
      case "url": return fullRequest;
      case "status": return it.status ?? 0;
      case "time": return it.duration_ms ?? 0;
      case "length": return it.resp_len ?? 0;
      default: return "";
    }
  }

  function renderRequests(items) {
    targetsTable.innerHTML = "";
    let filtered = (items || []).filter(pathMatches);
    filtered = [...filtered].sort((a, b) => {
      const va = getTargetsSortValue(a, targetsSortCol);
      const vb = getTargetsSortValue(b, targetsSortCol);
      let cmp = 0;
      if (typeof va === "number" && typeof vb === "number") cmp = va - vb;
      else cmp = String(va).localeCompare(String(vb));
      return targetsSortDir === "asc" ? cmp : -cmp;
    });
    filtered.forEach((it) => {
      const row = document.createElement("tr");
      if (selectedId === it.id) row.classList.add("selected");
      const requestCell = document.createElement("td");
      const fullRequest = `${it.method} ${it.url}`;
      const shortRequest = fullRequest.length > 80 ? `${fullRequest.slice(0, 80)}…` : fullRequest;
      requestCell.textContent = shortRequest;
      requestCell.title = fullRequest;
      const statusCell = document.createElement("td");
      statusCell.textContent = String(it.status || "");
      const timeCell = document.createElement("td");
      timeCell.textContent = `${it.duration_ms} ms`;
      const lenCell = document.createElement("td");
      lenCell.textContent = String(it.resp_len || "");
      row.appendChild(requestCell);
      row.appendChild(statusCell);
      row.appendChild(timeCell);
      row.appendChild(lenCell);
      row.addEventListener("click", async () => {
        selectedId = it.id;
        targetsTable.querySelectorAll("tr").forEach((tr) => tr.classList.remove("selected"));
        row.classList.add("selected");
        await fillTargetsFromRequest(it);
      });
      targetsTable.appendChild(row);
    });
    if (selectedPathType === "file" && filtered.length > 0) {
      const first = filtered[0];
      selectedId = first.id;
      fillTargetsFromRequest(first);
    }
  }

  async function loadTargets() {
    try {
      const currentOpen = new Set();
      const currentPaths = new Set();
      domainList.querySelectorAll("details").forEach((el) => {
        if (el.open) {
          if (el.dataset.host) {
            currentOpen.add(el.dataset.host);
          }
          if (el.dataset.path) {
            currentPaths.add(el.dataset.path);
          }
        }
      });
      if (currentOpen.size > 0) {
        openHosts = currentOpen;
      }
      if (currentPaths.size > 0) {
        openPaths = currentPaths;
      }
      const res = await fetch(`/api/projects/targets?project_id=${projectId}`);
      const data = await res.json();
      lastRequests = Array.isArray(data.requests) ? data.requests : [];
      renderDomains(data.domains || []);
      renderRequests(lastRequests);
    } catch (err) {
      domainList.innerHTML = "";
      targetsTable.innerHTML = "";
    }
  }

  function collectTargetMatches(text, target) {
    const needleRaw = (searchInput && searchInput.value || "").trim();
    const needle = needleRaw.toLowerCase();
    if (!needle) return [];
    const hay = (text || "").toLowerCase();
    const matches = [];
    let start = 0;
    while (true) {
      const idx = hay.indexOf(needle, start);
      if (idx === -1) break;
      matches.push({ target, start: idx, end: idx + needle.length });
      start = idx + needle.length;
    }
    return matches;
  }

  function refreshTargetMatches() {
    const scope = searchScope ? searchScope.value : "resp";
    searchMatches = [];
    if (scope === "req" || scope === "both") {
      searchMatches = searchMatches.concat(collectTargetMatches(targetsReq ? targetsReq.value : "", "req"));
    }
    if (scope === "resp" || scope === "both") {
      searchMatches = searchMatches.concat(collectTargetMatches(targetsResp ? targetsResp.value : "", "resp"));
    }
    searchIndex = 0;
  }

  function focusTargetMatch(match) {
    if (!match) return;
    const view = match.target === "req" ? targetsReq : targetsResp;
    if (!view) return;
    focusTextareaMatch(view, match.start, match.end);
  }

  function findNextTargetMatch() {
    if (searchMatches.length === 0) {
      refreshTargetMatches();
    }
    if (searchMatches.length === 0) return;
    const match = searchMatches[searchIndex % searchMatches.length];
    searchIndex += 1;
    focusTargetMatch(match);
  }

  function findPrevTargetMatch() {
    if (searchMatches.length === 0) {
      refreshTargetMatches();
    }
    if (searchMatches.length === 0) return;
    searchIndex = (searchIndex - 1 + searchMatches.length) % searchMatches.length;
    const match = searchMatches[searchIndex];
    focusTargetMatch(match);
  }

  function updateTargetsSortHeaders() {
    const labels = { url: "Запрос", status: "Статус", time: "Время", length: "Длина" };
    targetsRoot.querySelectorAll(".targets-requests .targets-table th.sortable").forEach((h) => {
      const col = h.dataset.sort;
      h.textContent = (labels[col] || col) + (col === targetsSortCol ? (targetsSortDir === "asc" ? " ↑" : " ↓") : "");
    });
  }

  targetsRoot.querySelectorAll(".targets-requests .targets-table th.sortable").forEach((th) => {
    th.addEventListener("click", () => {
      const col = th.dataset.sort;
      if (targetsSortCol === col) {
        targetsSortDir = targetsSortDir === "asc" ? "desc" : "asc";
      } else {
        targetsSortCol = col;
        targetsSortDir = "asc";
      }
      updateTargetsSortHeaders();
      renderRequests(lastRequests);
    });
  });
  updateTargetsSortHeaders();

  loadTargets();
  setInterval(loadTargets, 5000);

  if (searchInput) {
    searchInput.addEventListener("input", refreshTargetMatches);
  }
  if (searchScope) {
    searchScope.addEventListener("change", refreshTargetMatches);
  }
  if (searchNext) {
    searchNext.addEventListener("click", findNextTargetMatch);
  }
  if (searchPrev) {
    searchPrev.addEventListener("click", findPrevTargetMatch);
  }

  const targetsReqContextMenu = document.getElementById("targets-req-context-menu");
  const targetsReqCodeView = document.getElementById("targets-req-code-view");
  if (targetsReqContextMenu && targetsReqCodeView) {
    targetsReqCodeView.addEventListener("contextmenu", (e) => {
      e.preventDefault();
      targetsReqContextMenu.style.left = e.clientX + "px";
      targetsReqContextMenu.style.top = e.clientY + "px";
      targetsReqContextMenu.classList.remove("hidden");
      requestAnimationFrame(() => {
        const rect = targetsReqContextMenu.getBoundingClientRect();
        if (rect.right > window.innerWidth) targetsReqContextMenu.style.left = (window.innerWidth - rect.width) + "px";
        if (rect.bottom > window.innerHeight) targetsReqContextMenu.style.top = (window.innerHeight - rect.height) + "px";
      });
    });
    targetsReqContextMenu.querySelectorAll(".context-menu-item").forEach((btn) => {
      btn.addEventListener("click", (e) => {
        e.stopPropagation();
        targetsReqContextMenu.classList.add("hidden");
        const reqText = targetsReq?.dataset?.raw || targetsReq?.value || "";
        if (btn.dataset.action === "repeater") {
          const repeater = document.getElementById("repeater-req");
          if (repeater && reqText) {
            if (typeof createRepeaterTabWithRequest === "function") {
              createRepeaterTabWithRequest(reqText);
            } else {
              updateCodeDisplay(repeater, reqText, true, true);
            }
          }
        } else if (btn.dataset.action === "automator") {
          const automator = document.getElementById("automator-req");
          if (automator && reqText) {
            if (typeof createAutomatorTabWithRequest === "function") {
              createAutomatorTabWithRequest(reqText);
            } else {
              updateCodeDisplay(automator, reqText, true, true);
            }
          }
        } else if (btn.dataset.action === "copy") {
          const text = targetsReq?.value || reqText;
          if (text) navigator.clipboard.writeText(text).catch(() => {});
        }
      });
    });
    document.addEventListener("click", () => targetsReqContextMenu.classList.add("hidden"));
    document.addEventListener("contextmenu", (e) => {
      if (!targetsReqContextMenu.contains(e.target) && !targetsReqCodeView.contains(e.target)) {
        targetsReqContextMenu.classList.add("hidden");
      }
    });
  }

  const targetsRespContextMenu = document.getElementById("targets-resp-context-menu");
  const targetsRespCodeView = document.getElementById("targets-resp-code-view");
  if (targetsRespContextMenu && targetsRespCodeView) {
    targetsRespCodeView.addEventListener("contextmenu", (e) => {
      e.preventDefault();
      targetsRespContextMenu.style.left = e.clientX + "px";
      targetsRespContextMenu.style.top = e.clientY + "px";
      targetsRespContextMenu.classList.remove("hidden");
      requestAnimationFrame(() => {
        const rect = targetsRespContextMenu.getBoundingClientRect();
        if (rect.right > window.innerWidth) targetsRespContextMenu.style.left = (window.innerWidth - rect.width) + "px";
        if (rect.bottom > window.innerHeight) targetsRespContextMenu.style.top = (window.innerHeight - rect.height) + "px";
      });
    });
    targetsRespContextMenu.querySelectorAll(".context-menu-item").forEach((btn) => {
      btn.addEventListener("click", (e) => {
        e.stopPropagation();
        targetsRespContextMenu.classList.add("hidden");
        if (btn.dataset.action === "copy") {
          const text = targetsResp?.value || targetsResp?.dataset?.raw || "";
          if (text) navigator.clipboard.writeText(text).catch(() => {});
        }
      });
    });
    document.addEventListener("click", () => targetsRespContextMenu.classList.add("hidden"));
    document.addEventListener("contextmenu", (e) => {
      if (!targetsRespContextMenu.contains(e.target) && !targetsRespCodeView.contains(e.target)) {
        targetsRespContextMenu.classList.add("hidden");
      }
    });
  }

  if (targetsRoot) {
    targetsRoot.querySelectorAll(".detail-tabs .tab").forEach((tab) => {
      tab.addEventListener("click", async () => {
        const detail = tab.dataset.detail || "";
        if (detail.startsWith("targets-req")) {
          await renderView(targetsReq, "req", detail.endsWith("hex") ? "hex" : detail.endsWith("pretty") ? "pretty" : "raw");
        } else if (detail.startsWith("targets-resp")) {
          if (detail.endsWith("render")) {
            const codeView = targetsResp.closest(".code-view");
            if (codeView) codeView.style.display = "none";
            if (targetsRespRender) {
              targetsRespRender.classList.add("active");
              await renderResponseFrame(targetsRespRender, targetsResp.dataset.b64 || "", getEncoding("resp"));
            }
          } else {
            if (targetsRespRender) {
              targetsRespRender.classList.remove("active");
            }
            const codeView = targetsResp.closest(".code-view");
            if (codeView) codeView.style.display = "";
            await renderView(targetsResp, "resp", detail.endsWith("hex") ? "hex" : detail.endsWith("pretty") ? "pretty" : "raw");
          }
        }
        refreshTargetMatches();
      });
    });
  }
}

function setupProjectSettings(projectId) {
  const proxyList = document.getElementById("proxy-list");
  const proxyAddBtn = document.getElementById("proxy-add");
  const routingList = document.getElementById("routing-rules-list");
  const routingAddBtn = document.getElementById("routing-rule-add");
  const proxyModal = document.getElementById("proxy-modal");
  const proxyModalTitle = document.getElementById("proxy-modal-title");
  const proxyModalName = document.getElementById("proxy-modal-name");
  const proxyModalType = document.getElementById("proxy-modal-type");
  const proxyModalHost = document.getElementById("proxy-modal-host");
  const proxyModalPort = document.getElementById("proxy-modal-port");
  const proxyModalUser = document.getElementById("proxy-modal-user");
  const proxyModalPass = document.getElementById("proxy-modal-pass");
  const proxyModalSave = document.getElementById("proxy-modal-save");
  const routingModal = document.getElementById("routing-rule-modal");
  const routingModalTitle = document.getElementById("routing-rule-modal-title");
  const routingRuleIp = document.getElementById("routing-rule-ip");
  const routingRuleCondition = document.getElementById("routing-rule-condition");
  const routingRuleListeners = document.getElementById("routing-rule-listeners");
  const routingRuleProxy = document.getElementById("routing-rule-proxy");
  const routingRuleActive = document.getElementById("routing-rule-active");
  const routingRuleSave = document.getElementById("routing-rule-modal-save");
  const exportBtn = document.getElementById("project-export");
  const importInput = document.getElementById("project-import");
  const clearBtn = document.getElementById("project-clear");
  if (!proxyList || !routingList) return;

  let editingProxyId = null;
  let editingRuleId = null;

  async function loadProxies() {
    try {
      const res = await fetch(`/api/projects/proxies?project_id=${projectId}`);
      const proxies = await res.json();
      proxyList.innerHTML = "";
      (proxies || []).forEach((p) => {
        const row = document.createElement("tr");
        const auth = p.user ? `${p.user}:***` : "—";
        row.innerHTML = `
          <td>${escapeHtml(p.name || "—")}</td>
          <td>${p.type || "http"}</td>
          <td>${escapeHtml(p.host || "")}:${p.port || 0}</td>
          <td>${auth}</td>
          <td>
            <button class="btn" data-edit-proxy="${p.id}">Изменить</button>
            <button class="btn danger" data-delete-proxy="${p.id}">Удалить</button>
          </td>
        `;
        row.querySelector(`[data-edit-proxy="${p.id}"]`).addEventListener("click", () => openProxyModal(p));
        row.querySelector(`[data-delete-proxy="${p.id}"]`).addEventListener("click", () => deleteProxy(p.id));
        proxyList.appendChild(row);
      });
    } catch (err) {
      proxyList.innerHTML = "<tr><td colspan='5'>Ошибка загрузки</td></tr>";
    }
  }

  async function loadRoutingRules() {
    try {
      const [rulesRes, proxiesRes, listenersRes] = await Promise.all([
        fetch(`/api/projects/routing-rules?project_id=${projectId}`),
        fetch(`/api/projects/proxies?project_id=${projectId}`),
        fetch(`/api/projects/listeners?project_id=${projectId}`),
      ]);
      const rules = await rulesRes.json();
      const proxies = await proxiesRes.json();
      const listeners = await listenersRes.json();
      const proxyMap = (proxies || []).reduce((m, p) => { m[p.id] = p; return m; }, {});
      const listenerPorts = (listeners || []).filter((l) => l.active).map((l) => l.port);

      routingList.innerHTML = "";
      (rules || []).forEach((r, idx) => {
        const proxyLabel = r.proxy_id ? (proxyMap[r.proxy_id]?.name || `Прокси #${r.proxy_id}`) : "Мимо прокси";
        const statusLabel = r.active ? "Активно" : "Деактивировано";
        const listenersLabel = r.listeners === "all" || !r.listeners ? "Все" : r.listeners;
        const row = document.createElement("tr");
        row.dataset.ruleId = r.id;
        row.innerHTML = `
          <td class="routing-actions">
            <button class="btn icon-btn" data-move-up="${r.id}" title="Вверх">↑</button>
            <button class="btn icon-btn" data-move-down="${r.id}" title="Вниз">↓</button>
          </td>
          <td><code>${escapeHtml(r.ip_mask_domain || "—")}</code></td>
          <td>${r.condition_and_or || "AND"}</td>
          <td>${escapeHtml(listenersLabel)}</td>
          <td>${escapeHtml(proxyLabel)}</td>
          <td>${statusLabel}</td>
          <td>
            <button class="btn" data-edit-rule="${r.id}">Изменить</button>
            <button class="btn danger" data-delete-rule="${r.id}">Удалить</button>
          </td>
        `;
        row.querySelector(`[data-move-up="${r.id}"]`).addEventListener("click", () => moveRule(r.id, "up"));
        row.querySelector(`[data-move-down="${r.id}"]`).addEventListener("click", () => moveRule(r.id, "down"));
        row.querySelector(`[data-edit-rule="${r.id}"]`).addEventListener("click", () => openRoutingModal(r, proxies, listeners));
        row.querySelector(`[data-delete-rule="${r.id}"]`).addEventListener("click", () => deleteRule(r.id));
        routingList.appendChild(row);
      });
    } catch (err) {
      routingList.innerHTML = "<tr><td colspan='7'>Ошибка загрузки</td></tr>";
    }
  }

  function escapeHtml(s) {
    if (s == null) return "";
    const div = document.createElement("div");
    div.textContent = s;
    return div.innerHTML;
  }

  function openProxyModal(proxy) {
    editingProxyId = proxy ? proxy.id : null;
    proxyModalTitle.textContent = proxy ? "Редактировать прокси" : "Добавить прокси";
    proxyModalName.value = proxy ? proxy.name || "" : "";
    proxyModalType.value = proxy ? proxy.type || "http" : "http";
    proxyModalHost.value = proxy ? proxy.host || "" : "";
    proxyModalPort.value = proxy ? proxy.port || "" : "";
    proxyModalUser.value = proxy ? proxy.user || "" : "";
    proxyModalPass.value = proxy ? proxy.pass || "" : "";
    proxyModal.classList.remove("hidden");
  }

  async function saveProxy() {
    const name = proxyModalName.value.trim() || "Прокси";
    const type = proxyModalType.value;
    const host = proxyModalHost.value.trim();
    const port = parseInt(proxyModalPort.value || "0", 10);
    const user = proxyModalUser.value.trim();
    const pass = proxyModalPass.value;
    if (!host || !port) {
      alert("Укажите хост и порт.");
      return;
    }
    if (type !== "http" && type !== "socks5") {
      alert("Укажите тип прокси (HTTP или SOCKS5).");
      return;
    }
    try {
      const payload = { name, type, host, port, user, pass };
      const url = `/api/projects/proxies?project_id=${projectId}`;
      const method = editingProxyId ? "PUT" : "POST";
      const body = editingProxyId ? JSON.stringify({ ...payload, id: editingProxyId }) : JSON.stringify(payload);
      const res = await fetch(url, { method, headers: { "Content-Type": "application/json" }, body });
      if (!res.ok) {
        const msg = await res.text();
        alert(msg || "Ошибка сохранения");
        return;
      }
      proxyModal.classList.add("hidden");
      loadProxies();
      loadRoutingRules();
    } catch (err) {
      alert("Сервер недоступен.");
    }
  }

  async function deleteProxy(id) {
    if (!confirm("Удалить прокси? Правила, использующие его, будут деактивированы.")) return;
    try {
      const res = await fetch(`/api/projects/proxies?project_id=${projectId}&proxy_id=${id}`, { method: "DELETE" });
      if (!res.ok) throw new Error(await res.text());
      loadProxies();
      loadRoutingRules();
    } catch (err) {
      alert(err.message || "Ошибка удаления");
    }
  }

  function openRoutingModal(rule, proxies, listeners) {
    editingRuleId = rule ? rule.id : null;
    routingModalTitle.textContent = rule ? "Редактировать правило" : "Добавить правило";
    routingRuleIp.value = rule ? rule.ip_mask_domain || "" : "";
    routingRuleCondition.value = rule ? rule.condition_and_or || "AND" : "AND";
    routingRuleActive.value = rule ? (rule.active ? "1" : "0") : "1";

    routingRuleProxy.innerHTML = '<option value="">Мимо прокси (напрямую)</option>';
    (proxies || []).forEach((p) => {
      const opt = document.createElement("option");
      opt.value = p.id;
      opt.textContent = p.name || `${p.host}:${p.port}`;
      if (rule && rule.proxy_id === p.id) opt.selected = true;
      routingRuleProxy.appendChild(opt);
    });
    if (rule && !rule.proxy_id) routingRuleProxy.value = "";

    routingRuleListeners.innerHTML = "";
    const activeListeners = (listeners || []).filter((l) => l.active);
    activeListeners.forEach((l) => {
      const opt = document.createElement("option");
      opt.value = l.port;
      opt.textContent = `${l.address}:${l.port}`;
      if (rule && rule.listeners && rule.listeners !== "all") {
        const ports = rule.listeners.split(",").map((x) => x.trim());
        if (ports.includes(String(l.port))) opt.selected = true;
      }
      routingRuleListeners.appendChild(opt);
    });

    routingModal.classList.remove("hidden");
  }

  async function saveRoutingRule() {
    const ipMaskDomain = routingRuleIp.value.trim();
    const conditionAndOr = routingRuleCondition.value;
    const proxyVal = routingRuleProxy.value;
    const proxyId = proxyVal ? parseInt(proxyVal, 10) : null;
    const active = routingRuleActive.value === "1";

    const selectedPorts = Array.from(routingRuleListeners.selectedOptions).map((o) => o.value);
    const listeners = selectedPorts.length === 0 ? "all" : selectedPorts.join(",");

    try {
      const payload = { ip_mask_domain: ipMaskDomain, condition_and_or: conditionAndOr, listeners, proxy_id: proxyId, active };
      const url = `/api/projects/routing-rules?project_id=${projectId}`;
      const method = editingRuleId ? "PUT" : "POST";
      const body = editingRuleId ? JSON.stringify({ ...payload, id: editingRuleId }) : JSON.stringify(payload);
      const res = await fetch(url, { method, headers: { "Content-Type": "application/json" }, body });
      if (!res.ok) {
        const msg = await res.text();
        alert(msg || "Ошибка сохранения");
        return;
      }
      routingModal.classList.add("hidden");
      loadRoutingRules();
    } catch (err) {
      alert("Сервер недоступен.");
    }
  }

  async function deleteRule(id) {
    if (!confirm("Удалить правило?")) return;
    try {
      const res = await fetch(`/api/projects/routing-rules?project_id=${projectId}&rule_id=${id}`, { method: "DELETE" });
      if (!res.ok) throw new Error(await res.text());
      loadRoutingRules();
    } catch (err) {
      alert(err.message || "Ошибка удаления");
    }
  }

  async function moveRule(id, direction) {
    try {
      const res = await fetch(`/api/projects/routing-rules?project_id=${projectId}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ rule_id: id, move: direction }),
      });
      if (!res.ok) throw new Error(await res.text());
      loadRoutingRules();
    } catch (err) {
      alert(err.message || "Ошибка перемещения");
    }
  }

  proxyAddBtn?.addEventListener("click", () => openProxyModal(null));
  proxyModalSave?.addEventListener("click", saveProxy);
  routingAddBtn?.addEventListener("click", async () => {
    const [proxiesRes, listenersRes] = await Promise.all([
      fetch(`/api/projects/proxies?project_id=${projectId}`),
      fetch(`/api/projects/listeners?project_id=${projectId}`),
    ]);
    const proxies = await proxiesRes.json();
    const listeners = await listenersRes.json();
    openRoutingModal(null, proxies, listeners);
  });
  routingRuleSave?.addEventListener("click", saveRoutingRule);

  proxyModal?.querySelectorAll("[data-close]").forEach((el) => {
    el.addEventListener("click", () => proxyModal.classList.add("hidden"));
  });
  routingModal?.querySelectorAll("[data-close]").forEach((el) => {
    el.addEventListener("click", () => routingModal.classList.add("hidden"));
  });

  loadProxies();
  loadRoutingRules();

  if (exportBtn) {
    exportBtn.addEventListener("click", () => {
      window.location.href = `/api/projects/export?project_id=${projectId}`;
    });
  }

  if (importInput) {
    importInput.addEventListener("change", async () => {
      if (!importInput.files || importInput.files.length === 0) return;
      const file = importInput.files[0];
      const form = new FormData();
      form.append("file", file);
      try {
        const res = await fetch(`/api/projects/import?project_id=${projectId}`, {
          method: "POST",
          body: form,
        });
        if (!res.ok) {
          const msg = await res.text();
          alert(msg || "Ошибка импорта");
          return;
        }
        alert("Импорт завершен.");
        window.location.reload();
      } catch (err) {
        alert("Сервер недоступен.");
      } finally {
        importInput.value = "";
      }
    });
  }

  if (clearBtn) {
    clearBtn.addEventListener("click", async () => {
      if (!confirm("Очистить данные проекта?")) return;
      try {
        const res = await fetch(`/api/projects/clear?project_id=${projectId}`, {
          method: "POST",
        });
        if (!res.ok) {
          const msg = await res.text();
          alert(msg || "Ошибка очистки");
          return;
        }
        window.location.reload();
      } catch (err) {
        alert("Сервер недоступен.");
      }
    });
  }
}

function setupModules(projectId) {
  const modulesList = document.getElementById("modules-list");
  const headersList = document.getElementById("headers-rule-list");
  const headersAdd = document.getElementById("headers-add-rule");
  const headersModal = document.getElementById("headers-rule-modal");
  const headersName = document.getElementById("headers-rule-name");
  const headersValue = document.getElementById("headers-rule-value");
  const headersAction = document.getElementById("headers-rule-action");
  const headersSave = document.getElementById("headers-rule-save");
  if (!modulesList || !headersList) return;

  function openHeadersModal() {
    if (!headersModal) return;
    headersName.value = "";
    headersValue.value = "";
    headersAction.value = "add";
    headersModal.classList.remove("hidden");
  }

  function closeHeadersModal() {
    if (!headersModal) return;
    headersModal.classList.add("hidden");
  }

  if (headersModal) {
    headersModal.querySelectorAll("[data-close]").forEach((el) => {
      el.addEventListener("click", closeHeadersModal);
    });
  }

  async function loadModules() {
    try {
      const res = await fetch(`/api/projects/modules?project_id=${projectId}`);
      const items = await res.json();
      const list = Array.isArray(items) ? items : [];
      modulesList.innerHTML = "";
      list.forEach((mod) => {
        const item = document.createElement("div");
        item.className = "list-item";
        item.innerHTML = `
          <div><strong>${mod.name}</strong></div>
          <label class="switch">
            <input type="checkbox" ${mod.enabled ? "checked" : ""} data-key="${mod.key}" />
            <span class="slider"></span>
          </label>
        `;
        const toggle = item.querySelector("input");
        toggle.addEventListener("change", async () => {
          await fetch(`/api/projects/modules/toggle?project_id=${projectId}`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ key: mod.key, enabled: toggle.checked }),
          });
        });
        modulesList.appendChild(item);
      });
    } catch (err) {
      modulesList.innerHTML = "";
    }
  }

  async function loadHeaderRules() {
    try {
      const res = await fetch(`/api/projects/modules/header-rules?project_id=${projectId}`);
      const items = await res.json();
      const list = Array.isArray(items) ? items : [];
      headersList.innerHTML = "";
      list.forEach((rule) => {
        const row = document.createElement("tr");
        row.innerHTML = `
          <td>${rule.name}</td>
          <td>${rule.value || ""}</td>
          <td>${rule.action}</td>
          <td><button class="btn danger" data-id="${rule.id}">Удалить</button></td>
        `;
        row.querySelector("button").addEventListener("click", async () => {
          await fetch(`/api/projects/modules/header-rules/delete?project_id=${projectId}`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ id: rule.id }),
          });
          loadHeaderRules();
        });
        headersList.appendChild(row);
      });
    } catch (err) {
      headersList.innerHTML = "";
    }
  }

  if (headersAdd) {
    headersAdd.addEventListener("click", openHeadersModal);
  }
  if (headersSave) {
    headersSave.addEventListener("click", async () => {
      const payload = {
        name: headersName.value.trim(),
        value: headersValue.value.trim(),
        action: headersAction.value,
      };
      if (!payload.name) {
        alert("Укажите название заголовка.");
        return;
      }
      try {
        const res = await fetch(`/api/projects/modules/header-rules?project_id=${projectId}`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload),
        });
        if (!res.ok) {
          const msg = await res.text();
          alert(msg || "Ошибка сохранения");
          return;
        }
        closeHeadersModal();
        loadHeaderRules();
      } catch (err) {
        alert("Сервер недоступен.");
      }
    });
  }

  loadModules();
  loadHeaderRules();
}

function setupProxy(projectId) {
  const proxyRoot = document.getElementById("tab-proxy");
  const listenerList = document.getElementById("listener-list");
  const historyList = document.getElementById("history-list");
  const reqView = document.getElementById("req-view");
  const respView = document.getElementById("resp-view");
  const respRender = document.getElementById("resp-render");
  const searchInput = document.getElementById("proxy-search");
  const searchScope = document.getElementById("proxy-search-scope");
  const searchNext = document.getElementById("proxy-search-next");
  const sendToRepeater = document.getElementById("send-to-repeater");
  const sendToAutomator = document.getElementById("send-to-automator");
  const sendMenuToggle = document.getElementById("send-menu-toggle");
  const sendMenu = document.getElementById("send-menu");
  const browserHelp = document.getElementById("browser-help");
  const browserHelpBox = document.getElementById("browser-help-box");
  if (!proxyRoot || !listenerList || !historyList || !reqView || !respView) return;
  let selectedRequest = "";
  let selectedResponse = "";
  let selectedHistoryId = null;

  const reqCodeView = reqView ? reqView.closest(".code-view") : null;
  const respCodeView = respView ? respView.closest(".code-view") : null;
  function syncProxyHeights(h) {
    const height = Math.max(374, h);
    if (reqCodeView) reqCodeView.style.height = height + "px";
    if (respCodeView) respCodeView.style.height = height + "px";
    requestAnimationFrame(() => {
      resizeCodeInner(reqView);
      resizeCodeInner(respView);
    });
  }
  const proxyRo = new ResizeObserver((entries) => {
    for (const e of entries) {
      const el = e.target;
      if (el === reqView || el === respView) syncProxyHeights(el.offsetHeight);
    }
  });
  proxyRo.observe(reqView);
  proxyRo.observe(respView);
  let historySortCol = "id";
  let historySortDir = "desc";
  let searchMatches = [];
  let searchIndex = 0;

  const filterStorageKey = (pid) => `abp_proxy_filter_${pid}`;
  function loadProxyFilter(pid) {
    try {
      const raw = localStorage.getItem(filterStorageKey(pid));
      if (!raw) return null;
      const data = JSON.parse(raw);
      return {
        requestContains: data.requestContains || "",
        requestExclude: !!data.requestExclude,
        method: data.method || "",
        params: data.params || "",
        status: data.status || "",
        mime: data.mime || "",
        listenerPort: data.listenerPort || "",
        proxyUsed: data.proxyUsed || "",
      };
    } catch (_) {
      return null;
    }
  }
  function saveProxyFilter(pid, filter) {
    try {
      localStorage.setItem(filterStorageKey(pid), JSON.stringify(filter));
    } catch (_) {}
  }

  let historyFilter = loadProxyFilter(projectId) || {
    requestContains: "",
    requestExclude: false,
    method: "",
    params: "",
    status: "",
    mime: "",
    listenerPort: "",
    proxyUsed: "",
  };

  const filterModal = document.getElementById("proxy-filter-modal");
  const filterBtn = document.getElementById("proxy-history-filter-btn");
  const filterRequest = document.getElementById("proxy-filter-request");
  const filterRequestExclude = document.getElementById("proxy-filter-request-exclude");
  const filterMethod = document.getElementById("proxy-filter-method");
  const filterParams = document.getElementById("proxy-filter-params");
  const filterStatus = document.getElementById("proxy-filter-status");
  const filterMime = document.getElementById("proxy-filter-mime");
  const filterListener = document.getElementById("proxy-filter-listener");
  const filterProxy = document.getElementById("proxy-filter-proxy");
  const filterApply = document.getElementById("proxy-filter-apply");

  function applyHistoryFilter(list) {
    return list.filter((it) => {
      const requestText = `${it.method || ""} ${it.url || ""}`.trim().toLowerCase();
      const reqContains = (historyFilter.requestContains || "").trim().toLowerCase();
      if (reqContains) {
        const matches = requestText.includes(reqContains);
        if (historyFilter.requestExclude && matches) return false;
        if (!historyFilter.requestExclude && !matches) return false;
      }
      if (historyFilter.method && (it.method || "") !== historyFilter.method) return false;
      if (historyFilter.params === "get" && !it.has_get) return false;
      if (historyFilter.params === "post" && !it.has_post) return false;
      if (historyFilter.params === "any" && !it.has_get && !it.has_post) return false;
      const status = it.status ?? 0;
      if (historyFilter.status === "2xx" && (status < 200 || status >= 300)) return false;
      if (historyFilter.status === "3xx" && (status < 300 || status >= 400)) return false;
      if (historyFilter.status === "4xx" && (status < 400 || status >= 500)) return false;
      if (historyFilter.status === "5xx" && (status < 500)) return false;
      const mime = (it.resp_mime || "").toLowerCase();
      const mimeFilter = (historyFilter.mime || "").trim().toLowerCase();
      if (mimeFilter && !mime.includes(mimeFilter)) return false;
      if (historyFilter.listenerPort) {
        const port = parseInt(historyFilter.listenerPort, 10);
        if ((it.listener_port || 0) !== port) return false;
      }
      if (historyFilter.proxyUsed) {
        const itemProxy = (it.proxy_used || "-").trim();
        const filterProxy = historyFilter.proxyUsed.trim();
        if (itemProxy !== filterProxy) return false;
      }
      return true;
    });
  }

  function openFilterModal() {
    if (!filterModal) return;
    filterRequest.value = historyFilter.requestContains;
    filterRequestExclude.checked = historyFilter.requestExclude;
    filterMethod.value = historyFilter.method || "";
    filterParams.value = historyFilter.params || "";
    filterStatus.value = historyFilter.status || "";
    filterMime.value = historyFilter.mime || "";
    filterListener.innerHTML = '<option value="">Все</option>';
    filterProxy.innerHTML = '<option value="">Все</option>';
    const ports = new Set();
    const proxies = new Set();
    Promise.all([
      fetch(`/api/projects/listeners?project_id=${projectId}`).then((r) => r.json()),
      fetch(`/api/projects/history?project_id=${projectId}`).then((r) => r.json()),
    ])
      .then(([listeners, historyItems]) => {
        (Array.isArray(listeners) ? listeners : []).forEach((l) => ports.add(l.port));
        (Array.isArray(historyItems) ? historyItems : []).forEach((it) => {
          if (it.listener_port) ports.add(it.listener_port);
          const p = (it.proxy_used || "-").trim();
          if (p) proxies.add(p);
        });
        [...ports].sort((a, b) => a - b).forEach((port) => {
          const opt = document.createElement("option");
          opt.value = port;
          opt.textContent = `:${port}`;
          if (String(port) === String(historyFilter.listenerPort)) opt.selected = true;
          filterListener.appendChild(opt);
        });
        [...proxies].sort().forEach((name) => {
          const opt = document.createElement("option");
          opt.value = name;
          opt.textContent = name;
          if (name === historyFilter.proxyUsed) opt.selected = true;
          filterProxy.appendChild(opt);
        });
      })
      .catch(() => {});
    filterModal.classList.remove("hidden");
  }

  function closeFilterModal() {
    if (filterModal) filterModal.classList.add("hidden");
  }

  if (filterBtn) {
    filterBtn.addEventListener("click", openFilterModal);
  }
  if (filterModal) {
    filterModal.querySelectorAll("[data-close]").forEach((el) => {
      el.addEventListener("click", closeFilterModal);
    });
  }
  const filterClear = document.getElementById("proxy-filter-clear");
  if (filterClear) {
    filterClear.addEventListener("click", () => {
      filterRequest.value = "";
      filterRequestExclude.checked = false;
      filterMethod.value = "";
      filterParams.value = "";
      filterStatus.value = "";
      filterMime.value = "";
      filterListener.value = "";
      if (filterProxy) filterProxy.value = "";
    });
  }
  if (filterApply) {
    filterApply.addEventListener("click", () => {
      historyFilter = {
        requestContains: filterRequest.value,
        requestExclude: filterRequestExclude.checked,
        method: filterMethod.value || "",
        params: filterParams.value || "",
        status: filterStatus.value || "",
        mime: filterMime.value,
        listenerPort: filterListener.value || "",
        proxyUsed: filterProxy ? filterProxy.value || "" : "",
      };
      saveProxyFilter(projectId, historyFilter);
      closeFilterModal();
      loadHistory();
    });
  }

  function collectMatches(text, target) {
    const needleRaw = (searchInput && searchInput.value || "").trim();
    const needle = needleRaw.toLowerCase();
    if (!needle) return [];
    const hay = (text || "").toLowerCase();
    const matches = [];
    let start = 0;
    while (true) {
      const idx = hay.indexOf(needle, start);
      if (idx === -1) break;
      matches.push({ target, start: idx, end: idx + needle.length });
      start = idx + needle.length;
    }
    return matches;
  }

  function refreshSearchMatches() {
    const scope = searchScope ? searchScope.value : "resp";
    searchMatches = [];
    if (scope === "req" || scope === "both") {
      searchMatches = searchMatches.concat(collectMatches(reqView ? reqView.value : "", "req"));
    }
    if (scope === "resp" || scope === "both") {
      searchMatches = searchMatches.concat(collectMatches(respView ? respView.value : "", "resp"));
    }
    searchIndex = 0;
  }

  function focusMatch(match) {
    if (!match) return;
    const view = match.target === "req" ? reqView : respView;
    if (!view) return;
    focusTextareaMatch(view, match.start, match.end);
  }

  function findNextMatch() {
    if (searchMatches.length === 0) {
      refreshSearchMatches();
    }
    if (searchMatches.length === 0) return;
    const match = searchMatches[searchIndex % searchMatches.length];
    searchIndex += 1;
    focusMatch(match);
  }

  async function loadListeners() {
    try {
      const res = await fetch(`/api/projects/listeners?project_id=${projectId}`);
      const listeners = await res.json();
      const list = Array.isArray(listeners) ? listeners : [];
      listenerList.innerHTML = "";
      list.forEach((l) => {
        const item = document.createElement("div");
        item.className = "list-item";
        item.innerHTML = `
          <div><strong>${l.address}:${l.port}</strong></div>
          <div class="muted">ID: ${l.id} ${l.active ? "активен" : "остановлен"} | MITM: ${l.mitm ? "on" : "off"}</div>
          <button class="btn danger" data-id="${l.id}">Остановить</button>
        `;
        item.querySelector("button").addEventListener("click", async () => {
          await fetch(`/api/projects/listeners?project_id=${projectId}&listener_id=${l.id}`, { method: "DELETE" });
          loadListeners();
        });
        listenerList.appendChild(item);
      });
    } catch (err) {
      listenerList.innerHTML = "";
    }
  }

  function getSortValue(it, col) {
    const mime = (it.resp_mime || "").split(";")[0] || "";
    const serverIp = it.resp_ip || it.server_addr || "";
    const params = [it.has_get ? "GET" : "", it.has_post ? "POST" : ""].filter(Boolean).join(" ");
    switch (col) {
      case "id": return it.id ?? 0;
      case "url": return `${it.method || ""} ${it.url || ""}`.trim().toLowerCase();
      case "params": return params.toLowerCase();
      case "status": return it.status ?? 0;
      case "server": return serverIp.toLowerCase();
      case "mime": return mime.toLowerCase();
      case "time": return it.duration_ms ?? 0;
      case "length": return it.resp_len ?? 0;
      case "date": return it.created_at || "";
      case "listener": return it.listener_port ?? 0;
      case "proxy": return (it.proxy_used || "").toLowerCase();
      default: return "";
    }
  }

  async function loadHistory() {
    try {
      const res = await fetch(`/api/projects/history?project_id=${projectId}`);
      const items = await res.json();
      let list = Array.isArray(items) ? items : [];
      list = applyHistoryFilter(list);
      list = [...list].sort((a, b) => {
        const va = getSortValue(a, historySortCol);
        const vb = getSortValue(b, historySortCol);
        let cmp = 0;
        if (typeof va === "number" && typeof vb === "number") cmp = va - vb;
        else cmp = String(va).localeCompare(String(vb));
        return historySortDir === "asc" ? cmp : -cmp;
      });
      historyList.innerHTML = "";
      list.forEach((it) => {
        const row = document.createElement("tr");
        const params = [
          it.has_get ? "G ✓" : null,
          it.has_post ? "P ✓" : null,
        ].filter(Boolean).join(" ");
        const mime = (it.resp_mime || "").split(";")[0] || "-";
        const serverIp = it.resp_ip || it.server_addr || "-";
        const createdAt = it.created_at ? new Date(it.created_at).toLocaleString() : "-";
        const listenerPort = it.listener_port ? `:${it.listener_port}` : "-";
        const proxyUsed = it.proxy_used || "-";
        const requestText = `${it.method || ""} ${it.url || ""}`.trim() || it.url || "-";
        const cells = [
          `${it.id}`,
          requestText,
          params || "-",
          `${it.status}`,
          `${serverIp}`,
          `${mime}`,
          `${it.duration_ms} ms`,
          `${it.resp_len}`,
          `${createdAt}`,
          `${listenerPort}`,
          proxyUsed,
        ];
        cells.forEach((value, idx) => {
          const td = document.createElement("td");
          if (idx === 1) {
            td.classList.add("col-request");
            td.textContent = value.length > 52 ? value.slice(0, 52) + "…" : value;
            td.title = value;
          } else if (idx === 5) {
            td.classList.add("col-mime");
            td.textContent = value.length > 20 ? value.slice(0, 20) + "…" : value;
            td.title = value;
          } else {
            td.textContent = value;
          }
          if (idx === 4) {
            td.classList.add("col-server");
            td.title = value;
          }
          row.appendChild(td);
        });
        if (selectedHistoryId === it.id) row.classList.add("selected");
        row.addEventListener("click", async () => {
          historyList.querySelectorAll("tr").forEach((tr) => tr.classList.remove("selected"));
          row.classList.add("selected");
          selectedHistoryId = it.id;
          const detailRes = await fetch(`/api/projects/history/detail?project_id=${projectId}&id=${it.id}`);
          const detail = await detailRes.json();
          selectedRequest = detail.req_raw;
          selectedResponse = detail.resp_raw;
          reqView.dataset.raw = detail.req_raw;
          reqView.dataset.hex = detail.req_hex;
          reqView.dataset.b64 = detail.req_b64 || "";
          respView.dataset.raw = detail.resp_raw;
          respView.dataset.hex = detail.resp_hex;
          respView.dataset.b64 = detail.resp_b64 || "";
          await renderView(reqView, "req", getActiveDetailMode("req", proxyRoot));
          await renderView(respView, "resp", getActiveDetailMode("resp", proxyRoot));
          if (respRender && respRender.classList.contains("active")) {
            await renderResponseFrame(respRender, respView.dataset.b64 || "", getEncoding("resp"));
          }
          refreshSearchMatches();
        });
        historyList.appendChild(row);
      });
    } catch (err) {
      historyList.innerHTML = "";
    }
  }

  const listenerStart = document.getElementById("listener-start");
  if (listenerStart) {
    listenerStart.addEventListener("click", async () => {
      const address = document.getElementById("listener-address").value || "0.0.0.0";
      const port = parseInt(document.getElementById("listener-port").value || "8081", 10);
      const mitm = document.getElementById("listener-mitm").value === "on";
      try {
        const res = await fetch(`/api/projects/listeners?project_id=${projectId}`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ address, port, mitm }),
        });
        if (!res.ok) {
          const message = (await res.text()) || "Не удалось создать слушатель";
          alert(`Не удалось создать слушатель: ${message}`);
          return;
        }
        loadListeners();
      } catch (err) {
        alert("Сервер недоступен. Проверьте, что AntiBurp запущен.");
      }
    });
  }

  sendToRepeater.addEventListener("click", () => {
    const repeater = document.getElementById("repeater-req");
    if (repeater && selectedRequest) {
      if (typeof createRepeaterTabWithRequest === "function") {
        createRepeaterTabWithRequest(selectedRequest);
      } else {
        updateCodeDisplay(repeater, selectedRequest || "", true, true);
      }
    }
    if (sendMenu) sendMenu.classList.remove("open");
    updateSendToggleActive();
  });

  sendToAutomator.addEventListener("click", () => {
    const automator = document.getElementById("automator-req");
    if (automator && selectedRequest) {
      if (typeof createAutomatorTabWithRequest === "function") {
        createAutomatorTabWithRequest(selectedRequest);
      } else {
        updateCodeDisplay(automator, selectedRequest || "", true, true);
      }
    }
    if (sendMenu) sendMenu.classList.remove("open");
    updateSendToggleActive();
  });

  function updateSendToggleActive() {
    if (sendMenuToggle && sendMenu) {
      if (sendMenu.classList.contains("open")) {
        sendMenuToggle.classList.add("active");
      } else {
        sendMenuToggle.classList.remove("active");
      }
    }
  }
  if (sendMenuToggle && sendMenu) {
    sendMenuToggle.addEventListener("click", (event) => {
      event.stopPropagation();
      sendMenu.classList.toggle("open");
      updateSendToggleActive();
    });
    document.addEventListener("click", (event) => {
      if (!sendMenu.contains(event.target) && event.target !== sendMenuToggle) {
        sendMenu.classList.remove("open");
        updateSendToggleActive();
      }
    });
  }

  const reqContextMenu = document.getElementById("proxy-req-context-menu");
  const proxyReqCodeView = document.getElementById("proxy-req-code-view");
  if (reqContextMenu && proxyReqCodeView) {
    proxyReqCodeView.addEventListener("contextmenu", (e) => {
      e.preventDefault();
      reqContextMenu.style.left = e.clientX + "px";
      reqContextMenu.style.top = e.clientY + "px";
      reqContextMenu.classList.remove("hidden");
      requestAnimationFrame(() => {
        const rect = reqContextMenu.getBoundingClientRect();
        if (rect.right > window.innerWidth) reqContextMenu.style.left = (window.innerWidth - rect.width) + "px";
        if (rect.bottom > window.innerHeight) reqContextMenu.style.top = (window.innerHeight - rect.height) + "px";
      });
    });
    reqContextMenu.querySelectorAll(".context-menu-item").forEach((btn) => {
      btn.addEventListener("click", (e) => {
        e.stopPropagation();
        reqContextMenu.classList.add("hidden");
        if (btn.dataset.action === "repeater") {
          const repeater = document.getElementById("repeater-req");
          if (repeater && selectedRequest) {
            if (typeof createRepeaterTabWithRequest === "function") {
              createRepeaterTabWithRequest(selectedRequest);
            } else {
              updateCodeDisplay(repeater, selectedRequest || "", true, true);
            }
          }
        } else if (btn.dataset.action === "automator") {
          const automator = document.getElementById("automator-req");
          if (automator && selectedRequest) {
            if (typeof createAutomatorTabWithRequest === "function") {
              createAutomatorTabWithRequest(selectedRequest);
            } else {
              updateCodeDisplay(automator, selectedRequest || "", true, true);
            }
          }
        } else if (btn.dataset.action === "copy") {
          const text = selectedRequest || reqView?.value || "";
          if (text) {
            navigator.clipboard.writeText(text).catch(() => {});
          }
        }
      });
    });
    document.addEventListener("click", () => reqContextMenu.classList.add("hidden"));
    document.addEventListener("contextmenu", (e) => {
      if (!reqContextMenu.contains(e.target) && !proxyReqCodeView.contains(e.target)) {
        reqContextMenu.classList.add("hidden");
      }
    });
  }

  const respContextMenu = document.getElementById("proxy-resp-context-menu");
  const proxyRespCodeView = document.getElementById("proxy-resp-code-view");
  if (respContextMenu && proxyRespCodeView) {
    proxyRespCodeView.addEventListener("contextmenu", (e) => {
      e.preventDefault();
      respContextMenu.style.left = e.clientX + "px";
      respContextMenu.style.top = e.clientY + "px";
      respContextMenu.classList.remove("hidden");
      requestAnimationFrame(() => {
        const rect = respContextMenu.getBoundingClientRect();
        if (rect.right > window.innerWidth) respContextMenu.style.left = (window.innerWidth - rect.width) + "px";
        if (rect.bottom > window.innerHeight) respContextMenu.style.top = (window.innerHeight - rect.height) + "px";
      });
    });
    respContextMenu.querySelectorAll(".context-menu-item").forEach((btn) => {
      btn.addEventListener("click", (e) => {
        e.stopPropagation();
        respContextMenu.classList.add("hidden");
        if (btn.dataset.action === "copy") {
          const text = selectedResponse || respView?.value || "";
          if (text) {
            navigator.clipboard.writeText(text).catch(() => {});
          }
        }
      });
    });
    document.addEventListener("click", () => respContextMenu.classList.add("hidden"));
    document.addEventListener("contextmenu", (e) => {
      if (!respContextMenu.contains(e.target) && !proxyRespCodeView.contains(e.target)) {
        respContextMenu.classList.add("hidden");
      }
    });
  }

  if (searchInput) {
    searchInput.addEventListener("input", refreshSearchMatches);
  }
  if (searchScope) {
    searchScope.addEventListener("change", refreshSearchMatches);
  }
  if (searchNext) {
    searchNext.addEventListener("click", findNextMatch);
  }

  if (browserHelp && browserHelpBox) {
    browserHelp.addEventListener("click", () => {
      browserHelpBox.innerText = [
        "Для HTTPS установите CA сертификат: скачайте /ca/download и добавьте в доверенные.",
        "Chrome: open -a \"Google Chrome\" --args --proxy-server=\"http=HOST:PORT;https=HOST:PORT\"",
        "Firefox: network.proxy.type=1, network.proxy.http=HOST, network.proxy.http_port=PORT",
        "Opera: open -a Opera --args --proxy-server=\"http=HOST:PORT;https=HOST:PORT\"",
        "Safari: Используйте системные настройки прокси macOS.",
      ].join("\n");
    });
  }

  proxyRoot.querySelectorAll(".detail-tabs .tab").forEach((tab) => {
    tab.addEventListener("click", async () => {
      if (tab.dataset.detail.startsWith("req")) {
        await renderView(reqView, "req", tab.dataset.detail.endsWith("hex") ? "hex" : tab.dataset.detail.endsWith("pretty") ? "pretty" : "raw");
      } else if (tab.dataset.detail.startsWith("resp")) {
        if (tab.dataset.detail.endsWith("render")) {
          const codeView = respView.closest(".code-view");
          if (codeView) codeView.style.display = "none";
          respRender.classList.add("active");
          await renderResponseFrame(respRender, respView.dataset.b64 || "", getEncoding("resp"));
        } else {
          respRender.classList.remove("active");
          const codeView = respView.closest(".code-view");
          if (codeView) codeView.style.display = "";
          await renderView(respView, "resp", tab.dataset.detail.endsWith("hex") ? "hex" : tab.dataset.detail.endsWith("pretty") ? "pretty" : "raw");
        }
      }
      refreshSearchMatches();
    });
  });

  function updateSortHeaders() {
    const labels = { id: "#", url: "Запрос", params: "Параметры", status: "Статус", server: "Сервер", mime: "MIME", time: "Время", length: "Длина", date: "Дата", listener: "Слушатель", proxy: "Прокси" };
    proxyRoot.querySelectorAll(".history-table th.sortable").forEach((h) => {
      const col = h.dataset.sort;
      h.textContent = (labels[col] || col) + (col === historySortCol ? (historySortDir === "asc" ? " ↑" : " ↓") : "");
    });
  }

  proxyRoot.querySelectorAll(".history-table th.sortable").forEach((th) => {
    th.addEventListener("click", () => {
      const col = th.dataset.sort;
      if (historySortCol === col) {
        historySortDir = historySortDir === "asc" ? "desc" : "asc";
      } else {
        historySortCol = col;
        historySortDir = "asc";
      }
      updateSortHeaders();
      loadHistory();
    });
  });
  updateSortHeaders();

  loadListeners();
  loadHistory();
  setInterval(loadHistory, 3000);
}

function setupInterceptor(projectId) {
  const toggle = document.getElementById("intercept-toggle");
  const queueEl = document.getElementById("intercept-queue");
  const editor = document.getElementById("intercept-editor");
  const sendBtn = document.getElementById("intercept-send");
  const dropBtn = document.getElementById("intercept-drop");
  if (!toggle || !queueEl || !editor || !sendBtn || !dropBtn) return;
  let currentId = "";

  toggle.addEventListener("change", async () => {
    const enabled = toggle.checked;
    if (!enabled) {
      updateCodeDisplay(editor, "", true, true);
      currentId = "";
    }
    await fetch(`/api/projects/intercept?project_id=${projectId}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled }),
    });
  });

  async function loadQueue() {
    try {
      const res = await fetch(`/api/projects/intercept?project_id=${projectId}`);
      const items = await res.json();
      const list = Array.isArray(items) ? items : [];
      queueEl.innerHTML = "";
      list.forEach((it) => {
        const item = document.createElement("div");
        item.className = "list-item";
        item.innerHTML = `<div><strong>${it.id}</strong></div><div class="muted">${it.created_at}</div>`;
        item.addEventListener("click", () => {
          currentId = it.id;
          updateCodeDisplay(editor, it.raw_req || "", true, true);
        });
        queueEl.appendChild(item);
      });
    } catch (err) {
      queueEl.innerHTML = "";
    }
  }

  sendBtn.addEventListener("click", async () => {
    if (!currentId) return;
    try {
      await fetch(`/api/projects/intercept/decision?project_id=${projectId}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: currentId, allow: true, raw_req: editor.value }),
      });
    } finally {
      currentId = "";
      updateCodeDisplay(editor, "", true, true);
      loadQueue();
    }
  });

  dropBtn.addEventListener("click", async () => {
    if (!currentId) return;
    await fetch(`/api/projects/intercept/decision?project_id=${projectId}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id: currentId, allow: false, raw_req: editor.value }),
    });
    currentId = "";
    updateCodeDisplay(editor, "", true, true);
    loadQueue();
  });

  const interceptContextMenu = document.getElementById("intercept-editor-context-menu");
  const interceptCodeView = document.getElementById("intercept-editor-code-view");
  if (interceptContextMenu && interceptCodeView) {
    interceptCodeView.addEventListener("contextmenu", (e) => {
      e.preventDefault();
      interceptContextMenu.style.left = e.clientX + "px";
      interceptContextMenu.style.top = e.clientY + "px";
      interceptContextMenu.classList.remove("hidden");
      requestAnimationFrame(() => {
        const rect = interceptContextMenu.getBoundingClientRect();
        if (rect.right > window.innerWidth) interceptContextMenu.style.left = (window.innerWidth - rect.width) + "px";
        if (rect.bottom > window.innerHeight) interceptContextMenu.style.top = (window.innerHeight - rect.height) + "px";
      });
    });
    interceptContextMenu.querySelectorAll(".context-menu-item").forEach((btn) => {
      btn.addEventListener("click", (e) => {
        e.stopPropagation();
        interceptContextMenu.classList.add("hidden");
        const reqText = editor?.value || "";
        if (btn.dataset.action === "repeater") {
          const repeater = document.getElementById("repeater-req");
          if (repeater && reqText) {
            if (typeof createRepeaterTabWithRequest === "function") {
              createRepeaterTabWithRequest(reqText);
            } else {
              updateCodeDisplay(repeater, reqText, true, true);
            }
          }
        } else if (btn.dataset.action === "automator") {
          const automator = document.getElementById("automator-req");
          if (automator && reqText) {
            if (typeof createAutomatorTabWithRequest === "function") {
              createAutomatorTabWithRequest(reqText);
            } else {
              updateCodeDisplay(automator, reqText, true, true);
            }
          }
        } else if (btn.dataset.action === "copy") {
          if (reqText) navigator.clipboard.writeText(reqText).catch(() => {});
        }
      });
    });
    document.addEventListener("click", () => interceptContextMenu.classList.add("hidden"));
    document.addEventListener("contextmenu", (e) => {
      if (!interceptContextMenu.contains(e.target) && !interceptCodeView.contains(e.target)) {
        interceptContextMenu.classList.add("hidden");
      }
    });
  }

  loadQueue();
  setInterval(loadQueue, 3000);
}

function setupRepeater(projectId) {
  const req = document.getElementById("repeater-req");
  const resp = document.getElementById("repeater-resp");
  const repeaterRoot = document.getElementById("tab-repeater");
  const repeaterRespRender = document.getElementById("repeater-resp-render");
  const sendBtn = document.getElementById("repeater-send");
  const cancelBtn = document.getElementById("repeater-cancel");
  const tabList = document.getElementById("repeater-tab-list");
  const tabAdd = document.getElementById("repeater-tab-add");
  const historyList = document.getElementById("repeater-history");
  const searchInput = document.getElementById("repeater-search");
  const searchScope = document.getElementById("repeater-search-scope");
  const searchNext = document.getElementById("repeater-search-next");
  const searchPrev = document.getElementById("repeater-search-prev");
  const searchFocus = document.getElementById("repeater-search-focus");
  const lenEl = document.getElementById("repeater-len");
  const timeEl = document.getElementById("repeater-time");
  if (!req || !resp || !sendBtn || !cancelBtn || !tabList || !tabAdd || !historyList || !lenEl || !timeEl) return;
  let currentRequestId = "";
  let isSending = false;
  let searchMatches = [];
  let searchIndex = 0;
  let lastNeedle = "";
  let lastScope = "";
  let repeaterState = { tabs: [], activeId: null };
  let draftTimer = null;
  let runsTimer = null;

  const reqCodeView = req ? req.closest(".code-view") : null;
  const respCodeView = resp ? resp.closest(".code-view") : null;
  function syncRepeaterHeights(h) {
    const height = Math.max(374, h);
    if (reqCodeView) reqCodeView.style.height = height + "px";
    if (respCodeView) respCodeView.style.height = height + "px";
    requestAnimationFrame(() => {
      resizeCodeInner(req);
      resizeCodeInner(resp);
    });
  }
  const ro = new ResizeObserver((entries) => {
    for (const e of entries) {
      const el = e.target;
      if (el === req || el === resp) syncRepeaterHeights(el.offsetHeight);
    }
  });
  ro.observe(req);
  ro.observe(resp);

  function getActiveTab(state) {
    return state.tabs.find((t) => t.id === state.activeId) || state.tabs[0];
  }

  async function renderRepeaterResp() {
    if (!repeaterRoot) return;
    const activeTab = repeaterRoot.querySelector(".detail-tabs .tab.active[data-detail^='resp']");
    const detail = activeTab?.dataset?.detail || "resp-pretty";
    if (detail.endsWith("render")) {
      const codeView = resp.closest(".code-view");
      if (codeView) codeView.style.display = "none";
      if (repeaterRespRender) {
        repeaterRespRender.classList.add("active");
        await renderResponseFrame(repeaterRespRender, resp.dataset.b64 || "", getEncoding("resp"));
      }
    } else {
      if (repeaterRespRender) repeaterRespRender.classList.remove("active");
      const codeView = resp.closest(".code-view");
      if (codeView) codeView.style.display = "";
      await renderView(resp, "resp", detail.endsWith("hex") ? "hex" : detail.endsWith("pretty") ? "pretty" : "raw");
    }
  }

  async function postRepeater(url, payload) {
    const res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!res.ok) {
      const message = (await res.text()) || `HTTP ${res.status}`;
      throw new Error(message);
    }
  }

  async function persistActiveDraft() {
    if (!repeaterState.activeId) return;
    const payload = {
      tab_id: repeaterState.activeId,
      req_raw: req.value || "",
      resp_raw: resp.dataset.raw || "",
      resp_hex: resp.dataset.hex || "",
      resp_b64: resp.dataset.b64 || "",
      resp_len: parseInt(lenEl.innerText || "0", 10),
      duration_ms: parseInt(timeEl.innerText || "0", 10),
    };
    try {
      await postRepeater(`/api/projects/repeater/tab/draft?project_id=${projectId}`, payload);
    } catch (_) {}
  }

  function renderTabs() {
    tabList.innerHTML = "";
    repeaterState.tabs.forEach((tab) => {
      const btn = document.createElement("button");
      btn.className = "tab" + (tab.id === repeaterState.activeId ? " active" : "");
      btn.innerHTML = `<span>${tab.name}</span><button class="tab-close" title="Удалить">×</button>`;
      btn.addEventListener("click", async () => {
        await persistActiveDraft();
        repeaterState.activeId = tab.id;
        const isFallbackTab = typeof tab.id === "string" && String(tab.id).startsWith("tab-");
        if (isFallbackTab) {
          renderTabs();
          updateCodeDisplay(req, "", true, true);
          updateCodeDisplay(resp, "", false, true);
          resp.dataset.raw = "";
          resp.dataset.hex = "";
          resp.dataset.b64 = "";
          lenEl.innerText = "0";
          timeEl.innerText = "0";
          renderHistory();
          return;
        }
        try {
          await postRepeater(`/api/projects/repeater/tab/activate?project_id=${projectId}`, { tab_id: tab.id });
          renderTabs();
          loadTabData(tab.id);
        } catch (err) {
          try {
            await new Promise((r) => setTimeout(r, 300));
            await postRepeater(`/api/projects/repeater/tab/activate?project_id=${projectId}`, { tab_id: tab.id });
            renderTabs();
            loadTabData(tab.id);
          } catch (retryErr) {
            alert(`Не удалось активировать вкладку: ${err.message}`);
            loadTabsFromServer();
          }
        }
      });
      btn.querySelector(".tab-close").addEventListener("click", async (event) => {
        event.stopPropagation();
        if (!confirm("Удалить вкладку и всю ее историю?")) return;
        try {
          await postRepeater(`/api/projects/repeater/tab/delete?project_id=${projectId}`, { tab_id: tab.id });
          loadTabsFromServer();
        } catch (err) {
          alert(`Не удалось удалить вкладку: ${err.message}`);
        }
      });
      btn.addEventListener("dblclick", async () => {
        const name = prompt("Название вкладки", tab.name);
        if (name) {
          try {
            await postRepeater(`/api/projects/repeater/tab/rename?project_id=${projectId}`, { tab_id: tab.id, name });
            loadTabsFromServer();
          } catch (err) {
            alert(`Не удалось переименовать вкладку: ${err.message}`);
          }
        }
      });
      tabList.appendChild(btn);
    });
  }

  function createNewTabWithRequest(rawReq) {
    fetch(`/api/projects/repeater/tabs?project_id=${projectId}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: `Tab ${repeaterState.tabs.length + 1}` }),
    }).then(() => loadTabsFromServer(rawReq));
  }

  createRepeaterTabWithRequest = createNewTabWithRequest;

  function renderHistory() {
    const active = getActiveTab(repeaterState);
    historyList.innerHTML = "";
    (active.history || []).forEach((item, index) => {
      const row = document.createElement("div");
      row.className = "list-item";
      const firstLine = (item.req_raw || "").split("\n")[0] || "Запрос";
      const timestamp = item.created_at || item.timestamp || "";
      const shortReq = firstLine.length > 120 ? firstLine.slice(0, 120) + "…" : firstLine;
      row.innerHTML = `<div class="history-request"><strong>#${active.history.length - index}</strong> ${shortReq}</div><div class="muted">${timestamp}</div>`;
        row.addEventListener("click", async () => {
          updateCodeDisplay(req, item.req_raw || "", true, true);
        resp.dataset.raw = item.resp_raw || "";
        resp.dataset.hex = item.resp_hex || "";
        resp.dataset.b64 = item.resp_b64 || "";
        await renderRepeaterResp();
        lenEl.innerText = `${item.resp_len || 0}`;
        timeEl.innerText = `${item.duration_ms || item.duration || 0}`;
        refreshSearchMatches();
      });
      historyList.appendChild(row);
    });
    refreshSearchMatches();
  }

  function addHistoryEntry(entry) {
    const active = getActiveTab(repeaterState);
    if (!active) return;
    active.history = [entry].concat(active.history || []);
    if (active.history.length > 20) {
      active.history = active.history.slice(0, 20);
    }
    renderHistory();
  }

  function setSendingState(value) {
    isSending = value;
    sendBtn.disabled = value;
    cancelBtn.disabled = !value;
  }

  function collectMatches(text, target) {
    const needle = (searchInput && searchInput.value || "").trim().toLowerCase();
    if (!needle) return [];
    const hay = (text || "").toLowerCase();
    const matches = [];
    let start = 0;
    while (true) {
      const idx = hay.indexOf(needle, start);
      if (idx === -1) break;
      matches.push({ target, start: idx, end: idx + needle.length });
      start = idx + needle.length;
    }
    return matches;
  }

  function refreshSearchMatches(resetIndex = true) {
    const scope = searchScope ? searchScope.value : "resp";
    searchMatches = [];
    if (scope === "req" || scope === "both") {
      searchMatches = searchMatches.concat(collectMatches(req ? req.value : "", "req"));
    }
    if (scope === "resp" || scope === "both") {
      searchMatches = searchMatches.concat(collectMatches(resp ? resp.value : "", "resp"));
    }
    if (resetIndex) {
      searchIndex = 0;
    }
    lastNeedle = ((searchInput && searchInput.value) || "").trim().toLowerCase();
    lastScope = scope;
  }

  function focusMatch(match) {
    if (!match) return;
    const view = match.target === "req" ? req : resp;
    if (!view) return;
    focusTextareaMatch(view, match.start, match.end);
  }

  function focusFirstMatchAfterSend() {
    refreshSearchMatches();
    if (searchMatches.length === 0) return;
    const scope = searchScope ? searchScope.value : "resp";
    let match = null;
    if (scope === "resp" || scope === "both") {
      match = searchMatches.find((m) => m.target === "resp") || null;
    }
    if (!match) {
      match = searchMatches[0];
    }
    if (!match) return;
    searchIndex = (searchMatches.indexOf(match) + 1) % searchMatches.length;
    focusMatch(match);
  }

  function findNextMatch() {
    const needle = ((searchInput && searchInput.value) || "").trim().toLowerCase();
    const scope = searchScope ? searchScope.value : "resp";
    if (needle !== lastNeedle || scope !== lastScope || searchMatches.length === 0) {
      refreshSearchMatches();
    }
    if (searchMatches.length === 0) return;
    const match = searchMatches[searchIndex % searchMatches.length];
    searchIndex += 1;
    focusMatch(match);
  }

  function findPrevMatch() {
    const needle = ((searchInput && searchInput.value) || "").trim().toLowerCase();
    const scope = searchScope ? searchScope.value : "resp";
    if (needle !== lastNeedle || scope !== lastScope || searchMatches.length === 0) {
      refreshSearchMatches();
    }
    if (searchMatches.length === 0) return;
    searchIndex = (searchIndex - 1 + searchMatches.length) % searchMatches.length;
    const match = searchMatches[searchIndex];
    focusMatch(match);
  }

  sendBtn.addEventListener("click", async () => {
    if (isSending) return;
    const raw_req = req.value;
    currentRequestId = `rep-${Date.now()}-${Math.random().toString(16).slice(2)}`;
    const tabId = repeaterState.activeId;
    const tab_id = typeof tabId === "number" && Number.isInteger(tabId) ? tabId : 0;
    setSendingState(true);
    try {
      const res = await fetch(`/api/projects/repeater/send?project_id=${projectId}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ raw_req, request_id: currentRequestId, tab_id }),
      });
      let data;
      try {
        data = await res.json();
      } catch (_) {
        const text = await res.text();
        data = { error: text || `HTTP ${res.status}` };
      }
      if (data.error) {
        updateCodeDisplay(resp, data.error || "", false, true);
        setSendingState(false);
        return;
      }
      resp.dataset.raw = data.resp_raw;
      resp.dataset.hex = data.resp_hex;
      resp.dataset.b64 = data.resp_b64 || "";
      await renderRepeaterResp();
      lenEl.innerText = `${data.resp_len || 0}`;
      timeEl.innerText = `${data.duration || 0}`;
      addHistoryEntry({
        req_raw: data.req_raw,
        resp_raw: data.resp_raw,
        resp_hex: data.resp_hex,
        resp_b64: data.resp_b64 || "",
        duration: data.duration,
        resp_len: data.resp_len,
        timestamp: data.timestamp,
      });
      persistActiveDraft();
      if (searchFocus && searchFocus.checked) {
        focusFirstMatchAfterSend();
      } else {
        refreshSearchMatches();
      }
    } catch (err) {
      updateCodeDisplay(resp, err.message || "Ошибка отправки", false, true);
    } finally {
      setSendingState(false);
    }
  });

  cancelBtn.addEventListener("click", async () => {
    if (!currentRequestId) return;
    await fetch(`/api/projects/repeater/cancel?project_id=${projectId}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ request_id: currentRequestId }),
    });
    setSendingState(false);
  });

  tabAdd.addEventListener("click", () => {
    createNewTabWithRequest("");
  });

  if (searchInput) {
    searchInput.addEventListener("input", refreshSearchMatches);
  }
  if (searchScope) {
    searchScope.addEventListener("change", refreshSearchMatches);
  }
  if (searchNext) {
    searchNext.addEventListener("click", findNextMatch);
  }
  if (searchPrev) {
    searchPrev.addEventListener("click", findPrevMatch);
  }

  if (repeaterRoot) {
    repeaterRoot.querySelectorAll(".detail-tabs .tab[data-detail^='resp']").forEach((tab) => {
      tab.addEventListener("click", async () => {
        const detail = tab.dataset.detail || "";
        if (detail.endsWith("render")) {
          const codeView = resp.closest(".code-view");
          if (codeView) codeView.style.display = "none";
          if (repeaterRespRender) {
            repeaterRespRender.classList.add("active");
            await renderResponseFrame(repeaterRespRender, resp.dataset.b64 || "", getEncoding("resp"));
          }
        } else {
          if (repeaterRespRender) repeaterRespRender.classList.remove("active");
          const codeView = resp.closest(".code-view");
          if (codeView) codeView.style.display = "";
          await renderView(resp, "resp", detail.endsWith("hex") ? "hex" : detail.endsWith("pretty") ? "pretty" : "raw");
        }
        refreshSearchMatches();
      });
    });
  }

  function loadTabsFromServer(presetReq) {
    fetch(`/api/projects/repeater/tabs?project_id=${projectId}`)
      .then((res) => res.json())
      .then((data) => {
        const tabs = Array.isArray(data.tabs) ? data.tabs : [];
        if (tabs.length === 0) {
          fetch(`/api/projects/repeater/tabs?project_id=${projectId}`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ name: "Tab 1" }),
          }).then(() => loadTabsFromServer(presetReq));
          return;
        }
        repeaterState.tabs = tabs.map((t) => ({ id: t.id, name: t.name, is_active: t.is_active, history: [] }));
        const active = repeaterState.tabs.find((t) => t.is_active) || repeaterState.tabs[0];
        repeaterState.activeId = active ? active.id : null;
        renderTabs();
        if (repeaterState.activeId) {
          loadTabData(repeaterState.activeId, presetReq);
        }
      })
      .catch(() => {
        // fallback UI + retry
        if (repeaterState.tabs.length === 0) {
          const fallbackId = `tab-${Date.now()}`;
          repeaterState.tabs = [{ id: fallbackId, name: "Tab 1", is_active: true, history: [] }];
          repeaterState.activeId = fallbackId;
          renderTabs();
        }
        setTimeout(() => loadTabsFromServer(presetReq), 1000);
      });
  }

  function loadTabData(tabId, presetReq) {
    fetch(`/api/projects/repeater/tab?project_id=${projectId}&tab_id=${tabId}`)
      .then((res) => {
        if (!res.ok) {
          loadTabsFromServer(presetReq);
          return Promise.reject(new Error("tab not found"));
        }
        return res.json();
      })
      .then(async (data) => {
        const tab = repeaterState.tabs.find((t) => t.id === tabId);
        if (!tab) return;
        tab.history = data.history || [];
        if (data.draft && data.draft.req_raw) {
          updateCodeDisplay(req, data.draft.req_raw || "", true, true);
          resp.dataset.raw = data.draft.resp_raw || "";
          resp.dataset.hex = data.draft.resp_hex || "";
          resp.dataset.b64 = data.draft.resp_b64 || "";
          await renderRepeaterResp();
          lenEl.innerText = `${data.draft.resp_len || 0}`;
          timeEl.innerText = `${data.draft.duration_ms || 0}`;
        } else if (tab.history.length > 0) {
          const latest = tab.history[0];
          updateCodeDisplay(req, latest.req_raw || "", true, true);
          resp.dataset.raw = latest.resp_raw || "";
          resp.dataset.hex = latest.resp_hex || "";
          resp.dataset.b64 = latest.resp_b64 || "";
          await renderRepeaterResp();
          lenEl.innerText = `${latest.resp_len || 0}`;
          timeEl.innerText = `${latest.duration_ms || 0}`;
        } else {
          updateCodeDisplay(req, presetReq || "", true, true);
          updateCodeDisplay(resp, "", false, true);
          resp.dataset.raw = "";
          resp.dataset.hex = "";
          resp.dataset.b64 = "";
          lenEl.innerText = "0";
          timeEl.innerText = "0";
          if (presetReq) {
            persistActiveDraft();
          }
        }
        renderHistory();
      })
      .catch(() => {
        const tab = repeaterState.tabs.find((t) => t.id === tabId);
        if (tab) {
          tab.history = [];
        }
        updateCodeDisplay(req, presetReq || "", true, true);
        updateCodeDisplay(resp, "", false, true);
        resp.dataset.raw = "";
        resp.dataset.hex = "";
        resp.dataset.b64 = "";
        lenEl.innerText = "0";
        timeEl.innerText = "0";
        renderHistory();
      });
  }

  req.addEventListener("input", () => {
    clearTimeout(draftTimer);
    draftTimer = setTimeout(persistActiveDraft, 400);
  });
  resp.addEventListener("input", () => {
    clearTimeout(draftTimer);
    draftTimer = setTimeout(persistActiveDraft, 400);
  });

  loadTabsFromServer();
}

function setupAutomator(projectId) {
  const req = document.getElementById("automator-req");
  const tabList = document.getElementById("automator-tab-list");
  const tabAdd = document.getElementById("automator-tab-add");
  const runBtn = document.getElementById("automator-run");
  const runList = document.getElementById("automator-run-list");
  const attackSel = document.getElementById("automator-attack");
  const rateInput = document.getElementById("automator-rate");
  const delayInput = document.getElementById("automator-delay");
  const jitterInput = document.getElementById("automator-jitter");
  const positionSelect = document.getElementById("automator-position-select");
  const positionEditor = document.getElementById("automator-position-editor");
  const modal = document.getElementById("automator-modal");
  const modalList = document.getElementById("automator-modal-list");
  const modalHead = document.getElementById("automator-modal-head");
  const modalReqView = document.getElementById("automator-modal-req-view");
  const modalRespView = document.getElementById("automator-modal-resp-view");
  const modalRespRender = document.getElementById("automator-modal-resp-render");
  const modalSearchInput = document.getElementById("automator-modal-search");
  const modalSearchScope = document.getElementById("automator-modal-search-scope");
  const modalSearchNext = document.getElementById("automator-modal-search-next");
  const modalSearchPrev = document.getElementById("automator-modal-search-prev");
  const modalSearchFocus = document.getElementById("automator-modal-search-focus");

  if (!req || !tabList || !tabAdd || !runBtn || !runList || !attackSel || !rateInput || !delayInput || !jitterInput || !positionSelect || !positionEditor) {
    return;
  }

  let automatorState = { tabs: [], activeId: null, positions: [], positionIndex: 0 };
  let draftTimer = null;
  let runsTimer = null;

  function defaultPositionConfig() {
    return {
      kind: "numbers",
      numbers: { start: 1, end: 10, step: 1, min_digits: 1, max_digits: 10 },
      words: [],
    };
  }

  function parseMarkers() {
    const raw = req.value || "";
    const parts = raw.split("§");
    if (parts.length % 2 === 0) return [];
    const values = [];
    for (let i = 1; i < parts.length; i += 2) {
      values.push(parts[i] || "");
    }
    return values;
  }

  function normalizePositionConfigs(values) {
    const count = values.length;
    const list = automatorState.positions;
    if (list.length < count) {
      for (let i = list.length; i < count; i += 1) {
        list.push(defaultPositionConfig());
      }
    }
    if (list.length > count) {
      list.splice(count);
    }
    if (automatorState.positionIndex >= count) {
      automatorState.positionIndex = 0;
    }
  }

  function renderPositionSelect(values) {
    positionSelect.innerHTML = "";
    if (values.length === 0) {
      positionSelect.disabled = true;
      const opt = document.createElement("option");
      opt.textContent = "Позиции не найдены";
      positionSelect.appendChild(opt);
      return;
    }
    positionSelect.disabled = false;
    values.forEach((val, idx) => {
      const opt = document.createElement("option");
      opt.value = String(idx);
      opt.textContent = `Позиция #${idx + 1} (${val || "пусто"})`;
      positionSelect.appendChild(opt);
    });
    positionSelect.value = String(automatorState.positionIndex);
  }

  function persistDraft() {
    if (!automatorState.activeId) return;
    const payload = {
      tab_id: automatorState.activeId,
      req_raw: req.value || "",
      config_json: JSON.stringify(buildConfig()),
    };
    fetch(`/api/projects/automator/tab/draft?project_id=${projectId}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    }).catch(() => {});
  }

  function buildConfig() {
    return {
      attack_type: attackSel.value,
      rate_per_sec: parseInt(rateInput.value || "10", 10),
      delay_sec: parseFloat(delayInput.value || "0"),
      jitter_sec: parseFloat(jitterInput.value || "0"),
      positions: automatorState.positions,
      placeholder: "§",
    };
  }

  function getPositionCount(cfg) {
    if (!cfg) return 0;
    if (cfg.kind === "dictionary") {
      return (cfg.words || []).length;
    }
    if (cfg.kind === "numbers" && cfg.numbers) {
      const n = cfg.numbers;
      let start = parseInt(n.start, 10) || 0;
      let end = parseInt(n.end, 10) || 0;
      let step = parseInt(n.step, 10) || 1;
      if (step === 0) step = 1;
      if (end < start) return 0;
      return Math.floor((end - start) / step) + 1;
    }
    return 0;
  }

  function renderPositionEditor(values) {
    positionEditor.innerHTML = "";
    if (values.length === 0) {
      positionEditor.innerHTML = `<div class="muted">Добавьте маркеры §значение§ в запрос, чтобы появились позиции.</div>`;
      return;
    }
    const cfg = automatorState.positions[automatorState.positionIndex] || defaultPositionConfig();
    const count = getPositionCount(cfg);
    positionEditor.innerHTML = `
      <div class="position-header">
        <span class="muted">Позиция #${automatorState.positionIndex + 1} (значение: ${values[automatorState.positionIndex] || "пусто"})</span>
        <span class="position-count" data-position-count>Запросов: <strong>${count}</strong></span>
      </div>
      <label>Тип</label>
      <select data-kind>
        <option value="numbers">Числа</option>
        <option value="dictionary">Словарь</option>
      </select>
      <div data-numbers>
        <label>От</label>
        <input type="number" data-start value="${cfg.numbers?.start ?? 1}" />
        <label>До</label>
        <input type="number" data-end value="${cfg.numbers?.end ?? 10}" />
        <label>Шаг</label>
        <input type="number" data-step value="${cfg.numbers?.step ?? 1}" />
        <label>Мин. цифр</label>
        <input type="number" data-min value="${cfg.numbers?.min_digits ?? 1}" />
        <label>Макс. цифр</label>
        <input type="number" data-max value="${cfg.numbers?.max_digits ?? 10}" />
      </div>
      <div data-words style="display:none">
        <label>Словарь (каждая строка - слово)</label>
        <div class="dict-tools">
          <button class="btn" data-dict-paste>Вставить</button>
          <button class="btn" data-dict-load>Загрузить</button>
          <button class="btn" data-dict-clear>Очистить</button>
          <button class="btn danger" data-dict-remove>Удалить строку</button>
          <div class="dict-add-row">
            <input type="text" data-dict-add-input placeholder="Вставить запись из строки" />
            <button class="btn" data-dict-add>Добавить</button>
          </div>
          <input type="file" data-dict-file accept=".txt" style="display:none" />
        </div>
        <textarea data-words-area class="code">${(cfg.words || []).join("\n")}</textarea>
      </div>
    `;
    const kindSel = positionEditor.querySelector("[data-kind]");
    const numbersBox = positionEditor.querySelector("[data-numbers]");
    const wordsBox = positionEditor.querySelector("[data-words]");
    kindSel.value = cfg.kind || "numbers";
    if (kindSel.value === "dictionary") {
      numbersBox.style.display = "none";
      wordsBox.style.display = "block";
    }
    function updateCountDisplay() {
      const el = positionEditor.querySelector("[data-position-count]");
      if (el) {
        const c = getPositionCount(cfg);
        el.innerHTML = `Запросов: <strong>${c}</strong>`;
      }
    }
    kindSel.addEventListener("change", () => {
      cfg.kind = kindSel.value;
      if (kindSel.value === "dictionary") {
        numbersBox.style.display = "none";
        wordsBox.style.display = "block";
      } else {
        numbersBox.style.display = "block";
        wordsBox.style.display = "none";
      }
      updateCountDisplay();
      scheduleDraft();
    });
    const bindNumber = (sel, key) => {
      const input = positionEditor.querySelector(sel);
      input.addEventListener("input", () => {
        cfg.numbers = cfg.numbers || {};
        cfg.numbers[key] = parseInt(input.value || "0", 10);
        updateCountDisplay();
        scheduleDraft();
      });
    };
    bindNumber("[data-start]", "start");
    bindNumber("[data-end]", "end");
    bindNumber("[data-step]", "step");
    bindNumber("[data-min]", "min_digits");
    bindNumber("[data-max]", "max_digits");
    const wordsArea = positionEditor.querySelector("[data-words-area]");
    wordsArea.addEventListener("input", () => {
      cfg.words = wordsArea.value.split("\n").map((w) => w.trim()).filter(Boolean);
      updateCountDisplay();
      scheduleDraft();
    });

    const pasteBtn = positionEditor.querySelector("[data-dict-paste]");
    const loadBtn = positionEditor.querySelector("[data-dict-load]");
    const clearBtn = positionEditor.querySelector("[data-dict-clear]");
    const removeBtn = positionEditor.querySelector("[data-dict-remove]");
    const addBtn = positionEditor.querySelector("[data-dict-add]");
    const addInput = positionEditor.querySelector("[data-dict-add-input]");
    const fileInput = positionEditor.querySelector("[data-dict-file]");

    if (pasteBtn) {
      pasteBtn.addEventListener("click", async () => {
        try {
          const text = await navigator.clipboard.readText();
          if (text) {
            wordsArea.value = wordsArea.value ? `${wordsArea.value}\n${text}` : text;
            cfg.words = wordsArea.value.split("\n").map((w) => w.trim()).filter(Boolean);
            updateCountDisplay();
            scheduleDraft();
          }
        } catch (err) {
          alert("Не удалось прочитать буфер обмена");
        }
      });
    }
    if (loadBtn && fileInput) {
      loadBtn.addEventListener("click", () => fileInput.click());
      fileInput.addEventListener("change", () => {
        const file = fileInput.files && fileInput.files[0];
        if (!file) return;
        const reader = new FileReader();
        reader.onload = () => {
          const text = reader.result ? String(reader.result) : "";
          wordsArea.value = wordsArea.value ? `${wordsArea.value}\n${text}` : text;
          cfg.words = wordsArea.value.split("\n").map((w) => w.trim()).filter(Boolean);
          updateCountDisplay();
          scheduleDraft();
          fileInput.value = "";
        };
        reader.readAsText(file);
      });
    }
    if (clearBtn) {
      clearBtn.addEventListener("click", () => {
        wordsArea.value = "";
        cfg.words = [];
        updateCountDisplay();
        scheduleDraft();
      });
    }
    if (addBtn && addInput) {
      addBtn.addEventListener("click", () => {
        const value = (addInput.value || "").trim();
        if (!value) return;
        wordsArea.value = wordsArea.value ? `${wordsArea.value}\n${value}` : value;
        cfg.words = wordsArea.value.split("\n").map((w) => w.trim()).filter(Boolean);
        addInput.value = "";
        updateCountDisplay();
        scheduleDraft();
      });
    }
    if (removeBtn) {
      removeBtn.addEventListener("click", () => {
        const text = wordsArea.value || "";
        if (!text) return;
        const lines = text.split("\n");
        const caret = wordsArea.selectionStart || 0;
        let pos = 0;
        let lineIndex = 0;
        for (let i = 0; i < lines.length; i += 1) {
          const nextPos = pos + lines[i].length;
          if (caret <= nextPos) {
            lineIndex = i;
            break;
          }
          pos = nextPos + 1;
        }
        lines.splice(lineIndex, 1);
        wordsArea.value = lines.join("\n");
        cfg.words = wordsArea.value.split("\n").map((w) => w.trim()).filter(Boolean);
        updateCountDisplay();
        scheduleDraft();
      });
    }
  }

  function scheduleDraft() {
    clearTimeout(draftTimer);
    draftTimer = setTimeout(persistDraft, 400);
  }

  function updatePositionsFromRequest() {
    const values = parseMarkers();
    normalizePositionConfigs(values);
    renderPositionSelect(values);
    renderPositionEditor(values);
  }

  function renderTabs() {
    tabList.innerHTML = "";
    automatorState.tabs.forEach((tab) => {
      const btn = document.createElement("button");
      btn.className = "tab" + (tab.id === automatorState.activeId ? " active" : "");
      btn.innerHTML = `<span>${tab.name}</span><button class="tab-close" title="Удалить">×</button>`;
      btn.addEventListener("click", async () => {
        scheduleDraft();
        automatorState.activeId = tab.id;
        await fetch(`/api/projects/automator/tab/activate?project_id=${projectId}`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ tab_id: tab.id }),
        }).then(() => {
          renderTabs();
          loadTabData(tab.id);
        });
      });
      btn.querySelector(".tab-close").addEventListener("click", async (event) => {
        event.stopPropagation();
        if (!confirm("Удалить вкладку и всю ее историю?")) return;
        await fetch(`/api/projects/automator/tab/delete?project_id=${projectId}`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ tab_id: tab.id }),
        });
        loadTabsFromServer();
      });
      btn.addEventListener("dblclick", async () => {
        const name = prompt("Название вкладки", tab.name);
        if (name) {
          await fetch(`/api/projects/automator/tab/rename?project_id=${projectId}`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ tab_id: tab.id, name }),
          });
          loadTabsFromServer();
        }
      });
      tabList.appendChild(btn);
    });
  }

  function createNewTabWithRequest(rawReq) {
    fetch(`/api/projects/automator/tabs?project_id=${projectId}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: `Tab ${automatorState.tabs.length + 1}` }),
    }).then(() => loadTabsFromServer(rawReq));
  }

  createAutomatorTabWithRequest = createNewTabWithRequest;

  function loadTabsFromServer(presetReq) {
    fetch(`/api/projects/automator/tabs?project_id=${projectId}`)
      .then((res) => res.json())
      .then((data) => {
        const tabs = Array.isArray(data.tabs) ? data.tabs : [];
        if (tabs.length === 0) {
          fetch(`/api/projects/automator/tabs?project_id=${projectId}`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ name: "Tab 1" }),
          }).then(() => loadTabsFromServer(presetReq));
          return;
        }
        automatorState.tabs = tabs.map((t) => ({ id: t.id, name: t.name, is_active: t.is_active }));
        const active = automatorState.tabs.find((t) => t.is_active) || automatorState.tabs[0];
        automatorState.activeId = active ? active.id : null;
        renderTabs();
        if (automatorState.activeId) {
          loadTabData(automatorState.activeId, presetReq);
        }
      });
  }

  function applyConfig(config) {
    if (!config) return;
    attackSel.value = config.attack_type || "sniper";
    rateInput.value = config.rate_per_sec || 10;
    delayInput.value = config.delay_sec || 0;
    jitterInput.value = config.jitter_sec || 0;
    automatorState.positions = Array.isArray(config.positions) ? config.positions : [];
  }

  function loadTabData(tabId, presetReq) {
    fetch(`/api/projects/automator/tab?project_id=${projectId}&tab_id=${tabId}`)
      .then((res) => {
        if (!res.ok) {
          loadTabsFromServer(presetReq);
          return Promise.reject(new Error("tab not found"));
        }
        return res.json();
      })
      .then((data) => {
        const draft = data.draft || {};
        updateCodeDisplay(req, draft.req_raw || presetReq || "", true, true);
        if (draft.config_json) {
          try {
            applyConfig(JSON.parse(draft.config_json));
          } catch (err) {
            automatorState.positions = [];
          }
        } else {
          automatorState.positions = [];
        }
        updatePositionsFromRequest();
        if ((presetReq && !draft.req_raw) || (!draft.req_raw && req.value)) {
          scheduleDraft();
        }
        loadRuns(tabId);
        startRunsAutoRefresh(tabId);
      })
      .catch((err) => {
        if (err && err.message === "tab not found") return;
        updateCodeDisplay(req, presetReq || "", true, true);
        automatorState.positions = [];
        updatePositionsFromRequest();
        if (presetReq) {
          scheduleDraft();
        }
        loadRuns(tabId);
        startRunsAutoRefresh(tabId);
      });
  }

  function loadRuns(tabId) {
    fetch(`/api/projects/automator/runs?project_id=${projectId}&tab_id=${tabId}`)
      .then((res) => res.json())
      .then((data) => {
        const runs = Array.isArray(data.runs) ? data.runs : [];
        runList.innerHTML = "";
        runs.forEach((run) => {
          const total = Number(run.total || 0);
          const completed = Number(run.completed || 0);
          const percent = total > 0 ? Math.min(100, Math.round((completed / total) * 100)) : 0;
          const statusText = (() => {
            switch ((run.status || "").toLowerCase()) {
              case "running":
                return "Выполняется";
              case "finished":
                return "Завершено";
              case "failed":
                return "Ошибка";
              case "stopped":
                return "Остановлено";
              default:
                return run.status || "-";
            }
          })();
          const item = document.createElement("div");
          item.className = "list-item";
          item.innerHTML = `
            <div class="run-header">
              <div><strong>#${run.id}</strong> ${statusText}</div>
              <div class="run-actions">
                ${run.status === "running" ? `<button class="btn danger" data-stop>Остановить</button>` : `<button class="btn" data-delete>Удалить</button>`}
              </div>
            </div>
            <div class="run-progress">
              <div class="progress">
                <div class="progress-bar" style="width:${percent}%"></div>
              </div>
              <div class="muted">${percent}% · ${completed}/${total}</div>
              <div class="muted">${run.created_at || ""}</div>
            </div>
          `;
          const stopBtn = item.querySelector("[data-stop]");
          if (stopBtn) {
            stopBtn.addEventListener("click", (event) => {
              event.stopPropagation();
              fetch(`/api/projects/automator/stop?project_id=${projectId}`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ run_id: run.id }),
              }).then(() => loadRuns(tabId));
            });
          }
          const deleteBtn = item.querySelector("[data-delete]");
          if (deleteBtn) {
            deleteBtn.addEventListener("click", (event) => {
              event.stopPropagation();
              if (!confirm("Удалить завершенную задачу?")) return;
              fetch(`/api/projects/automator/delete?project_id=${projectId}`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ run_id: run.id }),
              }).then(() => loadRuns(tabId));
            });
          }
          item.addEventListener("click", () => openRunModal(run.id));
          runList.appendChild(item);
        });
      })
      .catch(() => {
        runList.innerHTML = "";
      });
  }

  function startRunsAutoRefresh(tabId) {
    if (runsTimer) {
      clearInterval(runsTimer);
    }
    runsTimer = setInterval(() => loadRuns(tabId), 2000);
  }

  let modalTimer = null;
  let modalSelectedId = null;
  let modalSortCol = "index";
  let modalSortDir = "asc";
  let modalRenderRunStatus = null;

  let modalSearchMatches = [];
  let modalSearchIndex = 0;

  function collectModalMatches(text, target) {
    const needle = (modalSearchInput && modalSearchInput.value || "").trim().toLowerCase();
    if (!needle) return [];
    const hay = (text || "").toLowerCase();
    const matches = [];
    let start = 0;
    while (true) {
      const idx = hay.indexOf(needle, start);
      if (idx === -1) break;
      matches.push({ target, start: idx, end: idx + needle.length });
      start = idx + needle.length;
    }
    return matches;
  }

  function refreshModalMatches() {
    const scope = modalSearchScope ? modalSearchScope.value : "resp";
    modalSearchMatches = [];
    if (scope === "req" || scope === "both") {
      modalSearchMatches = modalSearchMatches.concat(collectModalMatches(modalReqView ? modalReqView.value : "", "req"));
    }
    if (scope === "resp" || scope === "both") {
      modalSearchMatches = modalSearchMatches.concat(collectModalMatches(modalRespView ? modalRespView.value : "", "resp"));
    }
    modalSearchIndex = 0;
  }

  function focusModalMatch(match) {
    if (!match) return;
    const view = match.target === "req" ? modalReqView : modalRespView;
    if (!view) return;
    focusTextareaMatch(view, match.start, match.end);
  }

  function findNextModalMatch() {
    if (modalSearchMatches.length === 0) {
      refreshModalMatches();
    }
    if (modalSearchMatches.length === 0) return;
    const match = modalSearchMatches[modalSearchIndex % modalSearchMatches.length];
    modalSearchIndex += 1;
    focusModalMatch(match);
  }

  function findPrevModalMatch() {
    if (modalSearchMatches.length === 0) {
      refreshModalMatches();
    }
    if (modalSearchMatches.length === 0) return;
    modalSearchIndex = (modalSearchIndex - 1 + modalSearchMatches.length) % modalSearchMatches.length;
    const match = modalSearchMatches[modalSearchIndex];
    focusModalMatch(match);
  }

  function openRunModal(runId) {
    if (!modal || !modalList || !modalReqView || !modalRespView) return;
    modal.classList.remove("hidden");
    updateCodeDisplay(modalReqView, "", true, false);
    updateCodeDisplay(modalRespView, "", false, false);
    modalSortCol = "index";
    modalSortDir = "asc";
    if (modalRespRender) {
      modalRespRender.classList.remove("active");
      const codeView = modalRespView.closest(".code-view");
      if (codeView) codeView.style.display = "";
    }

    const renderRunStatus = () => {
      fetch(`/api/projects/automator/status?project_id=${projectId}&run_id=${runId}`)
        .then((res) => res.json())
        .then((data) => {
          const items = Array.isArray(data.items) ? data.items : [];
          const positionsCount = Number(data.positions_count || 0);
          const positionsNames = Array.isArray(data.positions_names) && data.positions_names.length === positionsCount
            ? data.positions_names
            : null;
          const sortKeys = ["index", ...Array.from({ length: positionsCount }, (_, i) => `payload_${i}`), "status", "time", "length"];
          const labels = ["#", ...Array.from({ length: positionsCount }, (_, i) => {
            const name = positionsNames?.[i];
            return (name !== undefined && name !== "" ? name : `Нагрузка ${i + 1}`);
          }), "Код", "Время", "Длина"];
          if (modalHead) {
            modalHead.innerHTML = labels.map((label, i) => {
              const key = sortKeys[i];
              const arrow = key === modalSortCol ? (modalSortDir === "asc" ? " ↑" : " ↓") : "";
              return `<th class="sortable" data-sort="${key}">${label}${arrow}</th>`;
            }).join("");
          }
          const getModalSortValue = (it, col) => {
            const values = Array.isArray(it.values) ? it.values : [];
            if (col === "index") return it.index ?? 0;
            if (col.startsWith("payload_")) {
              const idx = parseInt(col.replace("payload_", ""), 10);
              return (values[idx] !== undefined && values[idx] !== "" ? values[idx] : "-").toLowerCase();
            }
            if (col === "status") return it.status_code ?? 0;
            if (col === "time") return it.duration_ms ?? 0;
            if (col === "length") return it.resp_len ?? 0;
            return "";
          };
          const validSortCol = sortKeys.includes(modalSortCol) ? modalSortCol : "index";
          let sorted = [...items].sort((a, b) => {
            const va = getModalSortValue(a, validSortCol);
            const vb = getModalSortValue(b, validSortCol);
            let cmp = 0;
            if (typeof va === "number" && typeof vb === "number") cmp = va - vb;
            else cmp = String(va).localeCompare(String(vb));
            return modalSortDir === "asc" ? cmp : -cmp;
          });
          modalList.innerHTML = "";
          sorted.forEach((it) => {
            const row = document.createElement("tr");
            if (modalSelectedId === it.id) {
              row.classList.add("selected");
            }
            const values = Array.isArray(it.values) ? it.values : [];
            const payloadTds = Array.from({ length: positionsCount }, (_, i) => {
              const v = values[i] !== undefined && values[i] !== "" ? values[i] : "-";
              return `<td>${v}</td>`;
            }).join("");
            row.innerHTML = `
              <td>${it.index}</td>
              ${payloadTds}
              <td>${it.status_code || "-"}</td>
              <td>${it.duration_ms} ms</td>
              <td>${it.resp_len}</td>
            `;
            row.addEventListener("click", () => {
              modalList.querySelectorAll("tr").forEach((tr) => tr.classList.remove("selected"));
              row.classList.add("selected");
              modalSelectedId = it.id;
              fetch(`/api/projects/automator/request?project_id=${projectId}&id=${it.id}`)
                .then((res) => res.json())
                .then((detail) => {
                  modalReqView.dataset.raw = detail.req_raw || "";
                  modalReqView.dataset.hex = detail.req_hex || "";
                  modalReqView.dataset.b64 = detail.req_b64 || "";
                  modalRespView.dataset.raw = detail.resp_raw || "";
                  modalRespView.dataset.hex = detail.resp_hex || "";
                  modalRespView.dataset.b64 = detail.resp_b64 || "";
                  const reqScope = modal?.querySelector(".modal-request-row > div:first-child");
                  const respScope = modal?.querySelector(".modal-request-row > div:last-child");
                  renderView(modalReqView, "req", getActiveDetailMode("req", reqScope || modal));
                  renderView(modalRespView, "resp", getActiveDetailMode("resp", respScope || modal));
                  if (modalRespRender && modalRespRender.classList.contains("active")) {
                    renderResponseFrame(modalRespRender, modalRespView.dataset.b64 || "", getEncoding("resp"));
                  }
                  refreshModalMatches();
                  if (modalSearchFocus && modalSearchFocus.checked) {
                    findNextModalMatch();
                  }
                });
            });
            modalList.appendChild(row);
          });
        });
    };

    modalRenderRunStatus = renderRunStatus;
    renderRunStatus();
    if (modalTimer) {
      clearInterval(modalTimer);
    }
    modalTimer = setInterval(renderRunStatus, 2000);
  }

  function closeModal() {
    if (modal) {
      modal.classList.add("hidden");
    }
    if (modalTimer) {
      clearInterval(modalTimer);
      modalTimer = null;
    }
    modalSelectedId = null;
  }

  if (modal) {
    modal.querySelectorAll("[data-close]").forEach((el) => {
      el.addEventListener("click", closeModal);
    });
    modal.addEventListener("click", (e) => {
      const th = e.target.closest("th.sortable");
      if (!th || !modalRenderRunStatus) return;
      const col = th.dataset.sort;
      if (modalSortCol === col) {
        modalSortDir = modalSortDir === "asc" ? "desc" : "asc";
      } else {
        modalSortCol = col;
        modalSortDir = "asc";
      }
      modalRenderRunStatus();
    });
    modal.querySelectorAll(".detail-tabs .tab").forEach((tab) => {
      tab.addEventListener("click", async () => {
        if (tab.dataset.detail.startsWith("req")) {
          await renderView(modalReqView, "req", tab.dataset.detail.endsWith("hex") ? "hex" : tab.dataset.detail.endsWith("pretty") ? "pretty" : "raw");
        } else if (tab.dataset.detail.startsWith("resp")) {
          if (tab.dataset.detail.endsWith("render")) {
            if (modalRespRender) {
              const codeView = modalRespView.closest(".code-view");
              if (codeView) codeView.style.display = "none";
              modalRespRender.classList.add("active");
              await renderResponseFrame(modalRespRender, modalRespView.dataset.b64 || "", getEncoding("resp"));
            }
          } else {
            if (modalRespRender) {
              modalRespRender.classList.remove("active");
            }
            const codeView = modalRespView.closest(".code-view");
            if (codeView) codeView.style.display = "";
            await renderView(modalRespView, "resp", tab.dataset.detail.endsWith("hex") ? "hex" : tab.dataset.detail.endsWith("pretty") ? "pretty" : "raw");
          }
        }
        refreshModalMatches();
      });
    });
  }

  if (modalSearchInput) {
    modalSearchInput.addEventListener("input", refreshModalMatches);
  }
  if (modalSearchScope) {
    modalSearchScope.addEventListener("change", refreshModalMatches);
  }
  if (modalSearchNext) {
    modalSearchNext.addEventListener("click", findNextModalMatch);
  }
  if (modalSearchPrev) {
    modalSearchPrev.addEventListener("click", findPrevModalMatch);
  }

  runBtn.addEventListener("click", async () => {
    if (!automatorState.activeId) return;
    const payload = {
      raw_request: req.value,
      attack_type: attackSel.value,
      rate_per_sec: parseInt(rateInput.value || "10", 10),
      delay_sec: parseFloat(delayInput.value || "0"),
      jitter_sec: parseFloat(jitterInput.value || "0"),
      positions: automatorState.positions,
      placeholder: "§",
      tab_id: automatorState.activeId,
    };
    const res = await fetch(`/api/projects/automator/run?project_id=${projectId}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!res.ok) {
      const msg = await res.text();
      alert(msg || "Не удалось запустить автоматизацию");
      return;
    }
    scheduleDraft();
    loadRuns(automatorState.activeId);
  });

  tabAdd.addEventListener("click", () => {
    createNewTabWithRequest("");
  });

  positionSelect.addEventListener("change", () => {
    automatorState.positionIndex = parseInt(positionSelect.value || "0", 10);
    updatePositionsFromRequest();
  });

  req.addEventListener("input", () => {
    updatePositionsFromRequest();
    scheduleDraft();
  });
  [attackSel, rateInput, delayInput, jitterInput].forEach((el) => {
    el.addEventListener("input", scheduleDraft);
  });

  const automatorReqContextMenu = document.getElementById("automator-req-context-menu");
  const automatorReqCodeView = document.getElementById("automator-req-code-view");
  let savedContextMenuSelection = { start: 0, end: 0 };
  if (automatorReqContextMenu && automatorReqCodeView) {
    automatorReqCodeView.addEventListener("contextmenu", (e) => {
      e.preventDefault();
      savedContextMenuSelection = { start: req.selectionStart, end: req.selectionEnd };
      automatorReqContextMenu.style.left = e.clientX + "px";
      automatorReqContextMenu.style.top = e.clientY + "px";
      automatorReqContextMenu.classList.remove("hidden");
      requestAnimationFrame(() => {
        const rect = automatorReqContextMenu.getBoundingClientRect();
        if (rect.right > window.innerWidth) automatorReqContextMenu.style.left = (window.innerWidth - rect.width) + "px";
        if (rect.bottom > window.innerHeight) automatorReqContextMenu.style.top = (window.innerHeight - rect.height) + "px";
      });
    });
    automatorReqContextMenu.querySelectorAll(".context-menu-item").forEach((btn) => {
      btn.addEventListener("click", (e) => {
        e.stopPropagation();
        automatorReqContextMenu.classList.add("hidden");
        req.focus();
        const start = savedContextMenuSelection.start;
        const end = savedContextMenuSelection.end;
        const text = req.value || "";
        if (btn.dataset.action === "add-position") {
          let newText;
          if (start !== end && end > start) {
            const selected = text.slice(start, end);
            newText = text.slice(0, start) + "§" + selected + "§" + text.slice(end);
          } else {
            newText = text.slice(0, start) + "§§" + text.slice(start);
          }
          req.value = newText;
          updateCodeDisplay(req, newText, true, true);
          updatePositionsFromRequest();
          scheduleDraft();
        } else if (btn.dataset.action === "clear-positions") {
          const cleared = text.replace(/§/g, "");
          req.value = cleared;
          updateCodeDisplay(req, cleared, true, true);
          updatePositionsFromRequest();
          scheduleDraft();
        }
      });
    });
    document.addEventListener("click", () => automatorReqContextMenu.classList.add("hidden"));
    document.addEventListener("contextmenu", (e) => {
      if (!automatorReqContextMenu.contains(e.target) && !automatorReqCodeView.contains(e.target)) {
        automatorReqContextMenu.classList.add("hidden");
      }
    });
  }

  loadTabsFromServer();
}
