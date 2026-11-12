// copy.js puts a file's or a command's text on the clipboard. The buttons stay hidden
// without it (or without the clipboard API); the text can still be selected by hand.
(function () {
  "use strict";
  if (!window.isSecureContext || !navigator.clipboard || !navigator.clipboard.writeText) return;
  document.querySelectorAll("button[data-copy]").forEach(function (btn) {
    var src = document.getElementById(btn.getAttribute("data-copy"));
    var label = btn.querySelector("[data-copy-label]");
    if (!src || !label) return;
    var text = label.textContent;
    var timer = 0;
    var say = function (msg) {
      label.textContent = msg;
      clearTimeout(timer);
      timer = setTimeout(function () { label.textContent = text; }, 2000);
    };
    btn.hidden = false;
    btn.addEventListener("click", function () {
      navigator.clipboard.writeText(src.textContent).then(function () { say("Kopyalandı"); }, function () { say("Kopyalanamadı"); });
    });
  });
})();
