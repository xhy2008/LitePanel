import { computed, onMounted, onUnmounted, readonly, ref } from 'vue';

export type Breakpoint = 'pc' | 'tablet' | 'phone';

// 与设计 16.1 的断点表一致。
export const TABLET_MIN = 768;
export const PC_MIN = 1200;

export function breakpointOf(width: number): Breakpoint {
  if (width >= PC_MIN) return 'pc';
  if (width >= TABLET_MIN) return 'tablet';
  return 'phone';
}

const current = ref<Breakpoint>(
  breakpointOf(typeof window === 'undefined' ? PC_MIN : window.innerWidth),
);

// 模块级单例：多处 useBreakpoint 共用一个 resize 监听。
let installed = 0;
let width = typeof window === 'undefined' ? PC_MIN : window.innerWidth;
current.value = breakpointOf(width);

function onResize() {
  width = window.innerWidth;
  current.value = breakpointOf(width);
}

/** 组件级分支渲染用的断点（不是 CSS display:none）。 */
export function useBreakpoint() {
  onMounted(() => {
    if (typeof window === 'undefined') return;
    if (installed++ === 0) {
      window.addEventListener('resize', onResize, { passive: true });
    }
    // 模块可能在 innerWidth 变化前就被求值（SSR / 测试），挂载时再校一次。
    onResize();
  });
  onUnmounted(() => {
    if (--installed === 0 && typeof window !== 'undefined') {
      window.removeEventListener('resize', onResize);
    }
  });

  return {
    bp: readonly(current),
    width: () => width,
    isPC: computed(() => current.value === 'pc'),
    isTablet: computed(() => current.value === 'tablet'),
    isPhone: computed(() => current.value === 'phone'),
  };
}
