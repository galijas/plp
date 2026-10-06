// Theme selector: System (default), Dark or Light, remembered per browser.
// Loaded without defer from <head> so the right theme applies before the
// page paints.
(function () {
  var KEY = 'plportal-theme';
  var CHOICES = { system: 'System', dark: 'Dark', light: 'Light' };
  var media = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;

  function choice() {
    try {
      var v = localStorage.getItem(KEY);
      if (CHOICES[v]) return v;
    } catch (e) { /* storage blocked: default */ }
    return 'system';
  }

  function apply() {
    var c = choice();
    var dark = c === 'dark' || (c === 'system' && media && media.matches);
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
    var label = document.getElementById('theme-label');
    if (label) label.textContent = CHOICES[c];
    document.querySelectorAll('#theme-menu [data-theme-choice]').forEach(function (b) {
      b.setAttribute('aria-checked', b.dataset.themeChoice === c ? 'true' : 'false');
    });
  }

  apply();
  if (media) {
    var onChange = function () { if (choice() === 'system') apply(); };
    if (media.addEventListener) media.addEventListener('change', onChange);
    else if (media.addListener) media.addListener(onChange);
  }

  document.addEventListener('DOMContentLoaded', function () {
    var btn = document.getElementById('theme-btn');
    var menu = document.getElementById('theme-menu');
    if (!btn || !menu) return;
    apply();
    function close(focusBtn) {
      menu.hidden = true;
      btn.setAttribute('aria-expanded', 'false');
      if (focusBtn) btn.focus();
    }
    function open() {
      menu.hidden = false;
      btn.setAttribute('aria-expanded', 'true');
      var cur = menu.querySelector('[aria-checked="true"]') || menu.querySelector('button');
      if (cur) cur.focus();
    }
    btn.addEventListener('click', function () { if (menu.hidden) open(); else close(false); });
    menu.addEventListener('click', function (e) {
      var b = e.target.closest('[data-theme-choice]');
      if (!b) return;
      try { localStorage.setItem(KEY, b.dataset.themeChoice); } catch (err) { /* applies for this page only */ }
      apply();
      close(true);
    });
    menu.addEventListener('keydown', function (e) {
      var items = Array.prototype.slice.call(menu.querySelectorAll('button'));
      var i = items.indexOf(document.activeElement);
      if (e.key === 'ArrowDown') { e.preventDefault(); items[(i + 1) % items.length].focus(); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); items[(i - 1 + items.length) % items.length].focus(); }
      else if (e.key === 'Escape') { close(true); }
    });
    document.addEventListener('click', function (e) {
      if (!menu.hidden && !e.target.closest('.theme')) close(false);
    });
  });
})();
