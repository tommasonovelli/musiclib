// The sidebar collapses to its icons (NOTES.md N-272) and the theme is
// light, dark or the system's; both choices are remembered per browser. A
// classic script loaded in <head>, not a module: it runs before the body is
// parsed, so the remembered state is on <html> before the first paint and
// nothing moves or flashes on load. Without it the sidebar stays expanded,
// the theme follows the system and both buttons stay hidden.
(() => {
  const root = document.documentElement;
  const themes = ['system', 'light', 'dark'];
  let collapsed = false;
  let theme = 'system';
  try {
    collapsed = localStorage.getItem('musiclib.sidebar') === 'collapsed';
    theme = localStorage.getItem('musiclib.theme') || 'system';
  } catch { /* no storage: expanded, the system's theme */ }
  if (!themes.includes(theme)) theme = 'system';
  function remember(key, value) {
    try {
      if (value) localStorage.setItem(key, value); else localStorage.removeItem(key);
    } catch { /* not remembered */ }
  }
  function show() {
    root.dataset.sidebar = collapsed ? 'collapsed' : 'expanded';
    if (theme === 'system') delete root.dataset.theme; else root.dataset.theme = theme;
    // Form controls and scrollbars follow the chosen theme too.
    document.querySelector('meta[name="color-scheme"]').content = theme === 'system' ? 'light dark' : theme;
    const toggle = document.getElementById('sidebar-toggle');
    if (toggle) {
      toggle.setAttribute('aria-expanded', String(!collapsed));
      toggle.querySelector('.nav-label').textContent = `${collapsed ? 'Expand' : 'Collapse'} sidebar`;
    }
    const button = document.getElementById('theme-toggle');
    if (button) {
      button.querySelector('use').setAttribute('href', `#i-theme-${theme}`);
      button.querySelector('.nav-label').textContent = `Theme: ${theme[0].toUpperCase()}${theme.slice(1)}`;
    }
  }
  show();
  document.addEventListener('DOMContentLoaded', show);
  document.addEventListener('click', event => {
    if (event.target.closest('#sidebar-toggle')) {
      collapsed = !collapsed;
      remember('musiclib.sidebar', collapsed ? 'collapsed' : 'expanded');
    } else if (event.target.closest('#theme-toggle')) {
      theme = themes[(themes.indexOf(theme) + 1) % themes.length];
      remember('musiclib.theme', theme === 'system' ? '' : theme);
    } else {
      return;
    }
    show();
  });
})();
