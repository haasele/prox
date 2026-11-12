"use strict";

const TABS = ["applications", "rules", "activity", "settings"];
const TAB_ALIASES = { policy: "settings", enforce: "settings" };
const COLUMNS = { applications: 7, unmanaged: 5, rules: 8, leases: 5 };
const REFRESH_KINDS = ["rewrite", "route", "collision", "lease", "deny", "reconcile", "rule", "policy", "hint", "port", "discover"];

const state = {
  status: null,
  routes: [],
  rules: [],
  leases: [],
  containers: null,
  search: "",
  loaded: { routes: false, rules: false, leases: false, containers: false },
};

let editingRule = 0;
let pendingRoute = "";
let refreshQueued = false;
let policySaving = false;
let enforcing = false;
let bootAttempt = 0;
let bootTimer = null;
let bootNotified = false;
let historyLoading = false;
let eventSource = null;
let lastDockerError = "";

const conn = { api: "unknown", sse: "connecting" };
let lastConnection = "starting";

const deferredRenders = new Map();

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) {
    throw new Error(data && data.error ? data.error : res.status + " " + res.statusText);
  }
  return data;
}

/* ---------- notices and announcements ---------- */

const noticeQueue = [];
let activeNotice = null;
let noticeTimer = null;

function notify(message, tone = "info") {
  const text = String(message || "").trim();
  if (!text) return;
  if (activeNotice && activeNotice.text === text) return;
  if (noticeQueue.some((item) => item.text === text)) return;
  noticeQueue.push({ text, tone });
  while (noticeQueue.length > 4) noticeQueue.shift();
  if (!activeNotice) showNextNotice();
}

function showNextNotice() {
  const box = $("#notice");
  if (!box) return;
  const next = noticeQueue.shift();
  if (!next) return;
  activeNotice = next;
  box.hidden = false;
  box.classList.remove("hidden");
  box.classList.add("show");
  box.classList.toggle("warn", next.tone === "warn");
  box.classList.toggle("error", next.tone === "error");
  box.textContent = next.text;
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => {
    box.classList.remove("show");
    box.classList.add("hidden");
    box.hidden = true;
    box.textContent = "";
    activeNotice = null;
    if (noticeQueue.length) setTimeout(showNextNotice, 180);
  }, next.tone === "error" ? 8000 : next.tone === "warn" ? 6000 : 4000);
}

let lastAnnouncement = "";

function announce(message) {
  if (!message || message === lastAnnouncement) return;
  lastAnnouncement = message;
  const box = $("#announcer");
  if (!box) return;
  box.textContent = "";
  requestAnimationFrame(() => { box.textContent = message; });
}

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined) node.textContent = text;
  return node;
}

function emptyRow(tbody, columns, text, action) {
  const tr = el("tr");
  const td = el("td", "empty");
  td.colSpan = columns;
  td.appendChild(document.createTextNode(text));
  if (action) {
    const btn = action.icon
      ? labeledButton(action.icon, action.label, action.label, action.onClick)
      : el("button", "action", action.label);
    if (!action.icon) {
      btn.type = "button";
      btn.addEventListener("click", action.onClick);
    }
    td.appendChild(document.createTextNode(" "));
    td.appendChild(btn);
  }
  tr.appendChild(td);
  tbody.appendChild(tr);
}

function errorRow(tbody, columns, text, retry) {
  const tr = el("tr");
  const td = el("td", "empty");
  td.colSpan = columns;
  td.appendChild(document.createTextNode(text + " "));
  const btn = el("button", "action", "Retry");
  btn.type = "button";
  btn.addEventListener("click", retry);
  td.appendChild(btn);
  tr.appendChild(td);
  tbody.appendChild(tr);
}

function focusInDataRow() {
  const active = document.activeElement;
  if (!active || !active.closest) return false;
  const row = active.closest("tbody tr");
  return Boolean(row && !row.querySelector("td.empty"));
}

function renderTable(name, render) {
  if (focusInDataRow()) {
    deferredRenders.set(name, render);
    return;
  }
  deferredRenders.delete(name);
  render();
}

function flushDeferredRenders() {
  if (!deferredRenders.size || focusInDataRow()) return;
  const renders = Array.from(deferredRenders.values());
  deferredRenders.clear();
  for (const render of renders) render();
}

document.addEventListener("focusout", () => {
  setTimeout(flushDeferredRenders, 0);
});

function focusSoon(selector) {
  requestAnimationFrame(() => {
    const target = typeof selector === "string" ? $(selector) : selector;
    if (target && typeof target.focus === "function") target.focus({ preventScroll: true });
  });
}

function fmtTime(sec) {
  if (!sec) return "";
  return new Date(sec * 1000).toLocaleTimeString();
}

function httpsURL(host) {
  const port = state.status ? state.status.https_port : 443;
  return "https://" + host + (port && port !== 443 ? ":" + port : "");
}

function tcpAddress(route) {
  return route.host + (route.gateway_port ? ":" + route.gateway_port : "");
}

function routeURL(route) {
  return route.proto === "tcp" ? "" : httpsURL(route.host);
}

function routeStatus(route) {
  const raw = String(route.status || "").toLowerCase();
  const cls = raw === "up" || raw === "down" ? raw : "pending";
  return el("span", "state " + cls, raw || "pending");
}

function appName(route) {
  return route.service || route.container_name || (route.host || "").split(".")[0] || "app";
}

function containerName(container) {
  if (container.name) return container.name;
  return container.id ? container.id.slice(0, 12) : "container";
}

const SVG_NS = "http://www.w3.org/2000/svg";

function icon(name) {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("class", "icon");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  const use = document.createElementNS(SVG_NS, "use");
  use.setAttribute("href", "#i-" + name);
  svg.appendChild(use);
  return svg;
}

function iconButton(name, label, onClick) {
  const btn = el("button", "action icon-action");
  btn.type = "button";
  btn.setAttribute("aria-label", label);
  btn.title = label;
  btn.appendChild(icon(name));
  btn.addEventListener("click", onClick);
  return btn;
}

function labeledButton(name, label, accessibleLabel, onClick) {
  const btn = el("button", "action");
  btn.type = "button";
  btn.setAttribute("aria-label", accessibleLabel);
  btn.title = accessibleLabel;
  btn.appendChild(icon(name));
  btn.appendChild(document.createTextNode(label));
  btn.addEventListener("click", onClick);
  return btn;
}

async function copyAddress(text) {
  let copied = false;
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
      copied = true;
    }
  } catch { copied = false; }
  if (!copied) {
    try {
      const field = document.createElement("textarea");
      field.value = text;
      field.setAttribute("readonly", "");
      field.style.position = "fixed";
      field.style.opacity = "0";
      document.body.appendChild(field);
      field.select();
      copied = document.execCommand("copy");
      field.remove();
    } catch { copied = false; }
  }
  if (copied) notify("Copied " + text + ".");
  else notify("Could not copy " + text + ".", "error");
}

/* ---------- dialogs ---------- */

function openDialogElement(dialog) {
  if (!dialog || dialog.open) return;
  if (typeof dialog.showModal === "function") dialog.showModal();
  else dialog.setAttribute("open", "");
}

function closeDialogElement(dialog) {
  if (!dialog || !dialog.open) return;
  if (typeof dialog.close === "function") dialog.close();
  else dialog.removeAttribute("open");
}

function clickedOutside(dialog, event) {
  const rect = dialog.getBoundingClientRect();
  return event.clientX < rect.left || event.clientX > rect.right ||
    event.clientY < rect.top || event.clientY > rect.bottom;
}

function confirmAction(opts = {}) {
  const dialog = $("#confirm-dialog");
  if (!dialog) return Promise.resolve(false);
  const title = $("#confirm-title", dialog);
  const message = $("#confirm-message", dialog);
  const ok = $("#confirm-ok", dialog);
  const cancel = $("#confirm-cancel", dialog);
  if (title) title.textContent = opts.title || "Are you sure?";
  if (message) message.textContent = opts.message || "";
  const okLabel = $("#confirm-ok-label", dialog);
  if (okLabel) okLabel.textContent = opts.confirmLabel || "Delete";

  return new Promise((resolve) => {
    let done = false;
    const cleanup = () => {
      ok.removeEventListener("click", onOk);
      cancel.removeEventListener("click", onCancel);
      dialog.removeEventListener("cancel", onCancelEvent);
      dialog.removeEventListener("click", onBackdrop);
    };
    const finish = (value) => {
      if (done) return;
      done = true;
      cleanup();
      closeDialogElement(dialog);
      resolve(value);
    };
    const onOk = () => finish(true);
    const onCancel = () => finish(false);
    const onCancelEvent = (event) => { event.preventDefault(); finish(false); };
    const onBackdrop = (event) => {
      if (event.target === dialog && clickedOutside(dialog, event)) finish(false);
    };
    ok.addEventListener("click", onOk);
    cancel.addEventListener("click", onCancel);
    dialog.addEventListener("cancel", onCancelEvent);
    dialog.addEventListener("click", onBackdrop);
    openDialogElement(dialog);
    cancel.focus();
  });
}

/* ---------- tabs ---------- */

function hashName() {
  return location.hash.replace(/^#/, "");
}

function tabFromHash() {
  const name = hashName();
  return TABS.includes(name) ? name : TAB_ALIASES[name] || null;
}

function activateTab(name, updateURL = false, scroll = false) {
  if (!TABS.includes(name)) name = "applications";
  for (const tab of $$(".tab[data-tab]")) {
    if (tab.dataset.tab === name) tab.setAttribute("aria-current", "page");
    else tab.removeAttribute("aria-current");
  }
  let active = null;
  for (const panel of $$("[data-panel]")) {
    const match = panel.dataset.panel === name;
    panel.hidden = !match;
    if (match) active = panel;
  }
  if (scroll && active) active.scrollIntoView({ block: "start" });
  if (updateURL) {
    try { history.replaceState(null, "", "#" + name); } catch { location.hash = name; }
  }
}

function initTabs() {
  const initial = tabFromHash();
  const raw = hashName();
  const direct = TABS.includes(raw);
  activateTab(initial || "applications", !initial, Boolean(initial));
  if (initial && !direct) focusSoon("#" + raw);
  const nav = $("nav.tabs");
  if (nav) {
    nav.addEventListener("click", (e) => {
      const tab = e.target.closest("a.tab[data-tab]");
      if (!tab) return;
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
      e.preventDefault();
      activateTab(tab.dataset.tab, true, true);
    });
  }
  window.addEventListener("hashchange", () => {
    const name = tabFromHash();
    if (!name) return;
    const nextRaw = hashName();
    activateTab(name, TABS.includes(nextRaw), true);
    if (!TABS.includes(nextRaw)) focusSoon("#" + nextRaw);
  });
}

/* ---------- rule dialog ---------- */

function ruleForm() {
  return $("#rule-form");
}

function openRuleDialog() {
  const dialog = $("#rule-dialog");
  if (!dialog) return;
  openDialogElement(dialog);
  const form = ruleForm();
  if (form) form.elements.name.focus();
}

function closeRuleDialog() {
  closeDialogElement($("#rule-dialog"));
}

function syncRuleFormState() {
  const form = ruleForm();
  if (!form) return;
  const proto = form.elements.proto.value;
  const action = form.elements.action.value;
  form.elements.target.placeholder = proto === "http" ? "443" : "source port";
  const redirectRow = $("#redirect-row");
  const eligible = proto === "http" && action === "allow";
  if (redirectRow) redirectRow.classList.toggle("hidden", !eligible);
  form.elements.redirect.disabled = !eligible;
}

function setRuleError(message, field) {
  const form = ruleForm();
  if (form) {
    for (const node of form.querySelectorAll("[aria-invalid]")) node.removeAttribute("aria-invalid");
  }
  const box = $("#rule-error");
  if (box) box.textContent = message || "";
  if (!message) return;
  if (field) {
    field.setAttribute("aria-invalid", "true");
    field.focus();
  } else if (box) {
    box.focus();
  }
}

function setFormMode(ruleID, context) {
  editingRule = ruleID || 0;
  const title = $("#rule-title");
  if (title) title.textContent = editingRule ? "Edit rule" : "New rule";
  const form = ruleForm();
  const submit = form && form.querySelector("button[type=submit]");
  if (submit) submit.textContent = editingRule ? "Save rule" : "Create rule";
  const ctx = $("#rule-context");
  if (ctx) ctx.textContent = context || "";
  setRuleError("");
  openRuleDialog();
}

function setRouteNow(targetAddr) {
  pendingRoute = targetAddr || "";
  const row = $("#route-now-row");
  if (row) row.classList.toggle("hidden", !pendingRoute);
  const form = ruleForm();
  if (form) {
    form.elements.route_now.checked = !!pendingRoute;
    form.elements.route_now.disabled = !pendingRoute;
  }
}

function fillRuleForm(values, ruleID, context) {
  const form = ruleForm();
  if (!form) return;
  const f = form.elements;
  f.name.value = values.name || "";
  f.project.value = values.project || "";
  f.proto.value = values.proto === "tcp" ? "tcp" : "http";
  f.source.value = values.source || "";
  f.target.value = values.target || "";
  f.resolve.value = values.resolve || "";
  f.action.value = values.action === "publish" || values.action === "deny" ? values.action : "allow";
  f.redirect.checked = values.redirect !== false;
  syncRuleFormState();
  setFormMode(ruleID, context);
}

function editRule(rule) {
  setRouteNow("");
  fillRuleForm({
    name: rule.name,
    project: rule.project,
    proto: rule.proto,
    source: rule.source,
    target: rule.target,
    resolve: rule.resolve,
    action: rule.action,
    redirect: rule.redirect,
  }, rule.id, "Editing rule " + rule.id + ". Changes apply on the next matching create.");
}

function prefillFromRoute(route) {
  const existing = state.rules.find((r) => r.id === route.rule_id);
  if (existing) {
    editRule(existing);
    return;
  }
  setRouteNow("");
  fillRuleForm({
    name: appName(route),
    project: route.project || "",
    proto: route.proto === "tcp" || route.proto === "sni" ? "tcp" : "http",
    source: route.source_port || "",
    target: route.proto === "tcp" ? (route.gateway_port || "") : "",
    resolve: route.host,
    action: "allow",
    redirect: route.redirect,
  }, 0, "Prefilled from " + appName(route) + ". Adjust any field before creating.");
}

function prefillFromSuggestion(suggestion, targetAddr) {
  const value = suggestion || {};
  setRouteNow(value.proto === "http" ? targetAddr : "");
  fillRuleForm({
    name: value.name,
    project: value.project || "",
    proto: value.proto,
    source: value.source || "",
    target: "",
    resolve: value.resolve || "",
    action: "allow",
    redirect: true,
  }, 0, "Prefilled from " + (value.name || "container") + ". Adjust any field before creating.");
}

function unmanagedTarget(name) {
  if (!name || !state.containers) return "";
  const items = state.containers.unmanaged || [];
  const hit = items.find((c) => c.name === name || c.service === name);
  return hit && hit.target_addr ? hit.target_addr : "";
}

function resetRuleForm() {
  editingRule = 0;
  pendingRoute = "";
  const form = ruleForm();
  if (form) {
    form.reset();
    form.elements.redirect.checked = true;
    setRouteNow("");
    syncRuleFormState();
  }
  const title = $("#rule-title");
  if (title) title.textContent = "New rule";
  const submit = form && form.querySelector("button[type=submit]");
  if (submit) submit.textContent = "Create rule";
  const context = $("#rule-context");
  if (context) context.textContent = "";
  setRuleError("");
}

function openNewRule() {
  resetRuleForm();
  openRuleDialog();
}

async function submitRule(e) {
  e.preventDefault();
  const form = ruleForm();
  if (!form) return;
  setRuleError("");
  const data = new FormData(form);
  const name = String(data.get("name") || "").trim();
  const rawSource = String(data.get("source") || "").trim();
  const rawTarget = String(data.get("target") || "").trim();
  const proto = String(data.get("proto") || "http");
  const action = String(data.get("action") || "allow");
  const body = {
    name,
    project: String(data.get("project") || "").trim(),
    proto,
    source: Number(rawSource || 0) || 0,
    target: Number(rawTarget || 0) || 0,
    resolve: String(data.get("resolve") || "").trim(),
    action,
  };
  if (proto === "http" && action === "allow") {
    body.redirect = data.get("redirect") === "on";
  }
  if (!body.name) {
    setRuleError("Enter a name to match the service, image, or profile.", form.elements.name);
    return;
  }
  for (const [label, raw, field] of [
    ["Source port", rawSource, form.elements.source],
    ["Gateway port", rawTarget, form.elements.target],
  ]) {
    if (raw === "") continue;
    const value = Number(raw);
    if (!Number.isInteger(value) || value < 1 || value > 65535) {
      setRuleError(label + " must be a whole number between 1 and 65535.", field);
      return;
    }
  }
  if (body.action === "allow" && body.proto === "http" && body.target && body.target !== 443) {
    setRuleError("HTTP rules terminate on 443. Choose TCP for another gateway port.", form.elements.target);
    return;
  }
  const editing = editingRule;
  const button = form.querySelector("button[type=submit]");
  button.disabled = true;
  button.textContent = editing ? "Saving…" : "Creating…";
  try {
    const rule = await api(editing ? "/v1/rules/" + editing : "/v1/rules", {
      method: editing ? "PUT" : "POST",
      body: JSON.stringify(body),
    });
    let message = editing ? "Rule " + editing + " saved." : "Rule " + rule.id + " created.";
    let routeFailed = false;
    if (!editing && pendingRoute && data.get("route_now") === "on" && body.action === "allow" && body.proto === "http") {
      const host = body.resolve || (body.name + ".localhost");
      try {
        const route = await api("/v1/routes", {
          method: "POST",
          body: JSON.stringify({
            host,
            proto: "http",
            source_port: body.source,
            target_addr: pendingRoute,
            name: body.name,
            redirect: body.redirect,
          }),
        });
        message += " Route " + route.id + " added for " + host + ".";
      } catch (err) {
        message += " Route failed: " + err.message;
        routeFailed = true;
      }
    }
    notify(message, routeFailed ? "warn" : "info");
    resetRuleForm();
    closeRuleDialog();
    const refreshed = await refreshAll();
    if (!refreshed) notify("Saved, but some data could not refresh.", "warn");
    focusSoon("#new-rule");
  } catch (err) {
    setRuleError(err.message);
  } finally {
    button.disabled = false;
    button.textContent = editing ? "Save rule" : "Create rule";
  }
}

function initRuleDialog() {
  const form = ruleForm();
  if (form) {
    form.elements.proto.addEventListener("change", syncRuleFormState);
    form.elements.action.addEventListener("change", syncRuleFormState);
    form.addEventListener("submit", submitRule);
    form.addEventListener("input", (e) => {
      if (e.target.matches("input, select")) e.target.removeAttribute("aria-invalid");
    });
    syncRuleFormState();
  }
  const close = $("#rule-close");
  if (close) close.addEventListener("click", closeRuleDialog);
  const cancel = $("#rule-cancel");
  if (cancel) cancel.addEventListener("click", closeRuleDialog);
  const create = $("#new-rule");
  if (create) create.addEventListener("click", openNewRule);
  const dialog = $("#rule-dialog");
  if (dialog) {
    dialog.addEventListener("cancel", (e) => {
      e.preventDefault();
      closeRuleDialog();
    });
    dialog.addEventListener("close", resetRuleForm);
    dialog.addEventListener("click", (e) => {
      if (e.target === dialog && clickedOutside(dialog, e)) closeRuleDialog();
    });
  }
}

/* ---------- applications ---------- */

function routeMatches(route, query) {
  if (!query) return true;
  const hay = [
    appName(route),
    route.host,
    route.project,
    route.service,
    route.container_name,
    route.target_addr,
  ].filter(Boolean).join(" ").toLowerCase();
  return hay.includes(query);
}

function clearSearch() {
  const input = $("#app-search");
  if (input) input.value = "";
  state.search = "";
  renderApplications();
  if (input) input.focus();
}

function renderApplications() {
  const tbody = $("#applications tbody");
  if (!tbody) return;
  tbody.replaceChildren();
  const query = state.search.trim().toLowerCase();
  const routes = state.routes.filter((r) => routeMatches(r, query));
  if (!routes.length) {
    if (query) {
      emptyRow(tbody, COLUMNS.applications, "No applications match the search.", {
        label: "Clear search",
        onClick: clearSearch,
      });
    } else {
      emptyRow(tbody, COLUMNS.applications, "No applications yet.", {
        label: "New rule",
        icon: "plus",
        onClick: openNewRule,
      });
    }
    return;
  }
  for (const r of routes) {
    const tr = el("tr");

    const appTd = el("td");
    const app = el("div", "app", appName(r));
    app.title = appName(r);
    appTd.appendChild(app);
    const sub = [
      r.service && r.container_name && r.service !== r.container_name ? r.container_name : "",
      r.project || "",
    ].filter(Boolean).join(" / ");
    if (sub) {
      const subLine = el("div", "sub", sub);
      subLine.title = sub;
      appTd.appendChild(subLine);
    }
    tr.appendChild(appTd);

    const dnsTd = el("td");
    if (r.proto === "tcp") {
      const addr = tcpAddress(r);
      const text = el("span", "mono addr", addr);
      text.title = "tcp://" + addr;
      text.setAttribute("translate", "no");
      dnsTd.appendChild(text);
    } else {
      const url = routeURL(r);
      const link = el("a", "host");
      const hostText = el("span", "host-text", r.host);
      link.href = url;
      link.target = "_blank";
      link.rel = "noreferrer";
      link.title = url;
      link.setAttribute("translate", "no");
      link.appendChild(hostText);
      link.appendChild(icon("external"));
      dnsTd.appendChild(link);
    }
    if (r.collision) dnsTd.appendChild(el("div", "sub", "renamed"));
    tr.appendChild(dnsTd);

    const sourceTd = el("td", "port", r.source_port ? String(r.source_port) : "auto");
    sourceTd.setAttribute("translate", "no");
    tr.appendChild(sourceTd);

    const gateway = r.gateway_port ? String(r.gateway_port) + " · " + (r.proto || "http") : "—";
    const targetTd = el("td", "port ellipsis", gateway);
    targetTd.title = r.gateway_port ? "Gateway port " + r.gateway_port + " (" + (r.proto || "http") + ")" : "";
    tr.appendChild(targetTd);

    const statusTd = el("td", "ellipsis");
    statusTd.appendChild(routeStatus(r));
    tr.appendChild(statusTd);

    const upstreamTd = el("td", "mono ellipsis", r.target_addr || "—");
    upstreamTd.title = r.target_addr || "";
    upstreamTd.setAttribute("translate", "no");
    tr.appendChild(upstreamTd);

    const actions = el("div", "actions");
    actions.appendChild(iconButton("pencil", "Edit rule for " + appName(r), () => prefillFromRoute(r)));
    if (r.proto === "tcp") {
      const addr = tcpAddress(r);
      actions.appendChild(iconButton("copy", "Copy address for " + appName(r), () => copyAddress(addr)));
    }
    if ((r.container_id || "").startsWith("manual:")) {
      actions.appendChild(iconButton("trash", "Delete manual route " + r.host, () => removeManualRoute(r)));
    }
    const actionTd = el("td");
    actionTd.appendChild(actions);
    tr.appendChild(actionTd);

    tbody.appendChild(tr);
  }
}

async function removeManualRoute(route) {
  const ok = await confirmAction({
    title: "Remove route?",
    message: "Remove " + route.host + " from prox? The running listener keeps listening.",
    confirmLabel: "Remove route",
  });
  if (!ok) return;
  try {
    await api("/v1/routes/" + route.id, { method: "DELETE" });
    notify("Route removed.");
    await refreshAll();
    focusSoon(".tab[aria-current='page']");
  } catch (err) {
    notify(err.message, "error");
  }
}

function renderReservedPorts() {
  const out = $("#reserved");
  if (!out) return;
  const s = state.status || {};
  const http = s.own_http_ports || [];
  const tcp = s.own_tcp_ports || [];
  const parts = [];
  if (http.length) parts.push("HTTP " + http.join(", "));
  if (tcp.length) parts.push("TCP " + tcp.join(", "));
  if (!parts.length) {
    out.textContent = "";
    out.hidden = true;
    return;
  }
  out.hidden = false;
  out.textContent = "Reserved ports: " + parts.join(" / ");
}

/* ---------- rules ---------- */

function actionLabel(action) {
  if (action === "publish") return "Publish";
  if (action === "deny") return "Deny";
  if (action === "route-tcp") return "TCP";
  if (action === "route-http") return "HTTP";
  return "Allow";
}

async function deleteRule(rule) {
  const ok = await confirmAction({
    title: "Delete rule?",
    message: "Rule " + rule.id + " (" + rule.name + ") will stop applying on the next matching create.",
    confirmLabel: "Delete",
  });
  if (!ok) return;
  try {
    await api("/v1/rules/" + rule.id, { method: "DELETE" });
    if (editingRule === rule.id) {
      closeRuleDialog();
      resetRuleForm();
    }
    notify("Rule " + rule.id + " deleted.");
    await refreshAll();
    focusSoon("#new-rule");
  } catch (err) {
    notify(err.message, "error");
  }
}

function renderRules() {
  const tbody = $("#rules tbody");
  if (!tbody) return;
  tbody.replaceChildren();
  if (!state.rules.length) {
    emptyRow(tbody, COLUMNS.rules, "No rules yet.", { label: "New rule", icon: "plus", onClick: openNewRule });
    return;
  }
  for (const r of state.rules) {
    const tr = el("tr");
    tr.appendChild(el("td", "port", String(r.id)));
    const nameTd = el("td", "ellipsis", r.name);
    nameTd.title = r.name;
    tr.appendChild(nameTd);
    const projectTd = el("td", "ellipsis", r.project || "—");
    projectTd.title = r.project || "";
    tr.appendChild(projectTd);
    tr.appendChild(el("td", "", actionLabel(r.action)));
    tr.appendChild(el("td", "port", r.source ? String(r.source) : "auto"));
    tr.appendChild(el("td", "port", r.target ? String(r.target) : "—"));
    const resolveTd = el("td", "ellipsis", r.resolve || "—");
    resolveTd.title = r.resolve || "";
    resolveTd.setAttribute("translate", "no");
    tr.appendChild(resolveTd);
    const td = el("td");
    const actions = el("div", "actions");
    actions.appendChild(iconButton("pencil", "Edit rule " + r.id + " (" + r.name + ")", () => editRule(r)));
    actions.appendChild(iconButton("trash", "Delete rule " + r.id + " (" + r.name + ")", () => deleteRule(r)));
    td.appendChild(actions);
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

/* ---------- leases ---------- */

function renderLeases() {
  const tbody = $("#leases tbody");
  if (!tbody) return;
  tbody.replaceChildren();
  if (!state.leases.length) {
    emptyRow(tbody, COLUMNS.leases, "No leases.");
    return;
  }
  for (const l of state.leases) {
    const tr = el("tr");
    tr.appendChild(el("td", "port", String(l.id)));
    const hostTd = el("td", "ellipsis", l.host);
    hostTd.title = l.host;
    tr.appendChild(hostTd);
    tr.appendChild(el("td", "port", String(l.pid)));
    tr.appendChild(el("td", "port", String(l.port)));
    const commandTd = el("td", "mono ellipsis", l.command || "—");
    commandTd.title = l.command || "";
    tr.appendChild(commandTd);
    tbody.appendChild(tr);
  }
}

/* ---------- warnings and unmanaged containers ---------- */

function renderWarnings(warnings) {
  const box = $("#warnings");
  const list = $("#warning-list");
  if (!box || !list) return;
  list.replaceChildren();
  if (!warnings || !warnings.length) {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  for (const w of warnings) list.appendChild(el("li", "", w));
}

function renderUnmanaged() {
  const tbody = $("#unmanaged tbody");
  if (!tbody) return;
  tbody.replaceChildren();
  const items = state.containers ? state.containers.unmanaged || [] : [];
  if (!items.length) {
    emptyRow(tbody, COLUMNS.unmanaged, "No unmanaged containers.");
    return;
  }
  for (const c of items) {
    const name = containerName(c);
    const tr = el("tr");
    const nameTd = el("td", "ellipsis", name);
    nameTd.title = name;
    tr.appendChild(nameTd);
    const imageTd = el("td", "mono ellipsis", c.image || "—");
    imageTd.title = c.image || "";
    imageTd.setAttribute("translate", "no");
    tr.appendChild(imageTd);
    const projectTd = el("td", "ellipsis", c.project || "—");
    projectTd.title = c.project || "";
    tr.appendChild(projectTd);
    const ports = (c.ports || []).join(", ");
    const portsTd = el("td", "mono ellipsis", ports || "—");
    portsTd.title = ports;
    portsTd.setAttribute("translate", "no");
    tr.appendChild(portsTd);
    const td = el("td");
    td.appendChild(labeledButton("plus", "New rule", "New rule for " + name, () => prefillFromSuggestion(c.suggestion, c.target_addr || "")));
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

/* ---------- events ---------- */

function eventAllowButton(ev) {
  if (!["deny", "warn", "collision"].includes(ev.kind)) return null;
  const d = ev.data || {};
  if (!d.container && !d.service) return null;
  const name = d.service || d.container;
  const btn = labeledButton("plus", "New rule", "New rule for " + name, async () => {
    try {
      const q = new URLSearchParams({
        name,
        project: d.project || "",
        image: d.image || "",
        port: (d.ports && d.ports.length) ? String(d.ports[0]) : "",
      });
      const suggestion = await api("/v1/suggest?" + q.toString());
      const target = unmanagedTarget(d.service) || unmanagedTarget(d.container);
      prefillFromSuggestion(suggestion, target);
    } catch (err) { notify(err.message, "error"); }
  });
  return btn;
}

const renderedEventIds = new Set();

function appendEvent(ev, prepend = true) {
  const list = $("#events");
  if (!list) return;
  if (ev.id !== undefined && ev.id !== null) {
    if (renderedEventIds.has(ev.id)) return;
    renderedEventIds.add(ev.id);
  }
  const empty = list.querySelector(".empty-event");
  if (empty) empty.remove();
  const li = el("li");
  const time = el("time", "", fmtTime(ev.ts));
  if (ev.ts) time.dateTime = new Date(ev.ts * 1000).toISOString();
  li.appendChild(time);
  li.appendChild(el("span", "kind" + (ev.level ? " lvl-" + ev.level : ""), ev.kind || "event"));
  li.appendChild(el("span", "message", ev.message || ""));
  const rule = eventAllowButton(ev);
  if (rule) li.appendChild(rule);
  if (prepend) list.prepend(li); else list.appendChild(li);
  while (list.children.length > 200) list.lastElementChild.remove();
}

function eventID(ev) {
  const id = Number(ev && ev.id);
  return Number.isFinite(id) ? id : null;
}

function eventTime(ev) {
  const ts = Number(ev && ev.ts);
  return Number.isFinite(ts) ? ts : 0;
}

function renderEventHistory(history) {
  const list = $("#events");
  if (!list) return;
  renderedEventIds.clear();
  list.replaceChildren();
  if (!Array.isArray(history) || !history.length) {
    list.appendChild(el("li", "empty-event", "No events yet."));
    return;
  }
  // The API returns ascending id/ts order; display newest first and match live prepends.
  const ordered = history.slice().sort((a, b) => {
    const aID = eventID(a);
    const bID = eventID(b);
    if (aID !== null && bID !== null && aID !== bID) return bID - aID;
    return eventTime(b) - eventTime(a);
  });
  for (const ev of ordered) appendEvent(ev, false);
}

const bufferedEvents = [];

async function loadEventHistory() {
  if (historyLoading) return;
  historyLoading = true;
  try {
    const history = await api("/v1/events?limit=80");
    renderEventHistory(history);
    for (const ev of bufferedEvents.splice(0)) appendEvent(ev);
  } catch {
    for (const ev of bufferedEvents.splice(0)) appendEvent(ev);
  } finally {
    historyLoading = false;
  }
}

/* ---------- status, polling, and refresh ---------- */

function connectionLabel() {
  if (conn.api === "fail") return "offline";
  if (conn.sse === "error") return "reconnecting";
  if (conn.api === "ok" && conn.sse === "open") return "connected";
  return "starting";
}

function updateConnection(part, value) {
  if (conn[part] === value) {
    renderStatus();
    return;
  }
  conn[part] = value;
  const label = connectionLabel();
  if (label !== lastConnection) {
    lastConnection = label;
    if (label === "connected") announce("Connected to prox.");
    else if (label === "reconnecting") announce("Reconnecting to prox.");
    else if (label === "offline") announce("prox API unreachable.");
  }
  renderStatus();
}

function renderStatus() {
  const status = $("#status");
  if (!status) return;
  const s = state.status;
  const label = connectionLabel();
  if (label !== "connected") {
    const text = label === "offline" ? "offline" : label === "reconnecting" ? "reconnecting…" : "connecting…";
    const cls = label === "reconnecting" ? "status warn" : label === "offline" ? "status bad" : "status";
    if (status.textContent !== text) status.textContent = text;
    if (status.className !== cls) status.className = cls;
    status.removeAttribute("title");
  } else if (!s) {
    if (status.textContent !== "connecting…") status.textContent = "connecting…";
    if (status.className !== "status") status.className = "status";
    status.removeAttribute("title");
  } else {
    const dockerError = s.docker_ok ? "" : (s.docker_error || "");
    if (dockerError && dockerError !== lastDockerError) {
      lastDockerError = dockerError;
      notify("Docker unreachable: " + dockerError, "error");
    } else if (!dockerError) {
      lastDockerError = "";
    }
    const text = s.routes === 1 ? "1 route" : (s.routes || 0) + " routes";
    if (status.textContent !== text) status.textContent = text;
    if (status.className !== "status ok") status.className = "status ok";
    status.removeAttribute("title");
  }
  if (s) {
    const policy = $("#policy");
    if (policy && !policySaving && document.activeElement !== policy && s.policy && policy.value !== s.policy) {
      policy.value = s.policy;
    }
    const enforce = $("#enforce");
    if (enforce && document.activeElement !== enforce && enforce.checked !== !!s.enforcement) {
      enforce.checked = !!s.enforcement;
    }
    renderReservedPorts();
  }
}

async function refreshStatus() {
  try {
    state.status = await api("/v1/status");
    updateConnection("api", "ok");
    return true;
  } catch {
    updateConnection("api", "fail");
    return false;
  }
}

function renderLoadingRows() {
  const labels = {
    applications: "Loading applications…",
    unmanaged: "Loading containers…",
    rules: "Loading rules…",
    leases: "Loading leases…",
  };
  for (const [id, text] of Object.entries(labels)) {
    const tbody = $("#" + id + " tbody");
    if (!tbody) continue;
    tbody.setAttribute("aria-busy", "true");
    tbody.replaceChildren();
    emptyRow(tbody, COLUMNS[id], text);
  }
}

function showTableError(id, text, retry) {
  const tbody = $("#" + id + " tbody");
  if (!tbody) return;
  renderTable(id, () => {
    tbody.removeAttribute("aria-busy");
    tbody.replaceChildren();
    errorRow(tbody, COLUMNS[id], text, retry);
  });
}

async function retryTable(refresh) {
  const ok = await refresh();
  if (ok) focusSoon(".tab[aria-current='page']");
}

async function refreshRoutes() {
  try {
    state.routes = await api("/v1/routes");
    state.loaded.routes = true;
    renderTable("applications", () => {
      const tbody = $("#applications tbody");
      if (tbody) tbody.removeAttribute("aria-busy");
      renderApplications();
    });
    return true;
  } catch {
    if (!state.loaded.routes) {
      showTableError("applications", "Applications could not load.", () => retryTable(refreshRoutes));
    }
    return false;
  }
}

async function refreshRules() {
  try {
    state.rules = await api("/v1/rules");
    state.loaded.rules = true;
    renderTable("rules", () => {
      const tbody = $("#rules tbody");
      if (tbody) tbody.removeAttribute("aria-busy");
      renderRules();
    });
    return true;
  } catch {
    if (!state.loaded.rules) {
      showTableError("rules", "Rules could not load.", () => retryTable(refreshRules));
    }
    return false;
  }
}

async function refreshLeases() {
  try {
    state.leases = await api("/v1/leases");
    state.loaded.leases = true;
    renderTable("leases", () => {
      const tbody = $("#leases tbody");
      if (tbody) tbody.removeAttribute("aria-busy");
      renderLeases();
    });
    return true;
  } catch {
    if (!state.loaded.leases) {
      showTableError("leases", "Leases could not load.", () => retryTable(refreshLeases));
    }
    return false;
  }
}

async function refreshContainers() {
  try {
    const view = await api("/v1/containers");
    state.containers = view;
    state.loaded.containers = true;
    const warnings = (view.warnings || []).filter((w) =>
      !(w.startsWith("container ") && w.includes("publishes host ports directly"))
    );
    renderTable("unmanaged", () => {
      const tbody = $("#unmanaged tbody");
      if (tbody) tbody.removeAttribute("aria-busy");
      renderWarnings(warnings);
      renderUnmanaged();
    });
    return true;
  } catch {
    if (!state.loaded.containers) {
      showTableError("unmanaged", "Containers could not load.", () => retryTable(refreshContainers));
    }
    return false;
  }
}

async function refreshAll() {
  const results = await Promise.allSettled([
    refreshStatus(),
    refreshRoutes(),
    refreshRules(),
    refreshLeases(),
    refreshContainers(),
  ]);
  return results.every((r) => r.status === "fulfilled" && r.value);
}

function queueRefresh() {
  if (refreshQueued) return;
  refreshQueued = true;
  setTimeout(() => {
    refreshQueued = false;
    refreshAll();
  }, 250);
}

function connectEvents() {
  if (eventSource) return;
  let es;
  try {
    es = new EventSource("/v1/events/stream");
  } catch {
    updateConnection("sse", "error");
    return;
  }
  eventSource = es;
  es.onopen = () => {
    updateConnection("sse", "open");
    loadEventHistory();
  };
  es.onerror = () => {
    updateConnection("sse", "error");
  };
  es.onmessage = (message) => {
    try {
      const ev = JSON.parse(message.data);
      if (historyLoading) {
        if (bufferedEvents.length < 200) bufferedEvents.push(ev);
      } else {
        appendEvent(ev);
      }
      if (REFRESH_KINDS.includes(ev.kind)) queueRefresh();
    } catch { /* ignore malformed events */ }
  };
}

async function initialLoad() {
  clearTimeout(bootTimer);
  const results = await Promise.allSettled([refreshRoutes(), refreshRules(), refreshLeases(), refreshContainers()]);
  const ok = results.every((r) => r.status === "fulfilled" && r.value);
  if (ok) {
    bootAttempt = 0;
    bootNotified = false;
    return;
  }
  if (!bootNotified) {
    bootNotified = true;
    notify("Some data could not load. Use Retry in the table.", "error");
  }
  bootAttempt += 1;
  const delay = Math.min(30000, 2000 * Math.pow(2, Math.min(bootAttempt - 1, 4)));
  bootTimer = setTimeout(initialLoad, delay);
}

function startPolling() {
  refreshStatus();
  setInterval(refreshStatus, 5000);
}

/* ---------- search, policy, enforcement ---------- */

function initSearch() {
  const input = $("#app-search");
  if (!input) return;
  state.search = input.value;
  const apply = () => {
    state.search = input.value;
    if (state.loaded.routes) renderApplications();
  };
  input.addEventListener("input", apply);
  input.addEventListener("search", apply);
}

function initSettingsControls() {
  const policy = $("#policy");
  if (policy) {
    policy.addEventListener("change", async () => {
      if (policySaving) return;
      const previous = state.status ? state.status.policy : "rewrite";
      const next = policy.value;
      policySaving = true;
      policy.disabled = true;
      try {
        await api("/v1/policy", { method: "POST", body: JSON.stringify({ policy: next }) });
        notify("Policy set to " + next + ".");
        await refreshAll();
      } catch (err) {
        policy.value = previous;
        notify(err.message, "error");
      } finally {
        policySaving = false;
        policy.disabled = false;
      }
    });
  }

  const enforce = $("#enforce");
  if (enforce) {
    enforce.addEventListener("change", async (e) => {
      if (enforcing) return;
      const enabled = e.target.checked;
      enforcing = true;
      try {
        const st = await api("/v1/enforce", { method: "POST", body: JSON.stringify({ enabled }) });
        const parts = [];
        if (enabled) {
          parts.push(st && st.socket ? "Enforcement listening on " + st.socket + "." : "Enforcement enabled.");
        } else {
          parts.push("Enforcement disabled.");
        }
        const instructions = st && st.instructions ? st.instructions : [];
        if (instructions.length) parts.push(instructions.join(" "));
        notify(parts.join(" "), instructions.length ? "warn" : "info");
        await refreshStatus();
      } catch (err) {
        notify(err.message, "error");
        e.target.checked = !enabled;
      } finally {
        enforcing = false;
      }
    });
  }
}

/* ---------- boot ---------- */

function init() {
  initTabs();
  initSearch();
  initRuleDialog();
  initSettingsControls();
  renderLoadingRows();
  renderReservedPorts();
  startPolling();
  connectEvents();
  loadEventHistory();
  initialLoad();
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", init);
} else {
  init();
}
