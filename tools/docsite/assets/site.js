// Wormhole docs: theme switch, search over the generated index, in-page
// contents tracking, copy buttons on code. No dependencies, no requests
// beyond the site's own files.
(function () {
  "use strict";

  // Theme.
  var root = document.documentElement;
  var toggle = document.querySelector("[data-theme-toggle]");
  function currentTheme() {
    var t = root.getAttribute("data-theme");
    if (t) return t;
    return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
  function labelToggle() {
    if (!toggle) return;
    var next = currentTheme() === "dark" ? "light" : "dark";
    toggle.setAttribute("aria-label", "Switch to " + next + " theme");
  }
  if (toggle) {
    toggle.addEventListener("click", function () {
      var next = currentTheme() === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", next);
      try { localStorage.setItem("wh-theme", next); } catch (e) {}
      labelToggle();
    });
    labelToggle();
  }

  // Search.
  var input = document.getElementById("q");
  var panel = document.getElementById("search-results");
  var index = null;
  var loading = null;
  function loadIndex() {
    if (index || loading) return loading;
    loading = fetch(input.getAttribute("data-index")).then(function (r) { return r.json(); }).then(function (j) { index = j; return j; });
    return loading;
  }
  function escapeHTML(s) {
    return s.replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; });
  }
  function snippet(text, q) {
    var i = text.toLowerCase().indexOf(q);
    if (i < 0) return "";
    var start = Math.max(0, i - 60);
    var end = Math.min(text.length, i + q.length + 80);
    var s = (start > 0 ? "…" : "") + text.slice(start, end) + (end < text.length ? "…" : "");
    var re = new RegExp("(" + q.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + ")", "ig");
    return escapeHTML(s).replace(re, "<mark>$1</mark>");
  }
  function search(q) {
    q = q.trim().toLowerCase();
    if (q.length < 2 || !index) { panel.hidden = true; panel.innerHTML = ""; return; }
    var hits = [];
    index.forEach(function (p) {
      var score = 0;
      var where = p.url;
      var label = "";
      if (p.title.toLowerCase().indexOf(q) >= 0) score += 10;
      (p.headings || []).forEach(function (h) {
        if (h.text.toLowerCase().indexOf(q) >= 0 && score < 20) { score += 6; where = p.url + "#" + h.id; label = h.text; }
      });
      var t = p.text.toLowerCase();
      var n = t.split(q).length - 1;
      if (n > 0) score += Math.min(n, 5);
      if (score > 0) hits.push({ page: p, score: score, url: where, label: label });
    });
    hits.sort(function (a, b) { return b.score - a.score || a.page.number - b.page.number; });
    hits = hits.slice(0, 6);
    if (!hits.length) {
      panel.innerHTML = '<p class="search-empty">No sheet mentions “' + escapeHTML(q) + '”.</p>';
      panel.hidden = false;
      return;
    }
    panel.innerHTML = "<ol>" + hits.map(function (h) {
      return '<li><a href="' + h.url + '"><span class="search-n">' + String(h.page.number).padStart(2, "0") + "</span>" +
        '<span class="search-body"><span class="search-title">' + escapeHTML(h.page.title) + (h.label ? ' <span class="search-h">› ' + escapeHTML(h.label) + "</span>" : "") + "</span>" +
        '<span class="search-snippet">' + snippet(h.page.text, q) + "</span></span></a></li>";
    }).join("") + "</ol>";
    panel.hidden = false;
  }
  if (input && panel) {
    input.addEventListener("focus", loadIndex);
    input.addEventListener("input", function () {
      var q = input.value;
      loadIndex().then(function () { if (input.value === q) search(q); });
    });
    input.addEventListener("keydown", function (e) {
      if (e.key === "Escape") { panel.hidden = true; input.blur(); }
      if (e.key === "ArrowDown") { var first = panel.querySelector("a"); if (first) { first.focus(); e.preventDefault(); } }
    });
    panel.addEventListener("keydown", function (e) {
      var links = Array.prototype.slice.call(panel.querySelectorAll("a"));
      var i = links.indexOf(document.activeElement);
      if (e.key === "ArrowDown" && i < links.length - 1) { links[i + 1].focus(); e.preventDefault(); }
      if (e.key === "ArrowUp") { if (i > 0) links[i - 1].focus(); else input.focus(); e.preventDefault(); }
      if (e.key === "Escape") { panel.hidden = true; input.focus(); }
    });
    document.addEventListener("click", function (e) {
      if (!panel.contains(e.target) && e.target !== input) panel.hidden = true;
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "/" && document.activeElement !== input && !/input|textarea/i.test(document.activeElement.tagName)) {
        input.focus(); e.preventDefault();
      }
    });
  }

  // In-page contents: mark the heading currently in view.
  var contents = document.querySelector(".contents");
  if (contents && "IntersectionObserver" in window) {
    var links = {};
    contents.querySelectorAll("a[href^='#']").forEach(function (a) { links[a.getAttribute("href").slice(1)] = a; });
    var visible = {};
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) { visible[en.target.id] = en.isIntersecting; });
      var ids = Object.keys(links);
      var active = null;
      for (var i = 0; i < ids.length; i++) { if (visible[ids[i]]) { active = ids[i]; break; } }
      if (!active) return;
      ids.forEach(function (id) { links[id].classList.toggle("active", id === active); });
    }, { rootMargin: "-10% 0px -70% 0px", threshold: 0 });
    Object.keys(links).forEach(function (id) { var el = document.getElementById(id); if (el) io.observe(el); });
  }

  // Copy buttons on code blocks.
  document.querySelectorAll(".prose pre").forEach(function (pre) {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy";
    btn.setAttribute("aria-label", "Copy to clipboard");
    btn.innerHTML = '<svg class="icon" viewBox="0 0 20 20" aria-hidden="true" focusable="false"><rect x="7" y="7" width="10" height="10" rx="1.5"/><path d="M13 7V4.5A1.5 1.5 0 0 0 11.5 3h-7A1.5 1.5 0 0 0 3 4.5v7A1.5 1.5 0 0 0 4.5 13H7"/></svg>';
    btn.addEventListener("click", function () {
      var text = pre.querySelector("code") ? pre.querySelector("code").innerText : pre.innerText;
      navigator.clipboard.writeText(text.replace(/\n$/, "")).then(function () {
        btn.classList.add("done");
        btn.setAttribute("aria-label", "Copied");
        setTimeout(function () { btn.classList.remove("done"); btn.setAttribute("aria-label", "Copy to clipboard"); }, 1400);
      });
    });
    pre.appendChild(btn);
  });
})();
