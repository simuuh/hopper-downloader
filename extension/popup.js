// Hopper browser extension — icon popup.
//
// Reachability check hits GET /api/v1/version, which hopperd deliberately
// serves unauthenticated (see docs/ARCHITECTURE.md/api/server.go's handleVersion) —
// the same rationale as /healthz. An API token is sent when one is
// configured (harmless, ignored by that handler) but its absence must
// not be treated as "unreachable": a fresh install with no token yet
// pasted in should still be able to see that hopperd itself is up.
const DEFAULT_TARGET = 'http://localhost:9667';
const VERSION_CHECK_TIMEOUT_MS = 3000;

function timeAgo(timestamp) {
  const minutes = Math.round((Date.now() - timestamp) / 60000);
  if (minutes < 1) return 'just now';
  if (minutes === 1) return '1 min ago';
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours === 1) return '1 hour ago';
  if (hours < 24) return `${hours} hours ago`;
  const days = Math.round(hours / 24);
  return days === 1 ? '1 day ago' : `${days} days ago`;
}

async function loadStatus() {
  const { targetUrl } = await chrome.storage.sync.get({ targetUrl: DEFAULT_TARGET });
  const { apiToken } = await chrome.storage.local.get({ apiToken: '' });

  const dot = document.getElementById('status-dot');
  const text = document.getElementById('status-text');
  document.getElementById('open-ui').dataset.url = targetUrl;

  try {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), VERSION_CHECK_TIMEOUT_MS);
    const headers = apiToken ? { Authorization: `Bearer ${apiToken}` } : {};
    const resp = await fetch(`${targetUrl}/api/v1/version`, { headers, signal: controller.signal });
    clearTimeout(timer);
    if (!resp.ok) {
      throw new Error(`HTTP ${resp.status}`);
    }
    const data = await resp.json();
    dot.className = 'dot dot-on';
    text.textContent = `Connected to ${targetUrl} (v${data.version || '?'})`;
    text.title = text.textContent;
  } catch {
    dot.className = 'dot dot-off';
    text.textContent = `Not reachable (${targetUrl})`;
    text.title = text.textContent;
  }
}

async function loadLastCnl() {
  const { lastCnl } = await chrome.storage.local.get({ lastCnl: null });
  const el = document.getElementById('last-cnl');
  if (!lastCnl) {
    el.textContent = 'No recent imports';
    return;
  }

  const countLabel =
    lastCnl.linkCount != null ? `${lastCnl.linkCount} link${lastCnl.linkCount === 1 ? '' : 's'}` : 'CnL import';
  const sourceLabel = lastCnl.source ? ` from ${lastCnl.source}` : '';
  const statusLabel = lastCnl.ok === false ? ' (failed)' : '';
  el.textContent = `Last: ${countLabel}${sourceLabel} (${timeAgo(lastCnl.timestamp)})${statusLabel}`;
}

document.getElementById('version').textContent = `v${chrome.runtime.getManifest().version}`;

document.getElementById('open-ui').addEventListener('click', (e) => {
  const url = e.currentTarget.dataset.url;
  if (url) {
    chrome.tabs.create({ url });
  }
});

document.getElementById('open-options').addEventListener('click', () => {
  chrome.runtime.openOptionsPage();
});

loadStatus();
loadLastCnl();
