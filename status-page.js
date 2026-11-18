(() => {
  const labels = { operational: '运行正常', slow: '可用 · 响应较慢', degraded: '部分失败', outage: '不可用', unknown: '暂无有效数据' };
  const headlines = { operational: '所有监测项目运行正常', slow: '服务可用，部分响应较慢', degraded: '部分监测项目出现故障', outage: '监测节点无法访问服务', unknown: '监测数据暂不完整' };
  const time = (value, dateOnly = false) => value ? new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
    ...(dateOnly ? {} : { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }),
  }).format(new Date(value)) : '尚无记录';
  const percentage = value => typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(2)}%` : '—';
  const dayIncomplete = day => day.status !== 'unknown' && (day.unknown > 0 || day.expected > day.checks);
  const dayClassName = day => `day ${day.status}${dayIncomplete(day) ? ' incomplete' : ''}`;
  const dayDescription = day => `${day.date} · ${day.checks ? labels[day.status] || labels.unknown : '无记录'}${dayIncomplete(day) ? '（含未知或缺测）' : ''}；${day.checks} 轮记录：正常 ${day.ok}，较慢但可用 ${day.slow}，部分失败 ${day.degraded || 0}，失败 ${day.failed}，未知 ${day.unknown}；预计 ${day.expected} 轮${day.expected > day.checks ? `，缺测 ${day.expected - day.checks} 轮` : ''}`;
  const historyRates = days => {
    const sums = days.reduce((sum, day) => ({ available: sum.available + day.ok + day.slow, known: sum.known + day.ok + day.slow + (day.degraded || 0) + day.failed, checks: sum.checks + day.checks, expected: sum.expected + day.expected }), { available: 0, known: 0, checks: 0, expected: 0 });
    return { availability: sums.known ? sums.available * 100 / sums.known : null, coverage: sums.expected ? Math.min(100, sums.checks * 100 / sums.expected) : null };
  };
  if (typeof module !== 'undefined') { module.exports = { labels, headlines, time, percentage, dayDescription, dayClassName, historyRates }; return; }
  if (location.hostname === 'kakenhidesu.github.io') { location.replace('https://status.kusotech.club/'); return; }
  const $ = id => document.getElementById(id);
  const node = (tag, cls, text) => { const el = document.createElement(tag); if (cls) el.className = cls; if (text !== undefined) el.textContent = text; return el; };
  let data = null, period = 90, eventLimit = 10, loading = false, lastRendered = '';

  function renderComponents() {
    const expanded = new Set([...document.querySelectorAll('.probe-detail[open]')].map(el => el.dataset.id));
    const groups = new Map();
    const fragment = document.createDocumentFragment();
    for (const component of data.components) {
      let group = groups.get(component.group);
      if (!group) { group = node('div', 'component-group'); group.append(node('h3', 'group-title', component.group)); groups.set(component.group, group); fragment.append(group); }
      const row = node('article', 'component');
      const heading = node('div', 'component-heading');
      const name = node('div'); name.append(node('div', 'component-name', component.name), node('div', 'host', component.host));
      const isImage = component.id === 'originals' || component.id === 'thumbnails';
      heading.append(name, node('span', `state-label ${component.status}`, isImage && component.status === 'slow' ? '可用 · 加载较慢' : labels[component.status] || labels.unknown));
      row.append(heading);
      const bars = node('div', `bars period-${period}`); bars.setAttribute('role', 'group'); bars.setAttribute('aria-label', `${component.name} 最近 ${period} 天的记录`);
      const detail = node('p', 'day-detail'); detail.hidden = true;
      const history = component.history.slice(-period), rates = historyRates(history);
      history.forEach((day, index) => {
        const bar = node('button', dayClassName(day));
        bar.type = 'button'; bar.title = dayDescription(day); bar.setAttribute('aria-label', dayDescription(day)); bar.tabIndex = index === period - 1 ? 0 : -1;
        bar.addEventListener('click', () => { detail.textContent = dayDescription(day); detail.hidden = false; });
        bar.addEventListener('keydown', event => {
          if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
          event.preventDefault(); const children = [...bars.children], current = children.indexOf(bar);
          const next = event.key === 'Home' ? 0 : event.key === 'End' ? children.length - 1 : Math.max(0, Math.min(children.length - 1, current + (event.key === 'ArrowLeft' ? -1 : 1)));
          bar.tabIndex = -1; children[next].tabIndex = 0; children[next].focus();
        }); bars.append(bar);
      });
      const caption = node('div', 'bar-caption');
      caption.append(node('span', '', `${period} 天前`), node('span', 'uptime', `${percentage(rates.availability)} 已记录可用率`), node('span', '', '今天'));
      const probeDetail = node('details', 'probe-detail'); probeDetail.dataset.id = component.id; probeDetail.open = expanded.has(component.id);
      const summary = node('summary'); summary.append(node('span', '', component.latencyMs ? `${isImage ? '最长下载' : '最近响应'} ${component.latencyMs.toLocaleString()} ms` : '最近一次检测'), node('span', 'expand', '+'));
      probeDetail.append(summary, node('p', '', `${component.detail}。覆盖率 ${percentage(rates.coverage)}。`));
      row.append(bars, caption, detail, probeDetail); group.append(row);
    }
    $('components').replaceChildren(fragment); $('components').setAttribute('aria-busy', 'false');
  }

  function renderIncidents() {
    const events = data.incidents || [];
    $('history-count').textContent = events.length ? `${events.length} 条` : '';
    const fragment = document.createDocumentFragment();
    if (!events.length) fragment.append(node('div', 'empty', '暂无故障记录'));
    for (const event of events.slice(0, eventLimit)) {
      const row = node('article', 'incident'), when = node('div', 'incident-time', time(event.startedAt, true));
      when.append(node('span', '', time(event.startedAt).split(' ').at(-1)));
      const body = node('div'), top = node('div', 'incident-top');
      const state = { active: data.stale || data.storageError ? '等待更新' : '持续中', resolved: '已恢复', interrupted: '记录中断' }[event.status] || '未知';
      top.append(node('h3', '', `${event.componentName} · ${event.severity === 'outage' ? '访问失败' : '部分失败'}`), node('span', `badge ${event.status}`, state));
      body.append(top, node('p', '', event.detail));
      if (event.endedAt) body.append(node('p', 'resolution', `${event.status === 'resolved' ? '恢复于' : '最后确认异常于'} ${time(event.endedAt)}`));
      row.append(when, body); fragment.append(row);
    }
    $('incidents').replaceChildren(fragment); $('more-incidents').hidden = events.length <= eventLimit;
  }

  function render() {
    const state = data.overall;
    $('headline').textContent = data.storageError ? '监测记录暂时无法保存' : data.stale ? (data.checkedAt ? '监测记录等待更新' : '等待首次定期检测') : headlines[state] || headlines.unknown;
    $('hero-icon').className = `hero-icon ${state}`; $('hero-icon').textContent = { operational: '✓', slow: '≈', degraded: '!', outage: '×', unknown: '·' }[state] || '·';
    $('probe-location').textContent = `${data.location} · 每 ${data.intervalSeconds / 60} 分钟检测`;
    $('last-updated').textContent = `最后检测 ${time(data.checkedAt)}`;
    $('load-warning').hidden = !data.historyWarning; $('load-warning').textContent = data.historyWarning ? '部分历史记录无法读取。' : '';
    renderComponents(); renderIncidents();
  }
  async function load() {
    if (loading) return; loading = true;
    const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 10000);
    try {
      const response = await fetch('/api/status', { cache: 'no-store', credentials: 'omit', signal: controller.signal });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const next = await response.json();
      if (!Array.isArray(next.components) || !next.components.length || !Array.isArray(next.incidents)) throw new Error('Invalid status data');
      data = next;
      const key = JSON.stringify([data.checkedAt, data.overall, data.stale, data.storageError, data.historyWarning]);
      if (key !== lastRendered || !$('load-warning').hidden) { render(); lastRendered = key; }
    } catch {
      $('headline').textContent = '暂时无法读取监测记录'; $('hero-icon').className = 'hero-icon unknown'; $('hero-icon').textContent = '·';
      $('load-warning').hidden = false; $('load-warning').textContent = data ? `显示 ${time(data.checkedAt)} 的记录，正在重试。` : '正在重试。';
    } finally { clearTimeout(timer); loading = false; }
  }
  document.querySelectorAll('[data-period]').forEach(button => button.addEventListener('click', () => {
    period = Number(button.dataset.period);
    document.querySelectorAll('[data-period]').forEach(item => { const selected = item === button; item.classList.toggle('selected', selected); item.setAttribute('aria-pressed', String(selected)); });
    if (data) renderComponents();
  }));
  $('more-incidents').addEventListener('click', () => { eventLimit += 10; if (data) renderIncidents(); });
  load(); setInterval(() => { if (!document.hidden) load(); }, 60000);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) load(); });
})();
