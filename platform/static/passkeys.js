// SPDX-License-Identifier: AGPL-3.0-only

// Passkeys: adding one on the account page, and using one as the second step
// of signing in. A button with data-passkey="add" or "use" asks the console
// to begin (data-begin), hands the options to the browser, and sends what
// the browser answers to data-finish, which says where to go next. Pages
// swap without reloading, so this listens on the whole document.
(function () {
  "use strict";

  function toBytes(s) {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    var bin = atob(s), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  }

  function toText(buf) {
    var bytes = new Uint8Array(buf), bin = "";
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function forCreate(options) {
    var pk = options.publicKey;
    pk.challenge = toBytes(pk.challenge);
    pk.user.id = toBytes(pk.user.id);
    (pk.excludeCredentials || []).forEach(function (c) { c.id = toBytes(c.id); });
    return { publicKey: pk };
  }

  function forGet(options) {
    var pk = options.publicKey;
    pk.challenge = toBytes(pk.challenge);
    (pk.allowCredentials || []).forEach(function (c) { c.id = toBytes(c.id); });
    return { publicKey: pk };
  }

  function answer(cred) {
    var r = cred.response, out = { id: cred.id, rawId: toText(cred.rawId), type: cred.type, response: { clientDataJSON: toText(r.clientDataJSON) } };
    if (r.attestationObject) {
      out.response.attestationObject = toText(r.attestationObject);
      if (r.getTransports) out.response.transports = r.getTransports();
    } else {
      out.response.authenticatorData = toText(r.authenticatorData);
      out.response.signature = toText(r.signature);
      if (r.userHandle) out.response.userHandle = toText(r.userHandle);
    }
    return out;
  }

  function call(url, body) {
    var csrf = document.querySelector("input[name=csrf]");
    return fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf ? csrf.value : "" },
      body: body ? JSON.stringify(body) : null,
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) throw new Error(data.error || "That didn't work. Try again.");
        return data;
      });
    });
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest && event.target.closest("[data-passkey]");
    if (!button) return;
    event.preventDefault();
    var box = button.closest(".passkey-add") || button.parentElement;
    var status = box.querySelector(".passkey-status");
    var say = function (msg) { if (status) status.textContent = msg; };
    if (!window.PublicKeyCredential || !navigator.credentials) {
      say("This browser can't use passkeys. Use a code instead.");
      return;
    }
    var adding = button.dataset.passkey === "add";
    var name = box.querySelector("input[name=passkey-name]");
    button.disabled = true;
    say("");
    call(button.dataset.begin)
      .then(function (options) {
        return adding ? navigator.credentials.create(forCreate(options)) : navigator.credentials.get(forGet(options));
      })
      .then(function (cred) {
        var url = button.dataset.finish;
        if (adding) url += "?name=" + encodeURIComponent(name && name.value.trim() || "Passkey");
        return call(url, answer(cred));
      })
      .then(function (done) { window.location.assign(done.to); })
      .catch(function (err) {
        say(err && err.name === "NotAllowedError" ? "Cancelled, or the passkey didn't answer. Try again." :
          err && err.name === "InvalidStateError" ? "That passkey is already added." :
          err && err.name === "SecurityError" ? "Passkeys work only at this network's own address. Open the console there." :
          (err && err.message) || "That didn't work. Try again.");
      })
      .finally(function () { button.disabled = false; });
  });
})();
