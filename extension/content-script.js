// Hopper browser extension — isolated-world content script.
//
// Runs alongside inject.js (the MAIN-world half of this pair — see its own
// header comment for why the interception itself has to live there). A
// MAIN-world script has no chrome.* API access at all, so it can't call
// chrome.runtime.connect/sendMessage directly; instead it dispatches a
// CustomEvent on window (same DOM, shared between both worlds) with a
// JSON-stringified detail — cross-world object references aren't reliably
// readable, only structured-cloneable/primitive values are, so passing a
// plain string sidesteps that entirely. This script just relays it to the
// service worker.
const RELAY_EVENT = '__hopper_cnl_intercept__';

window.addEventListener(RELAY_EVENT, (event) => {
  let payload;
  try {
    payload = JSON.parse(event.detail);
  } catch {
    return;
  }
  if (!payload || !payload.path) {
    return;
  }
  sendCnlOverPort(payload);
});

// sendCnlOverPort forwards a relayed CnL request to the service worker over
// a short-lived chrome.runtime.connect port, not a one-shot sendMessage.
//
// A plain sendMessage can race a sleeping MV3 service worker: the runtime
// wakes it to deliver the message, but there's no guarantee its async
// response (an awaited fetch() to hopperd) finishes before Chrome decides
// the worker is idle again and tears the message channel down under it —
// observed live as "message port closed before a response was received",
// with the page already showing its fake 200 (from inject.js) while the
// real link never reached hopperd. Connecting a port both wakes the worker
// the same way sendMessage does *and* keeps it alive for as long as the
// port stays open (see service-worker.js's onConnect handler), so the
// connection itself is the keep-alive for the forward — no retry loop
// needed on this side.
function sendCnlOverPort(payload) {
  try {
    const port = chrome.runtime.connect({ name: 'cnl' });
    port.onMessage.addListener(() => {
      port.disconnect();
    });
    port.onDisconnect.addListener(() => {
      // chrome.runtime.lastError is expected here if the worker never got
      // to respond (e.g. it was reloaded mid-forward). The page already
      // has its fake 200 from inject.js either way — nothing to retry.
      void chrome.runtime.lastError;
    });
    port.postMessage({ type: 'cnl', ...payload });
  } catch {
    // "Extension context invalidated" (e.g. the extension was just
    // reloaded/updated while this tab was open) throws synchronously
    // rather than rejecting — same "nothing left to retry" reasoning.
  }
}
