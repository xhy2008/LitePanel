import { describe, it, expect } from 'vitest';
import { cwdOfQuery, stripCwd } from '../composables/cwdQuery';

// 文件页"在终端中打开"靠 ?cwd= 传目录。这函数小，但它坏掉的表现是
// "点了菜单什么也没发生"，而这条链路跨了两个页面，真机上最难归因。

describe('cwdOfQuery', () => {
  it('取到目录', () => expect(cwdOfQuery({ cwd: '/data/x' })).toBe('/data/x'));
  it('没有就是空串', () => expect(cwdOfQuery({})).toBe(''));
  // ?cwd[]=a 会解析成数组；当成字符串用会得到 "a" 或 "[object Object]",
  // 那是一个看起来能用、实际打开错误目录的失败。
  it('数组形式一律当作没有', () => {
    expect(cwdOfQuery({ cwd: ['/a', '/b'] })).toBe('');
  });
  it('空白等于没有（不弹出起始目录为空的抽屉）', () => {
    expect(cwdOfQuery({ cwd: '   ' })).toBe('');
  });
});

describe('stripCwd', () => {
  it('只去掉 cwd，别的参数原样留着', () => {
    expect(stripCwd({ cwd: '/a', tab: 'x' })).toEqual({ tab: 'x' });
  });
  // 全清掉之后必须是个空对象而不是 undefined：router.replace 收到
  // undefined 的 query 会保留上一次的，等于什么都没抹掉。
  it('没有别的参数时得到空对象', () => {
    expect(stripCwd({ cwd: '/a' })).toEqual({});
  });
});
