// 启动引导：把任何启动期异常变成屏幕上看得见的文字。
//
// 为什么需要它：面板主要通过手机 Chrome 访问，那里没有 DevTools。
// 启动期一抛错就是纯白屏，用户只能反复刷新，我也拿不到任何线索。
// 宁可显示一条丑陋的报错条，也不要白屏。
export interface BootDeps {
  boot: () => unknown | Promise<unknown>;
  // 错误回抛点：将来可接远端上报；测试里用作断言钩子。
  onError?: (e: unknown) => void;
  root?: HTMLElement | null;
}

export function mountApp(deps: BootDeps) {
  const report = (e: unknown) => {
    renderFatal(deps.root ?? document.getElementById('app'), e);
    deps.onError?.(e);
  };

  try {
    const r = deps.boot() as unknown;
    if (r && typeof (r as Promise<unknown>).then === 'function') {
      (r as Promise<unknown>).catch(report);
    }
  } catch (e) {
    report(e);
  }
}

function renderFatal(el: HTMLElement | null | undefined, e: unknown) {
  const msg = e instanceof Error ? e.message : String(e);
  const stack = e instanceof Error && e.stack ? e.stack : '';

  // 没有 #app 可用时（脚本比 DOM 先跑）只能退回整页文本。
  if (!el) {
    document.body.textContent = `LitePanel 启动失败：${msg}\n${stack}`;
    return;
  }

  el.textContent = '';
  const box = document.createElement('div');
  box.setAttribute('style', 'padding:16px;font:13px/1.6 ui-monospace,monospace;color:#ff6b6b');

  const title = document.createElement('div');
  title.setAttribute('style', 'font-weight:700;margin-bottom:8px');
  title.textContent = 'LitePanel 启动失败';
  box.appendChild(title);

  const m = document.createElement('div');
  m.setAttribute('style', 'color:#ffb86b;margin-bottom:8px');
  m.textContent = msg;
  box.appendChild(m);

  if (stack) {
    const pre = document.createElement('pre');
    pre.setAttribute(
      'style',
      'white-space:pre-wrap;word-break:break-all;color:#8b949e;font-size:11px;margin:0',
    );
    pre.textContent = stack;
    box.appendChild(pre);
  }

  const hint = document.createElement('div');
  hint.setAttribute('style', 'margin-top:10px;color:#8b949e');
  hint.textContent = '把以上内容发给开发者；服务端 dev/litepanel.log 里同时有请求日志。';
  box.appendChild(hint);

  el.appendChild(box);
}
