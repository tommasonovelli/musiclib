// Queue views transport JSON; only the server validates paths and overrides.
const errorBox = document.querySelector('#error');
function text(parent, tag, value) {
  const node = document.createElement(tag);
  node.textContent = value;
  parent.append(node);
  return node;
}
function error(value, background = false) {
  errorBox.replaceChildren();
  text(errorBox, 'strong', `${value.status ? value.status + ' ' : ''}${value.code || 'network_error'}: ${value.message || 'Cannot reach the server'}`);
  errorBox.hidden = false;
  if (!background) errorBox.focus();
}
async function request(url, method = 'GET', payload, background = false) {
  try {
    const headers = method === 'GET' ? {} : { 'X-Musiclib-Request': '1' };
    if (payload !== undefined) headers['Content-Type'] = 'application/json';
    const response = await fetch(url, { method, headers, body: payload === undefined ? undefined : JSON.stringify(payload), cache: 'no-store' });
    const result = await response.json();
    if (!response.ok) { error({ ...result, status: response.status }, background); return null; }
    errorBox.hidden = true;
    return result;
  } catch { error({ code: 'network_error', message: 'Cannot reach the server' }, background); return null; }
}
function link(parent, href, label) {
  const anchor = text(parent, 'a', label);
  anchor.href = href;
  return anchor;
}
function warnings(parent, values) {
  parent.replaceChildren();
  for (const w of values || []) text(parent, 'li', `${w.path || ''} ${w.code}: ${w.message}`);
}
function jobLinks(parent, job) {
  if (job.batch_id) link(parent, `/import?batch=${encodeURIComponent(job.batch_id)}`, 'Batch report');
  if (job.result_album_id || job.album_id) link(parent, `/albums/${encodeURIComponent(job.result_album_id || job.album_id)}`, 'Album');
}
function retry(parent, job, refresh, overrides = false) {
  if (job.state !== 'failed') return;
  const form = text(parent, 'form', '');
  if (overrides && job.kind === 'import') {
    for (const field of ['artist', 'title']) {
      const label = text(form, 'label', `${field === 'artist' ? 'Album artist' : 'Album title'} override (blank clears): `);
      const input = text(label, 'input', '');
      input.name = field;
      input.value = job.overrides?.[field] || '';
    }
  }
  const button = text(form, 'button', 'Retry');
  button.type = 'submit';
  form.addEventListener('submit', async event => {
    event.preventDefault();
    button.disabled = true;
    // Empty body preserves stored overrides; complete replacement clears blanks.
    const body = overrides && job.kind === 'import' ? {
      artist: form.elements.namedItem('artist').value || null,
      title: form.elements.namedItem('title').value || null
    } : undefined;
    const result = await request(`/api/jobs/${encodeURIComponent(job.id)}/retry`, 'POST', body);
    if (result) await refresh();
    else button.disabled = false;
  });
}
const source = document.querySelector('#source');
if (source) {
  const input = document.querySelector('#import-path');
  const entries = document.querySelector('#source-entries');
  const report = document.querySelector('#report');
  let active = false;
  let reportGeneration = 0;
  const invalidatedDrafts = new Set();
  const requestID = crypto.randomUUID(); // Retained for the same submission after lost answers.
  async function browse(path) {
    const data = await request(`/api/import-source?path=${encodeURIComponent(path)}`);
    if (!data) return;
    input.value = data.path;
    document.querySelector('#source-path').textContent = `/import${data.path ? '/' + data.path : ''}`;
    entries.replaceChildren();
    if (data.path) {
      const up = data.path.split('/').slice(0, -1).join('/');
      link(text(entries, 'li', ''), '#', '..').addEventListener('click', event => { event.preventDefault(); browse(up); });
    }
    for (const entry of data.entries) {
      const li = text(entries, 'li', '');
      if (entry.type === 'directory') {
        link(li, '#', entry.name + '/').addEventListener('click', event => {
          event.preventDefault(); browse(data.path ? `${data.path}/${entry.name}` : entry.name);
        });
      } else li.textContent = `${entry.name} (${entry.type}; not navigable)`;
    }
  }
  async function loadReport(id, background = false) {
    const generation = reportGeneration;
    const data = await request(`/api/imports/${encodeURIComponent(id)}`, 'GET', undefined, background);
    if (!data || generation !== reportGeneration) return;
    // Read drafts after the fetch: users may have typed while it was in flight.
    const drafts = new Map();
    let focused;
    for (const li of document.querySelectorAll('#candidates > li[data-job-id]')) {
      const form = li.querySelector('form');
      if (!form) continue;
      const values = {};
      for (const field of ['artist', 'title']) {
        const input = form.elements.namedItem(field);
        values[field] = input.value;
        if (input === document.activeElement) {
          focused = { id: li.dataset.jobId, field, start: input.selectionStart, end: input.selectionEnd, direction: input.selectionDirection };
        }
      }
      drafts.set(li.dataset.jobId, values);
    }
    source.hidden = true; report.hidden = false;
    document.querySelector('#batch-state').textContent = `${data.path || '/import'} — ${data.state} — created ${data.created_at}`;
    document.querySelector('#scan').textContent = `${data.scan.state}${data.scan.error_code ? ': ' + data.scan.error_code + ' — ' + data.scan.error_message : ''}`;
    warnings(document.querySelector('#scan-warnings'), data.scan.warnings);
    const list = document.querySelector('#candidates'); list.replaceChildren();
    if (!data.candidates.length) text(list, 'li', data.state === 'completed' ? 'No valid candidate. See scan error and warnings above.' : 'Scanning for candidates…');
    for (const job of data.candidates) {
      const li = text(list, 'li', `${job.source_rel} — ${job.state}`);
      li.dataset.jobId = job.id;
      if (job.error_code) text(li, 'p', `${job.error_code}: ${job.error_message}`);
      jobLinks(li, job);
      const warningList = text(li, 'ul', ''); warnings(warningList, job.warnings);
      retry(li, job, () => {
        // A successful retry starts a new attempt: never carry its old form forward.
        invalidatedDrafts.add(job.id);
        reportGeneration++;
        return loadReport(id);
      }, true);
      const form = li.querySelector('form');
      if (form && drafts.has(job.id) && !invalidatedDrafts.has(job.id)) {
        for (const field of ['artist', 'title']) form.elements.namedItem(field).value = drafts.get(job.id)[field];
        if (focused?.id === job.id) {
          const input = form.elements.namedItem(focused.field);
          input.focus();
          input.setSelectionRange(focused.start, focused.end, focused.direction);
        }
      }
      invalidatedDrafts.delete(job.id);
    }
    active = data.state === 'scanning' || data.state === 'importing';
    document.querySelector('#nav-active').hidden = !active;
  }
  document.querySelector('#import-form').addEventListener('submit', async event => {
    event.preventDefault();
    const button = event.currentTarget.querySelector('button');
    button.disabled = true;
    const data = await request('/api/imports', 'POST', { id: requestID, path: input.value });
    button.disabled = false;
    if (!data) return;
    history.replaceState(null, '', `/import?batch=${encodeURIComponent(data.id)}`);
    await loadReport(data.id);
  });
  const batch = new URLSearchParams(location.search).get('batch');
  if (batch) loadReport(batch);
  else browse('');
  setInterval(() => { if (active && !document.hidden) loadReport(new URLSearchParams(location.search).get('batch'), true); }, 2000);
}
const activity = document.querySelector('#jobs');
if (activity) {
  let active = false;
  async function refresh() {
    const all = []; let after = null;
    do {
      const page = await request(`/api/jobs?limit=200${after ? '&after=' + encodeURIComponent(after) : ''}`);
      if (!page) return;
      all.push(...page.jobs); after = page.next;
    } while (after);
    activity.replaceChildren();
    for (const job of all) {
      const li = text(activity, 'li', `${job.kind}${job.source_rel ? ' — ' + job.source_rel : ''} — ${job.state} — queued ${job.queued_at}, updated ${job.updated_at}`);
      if (job.error_code) text(li, 'p', `${job.error_code}: ${job.error_message}`);
      jobLinks(li, job);
      retry(li, job, refresh);
    }
    if (!all.length) text(activity, 'li', 'No pending, running or failed jobs.');
    active = all.some(job => job.state === 'pending' || job.state === 'running');
    document.querySelector('#nav-active').hidden = !active;
    document.querySelector('#activity-state').textContent = `${all.length} jobs${active ? ' — work in progress' : ' — idle'}`;
  }
  document.querySelector('#retry-failed').addEventListener('click', async () => {
    if (await request('/api/jobs/retry-failed', 'POST')) await refresh();
  });
  document.querySelector('#render-all').addEventListener('click', async () => {
    if (!confirm('Queue rendering of every active album and unfinished trash removals? This may take time and disk space.')) return;
    if (await request('/api/render-all', 'POST')) await refresh();
  });
  refresh();
  setInterval(() => { if (active && !document.hidden) refresh(); }, 2000);
}
