import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { fileURLToPath, URL } from 'node:url';
import { defineConfig, searchForWorkspaceRoot, type Plugin } from 'vite';
import vue from '@vitejs/plugin-vue';
import tailwindcss from '@tailwindcss/vite';

const webRoot = fileURLToPath(new URL('./', import.meta.url));

// In mock mode (`npm run dev:mock`) the MSW worker script is served straight from the installed
// msw package, so it is never copied into public/ and never ends up in the production build.
function mockServiceWorker(): Plugin {
  const workerPath = createRequire(import.meta.url).resolve(
    'msw/mockServiceWorker.js',
  );
  return {
    name: 'factory:msw-worker',
    apply: 'serve',
    configureServer(server) {
      server.middlewares.use('/mockServiceWorker.js', (_req, res, next) => {
        readFile(workerPath).then((body) => {
          res.setHeader('Content-Type', 'text/javascript');
          res.setHeader('Cache-Control', 'no-cache');
          res.end(body);
        }, next);
      });
    },
  };
}

// https://vite.dev/config/
export default defineConfig(({ mode }) => ({
  base: '/',
  plugins: [vue(), tailwindcss(), mode === 'mock' && mockServiceWorker()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // Embedded by `go build -tags webui` (internal/webui/embed_dist.go); gitignored.
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
  },
  server: {
    fs: {
      // The MSW handlers import the contract's examples from ../api/examples.
      allow: [
        searchForWorkspaceRoot(webRoot),
        fileURLToPath(new URL('../api/examples', import.meta.url)),
      ],
    },
    // `npm run dev` talks to a local dispatcher (`factory serve`, --addr :8080).
    proxy:
      mode === 'mock'
        ? undefined
        : {
            '/api': process.env.FACTORY_API_URL ?? 'http://127.0.0.1:8080',
          },
  },
}));
