// The sidebar collapses to its icons; the choice is remembered per browser
// (NOTES.md N-272). A classic script loaded in <head>, not a module: it runs
// before the body is parsed, so the remembered state is on <html> before the
// first paint and nothing moves on load. Without it the sidebar stays
// expanded and the toggle stays hidden.
(() => {
  const root = document.documentElement;
  let collapsed = false;
  try { collapsed = localStorage.getItem('musiclib.sidebar') === 'collapsed'; } catch { /* no storage: expanded */ }
  function show() {
    root.dataset.sidebar = collapsed ? 'collapsed' : 'expanded';
    const button = document.getElementById('sidebar-toggle');
    if (!button) return;
    button.setAttribute('aria-expanded', String(!collapsed));
    button.querySelector('.nav-label').textContent = `${collapsed ? 'Expand' : 'Collapse'} sidebar`;
  }
  show();
  document.addEventListener('DOMContentLoaded', show);
  document.addEventListener('click', event => {
    if (!event.target.closest('#sidebar-toggle')) return;
    collapsed = !collapsed;
    try { localStorage.setItem('musiclib.sidebar', collapsed ? 'collapsed' : 'expanded'); } catch { /* not remembered */ }
    show();
  });
})();
