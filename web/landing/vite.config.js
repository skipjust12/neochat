import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The bundle is committed to server/assets/landing and embedded into the Go
// binary (server.go's //go:embed assets), so neither the Dockerfile nor CI's
// `go build` needs Node. Rebuild with `npm run build` after editing src/.
export default defineConfig({
  plugins: [react()],
  base: '/assets/landing/',
  build: {
    outDir: fileURLToPath(new URL('../../server/assets/landing', import.meta.url)),
    emptyOutDir: true,
    target: 'es2022',
    cssCodeSplit: false,
    rollupOptions: {
      input: fileURLToPath(new URL('./src/main.jsx', import.meta.url)),
      output: {
        // Fixed names: frontend.html references them directly.
        entryFileNames: 'landing.js',
        assetFileNames: 'landing[extname]',
        codeSplitting: false
      }
    }
  }
});
