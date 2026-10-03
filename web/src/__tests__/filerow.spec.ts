import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount } from '@vue/test-utils';
import FileRow from '../components/files/FileRow.vue';
import { iconFor } from '../api/files';
import { hasIcon } from '../components/iconPaths';
import type { FsEntry } from '../api/files';

// 行组件里唯一值得较真的是手机端的手势：长按弹菜单、滚动时取消长按、
// 勾选与"打开"是两个互不干扰的目标。这几条错了不会报错，只会让手机上
// "想多选却打开了文件"或"一滚动就满屏弹菜单"。

function entry(over: Partial<FsEntry> = {}): FsEntry {
  return {
    name: 'a.txt', is_dir: false, is_symlink: false, size: 2048,
    mtime: Math.floor(Date.now() / 1000), mime: 'text/plain', mode: '-rw-r--r--', ...over,
  };
}

function mk(over: Record<string, unknown> = {}) {
  return mount(FileRow, { props: { entry: entry(), selected: false, ...over } });
}

// 长按是定时器驱动的，用假时钟才能既快又确定地跨过 500ms 阈值。
beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

// happy-dom 不带 TouchEvent 构造器，造一个只带 touches 的普通事件。
// 必须带 touches：组件里靠 e.touches.length 区分单指长按与双指缩放。
function touch(el: Element, type: string, y: number) {
  const ev: any = new Event(type, { bubbles: true });
  ev.touches = [{ clientX: 40, clientY: y }];
  el.dispatchEvent(ev);
}

describe('FileRow', () => {
  it('单击行本体发出 open', async () => {
    const w = mk();
    await w.find('.row').trigger('click');
    expect(w.emitted('open')?.[0]?.[0]).toMatchObject({ name: 'a.txt' });
  });

  // 圆圈必须能独立勾选：如果它把 click 冒泡给行，点圆圈会同时触发 open,
  // 手机端就变成了"想勾选却打开了文件"。
  it('点圆圈只发 toggle，不触发 open', async () => {
    const w = mk();
    await w.find('.chk').trigger('click');
    expect(w.emitted('toggle')).toHaveLength(1);
    expect(w.emitted('open')).toBeUndefined();
  });

  it('选中态：圆圈 on 且行有 sel', () => {
    const w = mk({ selected: true });
    expect(w.find('.chk').classes()).toContain('on');
    expect(w.find('.row').classes()).toContain('sel');
  });

  it('剪切态整行压暗（cut 类）', () => {
    expect(mk({ cut: true }).find('.row').classes()).toContain('cut');
  });

  it('目录不显示大小（大小对目录无意义）', () => {
    const w = mk({ entry: entry({ is_dir: true, size: 4096 }) });
    expect(w.find('.sz').text()).toBe('');
  });

  it('文件显示人类可读大小', () => {
    expect(mk().find('.sz').text()).toBe('2.0 KB');
  });

  it('符号链接带箭头标记', () => {
    expect(mk({ entry: entry({ is_symlink: true }) }).find('.lnk').exists()).toBe(true);
  });

  it('PC 右键发出 menu 并阻止原生菜单', () => {
    const w = mk();
    const ev: any = new Event('contextmenu', { bubbles: true, cancelable: true });
    ev.clientX = 55;
    ev.clientY = 66;
    w.find('.row').element.dispatchEvent(ev);
    // 不拦住原生菜单，我们的菜单会叠在它下面。
    expect(ev.defaultPrevented).toBe(true);
    expect(w.emitted('menu')?.[0]).toEqual([
      expect.objectContaining({ name: 'a.txt' }),
      55,
      66,
    ]);
  });

  it('长按 500ms 发出 menu', () => {
    const w = mk();
    touch(w.find('.row').element, 'touchstart', 100);
    vi.advanceTimersByTime(500);
    expect(w.emitted('menu')).toHaveLength(1);
  });

  it('短按（不足阈值）不弹菜单', () => {
    const w = mk();
    touch(w.find('.row').element, 'touchstart', 100);
    vi.advanceTimersByTime(300);
    touch(w.find('.row').element, 'touchend', 100);
    vi.advanceTimersByTime(500);
    expect(w.emitted('menu')).toBeUndefined();
  });

  // 滚动手势必须取消长按：否则滑动列表时每滑过一行都弹一个菜单。
  it('手指移动超过阈值则取消长按', () => {
    const w = mk();
    const el = w.find('.row').element;
    touch(el, 'touchstart', 100);
    touch(el, 'touchmove', 140); // 移动 40px
    vi.advanceTimersByTime(500);
    expect(w.emitted('menu')).toBeUndefined();
  });

  // 长按弹菜单后手指抬起会附带一个 click；不能再触发 open,
  // 否则"想弹菜单"变成"打开了文件"。
  it('长按后的 click 不再触发 open', () => {
    const w = mk();
    touch(w.find('.row').element, 'touchstart', 100);
    vi.advanceTimersByTime(500);
    expect(w.emitted('menu')).toHaveLength(1);
    w.find('.row').trigger('click');
    expect(w.emitted('open')).toBeUndefined();
  });

  // 图标名必须有图形：AppIcon 对未知名字是 PATHS[name] ?? []，会静默画
  // 一个空 SVG。名字从各个真实入口派生（目录/各类型文件/勾选态），
  // 而不是手抄一份清单 —— 手抄的清单只保护“测试自己写的那几个字”。
  it('各个渲染路径用到的图标都有图形', () => {
    const names = new Set<string>(['check']); // 勾选圆圈用的固定图标
    for (const e of [
      entry(),
      entry({ is_dir: true }),
      entry({ name: 'a.png', mime: 'image/png' }),
      entry({ name: 'a.zip' }),
      entry({ name: 'a.sh' }),
    ]) {
      names.add(iconFor(e));
    }
    for (const n of names) expect(hasIcon(n), `缺图标 ${n}`).toBe(true);
  });
});
