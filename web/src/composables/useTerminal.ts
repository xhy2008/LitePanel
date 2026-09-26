import type { Terminal } from '@xterm/xterm';
import type { FitAddon } from '@xterm/addon-fit';

// 上行帧负载格式，与后端 internal/termws/frame.go 完全对应：
//   'k' | 原始按键字节...
//   'r' | cols(2B 大端) | rows(2B 大端)
const KEYS = 0x6b; // 'k'
const RESIZE = 0x72; // 'r'
const MAX_DIM = 0xffff;

export function keysFrame(data: Uint8Array): Uint8Array {
  const out = new Uint8Array(1 + data.length);
  out[0] = KEYS;
  out.set(data, 1);
  return out;
}

export function resizeFrame(cols: number, rows: number): Uint8Array {
  // 超过 2 字节会静默回绕成别的值（70000 → 4464），比夹住更难查。
  const c = Math.max(0, Math.min(MAX_DIM, Math.trunc(cols)));
  const r = Math.max(0, Math.min(MAX_DIM, Math.trunc(rows)));
  return new Uint8Array([RESIZE, (c >> 8) & 0xff, c & 0xff, (r >> 8) & 0xff, r & 0xff]);
}

export interface FrameDeps {
  // cols/rows 是 fit() 之后要读的现成尺寸；单独列出来（而不是收整个
  // Terminal）才能让测试传一个假终端。
  term: Pick<Terminal, 'write' | 'onData' | 'onResize' | 'dispose' | 'cols' | 'rows'>;
  fit: Pick<FitAddon, 'fit'>;
  channel: string;
  /** 发一帧到该会话频道（实现里是 ws.sendTerm(channel, payload)）。 */
  send(payload: Uint8Array): void;
  /** WS 是否可用。按键在没有连接时不能默默排队。 */
  connected(): boolean;
  onSize?: (cols: number, rows: number) => void;
  /** 断线期间被丢弃的按键数量（视图据此提示，而不是让用户怀疑服务器卡了）。 */
  onDropped?: (n: number) => void;
}

export interface TerminalRuntime {
  /** 挂上 onData/onResize，并把当前尺寸上报一次。 */
  attach(): void;
  /** WS 上的二进制负载（已去掉频道头）→ 终端。 */
  onPayload(p: Uint8Array): void;
  /** 连接恢复：只补报一次尺寸，**不**补发断线期间的按键。 */
  onOpen(): void;
  /** 定时兜底：布局变化不一定触发 xterm 的 onResize。 */
  checkSize(): void;
  dispose(): void;
}

const encoder = new TextEncoder();

export function createTerminalRuntime(deps: FrameDeps): TerminalRuntime {
  let lastCols = 0;
  let lastRows = 0;
  let dropped = 0;
  let attached = false;
  let disposed = false;
  // xterm 的 onData/onResize 返回 IDisposable，不是卸载函数：当成函数调
  // 会当场 TypeError，而这个错误只在"切换标签/离开页面"这条路上出现，
  // 平时一路顺利，离开时整条路由炸掉。
  const disposables: { dispose(): void }[] = [];

  function measure(): { cols: number; rows: number } | null {
    // 容器还没布局好（display:none 的标签、键盘动画中）时 fit 会算出 0。
    // 0 发给 tmux 会得到一个 0 列的窗口，之后的输出全挤成一条竖线。
    try {
      deps.fit.fit();
    } catch {
      return null; // 终端已销毁或容器不在文档里
    }
    const cols = deps.term.cols;
    const rows = deps.term.rows;
    if (cols < 1 || rows < 1) return null;
    return { cols, rows };
  }

  function reportSize(force = false) {
    if (disposed) return;
    const m = measure();
    if (!m) return;
    // 尺寸不上报也要回调：视图要据此判断"屏幕与 tmux 尺寸不一致"并提示。
    deps.onSize?.(m.cols, m.rows);
    // "已上报"只在真的发出去之后成立。若先更新 last* 再发，发送一失败
    // 这次变更就被永久吞掉：tmux 停在旧窗口尺寸上，屏幕右侧内容被切掉，
    // 而且之后怎么调整窗口都不会再修好（窗口尺寸还是"已同步"）。
    if (!force && m.cols === lastCols && m.rows === lastRows) return;
    if (!deps.connected()) return;
    try {
      deps.send(resizeFrame(m.cols, m.rows));
    } catch {
      return; // 不更新 last*：下一次检查会重试
    }
    lastCols = m.cols;
    lastRows = m.rows;
  }

  return {
    attach() {
      if (disposed || attached) return;
      attached = true;
      disposables.push(
        deps.term.onData((s) => {
          if (disposed) return;
          if (!deps.connected()) {
            // 断线期间的按键**不排队**：那是把用户几分钟前敲的字符塞进
            // 现在的 shell（半途重连的 "rm -rf" 会打在错误的提示符上）。
            dropped += 1;
            deps.onDropped?.(dropped);
            return;
          }
          try {
            deps.send(keysFrame(encoder.encode(s)));
          } catch {
            // isOpen() 到 send 之间连接刚好断掉是常态（拔网线、切后台）：
            // 这里抛出去会变成 xterm 事件回调里的未捕获异常，
            // 把整个标签页的渲染打断。按键计数已经说明发生了什么。
            dropped += 1;
            deps.onDropped?.(dropped);
          }
        }),
      );
      disposables.push(deps.term.onResize(() => reportSize()));
      reportSize(true);
    },

    onPayload(p) {
      if (disposed) return;
      deps.term.write(p);
    },

    onOpen() {
      // 只重报尺寸：面板与 tmux 的尺寸可能已经漂开，而漏掉的按键必须丢。
      reportSize(true);
    },

    checkSize() {
      reportSize();
    },

    dispose() {
      if (disposed) return; // 重复 dispose 会二次释放 xterm，真终端那里会抛
      disposed = true;
      for (const d of disposables) d.dispose();
      disposables.length = 0;
      deps.term.dispose();
    },
  };
}
