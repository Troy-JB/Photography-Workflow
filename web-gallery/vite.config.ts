import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// The gallery is served from its own origin. VITE_API_BASE may point it at the
// API host; in dev the proxy keeps same-origin cookies-free calls simple.
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5174,
    proxy: {
      '/g/': 'http://localhost:8080',
    },
  },
  build: {
    target: 'es2022',
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
})
