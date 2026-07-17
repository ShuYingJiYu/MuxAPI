import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发环境将管理 API 转发到本地 Go 服务；生产环境由 Go 内嵌 dist。
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: { '/admin': 'http://127.0.0.1:8080' }, // 接真后端时走代理
  },
})
