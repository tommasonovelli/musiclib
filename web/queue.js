// Import and Activity (round 21, NOTES.md N-286 to N-291). The server
// renders both pages, so they read, browse and switch tabs without this
// module. It adds the actions, and keeps the page current while work runs:
// every two seconds it fetches the page itself and adopts what changed. A
// row is replaced only when its version changed, so focus, an open
// «Details», a half-typed title and the scroll survive a poll. Catalog text
// is escaped once, by html/template; this module writes only textContent.
const $ = (s, root = document) => root.querySelector(s);
const $$ = (s, root = document) => [...root.querySelectorAll(s)];
const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;

// ---- Errors: one sentence where it happened, the answer in «Details» -------

const sentences = {
  network: 'The library isn’t responding. Try again in a moment.',
  job_in_progress: 'This album is already being imported again.',
  job_not_retryable: 'This album was imported in the meantime: reload the page.',
  job_not_dismissable: 'This was retried in the meantime: reload the page.',
  text_empty: 'The name is empty: fill it in.',
  text_control_char: 'The name has a line break or an invisible character: remove it.',
  text_too_long: 'The name is too long.'
};
// A notice exists only while there is an error (N-296): it is made right
// after the control or line where it happened, whole before it goes in, so
// the alert is announced once with its words; it goes when that place
// next succeeds.
const noticeAfter = at => (at.nextElementSibling?.classList.contains('notice') ? at.nextElementSibling : null);
const clear = at => noticeAfter(at)?.remove();
function fail(at, e, fallback, focus = true) {
  let box = noticeAfter(at);
  const fresh = !box;
  if (fresh) {
    box = document.createElement('div');
    box.className = 'notice';
    box.setAttribute('role', 'alert');
    box.tabIndex = -1;
    const details = document.createElement('details'), summary = document.createElement('summary');
    summary.textContent = 'Details';
    details.append(summary, document.createElement('pre'));
    box.append(document.createElement('p'), details);
  }
  const [p, details] = box.children;
  p.textContent = sentences[e.code] || fallback;
  details.hidden = !e.status;
  details.open = false;
  $('pre', box).textContent = JSON.stringify(e, null, 2);
  if (fresh) at.after(box);
  if (focus) box.focus();
}
async function send(url, body) {
  const headers = { 'X-Musiclib-Request': '1' };
  if (body) headers['Content-Type'] = 'application/json';
  try {
    const r = await fetch(url, { method: 'POST', headers, body: body && JSON.stringify(body), cache: 'no-store' });
    const data = await r.json().catch(() => ({}));
    return r.ok ? { data } : { error: { status: r.status, ...data, url } };
  } catch { return { error: { code: 'network', url } }; }
}

// ---- Relative times, rewritten in place (N-290) ----------------------------

const full = new Intl.DateTimeFormat('en-GB', { dateStyle: 'long', timeStyle: 'short' });
const date = new Intl.DateTimeFormat('en-GB', { dateStyle: 'long' });
function relative(d) {
  const s = (Date.now() - d) / 1000;
  if (s < 60) return 'just now';
  if (s < 3600) return `${plural(Math.floor(s / 60), 'minute')} ago`;
  if (s < 86400) return `${plural(Math.floor(s / 3600), 'hour')} ago`;
  if (s < 2592000) return `${plural(Math.floor(s / 86400), 'day')} ago`;
  return date.format(d);
}
function times() {
  for (const t of $$('time[datetime]')) {
    const d = new Date(t.dateTime);
    t.title = full.format(d);
    const words = relative(d);
    if (t.textContent !== words) t.textContent = words;
  }
}

// ---- Keeping the page current (N-291) --------------------------------------

const served = new Map(); // id of a [data-live] element -> the server's HTML
const remember = () => { for (const el of $$('[data-live]')) served.set(el.id, el.outerHTML); };
function enable(root) {
  for (const el of $$('[data-js]', root)) el.hidden = false;
  return root;
}
const focusable = row => $('a, button:not([disabled]), input:not([type=hidden]), summary', row);

// Parts ([data-part]: a group of Activity, its «And N more», an empty
// state) are rendered only while they have something to say (N-296): one
// the server no longer sends goes, a new one goes in beside its server
// neighbour that is already here.
function spot(f) { // where f goes: after the nearest server sibling that is here
  for (let s = f.previousElementSibling; s; s = s.previousElementSibling) {
    const here = s.id && document.getElementById(s.id);
    if (here) return node => here.after(node);
  }
  for (let s = f.nextElementSibling; s; s = s.nextElementSibling) {
    const here = s.id && document.getElementById(s.id);
    if (here) return node => here.before(node);
  }
  const parent = document.getElementById(f.parentElement.id);
  return node => parent?.append(node);
}

function patch(doc) {
  let lost = null; // the neighbour of a focused row or part that went away
  for (const el of $$('[data-part]')) {
    if (doc.getElementById(el.id)) continue;
    if (el.contains(document.activeElement)) lost = el.nextElementSibling || $('main');
    el.remove();
  }
  for (const f of [...doc.querySelectorAll('[data-part]')]) {
    if (document.getElementById(f.id)) continue;
    const node = document.importNode(f, true); // a copy: the server's page keeps its order for the next part
    spot(f)(node);
    enable(node);
    for (const el of $$('[data-live]', node)) served.set(el.id, el.outerHTML);
  }
  for (const el of $$('[data-live]')) {
    const fresh = doc.getElementById(el.id);
    if (!fresh || fresh.outerHTML === served.get(el.id)) continue;
    served.set(el.id, fresh.outerHTML);
    el.replaceWith(document.adoptNode(fresh));
  }
  for (const el of $$('[data-hide]')) {
    const fresh = doc.getElementById(el.id);
    if (fresh) el.hidden = fresh.hidden;
  }
  for (const list of $$('[data-rows]')) {
    const fresh = doc.getElementById(list.id);
    if (!fresh) continue;
    const old = new Map($$(':scope > [data-key]', list).map(row => [row.dataset.key, row]));
    let prev = null;
    for (const f of [...fresh.children]) {
      let row = old.get(f.dataset.key);
      old.delete(f.dataset.key);
      if (!row || row.dataset.version !== f.dataset.version) {
        const next = enable(document.adoptNode(f));
        if (row) {
          if (row.contains(document.activeElement)) lost = next;
          row.replaceWith(next);
        } else if (prev) prev.after(next);
        else list.prepend(next);
        row = next;
      }
      prev = row;
    }
    for (const row of old.values()) {
      if (row.contains(document.activeElement)) lost = row.nextElementSibling || row.previousElementSibling || list;
      row.remove();
    }
  }
  if (lost && !document.activeElement?.closest('main')) (focusable(lost) || $('[aria-selected=true]') || $('main')).focus();
  times();
  if (!chosen) {
    const open = tabs.find(t => t.getAttribute('aria-selected') === 'true');
    const first = tabs.find(t => $(`#rows-${t.dataset.tab}`).children.length);
    if (open && first && !$(`#rows-${open.dataset.tab}`).children.length) select(first);
  }
}

// Answers are applied in the order they were requested: one older than the
// last applied is dropped, so a report requested before an accepted change
// never overwrites one requested after it (N-210, N-291).
let requested = 0, applied = 0;
let kicked = false; // Work was just queued: poll once more even if the dot is off.

async function refresh(background = false) {
  const seq = ++requested;
  let doc;
  try {
    const r = await fetch(location.pathname + location.search, { cache: 'no-store', headers: { Accept: 'text/html' } });
    if (!r.ok) throw { status: r.status, url: r.url };
    doc = new DOMParser().parseFromString(await r.text(), 'text/html');
  } catch (e) {
    const at = $('#progress') || $('#activity-note');
    if (seq > applied && at) fail(at, e.status ? e : { code: 'network' }, sentences.network, !background);
    return;
  }
  if (seq < applied) return;
  applied = seq;
  kicked = false;
  const at = $('#progress') || $('#activity-note');
  if (at) clear(at);
  patch(doc);
}
function accepted() {
  kicked = true; // Work was queued: poll until the page says it is done.
  return refresh();
}

// ---- Tabs: the ARIA tabs pattern over links that work without JS ------------

const tabs = $$('.tab');
let chosen = false;
function select(tab, focus = false) {
  for (const t of tabs) {
    const on = t === tab;
    t.setAttribute('aria-selected', String(on));
    t.tabIndex = on ? 0 : -1;
    $(`#panel-${t.dataset.tab}`).hidden = !on;
  }
  if (!focus) return;
  tab.focus();
  const url = new URL(location);
  url.searchParams.set('tab', tab.dataset.tab);
  history.replaceState(null, '', url);
}
if (tabs.length) {
  const list = $('.tablist');
  list.setAttribute('role', 'tablist');
  for (const t of tabs) {
    t.setAttribute('role', 'tab');
    t.setAttribute('aria-controls', `panel-${t.dataset.tab}`);
    $(`#panel-${t.dataset.tab}`).setAttribute('role', 'tabpanel');
  }
  select(tabs.find(t => t.hasAttribute('aria-current')) || tabs[0]);
  for (const t of tabs) t.removeAttribute('aria-current');
  list.addEventListener('click', e => {
    const t = e.target.closest('.tab');
    if (!t || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey || e.button) return;
    e.preventDefault();
    chosen = true;
    select(t, true);
  });
  list.addEventListener('keydown', e => {
    const i = tabs.indexOf(document.activeElement);
    const to = { ArrowRight: i + 1, ArrowLeft: i - 1, Home: 0, End: tabs.length - 1, ' ': i }[e.key];
    if (i < 0 || to === undefined) return;
    e.preventDefault();
    chosen = true;
    select(tabs[(to + tabs.length) % tabs.length], true);
  });
}

// ---- Import --------------------------------------------------------------------

const start = $('#import-start');
if (start) {
  const id = crypto.randomUUID(); // One per page: a lost answer is sent again with it (N-210).
  start.addEventListener('click', async () => {
    start.disabled = true;
    const { data, error } = await send('/api/imports', { id, path: start.dataset.path });
    start.disabled = false;
    if (error) return fail($('.start-note'), error, 'The import didn’t start. Try again.');
    location.assign(`/import?batch=${encodeURIComponent(data.id)}`);
  });
}
$('.folders')?.addEventListener('keydown', e => {
  const links = $$('a.folder-row');
  const i = links.indexOf(document.activeElement);
  if (i < 0 || (e.key !== 'ArrowDown' && e.key !== 'ArrowUp')) return;
  e.preventDefault();
  links[Math.min(links.length - 1, Math.max(0, i + (e.key === 'ArrowDown' ? 1 : -1)))].focus();
});

// A row's one fix, and «Dismiss».
async function act(control, url, body, fallback, starts) {
  chosen = true; // The user is working in this tab: it stays open.
  const row = control.closest('.row');
  const buttons = $$('button', row);
  for (const b of buttons) b.disabled = true;
  const { error } = await send(url, body);
  for (const b of buttons) b.disabled = false;
  const at = $('.row-actions', row);
  if (error) return fail(at, error, fallback);
  clear(at);
  return starts ? accepted() : refresh();
}
document.addEventListener('submit', e => {
  const form = e.target.closest('form.fix');
  if (!form) return;
  e.preventDefault();
  // Exactly the two overrides of §7.3; an empty one goes back to the tags.
  const body = { artist: form.elements.namedItem('artist').value || null, title: form.elements.namedItem('title').value || null };
  act(form, `/api/jobs/${encodeURIComponent(form.dataset.job)}/retry`, body, 'The import didn’t start again. Try again.', true);
});
document.addEventListener('click', e => {
  const b = e.target.closest('[data-retry], [data-dismiss]');
  if (!b) return;
  if (b.dataset.retry) act(b, `/api/jobs/${encodeURIComponent(b.dataset.retry)}/retry`, undefined, 'It didn’t start again. Try again.', true);
  else act(b, `/api/jobs/${encodeURIComponent(b.dataset.dismiss)}/dismiss`, undefined, 'It wasn’t dismissed. Try again.', false);
});

// ---- Activity ------------------------------------------------------------------

document.addEventListener('click', async e => { // Delegated: the group comes and goes (N-296).
  const b = e.target.closest('#retry-all');
  if (!b) return;
  b.disabled = true;
  const { data, error } = await send('/api/jobs/retry-failed');
  b.disabled = false;
  if (error) return fail(b.closest('.group-head'), error, 'Nothing was queued again. Try again.');
  clear(b.closest('.group-head'));
  $('#activity-note').textContent = `${plural(data.retried, 'album')} queued again`;
  accepted();
});
const ask = $('#rebuild-ask');
$('#rebuild')?.addEventListener('click', () => {
  ask.returnValue = '';
  ask.showModal();
});
ask?.addEventListener('close', async () => {
  if (ask.returnValue !== 'ok') return;
  const { data, error } = await send('/api/render-all');
  if (error) return fail($('#rebuild-note'), error, 'The library folder wasn’t rebuilt. Try again.');
  clear($('#rebuild-note'));
  $('#rebuild-note').textContent = `${plural(data.enqueued, 'album')} queued`;
  accepted();
});

enable(document);
remember();
times();
setInterval(times, 30000);
(function poll() {
  setTimeout(async () => {
    if ((kicked || !$('#nav-active').hidden) && !document.hidden) await refresh(true);
    poll();
  }, 2000);
})();
