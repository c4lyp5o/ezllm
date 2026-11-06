import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    chunkSizeWarningLimit: 700,
    rollupOptions: {
      output: {
        // Vite 8 (rolldown) only accepts a FUNCTION here; the object form
        // throws "manualChunks is not a function". Keep the same two groups:
        // recharts ships its own d3/victory-vendor tree, react its scheduler.
        manualChunks(id: string) {
          if (!id.includes('node_modules')) return;
          if (/node_modules\/(recharts|d3-[^/]+|victory-vendor|react-smooth|decimal\.js-light|eventemitter3|lodash)\//.test(id)) {
            return 'recharts';
          }
          if (/node_modules\/(react|react-dom|react-router|react-router-dom|scheduler)\//.test(id)) {
            return 'react';
          }
        },
      },
    },
  },
  server: {
    proxy: {
      '/admin': 'http://127.0.0.1:20129',
      '/v1': 'http://127.0.0.1:20129',
      '/healthz': 'http://127.0.0.1:20129',
    },
  },
});
