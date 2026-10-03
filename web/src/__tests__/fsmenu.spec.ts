import { describe, it, expect } from 'vitest';
import { menuItemsFor } from '../components/files/fsMenu';
import type { MenuContext } from '../components/files/fsMenu';
import type { MenuItem } from '../components/files/ContextMenu.vue';
import { hasIcon } from '../components/iconPaths';

// 菜单项随上下文变化，是文件页最容易"悄悄少一行"的地方。测试盯的是
// 设计 8.5 点名的那几条：目录下多出"打开"、有剪贴板时"粘贴"可用、
// 剪切/复制/粘贴都在、删除始终在最后且是危险项。

const file = { name: 'a.txt', is_dir: false };
const dir = { name: 'sub', is_dir: true };

const base = { selectedCount: 1, hasClip: false, multi: false, canOpenTerminal: true };

function ctx(over: Partial<MenuContext>): MenuContext {
  return { ...base, entry: file, ...over };
}

function keys(items: MenuItem[]): string[] {
  return items.map((i) => i.key);
}

function find(items: MenuItem[], key: string): MenuItem | undefined {
  return items.find((i) => i.key === key);
}

describe('menuItemsFor', () => {
  it('文件：有下载、有重命名、没有打开', () => {
    const it = menuItemsFor(ctx({ entry: file }));
    expect(keys(it)).toContain('download');
    expect(keys(it)).toContain('rename');
    expect(keys(it)).not.toContain('open');
  });

  it('目录：有打开、没有下载', () => {
    const it = menuItemsFor(ctx({ entry: dir }));
    expect(keys(it)).toContain('open');
    expect(keys(it)).not.toContain('download');
  });

  // "粘贴"必须始终存在：整个隐藏会让用户以为面板不支持粘贴。
  it('剪贴板有内容时粘贴可用，没有时禁用而不是藏起来', () => {
    const on = menuItemsFor(ctx({ hasClip: true }));
    const off = menuItemsFor(ctx({ hasClip: false }));
    expect(find(on, 'paste')?.disabled).toBeFalsy();
    expect(find(off, 'paste')).toBeTruthy();
    expect(find(off, 'paste')?.disabled).toBe(true);
  });

  // 多选时不能重命名：一次改多个名字不是重命名，那属于另一个功能,
  // 绝不该借"重命名"这个入口发生。
  it('多选时没有重命名', () => {
    const it = menuItemsFor(ctx({ multi: true, selectedCount: 3 }));
    expect(keys(it)).not.toContain('rename');
  });

  // 删除文案带数量：删一个和删五百个的心理门槛不同，把数字写进菜单
  // 比到确认框里才说更负责。
  it('多选删除带数量', () => {
    const it = menuItemsFor(ctx({ multi: true, selectedCount: 42 }));
    expect(find(it, 'delete')?.label).toContain('42');
  });

  it('单选删除不带数量', () => {
    const it = menuItemsFor(ctx({ selectedCount: 1 }));
    expect(find(it, 'delete')?.label).toBe('删除');
  });

  it('删除始终是危险项且排在最后', () => {
    const it = menuItemsFor(ctx({}));
    const del = it[it.length - 1];
    expect(del.key).toBe('delete');
    expect(del.danger).toBe(true);
  });

  // 空白处右键没有明确目标：删除/重命名/下载都必须缺席，否则一次误点
  // 就是把整个目录送进回收站。属性也不给 —— 它过去显示的是**当前目录
  // 自己的路径**，那一行既不能复制也不能说明任何事，占个菜单项纯粹骗人
  // 点进去；路径在地址栏和面包屑里都有，"复制路径"也在同一菜单里。
  it('空白处且没选中时只给新建/粘贴/刷新/终端', () => {
    const it = menuItemsFor(ctx({ entry: null, selectedCount: 0 }));
    expect(keys(it)).toEqual(['mkdir', 'paste', 'refresh', 'terminal']);
  });

  // 空白处但已选中若干项时，属性变成有用的东西：给出这批的合计大小。
  it('空白处但选中了项时给属性', () => {
    expect(keys(menuItemsFor(ctx({ entry: null, selectedCount: 2 })))).toContain('props');
  });

  it('关掉终端能力时不出现"在终端中打开"', () => {
    expect(keys(menuItemsFor(ctx({ canOpenTerminal: false })))).not.toContain('terminal');
    expect(keys(menuItemsFor(ctx({ entry: null, canOpenTerminal: false })))).not.toContain('terminal');
  });

  it('文件上没有"在终端中打开"（那是目录才有的动作）', () => {
    expect(keys(menuItemsFor(ctx({ entry: file })))).not.toContain('terminal');
  });

  // 图标名全部要有图形：AppIcon 对未知名字静默画空 SVG，菜单里就是一行
  // 没有图标的文字，很难以察觉。名字从实际产出的菜单项派生，不另抄清单。
  it('所有可能出现的图标都有图形', () => {
    const seen = new Set<string>();
    for (const c of [
      ctx({ entry: file, hasClip: true }),
      ctx({ entry: dir, multi: true, selectedCount: 2 }),
      ctx({ entry: null, selectedCount: 0 }),
    ]) {
      for (const m of menuItemsFor(c)) if (m.icon) seen.add(m.icon);
    }
    // 防止"菜单整个返回空"也算通过：
    expect(seen.size).toBeGreaterThan(4);
    for (const n of seen) expect(hasIcon(n), `缺少图标 ${n}`).toBe(true);
  });
});
