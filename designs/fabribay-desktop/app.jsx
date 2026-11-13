// App shell and the single store every page reads and mutates.

const FB_PAGES = [
  ['overview', 'grid', '概览', 'Overview'], ['containers', 'box', '容器', 'Containers'], ['images', 'layers', '镜像', 'Images'],
  ['volumes', 'cylinder', '卷', 'Volumes'], ['networks', 'network', '网络', 'Networks'], ['builds', 'hammer', '构建', 'Builds'],
];
const FB_SOON = [['kubernetes', 'helm', 'Kubernetes', 'Kubernetes'], ['usb', 'usb', 'USB 设备', 'USB devices'], ['share', 'share', '共享', 'Share']];
const FB_DEFAULT_TAB = { containers: 'logs', images: 'layers', volumes: 'info', networks: 'info', builds: 'steps' };
const FB_PREFS = { theme: 'dark', density: 'compact', accent: 'orange', lang: 'zh', navCollapsed: false };

// fbLoadPrefs reads saved tweaks; ?theme= / ?lang= override them so a link can pin a look.
function fbLoadPrefs() {
  let p = {};
  try { p = JSON.parse(localStorage.getItem('fb-prefs') || '{}'); } catch (e) {}
  const q = new URLSearchParams(location.search);
  return { ...FB_PREFS, ...p, ...(q.get('theme') ? { theme: q.get('theme') } : {}), ...(q.get('lang') ? { lang: q.get('lang') } : {}) };
}

// fbPageFromHash maps the URL hash to a known page, defaulting to containers.
function fbPageFromHash() {
  const h = location.hash.replace('#', '');
  return [...FB_PAGES, ...FB_SOON, ['settings']].some((p) => p[0] === h) ? h : 'containers';
}

// useFBStore owns all prototype state and the actions that change it.
function useFBStore() {
  const [prefs, setPrefs] = React.useState(fbLoadPrefs);
  const [page, setPage] = React.useState(fbPageFromHash);
  const [engine, setEngine] = React.useState('running');
  const [containers, setContainers] = React.useState(() => FB_CONTAINERS.map((c) => ({ ...c, base: c.cpu })));
  const [images, setImages] = React.useState(FB_IMAGES);
  const [volumes, setVolumes] = React.useState(FB_VOLUMES);
  const [networks, setNetworks] = React.useState(FB_NETWORKS);
  const [builds, setBuilds] = React.useState(FB_BUILDS);
  const [events, setEvents] = React.useState(FB_EVENTS);
  const [logs, setLogs] = React.useState(() => Object.fromEntries(FB_CONTAINERS.map((c) => [c.id, fbLogs(c)])));
  const [cpuHist, setCpuHist] = React.useState(() => Array.from({ length: 60 }, (_, i) => 16 + 6 * Math.sin(i / 4) + (i * 7) % 5));
  const [sel, setSel] = React.useState({ containers: FB_CONTAINERS[1].id });
  const [tabs, setTabs] = React.useState(FB_DEFAULT_TAB);
  const [dockH, setDockH] = React.useState(232);
  const [dockMax, setDockMax] = React.useState(false);
  const [query, setQuery] = React.useState('');
  const [modal, setModal] = React.useState(null);
  const [asking, ask] = React.useState(null);
  const [toasts, setToasts] = React.useState([]);
  const [onboard, setOnboard] = React.useState(location.hash === '#onboarding');
  const [settings, setSettings] = React.useState({ cpus: 4, memory: 8, autoStart: true, idleSleep: true, defaultContext: true, cliLinks: true,
    buildWith: 'fabribay', testcontainers: true, domains: true, rosetta: true, updates: true });
  const tick = React.useRef(0);

  React.useEffect(() => { try { localStorage.setItem('fb-prefs', JSON.stringify(prefs)); } catch (e) {} }, [prefs]);
  React.useEffect(() => {
    const onHash = () => { setPage(fbPageFromHash()); setQuery(''); };
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  const later = (ms, fn) => setTimeout(fn, ms);
  const toast = (msg, kind = 'ok') => {
    const id = Date.now() + Math.random();
    setToasts((ts) => [...ts.slice(-2), { id, msg, kind }]);
    later(3200, () => setToasts((ts) => ts.filter((x) => x.id !== id)));
  };
  const event = (kind, name) => setEvents((es) => [{ id: Date.now() + Math.random(), kind, name, at: 0 }, ...es].slice(0, 12));
  const go = (p) => { location.hash = p; setDockMax(false); };
  const select = (id, onPage = page) => { setSel((m) => ({ ...m, [onPage]: id })); if (id == null) setDockMax(false); };
  const copy = (text) => { try { navigator.clipboard.writeText(text); } catch (e) {} toast(t('已复制', 'Copied')); };

  // Container lifecycle. Each transition passes through a short transient state so the row visibly reacts.
  const patchC = (id, p) => setContainers((cs) => cs.map((c) => (c.id === id ? { ...c, ...(typeof p === 'function' ? p(c) : p) } : c)));
  const startC = (c) => {
    patchC(c.id, { state: 'starting' });
    later(500, () => patchC(c.id, (x) => ({ state: 'running', since: 0, exit: 0, base: x.base || 0.5, cpu: x.base || 0.5, mem: x.mem || 24, spark: fbFlat(x.base || 0.5) })));
    event('start', c.name);
  };
  const stopC = (c) => {
    patchC(c.id, { state: 'stopping' });
    later(600, () => patchC(c.id, { state: 'exited', exit: 0, since: 0, cpu: 0, spark: [] }));
    event('stop', c.name);
  };
  const restartC = (c) => { patchC(c.id, { state: 'stopping' }); later(450, () => startC(c)); };
  const dropC = (list) => {
    const ids = new Set(list.map((c) => c.id));
    setContainers((cs) => cs.filter((c) => !ids.has(c.id)));
    setSel((m) => (ids.has(m.containers) ? { ...m, containers: null } : m));
  };
  const removeC = (c) => ask({
    title: t('删除容器 ', 'Delete container ') + c.name + t('？', '?'),
    body: c.state === 'running' ? t('容器正在运行，会被强制停止。', 'The container is running and will be force-stopped.') : null,
    ok: t('删除', 'Delete'), danger: true, run: () => { dropC([c]); event('destroy', c.name); toast(t('已删除 ', 'Deleted ') + c.name); },
  });
  const runContainer = (f) => {
    const c = { ...fbContainer({ name: f.name, image: f.image, state: 'running', since: 0, created: 0, cpu: 0.5, mem: 24, cmd: f.cmd, ports: f.ports }), base: 0.5, state: 'starting' };
    setContainers((cs) => [...cs, c]);
    setLogs((l) => ({ ...l, [c.id]: [] }));
    later(500, () => patchC(c.id, { state: 'running' }));
    select(c.id, 'containers');
    event('start', c.name);
  };

  const engineStart = () => { setEngine('starting'); later(1300, () => { setEngine('running'); toast(t('引擎已启动', 'Engine started')); }); };
  const engineStop = () => ask({
    title: t('停止引擎？', 'Stop the engine?'),
    body: t('运行中的容器会暂停，下次启动时恢复。', 'Running containers pause and resume on the next start.'),
    ok: t('停止', 'Stop'), danger: true, run: () => setEngine('stopped'),
  });

  // Live feed: jitter CPU, extend logs of running containers, advance the running build.
  const live = React.useRef({});
  live.current = { containers };
  React.useEffect(() => {
    if (engine !== 'running') return;
    const id = setInterval(() => {
      const n = ++tick.current;
      setContainers((cs) => cs.map((c) => {
        if (c.state !== 'running') return c;
        const cpu = Math.max(0.1, c.base * (0.55 + Math.random() * 0.9));
        return { ...c, cpu, spark: [...c.spark.slice(-23), cpu] };
      }));
      setCpuHist((h) => [...h.slice(-59), live.current.containers.reduce((a, c) => a + c.cpu, 0) + 2]);
      if (n % 2 === 0) {
        const up = live.current.containers.filter((c) => c.state === 'running' && c.base > 0.3);
        const c = up[n % (up.length || 1)];
        if (c) setLogs((l) => ({ ...l, [c.id]: [...(l[c.id] || []).slice(-199), fbLogLine(n)] }));
      }
      setBuilds((bs) => bs.map((b) => {
        if (b.status !== 'running') return b;
        const done = b.done + 1;
        return done >= FB_STEPS.length ? { ...b, done, status: 'ok', dur: 27.3, at: 0 } : { ...b, done };
      }));
    }, 1500);
    return () => clearInterval(id);
  }, [engine]);

  const theme = prefs.theme === 'system' ? (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : prefs.theme;
  const usedMem = 1.9 + containers.reduce((a, c) => a + (c.state === 'running' ? c.mem : 0), 0) / 1024;
  const imageRef = (i) => i.repo + ':' + i.tag;
  return {
    prefs, theme, setPref: (k, v) => setPrefs((p) => ({ ...p, [k]: typeof v === 'function' ? v(p[k]) : v })), page, go, engine, engineStart, engineStop, setEngine,
    containers, setContainers, images, setImages, volumes, setVolumes, networks, setNetworks, builds, setBuilds, events, event,
    logs, setLogs, cpuHist, sel, selId: sel[page], select, tab: tabs[page], setTab: (k) => setTabs((m) => ({ ...m, [page]: k })),
    dockH, setDockH, dockMax, setDockMax, query, setQuery, modal, setModal, asking, ask, toasts, toast, copy, later,
    onboard, setOnboard, settings, setSetting: (k, v) => setSettings((x) => ({ ...x, [k]: v })),
    startC, stopC, restartC, removeC, dropC, runContainer, usedMem, imageRef,
    imageUsers: (i) => containers.filter((c) => c.image === imageRef(i)),
    volumeUsers: (v) => containers.filter((c) => c.mounts.some((m) => m.startsWith(v.name + ':'))),
    networkUsers: (n) => containers.filter((c) => c.network === n.name),
  };
}

// Strip is the window title bar: breadcrumb, the page search, and the engine switch.
function Strip({ s }) {
  const input = React.useRef(null);
  React.useEffect(() => {
    // ⌘K focuses the search field, ⌘B folds the sidebar.
    const key = (e) => {
      if (!(e.metaKey || e.ctrlKey)) return;
      const k = e.key.toLowerCase();
      if (k === 'k') { e.preventDefault(); input.current && input.current.focus(); }
      if (k === 'b') { e.preventDefault(); s.setPref('navCollapsed', (v) => !v); }
    };
    window.addEventListener('keydown', key);
    return () => window.removeEventListener('keydown', key);
  }, []);
  const p = [...FB_PAGES, ...FB_SOON].find((x) => x[0] === s.page);
  const title = p ? t(p[2], p[3]) : t('设置', 'Settings');
  const searchable = ['containers', 'images', 'volumes', 'networks', 'builds'].includes(s.page);
  return (
    <header className="strip">
      <div className="lights"><i /><i /><i /></div>
      <IconBtn icon="sidebar" title={(s.prefs.navCollapsed ? t('展开侧栏', 'Show sidebar') : t('收起侧栏', 'Hide sidebar')) + ' ⌘B'} onClick={() => s.setPref('navCollapsed', (v) => !v)} />
      <span className="crumb"><FBMark size={16} />FabriBay<FBIcon name="chevron" size={12} /><b>{title}</b></span>
      <label className={'search' + (searchable ? '' : ' off')}>
        <FBIcon name="search" size={13} />
        <input ref={input} value={s.query} disabled={!searchable} onChange={(e) => s.setQuery(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Escape') { s.setQuery(''); e.target.blur(); } }}
          placeholder={searchable ? t('搜索', 'Search ') + (FB_LANG === 'en' ? title.toLowerCase() : title) + '…' : ''} aria-label={t('搜索', 'Search')} />
        {s.query ? <button className="ibtn" onClick={() => s.setQuery('')} aria-label={t('清除', 'Clear')}><FBIcon name="x" size={12} /></button> : <kbd>⌘K</kbd>}
      </label>
      {/* The sidebar's engine card carries this while the sidebar is open. */}
      <span className={'engine' + (s.prefs.navCollapsed ? '' : ' hidden')}>
        <i className={'dot ' + (s.engine === 'running' ? 'running' : s.engine === 'starting' ? 'busy' : 'exited')} />
        {s.engine === 'running' ? t('引擎运行中', 'Engine running') + ' · ' + s.settings.cpus + t(' 核 · ', ' CPU · ') + s.usedMem.toFixed(1) + '/' + s.settings.memory + ' GB'
          : s.engine === 'starting' ? t('引擎启动中…', 'Engine starting…') : t('引擎已停止', 'Engine stopped')}
        <IconBtn icon={s.engine === 'stopped' ? 'play' : 'power'} disabled={s.engine === 'starting'}
          title={s.engine === 'stopped' ? t('启动引擎', 'Start engine') : t('停止引擎', 'Stop engine')} onClick={s.engine === 'stopped' ? s.engineStart : s.engineStop} />
      </span>
    </header>
  );
}

// Rail is the navigation. Expanded it is a labelled sidebar with counts and the engine card;
// collapsed it shows icons only. The last three entries are reserved for features that are not built yet.
function Rail({ s }) {
  const open = !s.prefs.navCollapsed;
  const counts = { containers: s.containers.length, images: s.images.length, volumes: s.volumes.length, networks: s.networks.length, builds: s.builds.length };
  const item = ([k, icon, zh, en], soon) => (
    <button key={k} className={(s.page === k ? 'on' : '') + (soon ? ' soon' : '')} title={open ? undefined : t(zh, en)} aria-label={t(zh, en)}
      aria-current={s.page === k ? 'page' : undefined} onClick={() => s.go(k)}>
      <FBIcon name={icon} size={open ? 16 : 18} />
      {open && <span>{t(zh, en)}</span>}
      {open && (soon ? k === 'kubernetes' && <em>{t('关', 'Off')}</em> : counts[k] != null && <em>{counts[k]}</em>)}
    </button>
  );
  const running = s.engine === 'running';
  return (
    <nav className={'rail' + (open ? ' open' : '')} aria-label={t('导航', 'Navigation')}>
      {item(FB_PAGES[0])}
      {open ? <h6>Docker</h6> : <hr />}
      {FB_PAGES.slice(1).map((p) => item(p))}
      {open ? <h6>{t('扩展', 'Extensions')}</h6> : <hr />}
      {FB_SOON.map((p) => item(p, true))}
      <span className="grow" />
      {open && (
        <div className="engine-card">
          <div className="engine-top"><i className={'dot ' + (running ? 'running' : s.engine === 'starting' ? 'busy' : 'exited')} />
            <b>{running ? t('引擎运行中', 'Engine running') : s.engine === 'starting' ? t('引擎启动中…', 'Engine starting…') : t('引擎已停止', 'Engine stopped')}</b>
            <IconBtn icon={s.engine === 'stopped' ? 'play' : 'power'} disabled={s.engine === 'starting'}
              title={s.engine === 'stopped' ? t('启动引擎', 'Start engine') : t('停止引擎', 'Stop engine')} onClick={s.engine === 'stopped' ? s.engineStart : s.engineStop} /></div>
          <div className="bar"><i style={{ width: (running ? (s.usedMem / s.settings.memory) * 100 : 0) + '%' }} /></div>
          <div className="engine-nums"><span>{t('内存 ', 'Memory ')}{running ? s.usedMem.toFixed(1) : '0'} / {s.settings.memory} GB</span><span>CPU {running ? (s.cpuHist[s.cpuHist.length - 1] / s.settings.cpus).toFixed(0) : 0}%</span></div>
        </div>
      )}
      {item(['settings', 'sliders', '设置', 'Settings'])}
    </nav>
  );
}

// StatusBar is the footer line with engine-wide facts.
function StatusBar({ s }) {
  const up = s.containers.filter((c) => c.state === 'running').length;
  const cpu = s.engine === 'running' ? s.cpuHist[s.cpuHist.length - 1] / s.settings.cpus : 0;
  return (
    <footer className="status">
      <span>v0.4.0</span>
      <span>{up} / {s.containers.length} {t('运行中', 'running')}</span>
      <span>docker → fabribay</span>
      {s.settings.buildWith !== 'fabribay' && <span>{t('构建 → Docker', 'builds → Docker')}</span>}
      <span className="grow" />
      <span>CPU {cpu.toFixed(0)}%</span>
      <span>{s.usedMem.toFixed(1)} / {s.settings.memory} GB</span>
    </footer>
  );
}

// Tweaks is the design-review panel that floats outside the window.
function Tweaks({ s }) {
  const [open, setOpen] = React.useState(false);
  return (
    <div className={'tweaks' + (open ? ' open' : '')}>
      {open && (
        <div className="tweaks-panel">
          <label>{t('主题', 'Theme')}<Seg value={s.prefs.theme} onChange={(v) => s.setPref('theme', v)} items={[['dark', t('深色', 'Dark')], ['light', t('浅色', 'Light')], ['system', t('系统', 'System')]]} /></label>
          <label>{t('密度', 'Density')}<Seg value={s.prefs.density} onChange={(v) => s.setPref('density', v)} items={[['compact', t('紧凑', 'Compact')], ['cozy', t('宽松', 'Cozy')]]} /></label>
          <label>{t('强调色', 'Accent')}<span className="swatches">{['orange', 'cobalt', 'moss'].map((a) => (
            <button key={a} data-accent={a} aria-label={a} className={s.prefs.accent === a ? 'on' : ''} onClick={() => s.setPref('accent', a)} />))}</span></label>
          <label>{t('语言', 'Language')}<Seg value={s.prefs.lang} onChange={(v) => s.setPref('lang', v)} items={[['zh', '中文'], ['en', 'English']]} /></label>
          <button className="btn" onClick={() => { s.setOnboard(true); setOpen(false); }}>{t('查看首次引导', 'Show first-run setup')}</button>
        </div>
      )}
      <button className="tweaks-toggle" onClick={() => setOpen(!open)}><FBIcon name="sliders" size={14} />Tweaks</button>
    </div>
  );
}

// Confirm renders the store's pending question; Enter confirms, Esc cancels.
function Confirm({ s }) {
  const q = s.asking;
  if (!q) return null;
  const ok = () => { s.ask(null); q.run(); };
  return (
    <Modal title={q.title} onClose={() => s.ask(null)} width={380}
      footer={<React.Fragment><button className="btn" onClick={() => s.ask(null)}>{t('取消', 'Cancel')}</button>
        <button className={'btn ' + (q.danger ? 'danger' : 'primary')} autoFocus onClick={ok}>{q.ok}</button></React.Fragment>}>
      {q.body && <p className="muted">{q.body}</p>}
    </Modal>
  );
}

// EngineOff replaces a resource page while the engine is not running.
function EngineOff({ s }) {
  return s.engine === 'starting'
    ? <Empty icon="power" title={t('引擎启动中…', 'Engine starting…')} />
    : <Empty icon="power" title={t('引擎已停止', 'The engine is stopped')} action={t('启动引擎', 'Start engine')} onAction={s.engineStart} />;
}

// App wires theme attributes, the shell, and the current page.
function App() {
  const s = useFBStore();
  FB_LANG = s.prefs.lang;
  React.useEffect(() => { document.documentElement.dataset.theme = s.theme; document.documentElement.lang = s.prefs.lang === 'en' ? 'en' : 'zh-CN'; }, [s.theme, s.prefs.lang]);
  const pages = { overview: OverviewPage, containers: ContainersPage, images: ImagesPage, volumes: VolumesPage, networks: NetworksPage, builds: BuildsPage, settings: SettingsPage };
  const Page = pages[s.page];
  const soon = FB_SOON.find((p) => p[0] === s.page);
  const needsEngine = Page && s.page !== 'settings' && s.engine !== 'running';
  return (
    <React.Fragment>
      <div className="win" data-theme={s.theme} data-density={s.prefs.density} data-accent={s.prefs.accent} data-screen-label={s.onboard ? 'onboarding' : s.page}>
        <Strip s={s} />
        <div className="body">
          <Rail s={s} />
          <main className="main" key={s.page + s.prefs.lang}>
            {soon ? <Empty icon={soon[1]} title={t(soon[2], soon[3]) + t(' · 即将提供', ' · coming soon')} />
              : needsEngine ? <EngineOff s={s} /> : <Page s={s} />}
          </main>
        </div>
        <StatusBar s={s} />
        <Confirm s={s} />
        {s.onboard && <Onboarding s={s} />}
        <div className="toasts" aria-live="polite">{s.toasts.map((x) => <div key={x.id} className={'toast ' + x.kind}><FBIcon name={x.kind === 'err' ? 'x' : 'check'} size={14} sw={2.2} />{x.msg}</div>)}</div>
      </div>
      <Tweaks s={s} />
    </React.Fragment>
  );
}

ReactDOM.createRoot(document.getElementById('root')).render(<App />);
