/// <reference types="vitest" />
import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'
import { resolve } from 'node:path'

// TZ 必须在主进程钉住（本文件在池生成前求值，threads worker 创建时快照
// 父进程 env，必然带上 UTC）。spec 顶部 process.env.TZ 或 test.env 都在
// vitest 5 worker 预热之后才生效——预热阶段的环境装配/内部代码先碰
// Date，ICU 缓存宿主时区，后设不生效，formatTimestamp 的 UTC 断言随之
// 翻车。与 CI（ubuntu, TZ=UTC）一致，也符合 roadmap 约定：UTC 存储、
// 展示层按用户时区转换（mvp-plus1-roadmap.md「时区处理」）。
process.env.TZ = 'UTC'

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
    // Unit tests run under vitest (jsdom provides localStorage / window so
    // the api client and SSO helpers can be exercised unmodified).
    test: {
      environment: 'jsdom',
      // vitest 5 把默认池从 threads 换成了 forks；forks worker 在 drvfs/9p
      // 挂载（如 WSL 的 /mnt/f 工作区）上启动慢到撞 60s 超时，全军覆没。
      // threads 恢复 vitest 3 行为，实测 36 文件全部正常拉起。
      pool: 'threads',
      // TZ 的钉子在文件顶部（主进程，先于 worker 生成）：threads worker 在
      // 创建时快照父进程 env，必然带上。不能靠 spec 顶部 process.env.TZ，
      // 也不能靠 test.env——两者都在 vitest 5 worker 预热（环境装配/内部
      // 代码先碰 Date、ICU 缓存宿主时区）之后才生效，formatTimestamp 的
      // UTC 断言随之翻车。与 CI（ubuntu, TZ=UTC）一致，也符合 roadmap
      // 约定：UTC 存储、展示层按用户时区转换（mvp-plus1-roadmap.md「时区处理」）。
      // Pin the document origin: redirect_uri construction asserts depend on
      // window.location.origin (vitest's jsdom default is :3000).
      environmentOptions: { jsdom: { url: 'http://localhost/' } },
      setupFiles: ['./vitest.setup.ts'],
      include: ['src/**/*.spec.ts'],
      // jsdom cannot navigate, so the un-fakeable window.location calls log
      // "Not implemented: navigation" through the virtual console. The tests
      // assert the observable side effects instead; drop just that noise.
      onConsoleLog: (log) => !log.includes('Not implemented: navigation'),
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