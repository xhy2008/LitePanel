// 与后端 internal/filemgr 的 Entry/ListPage/Root/TrashItem 一一对应。
// 字段名保持 snake_case 同名（与 services.ts / fsJobs.ts 同一条理由：
// HTTP 与 WS 序列化的是同一个 Go 类型，前端自己改名就会与其中一边漂移）。

export interface FsEntry {
  name: string;
  // 列表条目后端**不填** path（一页 500 条重复同一目录前缀是白扔的
  // 字节）；/fs/stat 才填。所以列表里的完整路径必须自己用
  // joinPath(dir, name) 拼 —— 而不能用 entry.path。
  path?: string;
  is_dir: boolean;
  is_symlink: boolean;
  size: number;
  mtime: number;
  mime: string;
  mode: string;
}

export interface ListPage {
  path: string;
  page: number;
  size: number;
  total: number;
  entries: FsEntry[];
}

export interface FsRoot {
  path: string;
  device: string;
  fstype: string;
  total: number;
  free: number;
  used: number;
  // false = statfs 没成功、容量未知。显示 "—"，不能显示 0/0：
  // "容量取不到"与"盘满了"是两件相反的事。
  measured: boolean;
}

export interface TrashItem {
  id: string;
  name: string;
  origin: string;
  path: string;
  mount: string;
  is_dir: boolean;
  size: number;
  deleted_at: number;
}

export type SortKey = 'name' | 'size' | 'mtime' | 'type';

export interface ListParams {
  path: string;
  page?: number;
  size?: number;
  sort?: SortKey;
  desc?: boolean;
  hidden?: boolean;
}

export function listQuery(p: ListParams): string {
  const q = new URLSearchParams();
  q.set('path', p.path);
  if (p.page) q.set('page', String(p.page));
  if (p.size) q.set('size', String(p.size));
  if (p.sort) q.set('sort', p.sort);
  if (p.desc) q.set('order', 'desc');
  if (p.hidden) q.set('show_hidden', '1');
  return q.toString();
}

/**
 * 拼子项完整路径。
 *
 * 不叫 concat 而单独成函数，是因为根目录这一例必须特判：dir 是 "/" 时
 * 拼出来是 "//a"。f2fs 对 "//" 的解释与 "/" 不同（首个斜杠被折叠的
 * 规则只对前两个成立），列表能显示而后续 stat/download 全 404 —— 一种
 * 只在根目录点文件才复现的裂法最难查。
 */
export function joinPath(dir: string, name: string): string {
  return dir.endsWith('/') ? dir + name : dir + '/' + name;
}

/** 面包屑分段："/data/x" → [{label:'/'}…{label:'data',path:'/data'}…]。 */
export function crumbOf(abs: string): Array<{ label: string; path: string }> {
  const out: Array<{ label: string; path: string }> = [{ label: '/', path: '/' }];
  if (abs === '/' || abs === '') return out;
  let acc = '';
  for (const seg of abs.split('/').filter(Boolean)) {
    acc += '/' + seg;
    out.push({ label: seg, path: acc });
  }
  return out;
}

/** 父目录。根与 '/' 的父是自身（面包屑在根上不再显示"上一级"）。 */
export function parentOf(abs: string): string {
  if (abs === '/' || abs === '') return '/';
  const i = abs.lastIndexOf('/');
  if (i <= 0) return '/';
  return abs.slice(0, i);
}

/** 人类可读体积。0 要显示 "0 B" 而不是空 —— 空文件是真实信息。 */
export function sizeLabel(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return `${n} B`;
  const u = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${u[i]}`;
}

/** 列表用的时间：当年 "月-日 时:分"，跨年补年份。 */
export function mtimeLabel(epochSec: number, now = Date.now()): string {
  if (!epochSec) return '';
  const d = new Date(epochSec * 1000);
  const p = (n: number) => (n < 10 ? `0${n}` : String(n));
  const hm = `${p(d.getHours())}:${p(d.getMinutes())}`;
  const md = `${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  return d.getFullYear() === new Date(now).getFullYear() ? `${md} ${hm}` : `${d.getFullYear()}-${md}`;
}

/**
 * 单文件下载地址（设计 8.3）。
 *
 * 路径用 searchParams 编码而不是手拼：文件名里的 & # = 与空格是真实存在
 * 的（"报告 v2 & 终版.pdf" 这种），手拼会把一个合法路径切成两个参数。
 */
export function downloadUrl(path: string): string {
  return `/api/fs/download?path=${encodeURIComponent(path)}`;
}

/**
 * 多选打包下载地址。后端读的是重复的 path= 参数（r.URL.Query()["path"]）,
 * 一个都不能少：少一个就是"静默少打包了一个文件"，而下载下来的 zip
 * 里看不出少了东西。
 */
export function zipUrl(paths: string[]): string {
  const q = new URLSearchParams();
  for (const p of paths) q.append('path', p);
  return `/api/fs/zip?${q.toString()}`;
}

/** 按 mime / 扩展名挑图标（AppIcon 的名字）。 */
export function iconFor(e: FsEntry): string {
  if (e.is_dir) return 'folder';
  const ext = e.name.includes('.') ? e.name.split('.').pop()!.toLowerCase() : '';
  if (['zip', 'gz', 'tar', 'xz', 'bz2', '7z', 'rar'].includes(ext)) return 'zip';
  if (['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'svg'].includes(ext)) return 'image';
  if (e.mime.startsWith('audio/') || ['mp3', 'flac', 'wav'].includes(ext)) return 'audio';
  if (e.mime.startsWith('video/') || ['mp4', 'mkv', 'avi'].includes(ext)) return 'video';
  if (['sh', 'bash', 'py', 'js', 'ts', 'go', 'json', 'yaml', 'yml', 'toml'].includes(ext)) return 'code';
  if (['txt', 'md', 'log', 'conf', 'csv'].includes(ext) || e.mime.startsWith('text/')) return 'doc';
  return 'file';
}
