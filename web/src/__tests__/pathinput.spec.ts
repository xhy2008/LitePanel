import { describe, it, expect } from 'vitest';
import { mount } from '@vue/test-utils';
import PathInput from '../components/files/PathInput.vue';
import Breadcrumb from '../components/files/Breadcrumb.vue';

// 地址栏：重点是输入法那一条（用中文输入法时最容易踩，也最不会被想到要测）。

async function edit(w: ReturnType<typeof mount>, val: string) {
  await w.find('.txt').trigger('click');
  await w.vm.$nextTick();
  const el = w.find('input');
  await el.setValue(val);
  return el;
}

function key(el: any, opts: { isComposing?: boolean } = {}) {
  const ev: any = new Event('keydown', { bubbles: true, cancelable: true });
  ev.key = 'Enter';
  ev.isComposing = !!opts.isComposing;
  el.element.dispatchEvent(ev);
  return ev;
}

describe('PathInput', () => {
  it('默认显示当前路径，点击才变成输入框', async () => {
    const w = mount(PathInput, { props: { modelValue: '/data/x' } });
    expect(w.find('input').exists()).toBe(false);
    expect(w.find('.txt').text()).toBe('/data/x');
    await w.find('.txt').trigger('click');
    expect(w.find('input').exists()).toBe(true);
  });

  it('回车提交改动过的路径', async () => {
    const w = mount(PathInput, { props: { modelValue: '/data' } });
    const el = await edit(w, '/etc/hosts');
    key(el);
    expect(w.emitted('submit')?.[0]).toEqual(['/etc/hosts']);
  });

  // 中文输入法下敲回车是为了**确认候选词**，不是"我要提交地址"。
  // 少了这条判断，用户想打 /下载 却在半路提交了一串 "xiazai"。
  it('输入法选词的回车既不提交也不阻止默认行为', async () => {
    const w = mount(PathInput, { props: { modelValue: '/data' } });
    const el = await edit(w, '/xia');
    const ev = key(el, { isComposing: true });
    expect(w.emitted('submit')).toBeUndefined();
    // 阻止默认行为会让候选词选不上字 —— 比误提交更糟。
    expect(ev.defaultPrevented).toBe(false);
  });

  it('普通回车阻止表单默认的提交行为', async () => {
    const w = mount(PathInput, { props: { modelValue: '/data' } });
    const el = await edit(w, '/tmp');
    expect(key(el).defaultPrevented).toBe(true);
  });

  it('空白输入不提交（不发一次注定失败的请求）', async () => {
    const w = mount(PathInput, { props: { modelValue: '/data' } });
    const el = await edit(w, '   ');
    key(el);
    expect(w.emitted('submit')).toBeUndefined();
  });

  it('没改动就不提交', async () => {
    const w = mount(PathInput, { props: { modelValue: '/data' } });
    const el = await edit(w, '/data');
    key(el);
    expect(w.emitted('submit')).toBeUndefined();
  });

  // 用户在输入框里打字时，别的东西改了 modelValue（比如后台刷新回填）
  // 不该把他敲的字吞掉。
  it('正在编辑时外部改 modelValue 不覆盖输入框', async () => {
    const w = mount(PathInput, { props: { modelValue: '/data' } });
    const el = await edit(w, '/data/半');
    await w.setProps({ modelValue: '/other' });
    expect((w.find('input').element as HTMLInputElement).value).toBe('/data/半');
    void el;
  });

  it('未在编辑时外部改 modelValue 会跟上', async () => {
    const w = mount(PathInput, { props: { modelValue: '/data' } });
    await w.setProps({ modelValue: '/var/log' });
    expect(w.find('.txt').text()).toBe('/var/log');
  });
});

describe('Breadcrumb', () => {
  it('根目录只有一段', () => {
    const w = mount(Breadcrumb, { props: { path: '/' } });
    expect(w.findAll('.seg')).toHaveLength(1);
  });

  // 少一段就是"跳不回去"：分段必须逐级完整。
  it('每级都有一段且带正确绝对路径', () => {
    const w = mount(Breadcrumb, { props: { path: '/data/x/y' } });
    const segs = w.findAll('.seg');
    expect(segs).toHaveLength(4);
    expect(segs.map((s) => s.text())).toEqual(['/', 'data', 'x', 'y']);
  });

  it('点中间段跳到那一级', async () => {
    const w = mount(Breadcrumb, { props: { path: '/data/x/y' } });
    await w.findAll('.seg')[2].trigger('click');
    expect(w.emitted('go')?.[0]).toEqual(['/data/x']);
  });

  it('上一级按钮跳父目录', async () => {
    const w = mount(Breadcrumb, { props: { path: '/data/x' } });
    await w.find('.up').trigger('click');
    expect(w.emitted('go')?.[0]).toEqual(['/data']);
  });

  // 根目录的父是自身，摆一个按下去毫无变化的按钮不如不摆。
  it('根目录不显示上一级', () => {
    expect(mount(Breadcrumb, { props: { path: '/' } }).find('.up').exists()).toBe(false);
  });

  it('最后一段是普通文本（不可再次跳转的暗示）', () => {
    const segs = mount(Breadcrumb, { props: { path: '/data/x' } }).findAll('.seg');
    expect(segs[segs.length - 1].classes()).toContain('last');
  });
});
