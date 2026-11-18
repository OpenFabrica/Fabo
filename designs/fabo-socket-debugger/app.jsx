// Socket 调试设计稿：一个 Fabo 窗口，五个场景可切换（客户端 / 服务端 / HTTP Mock / 新建会话 / 从容器进入）
const { useState, useEffect, useRef } = React;

const PROTO_CLASS = { TCP: "", UDP: "udp", WS: "ws", HTTP: "http" };

/* 把带 \r\n 转义的文本转成字节数组，16 进制视图和字节数都基于它 */
function toBytes(s) {
  const raw = s.replace(/\\r/g, "\r").replace(/\\n/g, "\n").replace(/\\t/g, "\t");
  return Array.from(new TextEncoder().encode(raw));
}

/* 当前时间，格式与日志一致（时:分:秒.毫秒） */
function nowTs() {
  const d = new Date();
  const p = (n, w = 2) => String(n).padStart(w, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

/* 客户端会话的模拟回包：Redis 协议给像样的回复，其它原样回显 */
function fakeReply(sid, text) {
  if (sid === "redis") {
    const cmd = text.split(/\s|\\r/)[0].toUpperCase();
    if (cmd === "PING") return "+PONG\\r\\n";
    if (cmd === "DBSIZE") return ":184\\r\\n";
    if (cmd === "GET") return "$6\\r\\ncart=3\\r\\n";
    return "+OK\\r\\n";
  }
  if (sid === "ws") return "{\"type\":\"ack\"}";
  return null;
}

/* ── 窗口外壳 ── */
function TitleBar({ placeholder }) {
  return (
    <div className="titlebar">
      <div className="lights"><i></i><i></i><i></i></div>
      <Icon name="sidebar" size={16} style={{ color: "var(--sub)" }} />
      <div className="brand"><span className="mark"></span>Fabo</div>
      <div className="gsearch"><Icon name="search" size={14} /><span>{placeholder}</span><kbd>⌘K</kbd></div>
      <button className="tico"><Icon name="terminal" /></button>
      <button className="tico"><Icon name="gear" /></button>
    </div>
  );
}

/* 侧栏：在「共享」下面新增「调试」一项；有活动会话时显示绿点 */
function Sidebar({ page, go, live }) {
  const items = [
    ["overview", "grid", "概览"], ["containers", "box", "容器", 5], ["images", "layers", "镜像", 3],
    ["volumes", "db", "卷", 2], ["networks", "net", "网络", 4], ["builds", "hammer", "构建"], ["machines", "cpu", "Linux 机器", 2],
  ];
  const tools = [["pods", "hex", "Pods"], ["svc", "plug", "Services"], ["usb", "usb", "USB 设备"], ["share", "globe", "共享"]];
  const nav = ([id, icon, label, cnt]) => (
    <button key={id} className={"nav" + (page === id ? " on" : "")} onClick={() => go(id)}>
      <Icon name={icon} size={17} />{label}{cnt != null && <span className="cnt">{cnt}</span>}
    </button>
  );
  return (
    <div className="side">
      {items.map(nav)}
      <div className="group">KUBERNETES <span style={{ marginLeft: "auto", fontWeight: 400 }}>● 已关闭</span></div>
      {tools.map(nav)}
      <button className={"nav" + (page === "debug" ? " on" : "")} onClick={() => go("debug")}>
        <Icon name="pulse" size={17} />调试{live > 0 && <span className="live" title={`${live} 个会话活动中`}></span>}
      </button>
      <div className="vm">
        <div className="row" style={{ fontWeight: 600 }}><span style={{ color: "var(--text)" }}><span style={{ color: "var(--ok)" }}>●</span> Linux 虚拟机</span><span>运行中</span></div>
        <div className="row"><span>CPU</span><span>10% · 4</span></div>
        <div className="row"><span>内存</span><span>1016 MB / 8.0 GB</span></div>
      </div>
    </div>
  );
}

/* ── 会话列表 ── */
function SessionList({ sessions, cur, pick }) {
  const group = (kind, title) => (
    <>
      <h3>{title}</h3>
      {sessions.filter(s => s.kind === kind).map(s => (
        <button key={s.id} className={"sess" + (cur === s.id ? " on" : "")} onClick={() => pick(s.id)}>
          <span className={"proto " + PROTO_CLASS[s.proto]}>{s.proto}{s.state !== "idle" && <i className={"st " + (s.state === "ok" ? "ok" : "off")}></i>}</span>
          <span className="t">
            <div className="n">{s.name}</div>
            <div className={"a mono" + (s.kind === "server" ? " plain" : "")}>
              {s.kind === "server" ? (s.state === "ok" ? `监听 ${s.addr}${s.clients ? ` · ${s.clients} 个${s.proto === "UDP" ? "来源" : "连接"}` : ""}` : `${s.addr} · 未在监听`) : s.addr}
            </div>
          </span>
        </button>
      ))}
    </>
  );
  return <div className="sessions">{group("client", "客户端")}{group("server", "服务端")}</div>;
}

/* 一行日志；hex 模式下展示 16 进制 + ASCII 两栏 */
function LogLine({ l, hex, showSrc }) {
  const arrow = l.d === "out" ? "→" : l.d === "in" ? "←" : "•";
  let body = l.b;
  if (hex && l.d !== "sys") {
    const bytes = toBytes(l.b);
    const h = bytes.map(b => b.toString(16).padStart(2, "0")).join(" ");
    const a = bytes.map(b => (b >= 32 && b < 127 ? String.fromCharCode(b) : ".")).join("");
    body = <span className="hex"><span className="h">{h}</span><span className="asc">{a}</span></span>;
  }
  return (
    <div className={"ln " + l.d}>
      <span className="ts">{l.t}</span>
      <span className="dir">{arrow}</span>
      <span className="body2">{showSrc && l.src && <span className="src">{l.src}</span>}{body}{l.auto && <span className="tag">自动回复</span>}</span>
      <span className="sz">{l.n ? `${l.n} B` : ""}</span>
    </div>
  );
}

/* 日志区：自动滚到底，筛选按关键字 */
function LogPane({ lines, hex, filter, showSrc, empty }) {
  const ref = useRef(null);
  useEffect(() => { if (ref.current) ref.current.scrollTop = ref.current.scrollHeight; }, [lines.length]);
  const shown = filter ? lines.filter(l => (l.b + (l.src || "")).toLowerCase().includes(filter.toLowerCase())) : lines;
  return (
    <div className="log" ref={ref}>
      {shown.length === 0 && <div className="ln sys"><span></span><span></span><span className="body2">{empty}</span></div>}
      {shown.map((l, i) => <LogLine key={i} l={l} hex={hex} showSrc={showSrc} />)}
    </div>
  );
}

/* 日志上方的工具条：显示方式、编码、查找、导出、清空 */
function LogBar({ hex, setHex, filter, setFilter, clear, extra }) {
  return (
    <div className="bar">
      <div className="seg">
        <button className={!hex ? "on" : ""} onClick={() => setHex(false)}>文本</button>
        <button className={hex ? "on" : ""} onClick={() => setHex(true)}>16 进制</button>
      </div>
      <span className="field" style={{ width: 92 }}>UTF-8<Icon name="chev" size={13} style={{ marginLeft: "auto" }} /></span>
      {extra}
      <span className="field" style={{ width: 200, marginLeft: "auto" }}>
        <Icon name="search" size={13} /><input placeholder="在消息中查找" value={filter} onChange={e => setFilter(e.target.value)} />
      </span>
      <button className="ibtn" title="导出日志"><Icon name="download" /></button>
      <button className="ibtn" title="清空消息" onClick={clear}><Icon name="trash" /></button>
    </div>
  );
}

/* 输入区：快捷输入、文本 / 16 进制、定时重复发送；⌘↩ 发送 */
function Composer({ sid, onSend, disabled, target }) {
  const [text, setText] = useState("");
  const [mode, setMode] = useState("text");
  const [repeat, setRepeat] = useState(false);
  const [every, setEvery] = useState("1");
  const [clearAfter, setClearAfter] = useState(true);
  const timer = useRef(null);
  const send = (v = text) => {
    if (!v.trim() || disabled) return;
    onSend(v, mode);
    if (clearAfter && !repeat) setText("");
  };
  // 定时发送：开启后按间隔重复发送当前内容，关闭或断开即停止
  useEffect(() => {
    clearInterval(timer.current);
    if (repeat && !disabled && text.trim()) timer.current = setInterval(() => onSend(text, mode), Math.max(0.2, parseFloat(every) || 1) * 1000);
    return () => clearInterval(timer.current);
  }, [repeat, disabled, every, text, mode]);
  useEffect(() => { setRepeat(false); setText(""); }, [sid]);
  return (
    <div className="composer">
      <div className="top">
        <div className="seg">
          <button className={mode === "text" ? "on" : ""} onClick={() => setMode("text")}>文本</button>
          <button className={mode === "hex" ? "on" : ""} onClick={() => setMode("hex")}>16 进制</button>
        </div>
        {(QUICK[sid] || []).map(q => <button key={q} className="qk" title={q} onClick={() => send(q)}>{q}</button>)}
        <button className="qk add">＋ 快捷输入</button>
      </div>
      <textarea value={text} disabled={disabled} onChange={e => setText(e.target.value)}
        placeholder={disabled ? "连接后可发送" : mode === "hex" ? "输入 16 进制，如 50 49 4e 47 0d 0a" : "输入要发送的内容，\\r\\n 会按转义发送"}
        onKeyDown={e => { if (e.key === "Enter" && e.metaKey) { e.preventDefault(); send(); } }}></textarea>
      <div className="bot">
        <span className={"check" + (repeat ? " on" : "")} onClick={() => setRepeat(!repeat)}>
          <i>{repeat && <Icon name="check" size={11} sw={2.4} />}</i>定时发送
        </span>
        <span className="check" style={{ gap: 6 }}>每<input className="mini mono" value={every} onChange={e => setEvery(e.target.value)} />秒</span>
        <span className={"check" + (clearAfter ? " on" : "")} onClick={() => setClearAfter(!clearAfter)}>
          <i>{clearAfter && <Icon name="check" size={11} sw={2.4} />}</i>发送后清空
        </span>
        <span className="sp"></span>
        {target && <span className="kbd">{target}</span>}
        <span className="kbd">⌘↩</span>
        <button className="btn primary sm" disabled={disabled} style={{ opacity: disabled ? .5 : 1 }} onClick={() => send()}>
          {repeat ? <><Icon name="repeat" size={14} />发送中…</> : <><Icon name="send" size={14} />发送</>}
        </button>
      </div>
    </div>
  );
}

/* ── 客户端工作区 ── */
function ClientWork({ s, lines, push, toggle, clear, openEntry }) {
  const [hex, setHex] = useState(false);
  const [filter, setFilter] = useState("");
  const on = s.state === "ok";
  const onSend = (v, mode) => {
    push(s.id, { t: nowTs(), d: "out", b: v, n: mode === "hex" ? v.trim().split(/\s+/).length : toBytes(v).length });
    const r = fakeReply(s.id, v);
    if (r) setTimeout(() => push(s.id, { t: nowTs(), d: "in", b: r, n: toBytes(r).length }), 180);
  };
  return (
    <div className="work">
      <div className="whead">
        <span className="name">{s.name}</span>
        {on ? <span className="chip ok">已连接 {s.since}</span> : <span className="chip off">未连接</span>}
        <span className="chip link mono"><Icon name="link" size={12} />{s.addr}</span>
        {s.container && <button className="chip" onClick={openEntry} title="在容器页打开"><Icon name="container" size={12} />{s.container}</button>}
        <span className="sp"></span>
        {on ? <button className="btn danger sm" onClick={toggle}>断开</button> : <button className="btn primary sm" onClick={toggle}>连接</button>}
        <button className="ibtn"><Icon name="more" /></button>
      </div>
      <LogBar hex={hex} setHex={setHex} filter={filter} setFilter={setFilter} clear={clear} />
      <LogPane lines={lines} hex={hex} filter={filter} empty={on ? "还没有消息" : "未连接"} />
      <Composer sid={s.id} onSend={onSend} disabled={!on} />
    </div>
  );
}

/* 本机地址 / 容器内地址：容器里访问宿主要用 host.docker.internal */
function Addrs({ scheme, port, all }) {
  const [copied, setCopied] = useState(null);
  const copy = k => { setCopied(k); setTimeout(() => setCopied(null), 1200); };
  const row = (k, label, v) => (
    <div className="addr">
      <span className="k">{label}</span><span className="v mono">{v}</span>
      <button className="ibtn" style={{ width: 24, height: 24 }} onClick={() => copy(k)} title="复制">
        <Icon name={copied === k ? "check" : "copy"} size={13} />
      </button>
    </div>
  );
  return (
    <div className="addrs">
      {row("host", all ? "局域网" : "本机", `${scheme}${all ? "192.168.1.23" : "127.0.0.1"}:${port}`)}
      {row("ctr", "容器内", `${scheme}host.docker.internal:${port}`)}
    </div>
  );
}

/* ── WebSocket / TCP 服务端工作区 ── */
function ServerWork({ s, lines, push, toggle, clear }) {
  // UDP 没有连接，「来源」是最近发来数据的地址，回复按选中的来源发送
  const [hex, setHex] = useState(false);
  const [filter, setFilter] = useState("");
  const [clients, setClients] = useState(s.id === "wss" ? WS_CLIENTS : s.id === "udpsrv" ? UDP_PEERS : []);
  const [more, setMore] = useState(new URLSearchParams(location.search).get("more") === "1");
  const [autoListen, setAutoListen] = useState(false);
  const [auto, setAuto] = useState(s.proto !== "UDP");
  const on = s.state === "ok";
  const port = s.addr.replace(":", "");
  const sel = clients.filter(c => c.sel && !c.gone);
  const onSend = (v) => {
    const src = sel.length === 1 ? sel[0].name : `群发 · ${sel.length}`;
    push(s.id, { t: nowTs(), d: "out", b: v, src, n: toBytes(v).length });
  };
  return (
    <div className="work" style={{ position: "relative" }}>
      <div className="whead">
        <span className="name">{s.name}</span>
        {on ? <span className="chip ok">监听中 {s.since}</span> : <span className="chip off">未在监听</span>}
        <span className="chip mono">{s.proto === "WS" ? "WebSocket" : s.proto} {s.bind === "all" ? "0.0.0.0" : "127.0.0.1"}{s.addr}</span>
        <span className="sp"></span>
        {on ? <button className="btn danger sm" onClick={toggle}>停止</button> : <button className="btn primary sm" onClick={toggle}>开始监听</button>}
        <button className="ibtn" onClick={() => setMore(!more)}><Icon name="more" /></button>
        {more && (
          <div className="menu" style={{ right: 20, top: 52 }} onMouseLeave={() => setMore(false)}>
            <div onClick={() => setAutoListen(!autoListen)}><span className={"check" + (autoListen ? " on" : "")}><i>{autoListen && <Icon name="check" size={11} sw={2.4} />}</i></span>启动 Fabo 时自动监听</div>
            <div><Icon name="copy" size={14} />复制为新会话</div>
            <hr />
            <div style={{ color: "var(--err)" }}><Icon name="trash" size={14} />删除会话</div>
          </div>
        )}
      </div>
      <Addrs scheme={s.proto === "WS" ? "ws://" : s.proto === "UDP" ? "udp://" : ""} port={port} all={s.bind === "all"} />
      <LogBar hex={hex} setHex={setHex} filter={filter} setFilter={setFilter} clear={clear} />
      <div className="srvbody">
        <div className="clients">
          <div className="ch">{s.proto === "UDP" ? "来源" : "连接"} {clients.filter(c => !c.gone).length}
            <button className="ibtn" style={{ marginLeft: "auto", width: 24, height: 24 }} title="清空已断开" onClick={() => setClients(clients.filter(c => !c.gone))}><Icon name="trash" size={13} /></button>
          </div>
          <div style={{ flex: 1, overflow: "auto" }}>
            {clients.length === 0 && <div style={{ padding: 14, color: "var(--muted)", fontSize: 12.5 }}>{on ? (s.proto === "UDP" ? "等待数据" : "等待连接") : "未在监听"}</div>}
            {clients.map((c, i) => (
              <div key={i} className={"cl" + (c.gone ? " gone" : "")}
                onClick={() => !c.gone && setClients(clients.map((x, j) => j === i ? { ...x, sel: !x.sel } : x))}>
                <span className={"check" + (c.sel && !c.gone ? " on" : "")}><i>{c.sel && !c.gone && <Icon name="check" size={11} sw={2.4} />}</i></span>
                <span style={{ minWidth: 0 }}>
                  <div className="n">{c.name}</div>
                  <div className="ip mono">{c.ip}</div>
                </span>
                <span style={{ marginLeft: "auto", fontSize: 11.5, color: "var(--muted)" }}>{c.since}</span>
              </div>
            ))}
          </div>
          <div className="autoreply">
            <button className={"switch" + (auto ? " on" : "")} onClick={() => setAuto(!auto)}></button>
            <span>自动回复</span>
            {auto && <span className="mono" style={{ marginLeft: "auto", fontSize: 11.5, color: "var(--muted)" }}>{"{\"ok\":true}"}</span>}
          </div>
        </div>
        <div style={{ flex: 1, minWidth: 0, display: "flex", flexDirection: "column" }}>
          <LogPane lines={lines} hex={hex} filter={filter} showSrc empty={on ? "等待消息" : "未在监听"} />
        </div>
      </div>
      <Composer sid={s.id} onSend={onSend} disabled={!on || sel.length === 0}
        target={sel.length === 0 ? "未选连接" : sel.length === 1 ? `发给 ${sel[0].name}` : `群发 ${sel.length} 个连接`} />
    </div>
  );
}

/* ── HTTP 客户端：地址栏 + 请求 / 响应左右两栏 + 历史 ── */
function HttpClientWork({ s }) {
  const H = HTTP_CLIENT;
  const [reqTab, setReqTab] = useState("body");
  const [resTab, setResTab] = useState("body");
  const [method, setMethod] = useState(H.method);
  const [busy, setBusy] = useState(false);
  const send = () => { setBusy(true); setTimeout(() => setBusy(false), 450); };
  const cls = c => "code c" + String(c)[0];
  return (
    <div className="work">
      <div className="whead">
        <span className="name">{s.name}</span>
        <button className="chip" title="在容器页打开"><Icon name="container" size={12} />{s.container}</button>
        <span className="sp"></span>
        <button className="ibtn"><Icon name="more" /></button>
      </div>
      <div className="urlbar">
        <span className={"meth big " + method} onClick={() => setMethod(method === "POST" ? "GET" : method === "GET" ? "DELETE" : "POST")}>{method}<Icon name="chev" size={12} /></span>
        <input className="mono" defaultValue={H.url} />
        <button className="btn primary sm" onClick={send}>{busy ? "请求中…" : <><Icon name="send" size={14} />发送</>}</button>
      </div>
      <div className="hsplit">
        <div className="hpane">
          <div className="htabs">
            {[["params", "参数"], ["headers", `Headers ${H.headers.length}`], ["body", "Body"]].map(([k, l]) =>
              <button key={k} className={reqTab === k ? "on" : ""} onClick={() => setReqTab(k)}>{l}</button>)}
            {reqTab === "body" && <span className="field" style={{ marginLeft: "auto", height: 24, width: 84 }}>JSON<Icon name="chev" size={12} style={{ marginLeft: "auto" }} /></span>}
          </div>
          {reqTab === "body" && <textarea className="code-edit mono" defaultValue={H.body}></textarea>}
          {reqTab === "headers" && <div className="kvlist">{H.headers.map(([k, v]) => <div key={k}><span className="mono">{k}</span><span className="mono">{v}</span></div>)}<div><span className="mono" style={{ color: "var(--muted)" }}>添加 Header</span><span></span></div></div>}
          {reqTab === "params" && <div className="kvlist"><div><span className="mono" style={{ color: "var(--muted)" }}>添加参数</span><span></span></div></div>}
        </div>
        <div className="hpane">
          <div className="htabs">
            {[["body", "响应"], ["headers", `Headers ${H.resHeaders.length}`]].map(([k, l]) =>
              <button key={k} className={resTab === k ? "on" : ""} onClick={() => setResTab(k)}>{l}</button>)}
            <span style={{ marginLeft: "auto", display: "flex", gap: 10, alignItems: "center", fontSize: 12, color: "var(--sub)" }}>
              <span className={cls(H.status)}>{H.status} Created</span><span>{H.ms} ms</span><span>{H.size}</span>
            </span>
          </div>
          {resTab === "body"
            ? <pre className="log resbody" style={{ opacity: busy ? .45 : 1 }}>{H.resBody}</pre>
            : <div className="kvlist">{H.resHeaders.map(([k, v]) => <div key={k}><span className="mono">{k}</span><span className="mono">{v}</span></div>)}</div>}
        </div>
      </div>
      <div className="sectitle" style={{ paddingTop: 4 }}>历史</div>
      <div className="hist">
        {H.history.map(([m, p, c, t], i) => (
          <div key={i}><span className={"meth " + m}>{m}</span><span className="mono">{p}</span><span className={cls(c)}>{c}</span><span className="muted">{t}</span></div>
        ))}
      </div>
    </div>
  );
}

/* ── HTTP 服务端（Mock）：路由表 + 请求记录 ── */
function HttpWork({ s }) {
  const [sel, setSel] = useState(0);
  const r = ROUTES[sel];
  const cls = c => "code c" + String(c)[0];
  return (
    <div className="work" style={{ overflow: "auto" }}>
      <div className="whead">
        <span className="name">{s.name}</span>
        <span className="chip ok">监听中 {s.since}</span>
        <span className="chip mono">HTTP {s.addr}</span>
        <span className="sp"></span>
        <button className="btn danger sm">停止</button>
        <button className="ibtn"><Icon name="more" /></button>
      </div>
      <Addrs scheme="http://" port="7070" />
      <div className="sectitle">路由 <span className="muted">{ROUTES.length}</span>
        <button className="btn soft sm" style={{ marginLeft: "auto" }}><Icon name="plus" size={14} />添加路由</button>
      </div>
      <div className="routes">
        <table className="tbl">
          <thead><tr><th style={{ width: 80 }}>方法</th><th>路径</th><th style={{ width: 70 }}>状态码</th><th>响应</th><th style={{ width: 60, textAlign: "right" }}>命中</th></tr></thead>
          <tbody>
            {ROUTES.map((x, i) => (
              <tr key={i} className={sel === i ? "sel" : ""} onClick={() => setSel(i)} style={{ cursor: "pointer" }}>
                <td><span className={"meth " + x.m}>{x.m}</span></td>
                <td className="mono">{x.path}</td>
                <td><span className={cls(x.code)}>{x.code}</span></td>
                <td className="mono" style={{ color: "var(--sub)", fontSize: 12, maxWidth: 300, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                  {x.delay && <span className="chip" style={{ height: 20, marginRight: 6 }}>延迟 {x.delay}</span>}{x.body || "—"}
                </td>
                <td style={{ textAlign: "right", color: "var(--sub)" }}>{x.hits}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="sectitle">请求 <span className="muted">最近 {HTTP_REQS.length} 条</span></div>
      <div className="log" style={{ flex: "none", marginBottom: 16 }}>
        {HTTP_REQS.map((q, i) => (
          <div key={i} className="ln in" style={{ gridTemplateColumns: "70px 60px 1fr 120px 50px 70px" }}>
            <span className="ts">{q.t}</span>
            <span style={{ color: "#C9D1DC" }}>{q.m}</span>
            <span>{q.path}</span>
            <span className="src">{q.src}</span>
            <span style={{ color: q.code < 300 ? "#4CC38A" : q.code < 500 ? "#E3B341" : "#F0756B" }}>{q.code}</span>
            <span className="sz" style={{ textAlign: "right" }}>{q.ms} ms</span>
          </div>
        ))}
      </div>
    </div>
  );
}

/* ── 新建会话弹窗：类型 + 目标（容器端口优先）── */
function NewSession({ close, create, preset }) {
  const [kind, setKind] = useState(preset?.kind || "client");
  const [proto, setProto] = useState(preset?.proto || "TCP");
  const [target, setTarget] = useState(preset?.target || "shop-db-1:5432");
  const [via, setVia] = useState("net");
  const [tab, setTab] = useState("ctr");
  const [port, setPort] = useState("9002");
  const [bindAll, setBindAll] = useState(0);
  // 端口列表按协议过滤：UDP 只列 UDP 端口，其余列 TCP 端口
  const wantUdp = proto === "UDP";
  const ports = CONTAINERS.flatMap(c => c.ports.filter(p => (p.proto === "udp") === wantUdp).map(p => ({ c, p, key: `${c.name}:${p.p}` })));
  const pick = ports.find(x => x.key === target) || ports[0];
  const viaOk = pick && (via === "host" ? pick.p.host : true);
  return (
    <div className="scrim" onMouseDown={e => e.target === e.currentTarget && close()}>
      <div className="modal" data-screen-label="新建会话">
        <div className="mh"><h2>新建会话</h2><button className="ibtn" style={{ marginLeft: "auto" }} onClick={close}><Icon name="close" /></button></div>
        <div className="mb">
          <div>
            <div className="types">
              {["client", "server"].map(k => (
                <React.Fragment key={k}>
                  <span className="trow">{k === "client" ? "客户端" : "服务端"}</span>
                  {["TCP", "UDP", "WS", "HTTP"].map(p => (
                    <button key={k + p} className={"type" + (kind === k && proto === p ? " on" : "")} onClick={() => { setKind(k); setProto(p); }}>
                      <b>{p === "WS" ? "WebSocket" : p === "HTTP" ? (k === "client" ? "HTTP" : "HTTP Mock") : p}</b>
                    </button>
                  ))}
                </React.Fragment>
              ))}
            </div>
          </div>
          {kind === "client" ? (
            <>
              <div>
                <div className="lbl">目标
                  <span className="seg" style={{ marginLeft: "auto" }}>
                    <button className={tab === "ctr" ? "on" : ""} onClick={() => setTab("ctr")}>容器</button>
                    <button className={tab === "addr" ? "on" : ""} onClick={() => setTab("addr")}>地址</button>
                  </span>
                </div>
                {tab === "ctr" ? (
                  <div className="ports">
                    {ports.map(x => (
                      <div key={x.key} className={"port" + (pick && pick.key === x.key ? " on" : "")} onClick={() => setTarget(x.key)}>
                        <span className={"radio" + (pick && pick.key === x.key ? " on" : "")}></span>
                        <span className="ci"><Icon name="container" size={15} /></span>
                        <span><div className="n">{x.c.name}</div><div className="d mono">{x.c.domain}</div></span>
                        <span className="pp mono">{x.p.p}/{x.p.proto}{x.p.host ? ` → localhost:${x.p.host}` : "  未映射"}</span>
                      </div>
                    ))}
                  </div>
                ) : (
                  <input className="input mono" defaultValue={proto === "WS" ? "ws://127.0.0.1:8080" : "127.0.0.1:8080"} />
                )}
              </div>
              {tab === "ctr" && pick && (
                <div>
                  <div className="lbl">连接方式</div>
                  <div className="via">
                    <button className={via === "net" ? "on" : ""} onClick={() => setVia("net")}>
                      <span className={"radio" + (via === "net" ? " on" : "")}></span>容器网络<span className="mono">{pick.c.ip}:{pick.p.p}</span>
                    </button>
                    <button className={via === "host" ? "on" : ""} disabled={!pick.p.host} style={{ opacity: pick.p.host ? 1 : .45 }} onClick={() => setVia("host")}>
                      <span className={"radio" + (via === "host" ? " on" : "")}></span>映射端口<span className="mono">{pick.p.host ? `localhost:${pick.p.host}` : "未映射"}</span>
                    </button>
                  </div>
                </div>
              )}
              {proto === "WS" && <div className="row2"><div><div className="lbl">路径</div><input className="input mono" defaultValue="/live" /></div><div><div className="lbl">子协议</div><input className="input mono" placeholder="可选" /></div></div>}
            </>
          ) : (
            <div className="row2">
              <div><div className="lbl">端口</div><input className="input mono" value={port} onChange={e => setPort(e.target.value)} /></div>
              <div><div className="lbl">名称</div><input className="input" placeholder={`${proto === "WS" ? "WebSocket" : proto} :${port}`} /></div>
              <div><div className="lbl">监听</div>
                <span className="seg"><button className={bindAll === 0 ? "on" : ""} onClick={() => setBindAll(0)}>仅本机</button><button className={bindAll === 1 ? "on" : ""} onClick={() => setBindAll(1)}>所有网卡</button></span>
              </div>
              {proto !== "HTTP" && <div><div className="lbl">初始消息</div><input className="input mono" placeholder="连接时发送，可留空" /></div>}
              {proto === "WS" && <span className="check" style={{ gridColumn: "1 / -1" }}><i></i>启用 TLS（wss://，自签名证书，可换成自己的证书）</span>}
              <div style={{ gridColumn: "1 / -1", fontSize: 12.5, color: "var(--sub)" }} className="mono">容器内 → host.docker.internal:{port}</div>
              <span className="check" style={{ gridColumn: "1 / -1" }}><i></i>启动 Fabo 时自动监听</span>
            </div>
          )}
        </div>
        <div className="mf">
          <button className="btn soft" onClick={close}>取消</button>
          <button className="btn primary" disabled={kind === "client" && tab === "ctr" && !viaOk} onClick={() => create(kind, proto, pick, via, port)}>
            {kind === "client" ? "连接" : "开始监听"}
          </button>
        </div>
      </div>
    </div>
  );
}

/* ── 入口二：容器详情坞的端口行，点「调试」直接带着目标打开新建会话 ── */
function ContainersEntry({ openNew }) {
  const [menu, setMenu] = useState(true);
  const rows = [["shop-api-1", "api.shop.fabo.local"], ["shop-web-1", "web.shop.fabo.local"], ["shop-db-1", "db.shop.fabo.local"], ["redis-cache", "redis-cache.fabo.local"]];
  return (
    <div className="main" style={{ flexDirection: "row" }}>
      <div className="ctable" style={{ display: "flex", flexDirection: "column" }}>
        <div className="phead"><h1>容器</h1><span className="meta">4 个运行中</span></div>
        <div style={{ borderTop: "1px solid var(--line)" }}>
          {rows.map(([n, d]) => (
            <div key={n} className={"crow" + (n === "shop-db-1" ? " on" : "")}>
              <span className="cico"><Icon name="container" size={15} /></span>
              <span><div className="n">{n}</div><div className="d mono">{d}</div></span>
            </div>
          ))}
        </div>
      </div>
      <div className="dock" style={{ position: "relative" }}>
        <div className="whead" style={{ paddingTop: 18 }}><span className="name">shop-db-1</span><span className="chip ok">运行 4 分钟</span></div>
        <div className="tabs"><span className="on">信息</span><span>日志</span><span>终端</span><span>文件</span><span>设置</span></div>
        <div className="kv">
          <span>镜像</span><span className="mono">postgres:16-alpine</span>
          <span>IP</span><span className="mono">172.18.0.2</span>
          <span>网络</span><span>shop_default</span>
          <span>端口</span>
          <span className="pline"><span className="mono">5432/tcp</span><span style={{ color: "var(--muted)" }}>未映射</span>
            <button className="ibtn on" style={{ marginLeft: "auto" }} title="调试此端口" onClick={() => setMenu(!menu)}><Icon name="pulse" size={15} /></button>
          </span>
        </div>
        {menu && (
          <div className="menu" style={{ right: 20, top: 214 }}>
            <div className="hl" onClick={() => openNew({ kind: "client", proto: "TCP", target: "shop-db-1:5432" })}><Icon name="plug" size={14} />TCP 连接 5432<span className="k">容器网络</span></div>
            <div onClick={() => openNew({ kind: "client", proto: "TCP", target: "shop-db-1:5432" })}><Icon name="pulse" size={14} />新建调试会话…</div>
            <hr />
            <div onClick={() => setMenu(false)}><Icon name="copy" size={14} />复制地址<span className="k mono">172.18.0.2:5432</span></div>
          </div>
        )}
      </div>
    </div>
  );
}

/* ── 根组件：场景切换、会话状态、日志 ── */
function App() {
  // 地址栏 ?scene=&dark=1 优先，便于直接分享某个场景
  const q = new URLSearchParams(location.search);
  const [dark, setDark] = useState(q.has("dark") ? q.get("dark") === "1" : localStorage.getItem("sd-dark") === "1");
  const [scene, setScene] = useState(q.get("scene") || localStorage.getItem("sd-scene") || "client");
  const [sessions, setSessions] = useState(SESSIONS);
  const [cur, setCur] = useState("redis");
  const [logs, setLogs] = useState(LOGS);
  const [modal, setModal] = useState(null);
  const [toast, setToast] = useState(null);
  const pending = useRef(null);

  useEffect(() => { document.body.classList.toggle("dark", dark); localStorage.setItem("sd-dark", dark ? "1" : "0"); }, [dark]);
  // 场景按钮只是设计稿导航：把窗口切到对应页面和会话
  useEffect(() => {
    localStorage.setItem("sd-scene", scene);
    if (pending.current) { setCur(pending.current); pending.current = null; setModal(null); return; }
    if (scene === "client") { setCur("redis"); setModal(null); }
    if (scene === "server") { setCur("wss"); setModal(null); }
    if (scene === "http") { setCur("mock"); setModal(null); }
    if (scene === "httpc") { setCur("orders"); setModal(null); }
    if (scene === "udp") { setCur("udpsrv"); setModal(null); }
    if (scene === "new") { const pr = new URLSearchParams(location.search).get("proto") || "TCP"; setModal({ kind: "client", proto: pr, target: pr === "UDP" ? "statsd:8125" : "shop-db-1:5432" }); }
    if (scene === "newsrv") setModal({ kind: "server", proto: "UDP" });
    if (scene === "entry") setModal(null);
  }, [scene]);

  const s = sessions.find(x => x.id === cur);
  const push = (id, line) => setLogs(L => ({ ...L, [id]: [...(L[id] || []), line] }));
  const toggle = () => {
    const on = s.state !== "ok";
    setSessions(ss => ss.map(x => x.id === cur ? { ...x, state: on ? "ok" : "off", since: "刚刚" } : x));
    push(cur, { t: nowTs(), d: "sys", b: s.kind === "server" ? (on ? `开始监听 ${s.bind === "all" ? "0.0.0.0" : "127.0.0.1"}${s.addr}` : "已停止监听") : (on ? `已连接 ${s.addr}` : "已断开") });
  };
  const create = (kind, proto, pick, via, port) => {
    const id = "n" + Date.now();
    const addr = kind === "server" ? `:${port}` : pick ? (via === "host" ? `localhost:${pick.p.host}` : `${pick.c.name}:${pick.p.p}`) : "127.0.0.1:8080";
    const name = kind === "server" ? `${proto === "WS" ? "WebSocket" : proto} :${port}` : pick ? pick.c.name : addr;
    setSessions(ss => [...ss, { id, kind, proto, name, addr, container: pick && kind === "client" ? pick.c.name : null, state: "ok", since: "刚刚", clients: kind === "server" ? 0 : undefined }]);
    setLogs(L => ({ ...L, [id]: [{ t: nowTs(), d: "sys", b: kind === "server" ? `开始监听 127.0.0.1:${port}` : `已连接 ${addr}${via === "net" && pick ? `（${pick.c.ip}，容器网络）` : ""}` }] }));
    setCur(id); setModal(null);
    // 从「新建会话」或「从容器进入」场景创建后回到调试页，并保持选中新会话
    if (scene === "new" || scene === "newsrv" || scene === "entry") { pending.current = id; setScene(kind === "server" ? "server" : "client"); }
  };
  const page = scene === "entry" ? "containers" : "debug";
  const live = sessions.filter(x => x.state === "ok").length;
  const scenes = [["client", "TCP 客户端"], ["httpc", "HTTP 客户端"], ["server", "WebSocket 服务端"], ["udp", "UDP 服务端"], ["http", "HTTP Mock"], ["new", "新建会话"], ["newsrv", "新建服务端"], ["entry", "从容器进入"]];

  return (
    <div className="stage">
      <div className="switcher">
        {scenes.map(([k, l]) => <button key={k} className={scene === k ? "on" : ""} onClick={() => setScene(k)}>{l}</button>)}
        <span className="sep"></span>
        <button onClick={() => setDark(!dark)}>{dark ? "浅色" : "深色"}</button>
      </div>
      <div className={"win" + (dark ? " dark" : "")} data-screen-label={scenes.find(x => x[0] === scene)[1]}>
        <TitleBar placeholder={page === "debug" ? "搜索会话..." : "搜索容器..."} />
        <div className="body">
          <Sidebar page={page} go={id => id === "debug" ? setScene("client") : id === "containers" && setScene("entry")} live={live} />
          {page === "containers" ? (
            <ContainersEntry openNew={preset => setModal(preset)} />
          ) : (
            <div className="main">
              <div className="phead">
                <h1>调试</h1><span className="meta">{live} 个会话活动中</span>
                <button className="btn primary" style={{ marginLeft: "auto" }} onClick={() => setModal({})}><Icon name="plus" size={15} />新建会话</button>
              </div>
              <div className="split">
                <SessionList sessions={sessions} cur={cur} pick={setCur} />
                {s.kind === "client" && s.proto === "HTTP"
                  ? <HttpClientWork key={s.id} s={s} />
                  : s.kind === "client"
                  ? <ClientWork key={s.id} s={s} lines={logs[s.id] || []} push={push} toggle={toggle} clear={() => setLogs(L => ({ ...L, [s.id]: [] }))} openEntry={() => setScene("entry")} />
                  : s.proto === "HTTP"
                    ? <HttpWork key={s.id} s={s} />
                    : <ServerWork key={s.id} s={s} lines={logs[s.id] || []} push={push} toggle={toggle} clear={() => setLogs(L => ({ ...L, [s.id]: [] }))} />}
              </div>
            </div>
          )}
          {modal && <NewSession preset={modal} close={() => { setModal(null); if (scene === "new" || scene === "newsrv") setScene("client"); }} create={create} />}
          {toast && <div className="toast">{toast}</div>}
        </div>
      </div>
      <div className="note">
        设计稿可交互：点会话切换、⌘↩ 发送、勾选「定时发送」、切 16 进制、服务端勾选连接后群发、容器详情里点端口旁的调试图标。
      </div>
    </div>
  );
}

ReactDOM.createRoot(document.getElementById("root")).render(<App />);
