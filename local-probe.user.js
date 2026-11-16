// ==UserScript==
// @name         X 岛状态页：本地接口验证
// @namespace    https://github.com/kakenhidesu/xdnmb-stat
// @version      1.0.1
// @description  从测试者自己的浏览器验证公开接口，不使用服务器代理，不发送账号 Cookie。
// @match        https://kakenhidesu.github.io/xdnmb-stat/*
// @match        https://status.kusotech.club/*
// @match        http://localhost:18787/*
// @match        http://127.0.0.1:18787/*
// @connect      www.nmbxd.com
// @connect      www.nmbxd1.com
// @connect      api.nmb.best
// @connect      nmb.ovear.info
// @grant        GM_xmlhttpRequest
// @run-at       document-start
// ==/UserScript==

(() => {
  'use strict';
  const channel = 'xdnmb-status-v1';
  const requests = new Map();
  function allowed(value) {
    try {
      const url = new URL(value);
      if (url.protocol !== 'https:' || url.username || url.password || url.port || url.hash) return false;
      const keys = [...url.searchParams.keys()];
      if (new Set(keys).size !== keys.length) return false;
      if (url.host === 'nmb.ovear.info') return url.pathname === '/nmb-notice.json' && keys.every(x => x === '_');
      if (!['www.nmbxd.com', 'www.nmbxd1.com', 'api.nmb.best'].includes(url.host)) return false;
      if (url.pathname === '/Api/getCdnPath') return keys.every(x => x === '_');
      return url.pathname === '/Api/showf' && url.searchParams.get('id') === '4' &&
        url.searchParams.get('page') === '1' && keys.every(x => ['id', 'page', '_'].includes(x));
    } catch { return false; }
  }
  function send(message) { window.postMessage({ channel, ...message }, location.origin); }
  window.addEventListener('message', event => {
    if (event.source !== window || event.origin !== location.origin || event.data?.channel !== channel) return;
    const message = event.data;
    if (typeof message.id !== 'string' || message.id.length > 80) return;
    if (message.type === 'hello') { send({ type: 'ready', id: message.id }); return; }
    if (message.type === 'abort') { requests.get(message.id)?.abort(); requests.delete(message.id); return; }
    if (message.type !== 'request') return;
    if (!allowed(message.url) || requests.has(message.id) || requests.size >= 8) {
      send({ type: 'response', id: message.id, error: '请求不在公开检测范围内或并发过多' });
      return;
    }
    const done = result => {
      requests.delete(message.id);
      send({ type: 'response', id: message.id, ...result });
    };
    try {
      const request = GM_xmlhttpRequest({
        method: 'GET', url: message.url, anonymous: true, timeout: 10000,
        redirect: 'error',
        headers: { 'Cache-Control': 'no-cache' },
        onload: response => {
          if (!allowed(response.finalUrl || message.url) || response.responseText.length > 2000000) {
            done({ error: '响应地址或长度超出检测范围' });
            return;
          }
          done({ status: response.status, body: response.responseText,
            contentType: /^content-type:\s*([^\r\n]+)/im.exec(response.responseHeaders)?.[1] || '' });
        },
        onerror: () => done({ error: '浏览器请求失败，可能与网络、服务故障或脚本权限有关' }),
        ontimeout: () => done({ error: '浏览器请求超时' }),
        onabort: () => done({ error: '浏览器请求已取消' }),
      });
      requests.set(message.id, request);
    } catch { done({ error: '浏览器脚本请求失败，请检查脚本管理器权限' }); }
  });
})();
