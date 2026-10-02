import { describe, it, expect } from 'vitest';
import { mount } from '@vue/test-utils';
import ContextMenu from '../components/files/ContextMenu.vue';
import type { MenuItem } from '../components/files/ContextMenu.vue';
import { hasIcon } from '../components/iconPaths';

// 右键菜单被两个入口共用（设计 8.5：PC 右键 / 手机长按）。测试盯的是菜单
// 本身最容易被悄悄改坏的两件事：**该给的项有没有少**、**点下去触发的是不
// 是那一项**。这两条一旦退化，页面上只表现为"菜单里少了个功能"，而没人会
// 去数一个菜单该有几行。

function items(): MenuItem[] {
  return [
    { key: 'open', label: '打开', icon: 'folder' },
    { key: 'download', label: '下载', icon: 'download', sep: true },
    { key: 'cut', label: '剪切', icon: 'cut' },
    { key: 'copy', label: '复制', icon: 'copy' },
    { key: 'paste', label: '粘贴', icon: 'paste', disabled: true },
    { key: 'rename', label: '重命名', icon: 'edit' },
    { key: 'delete', label: '删除', icon: 'trash', danger: true },
  ];
}

function mk(over: Record<string, unknown> = {}) {
  return mount(ContextMenu, {
    props: { items: items(), x: 120, y: 80, ...over },
  });
}

function row(w: ReturnType<typeof mk>, label: string) {
  const r = w.findAll('.mi').find((x) => x.text().includes(label));
  if (!r) throw new Error(`菜单里没有「${label}」`);
  return r;
}

describe('ContextMenu', () => {
  it('每项都渲染出来', () => {
    expect(mk().findAll('.mi')).toHaveLength(7);
  });

  it('点击某一项只发出那一项的 key', async () => {
    const w = mk();
    await row(w, '重命名').trigger('click');
    expect(w.emitted('pick')?.[0]).toEqual(['rename']);
  });

  // 禁用项既不触发事件，视觉上也得是"不能点"。粘贴在无剪贴板时是灰的；
  // 若还能点，界面就是在骗用户"可以粘贴"。
  it('禁用项不触发 pick 且带 dis 类', async () => {
    const w = mk();
    await row(w, '粘贴').trigger('click');
    expect(w.emitted('pick')).toBeUndefined();
    expect(row(w, '粘贴').classes()).toContain('dis');
  });

  it('点任意一项后请求关闭', async () => {
    const w = mk();
    await row(w, '下载').trigger('click');
    expect(w.emitted('close')).toHaveLength(1);
  });

  it('菜单定位在传入坐标', () => {
    const style = mk({ x: 300, y: 200 }).find('.menu').attributes('style') ?? '';
    expect(style).toContain('left: 300px');
    expect(style).toContain('top: 200px');
  });

  // 靠近屏幕右下角时菜单会被挤出视口 —— 而那里恰恰是文件列表最常用的一片。
  it('超出视口时把菜单拉回来', () => {
    Object.defineProperty(window, 'innerWidth', { value: 400, configurable: true });
    Object.defineProperty(window, 'innerHeight', { value: 300, configurable: true });
    const style = mk({ x: 390, y: 290 }).find('.menu').attributes('style') ?? '';
    const left = Number(/left:\s*(\d+)px/.exec(style)?.[1]);
    const top = Number(/top:\s*(\d+)px/.exec(style)?.[1]);
    expect(left).toBeLessThan(390);
    expect(top).toBeLessThan(290);
  });

  // 长按与右键共用这一个组件；两者都会带出浏览器原生菜单/文本选中，
  // 于是我们的菜单叠在系统菜单下面，用户点了半天点在系统菜单上。
  it('菜单容器拦截 contextmenu 与 selectstart 的默认行为', () => {
    const el = mk().find('.menu').element;
    const ev = (type: string) => {
      const e = new Event(type, { bubbles: true, cancelable: true });
      el.dispatchEvent(e);
      return e.defaultPrevented;
    };
    expect(ev('contextmenu')).toBe(true);
    expect(ev('selectstart')).toBe(true);
  });

  it('点击遮罩关闭菜单', async () => {
    const w = mk();
    await w.find('.scrim').trigger('click');
    expect(w.emitted('close')).toHaveLength(1);
  });

  // 组件用到的图标名必须都在 PATHS 里：AppIcon 对未知名字是
  // PATHS[name] ?? []，会静默画一个空 SVG —— 图标消失这种退化在页面上几乎
  // 看不出来，只能靠测试钉住。名字从上面 items() 派生，不另抄一份清单，
  // 否则改了组件用的图标而测试名单不变，测的是那份手抄清单本身。
  it('用到的图标名都有图形', () => {
    for (const it of items()) {
      if (it.icon) expect(hasIcon(it.icon), `缺少图标 ${it.icon}`).toBe(true);
    }
  });

  it('没有可显示项时菜单自己不出现（长按空白处不该弹一个空框）', () => {
    const w = mount(ContextMenu, { props: { items: [], x: 10, y: 10 } });
    expect(w.find('.menu').exists()).toBe(false);
  });
});
