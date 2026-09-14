import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import App from '../App.vue';

// M1-T1：验证前端测试链路可用，且应用骨架能挂载。
describe('App 骨架', () => {
  it('渲染出面板标识', () => {
    const w = mount(App);
    expect(w.text()).toContain('LitePanel');
  });
});
