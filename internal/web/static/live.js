// Live updates without a framework. A page opts in with <main data-live="seconds">; every
// few seconds the same URL is fetched again and each element marked data-live-id is
// replaced with its fresh copy. The HTML is the server's own escaped output, so nothing
// here builds markup. Without JavaScript the pages fall back to <noscript> refreshes.
(function () {
  "use strict";
  var main = document.querySelector("main[data-live]");
  var status = document.querySelector("[data-live-status]");
  var timer = null;
  var busy = false;
  // Charts change once a minute; most refreshes ask the server to leave them out.
  var lastFull = Date.now();

  function interval(el) {
    var n = parseInt(el && el.getAttribute("data-live"), 10);
    return n > 0 ? n * 1000 : 0;
  }

  // Scroll log views to the newest line, but only if the reader is already there.
  function atBottom(el) { return el.scrollHeight - el.scrollTop - el.clientHeight < 40; }
  function followers() {
    return Array.prototype.map.call(document.querySelectorAll("[data-follow]"), function (el) {
      return { bottom: atBottom(el), top: el.scrollTop };
    });
  }
  function restore(saved) {
    document.querySelectorAll("[data-follow]").forEach(function (el, i) {
      var s = saved[i];
      if (!s || s.bottom) { el.scrollTop = el.scrollHeight; } else { el.scrollTop = s.top; }
    });
  }

  function setStatus(text, stale) {
    if (!status) return;
    status.hidden = false;
    status.textContent = text;
    status.classList.toggle("stale", !!stale);
  }

  function schedule(ms) {
    clearTimeout(timer);
    if (ms > 0 && !document.hidden) timer = setTimeout(refresh, ms);
  }

  function refresh() {
    if (busy || document.hidden) return;
    busy = true;
    var full = Date.now() - lastFull >= 60000;
    fetch(location.href, { headers: { "X-FootOps-Live": full ? "full" : "1" }, credentials: "same-origin", cache: "no-store" })
      .then(function (res) {
        // The session ended: show the login page instead of stale numbers.
        if (res.redirected && new URL(res.url).pathname === "/login") { location.assign("/login"); throw null; }
        if (!res.ok) throw new Error(res.status);
        return res.text();
      })
      .then(function (text) {
        if (full) lastFull = Date.now();
        var doc = new DOMParser().parseFromString(text, "text/html");
        var saved = followers();
        document.querySelectorAll("[data-live-id]").forEach(function (el) {
          var fresh = doc.querySelector('[data-live-id="' + el.getAttribute("data-live-id") + '"]');
          if (fresh) el.replaceWith(document.importNode(fresh, true));
        });
        restore(saved);
        var next = doc.querySelector("main");
        var ms = interval(next);
        if (!ms) { main.removeAttribute("data-live"); setStatus("güncel", false); return; }
        main.setAttribute("data-live", next.getAttribute("data-live"));
        setStatus("canlı · " + new Date().toLocaleTimeString("tr-TR"), false);
        schedule(ms);
      })
      .catch(function (err) {
        if (err === null) return;
        setStatus("bağlantı yok, tekrar deneniyor", true);
        schedule(Math.max(interval(main), 5000) * 2);
      })
      .finally(function () { busy = false; });
  }

  document.querySelectorAll("[data-follow]").forEach(function (el) { el.scrollTop = el.scrollHeight; });
  if (!main) return;
  setStatus("canlı", false);
  document.addEventListener("visibilitychange", function () {
    if (document.hidden) { clearTimeout(timer); } else if (main.hasAttribute("data-live")) { refresh(); }
  });
  schedule(interval(main));
})();
