const $ = id => document.getElementById(id);
const C = XdaoStatus;
const AUTO_INTERVAL = 300000, COOLDOWN = 30000;
const HISTORY_MAX = 40, HISTORY_KEY = 'xdao-status-history-v2';
const ROWS = [...C.APIS, { id: 'cdn', name: '图片 CDN' }, { id: 'notice', name: '公告接口' }];
let running = false, lastRun = 0, cooldownTimer;
let history = {};
try {
  const saved = JSON.parse(localStorage.getItem(HISTORY_KEY));
  if (saved && typeof saved === 'object' && !Array.isArray(saved)) history = saved;
} catch {}
for (const row of ROWS) {
  history[row.id] = Array.isArray(history[row.id])
    ? history[row.id].filter(s => Object.hasOwn(C.STATE_TEXT, s)).slice(-HISTORY_MAX) : [];
  const el = document.createElement('div');
  el.className = 'row';
  el.innerHTML = `<div class="row-top"><span class="dot wait" id="dot-${row.id}"></span>
    <span class="name">${row.name}</span><span class="host">${row.origin ? new URL(row.origin).host : ''}</span>
    <div class="result"><div class="state wait" id="state-${row.id}">等待检测</div></div></div>
    <p class="detail" id="detail-${row.id}"></p><ul class="checks" id="checks-${row.id}"></ul>
    <div class="ticks" id="ticks-${row.id}"></div>
    <div class="tick-label"><span>较早</span><span>最近 ${HISTORY_MAX} 次检测</span></div>`;
  $('list').appendChild(el);
  renderTicks(row.id);
}
function renderTicks(id) {
  const values = history[id];
  $('ticks-' + id).replaceChildren(...Array.from({ length: HISTORY_MAX }, (_, k) => {
    const tick = document.createElement('span');
    const status = values[values.length - HISTORY_MAX + k];
    tick.className = 'tick ' + (status || '');
    tick.title = C.STATE_TEXT[status] || '尚无记录';
    return tick;
  }));
}
function paint(id, status, detail) {
  $('dot-' + id).className = 'dot ' + status;
  $('state-' + id).className = 'state ' + status;
  $('state-' + id).textContent = C.STATE_TEXT[status] || '检测中';
  $('detail-' + id).textContent = detail;
}
function record(id, status) {
  history[id].push(status);
  history[id] = history[id].slice(-HISTORY_MAX);
  renderTicks(id);
}
function checkLine(id, label, result, url) {
  const line = document.createElement('li');
  line.className = 'check ' + result.status;
  const name = document.createElement(url ? 'a' : 'span');
  name.textContent = label;
  if (url) { name.href = url; name.target = '_blank'; name.rel = 'noopener noreferrer'; }
  const text = document.createElement('span');
  text.textContent = `${C.STATE_TEXT[result.status]} · ${result.detail}${Number.isFinite(result.elapsed) ? ` · ${result.elapsed} ms` : ''}`;
  line.append(name, text);
  $('checks-' + id).appendChild(line);
}
function setBanner(cls, text) {
  $('banner').className = cls;
  $('banner-text').textContent = text;
}

// This bridge uses requests made by the tester's own browser, never a server proxy.
async function localBrowserFetcher() {
  const channel = 'xdnmb-status-v1';
  const pending = new Map();
  const newId = () => globalThis.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  const readyId = newId();
  let ready;
  const readyPromise = new Promise(resolve => { ready = resolve; });
  const listener = event => {
    if (event.source !== window || event.origin !== location.origin || event.data?.channel !== channel) return;
    const message = event.data;
    if (message.type === 'ready' && message.id === readyId) ready(true);
    if (message.type !== 'response' || !pending.has(message.id)) return;
    const request = pending.get(message.id);
    if (message.error) request.reject(new Error(message.error));
    else if (message.status >= 200 && message.status <= 599 && typeof message.body === 'string') {
      request.resolve({ status: message.status, ok: message.status < 300, type: 'basic',
        headers: new Headers({ 'Content-Type': message.contentType || '' }),
        json: async () => JSON.parse(message.body) });
    } else request.reject(new Error('Invalid browser script response'));
  };
  window.addEventListener('message', listener);
  window.postMessage({ channel, type: 'hello', id: readyId }, location.origin);
  const connected = await Promise.race([readyPromise, new Promise(resolve => setTimeout(() => resolve(false), 400))]);
  if (!connected) { window.removeEventListener('message', listener); return null; }
  return {
    dispose: () => window.removeEventListener('message', listener),
    fetcher: (url, options) => {
      const id = newId();
      let abort;
      const promise = new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject });
        abort = () => {
          window.postMessage({ channel, type: 'abort', id }, location.origin);
          reject(new Error('Request aborted'));
        };
        if (options.signal.aborted) { abort(); return; }
        options.signal.addEventListener('abort', abort, { once: true });
        window.postMessage({ channel, type: 'request', id, url }, location.origin);
      });
      return promise.finally(() => { pending.delete(id); options.signal.removeEventListener('abort', abort); });
    },
  };
}
async function loadCatalog() {
  // Optional metadata only. Its server-side success/failure is never a health result.
  const url = new URL(window.XDAO_SAMPLE_CATALOG || './image-samples.json', location.href).href;
  return C.checkJson(url, data => C.validateCatalog(data), { timeout: 22000 });
}
async function runAll() {
  if (running || (lastRun && Date.now() - lastRun < COOLDOWN)) return;
  running = true;
  lastRun = Date.now();
  clearTimeout(cooldownTimer);
  $('btn').disabled = true;
  $('hint').style.display = 'none';
  setBanner('checking', '正在从你的浏览器检测…');
  for (const row of ROWS) {
    paint(row.id, 'wait', row.id === 'cdn' ? '正在获取当前图床地址和近期图片样本' : '');
    $('checks-' + row.id).replaceChildren();
  }
  let bridge;
  try {
    bridge = await localBrowserFetcher();
    const options = bridge ? { fetcher: bridge.fetcher } : {};
    $('method').textContent = bridge
      ? '接口：通过浏览器脚本从你的网络验证；图片：由你的浏览器直接加载。'
      : '接口：浏览器直接请求；受跨域限制时显示“无法验证”。图片：由你的浏览器直接加载。';
    const catalogPromise = loadCatalog();
    const noticePromise = C.checkJson(C.NOTICE_URL, C.validateNotice, options).then(result => {
      paint('notice', result.status, result.detail);
      return result;
    });
    const apis = await Promise.all(C.APIS.map(async api => {
      const result = await C.checkApi(api, options);
      paint(api.id, result.status, '分别验证 CDN 配置与帖子列表的 HTTP 状态、JSON 和内容格式');
      checkLine(api.id, 'CDN 配置', result.config);
      checkLine(api.id, '帖子列表', result.posts);
      return result;
    }));
    const catalog = await catalogPromise;
    const plan = C.imagePlan(apis, Date.now(), catalog.data);
    let completed = 0;
    const images = await C.mapLimit(plan.checks, 4, async item => {
      const result = await C.checkImage(item.url);
      paint('cdn', 'wait', `已检测 ${++completed}/${plan.checks.length} 项；近期图片 ${plan.recentCount}/${C.RECENT_COUNT} 张`);
      checkLine('cdn', `${item.label} · ${new URL(item.url).host} · ${item.path}`, result, item.url);
      return result;
    });
    const cdn = C.imageSummary(plan, images);
    const source = plan.catalogUsed ? `；样本目录更新于 ${new Date(plan.catalogTime).toLocaleTimeString()}，加载结果来自你的浏览器` : '';
    paint('cdn', cdn.status, `${cdn.passed}/${cdn.total} 项加载成功；近期图片 ${plan.recentCount}/${C.RECENT_COUNT} 张${source}`);
    for (const issue of plan.coverage) checkLine('cdn', '覆盖范围', { status: 'unknown', detail: issue });
    const notice = await noticePromise;
    const results = [...apis, { ...cdn, id: 'cdn' }, { ...notice, id: 'notice' }];
    const summary = C.banner(results);
    setBanner(summary.cls, summary.text);
    for (const result of results) record(result.id, result.status);
    try { localStorage.setItem(HISTORY_KEY, JSON.stringify(history)); } catch {}
    const hints = ['结果仅代表你当前网络下的本轮抽样，不能保证所有图片、上传功能或所有地区均正常。'];
    if (results.some(x => x.status === 'bad' || x.status === 'partial')) {
      hints.push('加载失败可能与服务故障、文件失效、限流、网络或浏览器拦截有关；本页面不能据此确定具体原因。');
    }
    if (apis.some(x => x.status === 'unknown') || notice.status === 'unknown') {
      hints.push('接口无法读取不等于接口正常或宕机。可安装下方可选浏览器脚本，在你自己的网络上验证真实响应。');
    }
    if (plan.coverage.length) hints.push('图片覆盖不足，固定旧图通过也不会得到完整通过的结论。');
    $('hint').textContent = hints.join('\n');
    $('hint').style.display = 'block';
  } catch (error) {
    setBanner('partial', '检测未完成，请重试');
    $('hint').textContent = '本轮检测发生异常，不能据此判断服务正常。';
    $('hint').style.display = 'block';
    for (const row of ROWS) {
      if ($('state-' + row.id).classList.contains('wait')) paint(row.id, 'unknown', '本轮检测未完成');
    }
    console.error('Status check failed', error);
  } finally {
    bridge?.dispose();
    running = false;
    $('time').textContent = new Date().toLocaleTimeString();
    const remaining = Math.max(0, COOLDOWN - (Date.now() - lastRun));
    $('btn').textContent = remaining ? '请稍候再检测' : '重新检测';
    cooldownTimer = setTimeout(() => { $('btn').disabled = false; $('btn').textContent = '重新检测'; }, remaining);
  }
}
$('btn').addEventListener('click', runAll);
runAll();
setInterval(() => { if (!document.hidden) runAll(); }, AUTO_INTERVAL);
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && Date.now() - lastRun >= AUTO_INTERVAL) runAll();
});
