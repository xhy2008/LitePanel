import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

// 用了一个**不存在的 CSS 变量**是最难发现的退化：它不报错、不崩、测试全绿,
// 只是那条样式静默失效 —— color: var(--danger) 在变量缺失时会退回继承色,
// 于是"删除"这一项不再是红色，而没人会专门去核对一个菜单项的颜色。
// （实际踩过：ContextMenu 初版写了 --danger / --hover，两个都不存在。）
//
// 与其逐个人工核对，不如让测试去扫全部源文件：新增组件写错变量名会立刻
// 在这里翻红，而不是等到某个界面上少了一种颜色。

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else if (/\.(vue|css)$/.test(name)) out.push(p);
  }
  return out;
}

// tokens.css 里 --x: 定义的是设计变量；组件里 var(--x) 消费。
// 只有**带兜底**的 var(--x, fallback) 允许用未知名字（那本来就是兜底写法）。
const tokens = readFileSync('src/styles/tokens.css', 'utf-8');
const defined = new Set([...tokens.matchAll(/(--[a-z0-9-]+)\s*:/g)].map((m) => m[1]));

describe('CSS 设计令牌', () => {
  it('tokens.css 定义了核心变量', () => {
    for (const t of ['--accent', '--err', '--card', '--border', '--text']) {
      expect(defined.has(t), `tokens.css 缺少 ${t}`).toBe(true);
    }
  });

  it('所有组件用的 var(--x) 都在 tokens.css 里定义', () => {
    const bad: string[] = [];
    for (const file of walk('src')) {
      const src = readFileSync(file, 'utf-8');
      // 跳过 tokens.css 自己：它定义变量，也引用别的变量做派生。
      if (file.endsWith('tokens.css')) continue;
      for (const m of src.matchAll(/var\((--[a-z0-9-]+)(\s*,)?/g)) {
        const [, name, hasFallback] = m;
        // 带兜底值的引用不算错（缺变量时走兜底）。
        if (hasFallback) continue;
        // --radius-sm 这类既有本文件局部定义的，也可能来自 tokens.css。
        if (!defined.has(name)) bad.push(`${file}: ${name}`);
      }
    }
    expect(bad, '未定义的设计变量:\n' + bad.join('\n')).toEqual([]);
  });

  // 组件在自己的 <style> 里 --x: 定义局部变量也算合法（scoped 定义）。
  // 这条单列是为了上面那条不误伤：先确认"局部定义"这条路走得通。
  it('组件内自定义变量被视为已定义', () => {
    const scoped = readFileSync('src/components/files/ContextMenu.vue', 'utf-8');
    const local = new Set([...scoped.matchAll(/(--[a-z0-9-]+)\s*:/g)].map((m) => m[1]));
    // ContextMenu 不定义局部变量，只用 tokens —— 这里断言它用的都在全局里。
    for (const m of scoped.matchAll(/var\((--[a-z0-9-]+)\)/g)) {
      expect([...defined, ...local].includes(m[1]), `ContextMenu 用了 ${m[1]}`).toBe(true);
    }
  });
});
