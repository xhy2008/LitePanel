import { describe, it, expect } from 'vitest';
import { mount } from '@vue/test-utils';
import AppIcon from '../components/AppIcon.vue';
import { hasIcon } from '../components/iconPaths';
import {
  joinPath,
  crumbOf,
  parentOf,
  sizeLabel,
  mtimeLabel,
  listQuery,
  iconFor,
} from '../api/files';
import type { FsEntry } from '../api/files';

function e(name: string, over: Partial<FsEntry> = {}): FsEntry {
  return { name, is_dir: false, is_symlink: false, size: 0, mtime: 0, mime: '', mode: '', ...over };
}

describe('joinPath', () => {
  it('普通目录拼一个斜杠', () => {
    expect(joinPath('/data/x', 'a.txt')).toBe('/data/x/a.txt');
  });

  // 根目录必须特判："//" 在 f2fs 上的解释与 "/" 不同，列表显示正常而
  // 后续 stat/download 全 404 —— 只在根目录点文件才复现的裂法最难查。
  it('根目录不产生双斜杠', () => {
    expect(joinPath('/', 'data')).toBe('/data');
  });

  // 空目录时也必须给绝对路径。后端明确拒绝"按进程 cwd 解释相对路径"
  // （M6-T1 的禁令：同一个参数在开发机与目标机指向不同目录），
  // 这里若拼出 'data'，得到的就是一个随面板启动方式漂移的删除目标。
  it('空目录也拼成绝对路径，绝不产出相对路径', () => {
    expect(joinPath('', 'data')).toBe('/data');
  });

  it('不去理会重复的尾斜杠以外的东西（交给后端 AbsClean）', () => {
    expect(joinPath('/data/', 'a')).toBe('/data/a');
  });
});

describe('crumbOf / parentOf', () => {
  it('每一段可点，且第一项是根', () => {
    expect(crumbOf('/data/www/app')).toEqual([
      { label: '/', path: '/' },
      { label: 'data', path: '/data' },
      { label: 'www', path: '/data/www' },
      { label: 'app', path: '/data/www/app' },
    ]);
  });

  it('根目录只有一项（根上没有"上一级"）', () => {
    expect(crumbOf('/')).toEqual([{ label: '/', path: '/' }]);
    expect(parentOf('/')).toBe('/');
  });

  it('一级目录的父是根，不是空串', () => {
    expect(parentOf('/data')).toBe('/');
    expect(parentOf('/data/www')).toBe('/data');
  });
});

describe('sizeLabel', () => {
  it('0 显示 "0 B" 而不是空', () => {
    expect(sizeLabel(0)).toBe('0 B');
  });
  it('字节不换算', () => {
    expect(sizeLabel(1023)).toBe('1023 B');
  });
  it('进位到合适单位', () => {
    expect(sizeLabel(1536)).toBe('1.5 KB');
    expect(sizeLabel(5 * 1024 * 1024)).toBe('5.0 MB');
    expect(sizeLabel(3.5 * 1024 ** 4)).toBe('3.5 TB');
  });
  it('大数不留小数', () => {
    expect(sizeLabel(1500 * 1024 * 1024)).toBe('1.5 GB');
    expect(sizeLabel(200 * 1024 * 1024)).toBe('200 MB');
  });
  it('负数与 NaN 显示未知', () => {
    expect(sizeLabel(-1)).toBe('—');
    expect(sizeLabel(NaN)).toBe('—');
  });
});

describe('mtimeLabel', () => {
  const now = new Date(2026, 5, 15, 12, 0, 0).getTime();
  it('当年给 月-日 时:分', () => {
    const t = new Date(2026, 4, 3, 9, 5, 0).getTime() / 1000;
    expect(mtimeLabel(t, now)).toBe('05-03 09:05');
  });
  it('跨年补年份', () => {
    const t = new Date(2025, 11, 31, 23, 59, 0).getTime() / 1000;
    expect(mtimeLabel(t, now)).toBe('2025-12-31');
  });
  it('0 回空串（没有 mtime 就别编一个时间）', () => {
    expect(mtimeLabel(0, now)).toBe('');
  });
});

describe('listQuery', () => {
  // 断言的是"能原样还原"，不是"等于 encodeURIComponent 的输出"：
  // URLSearchParams 把空格编成 '+'（Go 的 Query().Get 会还原成空格），
  // 逐字比对会在编码方式变了但同样正确时报假红。
  it('中文与空格路径能无损往返', () => {
    const q = listQuery({ path: '/data/中文 目录' });
    expect(q).not.toMatch(/[\u4e00-\u9fff]/); // 不能把原始 UTF-8 塞进 URL
    const back = new URLSearchParams(q).get('path');
    expect(back).toBe('/data/中文 目录');
  });
  it('只带非默认值（URL 短且可分享）', () => {
    const q = listQuery({ path: '/x' });
    expect(q).toBe('path=%2Fx');
  });
  it('desc → order=desc，hidden → show_hidden=1（后端只认这两个写法）', () => {
    const q = listQuery({ path: '/x', desc: true, hidden: true, sort: 'size', page: 3, size: 50 });
    expect(q).toContain('order=desc');
    expect(q).toContain('show_hidden=1');
    expect(q).toContain('sort=size');
    expect(q).toContain('page=3');
    expect(q).toContain('size=50');
  });
});

describe('iconFor', () => {
  const cases: Array<[FsEntry, string]> = [
    [e('dir', { is_dir: true }), 'folder'],
    [e('a.zip'), 'zip'],
    [e('a.tar.gz'), 'zip'],
    [e('a.PNG'), 'image'],
    [e('a.mp3'), 'audio'],
    [e('a.mp4'), 'video'],
    [e('a.go'), 'code'],
    [e('a.txt'), 'doc'],
    [e('notes', { mime: 'text/plain' }), 'doc'],
    [e('mystery.bin'), 'file'],
  ];

  it.each(cases.map(([en, want]) => [en.name, en, want] as const))('%s', (_n, en, want) => {
    expect(iconFor(en)).toBe(want);
  });

  // AppIcon 对未知名字是 PATHS[name] ?? []，静默画空 SVG ——
  // "图标没了"在页面上几乎看不出来。名字从 iconFor 的真实输出里取：
  // 硬编码名单只会保护测试自己写的那几个字，实现换成任意错名字照绿。
  it('iconFor 用到的每个图标名都有图形定义', () => {
    const names = [...new Set(cases.map(([en]) => iconFor(en)))];
    for (const n of names) {
      expect(hasIcon(n), `图标 ${n} 缺路径定义`).toBe(true);
      const w = mount(AppIcon, { props: { name: n } });
      expect(w.findAll('path').length, `图标 ${n} 画不出任何路径`).toBeGreaterThan(0);
    }
  });
});
