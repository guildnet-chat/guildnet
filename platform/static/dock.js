// SPDX-License-Identifier: AGPL-3.0-only

// The chat docked beside the console: open or hidden, remembered in this
// browser only. It runs in <head>, so the dock starts in its place without
// a flash. Hiding it keeps the chat connected; the Chat page always shows
// it, filling the page.
//
// Where there's room (1100px and up) the dock sits beside the page; below
// that it covers part of the page, so it starts hidden there, and whether
// it's open is remembered separately for each, so opening it on a wide
// screen doesn't cover the page on a narrow one.
(function () {
  var beside = window.matchMedia("(min-width: 1100px)");
  function key() { return beside.matches ? "guildnet-chat" : "guildnet-chat-over"; }
  function apply() {
    var saved = null;
    try { saved = localStorage.getItem(key()); } catch (e) {}
    // Open by default only where there's plenty of room beside the page.
    var open = saved ? saved === "open" : window.matchMedia("(min-width: 1280px)").matches;
    document.documentElement.dataset.chat = open ? "open" : "hidden";
  }
  apply();
  if (beside.addEventListener) beside.addEventListener("change", apply);

  function show(next) {
    document.documentElement.dataset.chat = next;
    try { localStorage.setItem(key(), next); } catch (e) {}
  }
  document.addEventListener("click", function (e) {
    var toggle = e.target.closest && e.target.closest("[data-chat-toggle]");
    if (!toggle) return;
    show(document.documentElement.dataset.chat === "open" ? "hidden" : "open");
  });

  // The chat draws its own Hide, in its header: it asks here, from this
  // site only. It's told whether hiding means anything, which it doesn't on
  // the Chat page, where the chat fills the page.
  window.addEventListener("message", function (e) {
    var frame = document.getElementById("chat-frame");
    if (e.origin !== location.origin || !frame || e.source !== frame.contentWindow) return;
    if (e.data && e.data.type === "guildnet-chat-hide") show("hidden");
  });
  function tell() {
    var frame = document.getElementById("chat-frame");
    if (!frame || !frame.contentWindow) return;
    var wide = !!document.querySelector('#page[data-chat="wide"]');
    frame.contentWindow.postMessage({ type: "guildnet-dock", hideable: !wide }, location.origin);
  }
  document.addEventListener("load", function (e) {
    if (!e.target || e.target.id !== "chat-frame") return;
    // Again a little later, in case the chat was still starting up.
    tell();
    setTimeout(tell, 1000);
    setTimeout(tell, 4000);
  }, true);
  document.addEventListener("htmx:after:swap", tell);
  document.addEventListener("DOMContentLoaded", tell);

  // Its width: drag its left edge, or use the arrow keys on it. Remembered
  // in this browser; kept to what leaves the page room.
  var root = document.documentElement, widthKey = "guildnet-chat-width";
  function setWidth(px, keep) {
    var most = Math.max(320, window.innerWidth - 248 - 360);
    px = Math.round(Math.min(Math.max(px, 320), most));
    root.style.setProperty("--chat-width", px + "px");
    if (keep) { try { localStorage.setItem(widthKey, String(px)); } catch (e) {} }
  }
  try { var saved = parseInt(localStorage.getItem(widthKey), 10); if (saved) setWidth(saved, false); } catch (e) {}

  document.addEventListener("pointerdown", function (e) {
    var handle = e.target.closest && e.target.closest("[data-chat-resize]");
    if (!handle || e.button !== 0) return;
    e.preventDefault();
    var dock = handle.parentElement, right = dock.getBoundingClientRect().right;
    root.classList.add("chat-resizing"); // the chat's frame mustn't swallow the drag
    function move(ev) { setWidth(right - ev.clientX, false); }
    function up(ev) {
      setWidth(right - ev.clientX, true);
      root.classList.remove("chat-resizing");
      document.removeEventListener("pointermove", move);
      document.removeEventListener("pointerup", up);
    }
    document.addEventListener("pointermove", move);
    document.addEventListener("pointerup", up);
  });
  document.addEventListener("keydown", function (e) {
    var handle = e.target.closest && e.target.closest("[data-chat-resize]");
    if (!handle || (e.key !== "ArrowLeft" && e.key !== "ArrowRight")) return;
    e.preventDefault();
    var now = handle.parentElement.getBoundingClientRect().width;
    setWidth(now + (e.key === "ArrowLeft" ? 40 : -40), true);
  });
})();
