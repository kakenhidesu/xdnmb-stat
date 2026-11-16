const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const C = require('../status-core.js');
const pageSource = fs.readFileSync(require.resolve('../status-page.js'), 'utf8');
const scriptSource = fs.readFileSync(require.resolve('../local-probe.user.js'), 'utf8');
const today = new Date().toISOString().slice(0, 10);
const posts = [1, 2, 3].map(id => ({ id, content: 'sample', img: `${today}/sample${id}`, ext: '.jpg', Replies: [] }));
const cdns = [{ url: 'https://image.nmb.best/' }];
const catalog = { generatedAt: Date.now(), cdns: cdns.map(x => x.url), paths: posts.map(x => x.img + x.ext) };

async function pageRun(t, options = {}) {
  const elements = new Map(), listeners = new Set(), timers = new Set(), requests = [], errors = [];
  const element = () => ({
    textContent: '', className: '', style: {}, children: [], disabled: false,
    append(...items) { this.children.push(...items); },
    appendChild(item) { this.children.push(item); },
    replaceChildren(...items) { this.children = items; },
    addEventListener() {},
    get classList() { return { contains: value => this.className.split(' ').includes(value) }; },
  });
  const get = id => { if (!elements.has(id)) elements.set(id, element()); return elements.get(id); };
  const win = {
    addEventListener(type, fn) { if (type === 'message') listeners.add(fn); },
    removeEventListener(type, fn) { listeners.delete(fn); },
    postMessage(data) { queueMicrotask(() => { for (const fn of [...listeners]) fn({ source: win, origin: 'http://localhost:8787', data }); }); },
  };
  const fetcher = async url => {
    if (url.includes('image-samples.json')) return new Response(JSON.stringify(catalog), { headers: { 'Content-Type': 'application/json' } });
    throw new TypeError('CORS blocked');
  };
  class FakeImage {
    naturalWidth = 20; naturalHeight = 20;
    set src(url) { queueMicrotask(() => options.imageFailure && url.includes(`/image/${today}/sample1`) ? this.onerror() : this.onload()); }
    removeAttribute() {}
  }
  const context = vm.createContext({
    XdaoStatus: { ...C,
      checkJson: (url, validate, opts = {}) => C.checkJson(url, validate, { fetcher, ...opts }),
      checkApi: (api, opts = {}) => C.checkApi(api, { fetcher, ...opts }),
      checkImage: url => C.checkImage(url, { ImageClass: FakeImage }),
    },
    document: { getElementById: get, createElement: element, addEventListener() {} }, window: win,
    location: { origin: 'http://localhost:8787', href: 'http://localhost:8787/' }, URL, Headers, Date,
    localStorage: { getItem: () => options.corruptHistory || null, setItem() {} },
    console: { error: (...args) => errors.push(args) },
    setTimeout: (fn, delay) => { const id = setTimeout(fn, delay); timers.add(id); return id; },
    clearTimeout, setInterval() {},
    GM_xmlhttpRequest: request => {
      requests.push(request);
      const url = request.url;
      const data = url.includes('getCdnPath') ? cdns : url.includes('showf') ? posts : { content: 'notice', enable: true, date: 20261116 };
      queueMicrotask(() => request.onload({ status: options.apiFailure && url.includes('showf') ? 503 : 200,
        responseText: JSON.stringify(data), responseHeaders: 'Content-Type: application/json\r\n', finalUrl: url }));
      return { abort() { request.onabort(); } };
    },
  });
  t.after(() => { for (const id of timers) clearTimeout(id); });
  if (options.bridge) vm.runInContext(scriptSource, context);
  vm.runInContext(pageSource, context);
  const deadline = Date.now() + 2500;
  while (!get('time').textContent && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 5));
  assert.ok(get('time').textContent, 'page completed its run');
  assert.equal(errors.length, 0);
  return { get, requests, win };
}
test('ordinary browser: CORS restrictions remain visible despite successful image catalogue and images', async t => {
  const { get, requests } = await pageRun(t, { corruptHistory: '"not an object"' });
  assert.match(get('banner-text').textContent, /检测不完整/);
  assert.match(get('state-main').textContent, /无法验证/);
  assert.equal(get('state-cdn').textContent, '通过');
  assert.match(get('detail-cdn').textContent, /10\/10/);
  assert.equal(requests.length, 0);
});
test('browser script: verified local API responses do not conceal failed original images', async t => {
  const { get, requests } = await pageRun(t, { bridge: true, imageFailure: true });
  assert.equal(get('state-main').textContent, '通过');
  assert.equal(get('state-cdn').textContent, '部分失败');
  assert.match(get('banner-text').textContent, /部分检测失败/);
  assert.equal(requests.length, 7);
  assert.ok(requests.every(x => x.anonymous === true));
});
test('browser script: 503 returned through the bridge is an API failure', async t => {
  const { get } = await pageRun(t, { bridge: true, apiFailure: true });
  assert.equal(get('state-main').textContent, '部分失败');
  assert.match(get('banner-text').textContent, /部分检测失败/);
});
test('browser script: full success is explicitly a sample result, and arbitrary destinations are rejected', async t => {
  const { get, requests, win } = await pageRun(t, { bridge: true });
  assert.equal(get('banner-text').textContent, '本轮抽样检测通过');
  win.postMessage({ channel: 'xdnmb-status-v1', type: 'request', id: 'untrusted', url: 'https://example.com/private' });
  await new Promise(resolve => setTimeout(resolve, 5));
  assert.equal(requests.length, 7);
});
