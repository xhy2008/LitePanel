// 五个一级标签：路由 name、中文名与图标名（AppIcon 的 key）。
export interface TabDef {
  name: string;
  label: string;
  icon: string;
}

export const TABS: TabDef[] = [
  { name: 'quick', label: '快捷命令', icon: 'bolt' },
  { name: 'term', label: '终端', icon: 'terminal' },
  { name: 'files', label: '文件管理', icon: 'folder' },
  { name: 'downloads', label: '下载', icon: 'download' },
  { name: 'settings', label: '设置', icon: 'settings' },
];

export function tabOf(name: string): TabDef {
  return TABS.find((t) => t.name === name) ?? TABS[0];
}
