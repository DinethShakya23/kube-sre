import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The website at the domain root. It shares components with the console
// but is deployed on its own, so its build is not committed.
export default defineConfig({
  root: 'site',
  base: '/',
  plugins: [react()],
  build: {
    outDir: '../site-dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 900
  }
});
