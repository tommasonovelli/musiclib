// The album editor (NOTES.md N-256 to N-267): changes use If-Match (§10.4),
// then the page adopts its fresh server HTML (N-246) under the unsaved edits.
let editor = document.querySelector('#editor');
const $ = (s, root = document) => root.querySelector(s);
const $$ = (s, root = editor) => [...root.querySelectorAll(s)];
const base = `/api/albums/${editor.dataset.id}`;
const json = JSON.stringify, on = (type, f) => document.addEventListener(type, f);
let etag = editor.dataset.etag, saved, target, shown = false, busy, timer;

// Edits: named form controls by track and name; «No genre» joins its genre.
const fields = () => [...$('#metadata').elements].filter(f => f.name);
const key = f => (f.closest('[data-track]')?.dataset.track || '') + f.name;
const value = f => f.type == 'checkbox' ? String(f.checked)
  : f.name == 'genre' && f.closest('[data-track]')?.querySelector('[name=nogenre]').checked ? '\0' : f.value;
const changed = () => fields().filter(f => value(f) !== saved.get(key(f)));
const count = () => changed().filter(f => !/^(nogenre|new_artist)$/.test(f.name)).length;
const fold = s => s.normalize('NFKC').trim().toLowerCase();
const artist = () => $$('#artists option').find(o => fold(o.value) == fold($('#artist-name').value));

function arm() {
  for (const n of $$('[data-js]')) n.hidden = n.disabled = n.readOnly = false;
  saved = new Map(fields().map(f => [key(f), value(f)]));
}

function wrong() {
  $('#save-notice').hidden = true;
  for (const r of $$('.is-wrong')) r.classList.remove('is-wrong');
}

// Artist, inherited placeholders (N-259), Save bar. «Create artist» only
// stages the name while the field says it; Save creates it (N-298).
function update() {
  const o = artist(), name = $('#artist-name').value, id = $('[name=artist_id]'), staged = $('[name=new_artist]');
  if (o || fold(staged.value) != fold(name)) staged.value = '';
  if (o) id.value = o.dataset.id;
  else if (staged.value) id.value = '';
  else id.value ||= saved.get('artist_id');
  const n = count();
  $('#create-artist').hidden = !!o || !name.trim() || !!staged.value;
  $('#new-artist').hidden = !staged.value;
  $('#rename-artist').hidden = !o || id.value != saved.get('artist_id');
  for (const r of $$('[data-track]')) {
    $('[name=artist]', r).placeholder = name;
    $('[name=genre]', r).placeholder = $('[name=nogenre]', r).checked ? 'No genre' : $('#album-genre').value;
  }
  if (n) shown = false;
  else wrong(); // No edits, no save error.
  $('#changes').textContent = n ? `${n} change${n > 1 ? 's' : ''}` : shown ? 'Saved' : '';
  $('#savebar').classList.toggle('is-on', n > 0 || shown);
  $('#save').disabled = !n || busy;
}

const page = async () => {
  const response = await fetch(location.pathname, { cache: 'no-store' });
  if (!response.ok) throw 0;
  return new DOMParser().parseFromString(await response.text(), 'text/html');
};

// N-260: the change's ETag, never the page's (a newer revision must meet
// its 412); after a conflict, the page's.
async function refresh(tag) {
  const kept = changed().map(f => [key(f), f.value, f.checked]), focused = document.activeElement?.id;
  let doc;
  try {
    doc = await page();
  } catch {
    etag = tag || etag;
    return fail({ code: 'stale' }, $('#head-notice'));
  }
  const fresh = document.adoptNode($('#editor', doc));
  for (const n of $$('.notice:not([hidden])')) $('#' + n.id, fresh).replaceWith(n);
  editor.replaceWith(fresh);
  // The sidebar's server state: the current view and the count, not the
  // toggle, whose state is the browser's (N-272, N-278); the back link.
  for (const s of ['.nav', '.fix-filter', '.backlink']) $(s).replaceWith(document.adoptNode($(s, doc)));
  editor = fresh;
  document.title = doc.title;
  etag = editor.dataset.etag = tag || editor.dataset.etag;
  arm();
  const byKey = new Map(fields().map(f => [key(f), f]));
  for (const [k, v, checked] of kept) {
    const f = byKey.get(k);
    if (!f) continue; // A track deleted meanwhile.
    f.value = v;
    f.checked = checked;
    if (k == 'artist_id') $('#artist-name').value = $(`#artists [data-id="${v}"]`)?.value ?? '';
    if (k == 'new_artist' && v) $('#artist-name').value = v;
  }
  update();
  document.getElementById(focused)?.focus();
  glow();
}

async function call(url, method, body, box, match = etag) {
  const headers = { 'X-Musiclib-Request': '1' };
  if (match) headers['If-Match'] = match;
  if (body) headers['Content-Type'] = body instanceof Blob ? 'application/octet-stream' : 'application/json';
  let response, data;
  try {
    response = await fetch(url, { method, headers, body });
    data = await response.json();
  } catch {
    return fail({ code: 'network' }, box);
  }
  if (!response.ok) return fail({ ...data, status: response.status, url }, box);
  box.hidden = true;
  return data;
}

// A control rendered with method, URL, confirmation; a render has no ETag.
async function run(b, body = b.dataset.attachment && json({ attachment_id: b.dataset.attachment })) {
  const d = b.dataset;
  if (d.ask && !await ask(d.ask, b.textContent)) return;
  const data = await call(d.url || target.dataset.lyrics, d.method, body, $('.notice', b.closest('[data-area]')));
  if (data) await refresh(data.etag || etag);
}

// One sentence per code (N-261).
const exists = 'This artist already exists: choose it from the list.', badTag = 'A tag of this file can’t be kept: fix its tags.';
const sentences = {
  precondition_failed: 'This album was changed in another window.',
  artist: 'Choose an artist from the list, or create it.',
  artist_exists: exists,
  artist_folder_conflict: exists,
  artist_not_found: 'This artist isn’t in your library any more: choose another, or create it.',
  album_folder_conflict: 'The artist already has an album with this title: change it.',
  text_empty: 'A title or a name is empty: fill it in.',
  text_control_char: 'A text has a line break or an invisible character: remove it.',
  invalid_cover: 'This image can’t be a cover: use a JPEG or a PNG.',
  cover_not_embeddable: 'This image is too large for the album’s files: choose a smaller one.',
  invalid_lyrics: 'Lyrics files must be UTF-8: save it as UTF-8.',
  attachment_path_collision: 'An extra file has this name: change “Save as”.',
  track_exists: 'This file is already a track of the album.',
  corrupt_audio: 'This file is damaged and can’t be played.',
  unsupported_audio: 'Only FLAC, MP3 and M4A files can be tracks.',
  unrenderable_tag: badTag,
  invalid_tag: badTag,
  genre_not_writable: 'A genre can’t be written to the album’s MP3 files: change it.',
  insufficient_space: 'There isn’t enough disk space.',
  body_too_large: 'The file is too large.',
  network: 'The server doesn’t answer: try again.',
  login_required: 'You’re signed out. Sign in again in another tab, then save.',
  stale: 'Done, but the page didn’t update: reload it.'
};

function sentence(e) {
  const { code = '', details = {} } = e;
  if (code == 'duplicate_track_number') {
    // names: "track <id>".
    const rows = (details.names || []).map(n => $(`[data-track="${n.slice(6)}"]`)).filter(Boolean);
    for (const r of rows) r.classList.add('is-wrong');
    const [a, b] = rows.map(r => $('[name=title]', r).value);
    return b ? `${a} and ${b} both have number ${$('[name=no]', rows[0]).value}${$('.disc') ? ` on disc ${$('[name=disc]', rows[0]).value}` : ''}.`
      : 'Two tracks have the same number on the same disc.';
  }
  if (code == 'precondition_failed' && e.url.startsWith('/api/artists')) return 'This artist was changed in another window.';
  if (code.startsWith('path_')) return '“Save as” must be a name like Scans/front.jpg.';
  return sentences[code] || 'The change didn’t go through: try again.';
}

function fail(e, box) {
  const [p, again, details] = box.children;
  if (/^artist_(exists|folder|not)/.test(e.code)) artists();
  p.textContent = sentence(e);
  again.hidden = e.code != 'precondition_failed';
  details.hidden = !e.status;
  details.open = false;
  $('pre', box).textContent = json(e, null, 2);
  box.hidden = false;
  box.focus();
  return null;
}

const modal = dialog => {
  dialog.returnValue = '';
  dialog.showModal();
  return new Promise(done => dialog.addEventListener('close', () => done(dialog.returnValue == 'ok'), { once: true }));
};

// Confirms with the button's verb; with a value, asks for it.
async function ask(text, verb, value) {
  const input = $('#ask-input');
  $('#ask-text').textContent = text;
  $('#ask-ok').textContent = verb;
  input.hidden = input.disabled = value == null;
  input.value = value ?? '';
  return await modal($('#ask')) && (value == null || input.value.trim());
}

// The artists as the server has them now, after an answer that says the
// list was out of date; the edits stay.
async function artists() {
  try {
    $('#artists').replaceWith(document.adoptNode($('#artists', await page())));
    update();
  } catch { /* The notice already says what happened. */ }
}

// §4.3: each album of the artist gets a revision; only this one's is taken.
async function rename() {
  const b = $('#rename-artist'), id = $('[name=artist_id]').value, o = $(`#artists [data-id="${id}"]`);
  const name = await ask(b.dataset.ask, b.textContent, o.value);
  const a = name && await call(`/api/artists/${id}`, 'PUT', json({ name }), $('#head-notice'), o.dataset.etag);
  if (!a) return;
  if (a.etag != o.dataset.etag) etag = etag.replace(/(\d+)"$/, (_, r) => ++r + '"');
  await refresh(etag);
}

const words = s => s.normalize('NFKC').toLowerCase().replace(/[^\p{L}\p{N}]+/gu, ' ').trim();

// .lrc to track (N-264): the title after the number among that number's
// tracks, the title anywhere, the name as a title, the number alone.
function match(name, tracks) {
  const stem = name.replace(/\.lrc$/i, ''), one = list => list.length == 1 ? list[0] : null;
  const [, disc, no, rest = stem] = /^(?:(\d{1,2})[-.])?(\d{1,3})(?!\d)[\s._-]*(.*)$/.exec(stem) || [];
  const numbered = tracks.filter(t => !no || t.no == no && (!disc || t.disc == disc)), titled = (list, s) => list.filter(t => t.title == words(s));
  return one(titled(numbered, rest)) || one(titled(tracks, rest)) || one(titled(tracks, stem)) || no && one(numbered);
}

async function addLyrics(files) {
  const tracks = $$('[data-track]').map(r => ({ r, disc: +$('[name=disc]', r).value, no: +$('[name=no]', r).value, title: words($('[name=title]', r).value) }));
  const pairs = files.map(f => [f, match(f.name, tracks)]), list = $('#lrc-list'), lost = [];
  list.replaceChildren();
  for (const [file, t] of pairs) {
    // Claimed twice: neither.
    if (!t || pairs.filter(p => p[1] == t).length > 1) {
      lost.push(file.name);
      continue;
    }
    const item = $('#lrc-item').content.firstChild.cloneNode(true), [a, b] = $$('span', item);
    a.textContent = file.name;
    b.textContent = $('[name=title]', t.r).value;
    Object.assign(item, { file, row: t.r });
    list.append(item);
  }
  $('#lrc-lost').textContent = lost.length ? `No track for ${lost.join(', ')}.` : '';
  $('#lrc-ok').hidden = !list.children.length;
  if (!await modal($('#lrc-dialog'))) return;
  let tag;
  for (const item of [...list.children]) {
    if (!$('input', item).checked) continue;
    const a = await call(item.row.dataset.lyrics, 'PUT', item.file, $('#tracks-notice'));
    if (!a) break;
    etag = tag = a.etag;
  }
  if (tag) await refresh(tag);
}

// Audio files become tracks one at a time, each with the previous answer's
// ETag; the first refusal stops the rest and names its file. Only .flac,
// .mp3 and .m4a files are sent: a refused upload still stays as an original.
async function addTracks(all) {
  const box = $('#tracks-notice'), audio = f => /\.(flac|mp3|m4a)$/i.test(f.name), files = all.filter(audio), skipped = all.filter(f => !audio(f)).map(f => f.name), notes = skipped.length ? [`Not added, not FLAC, MP3 or M4A: ${skipped.join(', ')}.`] : [];
  let tag;
  for (const [i, file] of files.entries()) {
    $('#tracks-status').textContent = `Adding ${i + 1} of ${files.length}: ${file.name}…`;
    const a = await call(`${base}/tracks?name=${encodeURIComponent(file.name)}`, 'POST', file, box);
    if (!a) {
      $('p', box).textContent = `${file.name}: ${$('p', box).textContent}`;
      break;
    }
    etag = tag = a.etag;
    notes.push(...a.warnings.map(w => w.message));
  }
  if (tag) await refresh(tag);
  $('#tracks-status').textContent = notes.join(' ');
}

on('input', e => {
  const t = e.target, r = t.closest('[data-track]');
  if (t.name == 'nogenre' && t.checked) $('[name=genre]', r).value = '';
  if (t.name == 'genre' && r && t.value) $('[name=nogenre]', r).checked = false;
  update();
});

on('change', e => {
  const t = e.target, file = t.files?.[0];
  if (t.id == 'lrc-files' && file) addLyrics([...t.files]);
  else if (t.id == 'track-files' && file) addTracks([...t.files]);
  else if (t.id == 'extra-file' && file) {
    $('#extra-row').hidden = false;
    $('#extra-path').value = file.name;
    $('#extra-path').focus();
  } else if (file && t.dataset.url) run(t, file);
  if (t.files && t.id != 'extra-file') t.value = '';
  if (t.id == 'artist-name' && artist()) t.value = artist().value;
  update();
});

on('click', e => {
  const b = e.target.closest('button'), d = b?.dataset;
  if (!b) return;
  if (d.pick || d.open) target = b.closest('[data-track]');
  b.closest('[popover]')?.hidePopover();
  if ('close' in d) b.closest('dialog').close(b.value);
  if (d.pick) $('#' + d.pick).click();
  if (d.open) $('#' + d.open).showModal();
  if (d.method) run(b);
  if (d.action == 'reapply') {
    for (const n of $$('.notice', document)) n.hidden = true;
    refresh();
  }
  if (b.id == 'create-artist') {
    $('[name=new_artist]').value = $('#artist-name').value.trim();
    update();
  }
  if (b.id == 'rename-artist') rename();
  if (b.id == 'extra-add') {
    const path = $('#extra-path').value.trim(), box = $('#extras-notice');
    if (!path) return fail({ code: 'path_empty' }, box);
    call(`${base}/attachments?path=${encodeURIComponent(path)}`, 'POST', $('#extra-file').files[0], box).then(a => a && refresh(a.etag));
  }
});

// Enter in the title saves.
on('beforeinput', e => {
  if (e.target.id == 'album-title' && /Line|Para/.test(e.inputType)) {
    e.preventDefault();
    $('#save').click();
  }
});

// An image dropped on the cover replaces it; files dropped anywhere else
// on the editor of an album outside the trash are added as tracks.
for (const type of ['dragover', 'dragleave', 'drop']) {
  on(type, e => {
    const cover = e.target.closest?.('#cover-drop'), box = cover || editor.dataset.trashed == 'false' && e.target.closest?.('#editor') && $('.tracks');
    if (!box || !e.dataTransfer.types.includes('Files')) return;
    e.preventDefault();
    box.classList.toggle('is-drop', type == 'dragover');
    if (type == 'drop') cover ? run($('#cover-file'), e.dataTransfer.files[0]) : addTracks([...e.dataTransfer.files]);
  });
}

on('submit', async e => {
  if (e.target.id != 'metadata') return;
  e.preventDefault();
  const v = (name, root = editor) => $(`[name=${name}]`, root).value, own = s => s.trim() ? s : null;
  if (!artist() && !v('new_artist')) return fail({ code: 'artist' }, $('#head-notice'));
  wrong();
  const sent = new Map(fields().map(f => [key(f), value(f)]));
  busy = true;
  update();
  const album = await call(base, 'PUT', json({
    artist_id: v('artist_id') || null, new_artist: v('new_artist') || null, title: v('title'), year: v('year') ? +v('year') : null, genre: own(v('genre')),
    compilation: $('[name=compilation]').checked,
    tracks: $$('[data-track]').map(r => ({
      id: r.dataset.track, disc: +v('disc', r), no: +v('no', r), title: v('title', r),
      artist: own(v('artist', r)), genre: $('[name=nogenre]', r).checked ? '' : own(v('genre', r))
    }))
  }), $('#save-notice'));
  busy = false;
  if (!album) return update();
  saved = sent; // What was typed during the save stays an edit.
  shown = true;
  clearTimeout(timer);
  timer = setTimeout(() => { shown = false; update(); }, 2000);
  await refresh(album.etag);
});

addEventListener('beforeunload', e => { if (count()) e.preventDefault(); });
// An invalid field in a closed ⋯ menu (Disc): open it, so the browser can
// show why Save did nothing (N-279).
document.addEventListener('invalid', e => e.target.closest('[popover]:not(:popover-open)')?.showPopover(), true);

// The album's head lies on its own cover, blurred (v1.1.0): once the cover
// is decoded, its URL goes to the CSS (from script: the CSP forbids inline
// style attributes, not the CSSOM). No cover, or one that does not decode,
// means no field; a new cover replaces it.
async function glow() {
  const main = $('#main'), img = $('img.cover', editor);
  let lit = false;
  try {
    if (img) { await img.decode(); lit = true; }
  } catch { /* Not decodable: no field. */ }
  if (img != $('img.cover', editor)) return; // Replaced meanwhile.
  if (lit) main.style.setProperty('--cover', `url("${img.currentSrc}")`);
  main.toggleAttribute('data-glow', lit);
}

// The server's status every 2 s while a render is pending or running.
setInterval(async () => {
  const s = $('#status');
  if (document.hidden || s.dataset.pending != 'true') return;
  try {
    s.replaceWith(document.adoptNode($('#status', await page())));
  } catch { /* Retry on the next tick. */ }
}, 2000);

arm();
update();
glow();
