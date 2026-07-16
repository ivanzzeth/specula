import { defineConfig, type Plugin } from 'vite';
import react from '@vitejs/plugin-react';
import { writeFileSync } from 'node:fs';
import { resolve } from 'node:path';

// build 后重建 dist/.gitkeep：emptyOutDir 会清掉它，但该占位需保留——它让
// //go:embed all:dist 在"未构建的裸 clone"上也能编译（dist 真产物不入库）。
function keepGitkeep(): Plugin {
  let outDir = 'dist';
  return {
    name: 'keep-dist-gitkeep',
    configResolved(c) {
      outDir = c.build.outDir;
    },
    closeBundle() {
      writeFileSync(resolve(outDir, '.gitkeep'), '');
    },
  };
}

// base '/'，build 输出 dist；dev 时把 /api 代理到本地控制面（:8081）。
export default defineConfig({
  base: '/',
  plugins: [react(), keepGitkeep()],
  resolve: {
    // '@' → src, the import root shadcn/ui components expect.
    alias: { '@': resolve(__dirname, 'src') },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': { target: 'http://127.0.0.1:8081', changeOrigin: true },
      '/healthz': { target: 'http://127.0.0.1:8081', changeOrigin: true },
      '/metrics': { target: 'http://127.0.0.1:8081', changeOrigin: true },
    },
  },
});
