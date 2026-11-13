// Shared mock data, icons and small primitives for the three direction studies.

// Compose projects first, then standalone containers (compose: false).
const FB_GROUPS = [
  { id: 'shop', name: 'shop', compose: true, items: [
    { name: 'shop-web-1', image: 'nginx:1.27-alpine', state: 'running', status: '已运行 4 分钟', ports: '8080→80', cpu: 0.4, mem: '18 MB', created: '3 天前', spark: [2, 3, 2, 4, 3, 2, 3, 5, 3, 2, 2, 3] },
    { name: 'shop-api-1', image: 'shop/api:dev', state: 'running', status: '已运行 4 分钟', ports: '3000→3000', cpu: 12.6, mem: '212 MB', created: '3 天前', spark: [4, 6, 9, 7, 12, 10, 8, 13, 11, 9, 12, 10] },
    { name: 'shop-db-1', image: 'postgres:17', state: 'running', status: '已运行 4 分钟', ports: '5432→5432', cpu: 1.8, mem: '96 MB', created: '3 天前', spark: [3, 3, 4, 3, 5, 4, 3, 3, 4, 6, 4, 3] },
  ] },
  { id: 'observability', name: 'observability', compose: true, items: [
    { name: 'grafana', image: 'grafana/grafana:11.3', state: 'running', status: '已运行 2 小时', ports: '3001→3000', cpu: 0.9, mem: '142 MB', created: '上周', spark: [2, 2, 3, 2, 2, 4, 3, 2, 2, 3, 2, 2] },
    { name: 'prometheus', image: 'prom/prometheus:v3.0', state: 'running', status: '已运行 2 小时', ports: '9090→9090', cpu: 2.3, mem: '188 MB', created: '上周', spark: [4, 5, 4, 6, 5, 4, 5, 7, 5, 4, 5, 4] },
    { name: 'loki', image: 'grafana/loki:3.2', state: 'failed', status: '退出 (1) · 6 分钟前', ports: '', cpu: 0, mem: '—', created: '上周', spark: [] },
  ] },
  { id: 'standalone', name: '', compose: false, items: [
    { name: 'redis-cache', image: 'redis:7.4-alpine', state: 'running', status: '已运行 5 天', ports: '6379→6379', cpu: 0.2, mem: '9 MB', created: '5 天前', spark: [1, 1, 2, 1, 1, 1, 2, 1, 1, 1, 1, 2] },
    { name: 'mailpit', image: 'axllent/mailpit:v1.21', state: 'running', status: '已运行 1 天', ports: '8025→8025', cpu: 0.1, mem: '24 MB', created: '2 周前', spark: [1, 1, 1, 2, 1, 1, 1, 1, 2, 1, 1, 1] },
    { name: 'minio', image: 'minio/minio:latest', state: 'running', status: '已运行 1 天', ports: '9000→9000', cpu: 0.6, mem: '131 MB', created: '2 周前', spark: [2, 2, 3, 2, 3, 2, 2, 2, 3, 2, 2, 2] },
    { name: 'db-migrate', image: 'shop/migrate:dev', state: 'exited', status: '退出 (0) · 2 天前', ports: '', cpu: 0, mem: '—', created: '2 天前', spark: [] },
    { name: 'pg-backup', image: 'postgres:17', state: 'exited', status: '退出 (0) · 昨天', ports: '', cpu: 0, mem: '—', created: '上个月', spark: [] },
  ] },
];

const FB_FILTERS = [['全部', 11], ['运行中', 8], ['已停止', 2], ['异常', 1]];

const FB_NAV = [
  { key: 'containers', label: '容器', icon: 'box', count: 11 },
  { key: 'images', label: '镜像', icon: 'layers', count: 32 },
  { key: 'volumes', label: '卷', icon: 'cylinder', count: 9 },
  { key: 'networks', label: '网络', icon: 'network', count: 5 },
  { key: 'builds', label: '构建', icon: 'hammer', count: 14 },
];

const FB_LOGS = [
  ['10:42:07', 'INFO', 'server listening on :3000'],
  ['10:42:07', 'INFO', 'connected to postgres shop-db-1:5432'],
  ['10:42:31', 'GET', '/api/products 200 12ms'],
  ['10:42:33', 'GET', '/api/cart 200 4ms'],
  ['10:43:02', 'WARN', 'slow query 412ms SELECT * FROM orders WHERE user_id = $1'],
  ['10:43:10', 'POST', '/api/checkout 201 88ms'],
  ['10:43:11', 'INFO', 'order 48213 queued for fulfilment'],
  ['10:43:40', 'GET', '/api/products?page=2 200 9ms'],
  ['10:44:02', 'GET', '/healthz 200 1ms'],
  ['10:44:18', 'WARN', 'redis-cache latency 38ms'],
  ['10:44:32', 'GET', '/api/orders/48213 200 6ms'],
];

// Line icons on a 24 grid; stroke comes from currentColor.
const FB_ICONS = {
  grid: <g><rect x="4" y="4" width="7" height="7" rx="1.5" /><rect x="13" y="4" width="7" height="7" rx="1.5" /><rect x="4" y="13" width="7" height="7" rx="1.5" /><rect x="13" y="13" width="7" height="7" rx="1.5" /></g>,
  box: <path d="M12 3l8 4.5v9L12 21l-8-4.5v-9L12 3zM4 7.5l8 4.5 8-4.5M12 12v9" />,
  layers: <path d="M12 4l9 5-9 5-9-5 9-5zM3 14l9 5 9-5" />,
  cylinder: <path d="M5 6c0-1.7 3.1-3 7-3s7 1.3 7 3v12c0 1.7-3.1 3-7 3s-7-1.3-7-3V6zM5 6c0 1.7 3.1 3 7 3s7-1.3 7-3M5 12c0 1.7 3.1 3 7 3s7-1.3 7-3" />,
  network: <g><rect x="9" y="3" width="6" height="5" rx="1" /><rect x="3" y="16" width="6" height="5" rx="1" /><rect x="15" y="16" width="6" height="5" rx="1" /><path d="M12 8v4M6 16v-4h12v4" /></g>,
  hammer: <path d="M13.5 4.5l6 6-2.5 2.5-6-6zM12.5 9L4 17.5 6.5 20 15 11.5" />,
  sliders: <g><path d="M4 7h9M19 7h1M4 17h1M11 17h9" /><circle cx="16" cy="7" r="2.5" /><circle cx="8" cy="17" r="2.5" /></g>,
  helm: <g><circle cx="12" cy="12" r="8" /><circle cx="12" cy="12" r="2.5" /><path d="M12 4v5.5M12 14.5V20M4 12h5.5M14.5 12H20M6.3 6.3l3.9 3.9M13.8 13.8l3.9 3.9M17.7 6.3l-3.9 3.9M10.2 13.8l-3.9 3.9" /></g>,
  usb: <g><path d="M12 3v14M9 6l3-3 3 3M8 9v3l4 3M16 8v3l-4 3" /><circle cx="12" cy="19" r="2" /></g>,
  share: <path d="M12 15V4M8 8l4-4 4 4M5 13v6h14v-6" />,
  play: <path d="M8 5.5l10 6.5-10 6.5z" />,
  stop: <rect x="7" y="7" width="10" height="10" rx="1.5" />,
  restart: <path d="M19 12a7 7 0 11-2.6-5.4M19 4v4.5h-4.5" />,
  trash: <path d="M5 7h14M9 7V4h6v3M7 7l1 13h8l1-13" />,
  more: <g fill="currentColor" stroke="none"><circle cx="6" cy="12" r="1.5" /><circle cx="12" cy="12" r="1.5" /><circle cx="18" cy="12" r="1.5" /></g>,
  search: <g><circle cx="11" cy="11" r="6" /><path d="M20 20l-4.5-4.5" /></g>,
  terminal: <path d="M5 7l5 5-5 5M12 17h7" />,
  chevron: <path d="M9 6l6 6-6 6" />,
  down: <path d="M6 9l6 6 6-6" />,
  plus: <path d="M12 5v14M5 12h14" />,
  out: <path d="M10 5H5v14h14v-5M14 5h5v5M19 5l-8 8" />,
  stack: <path d="M4 7h16M4 12h16M4 17h16" />,
};

// Icon renders one of FB_ICONS at the given pixel size.
function FBIcon({ name, size = 16, sw = 1.7 }) {
  return (
    <svg className="fb-icon" width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor"
      strokeWidth={sw} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{FB_ICONS[name]}</svg>
  );
}

// FBMark draws the FabriBay mark: a cobalt bay holding an orange container.
function FBMark({ size = 20, bay = '#1438D6', box = '#FF6A1F' }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" aria-hidden="true">
      <path d="M3 4h5v12h8V4h5v14l-3 3H6l-3-3z" fill={bay} />
      <path d="M9.5 7.5h3.5l1.5 1.5v5.5h-5z" fill={box} />
    </svg>
  );
}

// FBLights draws the macOS window buttons.
function FBLights() {
  return <div className="fb-lights"><i style={{ background: '#FF5F57' }} /><i style={{ background: '#FEBC2E' }} /><i style={{ background: '#28C840' }} /></div>;
}

// FBSpark draws a tiny CPU history as bars; empty history renders a flat rule.
function FBSpark({ values, height = 14 }) {
  if (!values.length) return <span className="fb-spark fb-spark-off" style={{ height }} />;
  const max = 14;
  return (
    <span className="fb-spark" style={{ height }}>
      {values.map((v, i) => <i key={i} style={{ height: Math.max(2, Math.round((v / max) * height)) }} />)}
    </span>
  );
}

Object.assign(window, { FB_GROUPS, FB_FILTERS, FB_NAV, FB_LOGS, FBIcon, FBMark, FBLights, FBSpark });
