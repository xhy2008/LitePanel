import { createPinia } from 'pinia';

// 单例：组件外（路由守卫等）取 store 时也用它，避免出现两份状态。
export const pinia = createPinia();
