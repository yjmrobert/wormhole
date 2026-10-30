// The inbox page. State comes from /api/inbox and is pushed over /api/events
// whenever anything changes; the buttons call the same verbs the CLI runs.
(() => {
  "use strict";
  const token = new URLSearchParams(location.search).get("t") || "";
  const $ = (id) => document.getElementById(id);
  let view = { items: [], waiting: 0, snoozed: 0, handle: "", agent_available: false, snooze_default: "2h" };
  let selected = null;
  let busy = false;
  let pending = null; // {kind: "open"|"agent"|"decline"}

  const api = async (path, opts = {}) => {
    const res = await fetch(path, {
      ...opts,
      headers: { "X-Wormhole-Token": token, "Content-Type": "application/json", ...(opts.headers || {}) },
    });
    let body = {};
    try { body = await res.json(); } catch (_) { /* no body */ }
    if (!res.ok || body.ok === false) {
      throw new Error(body.error || `${res.status} ${res.statusText}`);
    }
    return body;
  };

  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  const ago = (iso) => {
    const d = (Date.now() - new Date(iso).getTime()) / 1000;
    if (d < 60) return `${Math.max(0, Math.floor(d))}s ago`;
    if (d < 3600) return `${Math.floor(d / 60)}m ago`;
    if (d < 172800) return `${Math.floor(d / 3600)}h ago`;
    return `${Math.floor(d / 86400)}d ago`;
  };
  const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

  const toast = (text, bad) => {
    const el = $("toast");
    el.textContent = text;
    el.classList.toggle("bad", !!bad);
    el.hidden = false;
    clearTimeout(toast.t);
    toast.t = setTimeout(() => { el.hidden = true; }, bad ? 8000 : 4000);
  };

  const renderList = () => {
    const list = $("list");
    list.querySelectorAll(".item").forEach((n) => n.remove());
    $("empty").hidden = view.items.length > 0;
    for (const it of view.items) {
      const b = document.createElement("button");
      b.type = "button";
      b.className = "item" + (it.thread_id === selected ? " active" : "");
      b.dataset.id = it.thread_id;
      const tags = [`<span class="tag ${it.kind === "ask" ? "ask" : ""}">${it.kind}</span>`];
      if (it.repo) tags.push(`<span class="tag">${esc(it.repo)}</span>`);
      if (it.ref) tags.push(`<span class="tag">${esc(it.ref)}</span>`);
      if (it.snoozed_until) tags.push(`<span class="tag snz">snoozed · ${esc(clock(it.snoozed_until))}</span>`);
      b.innerHTML = `<div class="who">^${esc(it.peer)}</div><div class="when">${esc(ago(it.received))}</div>` +
        `<div class="what">${esc(it.preview)}</div><div class="tags">${tags.join("")}</div>`;
      b.addEventListener("click", () => { selected = it.thread_id; pending = null; render(); });
      list.appendChild(b);
    }
  };

  const renderPane = () => {
    const it = view.items.find((x) => x.thread_id === selected);
    $("pane").hidden = !it;
    $("placeholder").hidden = !!it;
    if (!it) return;
    const verb = it.kind === "ask" ? "asks" : "replied";
    $("title").textContent = `^${it.peer} ${verb}${it.ref ? ` about ${it.ref}` : ""}`;
    const expires = Math.max(0, Math.round((new Date(it.expires_at) - Date.now()) / 86400000));
    $("sub").textContent = `thread ${it.thread_id} · human_reviewed: ${it.human_reviewed} · hop ${it.hop} · expires in ${expires}d` + (it.snoozed_until ? ` · snoozed until ${clock(it.snoozed_until)}` : "");
    const msg = $("msg");
    msg.textContent = "";
    const cut = it.text.indexOf("\n---\n");
    if (cut >= 0) {
      const pre = document.createElement("span");
      pre.className = "pre";
      pre.textContent = it.text.slice(0, cut + 5);
      msg.appendChild(pre);
      msg.appendChild(document.createTextNode(it.text.slice(cut + 5)));
    } else {
      msg.textContent = it.text;
    }
    $("agent").disabled = !view.agent_available || busy;
    $("agent").title = view.agent_available ? "Run it headless under copilot.safe_tools and extra_args" : "copilot.safe_tools is empty; set the auto-mode profile first";
    $("open").disabled = busy;
    $("later").disabled = busy;
    $("decline").disabled = busy;
    $("unsnooze").hidden = !it.snoozed_until;
    $("hint").textContent = busy
      ? "Running the kickoff headless first; this can take a minute. The session appears in the Copilot app when it is done."
      : `Open runs it with your normal permissions and shows the session in the Copilot app. Hand to my agent runs it headless under the safe profile. Later hides it (default ${view.snooze_default}); Decline tells ^${it.peer} you passed.`;
    const c = $("confirm");
    c.hidden = !pending;
    if (pending) {
      $("reason").hidden = pending.kind !== "decline";
      $("confirmtext").textContent = {
        open: `Run this message from ^${it.peer} with your normal tools?`,
        agent: `Hand this message from ^${it.peer} to your agent with the safe profile (copilot.safe_tools)?`,
        decline: `Decline this message from ^${it.peer}? They will see "declined" and your reason, if any.`,
      }[pending.kind];
      if (pending.kind === "decline") $("reason").focus(); else $("confirmyes").focus();
    }
  };

  const render = () => {
    const n = view.waiting;
    document.title = n > 0 ? `(${n}) Wormhole inbox` : "Wormhole inbox";
    $("handle").textContent = `inbox · ^${view.handle}`;
    $("conn").textContent = `${view.waiting} waiting · ${view.snoozed} snoozed` + (view.paused ? " · daemon paused" : "");
    if (selected && !view.items.some((x) => x.thread_id === selected)) selected = null;
    if (!selected && view.items.length) selected = view.items[0].thread_id;
    renderList();
    renderPane();
  };

  const apply = (v) => { view = v; render(); };

  const load = async () => {
    try {
      apply(await api("/api/inbox"));
      $("dot").className = "dot ok";
    } catch (e) {
      $("dot").className = "dot bad";
      $("conn").textContent = `cannot reach the daemon: ${e.message}`;
    }
  };

  const listen = () => {
    const es = new EventSource(`/api/events?t=${encodeURIComponent(token)}`);
    es.addEventListener("inbox", (ev) => {
      $("dot").className = "dot ok";
      apply(JSON.parse(ev.data));
    });
    es.onerror = () => {
      $("dot").className = "dot bad";
      $("conn").textContent = "daemon connection lost; retrying";
    };
  };

  const act = async (kind) => {
    const id = selected;
    if (!id) return;
    busy = true;
    pending = null;
    render();
    try {
      if (kind === "open" || kind === "agent") {
        const r = await api(`/api/threads/${id}/open`, { method: "POST", body: JSON.stringify({ agent: kind === "agent" }) });
        const res = r.result || {};
        toast(res.opened ? `Opened session ${res.session_id} in the Copilot app` : `Session ${res.session_id} started; the app did not open (${res.open_error}). Fallback: ${res.fallback}`, !res.opened);
      } else if (kind === "decline") {
        const r = await api(`/api/threads/${id}/decline`, { method: "POST", body: JSON.stringify({ reason: $("reason").value }) });
        toast(`Declined; ^${r.peer} will see: ${r.status}`);
        $("reason").value = "";
      }
    } catch (e) {
      toast(e.message, true);
    } finally {
      busy = false;
      await load();
    }
  };

  const snooze = async (duration) => {
    const id = selected;
    if (!id) return;
    $("latermenu").hidden = true;
    try {
      const r = await api(`/api/threads/${id}/snooze`, { method: "POST", body: JSON.stringify({ duration }) });
      toast(r.snoozed_until ? `Snoozed until ${clock(r.snoozed_until)}; the daemon will remind you once` : "Back in the inbox");
    } catch (e) {
      toast(e.message, true);
    }
    await load();
  };

  $("open").addEventListener("click", () => { pending = { kind: "open" }; render(); });
  $("agent").addEventListener("click", () => { pending = { kind: "agent" }; render(); });
  $("decline").addEventListener("click", () => { pending = { kind: "decline" }; render(); });
  $("confirm").addEventListener("submit", (e) => { e.preventDefault(); if (pending) act(pending.kind); });
  $("confirmno").addEventListener("click", () => { pending = null; render(); });
  $("later").addEventListener("click", () => { $("latermenu").hidden = !$("latermenu").hidden; });
  $("latermenu").querySelectorAll("button[data-d]").forEach((b) => b.addEventListener("click", () => snooze(b.dataset.d)));
  $("customsnooze").addEventListener("submit", (e) => { e.preventDefault(); snooze($("customd").value.trim()); $("customd").value = ""; });
  $("unsnooze").addEventListener("click", () => snooze("off"));
  document.addEventListener("click", (e) => { if (!e.target.closest(".later")) $("latermenu").hidden = true; });
  document.addEventListener("keydown", (e) => {
    if (e.target.tagName === "INPUT" || busy) return;
    const i = view.items.findIndex((x) => x.thread_id === selected);
    if (e.key === "j" || e.key === "ArrowDown") { if (i < view.items.length - 1) { selected = view.items[i + 1].thread_id; pending = null; render(); } }
    else if (e.key === "k" || e.key === "ArrowUp") { if (i > 0) { selected = view.items[i - 1].thread_id; pending = null; render(); } }
    else if (e.key === "Escape") { pending = null; $("latermenu").hidden = true; render(); }
  });

  load().then(listen);
})();
