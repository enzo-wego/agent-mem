import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { defineConfig, mergeConfig } from 'vite'
import base from '../vite.config.ts'

const fixture = readFileSync(fileURLToPath(new URL('./fixtures/resolve-links.json', import.meta.url)), 'utf8')

// Mock API for the real-page Search check: no worker, no proxy.
export default mergeConfig(
  base,
  defineConfig({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: {
      port: 5199,
      strictPort: true,
      proxy: {},
    },
    plugins: [
      {
        name: 'mock-api',
        configureServer(server) {
          server.middlewares.use((req, res, next) => {
            const url = req.url ?? ''
            if (!url.startsWith('/api/')) return next()
            res.setHeader('Content-Type', 'application/json')
            res.end(url.startsWith('/api/graph/resolve') && req.method === 'POST' ? fixture : url.startsWith('/api/projects') ? '[]' : '{}')
          })
        },
      },
    ],
  }),
)
