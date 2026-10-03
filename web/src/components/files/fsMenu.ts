// 右键菜单项的生成（设计 8.5）。
//
// 为什么是纯函数而不是写在组件模板里：菜单"该出现哪些项"是这套界面里
// 规则最密的一条（目录/文件/多选/有无剪贴板 四种条件交叉），而它的错误
// 在页面上只表现为"菜单里少了某一行"，没人会去数。做成纯函数才能把这些
// 组合一条条钉死。
//
// 另一个原因是 PC 右键与手机长按要给出**完全一样**的菜单：两处各自
// 拼一份列表，迟早漂移成"手机上少几个功能"。

import type { MenuItem } from './ContextMenu.vue';

export interface MenuContext {
  /** 目标条目；null 表示点在空白处（= 目录空白右键）。 */
  entry: { name: string; is_dir: boolean } | null;
  /** 当前选中项数量（含被右键的那一项 —— 右键已选中项时不缩小选择）。 */
  selectedCount: number;
  /** 剪贴板里有东西（决定"粘贴"是否可用）。 */
  hasClip: boolean;
  /** 多选（>1）时不给"重命名"：一次改多个名字不是重命名。 */
  multi: boolean;
  /** 用户在"终端"页能用的能力；关掉时不显示"在终端中打开"。 */
  canOpenTerminal?: boolean;
}

/**
 * 生成菜单项。顺序即显示顺序，sep 标记分组边界。
 *
 * 几个不那么显然的取舍：
 *
 * · 空白处右键（entry 为 null）只给"新建 / 粘贴 / 刷新 / 属性"，不给
 *   删除、重命名、下载 —— 那些需要一个明确的目标，灰着摆在那儿比不摆
 *   更容易被误点（用户以为"粘贴到别处失败了"，实际是删掉了整个目录）。
 *
 * · "粘贴"永远出现，只是无剪贴板时 disabled。整个藏起来会让用户以为
 *   面板不支持粘贴（设计里"粘贴有剪贴板内容时可用"就是这个意思）。
 *
 * · "删除"在多选时文案是"删除 N 项"：删一个和删五百个的心理门槛完全不同,
 *   把数量写进菜单比弹确认框时才说更负责。
 */
export function menuItemsFor(c: MenuContext): MenuItem[] {
  const items: MenuItem[] = [];

  if (c.entry === null) {
    items.push({ key: 'mkdir', label: '新建文件夹', icon: 'add' });
    items.push({ key: 'paste', label: '粘贴', icon: 'paste', disabled: !c.hasClip, sep: true });
    items.push({ key: 'refresh', label: '刷新', icon: 'refresh' });
    items.push({ key: 'props', label: '属性', icon: 'properties' });
    if (c.canOpenTerminal) {
      items.push({ key: 'terminal', label: '在终端中打开', icon: 'terminal_run' });
    }
    return items;
  }

  if (c.entry.is_dir) {
    items.push({ key: 'open', label: '打开', icon: 'folder' });
  }
  if (!c.multi) {
    items.push({ key: 'rename', label: '重命名', icon: 'edit' });
  }
  if (!c.entry.is_dir) {
    items.push({ key: 'download', label: '下载', icon: 'download' });
  }
  items.push({ key: 'cut', label: '剪切', icon: 'cut', sep: true });
  items.push({ key: 'copy', label: '复制', icon: 'copy' });
  items.push({ key: 'paste', label: '粘贴', icon: 'paste', disabled: !c.hasClip });
  if (c.canOpenTerminal && c.entry.is_dir) {
    items.push({ key: 'terminal', label: '在终端中打开', icon: 'terminal_run' });
  }
  items.push({ key: 'cpath', label: '复制路径', icon: 'copy', sep: true });
  items.push({ key: 'props', label: '属性', icon: 'properties' });
  items.push({
    key: 'delete',
    label: c.multi ? `删除 ${c.selectedCount} 项` : '删除',
    icon: 'trash',
    danger: true,
  });
  return items;
}
