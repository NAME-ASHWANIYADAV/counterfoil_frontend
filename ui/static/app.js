// Counterfoil: light live-refresh. Pages that carry <main data-live="1">
// re-fetch themselves as a partial every 2.5 s, skipping refreshes while
// the person is typing or has a dialog open. No framework, no surprises.
(function () {
  var main = document.getElementById("main");
  if (!main || main.dataset.live !== "1") return;
  var timer = setInterval(function () {
    if (document.activeElement &&
        /INPUT|TEXTAREA|SELECT|BUTTON/.test(document.activeElement.tagName)) return;
    var url = new URL(window.location.href);
    url.searchParams.set("partial", "1");
    fetch(url.toString(), { headers: { "X-Partial": "1" } })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) {
        if (html === null) return;
        var sc = { x: window.scrollX, y: window.scrollY };
        var tmp = document.createElement("div");
        tmp.innerHTML = html;
        var next = tmp.querySelector("main") || tmp;
        main.innerHTML = next.innerHTML;
        window.scrollTo(sc.x, sc.y);
      })
      .catch(function () {});
  }, 2500);
  window.addEventListener("beforeunload", function () { clearInterval(timer); });
})();

// Keep chat scrolled to the latest message.
(function () {
  var log = document.querySelector(".chat .log");
  if (log) log.scrollTop = log.scrollHeight;
})();

// One arrival, then stillness: drop the boot class after the entrance so
// the live poller's re-renders never replay it.
setTimeout(function () { document.documentElement.classList.remove("boot"); }, 950);

// Honest busy state on genuinely slow actions (the model draft takes seconds).
document.addEventListener("submit", function (e) {
  var b = e.target.querySelector("button[data-slow]");
  if (b) { b.dataset.busy = "1"; b.textContent = b.dataset.slow; }
});

// A hash chip copies itself — the full value when the title holds it.
document.addEventListener("click", function (e) {
  var h = e.target.closest ? e.target.closest(".hash") : null;
  if (!h || !navigator.clipboard) return;
  var full = h.getAttribute("title") || h.textContent;
  navigator.clipboard.writeText(full).then(function () {
    var t = h.textContent;
    h.textContent = "copied";
    setTimeout(function () { h.textContent = t; }, 900);
  }).catch(function () {});
});

// "/" focuses the chat input, like any good tool.
document.addEventListener("keydown", function (e) {
  if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey) return;
  var el = document.activeElement;
  if (el && /INPUT|TEXTAREA|SELECT/.test(el.tagName)) return;
  var box = document.querySelector('.chat .bar input[type=text]');
  if (box) { e.preventDefault(); box.focus(); }
});
