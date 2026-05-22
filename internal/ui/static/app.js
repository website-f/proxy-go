// jobcloud — tiny progressive-enhancement script.
// Avoids pulling in htmx/vue: ~50 lines of vanilla JS is all the
// interactivity this UI actually needs.

(function () {
  // Poll every element with [data-poll-url] every [data-poll-ms] ms and
  // replace its innerHTML with the response. Used by the dashboard
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

  // Confirm() wrapper for buttons that declare data-confirm.
  document.addEventListener("submit", function (e) {
    var msg = e.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
    }
  });
})();
