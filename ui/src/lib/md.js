// Escapes everything first and emits a fixed set of tags, so the output is safe for
// dangerouslySetInnerHTML. The XSS cases live in md_check.mjs.

export function esc(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

const TOKEN = /%%KSCODE(\d+)%%/g;

function inline(s) {
  // s is already escaped. Code spans are lifted out so their text is not formatted.
  const codes = [];
  s = s.replace(/`([^`]+)`/g, (_, c) => {
    codes.push(c);
    return '%%KSCODE' + (codes.length - 1) + '%%';
  });
  s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/(^|[^*])\*([^*\s][^*]*)\*/g, '$1<em>$2</em>');
  s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)"'<>]+)\)/g, (_, t, u) => {
    return '<a href="' + u + '" target="_blank" rel="noopener noreferrer">' + t + '</a>';
  });
  return s.replace(TOKEN, (m, i) => (i < codes.length ? '<code>' + codes[+i] + '</code>' : m));
}

function cells(r) {
  return r
    .trim()
    .replace(/^\||\|$/g, '')
    .split('|')
    .map(c => c.trim());
}

function table(rows) {
  const head = cells(rows[0]);
  let body = rows.slice(1);
  if (body.length && /^[\s|:-]+$/.test(body[0])) body = body.slice(1);
  let t = '<div class="table-wrap"><table class="table"><thead><tr>';
  head.forEach(c => (t += '<th>' + inline(esc(c)) + '</th>'));
  t += '</tr></thead><tbody>';
  body.forEach(r => {
    t += '<tr>';
    cells(r).forEach(c => (t += '<td>' + inline(esc(c)) + '</td>'));
    t += '</tr>';
  });
  return t + '</tbody></table></div>';
}

export function render(src) {
  const lines = String(src == null ? '' : src)
    .replace(/\r\n?/g, '\n')
    .split('\n');
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (/^```/.test(line)) {
      const buf = [];
      i++;
      while (i < lines.length && !/^```/.test(lines[i])) buf.push(lines[i++]);
      i++;
      out.push('<pre><code>' + esc(buf.join('\n')) + '</code></pre>');
      continue;
    }
    const h = /^(#{1,4})\s+(.*)$/.exec(line);
    if (h) {
      const n = Math.min(h[1].length + 1, 4);
      out.push('<h' + n + '>' + inline(esc(h[2])) + '</h' + n + '>');
      i++;
      continue;
    }
    if (/^>\s?/.test(line)) {
      const q = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) q.push(lines[i++].replace(/^>\s?/, ''));
      out.push('<blockquote>' + inline(esc(q.join(' '))) + '</blockquote>');
      continue;
    }
    if (/^\s*[-*]\s+/.test(line) || /^\s*\d+\.\s+/.test(line)) {
      const ord = /^\s*\d+\./.test(line);
      const re = ord ? /^\s*\d+\.\s+/ : /^\s*[-*]\s+/;
      const items = [];
      while (i < lines.length && re.test(lines[i])) items.push('<li>' + inline(esc(lines[i++].replace(re, ''))) + '</li>');
      out.push((ord ? '<ol>' : '<ul>') + items.join('') + (ord ? '</ol>' : '</ul>'));
      continue;
    }
    if (/^\|.*\|\s*$/.test(line)) {
      const rows = [];
      while (i < lines.length && /^\|.*\|\s*$/.test(lines[i])) rows.push(lines[i++]);
      out.push(table(rows));
      continue;
    }
    if (/^\s*$/.test(line)) {
      i++;
      continue;
    }
    const p = [];
    while (i < lines.length && !/^\s*$/.test(lines[i]) && !/^(```|#{1,4}\s|>|\s*[-*]\s|\s*\d+\.\s|\|)/.test(lines[i])) {
      p.push(lines[i++]);
    }
    if (!p.length) p.push(lines[i++]);
    out.push('<p>' + inline(esc(p.join('\n'))).replace(/\n/g, '<br>') + '</p>');
  }
  return out.join('\n');
}
