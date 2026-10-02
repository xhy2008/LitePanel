// 图标路径表。单独成模块是为了让它可测：AppIcon 对未知名字是
// `PATHS[name] ?? []`，会静默画一个空 SVG —— 图标消失这种退化在页面上
// 几乎看不出来，只能靠测试把用到的名字钉住
// （名单见 __tests__/servicestile.spec.ts）。
export const PATHS: Record<string, string[]> = {
  bolt: ['M13 2 5 13h5l-1 9 8-11h-5z'],
  terminal: [
    'M3 5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z',
    'M7 9l3 3-3 3',
    'M12 15h5',
  ],
  folder: ['M3 6a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z'],
  download: ['M12 3v11', 'M8 10l4 4 4-4', 'M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2'],
  settings: [
    'M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6z',
    'M12 2v3',
    'M12 19v3',
    'M2 12h3',
    'M19 12h3',
    'M5 5l2 2',
    'M17 17l2 2',
    'M19 5l-2 2',
    'M7 17l-2 2',
  ],

  // ---- 服务管理 ----
  robot: [
    'M7 8h10a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2v-7a2 2 0 0 1 2-2z',
    'M12 4v4',
    'M9.5 12v2',
    'M14.5 12v2',
  ],
  check: ['M4 13l5 5L20 7'],
  alert: ['M12 4 2.5 20h19z', 'M12 10v4', 'M12 17h.01'],
  stop: ['M7 7h10v10H7z'],
  receipt: ['M6 3h12v18l-3-2-3 2-3-2-3 2z', 'M9 8h6', 'M9 12h6'],
  add: ['M12 5v14', 'M5 12h14'],
  info: ['M12 8h.01', 'M11 12h1v4h1', 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z'],
  memory: ['M7 5h10v14H7z', 'M10 9h4v6h-4z', 'M4 9h3', 'M4 15h3', 'M17 9h3', 'M17 15h3'],
  copy: ['M9 9h10v12H9z', 'M15 9V3H5v10h4'],
  trash: ['M4 7h16', 'M9 7V4h6v3', 'M6 7l1 14h10l1-14', 'M10 11v6', 'M14 11v6'],
  close: ['M6 6l12 12', 'M18 6 6 18'],

  // ---- 后台任务 ----
  move: ['M4 7h9a4 4 0 0 1 4 4v3', 'M14 11l3 3 3-3', 'M4 4l-3 3 3 3', 'M4 7v10'],
  retry: ['M20 12a8 8 0 1 1-2.3-5.7', 'M20 4v4h-4'],

  // ---- 快捷命令 ----
  arrow_upward: ['M12 19V5', 'M5 12l7-7 7 7'],
  arrow_downward: ['M12 5v14', 'M19 12l-7 7-7-7'],
  edit: ['M4 20h4L20 8l-4-4L4 16z', 'M14 6l4 4'],
  hourglass_top: ['M6 3h12', 'M6 21h12', 'M7 3c0 5 5 5 5 9s-5 4-5 9', 'M17 3c0 5-5 5-5 9s5 4 5 9'],
};

/** 这个名字有没有对应的图形。 */
export function hasIcon(name: string): boolean {
  return (PATHS[name]?.length ?? 0) > 0;
}
