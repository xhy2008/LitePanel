// 终端实例的保留/回收策略。
//
// 从 TerminalView 里抽出来，是因为"同时保留几个实例"是这条路径上唯一
// 会真正漏资源的地方，而它在组件里测不动（真 xterm 的 open() 在
// happy-dom 里需要 canvas 度量）。组件那边只负责把 DOM 和 WS 接进来。

export interface Runtime {
  /** 属于哪个会话：视图与测试都靠它辨认实例身份。 */
  readonly id: number;
  dispose(): void;
}

export interface RuntimeDeps<R extends Runtime> {
  /** 建立某个会话的实例（里面 new Terminal + subscribeTerm）。 */
  make(id: number): R;
}

export function useTermRuntimes<R extends Runtime>(deps: RuntimeDeps<R>) {
  // 同时只保留一个：每个实例都挂着 xterm 屏幕缓冲、一条 WS 订阅和一个
  // 尺寸轮询。留着"看过的全部"，切过五个标签就有五份输出往浏览器灌，
  // 而用户只看得到一个。
  let current: R | null = null;

  function drop() {
    if (!current) return;
    const old = current;
    current = null; // 先摘再 dispose：抛错也不会留下一个"已死但被持有"的实例
    old.dispose();
  }

  return {
    focus(id: number) {
      if (!id) {
        drop();
        return;
      }
      // 重复 focus 同一会话不能重建：那会把正在看的屏幕连缓冲一起扔掉，
      // 闪一下黑屏再重放历史。
      if (current?.id === id) return;
      drop();
      current = deps.make(id);
    },

    /** 会话被删除：回收它的实例，必要时补建新的当前会话。 */
    remove(id: number, nextActive: number) {
      if (current?.id === id) {
        drop();
        if (nextActive) current = deps.make(nextActive);
        return;
      }
      // 删的是别的会话：不动现有实例（重建它等于把用户正在看的屏幕扔掉）。
      // 唯一要补的情况是"当前根本没实例"—— 例如会话全死了之后又删掉
      // 一个死会话，此时 activeId 指向的那个还是一片黑。
    },

    /** 会话在 tmux 侧退出：留着实例，用户会以为程序还在跑。 */
    markDead(id: number) {
      if (current?.id === id) drop();
    },

    get(id: number): R | undefined {
      return current?.id === id ? current : undefined;
    },

    live(): number[] {
      return current ? [current.id] : [];
    },

    disposeAll() {
      drop();
    },
  };
}
