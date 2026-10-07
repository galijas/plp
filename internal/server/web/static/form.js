// Submission form.
//
// Clients (guests): answers auto-save as a draft, files upload in chunks as
// soon as they are chosen (resumable after a dropped connection), and
// Submit checks required answers before sending.
//
// Admins: the same form as a preview, plus Edit buttons on the form,
// every section and every item. Edits stay local until "Save form", which
// stores a new form version.
(function () {
  'use strict';

  var root = document.getElementById('form-root');
  var rail = document.getElementById('rail-list');
  var statusEl = document.getElementById('save-status');
  var isAdmin = document.getElementById('app').dataset.role === 'admin';

  var TYPE_LABELS = {
    short: 'Short answer',
    paragraph: 'Paragraph',
    radio: 'Multiple choice (pick one)',
    checkbox: 'Checkboxes (pick any)',
    file: 'File upload',
    info: 'Text only (no answer)'
  };

  var state = {
    form: null,       // the form as shown (admins: the working copy)
    saved: null,      // admins: JSON of the last saved form
    version: 0,
    answers: {},
    uploads: [],      // {id, itemId, name, size, received, complete, file, xhr, status, error}
    maxChunk: 8 << 20,
    maxTotal: 0,
    editing: null,    // admins: id of the open editor ('__form__' for the header)
    changed: {},      // admins: ids edited since the last save
    savePending: false,
    saveTimer: null,
    submitting: false
  };

  // ---------- helpers ----------

  function h(tag, attrs) {
    var el = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        var v = attrs[k];
        if (v === null || v === undefined || v === false) return;
        if (k === 'class') el.className = v;
        else if (k === 'text') el.textContent = v;
        else if (k === 'html') el.innerHTML = v;
        else if (k.slice(0, 2) === 'on') el.addEventListener(k.slice(2), v);
        else if (v === true) el.setAttribute(k, '');
        else el.setAttribute(k, v);
      });
    }
    for (var i = 2; i < arguments.length; i++) {
      var c = arguments[i];
      if (c === null || c === undefined || c === false) continue;
      if (Array.isArray(c)) c.forEach(function (x) { if (x) el.appendChild(typeof x === 'string' ? document.createTextNode(x) : x); });
      else el.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
    }
    return el;
  }

  function esc(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  // Description markup: **bold**, [text](url), "- " list lines, blank
  // lines between paragraphs. Everything is escaped first; only http(s)
  // and mailto links, and the portal's own /guides/ files, become links.
  function inline(text) {
    var s = esc(text);
    s = s.replace(/\[([^\]]+)\]\(((?:[^()\s]|\([^()\s]*\))+)\)/g, function (m, label, url) {
      var raw = url.replace(/&amp;/g, '&');
      if (!/^(https?:\/\/|mailto:|\/guides\/)/i.test(raw)) return m;
      return '<a href="' + url + '" target="_blank" rel="noopener noreferrer">' + label + '</a>';
    });
    s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    return s;
  }

  function markup(text) {
    if (!text) return '';
    var blocks = String(text).replace(/\r\n/g, '\n').split(/\n\s*\n/);
    var out = '';
    blocks.forEach(function (b) {
      var lines = b.split('\n');
      var list = [], para = [];
      var flushPara = function () { if (para.length) { out += '<p>' + para.map(inline).join('<br>') + '</p>'; para = []; } };
      var flushList = function () { if (list.length) { out += '<ul>' + list.map(function (l) { return '<li>' + inline(l) + '</li>'; }).join('') + '</ul>'; list = []; } };
      lines.forEach(function (l) {
        if (/^\s*-\s+/.test(l)) { flushPara(); list.push(l.replace(/^\s*-\s+/, '')); }
        else if (l.trim()) { flushList(); para.push(l); }
      });
      flushPara();
      flushList();
    });
    return out;
  }

  function fmtBytes(n) {
    if (n < 1024) return n + ' B';
    var u = ['KB', 'MB', 'GB', 'TB'], i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
    return n.toFixed(n < 10 ? 1 : 0) + ' ' + u[i];
  }

  function randomId(prefix) {
    var a = new Uint8Array(6);
    crypto.getRandomValues(a);
    return prefix + Array.prototype.map.call(a, function (b) { return b.toString(36); }).join('').slice(0, 10);
  }

  function api(method, url, body) {
    return fetch(url, {
      method: method,
      headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
      credentials: 'same-origin'
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) {
          var err = new Error(data.error || ('The server answered ' + res.status + '.'));
          err.status = res.status;
          err.data = data;
          throw err;
        }
        return data;
      });
    });
  }

  function setStatus(text, isError) {
    statusEl.textContent = text;
    statusEl.classList.toggle('err', !!isError);
  }

  function allItems() {
    var out = [];
    state.form.sections.forEach(function (s) { s.items.forEach(function (it) { out.push(it); }); });
    return out;
  }

  function findItem(id) {
    for (var i = 0; i < state.form.sections.length; i++) {
      var s = state.form.sections[i];
      for (var j = 0; j < s.items.length; j++) if (s.items[j].id === id) return { section: s, si: i, ii: j, item: s.items[j] };
    }
    return null;
  }

  // ---------- answers ----------

  function answer(id) {
    if (!state.answers[id]) state.answers[id] = {};
    return state.answers[id];
  }

  function filesFor(itemId, onlyComplete) {
    return state.uploads.filter(function (u) { return u.itemId === itemId && (!onlyComplete || u.complete); });
  }

  function isAnswered(it) {
    var a = state.answers[it.id] || {};
    switch (it.type) {
      case 'short': case 'paragraph': return !!(a.text && a.text.trim());
      case 'radio': case 'checkbox': return !!((a.choices && a.choices.length) || (a.other && a.other.trim()));
      case 'file': return filesFor(it.id, true).length > 0;
    }
    return true;
  }

  function missingItems() {
    return allItems().filter(function (it) { return it.required && it.type !== 'info' && !isAnswered(it); });
  }

  function changed() {
    updateRail();
    if (isAdmin) return;
    clearTimeout(state.saveTimer);
    state.savePending = true;
    setStatus('Unsaved changes…');
    state.saveTimer = setTimeout(saveDraft, 900);
  }

  function saveDraft() {
    clearTimeout(state.saveTimer);
    if (!state.savePending) return Promise.resolve();
    state.savePending = false;
    setStatus('Saving…');
    return api('PUT', '/api/draft', { answers: state.answers }).then(function (r) {
      setStatus('Draft saved at ' + r.savedAt + '. You can close this page and continue later.');
    }, function (err) {
      state.savePending = true;
      setStatus('Not saved: ' + err.message, true);
      if (err.status !== 401) state.saveTimer = setTimeout(saveDraft, 10000);
    });
  }

  // ---------- rendering ----------

  function render() {
    root.textContent = '';
    var f = state.form;

    if (isAdmin) {
      root.appendChild(h('p', { class: 'notice' },
        'This is the form as clients see it. Use Edit to change the form, its sections and questions; nothing changes for clients until you click Save form. Answers typed here are not saved.'));
    }

    var head = h('header', { class: 'form-head' + (state.changed.__form__ ? ' changed' : '') });
    if (isAdmin && state.editing === '__form__') {
      head.appendChild(formEditor());
    } else {
      if (!isAdmin && state.user) head.appendChild(h('p', { class: 'who-line', text: 'Signed in as ' + state.user + (state.expiresAt ? ' · invite code valid until ' + state.expiresAt : '') }));
      head.appendChild(h('h1', { id: 'form-title', text: f.title }));
      if (f.description) head.appendChild(h('div', { class: 'md', html: markup(f.description) }));
      if (isAdmin) head.appendChild(h('p', null, h('button', { class: 'btn small', type: 'button', onclick: function () { openEditor('__form__'); } }, 'Edit form title and description')));
    }
    root.appendChild(head);

    f.sections.forEach(function (sec, si) {
      root.appendChild(renderSection(sec, si));
    });

    if (isAdmin) {
      root.appendChild(h('div', { class: 'add-row' },
        h('button', { class: 'btn', type: 'button', onclick: addSection }, 'Add a section')));
    }
    if (state.guides) root.appendChild(renderGuides());
    root.appendChild(renderSubmitZone());
    renderRail();
    if (isAdmin) renderAdminBar();
  }

  function renderSection(sec, si) {
    var n = state.form.sections.length;
    var el = h('section', { class: 'section', id: 'sec-' + sec.id, 'aria-labelledby': 'sec-h-' + sec.id });
    var head = h('div', { class: 'section-head' + (isAdmin ? ' has-edit' : '') + (state.changed[sec.id] ? ' changed' : '') });
    if (isAdmin && state.editing === sec.id) {
      head.appendChild(sectionEditor(sec, si));
    } else {
      head.appendChild(h('span', { class: 'step', text: 'Section ' + (si + 1) + ' of ' + n }));
      head.appendChild(h('h2', { id: 'sec-h-' + sec.id, text: sec.title || 'Untitled section' }));
      if (sec.description) head.appendChild(h('div', { class: 'md', html: markup(sec.description) }));
      if (isAdmin) head.appendChild(h('button', { class: 'btn small edit-btn', type: 'button', onclick: function () { openEditor(sec.id); } }, 'Edit'));
    }
    el.appendChild(head);
    sec.items.forEach(function (it) { el.appendChild(renderItem(it)); });
    if (isAdmin) {
      el.appendChild(h('div', { class: 'add-row' },
        h('button', { class: 'btn small', type: 'button', onclick: function () { addItem(sec); } }, 'Add a question to this section')));
    }
    return el;
  }

  function renderItem(it) {
    var wrap = h('div', { class: 'q' + (it.type === 'info' ? ' info' : '') + (isAdmin ? ' has-edit' : '') + (state.changed[it.id] ? ' changed' : ''), id: 'q-' + it.id });
    if (isAdmin && state.editing === it.id) {
      wrap.appendChild(itemEditor(it));
      return wrap;
    }
    var inputId = 'in-' + it.id;
    var titleTag = (it.type === 'short' || it.type === 'paragraph') ? 'label' : 'span';
    var title = h(titleTag, { class: 'q-title', id: 'qt-' + it.id, for: titleTag === 'label' ? inputId : null }, it.title,
      it.required ? h('span', { class: 'req', 'aria-hidden': 'true', title: 'Required' }, '*') : null,
      it.required ? h('span', { class: 'visually-hidden' }, ' (required)') : null);
    wrap.appendChild(title);
    if (it.description) wrap.appendChild(h('div', { class: 'md', id: 'qd-' + it.id, html: markup(it.description) }));
    if (isAdmin) wrap.appendChild(h('button', { class: 'btn small edit-btn', type: 'button', onclick: function () { openEditor(it.id); } }, 'Edit'));

    var a = state.answers[it.id] || {};
    var describedBy = it.description ? 'qd-' + it.id : null;
    switch (it.type) {
      case 'short':
        wrap.appendChild(h('input', {
          type: 'text', id: inputId, value: a.text || '', maxlength: '1000', 'aria-describedby': describedBy,
          required: it.required, oninput: function (e) { answer(it.id).text = e.target.value; clearInvalid(it.id); changed(); }
        }));
        break;
      case 'paragraph':
        var ta = h('textarea', {
          id: inputId, maxlength: '20000', 'aria-describedby': describedBy, required: it.required,
          oninput: function (e) { answer(it.id).text = e.target.value; clearInvalid(it.id); changed(); }
        });
        ta.value = a.text || '';
        wrap.appendChild(ta);
        break;
      case 'radio':
      case 'checkbox':
        wrap.appendChild(renderOptions(it, a));
        break;
      case 'file':
        wrap.appendChild(renderUpload(it));
        break;
    }
    return wrap;
  }

  function renderOptions(it, a) {
    var group = h('div', { class: 'options', role: it.type === 'radio' ? 'radiogroup' : 'group', 'aria-labelledby': 'qt-' + it.id });
    var isRadio = it.type === 'radio';
    var name = 'opt-' + it.id;
    var otherText;
    (it.options || []).forEach(function (opt, i) {
      var checked = (a.choices || []).indexOf(opt) >= 0;
      group.appendChild(h('label', { class: 'check' },
        h('input', {
          type: isRadio ? 'radio' : 'checkbox', name: name, value: opt, checked: checked,
          onchange: function (e) {
            var ans = answer(it.id);
            if (isRadio) { ans.choices = [opt]; ans.other = ''; if (otherText) otherText.value = ''; }
            else {
              ans.choices = (ans.choices || []).filter(function (c) { return c !== opt; });
              if (e.target.checked) ans.choices.push(opt);
            }
            clearInvalid(it.id);
            changed();
          }
        }),
        h('span', { text: opt })));
    });
    if (it.allowOther) {
      var otherBox = h('input', {
        type: isRadio ? 'radio' : 'checkbox', name: name, value: '__other__', checked: !!a.other, 'aria-label': 'Other',
        onchange: function (e) {
          var ans = answer(it.id);
          if (isRadio) ans.choices = [];
          if (!e.target.checked) { ans.other = ''; otherText.value = ''; }
          else otherText.focus();
          changed();
        }
      });
      otherText = h('input', {
        type: 'text', value: a.other || '', maxlength: '1000', placeholder: 'Other', 'aria-label': 'Other answer',
        oninput: function (e) {
          var ans = answer(it.id);
          ans.other = e.target.value;
          if (e.target.value) {
            otherBox.checked = true;
            if (isRadio) { ans.choices = []; }
          }
          clearInvalid(it.id);
          changed();
        }
      });
      group.appendChild(h('div', { class: 'other-row' }, h('label', { class: 'check' }, otherBox, h('span', null, 'Other:')), otherText));
    }
    return group;
  }

  // ---------- uploads ----------

  var ARCHIVES = ['.zip', '.7z', '.rar', '.tar', '.gz', '.tgz', '.bz2', '.xz'];

  // limitsText describes an upload question's limits, naming archive types
  // separately so the list of branding file types stays readable.
  function limitsText(it) {
    var n = it.maxFiles, total = it.maxTotalMB || it.maxFileMB;
    var t = 'Up to ' + n + ' file' + (n > 1 ? 's' : '') + ', ' + it.maxFileMB + ' MB each';
    if (n > 1) t += ', ' + total + ' MB in total';
    var acc = it.accept || [];
    if (!acc.length) return t + ', any file type.';
    var plain = acc.filter(function (e) { return ARCHIVES.indexOf(e) < 0; });
    var arch = acc.filter(function (e) { return ARCHIVES.indexOf(e) >= 0; });
    var types = plain.join(', ');
    if (arch.length) types += (types ? ', or an archive (' : 'archives (') + arch.join(', ') + ')';
    return t + '. File types: ' + types + '.';
  }

  function itemBytes(itemId) {
    return filesFor(itemId).filter(function (u) { return !u.error; }).reduce(function (n, u) { return n + u.size; }, 0);
  }

  function renderUpload(it) {
    var box = h('div', { class: 'upload', id: 'up-' + it.id });
    var input = h('input', { type: 'file', class: 'visually-hidden', multiple: it.maxFiles > 1, accept: (it.accept || []).join(',') || null, tabindex: '-1', 'aria-hidden': 'true',
      onchange: function (e) { addFiles(it, e.target.files); e.target.value = ''; } });
    var drop = h('div', { class: 'drop crop' },
      h('button', { class: 'btn', type: 'button', disabled: isAdmin, 'aria-describedby': 'lim-' + it.id, onclick: function () { input.click(); } }, it.maxFiles > 1 ? 'Add files' : 'Add file'),
      h('div', null, isAdmin ? 'Clients drop files here or use the button.' : 'or drop files here'),
      h('div', { class: 'limits', id: 'lim-' + it.id, text: limitsText(it) }),
      input);
    if (!isAdmin) {
      ['dragenter', 'dragover'].forEach(function (ev) {
        drop.addEventListener(ev, function (e) { e.preventDefault(); drop.classList.add('over'); });
      });
      ['dragleave', 'drop'].forEach(function (ev) {
        drop.addEventListener(ev, function (e) { e.preventDefault(); drop.classList.remove('over'); });
      });
      drop.addEventListener('drop', function (e) { if (e.dataTransfer && e.dataTransfer.files) addFiles(it, e.dataTransfer.files); });
    }
    box.appendChild(drop);
    box.appendChild(h('ul', { class: 'files', id: 'files-' + it.id, 'aria-live': 'polite' }));
    setTimeout(function () { renderFiles(it.id); }, 0);
    return box;
  }

  function renderFiles(itemId) {
    var list = document.getElementById('files-' + itemId);
    if (!list) return;
    list.textContent = '';
    filesFor(itemId).forEach(function (u) {
      var pct = u.size ? Math.floor(100 * u.received / u.size) : 0;
      var meta, metaErr = false;
      if (u.complete) meta = fmtBytes(u.size) + ' · uploaded';
      else if (u.error) { meta = u.error; metaErr = true; }
      else if (u.xhr || u.status) meta = (u.status === 'waiting' ? 'Starting' : (u.status || 'Uploading')) + ' · ' + pct + '% of ' + fmtBytes(u.size);
      else { meta = 'Interrupted at ' + pct + '%. Add the same file again to continue, or remove it.'; metaErr = true; }
      var actions = h('div', { class: 'actions' });
      if (u.error && u.file) actions.appendChild(h('button', { class: 'btn small', type: 'button', onclick: function () { u.error = ''; runUpload(u); } }, 'Retry'));
      actions.appendChild(h('button', { class: 'btn small quiet', type: 'button', 'aria-label': 'Remove ' + u.name, onclick: function () { removeUpload(u); } }, u.complete ? 'Remove' : 'Cancel'));
      var li = h('li', { class: 'file' },
        h('span', { class: 'name', title: u.name, text: u.name }),
        actions,
        h('span', { class: 'meta' + (metaErr ? ' err' : ''), text: meta }));
      if (!u.complete) {
        var bar = h('div', { class: 'bar', role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(pct), 'aria-label': 'Upload of ' + u.name });
        var fill = h('span');
        fill.style.width = pct + '%';
        bar.appendChild(fill);
        li.appendChild(bar);
      }
      list.appendChild(li);
    });
  }

  function addFiles(it, fileList) {
    var files = Array.prototype.slice.call(fileList || []);
    files.forEach(function (file) {
      var ext = (file.name.match(/\.[^.]+$/) || [''])[0].toLowerCase();
      // Same name and size as an interrupted upload: continue that one.
      var resume = filesFor(it.id).filter(function (u) { return !u.complete && !u.xhr && !u.status && u.name === cleanName(file.name) && u.size === file.size; })[0];
      if (resume) { resume.file = file; resume.error = ''; runUpload(resume); return; }
      var problem = '';
      if (it.accept && it.accept.length && it.accept.indexOf(ext) < 0) problem = 'This question accepts ' + it.accept.join(', ') + ' files.';
      else if (file.size === 0) problem = 'The file is empty.';
      else if (file.size > it.maxFileMB * 1048576) problem = 'The file is larger than ' + it.maxFileMB + ' MB.';
      else if (filesFor(it.id).length >= it.maxFiles) problem = 'This question takes at most ' + it.maxFiles + ' file' + (it.maxFiles > 1 ? 's' : '') + '. Remove one to add another.';
      else if (itemBytes(it.id) + file.size > (it.maxTotalMB || it.maxFileMB) * 1048576) problem = 'The files of this question can be at most ' + (it.maxTotalMB || it.maxFileMB) + ' MB together.';
      var u = { id: null, itemId: it.id, name: cleanName(file.name), size: file.size, received: 0, complete: false, file: file, error: problem };
      state.uploads.push(u);
      if (problem) { renderFiles(it.id); return; }
      u.status = 'waiting';
      renderFiles(it.id);
      api('POST', '/api/uploads', { itemId: it.id, name: file.name, size: file.size }).then(function (r) {
        u.id = r.id;
        u.name = r.name;
        runUpload(u);
      }, function (err) {
        u.status = '';
        u.error = err.message;
        u.file = null;
        renderFiles(it.id);
        if (err.status === 401) sessionEnded(err.message);
      });
    });
    clearInvalid(it.id);
  }

  function cleanName(n) {
    n = n.replace(/\\/g, '/');
    return n.slice(n.lastIndexOf('/') + 1);
  }

  // runUpload sends the file in chunks from the server's position. Network
  // errors retry with a growing delay; the position check on the server
  // makes retries safe.
  function runUpload(u) {
    if (!u.id || !u.file || u.complete) return;
    var attempt = 0;
    var item = findItem(u.itemId);
    function next() {
      if (u.cancelled) return;
      if (u.received >= u.size) { finish(); return; }
      var end = Math.min(u.size, u.received + state.maxChunk);
      var xhr = new XMLHttpRequest();
      u.xhr = xhr;
      u.status = 'Uploading';
      xhr.open('PUT', '/api/uploads/' + encodeURIComponent(u.id));
      xhr.setRequestHeader('Upload-Offset', String(u.received));
      xhr.setRequestHeader('Content-Type', 'application/octet-stream');
      var base = u.received;
      xhr.upload.onprogress = function (e) {
        if (!e.lengthComputable) return;
        var shown = base + e.loaded;
        var li = document.getElementById('files-' + u.itemId);
        if (!li) return;
        var pct = Math.floor(100 * shown / u.size);
        var idx = filesFor(u.itemId).indexOf(u);
        var row = li.children[idx];
        if (row) {
          var fill = row.querySelector('.bar span');
          if (fill) fill.style.width = pct + '%';
          var bar = row.querySelector('.bar');
          if (bar) bar.setAttribute('aria-valuenow', String(pct));
          var meta = row.querySelector('.meta');
          if (meta) meta.textContent = 'Uploading · ' + pct + '% of ' + fmtBytes(u.size);
        }
      };
      xhr.onload = function () {
        u.xhr = null;
        var data = {};
        try { data = JSON.parse(xhr.responseText); } catch (e) { /* keep empty */ }
        if (xhr.status === 200 || xhr.status === 409) {
          attempt = 0;
          if (typeof data.received === 'number') u.received = data.received;
          if (data.complete) { finish(); return; }
          next();
        } else if (xhr.status === 401) {
          u.status = '';
          u.error = data.error || 'Your session ended.';
          renderFiles(u.itemId);
          sessionEnded(u.error);
        } else if (xhr.status >= 500 || xhr.status === 400) {
          retry(data.error);
        } else {
          u.status = '';
          u.error = data.error || ('Upload failed (' + xhr.status + ').');
          renderFiles(u.itemId);
        }
      };
      xhr.onerror = function () { u.xhr = null; retry(); };
      xhr.send(u.file.slice(u.received, end));
    }
    function retry(msg) {
      attempt++;
      if (attempt > 8) {
        u.error = (msg ? msg + ' ' : '') + 'The upload stopped after several tries. Check your connection and click Retry.';
        u.status = '';
        renderFiles(u.itemId);
        return;
      }
      var wait = Math.min(30, Math.pow(2, attempt));
      u.status = 'Connection problem, retrying in ' + wait + ' s';
      u.xhr = null;
      renderFiles(u.itemId);
      setTimeout(next, wait * 1000);
    }
    function finish() {
      u.complete = true;
      u.received = u.size;
      u.xhr = null;
      u.file = null;
      u.status = '';
      renderFiles(u.itemId);
      updateRail();
      if (item) clearInvalid(item.item.id);
    }
    renderFiles(u.itemId);
    next();
  }

  function removeUpload(u) {
    u.cancelled = true;
    if (u.xhr) { u.xhr.abort(); u.xhr = null; }
    var drop = function () {
      state.uploads = state.uploads.filter(function (x) { return x !== u; });
      renderFiles(u.itemId);
      updateRail();
    };
    if (!u.id) { drop(); return; }
    api('DELETE', '/api/uploads/' + encodeURIComponent(u.id)).then(drop, function (err) {
      if (err.status === 404) { drop(); return; }
      u.cancelled = false;
      u.error = 'Not removed: ' + err.message;
      renderFiles(u.itemId);
    });
  }

  function uploadsBusy() {
    return state.uploads.some(function (u) { return !u.complete && !u.error && (u.xhr || u.status); });
  }

  function sessionEnded(msg) {
    setStatus(msg + ' ', true);
    var a = h('a', { href: '/' }, 'Sign in again');
    statusEl.appendChild(a);
  }

  // ---------- rail and validation ----------

  function renderRail() {
    rail.textContent = '';
    state.form.sections.forEach(function (sec) {
      rail.appendChild(h('li', null, h('a', { href: '#sec-' + sec.id, id: 'rail-' + sec.id },
        h('span', { class: 'rail-title', text: sec.title || 'Untitled section' }),
        h('span', { class: 'rail-count' }))));
    });
    updateRail();
  }

  function updateRail() {
    state.form.sections.forEach(function (sec) {
      var link = document.getElementById('rail-' + sec.id);
      if (!link) return;
      var req = sec.items.filter(function (it) { return it.required && it.type !== 'info'; });
      var done = req.filter(isAnswered).length;
      var count = link.querySelector('.rail-count');
      if (!req.length) count.textContent = 'Nothing required';
      else count.textContent = done + ' of ' + req.length + ' required answered';
      link.classList.toggle('complete', req.length > 0 && done === req.length);
    });
  }

  function clearInvalid(id) {
    var q = document.getElementById('q-' + id);
    if (!q || !q.classList.contains('invalid')) return;
    q.classList.remove('invalid');
    var e = q.querySelector('.q-error');
    if (e) e.remove();
  }

  function markInvalid(items) {
    items.forEach(function (it) {
      var q = document.getElementById('q-' + it.id);
      if (!q || q.classList.contains('invalid')) return;
      q.classList.add('invalid');
      q.appendChild(h('p', { class: 'q-error error-text', text: it.type === 'file' ? 'Upload at least one file.' : 'This question needs an answer.' }));
    });
  }

  function renderGuides() {
    return h('section', { class: 'guides-zone', 'aria-labelledby': 'guides-h' },
      h('h2', { id: 'guides-h' }, 'Guides'),
      h('p', null, 'Every guide linked in this form, in one zip file, to keep for later.'),
      h('a', { class: 'btn', href: '/guides/all.zip', download: '' }, 'Download All Guides'));
  }

  function renderSubmitZone() {
    var zone = h('section', { class: 'submit-zone', id: 'submit-zone', 'aria-labelledby': 'submit-h' });
    zone.appendChild(h('h2', { id: 'submit-h', class: 'visually-hidden' }, 'Submit'));
    if (isAdmin) {
      zone.appendChild(h('p', null, 'Clients submit their answers and files here. Admins can\'t submit the form.'));
      zone.appendChild(h('button', { class: 'btn primary big', type: 'button', disabled: true }, 'Submit form'));
      return zone;
    }
    zone.appendChild(h('p', null, state.singleUse
      ? 'Check your answers and files, then submit. Your invite code allows one submission.'
      : 'Check your answers and files, then submit.'));
    var btn = h('button', { class: 'btn primary big', type: 'button', id: 'submit-btn', onclick: submit }, 'Submit form');
    zone.appendChild(btn);
    zone.appendChild(h('div', { id: 'submit-msg', role: 'alert' }));
    return zone;
  }

  function showSubmitError(text, items) {
    var box = document.getElementById('submit-msg');
    box.textContent = '';
    box.appendChild(h('p', { class: 'error-text spaced-top', text: text }));
    if (items && items.length) {
      var ul = h('ul', { class: 'missing' });
      items.forEach(function (it) {
        ul.appendChild(h('li', null, typeof it === 'string' ? it : h('a', { href: '#q-' + it.id, text: it.title })));
      });
      box.appendChild(ul);
    }
  }

  function submit() {
    if (state.submitting) return;
    var btn = document.getElementById('submit-btn');
    if (uploadsBusy()) {
      showSubmitError('Wait until all files have finished uploading, then submit.');
      return;
    }
    var failed = state.uploads.filter(function (u) { return !u.complete; });
    if (failed.length) {
      showSubmitError('Some files did not upload. Retry or remove them first:', failed.map(function (u) { return u.name; }));
      return;
    }
    var miss = missingItems();
    if (miss.length) {
      markInvalid(miss);
      showSubmitError('These required questions have no answer yet:', miss);
      var first = document.getElementById('q-' + miss[0].id);
      if (first) first.scrollIntoView({ behavior: 'smooth', block: 'center' });
      return;
    }
    state.submitting = true;
    btn.disabled = true;
    btn.textContent = 'Submitting…';
    document.getElementById('submit-msg').textContent = '';
    clearTimeout(state.saveTimer);
    state.savePending = false;
    api('POST', '/api/submit', { answers: state.answers, version: state.version }).then(function (r) {
      window.removeEventListener('beforeunload', onLeave);
      location.href = r.redirect || '/done';
    }, function (err) {
      state.submitting = false;
      btn.disabled = false;
      btn.textContent = 'Submit form';
      if (err.data && err.data.formChanged) {
        showSubmitError(err.message);
        load(true);
        return;
      }
      if (err.status === 401) { sessionEnded(err.message); }
      showSubmitError(err.message, err.data && err.data.missing);
    });
  }

  function onLeave(e) {
    if (state.savePending || uploadsBusy() || (isAdmin && isDirty())) {
      if (state.savePending) saveDraft();
      e.preventDefault();
      e.returnValue = '';
    }
  }
  window.addEventListener('beforeunload', onLeave);
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'hidden' && state.savePending) saveDraft();
  });

  // ---------- admin editing ----------

  function isDirty() { return state.saved !== null && JSON.stringify(state.form) !== state.saved; }

  function openEditor(id) {
    if (state.editing && state.editing !== id) {
      // Only one editor at a time; an open one is closed without applying.
      state.editing = null;
    }
    state.editing = id;
    render();
    var target = id === '__form__' ? root.querySelector('.form-head .editor') : document.querySelector('#q-' + id + ' .editor, #sec-' + id + ' .editor');
    if (target) {
      target.scrollIntoView({ block: 'nearest' });
      var first = target.querySelector('input, textarea, select');
      if (first) first.focus();
    }
  }

  function closeEditor(id) {
    state.editing = null;
    render();
    var el = document.getElementById(id === '__form__' ? 'form-title' : (findItem(id) ? 'q-' + id : 'sec-' + id));
    if (el) el.scrollIntoView({ block: 'nearest' });
  }

  function field(label, control, hint) {
    var id = control.id || randomId('f');
    control.id = id;
    return h('div', { class: 'field' }, h('label', { for: id }, label), control, hint ? h('span', { class: 'hint', html: hint }) : null);
  }

  var MARKUP_HINT = 'Formatting: <code>**bold**</code>, <code>[link text](https://…)</code>, lines starting with <code>- </code> for a list, an empty line between paragraphs.';

  function textArea(value, rows) {
    var t = h('textarea', { rows: String(rows || 5) });
    t.value = value || '';
    return t;
  }

  function editorButtons(apply, cancel, extra) {
    return h('div', { class: 'buttons' },
      h('button', { class: 'btn primary', type: 'button', onclick: apply }, 'Apply'),
      h('button', { class: 'btn', type: 'button', onclick: cancel }, 'Cancel'),
      h('span', { class: 'grow' }),
      extra);
  }

  function formEditor() {
    var title = h('input', { type: 'text', value: state.form.title, maxlength: '300' });
    var desc = textArea(state.form.description, 8);
    var ed = h('div', { class: 'editor' },
      field('Form title', title),
      field('Description', desc, MARKUP_HINT),
      editorButtons(function () {
        if (!title.value.trim()) { title.focus(); return; }
        state.form.title = title.value.trim();
        state.form.description = desc.value;
        state.changed.__form__ = true;
        closeEditor('__form__');
      }, function () { closeEditor('__form__'); }));
    return ed;
  }

  function sectionEditor(sec, si) {
    var title = h('input', { type: 'text', value: sec.title, maxlength: '300' });
    var desc = textArea(sec.description, 3);
    var n = state.form.sections.length;
    var extra = [
      h('button', { class: 'btn small', type: 'button', disabled: si === 0, onclick: function () { moveSection(si, -1); } }, 'Move up'),
      h('button', { class: 'btn small', type: 'button', disabled: si === n - 1, onclick: function () { moveSection(si, 1); } }, 'Move down'),
      h('button', { class: 'btn small danger', type: 'button', disabled: n === 1, onclick: function () { deleteSection(si); } }, 'Delete section')
    ];
    return h('div', { class: 'editor' },
      field('Section title', title),
      field('Description', desc, MARKUP_HINT),
      editorButtons(function () {
        sec.title = title.value.trim();
        sec.description = desc.value;
        state.changed[sec.id] = true;
        closeEditor(sec.id);
      }, function () { closeEditor(sec.id); }, h('span', { class: 'btn-group' }, extra)));
  }

  function itemEditor(it) {
    var loc = findItem(it.id);
    var title = h('input', { type: 'text', value: it.title, maxlength: '300' });
    var desc = textArea(it.description, 6);
    var type = h('select', null, Object.keys(TYPE_LABELS).map(function (k) {
      return h('option', { value: k, selected: it.type === k }, TYPE_LABELS[k]);
    }));
    var required = h('input', { type: 'checkbox', checked: !!it.required });
    var options = textArea((it.options || []).join('\n'), 4);
    var allowOther = h('input', { type: 'checkbox', checked: !!it.allowOther });
    var maxFiles = h('input', { type: 'number', min: '1', max: '20', value: String(it.maxFiles || 10) });
    var maxMB = h('input', { type: 'number', min: '1', max: '2048', value: String(it.maxFileMB || 10) });
    var maxTotal = h('input', { type: 'number', min: '1', max: '20480', value: String(it.maxTotalMB || 100) });
    var accept = h('input', { type: 'text', value: (it.accept || []).join(', '), placeholder: '.zip, .png, .svg (empty: any type)' });
    var err = h('p', { class: 'error-text', role: 'alert' });

    var reqRow = h('label', { class: 'check spaced' }, required, h('span', null, 'Required: clients must answer before submitting'));
    var optBox = h('div', null,
      field('Options, one per line', options),
      h('label', { class: 'check spaced' }, allowOther, h('span', null, 'Add an "Other" choice with a text field')));
    var fileBox = h('div', null,
      h('div', { class: 'field-row' }, field('Max files', maxFiles), field('Max size per file (MB)', maxMB), field('Max total for this question (MB)', maxTotal)),
      field('Allowed file types', accept, 'Extensions separated by commas. Leave empty to accept any file.'));

    function sync() {
      var t = type.value;
      optBox.hidden = !(t === 'radio' || t === 'checkbox');
      fileBox.hidden = t !== 'file';
      reqRow.hidden = t === 'info';
    }
    type.addEventListener('change', sync);
    sync();

    var s = loc.section, si = loc.si, ii = loc.ii;
    var isFirst = si === 0 && ii === 0;
    var lastSec = state.form.sections.length - 1;
    var isLast = si === lastSec && ii === s.items.length - 1;
    var extra = h('span', { class: 'btn-group' },
      h('button', { class: 'btn small', type: 'button', disabled: isFirst, onclick: function () { moveItem(it.id, -1); } }, 'Move up'),
      h('button', { class: 'btn small', type: 'button', disabled: isLast, onclick: function () { moveItem(it.id, 1); } }, 'Move down'),
      h('button', { class: 'btn small danger', type: 'button', onclick: function () { deleteItem(it.id); } }, 'Delete'));

    return h('div', { class: 'editor' },
      field('Question', title),
      field('Description', desc, MARKUP_HINT),
      field('Type', type),
      reqRow, optBox, fileBox, err,
      editorButtons(function () {
        var t = type.value;
        if (!title.value.trim()) { err.textContent = 'Enter the question text.'; title.focus(); return; }
        var opts = options.value.split('\n').map(function (o) { return o.trim(); }).filter(Boolean);
        if ((t === 'radio' || t === 'checkbox') && !opts.length && !allowOther.checked) {
          err.textContent = 'Add at least one option.'; options.focus(); return;
        }
        var acc = accept.value.split(',').map(function (x) { x = x.trim().toLowerCase(); return x && x[0] !== '.' ? '.' + x : x; }).filter(Boolean);
        if (acc.some(function (x) { return !/^\.[a-z0-9]{1,10}$/.test(x); })) { err.textContent = 'Allowed file types must look like .zip or .png.'; accept.focus(); return; }
        var mf = parseInt(maxFiles.value, 10), mm = parseInt(maxMB.value, 10), mt = parseInt(maxTotal.value, 10);
        if (t === 'file' && (!(mf >= 1 && mf <= 20) || !(mm >= 1 && mm <= 2048))) { err.textContent = 'Files: 1 to 20 files, 1 to 2048 MB each.'; return; }
        if (t === 'file' && !(mt >= mm && mt <= 20480)) { err.textContent = 'The total limit must be at least the per-file limit, and at most 20480 MB.'; return; }
        it.title = title.value.trim();
        it.description = desc.value;
        it.type = t;
        it.required = t !== 'info' && required.checked;
        if (t === 'radio' || t === 'checkbox') { it.options = opts; it.allowOther = allowOther.checked; }
        else { delete it.options; delete it.allowOther; }
        if (t === 'file') { it.maxFiles = mf; it.maxFileMB = mm; it.maxTotalMB = mt; it.accept = acc; }
        else { delete it.maxFiles; delete it.maxFileMB; delete it.maxTotalMB; delete it.accept; }
        state.changed[it.id] = true;
        closeEditor(it.id);
      }, function () {
        if (it.__new) { removeItemRaw(it.id); state.editing = null; render(); return; }
        closeEditor(it.id);
      }, extra));
  }

  function addItem(sec) {
    var it = { id: randomId('q_'), type: 'short', title: 'New question', description: '', required: false, __new: true };
    sec.items.push(it);
    state.changed[it.id] = true;
    openEditor(it.id);
  }

  function addSection() {
    var sec = { id: randomId('s_'), title: 'New section', description: '', items: [] };
    state.form.sections.push(sec);
    state.changed[sec.id] = true;
    openEditor(sec.id);
  }

  function removeItemRaw(id) {
    var loc = findItem(id);
    if (loc) loc.section.items.splice(loc.ii, 1);
  }

  function deleteItem(id) {
    var loc = findItem(id);
    if (!loc) return;
    if (!window.confirm('Delete "' + loc.item.title + '"? Clients\' draft answers to it are dropped when you save. Existing submissions keep their answers.')) return;
    removeItemRaw(id);
    state.editing = null;
    render();
  }

  // Moving past the start or end of a section moves the item into the
  // neighbouring section.
  function moveItem(id, dir) {
    var loc = findItem(id);
    var secs = state.form.sections;
    var items = loc.section.items;
    var item = items.splice(loc.ii, 1)[0];
    var target = loc.ii + dir;
    if (target < 0) secs[loc.si - 1].items.push(item);
    else if (target > items.length) secs[loc.si + 1].items.unshift(item);
    else items.splice(target, 0, item);
    state.changed[id] = true;
    openEditor(id);
  }

  function moveSection(si, dir) {
    var secs = state.form.sections;
    var s = secs.splice(si, 1)[0];
    secs.splice(si + dir, 0, s);
    state.changed[s.id] = true;
    openEditor(s.id);
  }

  function deleteSection(si) {
    var sec = state.form.sections[si];
    var msg = sec.items.length
      ? 'Delete the section "' + sec.title + '" and its ' + sec.items.length + ' question(s)?'
      : 'Delete the section "' + sec.title + '"?';
    if (!window.confirm(msg)) return;
    state.form.sections.splice(si, 1);
    state.editing = null;
    render();
  }

  var adminBar;
  function renderAdminBar() {
    if (!adminBar) {
      adminBar = h('div', { class: 'admin-bar', role: 'region', 'aria-label': 'Form changes' });
      document.body.appendChild(adminBar);
    }
    adminBar.textContent = '';
    var dirty = isDirty();
    adminBar.appendChild(h('span', { class: 'msg', id: 'admin-msg' }, dirty
      ? [h('strong', null, 'Unsaved changes. '), 'Clients see the old form until you save.']
      : 'No unsaved changes. Form version ' + state.version + '.'));
    adminBar.appendChild(h('button', { class: 'btn', type: 'button', disabled: !dirty, onclick: function () {
      if (!window.confirm('Discard all unsaved changes to the form?')) return;
      state.form = JSON.parse(state.saved);
      state.changed = {};
      state.editing = null;
      render();
    } }, 'Discard changes'));
    adminBar.appendChild(h('button', { class: 'btn primary', type: 'button', disabled: !dirty, id: 'save-form', onclick: saveForm }, 'Save form'));
  }

  function cleanForSave(f) {
    var copy = JSON.parse(JSON.stringify(f));
    copy.sections.forEach(function (s) { s.items.forEach(function (it) { delete it.__new; }); });
    return copy;
  }

  function saveForm() {
    if (state.editing) {
      if (!window.confirm('An editor is still open; its changes are not applied yet. Save without them?')) return;
      state.editing = null;
    }
    var btn = document.getElementById('save-form');
    btn.disabled = true;
    btn.textContent = 'Saving…';
    var payload = cleanForSave(state.form);
    api('PUT', '/api/admin/form', { version: state.version, form: payload }).then(function (r) {
      state.version = r.version;
      state.form = payload;
      state.saved = JSON.stringify(payload);
      state.changed = {};
      render();
      document.getElementById('admin-msg').textContent = 'Saved as version ' + r.version + '. Clients see the new form when they next load the page.';
    }, function (err) {
      btn.disabled = false;
      btn.textContent = 'Save form';
      var msg = document.getElementById('admin-msg');
      msg.textContent = '';
      msg.appendChild(h('strong', null, err.message));
    });
  }

  // ---------- load ----------

  function load(keepAnswers) {
    return api('GET', '/api/form').then(function (d) {
      state.form = d.form;
      state.version = d.version;
      state.maxChunk = d.maxChunkBytes || state.maxChunk;
      state.maxTotal = d.maxTotalBytes;
      state.user = d.user;
      state.singleUse = !!d.singleUse;
      state.expiresAt = d.expiresAt || '';
      state.guides = d.guides || 0;
      if (isAdmin) {
        state.saved = JSON.stringify(d.form);
      } else {
        if (!keepAnswers) state.answers = d.answers || {};
        var local = {};
        state.uploads.forEach(function (u) { if (u.id) local[u.id] = u; });
        state.uploads = (d.uploads || []).map(function (u) { return local[u.id] || u; })
          .concat(state.uploads.filter(function (u) { return !u.id && u.error; }));
        if (d.savedAt) setStatus('Draft saved at ' + d.savedAt + '. You can close this page and continue later.');
        else setStatus('Your answers save automatically as you type.');
      }
      render();
      if (keepAnswers) changed();
    }, function (err) {
      root.textContent = '';
      root.appendChild(h('p', { class: 'flash error', text: err.message }));
      if (err.status === 401) root.appendChild(h('p', null, h('a', { class: 'btn', href: '/' }, 'Sign in again')));
    });
  }

  load(false);
})();
