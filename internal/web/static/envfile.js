// envfile.js adds a .env file to an environment textarea. The file is read in the
// browser and sent with the form like typed text; nothing else is uploaded.
(function () {
  "use strict";
  var MAX = 256 * 1024;
  var reVar = /^\s*(export\s+)?[A-Za-z_][A-Za-z0-9_]*\s*=/;
  document.querySelectorAll("[data-env-file]").forEach(function (box) {
    var input = box.querySelector("input[type=file]");
    var target = document.getElementById(box.getAttribute("data-env-file"));
    var msg = box.querySelector("[data-env-msg]");
    if (!input || !target || !window.File || !File.prototype.text) return;
    box.hidden = false;
    input.addEventListener("change", function () {
      var f = input.files && input.files[0];
      if (!f) return;
      if (f.size > MAX) {
        msg.textContent = f.name + " çok büyük (en fazla 256 KB).";
        input.value = "";
        return;
      }
      f.text().then(function (text) {
        var cur = target.value.replace(/\s+$/, "");
        target.value = cur ? cur + "\n" + text : text;
        var n = text.split(/\r?\n/).filter(function (l) { return reVar.test(l); }).length;
        msg.textContent = f.name + " eklendi: " + n + " değişken. Aynı ad iki kez olursa form söyler.";
        input.value = "";
      }, function () {
        msg.textContent = f.name + " okunamadı.";
      });
    });
  });
})();
