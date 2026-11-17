import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// The Lumeo homepage, authentication screens and admin console share this app
// origin. In development Vite proxies API, customer, and agent upload routes.
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5175,
    proxy: {
      '/api/': 'http://localhost:8080',
      '/c/': 'http://localhost:8080',
      '/agent/': 'http://localhost:8080',
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
