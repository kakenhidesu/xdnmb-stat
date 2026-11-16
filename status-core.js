/* All health results are measured by the tester. The server only supplies image addresses. */
const XdaoStatus = (() => {
  const TIMEOUT = 10000, SLOW = 3000;
  const RECENT_COUNT = 3, RECENT_DAYS = 7, MAX_CDNS = 3;
  const APIS = [
    { id: 'main', name: '主站接口', origin: 'https://www.nmbxd.com' },
    { id: 'backup', name: '主站备用接口', origin: 'https://www.nmbxd1.com' },
    { id: 'api', name: '备用 API', origin: 'https://api.nmb.best' },
  ];
  const NOTICE_URL = 'https://nmb.ovear.info/nmb-notice.json';
  const DEFAULT_CDN = 'https://image.nmb.best/';
  const BASELINES = ['2022-12-04/638c87770b738.jpg', '2026-02-12/698deed346c6c.png'];
  const STATE_TEXT = { ok: '通过', slow: '缓慢', partial: '部分失败', bad: '失败', unknown: '无法验证' };

  class ProbeError extends Error {
    constructor(message, status = 'bad') { super(message); this.status = status; }
  }
  function bustCache(url) {
    const target = new URL(url);
    target.searchParams.set('_', Date.now().toString());
    return target.href;
  }
  function cdnBase(value) {
    const url = new URL(value);
    if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash) throw new Error('Invalid CDN URL');
    return url.href.replace(/\/?$/, '/');
  }
  function validateCdns(data) {
    if (!Array.isArray(data) || !data.length || !data.every(x => x && typeof x.url === 'string')) return false;
    try { data.forEach(x => cdnBase(x.url)); return true; } catch { return false; }
  }
  function validPost(post) {
    return post && /^\d+$/.test(String(post.id)) && typeof post.content === 'string' &&
      typeof post.img === 'string' && typeof post.ext === 'string';
  }
  function validatePosts(data) {
    return Array.isArray(data) && data.length > 0 && data.every(post => validPost(post) &&
      (post.Replies === undefined || (Array.isArray(post.Replies) && post.Replies.every(validPost))));
  }
  function validateNotice(data) {
    return data && typeof data === 'object' && typeof data.content === 'string' &&
      typeof data.enable === 'boolean' && /^\d+$/.test(String(data.date));
  }
  function aggregate(results) {
    const states = results.map(x => x.status);
    if (!states.length) return 'unknown';
    if (states.every(x => x === 'bad')) return 'bad';
    if (states.some(x => x === 'bad' || x === 'partial')) return 'partial';
    if (states.includes('unknown')) return 'unknown';
    return states.includes('slow') ? 'slow' : 'ok';
  }
  async function checkJson(url, validate, options = {}) {
    const fetcher = options.fetcher || fetch;
    const timeout = options.timeout ?? TIMEOUT;
    const ctrl = new AbortController();
    const start = Date.now();
    let timer;
    try {
      const deadline = new Promise((_, reject) => {
        timer = setTimeout(() => {
          reject(new ProbeError('请求超时'));
          ctrl.abort();
        }, timeout);
      });
      const request = (async () => {
        const response = await fetcher(bustCache(url), {
          mode: 'cors', credentials: 'omit', cache: 'no-store', signal: ctrl.signal,
          referrerPolicy: 'no-referrer',
        });
        if (response.type === 'opaque' || response.status === 0) throw new ProbeError('无法读取接口响应', 'unknown');
        if (!response.ok) throw new ProbeError(`HTTP ${response.status}${response.status === 429 ? '（请求过于频繁）' : ''}`);
        if (!/\bapplication\/(?:[\w.-]+\+)?json\b/i.test(response.headers.get('content-type') || '')) {
          throw new ProbeError('响应不是 JSON，可能是错误页或验证页');
        }
        let data;
        try { data = await response.json(); }
        catch (error) {
          if (error instanceof SyntaxError) throw new ProbeError('JSON 无法解析');
          throw error;
        }
        if (!validate(data)) throw new ProbeError('返回内容不符合接口格式');
        return data;
      })();
      const data = await Promise.race([request, deadline]);
      const elapsed = Date.now() - start;
      return { status: elapsed > SLOW ? 'slow' : 'ok', elapsed, detail: 'HTTP 与返回内容验证通过', data };
    } catch (error) {
      return { status: error instanceof ProbeError ? error.status : 'unknown', elapsed: Date.now() - start,
        detail: error instanceof ProbeError ? error.message : '无法读取响应：可能受跨域策略、网络或服务故障影响' };
    } finally { clearTimeout(timer); }
  }
  async function checkApi(api, options = {}) {
    // Two sequential requests per origin avoid a burst against the same API.
    const config = await checkJson(api.origin + '/Api/getCdnPath', validateCdns, options);
    const posts = await checkJson(api.origin + '/Api/showf?id=4&page=1', validatePosts, options);
    return { id: api.id, name: api.name, origin: api.origin, status: aggregate([config, posts]), config, posts };
  }
  function recentPaths(postLists, now = Date.now()) {
    const files = new Set();
    for (const posts of postLists) {
      for (const post of posts.flatMap(x => [x, ...(x.Replies || [])])) {
        const path = post.img + post.ext;
        if (!/^\d{4}-\d{2}-\d{2}\/[a-zA-Z0-9_-]+\.(?:jpe?g|png|gif|webp|avif)$/i.test(path)) continue;
        const day = Date.parse(path.slice(0, 10) + 'T00:00:00Z');
        if (!Number.isFinite(day) || new Date(day).toISOString().slice(0, 10) !== path.slice(0, 10)) continue;
        if (day > now + 86400000 || now - day > RECENT_DAYS * 86400000 || BASELINES.includes(path)) continue;
        files.add(path);
      }
    }
    return [...files].sort().reverse().slice(0, RECENT_COUNT);
  }
  function validateCatalog(data, now = Date.now()) {
    if (!data || !Number.isFinite(data.generatedAt) || now - data.generatedAt > 15 * 60000 ||
        data.generatedAt > now + 60000 || !Array.isArray(data.cdns) || !data.cdns.length ||
        data.cdns.length > MAX_CDNS || !Array.isArray(data.paths) || data.paths.length > RECENT_COUNT) return false;
    return validateCdns(data.cdns.map(url => ({ url }))) && data.paths.every(path =>
      typeof path === 'string' && /^\d{4}-\d{2}-\d{2}\/[a-zA-Z0-9_-]+\.(?:jpe?g|png|gif|webp|avif)$/i.test(path));
  }
  function imagePlan(apis, now = Date.now(), catalog = null) {
    const liveBases = apis.flatMap(x => (x.config.data || []).map(cdn => cdnBase(cdn.url)));
    const usableCatalog = validateCatalog(catalog, now) ? catalog : null;
    const bases = [...new Set(liveBases.length ? liveBases : (usableCatalog?.cdns || []))];
    const postLists = apis.map(x => x.posts.data).filter(Boolean);
    if (usableCatalog) postLists.push(usableCatalog.paths.map(path => ({
      img: path.slice(0, path.lastIndexOf('.')), ext: path.slice(path.lastIndexOf('.')),
    })));
    const recent = recentPaths(postLists, now);
    const coverage = [];
    if (!bases.length) coverage.push('未能取得当前 CDN 配置，仅检测备用地址');
    if (bases.length > MAX_CDNS) coverage.push(`配置包含 ${bases.length} 个 CDN，本轮仅覆盖 ${MAX_CDNS} 个`);
    if (recent.length < RECENT_COUNT) coverage.push(`最近 ${RECENT_DAYS} 天图片样本不足（${recent.length}/${RECENT_COUNT}）`);
    const samples = [...BASELINES.map(path => ({ path, kind: '固定样本' })), ...recent.map(path => ({ path, kind: '近期图片' }))];
    const checks = (bases.length ? bases : [DEFAULT_CDN]).slice(0, MAX_CDNS).flatMap(base =>
      samples.flatMap(sample => ['thumb', 'image'].map(size => ({
        ...sample, size, url: new URL(size + '/' + sample.path, base).href,
        label: `${sample.kind} · ${size === 'thumb' ? '缩略图' : '原图'}`,
      }))));
    return { checks, coverage, recentCount: recent.length,
      catalogUsed: Boolean(usableCatalog), catalogTime: usableCatalog?.generatedAt };
  }
  function checkImage(url, options = {}) {
    return new Promise(resolve => {
      const img = new (options.ImageClass || Image)();
      const start = Date.now();
      let settled = false;
      const finish = (status, detail) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        img.onload = img.onerror = null;
        const elapsed = Date.now() - start;
        resolve({ status: status === 'ok' && elapsed > SLOW ? 'slow' : status, elapsed, detail });
        img.removeAttribute('src');
      };
      const timer = setTimeout(() => finish('bad', '图片加载超时'), options.timeout ?? TIMEOUT);
      img.onload = () => img.naturalWidth > 0 && img.naturalHeight > 0
        ? finish('ok', '图片加载并解码成功') : finish('bad', '图片无法解码');
      img.onerror = () => finish('bad', '图片加载失败，具体原因无法由浏览器确定');
      img.referrerPolicy = 'no-referrer';
      img.src = bustCache(url);
    });
  }
  async function mapLimit(items, limit, fn) {
    const results = new Array(items.length);
    let next = 0;
    await Promise.all(Array.from({ length: Math.min(limit, items.length) }, async () => {
      while (next < items.length) { const i = next++; results[i] = await fn(items[i], i); }
    }));
    return results;
  }
  function imageSummary(plan, results) {
    const status = aggregate([...results, ...plan.coverage.map(() => ({ status: 'unknown' }))]);
    return { status, passed: results.filter(x => x.status === 'ok' || x.status === 'slow').length,
      total: results.length, coverage: plan.coverage };
  }
  function banner(results) {
    const status = aggregate(results);
    return {
      ok: { cls: 'ok', text: '本轮抽样检测通过' },
      slow: { cls: 'partial', text: '本轮抽样通过，部分响应缓慢' },
      partial: { cls: 'partial', text: '部分检测失败，请查看详情' },
      bad: { cls: 'down', text: '本轮检测均失败，请查看详情' },
      unknown: { cls: 'partial', text: '检测不完整，部分项目无法验证' },
    }[status];
  }
  return { TIMEOUT, SLOW, RECENT_COUNT, RECENT_DAYS, APIS, NOTICE_URL, BASELINES, STATE_TEXT,
    validateCdns, validatePosts, validateNotice, validateCatalog, checkJson, checkApi, recentPaths, imagePlan,
    checkImage, mapLimit, aggregate, imageSummary, banner };
})();
if (typeof module !== 'undefined') module.exports = XdaoStatus;
