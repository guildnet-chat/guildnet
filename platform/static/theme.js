// SPDX-License-Identifier: AGPL-3.0-only

// Light or dark: whatever the system prefers, unless someone flips the switch
// on a page, which is remembered in this browser only. Loaded in <head>, not
// deferred, so the page never flashes the wrong theme.
(function () {
  var root = document.documentElement;
  var key = "guildnet-theme";
  try {
    var saved = localStorage.getItem(key);
    if (saved === "light" || saved === "dark") root.dataset.theme = saved;
  } catch (e) {}

  function current() {
    if (root.dataset.theme) return root.dataset.theme;
    return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }

  // A flip in another page of this site (the console around the chat app,
  // or another tab) applies here too.
  window.addEventListener("storage", function (e) {
    if (e.key !== key) return;
    if (e.newValue === "light" || e.newValue === "dark") root.dataset.theme = e.newValue;
    else delete root.dataset.theme;
  });

  // Listening on the document keeps the switch working after htmx swaps.
  document.addEventListener("click", function (e) {
    var t = e.target;
    if (!t.closest || !t.closest("[data-theme-toggle]")) return;
    var next = current() === "dark" ? "light" : "dark";
    root.dataset.theme = next;
    try { localStorage.setItem(key, next); } catch (e) {}
  });
})();
