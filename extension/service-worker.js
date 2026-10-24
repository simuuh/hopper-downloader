// Hopper browser extension — MV3 service worker.
//
// Three independent jobs:
//  1. Primary CnL path: receive { type: "cnl" } messages relayed by
//     content-script.js (itself relaying from inject.js's MAIN-world
//     fetch/XMLHttpRequest override — see those files' header comments)
//     over a persistent chrome.runtime.connect port, and forward the
//     request to the real hopperd's CnL listener from here, in the
//     service worker's own execution context. A service worker's fetch()
//     is not subject to page-level Mixed-Content/Shields rules at all
//     (it's not rendering or scripting a page), so this works in Brave
//     and hardened configurations that block the old
//     declarativeNetRequest-only approach outright.
//     Deliberately a port, not a one-shot onMessage/sendMessage: MV3
//     service workers can go idle between events, and a plain sendMessage
//     only guarantees the worker wakes to *receive* the message, not that
//     it stays alive long enough for forwardCnl's awaited fetch() to
//     finish and post a response back — observed live as "message port
//     closed before a response was received", with the page already
//     showing content-script.js's fake 200 while the link silently never
//     reached hopperd. A connect()ed port keeps the worker alive for the
//     duration of the connection, which a bare message does not.
//  2. Fallback CnL path: the original declarativeNetRequest redirect
//     rules, kept for browsers/versions without MAIN-world content
//     script support and as a second line of defense if the content
//     script fails to inject (e.g. a page loaded before install/update).
//     Redirects https://127.0.0.1:9666 probe/submit endpoints to the
//     real hopperd's CnL listener — no auth needed, CnL is gated by
//     hopperd's trusted_cidrs, not JWT.
//  3. Intercept .dlc/.txt downloads and import them into hopperd's
//     REST API instead of saving them to disk. Unlike CnL, every
//     /api/v1/* route requires a JWT (see docs/ARCHITECTURE.md) — there is no
//     browser-side login flow here, so this only works once the user
//     pastes a token obtained elsewhere (see options.html) into an
//     instance whose hopper.yaml token_ttl is long enough not to expire
//     on them. Missing/rejected token: log and let the download proceed
//     untouched, never block it.
//
// This file is loaded two different ways depending on the browser —
// manifest.json declares both background.service_worker and
// background.scripts pointing at this same file. Firefox has zero
// support for background.service_worker (confirmed against Mozilla's
// own compat data, not just undocumented — there's no version where it
// works), so it loads this as a plain background script (an "event
// page") instead; Chrome/Edge use the real service worker and ignore
// background.scripts. No code here is worker-only (no self./
// importScripts()/clients.*), so the exact same file behaves correctly
// either way with no branching needed — see MDN's background manifest
// key docs for this exact dual-declaration pattern.

const CNL_PORT = 9666;
const API_PORT = 9667;
const REDIRECT_RULE_IDS = [1, 2];

// One "Target URL" field drives both derived endpoints regardless of
// which port the user actually typed — see options.html's note. Only
// the host survives; the port is always overridden per purpose.
function deriveTarget(targetUrl, port) {
  try {
    const u = new URL(targetUrl);
    return `${u.protocol}//${u.hostname}:${port}`;
  } catch {
    return null;
  }
}

async function getSyncSettings() {
  const { targetUrl, dlcInterceptionEnabled, txtInterceptionEnabled } = await chrome.storage.sync.get({
    targetUrl: 'http://localhost:9667',
    dlcInterceptionEnabled: true,
    txtInterceptionEnabled: false,
  });
  return { targetUrl, dlcInterceptionEnabled, txtInterceptionEnabled };
}

async function getApiToken() {
  const { apiToken } = await chrome.storage.local.get({ apiToken: '' });
  return apiToken;
}

// updateRedirectRules rebuilds the CnL declarativeNetRequest rules from
// the current targetUrl. Called on install and whenever options are
// saved (see the storage.onChanged listener below).
async function updateRedirectRules() {
  const { targetUrl } = await getSyncSettings();
  const cnlTarget = deriveTarget(targetUrl, CNL_PORT);

  await chrome.declarativeNetRequest.updateDynamicRules({ removeRuleIds: REDIRECT_RULE_IDS, addRules: [] });
  if (!cnlTarget) {
    return;
  }

  // cnlTarget goes into regexSubstitution as-is, deliberately NOT run
  // through a regex-escaping helper: regexSubstitution is a replacement
  // template, not a regex pattern, and its only special syntax is a
  // capture-group reference (\1) or an escaped literal backslash (\\) —
  // there's no such thing as "an escaped dot" in that mini-language. A
  // previous version of this file escaped cnlTarget with the same
  // function used for regexFilter below, which turned every "." in the
  // host (e.g. "192.168.0.10") into "\.", and Chrome's own validator
  // rejects "\." as an invalid escape sequence in regexSubstitution —
  // "Rule with id 1 specifies an incorrect value for
  // action.redirect.regexSubstitution", on every single rule update.
  // \\1 in this template literal is the two characters \1 in the actual
  // string — the capture-group reference regexFilter's (.*) captures,
  // not a doubly-escaped backslash.
  await chrome.declarativeNetRequest.updateDynamicRules({
    removeRuleIds: [],
    addRules: [
      {
        id: 1,
        priority: 1,
        action: {
          type: 'redirect',
          redirect: { regexSubstitution: `${cnlTarget}/flash/\\1` },
        },
        condition: {
          regexFilter: `^https://127\\.0\\.0\\.1:${CNL_PORT}/flash/(.*)$`,
          resourceTypes: ['main_frame', 'sub_frame', 'xmlhttprequest', 'image', 'other'],
        },
      },
      {
        id: 2,
        priority: 1,
        action: {
          type: 'redirect',
          redirect: { regexSubstitution: `${cnlTarget}/jdcheck.js` },
        },
        condition: {
          regexFilter: `^https://127\\.0\\.0\\.1:${CNL_PORT}/jdcheck\\.js$`,
          resourceTypes: ['script', 'xmlhttprequest', 'other'],
        },
      },
    ],
  });
}

chrome.runtime.onInstalled.addListener(() => {
  updateRedirectRules();
});

chrome.storage.onChanged.addListener((changes, areaName) => {
  if (areaName === 'sync' && changes.targetUrl) {
    updateRedirectRules();
  }
});

// countLinksFromBody best-effort-counts links in a /flash/add submission
// (urls=<CRLF-separated list>, form-encoded — see docs/ARCHITECTURE.md's CnL protocol
// notes). /flash/addcrypted2 bodies are AES-encrypted and not decrypted
// here (hopperd does that), so their link count is unknowable client-side;
// null lets the popup fall back to a countless "CnL import" label instead
// of reporting a wrong number.
function countLinksFromBody(path, body) {
  if (!path.startsWith('/flash/add') || path.startsWith('/flash/addcrypted2')) {
    return null;
  }
  try {
    const params = new URLSearchParams(body);
    const urls = params.get('urls') || '';
    return urls
      .split(/\r?\n/)
      .map((s) => s.trim())
      .filter(Boolean).length;
  } catch {
    return null;
  }
}

// forwardCnl does the actual work: POST the relayed request to hopperd's
// real CnL listener and record it for the popup. Shared by the "cnl" port
// handler below — the sole path content-script.js uses (see this file's
// header comment, job 1, for why a port rather than onMessage).
async function forwardCnl(msg, sender) {
  try {
    const { targetUrl } = await getSyncSettings();
    const cnlTarget = deriveTarget(targetUrl, CNL_PORT);
    if (!cnlTarget) {
      return { ok: false, error: 'invalid target url' };
    }

    const resp = await fetch(`${cnlTarget}${msg.path}`, {
      method: 'POST',
      headers: { 'Content-Type': msg.contentType || 'application/x-www-form-urlencoded' },
      body: msg.body || '',
    });

    let sourceHost = '';
    try {
      sourceHost = new URL(sender?.tab?.url || '').hostname;
    } catch {
      // sender.tab.url can be missing (e.g. extension-initiated calls); leave blank
    }

    await chrome.storage.local.set({
      lastCnl: {
        timestamp: Date.now(),
        source: sourceHost,
        path: msg.path,
        linkCount: countLinksFromBody(msg.path, msg.body),
        ok: resp.ok,
      },
    });

    if (!resp.ok) {
      console.warn('Hopper: CnL forward returned non-OK status:', resp.status, msg.path);
    }
    return { ok: resp.ok, status: resp.status };
  } catch (err) {
    console.warn('Hopper: CnL forward failed:', err);
    return { ok: false, error: String(err) };
  }
}

// Primary CnL path — see this file's header comment, job 1. Connecting the
// port itself wakes a sleeping service worker (same as any runtime event)
// and keeps it alive for as long as the port stays open, so the connection
// is what keeps forwardCnl's awaited fetch() from being torn down mid-flight.
chrome.runtime.onConnect.addListener((port) => {
  if (port.name !== 'cnl') {
    return; // not ours; let any other listener handle it
  }
  port.onMessage.addListener(async (msg) => {
    if (!msg || msg.type !== 'cnl') {
      return;
    }
    const result = await forwardCnl(msg, port.sender);
    try {
      port.postMessage(result);
    } catch {
      // Tab/port may already be gone by the time the forward finishes.
    }
  });
});

function basename(path) {
  return path.split(/[\\/]/).pop();
}

// .dlc files are always link containers. A .txt file is usually just a
// text file — a README, a log, notes — that happens to contain a URL, so
// taking it away from the user is opt-in (txtInterceptionEnabled).
function isImportCandidate(filename, txtInterceptionEnabled) {
  const lower = filename.toLowerCase();
  return lower.endsWith('.dlc') || (txtInterceptionEnabled && lower.endsWith('.txt'));
}

chrome.downloads.onCreated.addListener(async (item) => {
  const filename = basename(item.filename || '');
  const { targetUrl, dlcInterceptionEnabled, txtInterceptionEnabled } = await getSyncSettings();
  if (!dlcInterceptionEnabled || !isImportCandidate(filename, txtInterceptionEnabled)) {
    return;
  }

  const apiToken = await getApiToken();
  if (!apiToken) {
    console.warn('Hopper: no API token configured, letting download proceed:', filename);
    return;
  }

  const apiTarget = deriveTarget(targetUrl, API_PORT);
  if (!apiTarget) {
    console.warn('Hopper: invalid target URL, letting download proceed:', filename);
    return;
  }

  try {
    const sourceUrl = item.finalUrl || item.url;
    const fileResp = await fetch(sourceUrl);
    if (!fileResp.ok) {
      throw new Error(`fetch source failed: HTTP ${fileResp.status}`);
    }
    const blob = await fileResp.blob();

    const form = new FormData();
    form.append('file', blob, filename);

    const importResp = await fetch(`${apiTarget}/api/v1/import`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${apiToken}` },
      body: form,
    });
    if (!importResp.ok) {
      throw new Error(`import failed: HTTP ${importResp.status}`);
    }

    await chrome.downloads.cancel(item.id);
    console.info('Hopper: imported and cancelled download:', filename);
  } catch (err) {
    console.warn('Hopper: import failed, letting download proceed:', filename, err);
  }
});
