(() => {
'use strict';

const PRESETS = [
  { id: 'openai', name: 'OpenAI', kind: 'openai', base: 'https://api.openai.com/v1' },
  { id: 'anthropic', name: 'Anthropic', kind: 'anthropic', base: 'https://api.anthropic.com/v1' },
  { id: 'gemini', name: 'Google Gemini', kind: 'openai', base: 'https://generativelanguage.googleapis.com/v1beta/openai' },
  { id: 'nvidia', name: 'NVIDIA NIM', kind: 'openai', base: 'https://integrate.api.nvidia.com/v1' },
  { id: 'agentrouter', name: 'AgentRouter', kind: 'anthropic', base: 'https://agentrouter.org/v1' },
  { id: 'openrouter', name: 'OpenRouter', kind: 'openai', base: 'https://openrouter.ai/api/v1' },
  { id: 'groq', name: 'Groq', kind: 'openai', base: 'https://api.groq.com/openai/v1' },
  { id: 'together', name: 'Together AI', kind: 'openai', base: 'https://api.together.xyz/v1' },
  { id: 'mistral', name: 'Mistral', kind: 'openai', base: 'https://api.mistral.ai/v1' },
  { id: 'deepseek', name: 'DeepSeek', kind: 'openai', base: 'https://api.deepseek.com/v1' },
  { id: 'xai', name: 'xAI', kind: 'openai', base: 'https://api.x.ai/v1' },
  { id: 'cerebras', name: 'Cerebras', kind: 'openai', base: 'https://api.cerebras.ai/v1' },
  { id: 'fireworks', name: 'Fireworks', kind: 'openai', base: 'https://api.fireworks.ai/inference/v1' },
  { id: 'perplexity', name: 'Perplexity', kind: 'openai', base: 'https://api.perplexity.ai' },
  { id: 'ollama', name: 'Ollama (local)', kind: 'openai', base: 'http://localhost:11434/v1' },
  { id: 'custom', name: 'Custom', kind: 'custom', base: '', auth_header: 'Authorization', auth_prefix: 'Bearer ' },
];

const SINK_PRESETS = [
  { id: 'logflare', name: 'Logflare', type: 'logflare', url: 'https://api.logflare.app' },
  { id: 'webhook', name: 'Webhook', type: 'webhook', url: '' },
];

const $app = document.getElementById('app');
let S = null;            // server state
let logs = [];
let page = 'providers';
let revealed = null;     // freshly created token, shown once
let chatMsgs = [];
let chatModel = '';
let pollTimer = null;
let actFilter = { provider: '', errors: false };

/* ---------- tiny DOM helpers ---------- */
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const c of kids.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return el;
}
const NS = 'http://www.w3.org/2000/svg';
function svg(tag, attrs, ...kids) {
  const el = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, v);
  for (const c of kids.flat()) if (c) el.append(c.nodeType ? c : document.createTextNode(c));
  return el;
}
function mascot() {
  const ink = '#1b1340';
  const st = `stroke:${ink};stroke-width:3;stroke-linecap:round;stroke-linejoin:round`;
  const eye = cx => [svg('circle', { cx, cy: 31, r: 9, style: `fill:#fff;stroke:${ink};stroke-width:2.5` }),
    svg('circle', { cx, cy: 31, r: 4.2, class: 'pupil', style: `fill:${ink}` })];
  return svg('svg', { viewBox: '0 0 64 64', class: 'mascot', 'aria-hidden': 'true' },
    svg('g', { class: 'ant' }, svg('path', { d: 'M20 15L13 5M44 15L51 5', style: st + ';fill:none' }),
      svg('circle', { cx: 12, cy: 4.5, r: 3.6, style: `fill:#ff6fb5;stroke:${ink};stroke-width:2.5` }),
      svg('circle', { cx: 52, cy: 4.5, r: 3.6, style: `fill:#62b6ff;stroke:${ink};stroke-width:2.5` })),
    svg('rect', { x: 4, y: 14, width: 56, height: 42, rx: 15, style: `fill:#ffd93d;stroke:${ink};stroke-width:3` }),
    eye(23), eye(41),
    svg('path', { d: 'M26 43q6 5.5 12 0', style: st + ';fill:none' }),
    svg('circle', { cx: 14, cy: 49, r: 1.8, style: 'fill:#3dd997' }),
    svg('circle', { cx: 50, cy: 49, r: 1.8, style: 'fill:#ff6fb5' }));
}
// googly eyes: pupils chase the cursor
document.addEventListener('mousemove', e => {
  document.querySelectorAll('.mascot').forEach(m => {
    const k = m.getBoundingClientRect().width / 64;
    m.querySelectorAll('.pupil').forEach(p => {
      const eye = p.previousSibling.getBoundingClientRect();
      const dx = e.clientX - (eye.left + eye.width / 2), dy = e.clientY - (eye.top + eye.height / 2);
      const d = Math.hypot(dx, dy) || 1, lim = Math.min(4.2 * k, d);
      p.style.transform = `translate(${(dx / d) * lim / k}px, ${(dy / d) * lim / k}px)`;
    });
  });
});

function toast(msg, bad) {
  document.querySelectorAll('.toast').forEach(t => t.remove());
  const t = h('div', { class: 'toast' + (bad ? ' bad' : ''), role: 'status' }, msg);
  if (bad) document.querySelectorAll('.mascot').forEach(m => { m.classList.remove('dizzy'); void m.getBoundingClientRect(); m.classList.add('dizzy'); });
  document.body.append(t);
  setTimeout(() => t.remove(), bad ? 4500 : 2200);
}

async function copy(text, msg) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = h('textarea', { style: 'position:fixed;opacity:0' }, text);
    document.body.append(ta); ta.select();
    try { document.execCommand('copy'); } catch {}
    ta.remove();
  }
  toast(msg || 'Copied');
}

function ago(ts) {
  if (!ts) return 'never';
  const s = Math.max(0, Date.now() / 1000 - ts);
  if (s < 45) return 'just now';
  if (s < 3600) return Math.round(s / 60) + ' min ago';
  if (s < 86400) return Math.round(s / 3600) + ' h ago';
  return Math.round(s / 86400) + ' d ago';
}
const fmt = n => n >= 10000 ? (n / 1000).toFixed(n >= 100000 ? 0 : 1) + 'k' : String(n);

/* ---------- api ---------- */
async function api(method, path, body) {
  const res = await fetch('/api' + path, {
    method,
    headers: { 'Content-Type': 'application/json', 'X-Wackrouter': '1' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 401 && path !== '/login' && path !== '/password') { showLogin(); throw new Error('Sign in again'); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}
async function act(fn, okMsg) {
  try { await fn(); if (okMsg) toast(okMsg); await refresh(); }
  catch (e) { toast(e.message, true); }
}
async function refresh() { S = await api('GET', '/state'); render(); }

/* ---------- shell ---------- */
const NAV = [
  ['providers', 'Providers', () => S.providers.length],
  ['access', 'Access', () => S.tokens.length],
  ['playground', 'Playground'],
  ['activity', 'Activity'],
  ['drains', 'Drains', () => S.sinks.length],
  ['settings', 'Settings'],
];

function render() {
  clearInterval(pollTimer);
  const views = { providers: pageProviders, access: pageAccess, playground: pagePlayground, activity: pageActivity, drains: pageDrains, settings: pageSettings };
  const view = (views[page] || pageProviders)();
  $app.replaceChildren(h('div', { class: 'shell' },
    h('aside', { class: 'rail' },
      h('div', { class: 'brand' }, mascot(), 'Wackrouter'),
      h('nav', { class: 'nav', 'aria-label': 'Main' }, NAV.map(([id, label, count]) =>
        h('a', { href: '#' + id, 'aria-current': page === id ? 'page' : null }, label,
          count ? h('span', { class: 'count' }, count()) : null))),
    ),
    h('main', null, view)));
  document.querySelector('.rail').append(h('div', { class: 'foot' }, 'Wack responsibly.'));
}

function go() {
  page = location.hash.slice(1) || 'providers';
  if (S) render();
}
window.addEventListener('hashchange', go);

/* ---------- login ---------- */
function showLogin() {
  S = null;
  clearInterval(pollTimer);
  const pw = h('input', { type: 'password', id: 'pw', autocomplete: 'current-password', required: true, autofocus: true });
  const form = h('form', {
    onsubmit: async e => {
      e.preventDefault();
      try { await api('POST', '/login', { password: pw.value }); await boot(); }
      catch (err) { toast(err.message, true); form.classList.remove('nope'); void form.offsetWidth; form.classList.add('nope'); pw.select(); }
    },
  },
    h('div', { class: 'brand' }, mascot(), 'Wackrouter'),
    h('p', { class: 'muted' }, 'Password, please. First run? It was printed in the server log.'),
    h('label', { class: 'field' }, h('span', null, 'Admin password'), pw),
    h('button', { class: 'btn primary', type: 'submit' }, 'Sign in'));
  $app.replaceChildren(h('div', { class: 'login' }, form));
  pw.focus();
}

/* ---------- providers ---------- */
function keyState(k) {
  if (!k.enabled) return { cls: 'off', chip: ['', 'Off'] };
  if (!k.last_at) return { cls: 'idle', chip: ['', 'Not used yet'] };
  if (k.last_status >= 200 && k.last_status < 400) return { cls: '', chip: ['ok', 'Working'] };
  if (k.last_status === 429) return { cls: 'idle', chip: ['warn', 'Rate limited'] };
  if (k.last_status === 401 || k.last_status === 403) return { cls: 'bad', chip: ['bad', 'Rejected'] };
  if (k.last_status === 0) return { cls: 'bad', chip: ['bad', 'Unreachable'] };
  return { cls: 'bad', chip: ['bad', 'HTTP ' + k.last_status] };
}

function pageProviders() {
  const url = location.origin + '/v1';
  const out = [];
  out.push(h('div', { class: 'page-head' },
    h('div', null, h('h1', null, 'Providers'),
      h('p', { class: 'muted' }, 'Feed it keys. It does the routing. Nobody gets hurt.')),
    h('button', { class: 'btn primary', onclick: () => providerDialog() }, 'Add provider')));
  out.push(h('div', { class: 'endpoint' },
    h('div', { class: 'url' }, url),
    h('button', { class: 'btn', onclick: () => copy(url, 'Address copied') }, 'Copy address'),
    h('p', { class: 'note' }, 'Point every app here. Use it as the base URL in any OpenAI-compatible tool, with an access token as the API key. Name models as provider/model, like openai/gpt-4o.')));

  if (!S.providers.length) {
    out.push(h('div', { class: 'panel empty' },
      mascot(),
      h('h2', null, 'Nothing to route yet'),
      h('p', { class: 'muted' }, 'Pick a provider, hand over a key, and the router is off to the races.'),
      h('div', { class: 'presets' }, PRESETS.slice(0, 8).map(p =>
        h('button', { class: 'btn', onclick: () => providerDialog(null, p) }, p.name)))));
  }
  for (const p of S.providers) out.push(providerRow(p));
  return out;
}

function providerRow(p) {
  const keyForm = (() => {
    const label = h('input', { type: 'text', placeholder: 'Label (optional)', 'aria-label': 'Key label' });
    const key = h('input', { type: 'password', placeholder: 'Paste API key', 'aria-label': 'API key', autocomplete: 'off', required: true });
    return h('form', {
      class: 'addkey',
      onsubmit: e => { e.preventDefault(); act(() => api('POST', `/providers/${p.id}/keys`, { label: label.value, key: key.value }), 'Key added. Yum.'); },
    }, label, key, h('button', { class: 'btn', type: 'submit' }, 'Add key'));
  })();

  const keyRows = p.keys.map(k => {
    const st = keyState(k);
    return h('div', { class: 'keyrow' + (k.enabled ? '' : ' off') },
      h('span', { class: 'label' }, k.label),
      h('span', { class: 'mask' }, '••••••' + k.hint),
      h('span', { class: 'chip ' + st.chip[0], title: k.last_err || (k.last_at ? 'Last request ' + ago(k.last_at) : '') }, st.chip[1]),
      h('button', { class: 'btn quiet', onclick: () => act(() => api('PATCH', `/providers/${p.id}/keys/${k.id}`, { enabled: !k.enabled })) }, k.enabled ? 'Turn off' : 'Turn on'),
      h('button', { class: 'btn quiet danger', onclick: () => { if (confirm(`Remove key "${k.label}"?`)) act(() => api('DELETE', `/providers/${p.id}/keys/${k.id}`), 'Key removed'); } }, 'Remove'));
  });

  const test = h('button', { class: 'btn' }, 'Test');
  test.addEventListener('click', async () => {
    test.disabled = true; test.textContent = 'Testing';
    try {
      const r = await api('POST', `/providers/${p.id}/test`);
      toast(r.ok ? `${p.name} works: ${r.models} models, ${r.ms} ms` : `${p.name} failed: ${r.error}`, !r.ok);
    } catch (e) { toast(e.message, true); }
    test.disabled = false; test.textContent = 'Test';
    refresh();
  });

  return h('section', { class: 'panel prov' + (p.enabled ? '' : ' off') },
    h('div', { class: 'prov-head' },
      h('div', { class: 'tile', 'aria-hidden': 'true' }, p.name.slice(0, 1).toUpperCase()),
      h('div', { class: 'prov-title' },
        h('div', { class: 'name' }, p.name, !p.enabled && h('span', { class: 'chip' }, 'Off'),
          S.default_provider === p.id && h('span', { class: 'chip warn' }, 'Default')),
        h('div', { class: 'sub' }, h('code', null, p.id + '/'), ' on ', p.base_url),
        (p.kind === 'custom' || p.headers.length) ? h('div', { class: 'row small', style: 'margin-top:4px;gap:6px' },
          p.kind === 'custom' && h('span', { class: 'chip' }, 'Key in ' + p.auth_header),
          p.headers.length > 0 && h('span', { class: 'chip', title: p.headers.map(x => x.name).join(', ') }, p.headers.length + ' custom header' + (p.headers.length === 1 ? '' : 's'))) : null),
      h('div', { class: 'bits', title: p.keys.length + ' key' + (p.keys.length === 1 ? '' : 's'), role: 'img', 'aria-label': p.keys.length + ' keys' },
        p.keys.map(k => h('i', { class: keyState(k).cls }))),
      h('div', { class: 'row' },
        test,
        h('button', { class: 'btn quiet', onclick: () => providerDialog(p) }, 'Edit'),
        h('button', { class: 'btn quiet', onclick: () => act(() => api('PUT', `/providers/${p.id}`, { name: p.name, kind: p.kind, base_url: p.base_url, enabled: !p.enabled, auth_header: p.auth_header, auth_prefix: p.auth_prefix })) }, p.enabled ? 'Turn off' : 'Turn on'),
        h('button', { class: 'btn quiet danger', onclick: () => { if (confirm(`Delete ${p.name} and all its keys?`)) act(() => api('DELETE', `/providers/${p.id}`), 'Provider deleted'); } }, 'Delete'))),
    h('div', { class: 'keys' },
      keyRows.length ? keyRows : h('div', { class: 'keyrow muted' }, 'No keys yet. Add one below.'),
      p.keys.length > 1 && h('div', { class: 'keyrow muted small' }, 'Requests rotate across enabled keys and fall back to the next one on rate limits or rejected keys.')),
    keyForm);
}

function slugify(s) { return s.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 32); }

function providerDialog(existing, preset) {
  const editing = !!existing;
  const v = existing || preset || { id: '', name: '', kind: 'openai', base: '' };
  const name = h('input', { type: 'text', value: v.name || '', required: true, autocomplete: 'off' });
  const id = h('input', { type: 'text', value: v.id || '', required: true, pattern: '[a-z0-9][a-z0-9\\-]{0,31}', readonly: editing, autocomplete: 'off' });
  const base = h('input', { type: 'url', value: v.base_url || v.base || '', required: true, placeholder: 'https://api.example.com/v1' });
  const kind = h('select', null,
    h('option', { value: 'openai', selected: v.kind === 'openai' }, 'OpenAI-compatible (Bearer key)'),
    h('option', { value: 'anthropic', selected: v.kind === 'anthropic' }, 'Anthropic (x-api-key)'),
    h('option', { value: 'custom', selected: v.kind === 'custom' }, 'Custom header'));
  const authName = h('input', { type: 'text', class: 'mono', value: v.auth_header || '', placeholder: 'api-key', autocomplete: 'off' });
  const authPrefix = h('input', { type: 'text', class: 'mono', value: v.auth_prefix || '', placeholder: 'Bearer  (optional, keep the trailing space)', autocomplete: 'off' });
  const authBox = h('div', { class: 'row', style: 'align-items:flex-start' },
    h('label', { class: 'field grow' }, h('span', null, 'Header name'), authName),
    h('label', { class: 'field grow' }, h('span', null, 'Text before the key'), authPrefix));
  const syncAuth = () => { authBox.style.display = kind.value === 'custom' ? '' : 'none'; };
  kind.addEventListener('change', syncAuth); syncAuth();

  const hdrs = (existing ? existing.headers : []).map(x => ({ name: x.name, value: '', hint: x.hint, had: true }));
  const hdrBox = h('div');
  const paintHdrs = () => hdrBox.replaceChildren(...hdrs.map((r, i) => h('div', { class: 'alias' },
    h('input', { type: 'text', class: 'mono', placeholder: 'X-Header-Name', value: r.name, 'aria-label': 'Header name', autocomplete: 'off', oninput: e => { r.name = e.target.value; } }),
    h('input', { type: 'password', class: 'mono', placeholder: r.had ? '•••• ' + r.hint + ' (blank keeps it)' : 'value', 'aria-label': 'Header value', autocomplete: 'off', oninput: e => { r.value = e.target.value; } }),
    h('button', { type: 'button', class: 'btn quiet danger', onclick: () => { hdrs.splice(i, 1); paintHdrs(); } }, 'Remove'))));
  paintHdrs();
  const key = h('input', { type: 'password', autocomplete: 'off', placeholder: editing ? 'Add another key (optional)' : 'Paste API key' });
  let idTouched = editing || !!preset;
  id.addEventListener('input', () => { idTouched = true; });
  name.addEventListener('input', () => { if (!idTouched) id.value = slugify(name.value); });

  const picks = h('div', { class: 'presetpick' });
  const paintPicks = () => picks.replaceChildren(...PRESETS.map(p => h('button', {
    type: 'button', class: 'btn', 'aria-pressed': String(id.value === p.id && base.value === p.base),
    onclick: () => { name.value = p.name; id.value = p.id; base.value = p.base; kind.value = p.kind; authName.value = p.auth_header || ''; authPrefix.value = p.auth_prefix || ''; syncAuth(); idTouched = true; paintPicks(); (p.base ? key : base).focus(); },
  }, p.name)));
  paintPicks();

  const dlg = h('dialog', null, h('form', {
    onsubmit: async e => {
      e.preventDefault();
      try {
        await api('PUT', `/providers/${id.value}`, { name: name.value, kind: kind.value, base_url: base.value, enabled: editing ? existing.enabled : true, auth_header: authName.value, auth_prefix: authPrefix.value, headers: hdrs.filter(r => r.name.trim()).map(r => ({ name: r.name, value: r.value })), key: key.value, key_label: '' });
        dlg.close(); toast(editing ? 'Provider saved' : 'Provider added. Wack!'); await refresh();
      } catch (err) { toast(err.message, true); }
    },
  },
    h('h2', null, editing ? 'Edit ' + existing.name : 'Add provider'),
    !editing && h('div', { class: 'field' }, h('span', null, 'Start from'), picks),
    h('label', { class: 'field' }, h('span', null, 'Name'), name),
    h('label', { class: 'field' }, h('span', null, 'Model prefix'), id,
      h('span', { class: 'muted small', style: 'font-weight:400' }, 'Models are addressed as prefix/model-name.')),
    h('label', { class: 'field' }, h('span', null, 'Base URL'), base),
    h('label', { class: 'field' }, h('span', null, 'Auth style'), kind),
    authBox,
    h('div', { class: 'field' }, h('span', null, 'Extra headers'),
      h('span', { class: 'muted small', style: 'font-weight:400' }, 'Sent with every request to this provider, for things like HTTP-Referer, project ids or gateway keys. Values are encrypted.'),
      hdrBox,
      h('div', null, h('button', { type: 'button', class: 'btn', onclick: () => { hdrs.push({ name: '', value: '', hint: '', had: false }); paintHdrs(); } }, 'Add header'))),
    h('label', { class: 'field' }, h('span', null, 'API key'), key),
    h('div', { class: 'row', style: 'justify-content:flex-end' },
      h('button', { class: 'btn', type: 'button', onclick: () => dlg.close() }, 'Cancel'),
      h('button', { class: 'btn primary', type: 'submit' }, editing ? 'Save changes' : 'Add provider'))));
  dlg.addEventListener('close', () => dlg.remove());
  document.body.append(dlg);
  dlg.showModal();
  (editing ? name : (preset ? key : name)).focus();
}

/* ---------- access tokens ---------- */
function snippets(secret) {
  const o = location.origin;
  const model = (S.providers[0] ? S.providers[0].id : 'openai') + '/your-model';
  return {
    curl: `curl ${o}/v1/chat/completions \\\n  -H "Authorization: Bearer ${secret}" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","messages":[{"role":"user","content":"Hello"}]}'`,
    python: `from openai import OpenAI\n\nclient = OpenAI(base_url="${o}/v1", api_key="${secret}")\nreply = client.chat.completions.create(\n    model="${model}",\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(reply.choices[0].message.content)`,
    env: `OPENAI_BASE_URL=${o}/v1\nOPENAI_API_KEY=${secret}`,
    anthropic: `# Anthropic SDKs and Claude Code, needs a provider with prefix "anthropic"\nANTHROPIC_BASE_URL=${o}/p/anthropic\nANTHROPIC_API_KEY=${secret}`,
  };
}

function pageAccess() {
  const out = [];
  out.push(h('div', { class: 'page-head' },
    h('div', null, h('h1', null, 'Access tokens'),
      h('p', { class: 'muted' }, 'One token per app or person. Revoke the troublemaker, keep everyone else.'))));

  const name = h('input', { type: 'text', placeholder: 'What will use it? e.g. laptop, Cursor, bot', required: true, 'aria-label': 'Token name', class: 'grow' });
  out.push(h('form', {
    class: 'row', style: 'margin-bottom:20px',
    onsubmit: async e => {
      e.preventDefault();
      try { const r = await api('POST', '/tokens', { name: name.value }); revealed = { name: name.value, secret: r.secret, tab: 'curl' }; await refresh(); }
      catch (err) { toast(err.message, true); }
    },
  }, name, h('button', { class: 'btn primary', type: 'submit' }, 'Create token')));

  if (revealed) {
    const r = revealed;
    const box = h('pre');
    const tabs = h('div', { class: 'tabs', role: 'tablist' });
    const paint = () => {
      const sn = snippets(r.secret);
      box.textContent = sn[r.tab];
      tabs.replaceChildren(...[['curl', 'curl'], ['python', 'Python'], ['env', 'OpenAI env'], ['anthropic', 'Anthropic env']].map(([id, label]) =>
        h('button', { role: 'tab', 'aria-selected': String(r.tab === id), onclick: () => { r.tab = id; paint(); } }, label)));
    };
    paint();
    out.push(h('div', { class: 'panel reveal' },
      h('h3', null, `Token for ${r.name}`),
      h('p', { class: 'small', style: 'margin:4px 0 12px' }, 'Copy it now. Wackrouter keeps only a hash, so this is its one and only appearance.'),
      h('div', { class: 'row' },
        h('div', { class: 'secret' }, r.secret),
        h('button', { class: 'btn', onclick: () => copy(r.secret, 'Token copied') }, 'Copy token'),
        h('button', { class: 'btn', onclick: () => { revealed = null; render(); } }, 'Done')),
      tabs, box,
      h('div', { class: 'row', style: 'margin-top:8px' }, h('button', { class: 'btn', onclick: () => copy(box.textContent, 'Snippet copied') }, 'Copy snippet'))));
  }

  if (!S.tokens.length) {
    out.push(h('div', { class: 'panel empty' }, h('h2', null, 'No tokens yet'),
      h('p', { class: 'muted' }, 'Create one above. Until you do, the proxy rejects every request.')));
  } else {
    out.push(h('div', { class: 'panel scroll' }, h('table', null,
      h('thead', null, h('tr', null, ['Name', 'Token', 'Last used', 'Requests', '', ''].map((t, i) => h('th', { class: i === 3 ? 'num' : '' }, t)))),
      h('tbody', null, S.tokens.map(t => h('tr', null,
        h('td', null, h('b', null, t.name), !t.enabled && [' ', h('span', { class: 'chip' }, 'Off')]),
        h('td', null, h('code', { class: 'muted' }, t.hint)),
        h('td', { class: 'muted' }, ago(t.last_used)),
        h('td', { class: 'num' }, t.requests.toLocaleString()),
        h('td', null, h('button', { class: 'btn quiet', onclick: () => act(() => api('PATCH', `/tokens/${t.id}`, { enabled: !t.enabled })) }, t.enabled ? 'Turn off' : 'Turn on')),
        h('td', null, h('button', { class: 'btn quiet danger', onclick: () => { if (confirm(`Delete token "${t.name}"? Anything using it stops working.`)) act(() => api('DELETE', `/tokens/${t.id}`), 'Token deleted'); } }, 'Delete'))))))));
  }
  return out;
}

/* ---------- playground ---------- */
function pagePlayground() {
  const list = h('datalist', { id: 'models' });
  const model = h('input', { type: 'text', list: 'models', class: 'mono', placeholder: 'provider/model, e.g. openai/gpt-4o-mini', 'aria-label': 'Model', value: chatModel, autocomplete: 'off' });
  model.addEventListener('input', () => { chatModel = model.value; });
  api('GET', '/models').then(r => {
    const ids = (r.data || []).map(m => m.id).sort();
    list.replaceChildren(...ids.map(id => h('option', { value: id })));
    if (!chatModel && ids.length) { model.placeholder = 'e.g. ' + ids[0]; }
  }).catch(() => {});

  const box = h('div', { class: 'panel chat', 'aria-live': 'polite' });
  const paint = () => {
    box.replaceChildren(...(chatMsgs.length ? chatMsgs.map(m => h('div', { class: 'msg ' + m.role }, m.content)) :
      [h('p', { class: 'muted' }, 'Send a message to check that a provider, key and model work end to end. Requests here go through the same path as your apps and appear in Activity as “playground”.')]));
    box.scrollTop = box.scrollHeight;
  };
  paint();

  const input = h('textarea', { rows: 2, placeholder: 'Message', 'aria-label': 'Message' });
  const send = h('button', { class: 'btn primary' }, 'Send');
  async function submit() {
    const text = input.value.trim();
    if (!text || !chatModel.trim()) { if (!chatModel.trim()) { toast('Choose a model first', true); model.focus(); } return; }
    chatMsgs.push({ role: 'user', content: text });
    const reply = { role: 'assistant', content: '' };
    chatMsgs.push(reply);
    input.value = ''; send.disabled = true; paint();
    const history = chatMsgs.slice(0, -1).filter(m => m.role !== 'error').map(m => ({ role: m.role, content: m.content }));
    try {
      const res = await fetch('/api/chat', {
        method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Wackrouter': '1' },
        body: JSON.stringify({ model: chatModel.trim(), messages: history, stream: true }),
      });
      if (res.status === 401) { showLogin(); return; }
      if (!res.ok) {
        const j = await res.json().catch(() => ({}));
        throw new Error((j.error && (j.error.message || j.error)) || 'HTTP ' + res.status);
      }
      const rd = res.body.getReader(), dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await rd.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const lines = buf.split('\n'); buf = lines.pop();
        for (const ln of lines) {
          if (!ln.startsWith('data:')) continue;
          const d = ln.slice(5).trim();
          if (!d || d === '[DONE]') continue;
          try { const j = JSON.parse(d); reply.content += (j.choices && j.choices[0] && j.choices[0].delta && j.choices[0].delta.content) || ''; } catch {}
        }
        paint();
      }
      if (!reply.content) reply.content = '(empty reply)';
    } catch (e) {
      chatMsgs.pop();
      chatMsgs.push({ role: 'error', content: e.message });
    }
    send.disabled = false; paint(); input.focus();
  }
  send.addEventListener('click', submit);
  input.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); submit(); } });

  return [
    h('div', { class: 'page-head' }, h('div', null, h('h1', null, 'Playground'),
      h('p', { class: 'muted' }, 'Poke a model and see what happens.')),
      h('button', { class: 'btn', onclick: () => { chatMsgs = []; paint(); } }, 'Clear chat')),
    h('div', { class: 'row', style: 'margin-bottom:12px' }, model, list),
    box,
    h('div', { class: 'composer' }, input, send),
    h('p', { class: 'muted small', style: 'margin-top:6px' }, 'Press Ctrl or ⌘ with Enter to send.'),
  ];
}

/* ---------- activity ---------- */
function pageActivity() {
  const stats = h('div', { class: 'stats' });
  const chart = h('div', { class: 'panel chart' });
  const table = h('div', { class: 'panel scroll' });

  const provSel = h('select', { 'aria-label': 'Provider filter' },
    h('option', { value: '' }, 'All providers'),
    S.providers.map(p => h('option', { value: p.id, selected: actFilter.provider === p.id }, p.name)));
  provSel.addEventListener('change', () => { actFilter.provider = provSel.value; paint(); });
  const errBox = h('input', { type: 'checkbox', id: 'errs', checked: actFilter.errors });
  errBox.addEventListener('change', () => { actFilter.errors = errBox.checked; paint(); });

  function paint() {
    const now = Date.now(), day = logs.filter(l => now - l.t < 864e5);
    const errs = day.filter(l => l.status >= 400 || l.status === 0).length;
    const tin = day.reduce((a, l) => a + l.in, 0), tout = day.reduce((a, l) => a + l.out, 0);
    stats.replaceChildren(
      ...[['Requests, 24 h', day.length.toLocaleString()], ['Errors', errs.toLocaleString()], ['Tokens in', fmt(tin)], ['Tokens out', fmt(tout)]]
        .map(([k, v]) => h('div', { class: 'panel stat' }, h('b', null, v), h('span', { class: 'muted small' }, k))));

    // hourly buckets, oldest to newest
    const B = 24, hr = 36e5, end = Math.ceil(now / hr) * hr;
    const ok = Array(B).fill(0), bad = Array(B).fill(0);
    for (const l of day) {
      const i = B - 1 - Math.floor((end - l.t) / hr);
      if (i >= 0 && i < B) (l.status >= 400 || l.status === 0 ? bad : ok)[i]++;
    }
    const max = Math.max(1, ...ok.map((v, i) => v + bad[i]));
    const W = 480, H = 80, base = H - 2, bw = W / B;
    const s = svg('svg', { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: 'none', role: 'img', 'aria-label': 'Requests per hour, last 24 hours' });
    for (let i = 0; i < B; i++) {
      const total = ok[i] + bad[i], hOk = (ok[i] / max) * (base - 4), hBad = (bad[i] / max) * (base - 4);
      const t = new Date(end - (B - i) * hr);
      const g = svg('g', null, svg('title', null, `${t.getHours()}:00, ${total} request${total === 1 ? '' : 's'}${bad[i] ? ', ' + bad[i] + ' failed' : ''}`),
        svg('rect', { x: i * bw, y: 0, width: bw, height: base, fill: 'transparent' }));
      if (total === 0) g.append(svg('rect', { x: i * bw + 2, y: base - 2, width: bw - 4, height: 2, rx: 1, style: 'fill:var(--line)' }));
      if (ok[i]) g.append(svg('rect', { x: i * bw + 2, y: base - hOk - hBad, width: bw - 4, height: hOk, rx: 2, style: 'fill:var(--lemon);stroke:var(--edge);stroke-width:1.5' }));
      if (bad[i]) g.append(svg('rect', { x: i * bw + 2, y: base - hBad, width: bw - 4, height: hBad, rx: 2, style: 'fill:var(--bad);stroke:var(--edge);stroke-width:1.5' }));
      s.append(g);
    }
    chart.replaceChildren(s, h('div', { class: 'axis muted small' }, h('span', null, '24 h ago'), h('span', null, 'now')));

    let rows = logs.filter(l => (!actFilter.provider || l.provider === actFilter.provider) && (!actFilter.errors || l.status >= 400 || l.status === 0));
    rows = rows.slice(0, 150);
    table.replaceChildren(rows.length ? h('table', null,
      h('thead', null, h('tr', null, ['Time', 'Token', 'Model', 'Key', 'Status', 'Time taken', 'Tokens in / out'].map((t, i) => h('th', { class: i > 4 ? 'num' : '' }, t)))),
      h('tbody', null, rows.map(l => {
        const bad = l.status >= 400 || l.status === 0;
        return h('tr', null,
          h('td', { class: 'muted', style: 'white-space:nowrap' }, new Date(l.t).toLocaleTimeString()),
          h('td', null, l.who),
          h('td', null, h('code', null, l.provider + '/' + (l.model || '…')), l.err && h('div', { class: 'small', style: 'color:var(--bad)' }, l.err)),
          h('td', { class: 'muted' }, l.key),
          h('td', null, h('span', { class: 'chip ' + (bad ? 'bad' : 'ok') }, l.status || 'failed')),
          h('td', { class: 'num' }, l.ms >= 1000 ? (l.ms / 1000).toFixed(1) + ' s' : l.ms + ' ms'),
          h('td', { class: 'num' }, l.in || l.out ? `${l.in} / ${l.out}` : '–'));
      }))) : h('div', { class: 'empty muted' }, logs.length ? 'No requests match these filters.' : 'No requests yet. Send one from the Playground or any app using your token.'));
  }

  const load = async () => { try { logs = await api('GET', '/logs'); if (page === 'activity') paint(); } catch {} };
  load();
  pollTimer = setInterval(load, 5000);
  paint();

  return [
    h('div', { class: 'page-head' }, h('div', null, h('h1', null, 'Activity'), h('p', { class: 'muted' }, 'Everything that went through, newest first.')),
      h('button', { class: 'btn', onclick: () => { if (confirm('Clear the request log?')) act(async () => { await api('DELETE', '/logs'); logs = []; }, 'Log cleared'); } }, 'Clear log')),
    stats, chart,
    h('div', { class: 'toolbar' }, provSel, h('label', { class: 'row small', for: 'errs' }, errBox, 'Errors only')),
    table,
  ];
}

/* ---------- drains ---------- */
function sinkState(k) {
  if (!k.enabled) return ['', 'Off'];
  if (!k.sent && !k.failed) return ['', 'Waiting for requests'];
  if (k.last_fail > k.last_ok) return ['bad', 'Failing'];
  return ['ok', 'Shipping'];
}

function pageDrains() {
  const out = [h('div', { class: 'page-head' },
    h('div', null, h('h1', null, 'Drains'),
      h('p', { class: 'muted' }, 'Ship every request to Logflare or your own webhook. Only metadata goes out: never prompts, replies or keys.')),
    h('button', { class: 'btn primary', onclick: () => sinkDialog() }, 'Add drain'))];
  if (!S.sinks.length) {
    out.push(h('div', { class: 'panel empty' }, mascot(),
      h('h2', null, 'Nowhere to drain yet'),
      h('p', { class: 'muted' }, 'Point it at a Logflare source and every request shows up there within a couple of seconds.'),
      h('div', { class: 'presets' }, SINK_PRESETS.map(p => h('button', { class: 'btn', onclick: () => sinkDialog(null, p) }, p.name)))));
  }
  S.sinks.forEach(k => {
    const st = sinkState(k);
    const test = h('button', { class: 'btn' }, 'Send test');
    test.addEventListener('click', async () => {
      test.disabled = true;
      try { const r = await api('POST', `/sinks/${k.id}/test`); toast(r.ok ? `${k.name} accepted the test event (${r.ms} ms)` : `${k.name} failed: ${r.error}`, !r.ok); }
      catch (e) { toast(e.message, true); }
      test.disabled = false; refresh();
    });
    out.push(h('section', { class: 'panel prov' + (k.enabled ? '' : ' off') },
      h('div', { class: 'prov-head' },
        h('div', { class: 'tile', 'aria-hidden': 'true' }, k.name.slice(0, 1).toUpperCase()),
        h('div', { class: 'prov-title' },
          h('div', { class: 'name' }, k.name, h('span', { class: 'chip ' + st[0], title: k.last_err || '' }, st[1])),
          h('div', { class: 'sub' }, k.type === 'logflare' ? ['Logflare source ', h('code', null, k.source), ' at ', k.url] : ['Webhook ', h('code', null, k.url)]),
          h('div', { class: 'sub' }, `${k.sent.toLocaleString()} sent` + (k.failed ? `, ${k.failed.toLocaleString()} failed` : '') + (k.last_ok ? ', last delivery ' + ago(k.last_ok) : ''),
            k.last_fail > k.last_ok && k.last_err ? h('div', { style: 'color:var(--bad)' }, k.last_err) : null)),
        h('div', { class: 'row' }, test,
          h('button', { class: 'btn quiet', onclick: () => sinkDialog(k) }, 'Edit'),
          h('button', { class: 'btn quiet', onclick: () => act(() => api('PATCH', `/sinks/${k.id}`, { enabled: !k.enabled })) }, k.enabled ? 'Turn off' : 'Turn on'),
          h('button', { class: 'btn quiet danger', onclick: () => { if (confirm(`Delete the ${k.name} drain?`)) act(() => api('DELETE', `/sinks/${k.id}`), 'Drain deleted'); } }, 'Delete')))));
  });
  return out;
}

function sinkDialog(existing, preset) {
  const editing = !!existing;
  const v = existing || preset || { id: '', name: '', type: 'logflare', url: 'https://api.logflare.app' };
  const name = h('input', { type: 'text', value: v.name || '', required: true, autocomplete: 'off' });
  const id = h('input', { type: 'text', value: v.id || '', required: true, pattern: '[a-z0-9][a-z0-9\\-]{0,31}', readonly: editing, autocomplete: 'off' });
  const type = h('select', null,
    h('option', { value: 'logflare', selected: v.type === 'logflare' }, 'Logflare'),
    h('option', { value: 'webhook', selected: v.type === 'webhook' }, 'Webhook (JSON)'));
  const url = h('input', { type: 'url', value: v.url || '', required: true });
  const source = h('input', { type: 'text', class: 'mono', value: v.source || '', placeholder: 'Source UUID or name', autocomplete: 'off' });
  const key = h('input', { type: 'password', autocomplete: 'off' });
  const urlLabel = h('span'), keyLabel = h('span'), keyNote = h('span', { class: 'muted small', style: 'font-weight:400' });
  const sourceBox = h('label', { class: 'field' }, h('span', null, 'Source'), source);
  const sync = () => {
    const lf = type.value === 'logflare';
    urlLabel.textContent = lf ? 'Logflare URL' : 'Webhook URL';
    keyLabel.textContent = lf ? 'Ingest API key' : 'Authorization header (optional)';
    keyNote.textContent = lf ? 'Use the self-hosted address here if you run your own Logflare.' : 'Sent as-is, for example Bearer abc123. Events arrive as a JSON array.';
    key.placeholder = editing && v.hint ? '•••• ' + v.hint + ' (blank keeps it)' : (lf ? 'Paste API key' : 'Bearer …');
    key.required = lf && !(editing && v.hint);
    sourceBox.style.display = lf ? '' : 'none';
    source.required = lf;
  };
  type.addEventListener('change', () => { if (type.value === 'logflare' && !url.value) url.value = 'https://api.logflare.app'; sync(); });
  sync();
  const dlg = h('dialog', null, h('form', {
    onsubmit: async e => {
      e.preventDefault();
      try {
        await api('PUT', `/sinks/${id.value}`, { name: name.value, type: type.value, url: url.value, source: source.value, key: key.value, enabled: editing ? existing.enabled : true });
        dlg.close(); toast(editing ? 'Drain saved' : 'Drain added. It starts with the next request.'); await refresh();
      } catch (err) { toast(err.message, true); }
    },
  },
    h('h2', null, editing ? 'Edit ' + existing.name : 'Add drain'),
    h('label', { class: 'field' }, h('span', null, 'Name'), name),
    h('label', { class: 'field' }, h('span', null, 'Short id'), id),
    h('label', { class: 'field' }, h('span', null, 'Type'), type),
    h('label', { class: 'field' }, urlLabel, url),
    sourceBox,
    h('label', { class: 'field' }, keyLabel, key, keyNote),
    h('div', { class: 'row', style: 'justify-content:flex-end' },
      h('button', { class: 'btn', type: 'button', onclick: () => dlg.close() }, 'Cancel'),
      h('button', { class: 'btn primary', type: 'submit' }, editing ? 'Save changes' : 'Add drain'))));
  name.addEventListener('input', () => { if (!editing && !id.dataset.t) id.value = slugify(name.value); });
  id.addEventListener('input', () => { id.dataset.t = '1'; });
  dlg.addEventListener('close', () => dlg.remove());
  document.body.append(dlg); dlg.showModal(); name.focus();
}

/* ---------- settings ---------- */
function pageSettings() {
  const def = h('select', { 'aria-label': 'Default provider' },
    h('option', { value: '' }, 'First provider with keys'),
    S.providers.map(p => h('option', { value: p.id, selected: S.default_provider === p.id }, p.name)));
  def.addEventListener('change', () => act(() => api('PUT', '/default', { provider: def.value }), 'Default saved'));

  const rows = Object.entries(S.aliases);
  if (!rows.length) rows.push(['', '']);
  const aliasBox = h('div');
  const paintAliases = () => aliasBox.replaceChildren(...rows.map((r, i) => h('div', { class: 'alias' },
    h('input', { type: 'text', class: 'mono', placeholder: 'alias, e.g. fast', value: r[0], 'aria-label': 'Alias', oninput: e => { r[0] = e.target.value; } }),
    h('input', { type: 'text', class: 'mono', placeholder: 'provider/model, e.g. groq/llama-3.3-70b-versatile', value: r[1], 'aria-label': 'Points to', oninput: e => { r[1] = e.target.value; } }),
    h('button', { class: 'btn quiet danger', onclick: () => { rows.splice(i, 1); if (!rows.length) rows.push(['', '']); paintAliases(); } }, 'Remove'))));
  paintAliases();

  const cur = h('input', { type: 'password', autocomplete: 'current-password', required: true, 'aria-label': 'Current password' });
  const nw = h('input', { type: 'password', autocomplete: 'new-password', required: true, minlength: 8, 'aria-label': 'New password' });

  return [
    h('div', { class: 'page-head' }, h('div', null, h('h1', null, 'Settings'))),
    h('section', { class: 'panel section' },
      h('h2', null, 'Default provider'),
      h('p', { class: 'muted' }, 'Used when a request names a model without a provider prefix.'),
      h('div', { style: 'max-width:340px' }, def)),
    h('section', { class: 'panel section' },
      h('h2', null, 'Model aliases'),
      h('p', { class: 'muted' }, 'Short names that point at a real model. Apps ask for “fast”, you decide what that means.'),
      aliasBox,
      h('div', { class: 'row' },
        h('button', { class: 'btn', onclick: () => { rows.push(['', '']); paintAliases(); } }, 'Add alias'),
        h('button', { class: 'btn primary', onclick: () => act(() => api('PUT', '/aliases', Object.fromEntries(rows.filter(r => r[0].trim() && r[1].trim()))), 'Aliases saved') }, 'Save aliases'))),
    h('section', { class: 'panel section' },
      h('h2', null, 'Admin password'),
      S.env_password ? h('p', { class: 'muted' }, 'The password comes from the WACKROUTER_PASSWORD environment variable. Change it there.') : [
        h('p', { class: 'muted' }, 'Changing it signs out every other browser.'),
        h('form', {
          class: 'row',
          onsubmit: async e => { e.preventDefault(); try { await api('POST', '/password', { current: cur.value, new: nw.value }); cur.value = nw.value = ''; toast('Password changed'); } catch (err) { toast(err.message, true); } },
        }, h('div', { class: 'grow' }, cur), h('div', { class: 'grow' }, nw), h('button', { class: 'btn primary', type: 'submit' }, 'Change password'))]),
    h('section', { class: 'panel section' },
      h('h2', null, 'Storage'),
      h('p', { class: 'muted' }, 'Keys are encrypted with AES-256-GCM. The master key sits in master.key beside wackrouter.json in the data folder, unless you set WACKROUTER_SECRET. Back both up together, and keep them apart if you can.'),
      h('div', { style: 'height:12px' }),
      h('button', { class: 'btn', onclick: async () => { await api('POST', '/logout'); showLogin(); } }, 'Sign out')),
  ];
}

/* ---------- boot ---------- */
async function boot() {
  try {
    const s = await api('GET', '/session');
    if (!s.ok) return showLogin();
    page = location.hash.slice(1) || 'providers';
    await refresh();
  } catch { showLogin(); }
}
boot();
})();
