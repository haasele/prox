const state = { status: null, routes: [], rules: [], leases: [], containers: null, events: [] };

const $ = (sel) => document.querySelector(sel);

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) {
    const msg = data && data.error ? data.error : (res.status + " " + res.statusText);
    throw new Error(msg);
  }
  return data;
}

let toastTimer = null;
function toast(msg, isError = false) {
  let el = document.querySelector(".toast");
  if (!el) {
    el = document.createElement("div");
    el.className = "toast";
    document.body.appendChild(el);
  }
  el.textContent = msg;
  el.classList.toggle("error", isError);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.remove(), isError ? 8000 : 3500);
}

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

function fmtTime(sec) {
  if (!sec) return "";
  return new Date(sec * 1000).toLocaleTimeString();
}

function httpsURL(host) {
  if (!state.status) return "https://" + host;
  const port = state.status.https_port;
  return "https://" + host + (port && port !== 443 ? ":" + port : "");
}

function routeURL(route) {
  if (route.proto === "tcp") {
    return "tcp://" + route.host + ":" + route.gateway_port;
  }
  return httpsURL(route.host);
}

function routeStatus(route) {
  const badge = el("span", "badge " + (route.status || "pending"), route.status || "pending");
  return badge;
}

function allowManaged(route) {
  const name = route.service || route.container_name || route.host.split(".")[0];
  const body = {
    name,
    project: route.project || "",
    source: route.source_port || 0,
    target: route.proto === "http" ? 443 : (route.gateway_port || 0),
    resolve: route.host,
    proto: route.proto === "http" ? "http" : "tcp",
    action: "allow",
  };
  return createRule(body, "Allowed " + route.host);
}

async function createRule(body, success) {
  try {
    const rule = await api("/v1/rules", { method: "POST", body: JSON.stringify(body) });
    toast(success + " (rule " + rule.id + ")");
    await refreshAll();
  } catch (e) {
    toast(e.message, true);
  }
}

function renderRoutes() {
  const wrap = $("#routes");
  wrap.replaceChildren();
  $("#route-count").textContent = state.routes.length + " total";
  if (!state.routes.length) {
    wrap.appendChild(el("p", "empty", "No routes yet. Create a container through the prox Docker socket."));
    return;
  }
  for (const r of state.routes) {
    const card = el("article", "card");
    const head = el("div", "card-head");
    head.appendChild(el("span", "host", r.host));
    head.appendChild(routeStatus(r));
    card.appendChild(head);
    const meta = el("div", "meta");
    meta.textContent = [
      r.proto,
      r.target_addr || "no upstream yet",
      r.project ? "project " + r.project : "",
      r.service ? "service " + r.service : "",
      r.collision ? "host collision — renamed" : "",
    ].filter(Boolean).join(" · ");
    card.appendChild(meta);
    const actions = el("div", "actions");
    const open = el("a", "button", r.proto === "tcp" ? "connect" : "open ↗");
    open.href = routeURL(r);
    open.target = "_blank";
    open.rel = "noreferrer";
    if (r.proto !== "tcp" || r.gateway_port) actions.appendChild(open);
    if (r.proto !== "tcp") {
      const allow = el("button", "button", "pin as rule");
      allow.onclick = () => allowManaged(r);
      actions.appendChild(allow);
    }
    card.appendChild(actions);
    wrap.appendChild(card);
  }
}

function renderRules() {
  const tbody = $("#rules tbody");
  tbody.replaceChildren();
  if (!state.rules.length) {
    tbody.appendChild(el("tr")).appendChild(el("td", "empty", "No rules. Defaults apply.")).colSpan = 8;
    return;
  }
  for (const r of state.rules) {
    const tr = el("tr");
    tr.appendChild(el("td", "", String(r.id)));
    tr.appendChild(el("td", "", r.name));
    tr.appendChild(el("td", "", r.project || "—"));
    tr.appendChild(el("td", "", r.action));
    tr.appendChild(el("td", "", r.source ? String(r.source) : "auto"));
    tr.appendChild(el("td", "", r.target ? String(r.target) : "—"));
    tr.appendChild(el("td", "", r.resolve || "—"));
    const td = el("td");
    const del = el("button", "button", "×");
    del.title = "delete rule";
    del.onclick = async () => {
      try {
        await api("/v1/rules/" + r.id, { method: "DELETE" });
        toast("Deleted rule " + r.id);
        await refreshAll();
      } catch (e) { toast(e.message, true); }
    };
    td.appendChild(del);
    tr.appendChild(td);
    tbody.appendChild(tr);
  }
}

function renderLeases() {
  const tbody = $("#leases tbody");
  tbody.replaceChildren();
  if (!state.leases.length) {
    const tr = el("tr");
    const td = el("td", "empty", "No host process leases.");
    td.colSpan = 5;
    tr.appendChild(td);
    tbody.appendChild(tr);
    return;
  }
  for (const l of state.leases) {
    const tr = el("tr");
    tr.appendChild(el("td", "", String(l.id)));
    tr.appendChild(el("td", "", l.host));
    tr.appendChild(el("td", "", String(l.pid)));
    tr.appendChild(el("td", "", String(l.port)));
    tr.appendChild(el("td", "", l.command || "—"));
    tbody.appendChild(tr);
  }
}

function renderWarnings(warnings) {
  const box = $("#warnings");
  const list = $("#warning-list");
  list.replaceChildren();
  if (!warnings || !warnings.length) {
    box.classList.add("hidden");
    return;
  }
  box.classList.remove("hidden");
  for (const w of warnings) list.appendChild(el("li", "", w));
}

function renderUnmanaged() {
  const wrap = $("#unmanaged");
  wrap.replaceChildren();
  const items = state.containers ? state.containers.unmanaged : [];
  if (!items.length) {
    wrap.appendChild(el("p", "empty", "Every running container is either managed by prox or publishes no host ports."));
    return;
  }
  for (const c of items) {
    const card = el("article", "card");
    const head = el("div", "card-head");
    head.appendChild(el("span", "host", c.name || c.id.slice(0, 12)));
    head.appendChild(el("span", "badge down", "unmanaged"));
    card.appendChild(head);
    const meta = el("div", "meta");
    meta.textContent = [c.image, c.project ? "project " + c.project : "", c.ports.join(", ")].filter(Boolean).join(" · ");
    card.appendChild(meta);
    const actions = el("div", "actions");
    const allow = el("button", "button primary", "allow");
    allow.onclick = () => createRule(c.suggestion, "Allowed " + c.suggestion.name);
    actions.appendChild(allow);
    card.appendChild(actions);
    wrap.appendChild(card);
  }
}

function eventAllowButton(ev) {
  const d = ev.data || {};
  if (!d.container && !d.service) return null;
  const name = d.service || d.container;
  const btn = el("button", "button", "allow");
  btn.onclick = async () => {
    try {
      const q = new URLSearchParams({
        name,
        project: d.project || "",
        image: d.image || "",
        port: (d.ports && d.ports.length) ? String(d.ports[0]) : "",
      });
      const suggestion = await api("/v1/suggest?" + q.toString());
      await createRule(suggestion, "Allowed " + suggestion.name);
    } catch (e) { toast(e.message, true); }
  };
  return btn;
}

function appendEvent(ev, prepend = true) {
  const list = $("#events");
  const li = el("li");
  li.appendChild(el("time", "", fmtTime(ev.ts)));
  li.appendChild(el("span", "kind lvl-" + (ev.level || "info"), ev.kind));
  const msg = el("span", "", ev.message);
  li.appendChild(msg);
  const allow = eventAllowButton(ev);
  if (allow) li.appendChild(allow);
  if (prepend) list.prepend(li); else list.appendChild(li);
  while (list.children.length > 200) list.lastChild.remove();
}

function renderEventHistory(events) {
  $("#events").replaceChildren();
  for (const ev of events) appendEvent(ev, false);
}

function renderStatus() {
  const s = state.status;
  const pill = $("#status-pill");
  if (!s) { pill.textContent = "offline"; pill.className = "pill bad"; return; }
  if (!s.docker_ok) {
    pill.textContent = "docker unreachable";
    pill.className = "pill bad";
    pill.title = s.docker_error || "";
  } else {
    pill.textContent = s.backend + " · " + s.routes + " routes · " + s.uptime_seconds + "s";
    pill.className = "pill ok";
  }
  $("#policy").value = s.policy;
  $("#enforce").checked = !!s.enforcement;
}

async function refreshStatus() { state.status = await api("/v1/status"); renderStatus(); }
async function refreshRoutes() { state.routes = await api("/v1/routes"); renderRoutes(); }
async function refreshRules() { state.rules = await api("/v1/rules"); renderRules(); }
async function refreshLeases() { state.leases = await api("/v1/leases"); renderLeases(); }
async function refreshContainers() {
  state.containers = await api("/v1/containers");
  renderWarnings(state.containers.warnings);
  renderUnmanaged();
}

let refreshQueued = false;
function queueRefresh() {
  if (refreshQueued) return;
  refreshQueued = true;
  setTimeout(async () => {
    refreshQueued = false;
    try { await refreshAll(); } catch (e) { toast(e.message, true); }
  }, 250);
}

async function refreshAll() {
  await Promise.all([refreshStatus(), refreshRoutes(), refreshRules(), refreshLeases(), refreshContainers()]);
}

function connectEvents() {
  const es = new EventSource("/v1/events/stream");
  es.onmessage = (m) => {
    try {
      const ev = JSON.parse(m.data);
      appendEvent(ev);
      if (["rewrite", "route", "collision", "lease", "deny", "reconcile", "rule", "policy"].includes(ev.kind)) queueRefresh();
    } catch { /* ignore */ }
  };
  es.onerror = () => { /* EventSource reconnects automatically */ };
}

$("#policy").addEventListener("change", async (e) => {
  try {
    await api("/v1/policy", { method: "POST", body: JSON.stringify({ policy: e.target.value }) });
    toast("Policy set to " + e.target.value);
    await refreshAll();
  } catch (err) { toast(err.message, true); }
});

$("#enforce").addEventListener("change", async (e) => {
  const enabled = e.target.checked;
  try {
    const st = await api("/v1/enforce", { method: "POST", body: JSON.stringify({ enabled }) });
    toast(enabled ? "Enforcement plugin listening on " + st.socket : "Enforcement disabled");
    if (st.instructions && st.instructions.length) {
      toast(st.instructions.join(" • "), true);
    }
    await refreshStatus();
  } catch (err) {
    toast(err.message, true);
    e.target.checked = !enabled;
  }
});

(async function init() {
  try {
    await refreshAll();
    const events = await api("/v1/events?limit=80");
    renderEventHistory(events);
    connectEvents();
    setInterval(refreshStatus, 5000);
  } catch (e) {
    toast("Cannot reach the prox API: " + e.message, true);
    const pill = $("#status-pill");
    pill.textContent = "offline";
    pill.className = "pill bad";
  }
})();
