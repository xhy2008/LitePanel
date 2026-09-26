import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';

import { useBusyWatch } from '../composables/useBusyWatch';

// 轮询这件事唯一要防的失败是"停不下来"和"从不开始"：
// 页面都关了还在打接口，或者挂上了却一次都不查。所以断言一律看
// fetch 的实际调用次数，不看代码自己的计数。
function store() {
  return { refreshBusy: vi.fn().mockResolvedValue(undefined), busyCalls: 0 };
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
});
afterEach(() => {
  vi.useRealTimers();
});

describe('useBusyWatch（终端页的忙闲轮询）', () => {
  it('挂载立刻查一次，之后按间隔继续查', async () => {
    const s = store();
    const w = useBusyWatch(s as never, 2000);
    w.start();
    expect(s.refreshBusy).toHaveBeenCalledTimes(1);
    // 每轮之间让微任务落地：上一轮没结掉就不发下一轮（防叠），而真实
    // 网络里 3 秒足够一次请求落地。
    await tick(w, 2000);
    expect(s.refreshBusy).toHaveBeenCalledTimes(2);
    await tick(w, 4000);
    expect(s.refreshBusy).toHaveBeenCalledTimes(4);
    w.stop();
  });

  // 离开终端页还在轮询，就是在给一个看不见的页面打接口 —— 而这正是
  // "标签上的忙一直不变"的反向故障：用户回到快捷命令页，面板却在背后
  // 持续刷新，看不出问题也停不下来。
  it('stop 之后不再有任何请求', async () => {
    const s = store();
    const w = useBusyWatch(s as never, 2000);
    w.start();
    w.stop();
    await vi.advanceTimersByTimeAsync(10000);
    expect(s.refreshBusy).toHaveBeenCalledTimes(1);
  });

  // 重复 start 不该叠出两条轮询链：两个定时器各自跑，请求量翻倍，
  // 而切标签页/回到页面上就会 start 第二次。
  it('start 两次只有一条轮询链', async () => {
    const s = store();
    const w = useBusyWatch(s as never, 2000);
    w.start();
    w.start();
    await tick(w, 2000);
    expect(s.refreshBusy).toHaveBeenCalledTimes(2);
    w.stop();
  });

  // 防叠：上一轮还没回来时不发下一轮。慢网络下不挡住的话，tmux 会被
  // 同一件事连续轰击，而且请求回来的顺序跟发出顺序不一致时角标会闪。
  it('上一轮没回来时不叠加请求', async () => {
    // 类型标注写成可空函数就行，但 TS 会因为在回调里赋值而把它收窄成
    // null（"这里一定是 null"），于是 release?.() 报"never 不可调用"。
    // 用对象装一层就绕开了收窄，语义不变。
    const held: { release: (() => void) | null } = { release: null };
    const s = {
      refreshBusy: vi.fn(
        () =>
          new Promise<void>((res) => {
            held.release = res;
          }),
      ),
    };
    const w = useBusyWatch(s as never, 2000);
    w.start();
    vi.advanceTimersByTime(6000); // 三轮的点，但一次都没返回
    expect(s.refreshBusy).toHaveBeenCalledTimes(1);
    held.release?.();
    await vi.advanceTimersByTimeAsync(2000);
    expect(s.refreshBusy).toHaveBeenCalledTimes(2);
    w.stop();
  });

  // 一次失败（网络抖一下）不许把轮询链掐断：掐断之后角标就永久停在
  // 最后一次成功的那个结论上，而那正是最开始要修的 bug。
  // 节拍必须是固定网格，不能跟着网络延迟漂：一次慢查询之后，若"响应
  // 回来才排下一轮"，节奏就永久性地被那次延迟拖开，角标要好几个间隔
  // 才更新一次 —— 用户看到的还是"卡在忙"，只不过从永久变成了很久。
  //
  // 断言只看"到某个时刻一共查了几次"，不读时钟：拿 Date.now() 打点要看
  // 假计时器的脸色（实测 vi.useFakeTimers 里 Date.now() 挪动的比例跟
  // setTimeout 并不一致），而次数是独立可数的。
  //
  // 让请求自己延迟 3 秒返回（间隔 2 秒），两种写法的次数就分开了：
  //   固定网格：t=0 查(→3000)、2000 被挡、t=4000 查(→7000)、6000 被挡、
  //            t=8000 查 → 共 3 次
  //   响应后再排：t=0 查(→3000) → 3000 才排 → t=5000 查(→8000) → 8000 前
  //            不会再排 → 共 2 次
  // 观测窗口取 8000 而不是 6000：6000 之内两种写法都是 2 次，分开不了。
  // （第一版我按 6000 写、断言 3 次，红了 —— 红的是我自己的排期推算，
  // 不是实现。这类"我以为节拍是这样"的错，只有把每一步列出来才会现形。）
  it('慢请求把节奏拖开算失败：节拍是固定网格', async () => {
    const s = {
      refreshBusy: vi.fn(
        () =>
          new Promise<void>((res) => {
            setTimeout(res, 3000); // 比间隔还慢
          }),
      ),
    };
    const w = useBusyWatch(s as never, 2000);
    w.start();
    await vi.advanceTimersByTimeAsync(8000);
    expect(s.refreshBusy).toHaveBeenCalledTimes(3);
    w.stop();
  });

  it('某次查询失败后仍然继续轮询', async () => {
    const s = { refreshBusy: vi.fn() };
    s.refreshBusy.mockRejectedValueOnce(new Error('boom'));
    const w = useBusyWatch(s as never, 2000);
    w.start();
    await flush();
    expect(s.refreshBusy).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(2000);
    await flush();
    expect(s.refreshBusy).toHaveBeenCalledTimes(2);
    w.stop();
  });
});

async function flush() {
  for (let i = 0; i < 4; i++) await Promise.resolve();
}

// advanceTimersByTimeAsync 而不是 advanceTimersByTime + 手动 flush：
// 后者会在所有计时器跑完之后才让微任务落地，于是"上一轮已结掉"这件事
// 永远晚一拍，每一轮都被防叠逻辑挡掉、节奏凭空变成两倍 —— 红的是测试
// 的排期假设，不是实现的节奏。
async function tick(w: { stop(): void }, ms: number) {
  await vi.advanceTimersByTimeAsync(ms);
}
