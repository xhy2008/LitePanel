import { describe, expect, it, vi } from 'vitest';
import { useTermRuntimes, type Runtime } from '../composables/useTermRuntimes';

// 一个会被记录销毁的假运行时。真 xterm 在 happy-dom 里 open() 需要
// canvas 度量，测不动；这里要测的是**保留/回收的策略**，不是渲染。
//
// make 与 made 必须来自**同一个**工厂：分开各调一次 fakeFactory()，
// made 里记录的是另一个工厂造出来的实例，dispose 断言就只是在读一个
// 没人碰过的对象 —— 永远绿，什么都测不到。
function fakeFactory() {
  const made: Fake[] = [];
  return {
    made,
    make: vi.fn((id: number): Fake => {
      const r: Fake = {
        id,
        disposed: false,
        dispose() {
          this.disposed = true;
        },
      };
      made.push(r);
      return r;
    }),
  };
}

interface Fake extends Runtime {
  id: number;
  disposed: boolean;
}

describe('终端实例的保留与回收', () => {
  it('focus 会建立目标会话的实例', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    rt.focus(1);
    expect(f.make).toHaveBeenCalledWith(1);
    expect(f.made).toHaveLength(1);
  });

  // 切走的会话必须销毁：每个实例都挂着一份 xterm 屏幕缓冲、一条 WS
  // 订阅和一个尺寸轮询。来回切十个标签就攒下十份，而用户只看得到一个。
  it('focus 另一个会话时销毁上一个', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    rt.focus(1);
    rt.focus(2);
    expect(f.made[0].disposed).toBe(true);
    expect(f.made[1].disposed).toBe(false);
    expect(rt.live()).toEqual([2]);
  });

  // 重复 focus 同一个会话不能重建实例：那会把用户正在看的屏幕
  // 连同缓冲一起扔掉，闪一下黑屏再重放历史。
  it('重复 focus 同一会话不重建', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    rt.focus(1);
    rt.focus(1);
    expect(f.make).toHaveBeenCalledTimes(1);
    expect(f.made[0].disposed).toBe(false);
  });

  it('同一时刻最多只有一个实例存活', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    for (const id of [1, 2, 3, 1, 2]) rt.focus(id);
    expect(rt.live()).toEqual([2]);
    expect(f.made.filter((m) => !m.disposed)).toHaveLength(1);
  });

  // 删除当前会话后要把剩下的那个补建出来，否则屏幕停在一片黑上，
  // 用户以为面板挂了。
  it('删除会话时回收它的实例，并补建新的当前会话', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    rt.focus(1);
    rt.remove(1, 2);
    expect(f.made[0].disposed).toBe(true);
    expect(rt.live()).toEqual([2]);
  });

  it('删除的不是当前会话时不重建任何东西', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    rt.focus(1);
    rt.remove(9, 1);
    expect(f.make).toHaveBeenCalledTimes(1);
    expect(rt.live()).toEqual([1]);
  });

  it('离开页面时全部回收', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    rt.focus(1);
    rt.disposeAll();
    expect(f.made[0].disposed).toBe(true);
    expect(rt.live()).toEqual([]);
  });

  // 重复 dispose 会二次调用 xterm 的 dispose：真终端对已释放的对象
  // 再 dispose 会抛，路由切换会被打断。
  it('重复 disposeAll 幂等', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    rt.focus(1);
    rt.disposeAll();
    expect(() => rt.disposeAll()).not.toThrow();
  });

  // 会话在 tmux 侧退出时实例必须被回收 —— 但入口不是专门的 markDead，
  // 而是"activeId 落到下一个活会话"：store.load 摘掉死会话之后视图就会
  // focus(新 id)，旧实例当拍 dispose。这里断言的就是那条路。
  it('切到下一个会话时回收上一个实例（退出会话的回收路径）', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    rt.focus(1);
    rt.focus(2);
    expect(f.made[0].disposed).toBe(true);
    expect(rt.live()).toEqual([2]);
  });

  it('focus(0) 回收当前实例（会话全没了的情况）', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    rt.focus(1);
    rt.focus(0);
    expect(f.made[0].disposed).toBe(true);
    expect(rt.live()).toEqual([]);
  });

  it('get 只在实例存在时返回它', () => {
    const f = fakeFactory();
    const rt = useTermRuntimes({ make: f.make });
    expect(rt.get(1)).toBeUndefined();
    rt.focus(1);
    expect(rt.get(1)?.id).toBe(1);
  });
});
