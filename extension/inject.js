// Hopper browser extension — MAIN-world injected script.
//
// Runs inside the actual page's JS realm (manifest.json's "world": "MAIN"
// content script), not the extension's isolated world — necessary because
// a CnL button's own script calls the *page's* window.fetch/XMLHttpRequest
// directly, and an isolated-world override would never be seen by it.
//
// Why this exists at all: declarativeNetRequest (service-worker.js) rewrites
// a request at the network layer, but Brave (and hardened Firefox/Chrome
// configs) can still apply Mixed-Content/Shields blocking to the *original*
// https-page -> http-target request before a dNR redirect ever gets a
// chance to apply, breaking CnL entirely with no working fallback. Patching
// fetch/XMLHttpRequest here intercepts the call at the JavaScript API level,
// before the browser ever attempts to open a real network connection — so
// no mixed-content/network-layer policy is ever evaluated for it.
//
// MAIN-world scripts have zero chrome.* API access (same restriction as any
// ordinary page script), so this can't call chrome.runtime.sendMessage
// directly. Instead it dispatches a CustomEvent on window, JSON-stringified
// (cross-world object references aren't reliably readable, only structured-
// cloneable/primitive values are) and content-script.js — the isolated-world
// half of this pair, sharing the same DOM — listens for it and relays to the
// service worker.
//
// Firefox: declarative "world": "MAIN" content scripts need Firefox 128+
// (confirmed against Mozilla's own compat data — Chrome/Edge only needed
// 111), which is why manifest.json's browser_specific_settings.gecko.
// strict_min_version is set to "128.0", not just any MV3-capable Firefox
// version. Below 128, Firefox has no declarative way to run a script in
// the page's own world at all; a Firefox-specific wrappedJSObject-based
// fallback was considered and deliberately not built — see docs/ARCHITECTURE.md's
// Firefox-compatibility entry for why (Firefox 128 shipped mid-2024;
// essentially every actively-updating install is past it by now, and the
// fallback path couldn't be tested in this environment without a real
// Firefox to verify it against).
(function () {
  const CNL_HOST = '127.0.0.1';
  const CNL_PORT = 9666;
  const RELAY_EVENT = '__hopper_cnl_intercept__';

  function isHopperCnlUrl(rawUrl) {
    try {
      const u = new URL(rawUrl, location.href);
      return u.hostname === CNL_HOST && Number(u.port) === CNL_PORT;
    } catch {
      return false;
    }
  }

  function relayToContentScript(path, body, contentType) {
    window.dispatchEvent(
      new CustomEvent(RELAY_EVENT, {
        detail: JSON.stringify({ path, body: body || '', contentType: contentType || 'application/x-www-form-urlencoded' }),
      })
    );
  }

  function jdcheckBody() {
    return "jdownloader=true; var version='2.0';";
  }

  // ---- fetch() override ----
  const originalFetch = window.fetch;
  window.fetch = function (input, init) {
    const rawUrl = typeof input === 'string' ? input : input && input.url;
    if (rawUrl && isHopperCnlUrl(rawUrl)) {
      const u = new URL(rawUrl, location.href);

      if (u.pathname === '/jdcheck.js') {
        return Promise.resolve(
          new Response(jdcheckBody(), { status: 200, headers: { 'Content-Type': 'application/javascript' } })
        );
      }

      return (async () => {
        let bodyText = '';
        let contentType = 'application/x-www-form-urlencoded';
        if (init) {
          if (init.headers) {
            const h = new Headers(init.headers);
            contentType = h.get('Content-Type') || contentType;
          }
          if (init.body != null) {
            try {
              bodyText = typeof init.body === 'string' ? init.body : await new Response(init.body).text();
            } catch {
              bodyText = '';
            }
          }
        }
        relayToContentScript(u.pathname + u.search, bodyText, contentType);
        return new Response('success', { status: 200, headers: { 'Content-Type': 'text/plain' } });
      })();
    }
    return originalFetch.call(this, input, init);
  };

  // ---- XMLHttpRequest override ----
  // Rather than replacing the class outright (which breaks `instanceof
  // XMLHttpRequest` and every property a page might rely on), patch
  // open/setRequestHeader/send on the real prototype and, only for a
  // matched URL, skip the real send() and synthesize a response on the
  // instance instead.
  const proto = window.XMLHttpRequest.prototype;
  const originalOpen = proto.open;
  const originalSend = proto.send;
  const originalSetRequestHeader = proto.setRequestHeader;

  proto.open = function (method, url, ...rest) {
    this.__hopperIntercept = isHopperCnlUrl(url) ? { method, url: new URL(url, location.href).href } : null;
    return originalOpen.call(this, method, url, ...rest);
  };

  proto.setRequestHeader = function (name, value) {
    if (this.__hopperIntercept) {
      if (String(name).toLowerCase() === 'content-type') {
        this.__hopperIntercept.contentType = value;
      }
      return; // never actually sent — nothing to forward this header to
    }
    return originalSetRequestHeader.call(this, name, value);
  };

  proto.send = function (body) {
    const intercept = this.__hopperIntercept;
    if (!intercept) {
      return originalSend.call(this, body);
    }

    const u = new URL(intercept.url);
    const isJdcheck = u.pathname === '/jdcheck.js';
    const responseText = isJdcheck ? jdcheckBody() : 'success';
    const contentType = isJdcheck ? 'application/javascript' : 'text/plain';

    if (!isJdcheck) {
      let bodyText = '';
      if (typeof body === 'string') {
        bodyText = body;
      } else if (body != null) {
        try {
          bodyText = body.toString();
        } catch {
          bodyText = '';
        }
      }
      relayToContentScript(u.pathname + u.search, bodyText, intercept.contentType);
    }

    // Real XHRs resolve async; a same-tick response can surprise page code
    // that attaches onload/onreadystatechange only after calling send().
    setTimeout(() => {
      Object.defineProperty(this, 'readyState', { value: 4, configurable: true });
      Object.defineProperty(this, 'status', { value: 200, configurable: true });
      Object.defineProperty(this, 'statusText', { value: 'OK', configurable: true });
      Object.defineProperty(this, 'responseText', { value: responseText, configurable: true });
      Object.defineProperty(this, 'response', { value: responseText, configurable: true });
      Object.defineProperty(this, 'responseURL', { value: intercept.url, configurable: true });
      this.getResponseHeader = (name) => (String(name).toLowerCase() === 'content-type' ? contentType : null);
      this.getAllResponseHeaders = () => `content-type: ${contentType}\r\n`;

      this.dispatchEvent(new Event('readystatechange'));
      this.dispatchEvent(new Event('load'));
      this.dispatchEvent(new Event('loadend'));
      if (typeof this.onreadystatechange === 'function') this.onreadystatechange();
      if (typeof this.onload === 'function') this.onload(new Event('load'));
      if (typeof this.onloadend === 'function') this.onloadend(new Event('loadend'));
    }, 0);
  };

  // ---- <script src="http://127.0.0.1:9666/jdcheck.js"> interception ----
  // filecrypt.cc (confirmed live — see docs/ARCHITECTURE.md's jdcheck.js 400 entry)
  // probes for a running CnL client by loading jdcheck.js as a real
  // <script src="...">, not via fetch/XHR — a request the overrides above
  // never see, since a script tag's own resource fetch isn't a JS-callable
  // API the way fetch/XMLHttpRequest are. A MutationObserver is the only
  // way to catch it from page-injected JS: watch for a matching <script>
  // being added (or given a matching src after the fact — some sites
  // create the element first, then set .src, which is an attribute
  // mutation, not a childList one), strip its src, and substitute an
  // inline script with the exact same effect the real jdcheck.js has.
  //
  // This can't guarantee pre-empting an already-in-flight fetch for a
  // script element that had `src` set before insertion (the browser
  // commits to fetching the resource synchronously as part of insertion,
  // before any MutationObserver microtask runs) — but that's fine here:
  // if nothing is listening on 127.0.0.1:9666, that request just fails
  // harmlessly in the background; if hopperctl's own proxy IS listening
  // and answers first, its real jdcheck.js sets the exact same
  // jdownloader=true/version, so a redundant fake script is a no-op, not
  // a conflict.
  function isJdcheckScriptSrc(src) {
    if (!src) return false;
    try {
      const u = new URL(src, location.href);
      return u.hostname === CNL_HOST && Number(u.port) === CNL_PORT && u.pathname === '/jdcheck.js';
    } catch {
      return false;
    }
  }

  function neutralizeJdcheckScript(node) {
    if (node.__hopperNeutralized) return;
    node.__hopperNeutralized = true;
    node.removeAttribute('src');
    const fake = document.createElement('script');
    fake.textContent = jdcheckBody();
    (document.head || document.documentElement).appendChild(fake);
  }

  function scanForJdcheckScript(node) {
    if (!node || node.nodeType !== Node.ELEMENT_NODE) return;
    if (node.tagName === 'SCRIPT') {
      if (isJdcheckScriptSrc(node.getAttribute('src'))) {
        neutralizeJdcheckScript(node);
      }
      return;
    }
    if (typeof node.querySelectorAll === 'function') {
      node.querySelectorAll('script[src]').forEach((el) => {
        if (isJdcheckScriptSrc(el.getAttribute('src'))) {
          neutralizeJdcheckScript(el);
        }
      });
    }
  }

  const jdcheckObserver = new MutationObserver((mutations) => {
    for (const mutation of mutations) {
      if (mutation.type === 'childList') {
        mutation.addedNodes.forEach(scanForJdcheckScript);
      } else if (mutation.type === 'attributes' && mutation.target.tagName === 'SCRIPT') {
        if (isJdcheckScriptSrc(mutation.target.getAttribute('src'))) {
          neutralizeJdcheckScript(mutation.target);
        }
      }
    }
  });

  jdcheckObserver.observe(document, {
    childList: true,
    subtree: true,
    attributes: true,
    attributeFilter: ['src'],
  });
})();
