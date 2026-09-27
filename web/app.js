// Progressive enhancement: all catalog mutations use the JSON API. The server
// owns validation, revisions and processing state; this module holds only edits.
const editor = document.querySelector('#editor');
if (editor) {
  editor.hidden = false;
  document.querySelector('#read-only').hidden = true;
  const form = document.querySelector('#metadata');
  const errorBox = document.querySelector('#error');
  const base = `/api/albums/${encodeURIComponent(editor.dataset.id)}`;
  let etag = editor.dataset.etag;
  let dirty = false;
  form.addEventListener('input', event => {
    dirty = true;
    if (event.target.matches('[data-inherited]')) event.target.dataset.inherited = 'false';
  });
  function showError(error) {
    errorBox.replaceChildren();
    const title = document.createElement('strong');
    title.className = 'notice-title';
    title.textContent = `${error.code || 'request_failed'}: ${error.message || 'Request failed'}`;
    errorBox.append(title);
    if (error.details && Object.keys(error.details).length) {
      const details = document.createElement('pre');
      details.className = 'notice-details';
      details.textContent = JSON.stringify(error.details, null, 2);
      errorBox.append(details);
    }
    if (error.status === 412) {
      const message = document.createElement('p');
      message.className = 'notice-hint';
      message.textContent = 'Another window saved a newer revision. Your edits are still here. Copy them or reload and re-apply them; this page will not overwrite the newer data.';
      errorBox.append(message);
    }
    errorBox.hidden = false;
    errorBox.focus();
  }
  // Do not accept a fresh ETag on failure, especially a 412: it would allow
  // a later click to silently overwrite another tab's edits.
  async function request(url, method, body, type) {
    const headers = { 'X-Musiclib-Request': '1', 'If-Match': etag };
    if (type) headers['Content-Type'] = type;
    let response;
    try { response = await fetch(url, { method, headers, body }); }
    catch { showError({ code: 'network_error', message: 'Cannot reach the server' }); return null; }
    let result;
    try { result = await response.json(); }
    catch { showError({ code: 'invalid_response', message: 'The server returned an unreadable response' }); return null; }
    if (!response.ok) { showError({ ...result, status: response.status }); return null; }
    errorBox.hidden = true;
    etag = response.headers.get('ETag') || result.etag || etag;
    editor.dataset.etag = etag;
    return result;
  }
  function refreshAfterSuccess() { location.reload(); }
  function trackURL(row) { return `${base}/tracks/${encodeURIComponent(row.dataset.track)}`; }
  function requiredFile(selector) {
    const file = document.querySelector(selector).files[0];
    if (!file) showError({ code: 'file_required', message: 'Choose a file first.' });
    return file;
  }
  function metadata() {
    const fields = new FormData(form);
    return {
      artist_id: fields.get('artist_id'), title: fields.get('title'),
      year: fields.get('year') === '' ? null : Number(fields.get('year')),
      genre: fields.get('genre') === '' ? null : fields.get('genre'),
      compilation: fields.has('compilation'),
      tracks: [...form.querySelectorAll('[data-track]')].map(row => {
        const input = name => row.querySelector(`[name="${name}"]`);
        const inherited = name => input(name).dataset.inherited === 'true' ? null : input(name).value;
        return { id: row.dataset.track, disc: Number(input('disc').value), no: Number(input('no').value),
          title: input('title').value, artist: inherited('artist'), genre: inherited('genre') };
      })
    };
  }
  form.addEventListener('submit', async event => {
    event.preventDefault();
    const result = await request(base, 'PUT', JSON.stringify(metadata()), 'application/json');
    if (result) { dirty = false; refreshAfterSuccess(); }
  });
  form.addEventListener('click', event => {
    const field = event.target.dataset.inherit;
    if (!field) return;
    const input = event.target.closest('td').querySelector(`[name="${field}"]`);
    input.value = '';
    input.dataset.inherited = 'true';
    dirty = true;
    input.focus();
  });
  document.querySelector('#create-artist').addEventListener('click', async () => {
    const name = document.querySelector('#new-artist').value;
    // Artist creation is independent of the album revision. A 409 exposes the
    // existing artist in details; no automatic merge or reassignment.
    let response;
    try { response = await fetch('/api/artists', { method: 'POST', headers: { 'X-Musiclib-Request': '1', 'Content-Type': 'application/json' }, body: JSON.stringify({ name }) }); }
    catch { showError({ code: 'network_error', message: 'Cannot reach the server' }); return; }
    const value = await response.json();
    if (!response.ok) { showError({ ...value, status: response.status }); return; }
    const option = new Option(value.name, value.id, true, true);
    document.querySelector('#artist-id').add(option);
    dirty = true;
    errorBox.hidden = true;
  });
  editor.addEventListener('click', async event => {
    const button = event.target.closest('button[data-action],button[data-attachment]');
    if (!button) return;
    const action = button.dataset.action;
    const row = button.closest('[data-track]');
    let url = base, method, body, type;
    if (button.dataset.attachment) {
      if (!confirm('Delete this attachment? There is no undo.')) return;
      url += `/attachments/${encodeURIComponent(button.dataset.attachment)}`; method = 'DELETE';
    } else if (action === 'track-delete') {
      if (!confirm('Delete this track? There is no undo.')) return;
      url = trackURL(row); method = 'DELETE';
    } else if (action === 'cover-upload') {
      body = requiredFile('#cover-file'); if (!body) return;
      url += '/cover'; method = 'PUT'; type = 'application/octet-stream';
    } else if (action === 'cover-choice') {
      const id = document.querySelector('#cover-choice').value;
      if (!id) { showError({ code: 'attachment_required', message: 'Choose an image attachment.' }); return; }
      url += '/cover'; method = 'PUT'; type = 'application/json'; body = JSON.stringify({ attachment_id: id });
    } else if (action === 'cover-remove') {
      if (!confirm('Remove this cover?')) return;
      url += '/cover'; method = 'DELETE';
    } else if (action === 'attachment-upload') {
      body = requiredFile('#attachment-file'); if (!body) return;
      const path = document.querySelector('#attachment-path').value;
      if (!path) { showError({ code: 'path_required', message: 'Enter a relative path.' }); return; }
      url += `/attachments?path=${encodeURIComponent(path)}`; method = 'POST'; type = 'application/octet-stream';
    } else if (action === 'lyrics-choice' || action === 'lyrics-upload' || action === 'lyrics-remove') {
      url = `${trackURL(row)}/lyrics`;
      if (action === 'lyrics-remove') {
        if (!confirm('Remove these lyrics?')) return;
        method = 'DELETE';
      } else if (action === 'lyrics-choice') {
        const id = row.querySelector('[data-lyrics-choice]').value;
        if (!id) { showError({ code: 'attachment_required', message: 'Choose an .lrc attachment.' }); return; }
        method = 'PUT'; type = 'application/json'; body = JSON.stringify({ attachment_id: id });
      } else {
        body = row.querySelector('[data-lyrics-file]').files[0];
        if (!body) { showError({ code: 'file_required', message: 'Choose an .lrc file.' }); return; }
        method = 'PUT'; type = 'application/octet-stream';
      }
    } else if (action === 'trash') {
      if (!confirm('Move this album to the trash? You can restore it later.')) return;
      method = 'DELETE';
    } else if (action === 'restore') { method = 'POST'; url += '/restore'; }
    else if (action === 'render') { method = 'POST'; url += '/render'; }
    if (method && dirty && !confirm('This action will reload the page and discard unsaved metadata edits. Continue?')) return;
    if (method && await request(url, method, body, type)) refreshAfterSuccess();
  });
  const status = document.querySelector('#status');
  async function poll() {
    if (document.hidden || status.dataset.pending !== 'true') return;
    try {
      const response = await fetch(`${base}/status`, { cache: 'no-store' });
      if (!response.ok) return;
      const data = await response.json();
      const state = data.job?.state;
      status.dataset.pending = state === 'pending' || state === 'running' ? 'true' : 'false';
      const label = state === 'failed' ? 'Error' : state === 'running' ? 'Processing' : state === 'pending' ? 'Queued' : data.trashed && !data.published_path ? 'Archived' : data.revision === data.published_revision && data.published_renderer === data.renderer && data.published_path ? 'Aligned' : 'Queued';
      status.textContent = label;
      status.dataset.status = label; // The chip colour follows the live status, not only the rendered one.
    } catch { /* Keep the pending state and retry on the next tick. */ }
  }
  // One timer per album page; no requests after activity ceases.
  setInterval(poll, 2000);
}
