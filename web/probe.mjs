// 直接加载 Vite 构建产物，验证它在“真实浏览器式”的 DOM 里能否挂载出界面。
import { JSDOM } from 'jsdom';
import { readFileSync, readdirSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import path from 'node:path';

const dist = '/data/data/com.termux/files/home/server-panel/internal/webdist/dist';
const html = readFileSync(path.join(dist, 'index.html'), 'utf8');
const entry = html.match(/src="(\/assets\/[^"]+\.js)"/)[1];

const dom = new JSDOM(html.replace(/<script[^>]*><\/script>/, ''), {
  url: 'http://127.0.0.1:9530/',
  pretendToBeVisual: true,
});
const { window } = dom;
globalThis.window = window;
globalThis.document = window.document;
for (const k of ['HTMLElement','Element','Node','Event','CustomEvent','MutationObserver','getComputedStyle','requestAnimationFrame','cancelAnimationFrame','navigator','location','history','localStorage','WebSocket','SVGElement']) {
  globalThis[k] = window[k];
}

// 拦截所有网络请求，看清挂载过程中到底发了哪些、有没有卡住。
const seen = [];
globalThis.fetch = async (p, o) => {
  seen.push((o?.method ?? 'GET') + ' ' + p);
  return { ok: true, status: 200, headers: new Map([['get', () => null]]), json: async () => ({ authenticated: true, must_change_password: false }) };
};
window.fetch = globalThis.fetch;

process.on('unhandledRejection', (e) => console.log('!! unhandledRejection:', e?.message ?? e));

const t0 = Date.now();
const mod = path.join(dist, entry);
await import(pathToFileURL(mod).href);
await new Promise((r) => setTimeout(r, 1500));

const app = window.document.getElementById('app');
console.log('入口:', entry);
console.log('挂载后 fetch 请求:', seen);
console.log('#app 子节点数:', app.children.length);
console.log('渲染出的根元素:', app.firstElementChild?.outerHTML?.slice(0, 200) ?? '(空)');
console.log('耗时 ms:', Date.now() - t0);
process.exit(0);
