// 与后端 internal/api/handlers_settings.go 的 settingItem / settingGroup 一一对应。
// 后端是 snake_case（Go struct tag），这里保持同名：自己改名就会与 HTTP 返回的字段
// 悄悄漂移（本项目反复栽过的坑）。

export type SettingKind = 'int' | 'string' | 'enum';

export interface SettingItem {
  key: string;
  group: string;
  kind: SettingKind;
  label: string;
  unit?: string;
  // int 项的闭区间、enum 项的允许值。控件的范围/选项**只能**来自这里：
  // 前端自己抄一份就会与后端漂移，渲染出"允许填而后端拒收"的输入框。
  min?: number;
  max?: number;
  enum?: number[];
  required?: boolean;

  // value 的类型随 kind 变：int/enum 是 number，string 是 string。
  // 后端把库里的坏值原样吐回并置 valid=false（不夹成 0），所以这里允许
  // 是 number | string —— 一个坏的数字项会是字符串形式的 "o3"。
  value: number | string;
  valid: boolean;

  set: boolean;
  overridden: boolean;

  // 敏感项：value 恒为空串，has_value 是它唯一能暴露的事实。输入框要显示
  // "已设置（留空则不修改）"而不能渲染成"没设过"。
  secret?: boolean;
  has_value?: boolean;
  // 改了必须重启才生效：界面据此打标记，否则"已保存"是骗人的。
  restart_required?: boolean;
}

export interface SettingGroup {
  group: string;
  items: SettingItem[];
}

// GET /api/settings。
export interface SettingsResponse {
  groups: SettingGroup[];
}

// PUT /api/settings 的响应：applied 为假时 reason 说明哪一项没能立即生效
// （例如 aria2 拒绝了某个全局选项）。保存后直接回全量 groups，前端整树替换，
// 不必自己 diff。
export interface SettingsSaveResponse {
  ok: boolean;
  applied: boolean;
  reason?: string;
  groups: SettingGroup[];
}

// 分组的中文标题。只有这一处，视图不许自己拼：两处写法一漂移，导航与
// 区块标题就对不上。后端 group 是稳定枚举，新分组在此缺席时视图回落显示
// 原始 group 名（宁可丑，也不要因为漏翻译就整块消失）。
export const GROUP_LABELS: Record<string, string> = {
  dashboard: '仪表盘',
  auth: '登录与安全',
  files: '文件管理',
  services: '托管服务',
  terminal: '终端',
  download: '下载',
};

// 分组在页面上的先后（锚点导航顺序）。未知分组排最后（视图按此排序，
// 与后端返回顺序解耦，两边不会因顺序假设不同而错位）。
export const GROUP_ORDER = ['dashboard', 'auth', 'files', 'services', 'terminal', 'download'];
