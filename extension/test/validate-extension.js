#!/usr/bin/env node
// Validates extension/inject.js's real, observable browser-facing
// behavior from plain Node — no browser, no new dependencies (only
// Node's own built-in `vm`/`fs`/`assert`/`path` modules, plus the
// fetch/Response/Headers/Event/CustomEvent/URL globals Node 18+ already
// provides). A minimal vm sandbox supplies just enough of window/
// document/location/MutationObserver/XMLHttpRequest for inject.js's
// actual IIFE to run *unmodified* and install its real overrides — this
// exercises the real shipped file byte-for-byte, not a reimplementation
// or a copy of its internal logic (which is private to the IIFE closure
// and has no exports to unit-test directly).
//
// Run: node extension/test/validate-extension.js
// Syntax-only check (no execution): node --check extension/test/validate-extension.js
//
// Requires Node 18+ (for global fetch/Response/Headers) — this repo's
// only other JS toolchain requirement (web/package.json) doesn't pin a
// Node version itself, so this is checked explicitly at startup instead
// of failing confusingly deep inside a sandboxed require.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

if (typeof fetch !== 'function' || typeof Response !== 'function' || typeof CustomEvent !== 'function') {
  console.error('This script requires Node 18+ (global fetch/Response) with CustomEvent support (Node 19+ for CustomEvent as a bare global).');
  process.exit(2);
}

const INJECT_JS_PATH = path.join(__dirname, '..', 'inject.js');
const source = fs.readFileSync(INJECT_JS_PATH, 'utf8');

// buildSandbox constructs a fresh, isolated vm context per check — state
// from one check (e.g. a MutationObserver instance, a relayed event)
// must never leak into another.
function buildSandbox() {
  const relayedEvents = [];
  let realFetchCalls = 0;
  let realSendCalls = 0;
  const observerInstances = [];

  // StubMutationObserver records every observe() call's (target,
  // options) instead of actually watching a DOM — there's no real DOM
  // here for it to watch, and the point of this check is purely "did
  // inject.js ask to observe the right thing with the right options".
  class StubMutationObserver {
    constructor(callback) {
      this.callback = callback;
      this.observeCalls = [];
      observerInstances.push(this);
    }
    observe(target, options) {
      this.observeCalls.push({ target, options });
    }
    disconnect() {}
  }

  // makeElement is a minimal stand-in for a DOM Element: just enough of
  // the attribute/child/textContent surface neutralizeJdcheckScript and
  // scanForJdcheckScript actually touch.
  function makeElement(tagName) {
    const attrs = {};
    let text = '';
    return {
      tagName: tagName.toUpperCase(),
      nodeType: 1, // Node.ELEMENT_NODE
      children: [],
      getAttribute(name) {
        return Object.prototype.hasOwnProperty.call(attrs, name) ? attrs[name] : null;
      },
      setAttribute(name, value) {
        attrs[name] = String(value);
      },
      removeAttribute(name) {
        delete attrs[name];
      },
      appendChild(child) {
        this.children.push(child);
        return child;
      },
      querySelectorAll() {
        return [];
      },
      set textContent(v) {
        text = v;
      },
      get textContent() {
        return text;
      },
    };
  }

  const documentStub = {
    head: makeElement('head'),
    documentElement: makeElement('html'),
    createElement: (tag) => makeElement(tag),
  };

  // StubXHR's open/send/setRequestHeader are what inject.js's real
  // fetch/XHR override patches (proto.open = function... etc., applied
  // to window.XMLHttpRequest.prototype) — send() here stands in for
  // "the request actually went to the network", so realSendCalls only
  // increments when inject.js's override decided NOT to intercept and
  // called the original through.
  class StubXHR {
    open(method, url) {
      this._method = method;
      this._url = url;
    }
    setRequestHeader() {}
    send() {
      realSendCalls++;
    }
    addEventListener() {}
    dispatchEvent() {
      return true;
    }
  }

  function realFetch(input, init) {
    realFetchCalls++;
    return Promise.resolve(new Response('real-network-response', { status: 200 }));
  }

  const sandbox = {
    console,
    setTimeout,
    URL,
    Headers,
    Response,
    Event,
    CustomEvent,
    Promise,
    Node: { ELEMENT_NODE: 1 },
    MutationObserver: StubMutationObserver,
    location: { href: 'https://example.com/some-cnl-button-page' },
    document: documentStub,
  };
  sandbox.window = sandbox;
  sandbox.window.fetch = realFetch;
  sandbox.window.XMLHttpRequest = StubXHR;
  sandbox.window.dispatchEvent = (event) => {
    relayedEvents.push(event);
    return true;
  };

  return {
    context: vm.createContext(sandbox),
    sandbox,
    relayedEvents,
    observerInstances,
    counts: () => ({ realFetchCalls, realSendCalls }),
  };
}

function loadInjectJS() {
  const env = buildSandbox();
  vm.runInContext(source, env.context, { filename: 'inject.js' });
  return env;
}

const results = [];

async function check(name, fn) {
  try {
    await fn();
    results.push({ name, ok: true });
    console.log(`ok - ${name}`);
  } catch (err) {
    results.push({ name, ok: false, err });
    console.error(`FAIL - ${name}`);
    console.error(`       ${err && err.message ? err.message : err}`);
  }
}

async function main() {
  // ---- jdcheckBody() exact content, via both interception paths ----
  await check('fetch(.../jdcheck.js) resolves to the exact jdownloader body', async () => {
    const env = loadInjectJS();
    const resp = await env.sandbox.window.fetch('http://127.0.0.1:9666/jdcheck.js');
    const text = await resp.text();
    assert.strictEqual(text, "jdownloader=true; var version='2.0';");
    assert.strictEqual(env.counts().realFetchCalls, 0, 'jdcheck.js must be answered without ever touching the real network');
  });

  await check('XHR to .../jdcheck.js resolves to the exact jdownloader body', async () => {
    const env = loadInjectJS();
    const xhr = new env.sandbox.window.XMLHttpRequest();
    xhr.open('GET', 'http://127.0.0.1:9666/jdcheck.js');
    xhr.send();
    await new Promise((r) => setTimeout(r, 20));
    assert.strictEqual(xhr.responseText, "jdownloader=true; var version='2.0';");
    assert.strictEqual(xhr.status, 200);
    assert.strictEqual(env.counts().realSendCalls, 0, 'jdcheck.js must be answered without ever touching the real network');
  });

  // ---- XHR override URL targeting: 127.0.0.1:9666/flash/* intercepted, everything else passed through ----
  await check('XHR to 127.0.0.1:9666/flash/add is intercepted (never reaches the real network)', async () => {
    const env = loadInjectJS();
    const xhr = new env.sandbox.window.XMLHttpRequest();
    xhr.open('POST', 'http://127.0.0.1:9666/flash/add');
    xhr.send('urls=https://example.com/file');
    await new Promise((r) => setTimeout(r, 20));
    assert.strictEqual(xhr.status, 200);
    assert.strictEqual(xhr.responseText, 'success');
    assert.strictEqual(env.counts().realSendCalls, 0, 'a matching flash/add request must not call through to the real XHR send()');
    assert.strictEqual(env.relayedEvents.length, 1, 'exactly one relay event should have been dispatched for content-script.js');
  });

  await check('XHR to an unrelated host is NOT intercepted (passes through to the real network)', async () => {
    const env = loadInjectJS();
    const xhr = new env.sandbox.window.XMLHttpRequest();
    xhr.open('POST', 'https://example.com/flash/add');
    xhr.send('urls=https://example.com/file');
    assert.strictEqual(env.counts().realSendCalls, 1, 'a non-127.0.0.1:9666 URL must fall through to the real XHR send()');
    assert.strictEqual(env.relayedEvents.length, 0);
  });

  await check('XHR to 127.0.0.1 on a different port is NOT intercepted', async () => {
    const env = loadInjectJS();
    const xhr = new env.sandbox.window.XMLHttpRequest();
    xhr.open('POST', 'http://127.0.0.1:9667/flash/add');
    xhr.send('urls=https://example.com/file');
    assert.strictEqual(env.counts().realSendCalls, 1, 'only port 9666 is a CnL target — 9667 (the API port) must pass through untouched');
  });

  await check('fetch() to 127.0.0.1:9666/flash/addcrypted2 is intercepted', async () => {
    const env = loadInjectJS();
    const resp = await env.sandbox.window.fetch('http://127.0.0.1:9666/flash/addcrypted2', {
      method: 'POST',
      body: 'crypted=abc123',
    });
    const text = await resp.text();
    assert.strictEqual(text, 'success');
    assert.strictEqual(resp.status, 200);
    assert.strictEqual(env.counts().realFetchCalls, 0);
    assert.strictEqual(env.relayedEvents.length, 1);
    const relayed = JSON.parse(env.relayedEvents[0].detail);
    assert.strictEqual(relayed.path, '/flash/addcrypted2');
    assert.strictEqual(relayed.body, 'crypted=abc123');
  });

  await check('fetch() to an unrelated URL is NOT intercepted (passes through to the real network)', async () => {
    const env = loadInjectJS();
    const resp = await env.sandbox.window.fetch('https://example.com/api/whatever');
    assert.strictEqual(env.counts().realFetchCalls, 1);
    const text = await resp.text();
    assert.strictEqual(text, 'real-network-response');
  });

  // ---- MutationObserver: watches the right target with the right options ----
  await check('MutationObserver watches document for childList+subtree+attributes(src)', async () => {
    const env = loadInjectJS();
    assert.strictEqual(env.observerInstances.length, 1, 'exactly one MutationObserver should be created');
    const observer = env.observerInstances[0];
    assert.strictEqual(observer.observeCalls.length, 1, 'observe() should be called exactly once');
    const { target, options } = observer.observeCalls[0];
    assert.strictEqual(target, env.sandbox.document, 'must observe the whole document, not just <head> or <body>');
    assert.strictEqual(options.childList, true);
    assert.strictEqual(options.subtree, true);
    assert.strictEqual(options.attributes, true);
    // Array.from(...), not a direct deepStrictEqual against the sandbox's
    // own array: options.attributeFilter was created inside the vm
    // context, so it's an instance of *that* realm's Array — a different
    // constructor from this file's own Array even when the contents are
    // identical, which deepStrictEqual (correctly) treats as unequal.
    assert.deepStrictEqual(Array.from(options.attributeFilter), ['src'], 'must be scoped to the src attribute only, not every attribute mutation');
  });

  const failed = results.filter((r) => !r.ok);
  console.log(`\n${results.length - failed.length}/${results.length} checks passed.`);
  if (failed.length > 0) {
    process.exit(1);
  }
}

main().catch((err) => {
  console.error('validate-extension.js crashed:', err);
  process.exit(1);
});
