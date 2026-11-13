// Mock data and formatting helpers for the FabriBay prototype.
// Every top-level name is prefixed (FB / fb) because the Babel scripts share one global scope.

let FB_LANG = 'zh';

// t picks the string for the current language; copy lives inline at the call site.
function t(zh, en) { return FB_LANG === 'en' ? en : zh; }

// fbAgo formats a duration in minutes as a short span ("4 分钟" / "4 min").
function fbAgo(min) {
  if (min < 1) return t('刚刚', 'just now');
  if (min < 60) return min + t(' 分钟', ' min');
  if (min < 1440) return Math.round(min / 60) + t(' 小时', ' h');
  return Math.round(min / 1440) + t(' 天', ' d');
}

// fbSince is fbAgo with the "ago" suffix, for created / finished columns.
function fbSince(min) { return min < 1 ? fbAgo(min) : fbAgo(min) + t('前', ' ago'); }

// fbSize formats megabytes, switching to GB above 1024.
function fbSize(mb) {
  if (mb == null) return '—';
  if (mb >= 1024) return (mb / 1024).toFixed(1) + ' GB';
  return (mb < 10 ? mb.toFixed(1) : Math.round(mb)) + ' MB';
}

// fbStatus is the container status sentence shown in tables and the dock.
function fbStatus(c) {
  if (c.state === 'running') return c.since < 1 ? t('刚刚启动', 'Just started') : t('已运行 ', 'Up ') + fbAgo(c.since);
  if (c.state === 'starting') return t('启动中…', 'Starting…');
  if (c.state === 'stopping') return t('停止中…', 'Stopping…');
  return t('退出', 'Exited') + ' (' + c.exit + ') · ' + fbSince(c.since);
}

// fbHash gives a small stable number from a string, used to derive fake details.
function fbHash(s) { let h = 7; for (const ch of s) h = (h * 31 + ch.charCodeAt(0)) % 100003; return h; }

// fbId derives a 12-char hex id from a seed.
function fbId(seed) { let h = fbHash(seed), out = ''; for (let i = 0; i < 12; i++) { h = (h * 1103 + 12345) % 65521; out += (h % 16).toString(16); } return out; }

// fbFlat makes a quiet CPU history around a base level.
function fbFlat(base) { return Array.from({ length: 24 }, (_, i) => Math.max(0.1, base * (0.7 + ((i * 7) % 5) / 8))); }

// fbContainer fills in the derived fields of a container record.
function fbContainer(o) {
  const running = o.state === 'running';
  return { id: fbId(o.name), project: null, exit: 0, ports: [], env: [], mounts: [], network: 'bridge', cmd: '', cpu: 0, mem: 0, ...o,
    cpu: running ? o.cpu : 0, spark: running ? fbFlat(o.cpu) : [], ip: '172.18.0.' + (2 + fbHash(o.name) % 200) };
}

const FB_CONTAINERS = [
  { name: 'shop-web-1', project: 'shop', image: 'nginx:1.27-alpine', state: 'running', since: 4, created: 4320, ports: [[8080, 80]], cpu: 0.4, mem: 18, cmd: 'nginx -g "daemon off;"', network: 'shop_default', mounts: ['./web/dist:/usr/share/nginx/html:ro'] },
  { name: 'shop-api-1', project: 'shop', image: 'shop/api:dev', state: 'running', since: 4, created: 4320, ports: [[3000, 3000]], cpu: 12.6, mem: 212, cmd: 'node dist/server.js', network: 'shop_default', env: ['NODE_ENV=development', 'DATABASE_URL=postgres://shop@shop-db-1/shop', 'REDIS_URL=redis://redis-cache:6379'], mounts: ['./api:/app'] },
  { name: 'shop-db-1', project: 'shop', image: 'postgres:17', state: 'running', since: 4, created: 4320, ports: [[5432, 5432]], cpu: 1.8, mem: 96, cmd: 'postgres', network: 'shop_default', env: ['POSTGRES_USER=shop', 'POSTGRES_DB=shop'], mounts: ['shop_pgdata:/var/lib/postgresql/data'] },
  { name: 'grafana', project: 'observability', image: 'grafana/grafana:11.3', state: 'running', since: 120, created: 10080, ports: [[3001, 3000]], cpu: 0.9, mem: 142, cmd: '/run.sh', network: 'observability_default', mounts: ['grafana_data:/var/lib/grafana'] },
  { name: 'prometheus', project: 'observability', image: 'prom/prometheus:v3.0', state: 'running', since: 120, created: 10080, ports: [[9090, 9090]], cpu: 2.3, mem: 188, cmd: '/bin/prometheus --config.file=/etc/prometheus/prometheus.yml', network: 'observability_default', mounts: ['prom_data:/prometheus'] },
  { name: 'loki', project: 'observability', image: 'grafana/loki:3.2', state: 'exited', exit: 1, since: 6, created: 10080, ports: [[3100, 3100]], cmd: '/usr/bin/loki -config.file=/etc/loki/local-config.yaml', network: 'observability_default', mounts: ['loki_data:/loki'] },
  { name: 'redis-cache', image: 'redis:7.4-alpine', state: 'running', since: 7200, created: 7200, ports: [[6379, 6379]], cpu: 0.2, mem: 9, cmd: 'redis-server' },
  { name: 'mailpit', image: 'axllent/mailpit:v1.21', state: 'running', since: 1440, created: 20160, ports: [[8025, 8025], [1025, 1025]], cpu: 0.1, mem: 24, cmd: '/mailpit' },
  { name: 'minio', image: 'minio/minio:latest', state: 'running', since: 1440, created: 20160, ports: [[9000, 9000], [9001, 9001]], cpu: 0.6, mem: 131, cmd: 'minio server /data --console-address :9001', mounts: ['minio_data:/data'] },
  { name: 'db-migrate', image: 'shop/migrate:dev', state: 'exited', exit: 0, since: 2880, created: 2880, cmd: 'migrate up' },
  { name: 'pg-backup', image: 'postgres:17', state: 'exited', exit: 0, since: 1500, created: 43200, cmd: 'pg_dump -Fc shop', mounts: ['backups:/backups'] },
].map(fbContainer);

const FB_IMAGES = [
  ['nginx', '1.27-alpine', 48, 12960], ['shop/api', 'dev', 412, 240], ['shop/migrate', 'dev', 96, 2880], ['postgres', '17', 438, 20160],
  ['grafana/grafana', '11.3', 486, 30240], ['prom/prometheus', 'v3.0', 292, 30240], ['grafana/loki', '3.2', 118, 30240],
  ['redis', '7.4-alpine', 41, 40320], ['axllent/mailpit', 'v1.21', 29, 40320], ['minio/minio', 'latest', 176, 21600],
  ['node', '22-bookworm-slim', 228, 50400], ['golang', '1.27-alpine', 262, 8640], ['alpine', '3.21', 8.3, 43200],
  ['debian', 'trixie', 121, 60480], ['shop/web', 'dev', 62, 300], ['python', '3.13-slim', 132, 86400],
].map(([repo, tag, size, created]) => ({ id: fbId(repo + tag), repo, tag, size, created, arch: repo === 'minio/minio' ? 'amd64' : 'arm64', scan: null }));
// Two images arrive already scanned so the security column shows both outcomes.
FB_IMAGES[3].scan = { c: 0, h: 2, m: 7, l: 14 };
FB_IMAGES[12].scan = { c: 0, h: 0, m: 0, l: 0 };

const FB_VOLUMES = [
  ['shop_pgdata', 412, 4320], ['grafana_data', 38, 10080], ['prom_data', 644, 10080], ['loki_data', 91, 10080],
  ['minio_data', 702, 20160], ['backups', 286, 43200], ['buildx_cache', 1320, 30240], ['go-mod-cache', 512, 8640], ['scratch', 0.4, 60480],
].map(([name, size, created]) => ({ name, size, created, driver: 'local' }));

const FB_NETWORKS = [
  { name: 'bridge', driver: 'bridge', subnet: '172.17.0.0/16', gateway: '172.17.0.1', builtin: true, created: 86400 },
  { name: 'host', driver: 'host', subnet: '', gateway: '', builtin: true, created: 86400 },
  { name: 'none', driver: 'null', subnet: '', gateway: '', builtin: true, created: 86400 },
  { name: 'shop_default', driver: 'bridge', subnet: '172.18.0.0/16', gateway: '172.18.0.1', created: 4320 },
  { name: 'observability_default', driver: 'bridge', subnet: '172.19.0.0/16', gateway: '172.19.0.1', created: 10080 },
];

const FB_STEPS = [
  ['[internal] load build definition from Dockerfile', 0.1, false], ['[internal] load metadata for docker.io/library/node:22-bookworm-slim', 0.8, false],
  ['[deps 1/3] FROM node:22-bookworm-slim', 0, true], ['[deps 2/3] COPY package.json pnpm-lock.yaml ./', 0.1, true],
  ['[deps 3/3] RUN pnpm install --frozen-lockfile', 14.2, true], ['[build 1/2] COPY . .', 0.4, false],
  ['[build 2/2] RUN pnpm build', 9.6, false], ['[runtime 1/2] COPY --from=build /app/dist ./dist', 0.3, false],
  ['exporting to image', 1.2, false],
];

const FB_BUILDS = [
  ['shop/api:dev', '~/code/shop/api', 'running', 0, 0], ['shop/web:dev', '~/code/shop/web', 'ok', 18.4, 300],
  ['shop/api:dev', '~/code/shop/api', 'ok', 26.7, 240], ['shop/api:dev', '~/code/shop/api', 'failed', 11.2, 262],
  ['shop/migrate:dev', '~/code/shop/migrate', 'ok', 7.9, 2880], ['fabribay/guest:test', '~/code/fabribay', 'ok', 44.1, 4000],
  ['shop/web:dev', '~/code/shop/web', 'ok', 2.1, 4400], ['shop/api:dev', '~/code/shop/api', 'ok', 31.5, 5800],
].map(([name, context, status, dur, at], i) => ({ id: fbId(name + i), name, context, status, dur, at, platform: 'linux/arm64', done: status === 'running' ? 5 : FB_STEPS.length }));

const FB_LOG_POOL = [
  ['INFO', 'server listening on :3000'], ['INFO', 'connected to postgres shop-db-1:5432'], ['GET', '/api/products 200 12ms'],
  ['GET', '/api/cart 200 4ms'], ['WARN', 'slow query 412ms SELECT * FROM orders WHERE user_id = $1'], ['POST', '/api/checkout 201 88ms'],
  ['INFO', 'order 48213 queued for fulfilment'], ['GET', '/api/products?page=2 200 9ms'], ['GET', '/healthz 200 1ms'],
  ['WARN', 'redis-cache latency 38ms'], ['GET', '/api/orders/48213 200 6ms'], ['INFO', 'cache refreshed, 214 keys'],
];
const FB_LOG_FAIL = [['INFO', 'starting loki 3.2'], ['INFO', 'reading /etc/loki/local-config.yaml'], ['ERROR', 'mkdir /loki/chunks: permission denied'], ['ERROR', 'error initialising module: store']];

// fbLogs builds the initial log buffer of a container; fbLogLine appends one more line.
function fbLogs(c) {
  const pool = c.exit ? FB_LOG_FAIL : FB_LOG_POOL;
  return pool.slice(0, c.exit ? 4 : 9).map(([lv, m], i) => ({ t: fbClock(i * 7), lv, m }));
}
function fbLogLine(n) { const [lv, m] = FB_LOG_POOL[2 + (n * 5) % 10]; return { t: fbClock(70 + n * 3), lv, m }; }
function fbClock(sec) { const s = 10 * 3600 + 42 * 60 + 7 + sec; const p = (v) => String(v).padStart(2, '0'); return p(Math.floor(s / 3600) % 24) + ':' + p(Math.floor(s / 60) % 60) + ':' + p(s % 60); }

// fbLayers derives a plausible layer stack for an image; sizes sum to the image size.
function fbLayers(img) {
  const cmds = ['ADD rootfs.tar.xz /', 'RUN apk add --no-cache ca-certificates tzdata', 'ENV PATH=/usr/local/bin:$PATH', 'RUN set -eux; addgroup -S app; adduser -S app -G app',
    'COPY --from=build /out /usr/local', 'WORKDIR /app', 'COPY . .', 'EXPOSE 8080', 'ENTRYPOINT ["docker-entrypoint.sh"]', 'CMD ["serve"]'];
  const weights = [30, 12, 0, 1, 38, 0, 19, 0, 0, 0];
  return cmds.map((cmd, i) => ({ id: fbId(img.id + i), cmd, size: Math.round(img.size * weights[i]) / 100 }));
}

// fbVulns expands a scan summary into individual findings.
function fbVulns(img) {
  if (!img.scan) return [];
  const pk = ['openssl', 'libxml2', 'zlib', 'curl', 'busybox', 'glibc', 'expat', 'libpng', 'ncurses', 'sqlite'];
  const out = [];
  [['c', 'Critical'], ['h', 'High'], ['m', 'Medium'], ['l', 'Low']].forEach(([k, sev]) => {
    for (let i = 0; i < img.scan[k]; i++) {
      const h = fbHash(img.id + k + i);
      out.push({ id: 'CVE-202' + (4 + h % 3) + '-' + (10000 + h % 39999), sev, pkg: pk[h % pk.length], ver: '3.' + (h % 9) + '.' + (h % 14), fix: h % 4 ? '3.' + (h % 9 + 1) + '.0' : '' });
    }
  });
  return out;
}

// A small filesystem shared by the container and volume file browsers: directories are objects, files are [bytes, minutes since change].
const FB_FS = {
  app: { dist: { 'server.js': [48211, 240], 'routes.js': [9120, 240], 'db.js': [3310, 240] }, node_modules: {}, 'package.json': [1204, 4320], 'pnpm-lock.yaml': [88410, 4320], '.env': [212, 4320] },
  etc: { 'hosts': [174, 4], 'resolv.conf': [38, 4], 'passwd': [1172, 43200], ssl: { 'cert.pem': [221300, 43200] } },
  var: { log: { 'app.log': [18220, 1] }, lib: {} },
  usr: { local: { bin: { 'node': [98211000, 50400], 'docker-entrypoint.sh': [388, 50400] } } },
  tmp: {},
};

const FB_EVENTS = [
  ['start', 'shop-web-1', 4], ['start', 'shop-api-1', 4], ['start', 'shop-db-1', 4], ['die', 'loki', 6],
  ['build', 'shop/api:dev', 240], ['pull', 'golang:1.27-alpine', 8640], ['create', 'backups', 43200],
].map(([kind, name, at], i) => ({ id: i, kind, name, at }));

Object.assign(window, { t, fbAgo, fbSince, fbSize, fbStatus, fbHash, fbId, fbFlat, fbContainer, fbLogs, fbLogLine, fbLayers, fbVulns });
