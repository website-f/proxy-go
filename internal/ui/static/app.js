// jobcloud — tiny progressive-enhancement script.
// Vanilla JS, no frameworks. Polls the dashboard partial, wires up the
// mobile nav toggle, and handles data-confirm submits.

(function () {
  // ── Live-poll partials ──────────────────────────────────────────
  // Any [data-poll-url] element re-fetches its URL every [data-poll-ms]
  // ms and swaps innerHTML with the response. Used by the dashboard
  // table to live-update without a full page reload.
  function pollOnce(el) {
    fetch(el.dataset.pollUrl, { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) { if (html != null) el.innerHTML = html; })
      .catch(function () { /* swallow; next tick retries */ });
  }
  document.querySelectorAll("[data-poll-url]").forEach(function (el) {
    var ms = parseInt(el.dataset.pollMs || "3000", 10);
    pollOnce(el);
    setInterval(function () { pollOnce(el); }, ms);
  });

  // ── Mobile nav toggle ───────────────────────────────────────────
  var toggle = document.querySelector("[data-nav-toggle]");
  var nav = document.querySelector("[data-nav]");
  if (toggle && nav) {
    toggle.addEventListener("click", function (e) {
      e.stopPropagation();
      var open = nav.classList.toggle("open");
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
    });
    // Close on outside click
    document.addEventListener("click", function (e) {
      if (!nav.classList.contains("open")) return;
      if (nav.contains(e.target) || toggle.contains(e.target)) return;
      nav.classList.remove("open");
      toggle.setAttribute("aria-expanded", "false");
    });
    // Close on Escape
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && nav.classList.contains("open")) {
        nav.classList.remove("open");
        toggle.setAttribute("aria-expanded", "false");
        toggle.focus();
      }
    });
  }

  // ── data-confirm submit guard ───────────────────────────────────
  document.addEventListener("submit", function (e) {
    var msg = e.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
    }
  });
})();
