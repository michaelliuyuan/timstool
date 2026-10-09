import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

// App version single source: package.json "version" (MS-11i pen1), read at
// config load. The topbar badge renders __APP_VERSION__; a release bump is a
// package.json bump and nothing else. NOTE: the plugin-vue import MUST stay
// default-form — plugin-vue 6.x has no named "vue" export (strict ESM link
// fails; mis-transcribing this line cost the pen1 debugging session).
const pkg = JSON.parse(readFileSync(resolve(process.cwd(), 'package.json'), 'utf8'))

export default defineConfig({
  plugins: [vue()],
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version),
  },
  server: {
    port: 3000,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
      '/ws': {
        target: 'ws://localhost:8080',
        ws: true,
      },
    },
  },
  build: {
    outDir: '../dist',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        // pg2tidb is an ops/management tool, not a high-traffic app, so
        // code-splitting's size benefit is marginal — and independent view
        // chunks are exactly what can 404 behind an inconsistent static-dir /
        // cache layer (the #t48 CDC blank-page failure). Bundle everything into
        // one chunk: no view chunk can be missed or negative-cached.
        inlineDynamicImports: true,
      },
    },
  },
})
