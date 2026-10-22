/** Mainline UI + explicit real remote backend; no response mocks.
 * E2E standard: test/e2e/browser-e2e-testing-standard.md
 * node test/e2e/remote_ui_proxy.mjs <private-config.json> [port]
 */
import { createServer } from '../../ui/node_modules/vite/dist/node/index.js';
import react from '../../ui/node_modules/@vitejs/plugin-react/dist/index.js';
import { readFileSync } from 'node:fs';
const config = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const target = new URL(config.base_url);
if (!['http:', 'https:'].includes(target.protocol) || ['localhost', '127.0.0.1'].includes(target.hostname)
    || target.username || target.password) throw new Error('Explicit remote HTTP backend required');
const port = Number(process.argv[3] || 8802);
// Preserve the browser's Host and Origin together so the real server's CSRF
// check remains enabled. Rewriting Host alone correctly produces a 403.
const proxy = { target: target.origin, ws: true, changeOrigin: false };
const server = await createServer({ configFile: false,
  root: new URL('../../ui', import.meta.url).pathname, plugins: [react()],
  server: { host: '127.0.0.1', port, strictPort: true,
    proxy: { '^/api/': proxy, '^/v1/': proxy, '^/healthz$': proxy } },
});
await server.listen();
console.log(`Mainline UI: http://127.0.0.1:${port}; real remote backend proxy enabled`);
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, async () => {
  await server.close(); process.exit(0);
});
