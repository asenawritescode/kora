import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'
import { readdirSync, statSync, readFileSync, writeFileSync, existsSync } from 'fs'
import { join } from 'path'
import { brotliCompressSync, gzipSync } from 'zlib'

function compressionPlugin() {
  const extRe = /\.(js|mjs|json|css|html)$/i
  return {
    name: 'vite:compression',
    apply: 'build' as const,
    enforce: 'post' as const,
    closeBundle() {
      const outDir = path.resolve(__dirname, 'dist')
      if (!existsSync(outDir)) return
      const files: string[] = []
      function walk(dir: string) {
        for (const entry of readdirSync(dir)) {
          const full = join(dir, entry)
          const stat = statSync(full)
          if (stat.isDirectory()) walk(full)
          else if (extRe.test(full)) files.push(full)
        }
      }
      walk(outDir)
      for (const file of files) {
        const content = readFileSync(file)
        if (content.byteLength < 1024) continue
        const brPath = file + '.br'
        const gzPath = file + '.gz'
        try {
          writeFileSync(brPath, brotliCompressSync(content))
          writeFileSync(gzPath, gzipSync(content))
        } catch {
          // skip compression failures
        }
      }
    },
  }
}

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    compressionPlugin(),
  ],
  // Root-relative asset paths so the SPA works at any URL prefix
  // (/workspace, /s/airtime/workspace, etc.)
  base: '/',
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8000',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
        manualChunks(id) {
          if (id.includes('node_modules/react-dom') || id.includes('node_modules/react/')) {
            return 'vendor-react'
          }
          if (id.includes('node_modules/@tanstack/')) {
            return 'vendor-tanstack'
          }
          if (id.includes('node_modules/@base-ui/')) {
            return 'vendor-base-ui'
          }
          if (id.includes('node_modules/lucide-react/')) {
            return 'vendor-icons'
          }
          if (id.includes('node_modules/recharts/')) {
            return 'vendor-charts'
          }
          if (id.includes('node_modules/js-yaml/')) {
            return 'vendor-yaml'
          }
          if (id.includes('node_modules/date-fns/')) {
            return 'vendor-date'
          }
          if (id.includes('node_modules/zustand/')) {
            return 'vendor-state'
          }
          if (id.includes('node_modules/zod/')) {
            return 'vendor-zod'
          }
        },
      },
    },
    // Target modern browsers for smaller output
    target: 'es2020',
    // Don't ship sourcemaps in production
    sourcemap: false,
    // Enable CSS code splitting
    cssCodeSplit: true,
  },
})
