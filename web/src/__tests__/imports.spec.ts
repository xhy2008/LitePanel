import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs';
import { join, dirname, resolve } from 'node:path';

// 相对导入路径写错，是这一轮里第二次踩到的坑（fsMenu 里写成
// '../components/files/ContextMenu.vue'，FilesView 里把 AppIcon 写成
// '../AppIcon.vue'）。它危险的地方在于**类型检查抓不到**：env.d.ts 里
// 有 declare module '*.vue'，于是任何以 .vue 结尾的 specifier 都被当成
// 合法模块名，vue-tsc 一路放行，直到 vite 打包才炸。
//
// 而构建是流程的最后一步 —— 前面跑完 500 个测试全绿之后才失败，白白
// 浪费一整轮。这里在测试阶段就把"文件到底在不在"核对掉。

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    if (name === 'node_modules' || name === 'dist') continue;
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else if (/\.(vue|ts)$/.test(name)) out.push(p);
  }
  return out;
}

// 只看运行时导入：type-only 的 import 会被编译器整条擦掉，指向哪里都
// 不影响构建（而且 `import type { X } from './X.vue'` 本来就该由类型
// 检查负责）。
// 两种形态都要抓：
//   import x from './a'        ·  import { x } from './a'
//   import './style.css'       ← 副作用式没有 from，但它同样是运行时导入
const IMPORT_RE = /(?:^|\n)\s*(?:import|export)(?!\s+type\b)[^;]*?from\s*['"](\.[^'"]*)['"]/g;
// 副作用式单独一条：整行就是 import '...';
const BARE_RE = /(?:^|\n)\s*import\s*['"](\.[^'"]*)['"]/g;

const files = walk('src').filter((f) => !f.includes('__tests__'));

describe('相对导入路径', () => {
  it('扫到了足量源文件（别因为路径写错而"全部通过"）', () => {
    expect(files.length).toBeGreaterThan(30);
  });

  it('每个相对导入都指向真实存在的文件', () => {
    const bad: string[] = [];
    for (const f of files) {
      const src = readFileSync(f, 'utf-8');
      const specs = [...src.matchAll(IMPORT_RE), ...src.matchAll(BARE_RE)].map((m) => m[1]);
      for (const spec of specs) {
        const base = resolve(dirname(f), spec);
        // vite/ts 的解析顺序：原样 → .ts/.vue → /index.ts/.vue
        const ok =
          existsSync(base) ||
          ['.ts', '.vue', '.tsx', '.js'].some((e) => existsSync(base + e)) ||
          ['.ts', '.vue'].some((e) => existsSync(join(base, 'index' + e)));
        if (!ok) bad.push(`${f} -> ${spec}`);
      }
    }
    expect(bad, '不存在的相对导入:\n' + bad.join('\n')).toEqual([]);
  });
});
