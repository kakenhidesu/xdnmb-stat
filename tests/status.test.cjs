const { test } = require('node:test');
const assert = require('node:assert/strict');
const C = require('../status-core.js');
const NOW = Date.parse('2026-11-16T08:00:00Z');
const cdns = [{ url: 'https://image.nmb.best/', rate: 1 }];
const post = (day, id = '1') => ({ id, content: 'sample', img: `${day}/sample${id}`, ext: '.jpg', Replies: [] });
const posts = [post('2026-11-16'), post('2026-11-15', '2'), post('2026-11-14', '3')];
const api = () => ({ config: { status: 'ok', data: cdns }, posts: { status: 'ok', data: posts } });
const json = (data, status = 200) => new Response(JSON.stringify(data), { status, headers: { 'Content-Type': 'application/json' } });
const probe = (response, validate = C.validateCdns) => C.checkJson('https://example.com/api?id=4', validate, { fetcher: async () => response });

test('HTTP 429 and 503 fail even when the response body has valid JSON', async () => {
  for (const code of [429, 503]) {
    const result = await probe(json(cdns, code));
    assert.equal(result.status, 'bad');
    assert.match(result.detail, new RegExp(String(code)));
  }
});
test('HTML challenge, malformed JSON and business error responses fail validation', async () => {
  for (const response of [
    new Response('<h1>Challenge</h1>', { headers: { 'Content-Type': 'text/html' } }),
    new Response('not json', { headers: { 'Content-Type': 'application/json' } }),
    json({ success: false, error: 'failure' }), json([]),
  ]) assert.equal((await probe(response)).status, 'bad');
});
test('opaque responses and browser network/CORS failures remain unknown', async () => {
  assert.equal((await probe({ type: 'opaque', status: 0 })).status, 'unknown');
  const result = await C.checkJson('https://example.com', C.validateCdns, { fetcher: async () => { throw new TypeError('Failed to fetch'); } });
  assert.equal(result.status, 'unknown');
  assert.notEqual(C.banner([result]).cls, 'ok');
});
test('JSON body consumption is inside the timeout and the request is aborted', async () => {
  let signal;
  const result = await C.checkJson('https://example.com', C.validateCdns, { timeout: 15, fetcher: async (_, options) => {
    signal = options.signal;
    return { ok: true, status: 200, headers: new Headers({ 'Content-Type': 'application/json' }), json: () => new Promise(() => {}) };
  } });
  assert.equal(result.status, 'bad');
  assert.match(result.detail, /超时/);
  assert.equal(signal.aborted, true);
});
test('readable JSON is validated and no-cors is never used', async () => {
  let seen;
  const result = await C.checkJson('https://example.com/api?id=4&page=1', C.validateCdns, { fetcher: async (url, options) => {
    seen = { url: new URL(url), options }; return json(cdns);
  } });
  assert.equal(result.status, 'ok');
  assert.equal(seen.options.mode, 'cors');
  assert.equal(seen.options.credentials, 'omit');
  assert.equal(seen.url.searchParams.get('id'), '4');
  assert.equal(seen.url.searchParams.get('page'), '1');
  assert.ok(seen.url.searchParams.has('_'));
});
test('API configuration alone does not hide a failed posts endpoint', async () => {
  const result = await C.checkApi(C.APIS[0], { fetcher: async url => url.includes('getCdnPath') ? json(cdns) : json({ error: 'failed' }, 503) });
  assert.equal(result.status, 'partial');
  assert.equal(result.config.status, 'ok');
  assert.equal(result.posts.status, 'bad');
});
test('post and notice schemas reject error objects', () => {
  assert.equal(C.validatePosts(posts), true);
  assert.equal(C.validatePosts([{ ...posts[0], Replies: [{ error: 'failure' }] }]), false);
  assert.equal(C.validateNotice({ content: 'notice', enable: true, date: 20261116 }), true);
  assert.ok(!C.validateNotice({ content: 'error', enable: 'true' }));
});
test('recent samples ignore pinned old images, deduplicate, inspect replies and reject unsafe paths', () => {
  const thread = post('2022-12-04');
  thread.Replies = [...posts, posts[0], { ...posts[0], img: 'https://evil.invalid/path' }, post('2027-01-01')];
  assert.deepEqual(C.recentPaths([[thread]], NOW), ['2026-11-16/sample1.jpg', '2026-11-15/sample2.jpg', '2026-11-14/sample3.jpg']);
});
test('plan tests thumbnail and original for two baseline and three recent files', () => {
  const plan = C.imagePlan([api(), api()], NOW);
  assert.equal(plan.checks.length, 10);
  assert.equal(plan.coverage.length, 0);
  assert.equal(plan.recentCount, 3);
  assert.equal(plan.checks.filter(x => x.size === 'image').length, 5);
});
test('fixed image success without current configuration/recent images cannot turn green', () => {
  const plan = C.imagePlan([], NOW);
  const result = C.imageSummary(plan, plan.checks.map(() => ({ status: 'ok' })));
  assert.equal(result.status, 'unknown');
  assert.notEqual(C.banner([result]).cls, 'ok');
});
test('one failed recent original is visible even when every other image loads', () => {
  const plan = C.imagePlan([api()], NOW);
  const results = plan.checks.map(x => ({ status: x.kind === '近期图片' && x.size === 'image' ? 'bad' : 'ok' }));
  const summary = C.imageSummary(plan, results);
  assert.equal(summary.status, 'partial');
  assert.equal(summary.passed, 7);
  assert.notEqual(C.banner([summary]).cls, 'ok');
});
test('excess CDN addresses leave coverage incomplete', () => {
  const data = api();
  data.config.data = Array.from({ length: 4 }, (_, i) => ({ url: `https://cdn${i}.example.com/` }));
  const plan = C.imagePlan([data], NOW);
  assert.equal(plan.checks.length, 30);
  assert.ok(plan.coverage.length);
});
test('server catalogue contributes only addresses, not health; stale data is rejected', () => {
  const catalog = { generatedAt: NOW, cdns: ['https://image.nmb.best/'], paths: C.recentPaths([posts], NOW), status: 'ok' };
  const plan = C.imagePlan([], NOW, catalog);
  assert.equal(plan.recentCount, 3);
  assert.equal(plan.coverage.length, 0);
  assert.equal(C.imageSummary(plan, plan.checks.map(() => ({ status: 'bad' }))).status, 'bad');
  assert.equal(C.validateCatalog({ ...catalog, generatedAt: NOW - 16 * 60000 }, NOW), false);
  assert.equal(C.imagePlan([], NOW, { ...catalog, generatedAt: NOW - 16 * 60000 }).recentCount, 0);
  assert.equal(C.validateCatalog({ ...catalog, paths: ['../../x'] }, NOW), false);
});
test('green summary is explicitly limited to this sample, unknown cannot disappear', () => {
  assert.match(C.banner([{ status: 'ok' }]).text, /抽样/);
  assert.equal(C.banner([{ status: 'ok' }, { status: 'unknown' }]).cls, 'partial');
});
test('image load requires a decoded image; errors and timeout settle and clean up', async () => {
  for (const scenario of ['load', 'error', 'empty', 'timeout']) {
    let img;
    class FakeImage {
      constructor() { img = this; this.naturalWidth = scenario === 'empty' ? 0 : 20; this.naturalHeight = 20; }
      set src(value) { this.url = value; if (scenario !== 'timeout') queueMicrotask(() => scenario === 'error' ? this.onerror() : this.onload()); }
      removeAttribute(name) { this.removed = name; }
    }
    const result = await C.checkImage('https://image.nmb.best/thumb/a.jpg', { ImageClass: FakeImage, timeout: 10 });
    assert.equal(result.status, scenario === 'load' ? 'ok' : 'bad');
    assert.equal(img.onload, null);
    assert.equal(img.onerror, null);
    assert.equal(img.removed, 'src');
    assert.equal(img.referrerPolicy, 'no-referrer');
  }
});
test('image concurrency is bounded and output stays in input order', async () => {
  let active = 0, peak = 0;
  const result = await C.mapLimit([1, 2, 3, 4, 5], 2, async item => {
    peak = Math.max(peak, ++active);
    await new Promise(resolve => setTimeout(resolve, 5));
    active--; return item * 2;
  });
  assert.equal(peak, 2);
  assert.deepEqual(result, [2, 4, 6, 8, 10]);
});
