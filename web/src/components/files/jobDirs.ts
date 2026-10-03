import { parentOf } from '../../api/files';
import { isActive } from '../../api/fsJobs';
import type { JobRow } from '../../api/fsJobs';

// 一个后台任务"改动了哪些目录"。
//
// 单独成模块而不是写在视图的 watch 里：这是判断"任务完成后该不该刷新
// 当前列表"的依据，判断错了要么"删完列表不动"（漏刷），要么"用户在翻
// 第 400 项时被弹回第一页"（多刷）。两种错误都不报错，只有把它做成纯
// 函数才能各摆一条用例钉死。

/**
 * 任务会改写到的目录集合。
 *
 * · 删除：源文件的父目录（文件从那儿消失）。dst 为空，忽略。
 * · 复制 / 移动：目标目录（dst，粘贴时就是当前目录）**加上**每个源的父
 *   目录 —— 移动会把源那一侧也清空，只认 dst 会让"剪切走的那个目录"
 *   停留在旧列表上。
 *
 * 返回去重后的绝对路径。src 里给的是文件本身而不是目录，所以一律取父目录；
 * 万一后端哪天传进来的是目录，parentOf 给出的是它的父，多刷一次父目录
 * 无害（不会漏刷到真正被改的那个）。
 */
export function affectedDirs(job: Pick<JobRow, 'op' | 'src' | 'dst'>): string[] {
  const out = new Set<string>();
  if (job.dst) out.add(job.dst);
  for (const p of job.src ?? []) out.add(parentOf(p));
  return [...out];
}

/**
 * 当前目录是否会被这个任务改写。
 *
 * dir 为空（还没加载出目录）时返回 false：没有"正在看的东西"可刷。
 */
export function touchesDir(job: Pick<JobRow, 'op' | 'src' | 'dst'>, dir: string): boolean {
  if (!dir) return false;
  return affectedDirs(job).includes(dir);
}

/**
 * 是否"刚从活跃态翻到终态"。
 *
 * 两个条件：上一帧是活跃态（wasActive === true）且现在不再是。上一帧还
 * 不存在（刚 load 出的历史任务）不算"刚跑完",否则一次冷启动就把满屏历史
 * 任务都当成刚完成、集体触发刷新。
 *
 * failed 也算数（一度想排除它，是错的）：后端里 delete/move 先校验后执行
 * （failed 时磁盘没变，多刷一次无害），但 copy **不回滚**（见 exec_run.go
 * "复制不整批回滚"）—— 传到一半失败的 copy 会把那一半留在目标目录,
 * 不刷就是漏刷。用户要看的错误文案在右下角任务抽屉里,刷列表冲不掉它。
 *
 * 只认这个边沿（而不是"只要看到 done 就刷"）：后端每 200ms 落库、done
 * 能连着推好几帧，不卡边沿就会把同一个目录连刷好几遍。
 */
export function justFinished(wasActive: boolean | undefined, now: JobRow): boolean {
  if (wasActive !== true) return false;
  return !isActive(now.state);
}
