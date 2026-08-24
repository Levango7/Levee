import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'
import { resolve } from 'node:path'

// Vite configuration for the LEVEE Web UI.
//
// The dev server proxies API requests to the LEVEE gRPC-gateway REST endpoint
// (default :8080) so that the frontend can call `/api/...` paths without
// worrying about CORS or absolute URLs. The build emits static assets to
// `dist/`, which the Go binary embeds via `internal/web`.
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const apiTarget = env.VITE_API_TARGET || 'http://localhost:8080'

  return {
    plugins: [vue()],
    resolve: {
      alias: {
        '@': resolve(__dirname, 'src'),
      },
    },
    server: {
      port: 5173,
      strictPort: true,
      proxy: {
        // REST API exposed by grpc-gateway.
        '/api': {
          target: apiTarget,
          changeOrigin: true,
          // No rewrite needed — /api/* paths map directly to gateway routes.
        },
      },
    },
    build: {
      outDir: 'dist',
      assetsDir: 'assets',
      sourcemap: false,
      // Produce a single chunk for the vendor code to keep the embed bundle
      // small and stable across releases.
      rollupOptions: {
        output: {
          manualChunks: {
            vendor: ['vue', 'vue-router', 'element-plus', '@element-plus/icons-vue', 'axios', 'dayjs'],
          },
        },
      },
    },
  }
})