import { describe, expect, it, vi } from 'vitest';
import { keysFrame, resizeFrame, type FrameDeps, createTerminalRuntime } from '../composables/useTerminal';

// 上行帧的字节形状是与 Go internal/termws/frame.go 的**线格式约定**，
// 必须写死字节而不是"和 encode 自己比一圈"：自己跟自己比永远一致，
// 但同时改掉两边却没人发现时，它照样绿。
describe('上行帧字节形状', () => {
  it('按键 = k + 原始字节', () => {
    expect(Array.from(keysFrame(new TextEncoder().encode('ls\r')))).toEqual([
      0x6b, 0x6c, 0x73, 0x0d,
    ]);
  });

  it('按键为空也要留类型字节', () => {
    expect(Array.from(keysFrame(new Uint8Array(0)))).toEqual([0x6b]);
  });

  // cols/rows 是 2 字节大端。写成小端的话，80 列会变成 20480 列，
  // tmux 那边要么报错要么开出一个荒谬的尺寸，而前端看起来"一切正常"。
  it('尺寸 = r + cols(2B 大端) + rows(2B 大端)', () => {
    expect(Array.from(resizeFrame(80, 24))).toEqual([0x72, 0x00, 0x50, 0x00, 0x18]);
  });

  it('超过 65535 的尺寸被夹住（否则会静默回绕成别的值）', () => {
    const f = resizeFrame(70000, 24);
    expect(f[1]).toBe(0xff);
    expect(f[2]).toBe(0xff);
  });

  it('中文按键按 UTF-8 原样传', () => {
    expect(Array.from(keysFrame(new TextEncoder().encode('中文')))).toEqual([
      0x6b, 0xe4, 0xb8, 0xad, 0xe6, 0x96, 0x87,
    ]);
  });
});

class FakeTerm {
  written: (string | Uint8Array)[] = [];
  cols = 80;
  rows = 24;
  // 容器尺寸。真终端里 cols/rows 是 fit 依据容器算出来的，
  // 容器没变时重复 fit 必须得到同一个值 —— 假对象也得遵守，
  // 否则"尺寸没变就不上报"这类守卫永远测不到（fit 每次都能造出新高值）。
  width = 800;
  height = 480;
  private dataCb: ((s: string) => void) | null = null;
  private resizeCb: ((s: { cols: number; rows: number }) => void) | null = null;
  disposed = false;
  write(b: Uint8Array | string) {
    this.written.push(b);
  }
  /** 模拟容器大小变化（转屏、软键盘弹出、侧栏收起） */
  resizeContainer(w: number, h: number) {
    this.width = w;
    this.height = h;
  }
  /**
   * 按 xterm 的 cellSize 语义算尺寸：这里是整除。
   * 刻意**不**夹到 >=1：容器 display:none 时真 fit 得到的就是 0，
   * 夹住了就等于假对象替实现圆场，"忽略 0 尺寸"那条守卫测不到。
   */
  computeSize() {
    this.cols = Math.floor(this.width / 10);
    this.rows = Math.floor(this.height / 20);
  }
  onData(cb: (s: string) => void) {
    this.dataCb = cb;
    return { dispose: () => (this.dataCb = null) };
  }
  onResize(cb: (s: { cols: number; rows: number }) => void) {
    this.resizeCb = cb;
    return { dispose: () => (this.resizeCb = null) };
  }
  dispose() {
    this.disposed = true;
  }
  /** 模拟用户在 xterm 里打字 */
  type(s: string) {
    this.dataCb?.(s);
  }
  /** 模拟 xterm 内部尺寸变了（write 时的自动换行也可能触发） */
  fireResize(cols: number, rows: number) {
    this.cols = cols;
    this.rows = rows;
    this.resizeCb?.({ cols, rows });
  }
}

class FakeFit {
  fits = 0;
  constructor(public term: FakeTerm) {}
  fit() {
    this.fits += 1;
    this.term.computeSize();
  }
}

function boot(over: Partial<FrameDeps> = {}) {
  const term = new FakeTerm();
  term.computeSize(); // 80x24（800/10, 480/20）
  const sent: Uint8Array[] = [];
  const deps: FrameDeps = {
    term: term as any,
    fit: new FakeFit(term) as any,
    channel: 'term:1',
    send: vi.fn((b: Uint8Array) => sent.push(b)),
    connected: () => true,
    onSize: over.onSize,
    ...over,
  };
  return { term, fit: deps.fit as unknown as FakeFit, sent, deps, rt: createTerminalRuntime(deps) };
}

const dec = (b: Uint8Array) => new TextDecoder().decode(b);

describe('terminal 运行时：输出方向', () => {
  // 必须原样给字节，不能先 TextDecoder 再写字符串：一个中文字符的 UTF-8
  // 三字节可能被拆在两个 WS 帧里，逐帧解码会把它变成两个 U+FFFD。
  it('WS 二进制帧原样（同一引用/同一字节序列）写进终端', () => {
    const { rt, term } = boot();
    const p = new TextEncoder().encode('你好\r\n');
    rt.onPayload(p);
    expect(term.written).toHaveLength(1);
    expect(Array.from(term.written[0] as Uint8Array)).toEqual(Array.from(p));
  });
});

describe('terminal 运行时：输入方向', () => {
  it('打字 → k 帧发出去', () => {
    const { rt, term, sent } = boot();
    rt.attach();
    term.type('ls\r');
    const keys = sent.filter((f) => f[0] === 0x6b);
    expect(keys).toHaveLength(1);
    expect(dec(keys[0])).toBe('kls\r');
  });

  // 连接没好时不能把按键丢掉还静默：用户会以为服务器卡了。
  // 也不能直接发：WS 没开，sendTerm 会抛。
  it('未连接时不发帧，但报告被丢弃', () => {
    const dropped = vi.fn();
    const { rt, term, sent } = boot({ connected: () => false, onDropped: dropped });
    rt.attach();
    term.type('ls\r');
    expect(sent).toHaveLength(0);
    expect(dropped).toHaveBeenCalledTimes(1);
  });

  // 断线期间攒下的按键在恢复后不能一股脑补发：那是把用户半小时前
  // 敲的字符塞进现在的 shell（"rm -rf" 半路重连就晚了）。
  it('重连后不补发断线期间的按键', () => {
    let up = false;
    const { rt, term, sent } = boot({ connected: () => up });
    rt.attach();
    term.type('rm -rf /tmp/x');
    up = true;
    rt.onOpen();
    expect(sent.filter((f) => dec(f).startsWith('k'))).toHaveLength(0);
  });
});

describe('terminal 运行时：尺寸上报', () => {
  it('attach 时按 fit 的结果上报一次', () => {
    const onSize = vi.fn();
    const { rt, sent } = boot({ onSize });
    rt.attach();
    // 首帧就该把尺寸报上去：tmux 那边的窗口在桥接接管时按默认 80x24
    // 开出来，真实尺寸不报，浏览器看到的就是 80 列的窄屏。
    expect(Array.from(sent[0])).toEqual([0x72, 0x00, 0x50, 0x00, 0x18]);
    expect(onSize).toHaveBeenCalledWith(80, 24);
  });

  // 每 200ms 一次 resize 检查若不带"变了才发"，会把 WS 灌满无意义的帧，
  // 后端每条都要给 tmux 发一次 window-size（ tmux 还要往 shell 发 SIGWINCH，
  // 打断正在打印的程序）。
  it('尺寸没变时不重复上报', () => {
    const { rt, sent } = boot();
    rt.attach();
    const before = sent.length;
    rt.checkSize();
    rt.checkSize();
    expect(sent.length).toBe(before);
  });

  it('尺寸变了才上报，且带上新的 cols/rows', () => {
    const onSize = vi.fn();
    const { rt, sent, term } = boot({ onSize });
    rt.attach();
    term.resizeContainer(1200, 800); // → 120x40
    rt.checkSize();
    const last = sent[sent.length - 1];
    expect(Array.from(last)).toEqual([0x72, 0x00, 0x78, 0x00, 0x28]);
    expect(onSize).toHaveBeenLastCalledWith(120, 40);
  });

  // 手机端软键盘弹出/收起会让容器高度抖动。上报窗口过窄会导致
  // tmux 在两个尺寸间来回切，屏幕上的历史行会跳。
  it('终端自己的 onResize 也会触发上报', () => {
    const onSize = vi.fn();
    const { rt, term, sent } = boot({ onSize });
    rt.attach();
    // 容器变化与 xterm 自己算出的尺寸要一致，否则假对象就在替实现圆场
    term.resizeContainer(1000, 600);
    term.fireResize(100, 30);
    const last = sent[sent.length - 1];
    expect(Array.from(last)).toEqual([0x72, 0x00, 0x64, 0x00, 0x1e]);
  });

  // 尺寸为 0 出现在容器还没布局好（display:none 的标签页）时。
  // 发给 tmux 会得到一个 0 列的窗口，之后的输出全挤成一条。
  it('忽略 0 尺寸', () => {
    const { rt, sent, term } = boot();
    rt.attach();
    const before = sent.length;
    term.resizeContainer(0, 0);
    rt.checkSize();
    expect(sent.length).toBe(before);
  });
});

describe('terminal 运行时：发送失败', () => {
  // isOpen() 到真正 send 之间连接刚好断掉是常态（拔网线、切后台），
  // ws.sendTerm 那里会抛。让它从 xterm 的回调里逃出去会打断整个页面的渲染。
  it('send 抛错时不外泄，按丢弃计数', () => {
    const dropped = vi.fn();
    const { rt, term } = boot({
      send: () => {
        throw new Error('未连接');
      },
      onDropped: dropped,
    });
    rt.attach();
    expect(() => term.type('ls\r')).not.toThrow();
    expect(dropped).toHaveBeenCalled();
  });
});

describe('terminal 运行时：释放', () => {
  // 切标签时不 dispose 旧实例，等于每个看过的会话都留一条 WS 订阅
  // 加一条 tmux 控制连接；来回切十次就有十份输出在往浏览器里灌。
  it('dispose 之后不再发送也不写入', () => {
    const { rt, term, sent } = boot();
    rt.attach();
    rt.dispose();
    term.type('still typing\r');
    expect(sent.filter((f) => dec(f).startsWith('k'))).toHaveLength(0);
    rt.onPayload(new TextEncoder().encode('late'));
    expect(term.written).toHaveLength(0);
  });

  it('dispose 会销毁终端本身', () => {
    const { rt, term } = boot();
    rt.attach();
    rt.dispose();
    expect(term.disposed).toBe(true);
  });

  // 重复 dispose 会二次调用 xterm 的 dispose：真 xterm 那里对已释放
  // 的对象再 dispose 会抛，把路由切换整个打断。
  it('重复 dispose 幂等', () => {
    const { rt } = boot();
    rt.attach();
    rt.dispose();
    expect(() => rt.dispose()).not.toThrow();
  });
});

describe('terminal 运行时：尺寸上报的失败重试', () => {
  // 发送失败的尺寸变更不能被视为"已上报"。若 lastCols 在 send 之前更新，
  // 这次变更就被永久吞掉：tmux 那边一直停在旧窗口尺寸上，屏幕右侧的
  // 内容会被切掉，而且之后无论怎么调整窗口都不会再修好。
  it('发送抛错后下一次检查会重试同一尺寸', () => {
    let boom = true;
    const sent: Uint8Array[] = [];
    const { rt, term } = boot({
      send: (b) => {
        if (b[0] !== 0x72) return;
        if (boom) {
          boom = false;
          throw new Error('连接断了');
        }
        sent.push(b);
      },
    });
    rt.attach(); // 第一次上报被吞
    term.resizeContainer(1200, 800); // → 120x40，这一次应当发出去
    rt.checkSize();
    expect(sent.map((f) => Array.from(f))).toEqual([[0x72, 0, 120, 0, 40]]);
  });

  it('未连接时不上报，恢复连接后补上', () => {
    let up = false;
    const sent: Uint8Array[] = [];
    const { rt } = boot({
      connected: () => up,
      send: (b) => {
        if (b[0] === 0x72) sent.push(b);
      },
    });
    rt.attach();
    expect(sent).toHaveLength(0);
    up = true;
    rt.onOpen();
    expect(sent.map((f) => Array.from(f))).toEqual([[0x72, 0, 80, 0, 24]]);
  });
});
