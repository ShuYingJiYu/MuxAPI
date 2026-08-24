import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发环境将管理 API 转发到本地 Go 服务；生产环境由 Go 内嵌 dist。
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      // 默认连接本地 Go 服务；预览线上数据时通过 MUXAPI_API_TARGET 临时切换。
      '/admin': { target: process.env.MUXAPI_API_TARGET || 'http://127.0.0.1:8080', changeOrigin: true },
      '/v1': { target: process.env.MUXAPI_API_TARGET || 'http://127.0.0.1:8080', changeOrigin: true },
    },
  },
})
