// SPDX-License-Identifier: AGPL-3.0-only

// Some settings only matter for agents GuildNet runs: when a form's "Works
// at" is outside GuildNet (the agent's own program picks its model and
// spends nothing through GuildNet), whatever is marked data-not-outside
// (the model, the effort, the daily budget) is turned off, and so isn't
// sent. Listens on the document, so it works on pages swapped in without a
// reload too.
(function () {
  function update(select) {
    if (!select.form) return;
    var outside = select.value === "outside";
    select.form.querySelectorAll("[data-not-outside]").forEach(function (label) {
      label.classList.toggle("off", outside);
      label.querySelectorAll("select, input").forEach(function (field) { field.disabled = outside; });
    });
  }
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (t instanceof HTMLSelectElement && t.name === "platform") update(t);
  });
})();

// A form on its way: the button pressed shows it ("Saving…" for a Save
// button, a spinner on any) and can't be pressed again. The page that comes
// back replaces it, and says "Saved." if it went through; if none comes
// back, the button is itself again. Boosted forms go when htmx sends them
// (after any "are you sure?"); others when they're submitted.
(function () {
  var pressed = null; // the button that submitted a form last

  function formOf(el) {
    return el instanceof HTMLFormElement ? el : el && el.closest ? el.closest("form") : null;
  }

  function busy(form, button) {
    button = button && form.contains(button) ? button : form.querySelector('button:not([type="button"]), input[type="submit"]');
    if (!button || button.classList.contains("busy")) return;
    button.classList.add("busy");
    button.setAttribute("aria-busy", "true");
    if (button.tagName === "BUTTON" && /^save\b/i.test(button.textContent.trim())) {
      button.dataset.label = button.textContent;
      button.textContent = "Saving…";
    }
    setTimeout(function () { idle(form); }, 20000);
  }

  function idle(form) {
    form.querySelectorAll(".busy").forEach(function (button) {
      button.classList.remove("busy");
      button.removeAttribute("aria-busy");
      if (button.dataset.label) {
        button.textContent = button.dataset.label;
        delete button.dataset.label;
      }
    });
  }

  document.addEventListener("submit", function (e) {
    pressed = e.submitter || null;
    var form = formOf(e.target);
    if (form && form.closest('[hx-boost="false"]')) busy(form, pressed);
  }, true);
  document.addEventListener("htmx:before:request", function (e) {
    var form = formOf(e.target);
    if (form) busy(form, pressed);
  });
  document.addEventListener("htmx:finally:request", function (e) {
    var form = formOf(e.target);
    if (form) idle(form);
  });
  // Back to a page from the browser's history: nothing's on its way.
  window.addEventListener("pageshow", function () {
    document.querySelectorAll("form").forEach(idle);
  });
})();

// On a phone, the console's menu opens from the button at the top, and
// closes on Escape or another press. The page a link brings swaps in a new
// sidebar, folded again.
(function () {
  function set(open) {
    var nav = document.getElementById("nav");
    var button = nav && nav.querySelector("[data-menu-toggle]");
    if (!button) return;
    nav.classList.toggle("open", open);
    button.setAttribute("aria-expanded", open ? "true" : "false");
  }
  document.addEventListener("click", function (e) {
    if (e.target.closest && e.target.closest("[data-menu-toggle]")) {
      var nav = document.getElementById("nav");
      set(!(nav && nav.classList.contains("open")));
    }
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") set(false);
  });
})();

// The console is loaded afresh where a click was going when the page it
// brings needs what this one wasn't loaded with: it's a newer GuildNet (a
// console left open since before a deploy; its version, on #page), or it
// has the chat docked beside it and this one hadn't, or the other way round
// (two-step sign-in just set up, which web chat waits for). Pages swap
// without reloading the dock, so it would otherwise never come.
(function () {
  var page = document.getElementById("page");
  var loaded = page && page.dataset.version;
  if (!loaded) return;
  var chatDock = page.dataset.chatDock || "";
  document.addEventListener("htmx:after:swap", function () {
    var now = document.getElementById("page");
    if (!now) return;
    if ((now.dataset.version && now.dataset.version !== loaded) || (now.dataset.chatDock || "") !== chatDock) {
      window.location.reload();
    }
  });
})();
