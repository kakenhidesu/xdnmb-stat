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
  const text = page.dayDescription({ date:'2026-11-16',status:'operational',checks:2,ok:1,slow:0,failed:0,unknown:1,expected:12 });
  assert.match(text, /未知 1/); assert.match(text, /预计 12/); assert.match(text, /含未知或缺测/); assert.match(text, /缺测 10 轮/);
  assert.doesNotMatch(text, /暂无有效数据/);
});
test('mixed daily evidence keeps its color and marks uncertainty separately', () => {
  const day = { date:'2026-11-17',status:'slow',checks:288,ok:274,slow:1,degraded:0,failed:0,unknown:13,expected:288 };
  assert.equal(page.dayClassName(day), 'day slow incomplete');
  assert.match(page.dayDescription(day), /可用.*含未知或缺测/);
  assert.match(page.dayDescription(day), /正常 274.*较慢但可用 1.*未知 13/);
  assert.doesNotMatch(page.dayDescription(day), /暂无有效数据|缺测 \d+ 轮/);
  assert.equal(page.dayClassName({...day,status:'operational',unknown:0,expected:289}), 'day operational incomplete');
  assert.equal(page.dayClassName({...day,status:'operational',unknown:0}), 'day operational');
  assert.equal(page.dayClassName({...day,status:'degraded'}), 'day degraded incomplete');
  assert.equal(page.dayClassName({...day,status:'unknown',ok:0,slow:0,unknown:288}), 'day unknown');
  assert.match(page.dayDescription({...day,status:'unknown',ok:0,slow:0,unknown:288}), /暂无有效数据/);
  assert.match(page.dayDescription({...day,status:'unknown',checks:0,ok:0,slow:0,unknown:0}), /无记录/);
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
