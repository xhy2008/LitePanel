/// <reference types="vite/client" />
declare module '*.vue' {
  import type { DefineComponent } from 'vue';
  const c: DefineComponent<Record<string, unknown>, Record<string, unknown>, unknown>;
  export default c;
}
