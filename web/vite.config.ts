import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// Vite dev server config. /api and /healthz and /metrics are proxied
// to the Go api service so the React app can call them as same-origin.
// In production the React build is served by the api binary directly
// (replacing the placeholder live HTML), so this proxy only matters
// during dev (`npm run dev`).
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    host: '0.0.0.0',
    port: 5173,
    proxy: {
      '/api':     { target: 'http://api:8080', changeOrigin: true },
      '/healthz': { target: 'http://api:8080', changeOrigin: true },
      '/metrics': { target: 'http://api:8080', changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: true,
  },
})
