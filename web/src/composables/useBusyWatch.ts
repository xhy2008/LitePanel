// 空闲会话要能被下一次的快捷命令复用，前提是面板知道它空闲了。忙闲结论
// 是后端算的，而"命令结束"这个事件不会推给任何人 —— 所以只能轮询。
// 这里只负责"按时问"，一次问什么由 store 决定。

export interface BusyPoller {
  refreshBusy(): Promise<unknown>;
}

// 忙闲轮询间隔。3 秒是拿"点命令到看出变化"的最坏延迟换 tmux 子进程数：
// 每轮一次 list-panes，会话数量级是个位数到几十，3 秒一次没压力。
// 再短就开始为了一个角标跟 tmux 抢 CPU，再长用户就会当成"卡住了"。
export const BUSY_POLL_MS = 3000;

export function useBusyWatch(store: BusyPoller, ms = BUSY_POLL_MS) {
  let timer: ReturnType<typeof setTimeout> | null = null;
  // 在飞的请求数。用它来防叠：慢网络下上一轮还没回来就发下一轮，
  // tmux 会被同一件事连续轰击。计数而不是布尔值，是因为 start() 会
  // 立刻发一次、而定时器那一次可能也在飞。
  let inFlight = 0;

  function arm() {
    timer = setTimeout(tick, ms);
  }

  function tick() {
    timer = null;
    // 先重排，再发请求：节拍是固定的网格，跟某一次请求多久回来无关。
    // 反过来（响应落地后才排下一轮）会让节奏跟着网络延迟漂 —— 一次 8 秒
    // 的慢查询之后，角标要 8+间隔 秒才更新一次，而这正是"看着卡在忙"。
    // 挡在中间的只有 inFlight。
    arm();
    if (inFlight > 0) return; // 上一轮还没回来，这一轮跳过
    inFlight++;
    // 直接调、不要先挂一个 Promise.resolve().then()：绕那一圈会把发起
    // 推迟到微任务，"start 之后立刻查了一次"就再也保证不上了。
    // 一次失败（网络抖一下就够）不能把轮询链掐断，吞掉错误让下一轮继续。
    try {
      Promise.resolve(store.refreshBusy())
        .catch(() => undefined)
        .then(() => {
          inFlight--;
        });
    } catch {
      inFlight--; // 同步抛错（store 自己坏了）也算这一轮结束了
    }
    // 吞掉错误这件事由 .catch 负责：一次失败（网络抖一下就够）不能把
    // 轮询链掐断 —— 掐断之后角标就永久停在最后一次成功的结论上。
  }

  return {
    start() {
      // 重复 start 不叠链：切标签页、回到页面上都会再 start 一次，
      // 两条链各自跑就是双倍请求，而且谁都停不掉谁。
      if (timer !== null) {
        clearTimeout(timer);
        timer = null;
      }
      tick();
    },
    stop() {
      // 只有这一件事：把下一轮抹掉。不需要额外的"已停"标志位 ——
      // 表清掉之后 tick 再也不会被调到，而响应落地时它只做 inFlight--，
      // 不会偷偷重排。留着那个标志位就是留一条永远走不到的分支。
      if (timer !== null) {
        clearTimeout(timer);
        timer = null;
      }
    },
  };
}
