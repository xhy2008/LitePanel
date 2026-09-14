import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mountApp } from '../boot';

// 移动端 Chrome 没有 DevTools：启动期一旦抛错就是纯白屏，什么线索都不留。
// 因此 bootstrap 必须把错误直接写在页面上——宁可看到丑陋的报错条，
// 也不要让用户对着白屏猜。这是可观测性的兜底，不是装饰。
function makeDom() {
  document.documentElement.innerHTML = '<body><div id="app"></div></body>';
}

describe('mountApp 启动失败可见化', () => {
  beforeEach(makeDom);
  afterEach(() => vi.restoreAllMocks());

  it('mount 抛错时把错误文本渲染进 #app 而不是留白屏', () => {
    const err = new Error('boom in setup');
    const boot = vi.fn(() => {
      throw err;
    });

    mountApp({ boot, onError: vi.fn() });

    const html = document.getElementById('app')?.innerHTML ?? '';
    expect(html).toContain('启动失败');
    expect(html).toContain('boom in setup');
  });

  it('错误栈也要带上：手机上看不到 DevTools，只能靠页面里的栈', () => {
    const err = Object.assign(new Error('x'), { stack: 'at foo (index.js:1:2)' });
    mountApp({ boot: () => { throw err; }, onError: vi.fn() });
    expect(document.getElementById('app')?.textContent).toContain('index.js:1:2');
  });

  it('成功时不往 #app 里塞任何报错内容', () => {
    const boot = vi.fn(() => {
      document.getElementById('app')!.innerHTML = '<div id="real">ok</div>';
    });
    const onError = vi.fn();
    mountApp({ boot, onError });
    expect(document.getElementById('app')?.innerHTML).toContain('id="real"');
    expect(onError).not.toHaveBeenCalled();
  });

  it('boot 返回 rejected promise 时同样可见（动态 import 失败最常见）', async () => {
    const onError = vi.fn();
    mountApp({ boot: () => Promise.reject(new Error('chunk load failed')), onError });
    await Promise.resolve();
    await Promise.resolve();
    expect(onError).toHaveBeenCalled();
    expect(document.getElementById('app')?.textContent).toContain('chunk load failed');
  });

  it('同时把错误抛回 window.onerror，便于将来接远端上报', () => {
    const onError = vi.fn();
    const err = new Error('e2');
    mountApp({ boot: () => { throw err; }, onError });
    expect(onError).toHaveBeenCalledWith(err);
  });
});

// 内联诊断钩子在 index.html 里，vitest 覆盖不到它的运行时行为，
// 但能锁住“它存在且条件收紧过”：一旦有人重构时删掉，或放宽到
// 把第三方注入脚本的失败也当致命错误（会凭空造出白屏），这里就红。
describe('index.html 内联诊断钩子', async () => {
  const html = await import('../..//index.html?raw' as string).then((m) => (m as any).default);

  it('必须有加载期错误兜底，且只认我们自己的产物', () => {
    expect(html).toContain('__litepanelFatal');
    expect(html).toContain('/assets/');
    expect(html).toMatch(/addEventListener\(\s*'error'/);
  });

  it('必须有挂起看门狗，用来区分“抛错”和“卡住”', () => {
    expect(html).toContain('__litepanelReady');
    expect(html).toContain('启动挂起');
  });
});
