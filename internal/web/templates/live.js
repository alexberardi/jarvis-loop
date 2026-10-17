// The board's "Working now" section refreshes itself every 5 s without reloading the page: it
// fetches the server-rendered (html/template-escaped) fragment its data-live names and swaps it
// in. Same origin only (CSP connect-src 'self'); the board's cookie is HttpOnly, so nothing here
// can read it.
(function () {
  var el = document.getElementById("working");
  if (!el || !el.dataset.live) return;
  var src = el.dataset.live;
  function tick() {
    if (document.hidden) return;
    fetch(src, { credentials: "same-origin", cache: "no-store" }).then(function (r) {
      if (r.ok && (r.headers.get("Content-Type") || "").indexOf("text/html") === 0) return r.text();
    }).then(function (html) {
      if (typeof html === "string") el.innerHTML = html;
    }).catch(function () {});
  }
  setInterval(tick, 5000);
  document.addEventListener("visibilitychange", tick);
})();
