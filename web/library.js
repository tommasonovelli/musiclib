// Library enhancements (NOTES.md N-246, N-247): live search, loading more
// albums on scroll, and the open album under its row. Without this module
// the page is a plain server-rendered list of links and a GET form.
// Server HTML is parsed into an inert document and adopted node by node;
// catalog text is escaped once, by html/template. Everything this module
// writes itself goes through textContent.
const results = document.querySelector('#results');
const input = document.querySelector('#filter-q');
const announcer = document.querySelector('#results-status');
const reduced = matchMedia('(prefers-reduced-motion: reduce)');
let generation = 0; // A search replaces the results: older answers are dropped.
let opened = null; // { tile, panel }

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

async function fetchPage(url, signal) {
  try {
    const response = await fetch(url, { signal, headers: { Accept: 'text/html' } });
    if (!response.ok) throw new Error(String(response.status));
    return new DOMParser().parseFromString(await response.text(), 'text/html');
  } catch (error) {
    if (error.name !== 'AbortError') announce('The library didn’t respond. Try again in a moment.');
    return null;
  }
}

function announce(text) { if (announcer) announcer.textContent = text; }
function reveal() { for (const node of results.querySelectorAll('[data-js]')) node.hidden = false; }
function tiles() { return [...results.querySelectorAll('#grid > .tile')]; }

// Arrow keys, Enter and Esc turn the links into buttons that open in place;
// a modified click still opens the album page (new tab, new window).
function enhance(tile) {
  const link = tile.querySelector('.tile-link');
  link.setAttribute('role', 'button');
  link.setAttribute('aria-expanded', 'false');
  return tile;
}

// ---- The head compacts while stuck (owner, NOTES.md N-284) -----------------
// CSS does it with a scroll-state query where the browser has one. Elsewhere
// an empty sentinel just above the head says when the head is stuck: its
// place does not depend on the head's size, so compacting cannot move it.
const head = document.querySelector('.page-head:has(.search)');
if (head && !CSS.supports('container-type: scroll-state')) {
  const sentinel = element('div');
  head.before(sentinel);
  new IntersectionObserver(([entry]) => {
    head.classList.toggle('is-stuck', !entry.isIntersecting && entry.boundingClientRect.top < 0);
  }).observe(sentinel);
}

// ---- Live search ----------------------------------------------------------

let searchTimer, searchController;
async function search() {
  const form = input.form;
  const params = new URLSearchParams(new FormData(form));
  if (!params.get('q').trim()) params.delete('q');
  const url = '/' + (params.size ? `?${params}` : '');
  searchController?.abort();
  searchController = new AbortController();
  const current = ++generation;
  const doc = await fetchPage(url, searchController.signal);
  const fresh = doc?.querySelector('#results');
  if (!fresh || current !== generation) return;
  opened = null;
  results.replaceChildren(...[...fresh.childNodes].map(node => document.adoptNode(node)));
  tiles().forEach(enhance);
  reveal();
  history.replaceState(null, '', url);
  document.title = doc.title;
  const count = tiles().length;
  announce(count ? `${count} album${count === 1 ? '' : 's'}${document.querySelector('#more') ? ' and more' : ''}` : results.textContent.trim());
  watchMore();
}
if (input) {
  input.addEventListener('input', () => { clearTimeout(searchTimer); searchTimer = setTimeout(search, 250); });
  input.form.addEventListener('submit', event => { event.preventDefault(); clearTimeout(searchTimer); search(); });
}

// ---- Empty trash -------------------------------------------------------------
// The server deletes the trashed albums whose removal from the library
// folder is complete and counts the others; the trash view is then fetched
// again.

const emptyAsk = document.querySelector('#empty-ask');
reveal();
results.addEventListener('click', event => {
  if (!event.target.closest('#empty-trash')) return;
  emptyAsk.returnValue = '';
  emptyAsk.showModal();
});
let emptying = false;
emptyAsk?.addEventListener('close', async () => {
  if (emptyAsk.returnValue !== 'ok' || emptying) return;
  emptying = true;
  let data, error;
  try {
    const response = await fetch('/api/trash/empty', { method: 'POST', headers: { 'X-Musiclib-Request': '1' }, cache: 'no-store' });
    data = await response.json().catch(() => ({}));
    if (!response.ok) error = { status: response.status, ...data };
  } catch { error = { code: 'network' }; }
  emptying = false;
  const box = document.querySelector('#trash-error');
  if (error && box) {
    box.querySelector('p').textContent = error.status ? 'The trash wasn’t emptied. Try again.' : 'The library didn’t respond. Try again in a moment.';
    box.querySelector('details').hidden = !error.status;
    box.querySelector('pre').textContent = JSON.stringify(error, null, 2);
    box.hidden = false;
    box.focus();
    return;
  }
  await search();
  const note = document.querySelector('#trash-note'), n = data.waiting;
  if (note && n) note.textContent = `${n} album${n === 1 ? ' is' : 's are'} still being removed from the library folder, or the removal failed: empty the trash again once Activity is done.`;
  document.querySelector('#empty-trash')?.focus();
});

// ---- More albums on scroll: the sentinel is the real «Load more» link ----

let loading = false;
const observer = new IntersectionObserver(entries => {
  if (entries.some(entry => entry.isIntersecting)) loadMore();
}, { rootMargin: '0px 0px 800px 0px' });
function watchMore() {
  observer.disconnect();
  const more = document.querySelector('#more');
  if (more) observer.observe(more);
}
async function loadMore() {
  const more = document.querySelector('#more');
  if (!more || loading) return;
  loading = true;
  const current = generation;
  const doc = await fetchPage(more.getAttribute('href'));
  loading = false;
  if (!doc || current !== generation || !more.isConnected) return;
  const grid = document.querySelector('#grid');
  for (const tile of doc.querySelectorAll('#grid > .tile')) grid.append(enhance(document.adoptNode(tile)));
  const next = doc.querySelector('#more');
  if (next) more.setAttribute('href', next.getAttribute('href'));
  else more.parentElement.remove();
  if (opened) place(opened.panel, opened.tile);
  watchMore(); // Observing again reports the sentinel at once if it is still near.
}

// ---- Cover colours ----------------------------------------------------------
// A 32×32 canvas samples the same-origin cover; a histogram of colours
// quantised to 4 bits per channel picks the most frequent colour as the
// background and the most frequent other colour with a contrast of at
// least 4.5:1 as the text. If none reaches it, the text is white or black,
// whichever contrasts more (one of the two always reaches 4.5:1).

function luminance(rgb) {
  const [r, g, b] = rgb.map(v => { v /= 255; return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}
function contrast(a, b) {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
}
function coverColours(img) {
  const canvas = element('canvas');
  canvas.width = canvas.height = 32;
  const context = canvas.getContext('2d', { willReadFrequently: true });
  let data;
  try {
    context.drawImage(img, 0, 0, 32, 32);
    data = context.getImageData(0, 0, 32, 32).data;
  } catch { return null; }
  const bins = new Map();
  for (let i = 0; i < data.length; i += 4) {
    if (data[i + 3] < 128) continue;
    const key = (data[i] >> 4) << 8 | (data[i + 1] >> 4) << 4 | data[i + 2] >> 4;
    const bin = bins.get(key) || { n: 0, sum: [0, 0, 0] };
    bin.n++;
    for (let c = 0; c < 3; c++) bin.sum[c] += data[i + c];
    bins.set(key, bin);
  }
  const ranked = [...bins.values()].sort((a, b) => b.n - a.n).map(bin => bin.sum.map(v => Math.round(v / bin.n)));
  if (!ranked.length) return null;
  const bg = ranked[0];
  const ink = ranked.find(colour => contrast(colour, bg) >= 4.5)
    || (contrast([255, 255, 255], bg) >= contrast([0, 0, 0], bg) ? [255, 255, 255] : [0, 0, 0]);
  return { bg, ink };
}
async function paint(panel, img) {
  try { await img.decode(); } catch { return; }
  const colours = coverColours(img);
  if (!colours || !panel.isConnected) return;
  panel.style.setProperty('--cover-bg', `rgb(${colours.bg.join(' ')})`);
  panel.style.setProperty('--cover-ink', `rgb(${colours.ink.join(' ')})`);
  panel.dataset.painted = 'true';
}

// ---- The open album ---------------------------------------------------------

// The panel goes after the last cover of the clicked cover's row.
function place(panel, tile) {
  const row = tiles().filter(other => other.offsetTop === tile.offsetTop);
  const last = row[row.length - 1];
  if (last.nextElementSibling !== panel) last.after(panel);
  const grid = panel.parentElement.getBoundingClientRect();
  const art = tile.querySelector('.art').getBoundingClientRect();
  panel.style.setProperty('--notch-x', `${art.left + art.width / 2 - grid.left}px`);
}

function statusLine(tile) {
  const word = tile.querySelector('.tile-status')?.textContent;
  if (!word) return null;
  const line = element('p', 'panel-status');
  line.append(element('span', 'dot'), element('span', '', word));
  return line;
}

// A track's duration as the album page shows it (N-302): m:ss, h:mm:ss from
// one hour, to the nearest second; an en dash while unknown.
function duration(ms) {
  const time = element('span', 'track-time');
  if (ms === null) {
    const dash = element('span', '', '–');
    dash.setAttribute('aria-hidden', 'true');
    time.append(dash, element('span', 'sr-only', 'Duration unknown'));
    return time;
  }
  const s = Math.round(ms / 1000), two = n => String(n).padStart(2, '0'), m = Math.floor(s / 60);
  time.append(element('span', 'sr-only', 'Duration '), `${s >= 3600 ? `${Math.floor(s / 3600)}:${two(m % 60)}` : m}:${two(s % 60)}`);
  return time;
}

function tracks(album) {
  const list = element('ol', 'panel-tracks');
  const discs = new Set(album.tracks.map(track => track.disc)).size > 1;
  let disc;
  for (const track of album.tracks) {
    if (discs && track.disc !== disc) {
      disc = track.disc;
      list.append(element('li', 'disc', `Disc ${disc}`));
    }
    const item = element('li');
    item.append(element('span', 'track-no', String(track.no)));
    const title = element('span', 'track-title', track.title);
    if (track.artist && track.artist !== album.artist_name) title.append(' ', element('span', 'track-artist', track.artist));
    item.append(title, duration(track.duration_ms));
    list.append(item);
  }
  return list;
}

async function fill(panel, tile, info) {
  let album;
  try {
    const response = await fetch(`/api/albums/${encodeURIComponent(tile.dataset.album)}`, { cache: 'no-store' });
    if (!response.ok) throw new Error(String(response.status));
    album = await response.json();
  } catch {
    info.querySelector('.panel-edit').before(element('p', 'panel-error', 'This album didn’t open because the library isn’t responding. Try again in a moment.'));
    return;
  }
  if (!panel.isConnected) return;
  const meta = [album.genre, album.year].filter(value => value !== null && value !== '').join(', ');
  if (meta) info.querySelector('.panel-artist').after(element('p', 'panel-meta', meta));
  info.querySelector('.panel-edit').before(tracks(album));
}

function build(tile) {
  const panel = element('li', 'album-panel');
  panel.id = 'album-panel';
  panel.dataset.status = tile.dataset.status;
  const clip = element('div', 'panel-clip');
  const body = element('div', 'panel-body');
  const info = element('div', 'panel-info');
  const title = element('h2', 'panel-title', tile.querySelector('.tile-title').textContent);
  title.id = 'album-panel-title';
  title.tabIndex = -1;
  panel.setAttribute('role', 'region');
  panel.setAttribute('aria-labelledby', title.id);
  info.append(title, element('p', 'panel-artist', tile.querySelector('.tile-artist').textContent));
  const status = statusLine(tile);
  if (status) info.append(status);
  const edit = element('a', 'btn panel-edit', 'Edit album');
  edit.href = tile.querySelector('.tile-link').getAttribute('href');
  info.append(edit);
  const source = tile.querySelector('.art img');
  if (source) {
    const cover = element('img', 'panel-cover');
    cover.alt = '';
    cover.width = cover.height = 240;
    cover.src = source.currentSrc || source.src;
    body.append(cover);
    panel.dataset.cover = 'true';
    paint(panel, source);
  }
  body.append(info);
  clip.append(body);
  panel.append(clip);
  fill(panel, tile, info);
  return panel;
}

function settle(panel, done) {
  let called = false;
  const once = () => { if (!called) { called = true; done(); } };
  panel.addEventListener('transitionend', event => { if (event.target === panel) once(); });
  setTimeout(once, reduced.matches ? 0 : 450); // No transitionend without a transition.
}

function close(focus) {
  if (!opened) return;
  const { tile, panel } = opened;
  opened = null;
  const link = tile.querySelector('.tile-link');
  link.setAttribute('aria-expanded', 'false');
  link.removeAttribute('aria-controls');
  panel.removeAttribute('id');
  panel.inert = true;
  panel.classList.remove('is-open');
  settle(panel, () => panel.remove());
  if (focus) link.focus();
}

function open(tile) {
  if (opened?.tile === tile) { close(true); return; }
  const previous = opened;
  if (previous) close(false);
  const panel = build(tile);
  const link = tile.querySelector('.tile-link');
  opened = { tile, panel };
  place(panel, tile);
  // Another cover of the same row: the panel is replaced, not reopened.
  const sameRow = previous && panel.nextElementSibling === previous.panel;
  if (sameRow) {
    previous.panel.remove();
    panel.classList.add('is-open');
  } else {
    panel.getBoundingClientRect(); // Start from the closed state.
    panel.classList.add('is-open');
  }
  link.setAttribute('aria-expanded', 'true');
  link.setAttribute('aria-controls', panel.id);
  panel.querySelector('.panel-title').focus({ preventScroll: true });
  settle(panel, () => panel.isConnected && panel.scrollIntoView({ block: 'nearest', behavior: reduced.matches ? 'auto' : 'smooth' }));
}

function move(from, key) {
  const all = tiles();
  const index = all.indexOf(from);
  const columns = all.filter(tile => tile.offsetTop === all[0].offsetTop).length;
  const target = { ArrowLeft: index - 1, ArrowRight: index + 1, ArrowUp: index - columns, ArrowDown: index + columns, Home: 0, End: all.length - 1 }[key];
  if (target === undefined) return false;
  all[Math.max(0, Math.min(all.length - 1, target))]?.querySelector('.tile-link').focus();
  return true;
}

if (results) {
  tiles().forEach(enhance);
  watchMore();
  results.addEventListener('click', event => {
    const edit = event.target.closest('.panel-edit');
    if (edit) {
      // Name only this cover, so it flies into the editor's (N-247).
      for (const named of document.querySelectorAll('.panel-cover')) named.style.removeProperty('view-transition-name');
      edit.closest('.album-panel').querySelector('.panel-cover')?.style.setProperty('view-transition-name', 'album-cover');
      return;
    }
    if (event.target.closest('#more')) { event.preventDefault(); loadMore(); return; }
    const link = event.target.closest('.tile-link');
    if (!link || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    open(link.closest('.tile'));
  });
  results.addEventListener('keydown', event => {
    if (event.key === 'Escape' && opened) {
      event.preventDefault();
      close(true);
      return;
    }
    const link = event.target.closest('.tile-link');
    if (!link) return;
    if (event.key === ' ') { event.preventDefault(); open(link.closest('.tile')); return; }
    if (!event.altKey && !event.ctrlKey && !event.metaKey && move(link.closest('.tile'), event.key)) event.preventDefault();
  });
  let frame;
  addEventListener('resize', () => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => opened && place(opened.panel, opened.tile));
  });
}
