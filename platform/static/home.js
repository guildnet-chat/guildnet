// SPDX-License-Identifier: AGPL-3.0-only

// The create-a-network form starts with the browser's own timezone, which
// is usually the company's. Without JavaScript the field is simply empty.
(function () {
  var field = document.querySelector("[data-guess-timezone]");
  if (!field || field.value) return;
  try {
    field.value = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch (e) {}
})();
