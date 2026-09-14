import { describe, expect, it } from 'vitest';
import { breakpointOf, type Breakpoint } from '../composables/useBreakpoint';

// M1-T8 验收：三个断点必须与实施的 1400/900/500 宽度用例一致。
describe('breakpointOf', () => {
  const cases: Array<[number, Breakpoint]> = [
    [1400, 'pc'],
    [1200, 'pc'],       // 边界：≥1200 为 PC
    [1199, 'tablet'],
    [900, 'tablet'],
    [768, 'tablet'],    // 边界：≥768 为平板
    [767, 'phone'],
    [500, 'phone'],
    [375, 'phone'],
  ];
  it.each(cases)('%ipx → %s', (w, want) => {
    expect(breakpointOf(w)).toBe(want);
  });
});
