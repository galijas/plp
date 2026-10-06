// Admin panel helpers: copy buttons, edit dialogs, confirmations, invite
// code generation and the "No specific email" switch.
(function () {
  var CODE_ALPHABET = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';

  function newCode() {
    var buf = new Uint32Array(16);
    crypto.getRandomValues(buf);
    var out = '';
    for (var i = 0; i < 16; i++) {
      if (i && i % 4 === 0) out += '-';
      out += CODE_ALPHABET[buf[i] % CODE_ALPHABET.length];
    }
    return out;
  }

  function copy(text, btn) {
    var done = function () {
      var old = btn.textContent;
      btn.textContent = 'Copied';
      setTimeout(function () { btn.textContent = old; }, 1500);
    };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done, function () { fallback(text); done(); });
    } else {
      fallback(text);
      done();
    }
  }

  function fallback(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) { window.prompt('Copy this:', text); }
    ta.remove();
  }

  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-copy]');
    if (b) { copy(b.dataset.copy, b); return; }
    b = e.target.closest('[data-open]');
    if (b) {
      var d = document.getElementById(b.dataset.open);
      if (d && d.showModal) d.showModal();
      return;
    }
    b = e.target.closest('[data-close]');
    if (b) {
      var dlg = b.closest('dialog');
      if (dlg) dlg.close();
      return;
    }
    b = e.target.closest('[data-generate]');
    if (b) {
      var input = document.getElementById(b.dataset.generate);
      if (input) { input.value = newCode(); input.focus(); }
    }
  });

  document.addEventListener('submit', function (e) {
    var msg = e.target.dataset && e.target.dataset.confirm;
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

  // "No specific email" disables the email field of the same form.
  function syncEmail(box) {
    var form = box.closest('form');
    var input = form && form.querySelector('[data-email-input]');
    if (!input) return;
    input.disabled = box.checked;
    input.required = !box.checked;
  }
  document.querySelectorAll('[data-any-email]').forEach(function (box) {
    syncEmail(box);
    box.addEventListener('change', function () { syncEmail(box); });
  });

  // SMTP: picking a security mode suggests its usual port, unless the
  // admin typed a different one.
  var port = document.getElementById('s-port');
  if (port) {
    document.querySelectorAll('input[name="security"][data-port]').forEach(function (r) {
      r.addEventListener('change', function () {
        if (['25', '465', '587', ''].indexOf(port.value) >= 0) port.value = r.dataset.port;
      });
    });
  }

  // Clicking the invite link field selects the whole link.
  document.querySelectorAll('[data-select-all]').forEach(function (el) {
    el.addEventListener('focus', function () { el.select(); });
  });

  // Bring a just-created or just-edited invite into view.
  var hl = document.querySelector('section[data-hl]') || document.querySelector('tr.hl');
  if (hl) hl.scrollIntoView({ block: 'center' });
})();
