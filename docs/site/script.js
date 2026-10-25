// onet site: copy buttons + footer year. No framework, no dependencies.
(function () {
  "use strict";

  function legacyCopy(text) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-1000px";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (err) {
      ok = false;
    }
    document.body.removeChild(ta);
    return ok;
  }

  function copyText(text) {
    if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(function () {
        return true;
      }).catch(function () {
        return legacyCopy(text);
      });
    }
    return Promise.resolve(legacyCopy(text));
  }

  function attach(el) {
    var wrap = el.closest(".copy-wrap");
    if (!wrap) return;
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy";
    btn.textContent = "copy";
    btn.setAttribute("aria-label", "Copy to clipboard");
    var timer = null;

    btn.addEventListener("click", function () {
      var text = el.innerText.trim();
      copyText(text).then(function (ok) {
        btn.textContent = ok ? "copied" : "failed";
        btn.setAttribute("aria-label", ok ? "Copied" : "Copy failed");
        clearTimeout(timer);
        timer = setTimeout(function () {
          btn.textContent = "copy";
          btn.setAttribute("aria-label", "Copy to clipboard");
        }, 1800);
      });
    });

    wrap.appendChild(btn);
  }

  var targets = document.querySelectorAll("[data-copy]");
  for (var i = 0; i < targets.length; i++) attach(targets[i]);

  var year = String(new Date().getFullYear());
  var slots = document.querySelectorAll("[data-year]");
  for (var j = 0; j < slots.length; j++) slots[j].textContent = year;
})();
