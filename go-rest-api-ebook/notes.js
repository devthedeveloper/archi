/* Tiny syntax highlighter + copy buttons for the ebook.
   No dependencies; runs once on DOMContentLoaded. */
(function () {
  'use strict';

  function esc(s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  var GO_KEYWORDS = new Set(('break case chan const continue default defer else fallthrough for func go goto if ' +
    'import interface map package range return select struct switch type var').split(' '));
  var GO_BUILTINS = new Set(('string bool byte rune error any int int8 int16 int32 int64 uint uint8 uint16 uint32 ' +
    'uint64 uintptr float32 float64 complex64 complex128 true false nil iota make new len cap append copy delete ' +
    'close panic recover print println min max clear').split(' '));

  // one master regex: comment | string | number | word
  var GO_RE = /(\/\/[^\n]*|\/\*[\s\S]*?\*\/)|(`[^`]*`|"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*')|\b(0[xX][0-9a-fA-F_]+|\d[\d_]*(?:\.\d+)?)\b|([A-Za-z_][A-Za-z0-9_]*)/g;

  function highlightGo(src) {
    var out = '', last = 0, m;
    GO_RE.lastIndex = 0;
    while ((m = GO_RE.exec(src)) !== null) {
      out += esc(src.slice(last, m.index));
      last = GO_RE.lastIndex;
      if (m[1]) out += '<span class="tok-c">' + esc(m[1]) + '</span>';
      else if (m[2]) out += '<span class="tok-s">' + esc(m[2]) + '</span>';
      else if (m[3]) out += '<span class="tok-n">' + esc(m[3]) + '</span>';
      else {
        var w = m[4];
        if (GO_KEYWORDS.has(w)) out += '<span class="tok-k">' + w + '</span>';
        else if (GO_BUILTINS.has(w)) out += '<span class="tok-t">' + w + '</span>';
        else if (src.slice(last).charAt(0) === '(') out += '<span class="tok-f">' + w + '</span>';
        else out += w;
      }
    }
    return out + esc(src.slice(last));
  }

  var JSON_RE = /("(?:[^"\\]|\\.)*")(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;
  function highlightJSON(src) {
    var out = '', last = 0, m;
    JSON_RE.lastIndex = 0;
    while ((m = JSON_RE.exec(src)) !== null) {
      out += esc(src.slice(last, m.index));
      last = JSON_RE.lastIndex;
      if (m[1] && m[2]) out += '<span class="tok-prop">' + esc(m[1]) + '</span>' + m[2];
      else if (m[1]) out += '<span class="tok-s">' + esc(m[1]) + '</span>';
      else if (m[3]) out += '<span class="tok-k">' + m[3] + '</span>';
      else out += '<span class="tok-n">' + m[4] + '</span>';
    }
    return out + esc(src.slice(last));
  }

  var SQL_KW = new Set(('select from where insert into values update set delete create table index if not exists ' +
    'primary key autoincrement unique references on cascade default check in and or as join order by limit offset ' +
    'returning conflict do text integer blob real null coalesce sum count over collate nocase asc desc pragma ' +
    'foreign_keys begin commit rollback transaction cast').split(' '));
  var SQL_RE = /(--[^\n]*)|('(?:[^'\\]|\\.)*')|\b(\d+(?:\.\d+)?)\b|([A-Za-z_][A-Za-z0-9_]*)/g;
  function highlightSQL(src) {
    var out = '', last = 0, m;
    SQL_RE.lastIndex = 0;
    while ((m = SQL_RE.exec(src)) !== null) {
      out += esc(src.slice(last, m.index));
      last = SQL_RE.lastIndex;
      if (m[1]) out += '<span class="tok-c">' + esc(m[1]) + '</span>';
      else if (m[2]) out += '<span class="tok-s">' + esc(m[2]) + '</span>';
      else if (m[3]) out += '<span class="tok-n">' + m[3] + '</span>';
      else if (SQL_KW.has(m[4].toLowerCase())) out += '<span class="tok-k">' + m[4] + '</span>';
      else out += m[4];
    }
    return out + esc(src.slice(last));
  }

  // bash: "$ " lines are commands, "#" lines are comments, rest is output
  function highlightBash(src) {
    return src.split('\n').map(function (line) {
      if (/^\$\s/.test(line)) {
        return '<span class="tok-prompt">$</span> ' + esc(line.slice(2));
      }
      if (/^\s*#/.test(line)) return '<span class="tok-c">' + esc(line) + '</span>';
      return '<span class="out">' + esc(line) + '</span>';
    }).join('\n');
  }

  function addCopyButton(card, pre) {
    var btn = document.createElement('button');
    btn.className = 'copy-btn';
    btn.type = 'button';
    btn.textContent = 'copy';
    btn.addEventListener('click', function () {
      navigator.clipboard.writeText(pre.textContent).then(function () {
        btn.textContent = 'copied!';
        setTimeout(function () { btn.textContent = 'copy'; }, 1400);
      });
    });
    card.appendChild(btn);
  }

  document.addEventListener('DOMContentLoaded', function () {
    document.querySelectorAll('pre > code').forEach(function (code) {
      var src = code.textContent.replace(/^\n/, '');
      var cls = code.className || '';
      if (/lang-go/.test(cls)) code.innerHTML = highlightGo(src);
      else if (/lang-json/.test(cls)) code.innerHTML = highlightJSON(src);
      else if (/lang-sql/.test(cls)) code.innerHTML = highlightSQL(src);
      else if (/lang-bash/.test(cls)) code.innerHTML = highlightBash(src);
    });
    document.querySelectorAll('.code-card').forEach(function (card) {
      var pre = card.querySelector('pre');
      if (pre) addCopyButton(card, pre);
    });
  });
})();
