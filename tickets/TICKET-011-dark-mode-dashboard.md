# TICKET-011: Dark Mode Toggle for Dashboard

**Type:** feature
**Priority:** P2
**Estimate:** S (2 days)
**Epic:** Frontend UX Improvements
**Labels:** p2, sprint-9, frontend, ux, accessibility
**Status:** TODO

## Problem Statement

The dashboard uses a fixed light theme with hardcoded color values in CSS and JavaScript canvas drawing code. Developers who work in dark mode (a majority of developers, according to Stack Overflow surveys) find the dashboard uncomfortable to use for extended periods. There is no way to switch to a dark theme.

Additionally, the dashboard does not respect the `prefers-color-scheme: dark` CSS media query, so users with system-wide dark mode set do not automatically get the dark theme.

## Context

Static assets are in `web/static/`. The dashboard HTML is `web/static/index.html`. Styles are in `web/static/css/`. JavaScript is in `web/static/js/`.

Color values are currently hardcoded in:
1. CSS stylesheets (background colors, text colors, border colors)
2. JavaScript canvas drawing code (goroutine state colors, connection lines, etc.)

## Goals

1. Implement dark mode using CSS custom properties (variables).
2. Add a toggle button in the dashboard header.
3. Persist the user's preference in `localStorage`.
4. Respect `prefers-color-scheme: dark` as the default when no `localStorage` preference is set.
5. Update the canvas rendering JavaScript to read theme colors from CSS custom properties (or a shared color map derived from the current theme).

## Non-Goals

- Custom color themes beyond light and dark.
- Server-side theme configuration.
- Changing the dashboard layout.

## Technical Design

### CSS Variables

Define color variables in `:root` and `[data-theme="dark"]`:

```css
:root {
    --bg-primary: #ffffff;
    --bg-secondary: #f5f5f5;
    --bg-card: #ffffff;
    --text-primary: #1a1a1a;
    --text-secondary: #666666;
    --border-color: #e0e0e0;
    --accent-color: #2563eb;
    --success-color: #16a34a;
    --error-color: #dc2626;
    --warning-color: #d97706;
    --canvas-bg: #f8f9fa;
    --canvas-node-free: #22c55e;
    --canvas-node-held: #ef4444;
    --canvas-node-waiting: #f59e0b;
    --shadow: 0 2px 4px rgba(0,0,0,0.1);
}

[data-theme="dark"] {
    --bg-primary: #0f172a;
    --bg-secondary: #1e293b;
    --bg-card: #1e293b;
    --text-primary: #f1f5f9;
    --text-secondary: #94a3b8;
    --border-color: #334155;
    --accent-color: #3b82f6;
    --success-color: #4ade80;
    --error-color: #f87171;
    --warning-color: #fbbf24;
    --canvas-bg: #0f172a;
    --canvas-node-free: #4ade80;
    --canvas-node-held: #f87171;
    --canvas-node-waiting: #fbbf24;
    --shadow: 0 2px 4px rgba(0,0,0,0.4);
}
```

### Theme Toggle

```html
<button id="theme-toggle" aria-label="Toggle dark mode" title="Toggle dark mode">
    <span id="theme-icon">☀️</span>
</button>
```

```javascript
const THEME_KEY = 'syncprim_theme';

function getPreferredTheme() {
    const stored = localStorage.getItem(THEME_KEY);
    if (stored) return stored;
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function applyTheme(theme) {
    document.documentElement.setAttribute('data-theme', theme);
    document.getElementById('theme-icon').textContent = theme === 'dark' ? '🌙' : '☀️';
}

function toggleTheme() {
    const current = document.documentElement.getAttribute('data-theme') || 'light';
    const next = current === 'dark' ? 'light' : 'dark';
    localStorage.setItem(THEME_KEY, next);
    applyTheme(next);
    updateCanvasColors(); // re-read CSS variables for canvas
}

document.getElementById('theme-toggle').addEventListener('click', toggleTheme);

// Apply on page load
applyTheme(getPreferredTheme());

// React to system theme changes
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', (e) => {
    if (!localStorage.getItem(THEME_KEY)) {
        applyTheme(e.matches ? 'dark' : 'light');
    }
});
```

### Canvas Color Synchronization

The canvas drawing code must read colors from CSS custom properties:

```javascript
function getCSSVar(name) {
    return getComputedStyle(document.documentElement)
        .getPropertyValue(name).trim();
}

function updateCanvasColors() {
    canvasColors = {
        background: getCSSVar('--canvas-bg'),
        nodeFree: getCSSVar('--canvas-node-free'),
        nodeHeld: getCSSVar('--canvas-node-held'),
        nodeWaiting: getCSSVar('--canvas-node-waiting'),
        text: getCSSVar('--text-primary'),
        line: getCSSVar('--border-color'),
    };
}
```

Call `updateCanvasColors()` on theme toggle and on initial load.

## Backend Implementation

None. This is a purely frontend change. The embedded static files in `web/static/` are served by the server via `embed.FS`.

## Frontend Implementation

1. Refactor all CSS color values to use `var(--variable-name)`.
2. Add `:root` and `[data-theme="dark"]` blocks with all color variables.
3. Add theme toggle button to `index.html` header.
4. Implement theme persistence in JavaScript.
5. Update canvas drawing code to use `getCSSVar()` for colors.
6. Add `prefers-color-scheme` media query listener.

## Database / State Changes

None. Theme preference is stored in `localStorage` only.

## API Changes

None.

## Infrastructure Requirements

None.

## Edge Cases

- `localStorage` disabled (private browsing in some browsers): `localStorage.setItem` throws. Wrap in try/catch; fall back to `prefers-color-scheme` default.
- User changes system theme while browser is open: the `matchMedia` listener applies the new theme if no explicit `localStorage` preference is set.
- Canvas renders before CSS variables are initialized: `getCSSVar` is called after DOM content loaded event, so CSS is guaranteed to be parsed.

## Failure Handling

- `localStorage` write fails: catch the exception, apply the theme in memory only for this session.
- `getComputedStyle` returns empty string for unknown variable: `getCSSVar` returns `""`. Canvas drawing falls back to the hardcoded color value.

## Security Considerations

None. Theme preference is stored client-side. No server data is involved.

## Testing Plan

### Unit Tests

None for a CSS/JS frontend change. Browser-level testing is more appropriate.

### Integration Tests

Verify that the embedded static files include the updated CSS and JavaScript after the change.

### E2E Tests

Manual browser tests:
1. Load dashboard; verify it matches system dark/light preference.
2. Click toggle; verify theme changes.
3. Reload page; verify preference is remembered.
4. Open in incognito (no localStorage); verify system preference is respected.
5. Change system dark mode setting while dashboard is open; verify auto-update (only if no explicit preference saved).

Automated: Use Playwright or Puppeteer to take screenshots in light and dark modes and compare against reference images.

## Monitoring Requirements

None.

## Logging Requirements

None.

## Metrics to Track

None.

## Rollback Plan

Revert CSS and JavaScript changes. The toggle button disappears and the hardcoded light theme returns.

## Acceptance Criteria

- [ ] Dashboard has a dark mode toggle button in the header
- [ ] Dark mode activates when system `prefers-color-scheme: dark` is set and no explicit preference is saved
- [ ] Theme preference persists across page reloads via `localStorage`
- [ ] Canvas goroutine colors update correctly in dark mode
- [ ] All text remains readable in both modes (WCAG AA contrast ratio ≥ 4.5:1 for normal text)
- [ ] Toggle button has `aria-label` for accessibility
- [ ] No hardcoded colors remain in CSS (all use custom properties)

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Static assets rebuilt and embedded
- [ ] Manual dark mode test passed
- [ ] CHANGELOG entry written
