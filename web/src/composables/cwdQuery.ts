// 从路由 query 里取"要打开的目录"。
//
import type { LocationQuery, LocationQueryRaw } from 'vue-router';

// 单独成模块而不是写在 TerminalView 里：终端视图要挂 xterm + WS + 定时器,
// 为了测一个 query 参数去 mock 这一整套不现实（而"顺手加的、没法测的
// 小功能"正是最容易坏掉的东西）。这里留纯函数，视图里只有一行接线。
/** 不是字符串就当没有：?cwd[]=a 这种数组形式不该被拼成 "a"。 */
export function cwdOfQuery(q: LocationQuery): string {
  const v = q.cwd;
  if (typeof v !== 'string') return '';
  return v.trim();
}

/**
 * 抹掉已消费的 cwd。
 *
 * 必须 replace 而不是 push：这次改写不该在浏览器历史里占一格 ——
 * 后退一步回到仍然带 ?cwd= 的地址、于是又弹一次抽屉，比留着更烦人。
 */
export function stripCwd(q: LocationQuery): LocationQueryRaw {
  const out: LocationQueryRaw = {};
  for (const [k, v] of Object.entries(q)) if (k !== 'cwd') out[k] = v;
  return out;
}
