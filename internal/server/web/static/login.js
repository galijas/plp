// Sign-in page: switches between client and admin sign-in, and fills the
// fields from an invite link (https://host/#invite=CODE&email=ADDRESS).
(function () {
  var guest = document.getElementById('guest-panel');
  var admin = document.getElementById('admin-panel');

  function show(mode) {
    guest.hidden = mode !== 'guest';
    admin.hidden = mode !== 'admin';
    var panel = mode === 'admin' ? admin : guest;
    var first = panel.querySelector('input:not([value]), input[value=""]') || panel.querySelector('input');
    if (first) first.focus();
  }

  document.getElementById('show-admin').addEventListener('click', function () { show('admin'); });
  document.getElementById('show-guest').addEventListener('click', function () { show('guest'); });

  function fromHash() {
    var hash = location.hash.replace(/^#/, '');
    if (!hash) return;
    if (hash === 'admin') {
      show('admin');
      return;
    }
    var p = new URLSearchParams(hash);
    var code = p.get('invite');
    var email = p.get('email');
    if (!code && !email) return;
    show('guest');
    if (code) document.getElementById('code').value = code;
    if (email) document.getElementById('email').value = email;
    // Keep the code out of the address bar and browser history.
    if (history.replaceState) history.replaceState(null, '', location.pathname);
    var btn = document.querySelector('#guest-form button[type=submit]');
    if (code && email && btn) btn.focus();
    else if (code) document.getElementById('email').focus();
  }
  fromHash();
  window.addEventListener('hashchange', fromHash);
})();
