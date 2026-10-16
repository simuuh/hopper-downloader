import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // During `npm run dev`, forward API/WS calls to a locally running
      // hopperd so the UI can be developed against a real backend.
      '/api': 'http://127.0.0.1:9667',
      '/ws': { target: 'ws://127.0.0.1:9667', ws: true },
    },
  },
})
