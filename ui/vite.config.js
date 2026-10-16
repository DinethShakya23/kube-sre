import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Builds into internal/web/static, which the Go binary embeds. Names are fixed (no-cache is
// sent anyway) and nothing is inlined, because the CSP allows only 'self'.
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
        entryFileNames: 'assets/app.js',
        chunkFileNames: 'assets/[name].js',
        assetFileNames: info => (info.names?.[0]?.endsWith('.css') ? 'assets/app.css' : 'assets/[name][extname]')
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
