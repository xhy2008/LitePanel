/// <reference types="vitest/config" />
import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

// 构建产物直接落到 Go embed 的目录，make build 前必须先 make web。
export default defineConfig({
  base: '/',
  plugins: [vue()],
  build: {
    outDir: '../internal/webdist/dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:9530',
      '/ws': { target: 'ws://127.0.0.1:9530', ws: true },
    },
  },
  test: {
    environment: 'happy-dom',
    include: ['src/**/*.spec.ts'],
  },
});
