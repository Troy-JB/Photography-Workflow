import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// base './' keeps the built asset URLs relative so the Wails asset server can
// serve them from the embedded FS root.
export default defineConfig({
  plugins: [vue()],
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 34115,
  },
})
