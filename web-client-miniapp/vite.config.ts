import { defineConfig, loadEnv } from 'vite'
import type { Plugin } from 'vite'
import uniModule from '@dcloudio/vite-plugin-uni'

const uni = ((uniModule as unknown as { default?: () => Plugin[] }).default ?? uniModule) as () => Plugin[]

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), 'VITE_')
  const apiTarget = env.VITE_API_PROXY_TARGET || 'http://localhost:8080'

  return {
    plugins: [uni()],
    server: {
      port: 5173,
      strictPort: true,
      proxy: {
        '/c/': { target: apiTarget, changeOrigin: true },
      },
    },
  }
})
