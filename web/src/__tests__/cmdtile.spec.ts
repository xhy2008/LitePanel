import { describe, expect, it, vi } from 'vitest';
import { mount } from '@vue/test-utils';
import CommandTile from '../components/quickcmd/CommandTile.vue';
import { hasIcon } from '../components/iconPaths';
import type { CommandRow } from '../api/quickcmd';

function row(over: Partial<CommandRow> = {}): CommandRow {
  return {
    id: 1,
    name: '磁盘占用排行',
    command: 'du -sh /data/* | sort -h',
    cwd: '',
    need_confirm: false,
    sort: 10,
    created_at: 0,
    ...over,
  };
}

describe('CommandTile', () => {
  it('显示名称与命令行', () => {
    const w = mount(CommandTile, { props: { row: row() } });
    expect(w.text()).toContain('磁盘占用排行');
    expect(w.text()).toContain('du -sh /data/* | sort -h');
  });

  it('点磁贴触发 run', async () => {
    const w = mount(CommandTile, { props: { row: row() } });
    await w.find('.cmd-tile').trigger('click');
    expect(w.emitted('run')?.[0]).toEqual([1]);
  });

  it('危险命令带"确认"角标（点之前就该看得见）', () => {
    const w = mount(CommandTile, { props: { row: row({ need_confirm: true }) } });
    expect(w.find('.kb-danger').exists()).toBe(true);
    expect(w.text()).toContain('确认');
  });

  it('安全命令不带确认角标', () => {
    const w = mount(CommandTile, { props: { row: row() } });
    expect(w.find('.kb-danger').exists()).toBe(false);
  });

  it('在飞状态吞掉点击：重复注入是真会跑第二遍', async () => {
    const w = mount(CommandTile, { props: { row: row(), pending: true } });
    await w.find('.cmd-tile').trigger('click');
    expect(w.emitted('run')).toBeUndefined();
    expect(w.find('.cmd-tile').classes()).toContain('busy');
  });

  // 点磁贴内部任何控件都不许顺带触发执行。少了这条，删掉
  // stopPropagation 后全部测试照样绿 —— 而线上的表现是"点删除，先把命令
  // 跑了一遍再删"，对 rm -rf 那条就是一次真事故。
  it.each([
    ['.mv-up', '上移'],
    ['.mv-down', '下移'],
    ['.ib-edit', '编辑'],
    ['.ib-del', '删除'],
  ])('%s（%s）不触发执行', async (sel) => {
    const w = mount(CommandTile, { props: { row: row() } });
    await w.find(sel).trigger('click');
    expect(w.emitted('run')).toBeUndefined();
  });

  it('排序控件不许是整块按钮里的嵌套按钮：移动端误触率很高', () => {
    // 磁贴整体是 role=button，内部再放 <button> 会在 a11y 树上出现
    // "按钮里的按钮"，VoiceOver 读不出层级；内部控件一律用 role 明确的 div。
    const w = mount(CommandTile, { props: { row: row() } });
    expect(w.find('.cmd-tile').attributes('role')).toBe('button');
    for (const sel of ['.mv-up', '.mv-down', '.ib-edit', '.ib-del']) {
      expect(w.find(sel).element.tagName, sel).toBe('DIV');
    }
  });

  it('用到的图标都有路径定义（AppIcon 对未知名字静默画空 SVG）', () => {
    for (const name of ['terminal', 'bolt', 'alert', 'add', 'arrow_upward', 'arrow_downward', 'edit', 'trash']) {
      expect(hasIcon(name), `图标 ${name} 缺路径定义`).toBe(true);
    }
  });
});
