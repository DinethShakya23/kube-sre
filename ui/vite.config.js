import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Builds into internal/web/static, which the Go binary embeds. Names carry a content hash,
// because a CDN in front may cache assets for hours whatever the server says; index.html is
// never cached, so a new build is picked up at once. Nothing is inlined: the CSP allows only 'self'.
export default defineConfig({
  base: '/ui/',
  plugins: [react()],
  build: {
    outDir: '../internal/web/static',
    emptyOutDir: true,
    cssCodeSplit: false,
    assetsInlineLimit: 0,
    chunkSizeWarningLimit: 900,
    rollupOptions: {
      output: {
        entryFileNames: 'assets/app-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: info => (info.names?.[0]?.endsWith('.css') ? 'assets/app-[hash].css' : 'assets/[name]-[hash][extname]')
      }
    }
  },
  server: {
    // Dev talks to the server an SSH tunnel forwards to :8765, or KUBESRE_URL.
    proxy: Object.fromEntries(
      ['/v1', '/healthz', '/readyz'].map(p => [p, { target: process.env.KUBESRE_URL || 'http://127.0.0.1:8765' }])
    )
  }
});
