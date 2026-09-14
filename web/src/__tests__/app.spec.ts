import { describe, expect, it } from 'vitest';
import { routes } from '../router';

// M1-T1 时这里挂载的是 "LitePanel" 占位骨架；M1-T8 起根组件变为出口路由，
// 骨架断言随之演进为路由表断言（5 个 tab + 登录页 + 外壳）。
describe('路由表', () => {
  const shell = routes.find((r) => r.path === '/');
  const children = shell?.children ?? [];
  const names = children.map((c) => c.name).filter(Boolean);

  it('包含五个一级标签页', () => {
    expect(names).toEqual(
      expect.arrayContaining(['quick', 'term', 'files', 'downloads', 'settings']),
    );
  });

  it('根路径重定向到快捷命令', () => {
    const first = children[0];
    expect(first?.path).toBe('');
    expect((first as { redirect?: unknown }).redirect).toEqual({ name: 'quick' });
  });

  it('登录页是独立顶层路由（不套外壳）', () => {
    const login = routes.find((r) => r.path === '/login');
    expect(login?.name).toBe('login');
    expect(login?.component).toBeTruthy();
  });

  // 外壳在 App.vue 里做断点分支，父路由不带 component；
  // 若误删 children 或写错路径，这里会红。
  it('父路由不带 component，五个视图都是懒加载函数', () => {
    expect(shell).toBeTruthy();
    expect((shell as { component?: unknown }).component).toBeUndefined();
    for (const c of children.filter((c) => c.name)) {
      expect(typeof (c as { component?: unknown }).component).toBe('function');
    }
  });
});
