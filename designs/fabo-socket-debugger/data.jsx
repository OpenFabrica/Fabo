// 模拟数据：容器、会话、消息。容器沿用 TestScreenshots 里的 shop 项目，保证与现有截图一致
const CONTAINERS = [
  { name: "shop-api-1", domain: "api.shop.fabo.local", ip: "172.18.0.3", ports: [{ p: 3000, host: 3000, proto: "tcp" }] },
  { name: "shop-web-1", domain: "web.shop.fabo.local", ip: "172.18.0.4", ports: [{ p: 80, host: 8080, proto: "tcp" }, { p: 8081, host: 8081, proto: "tcp", ws: true }] },
  { name: "shop-db-1", domain: "db.shop.fabo.local", ip: "172.18.0.2", ports: [{ p: 5432, host: null, proto: "tcp" }] },
  { name: "redis-cache", domain: "redis-cache.fabo.local", ip: "172.17.0.2", ports: [{ p: 6379, host: 6379, proto: "tcp" }] },
  { name: "statsd", domain: "statsd.fabo.local", ip: "172.17.0.5", ports: [{ p: 8125, host: 8125, proto: "udp" }] },
];

// kind: client / server；proto: TCP / UDP / WS / HTTP
const SESSIONS = [
  { id: "redis", kind: "client", proto: "TCP", name: "redis-cache", addr: "redis-cache:6379", container: "redis-cache", state: "ok", since: "2 分钟" },
  { id: "ws", kind: "client", proto: "WS", name: "shop-web 推送", addr: "ws://shop-web-1:8081/live", container: "shop-web-1", state: "ok", since: "14 秒" },
  { id: "orders", kind: "client", proto: "HTTP", name: "订单 API", addr: "http://api.shop.fabo.local/v1/orders", container: "shop-api-1", state: "idle" },
  { id: "statsd", kind: "client", proto: "UDP", name: "statsd", addr: "statsd:8125", container: "statsd", state: "off" },
  { id: "wss", kind: "server", proto: "WS", name: "回调接收", addr: ":9001", state: "ok", since: "6 分钟", clients: 3 },
  { id: "mock", kind: "server", proto: "HTTP", name: "支付网关 Mock", addr: ":7070", state: "ok", since: "1 小时" },
  { id: "tcpsrv", kind: "server", proto: "TCP", name: "设备上报", addr: ":5555", state: "off" },
  { id: "udpsrv", kind: "server", proto: "UDP", name: "日志收集", addr: ":5514", bind: "all", state: "ok", since: "20 分钟", clients: 2 },
];

// 每条消息：t 时间，d 方向（out 发出 / in 收到 / sys 系统），b 内容，src 来源（服务端会话）
const LOGS = {
  redis: [
    { t: "14:02:11.204", d: "sys", b: "已连接 redis-cache:6379（172.17.0.2，容器网络）" },
    { t: "14:02:13.880", d: "out", b: "PING\\r\\n", n: 6 },
    { t: "14:02:13.882", d: "in", b: "+PONG\\r\\n", n: 7 },
    { t: "14:02:20.415", d: "out", b: "SET session:42 \"cart=3\"\\r\\n", n: 25 },
    { t: "14:02:20.417", d: "in", b: "+OK\\r\\n", n: 5 },
    { t: "14:02:26.003", d: "out", b: "GET session:42\\r\\n", n: 16 },
    { t: "14:02:26.004", d: "in", b: "$6\\r\\ncart=3\\r\\n", n: 12 },
    { t: "14:02:31.771", d: "out", b: "INFO keyspace\\r\\n", n: 15 },
    { t: "14:02:31.773", d: "in", b: "$47\\r\\n# Keyspace\\r\\ndb0:keys=184,expires=12,avg_ttl=86012\\r\\n", n: 54 },
  ],
  ws: [
    { t: "14:03:40.112", d: "sys", b: "WebSocket 握手完成 · 101 Switching Protocols" },
    { t: "14:03:40.130", d: "in", b: "{\"type\":\"hello\",\"server\":\"shop-web\",\"v\":2}", n: 40 },
    { t: "14:03:41.006", d: "out", b: "{\"type\":\"subscribe\",\"topic\":\"orders\"}", n: 37 },
    { t: "14:03:41.018", d: "in", b: "{\"type\":\"ack\",\"topic\":\"orders\"}", n: 31 },
    { t: "14:03:47.552", d: "in", b: "{\"type\":\"order\",\"id\":\"A-1093\",\"total\":259.00,\"items\":3}", n: 55 },
    { t: "14:03:52.901", d: "in", b: "{\"type\":\"order\",\"id\":\"A-1094\",\"total\":42.50,\"items\":1}", n: 53 },
  ],
  wss: [
    { t: "14:00:02.551", d: "sys", b: "开始监听 127.0.0.1:9001" },
    { t: "14:00:09.300", d: "sys", b: "shop-api-1 已连接", src: "172.18.0.3:40512" },
    { t: "14:00:09.318", d: "in", b: "{\"event\":\"payment.created\",\"id\":\"pay_81\"}", src: "shop-api-1", n: 40 },
    { t: "14:00:09.319", d: "out", b: "{\"ok\":true}", src: "shop-api-1", n: 11, auto: true },
    { t: "14:01:15.022", d: "sys", b: "shop-web-1 已连接", src: "172.18.0.4:51880" },
    { t: "14:01:22.418", d: "in", b: "{\"event\":\"page.view\",\"path\":\"/cart\"}", src: "shop-web-1", n: 36 },
    { t: "14:01:22.419", d: "out", b: "{\"ok\":true}", src: "shop-web-1", n: 11, auto: true },
    { t: "14:02:40.991", d: "sys", b: "本机 已连接", src: "127.0.0.1:63012" },
    { t: "14:04:01.004", d: "out", b: "{\"event\":\"config.reload\"}", src: "群发 · 3", n: 25 },
  ],
};

const WS_CLIENTS = [
  { name: "shop-api-1", ip: "172.18.0.3:40512", since: "6 分钟", sel: true },
  { name: "shop-web-1", ip: "172.18.0.4:51880", since: "4 分钟", sel: true },
  { name: "本机", ip: "127.0.0.1:63012", since: "2 分钟", sel: true },
  { name: "shop-api-1", ip: "172.18.0.3:40388", since: "已断开", gone: true },
];

const ROUTES = [
  { m: "POST", path: "/v1/charges", code: 201, ct: "application/json", body: "{\"id\":\"ch_{{rand}}\",\"status\":\"succeeded\"}", hits: 18 },
  { m: "GET", path: "/v1/charges/:id", code: 200, ct: "application/json", body: "{\"id\":\"{{path.id}}\",\"status\":\"succeeded\"}", hits: 7 },
  { m: "POST", path: "/v1/refunds", code: 503, ct: "application/json", body: "{\"error\":\"gateway_timeout\"}", delay: "5000 ms", hits: 2 },
  { m: "DELETE", path: "/v1/charges/:id", code: 204, ct: "—", body: "", hits: 0 },
  { m: "ANY", path: "/static/*", code: 200, ct: "目录", body: "~/Mock/payments", hits: 31 },
];

const HTTP_REQS = [
  { t: "14:05:12", m: "POST", path: "/v1/charges", src: "shop-api-1", code: 201, ms: 1 },
  { t: "14:05:14", m: "GET", path: "/v1/charges/ch_9f2c", src: "shop-api-1", code: 200, ms: 1 },
  { t: "14:05:30", m: "POST", path: "/v1/refunds", src: "shop-api-1", code: 503, ms: 5002 },
  { t: "14:05:41", m: "GET", path: "/v1/customers", src: "本机", code: 404, ms: 0 },
];

// HTTP 客户端的一次请求与响应
const HTTP_CLIENT = {
  method: "POST", url: "http://api.shop.fabo.local/v1/orders",
  headers: [["Content-Type", "application/json"], ["Authorization", "Bearer dev-token"], ["X-Request-Id", "{{uuid}}"]],
  body: "{\n  \"items\": [{ \"sku\": \"TEE-01\", \"qty\": 2 }],\n  \"coupon\": \"WELCOME\"\n}",
  status: 201, ms: 38, size: "412 B",
  resHeaders: [["content-type", "application/json; charset=utf-8"], ["x-powered-by", "Express"], ["date", "Tue, 17 Nov 2026 06:08:12 GMT"]],
  resBody: "{\n  \"id\": \"A-1095\",\n  \"status\": \"pending\",\n  \"total\": 118.00,\n  \"items\": [\n    { \"sku\": \"TEE-01\", \"qty\": 2, \"price\": 59.00 }\n  ]\n}",
  history: [["POST", "/v1/orders", 201, "38 ms"], ["GET", "/v1/orders/A-1094", 200, "12 ms"], ["GET", "/v1/orders?limit=20", 200, "21 ms"], ["DELETE", "/v1/orders/A-1090", 409, "9 ms"]],
};

const UDP_LOG = [
  { t: "13:48:10.002", d: "sys", b: "开始监听 0.0.0.0:5514（所有网卡）" },
  { t: "14:06:01.311", d: "in", b: "<134>shop-api: GET /api/products 200 12ms", src: "shop-api-1", n: 41 },
  { t: "14:06:01.840", d: "in", b: "<131>shop-web: upstream timeout /cart", src: "shop-web-1", n: 37 },
  { t: "14:06:02.007", d: "in", b: "<134>shop-api: POST /api/checkout 201 88ms", src: "shop-api-1", n: 43 },
];
const UDP_PEERS = [
  { name: "shop-api-1", ip: "172.18.0.3:47011", since: "刚刚", sel: true },
  { name: "shop-web-1", ip: "172.18.0.4:38820", since: "1 秒前", sel: false },
];

const QUICK = {
  redis: ["PING\\r\\n", "INFO keyspace\\r\\n", "GET session:42\\r\\n", "DBSIZE\\r\\n"],
  ws: ["{\"type\":\"subscribe\",\"topic\":\"orders\"}", "{\"type\":\"ping\"}"],
  wss: ["{\"event\":\"config.reload\"}", "{\"ok\":true}"],
  statsd: ["shop.checkout:1|c", "shop.latency:38|ms"],
  tcpsrv: ["ACK\\n"],
};

LOGS.udpsrv = UDP_LOG;
Object.assign(window, { CONTAINERS, SESSIONS, LOGS, WS_CLIENTS, ROUTES, HTTP_REQS, QUICK, HTTP_CLIENT, UDP_PEERS });
