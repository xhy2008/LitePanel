/// <reference types="vite/client" />
declare module '*.vue' {
  import type { DefineComponent } from 'vue';
  const c: DefineComponent<Record<string, unknown>, Record<string, unknown>, unknown>;
  export default c;
}

declare interface Window {
  /** index.html 内联诊断钩子：置 true 表示启动已完成，看门狗据此闭嘴。 */
  __litepanelReady: boolean;
  __litepanelFatal: (e: unknown) => void;
}
