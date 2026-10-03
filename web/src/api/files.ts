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
  if (typeof abs !== 'string' || abs === '' || abs === '/') return out;
  let acc = '';
  for (const seg of abs.split('/').filter(Boolean)) {
    acc += '/' + seg;
    out.push({ label: seg, path: acc });
  }
  return out;
}

/** 父目录。根与 '/' 的父是自身（面包屑在根上不再显示"上一级"）。
 *
 * 非字符串输入（undefined / null）一律当根：这两个函数被面包屑的
 * computed 直接调，在这里抛 = 整个视图白屏。store 已经不会灌脏值,
 * 但"最多显示不好、绝不抛"才是能放心的契约。 */
export function parentOf(abs: string): string {
  if (typeof abs !== 'string' || abs === '' || abs === '/') return '/';
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

/**
 * 属性面板的「类型」文案。
 *
 * 只报能确证的部分：目录/符号链接看 lstat 的事实，其余先看后端给的 mime,
 * mime 不可用（Linux 上很多文件 lstat 不带 mime）再退到扩展名。认不出来
 * 就说「文件」—— 属性对话框里的一个错分类比缺一项更糟，用户会拿它决定
 * 要不要打开 / 删掉。
 */
export function typeLabel(e: FsEntry): string {
  if (e.is_symlink) return '符号链接';
  if (e.is_dir) return '目录';
  const mime = (e.mime || '').toLowerCase();
  const hit = TYPE_WORDS.find(([re]) => re.test(mime));
  if (hit) return hit[1];
  // 扩展名兜底：和 iconFor 用同一份分类口径，两处说不一样会是 bug 现场。
  // .tar.gz 这类双扩展名：取最后一段就只会看到 gz，已在 ARCHIVES 里；
  // 而 .tar 本身也在，不必再解析整串名字。
  const ext = e.name.includes('.') ? e.name.split('.').pop()!.toLowerCase() : '';
  if (ARCHIVES.includes(ext)) return '压缩包';
  return TYPE_BY_EXT[ext] ?? '文件';
}

const TYPE_WORDS: Array<[RegExp, string]> = [
  [/^image\//, '图片'],
  [/^video\//, '视频'],
  [/^audio\//, '音频'],
  [/^text\//, '文本'],
  [/pdf/, 'PDF'],
  [/(zip|gzip|x-tar|7z|rar|xz|bzip2)/, '压缩包'],
  [/json/, 'JSON'],
  [/(yaml|x-yaml|toml|x-toml)/, '配置'],
  [/(shellscript|x-sh)/, '脚本'],
  // octet-stream 不在这里：它是后端"认不出来"的默认值，不是可执行。
  [/^application\/(x-msdownload|x-executable|executable)$/, '程序'],
];

const ARCHIVES = ['zip', 'gz', 'tar', 'tgz', 'xz', 'bz2', '7z', 'rar'];

const TYPE_BY_EXT: Record<string, string> = {
  md: '文本', txt: '文本', log: '文本', csv: '表格', tsv: '表格',
  conf: '配置', ini: '配置', yaml: '配置', yml: '配置', toml: '配置',
  // 只有名字本身就是「可执行」的扩展名才敢这么说。.bin 不算：它可能是
  // 固件、core dump、任意数据文件，说成「程序」是替用户做他没做的判断。
  exe: '程序', msi: '程序', deb: '程序', rpm: '程序', apk: '程序',
  json: 'JSON', js: '脚本', ts: '脚本', py: '脚本', go: '程序', sh: '脚本', bash: '脚本',
  png: '图片', jpg: '图片', jpeg: '图片', gif: '图片', webp: '图片', svg: '图片', bmp: '图片',
  mp4: '视频', mkv: '视频', avi: '视频', mp3: '音频', flac: '音频', wav: '音频',
};

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
