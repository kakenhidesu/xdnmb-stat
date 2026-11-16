const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const page = require('../status-page.js');
test('unknown uptime is never displayed as 100%', () => {
  assert.equal(page.percentage(null), '—');
  assert.equal(page.percentage(undefined), '—');
  assert.equal(page.percentage(100), '100.00%');
});
test('daily details expose failed, unknown and missing samples', () => {
  const text = page.dayDescription({ date:'2026-11-16',status:'unknown',checks:2,ok:1,slow:0,failed:0,unknown:1,expected:12 });
  assert.match(text, /未知 1/); assert.match(text, /预计 12/); assert.match(text, /暂无有效数据/);
});
test('timestamps always use the displayed China time zone', () => {
  assert.match(page.time(Date.parse('2026-11-16T00:30:00Z')), /08:30:00/);
  assert.equal(page.time(0), '尚无记录');
});

test('slow success remains available while partial failures and unknown stay distinct', () => {
  assert.deepEqual(page.historyRates([]), { availability: null, coverage: null });
  assert.deepEqual(page.historyRates([{ok:1,slow:1,degraded:0,failed:0,unknown:1,checks:3,expected:6}]), { availability:100, coverage:50 });
  assert.deepEqual(page.historyRates([{ok:1,slow:1,degraded:1,failed:1,unknown:0,checks:4,expected:4}]), { availability:50, coverage:100 });
  assert.match(page.labels.slow, /可用/);
  assert.doesNotMatch(page.headlines.slow, /故障|异常/);
  assert.match(page.dayDescription({date:'2026-11-16',status:'slow',checks:1,ok:0,slow:1,degraded:0,failed:0,unknown:0,expected:1}), /较慢但可用 1/);
});
test('page has no visitor probe, remote image requests or script bridge', () => {
  const script = fs.readFileSync(require.resolve('../status-page.js'),'utf8');
  const html = fs.readFileSync(require.resolve('../index.html'),'utf8');
  assert.equal((script.match(/\bfetch\(/g)||[]).length, 1);
  assert.match(script, /fetch\('\/api\/status'/);
  assert.doesNotMatch(script, /new Image|postMessage|localStorage|runAll|probeOnce/);
  assert.doesNotMatch(html, /重新检测|Tampermonkey|local-probe/);
  assert.match(html, /历史事件/);
});
