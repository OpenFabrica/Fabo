// Overview, settings and the first-run setup.

// CpuChart draws the engine CPU history as an area; the y scale follows the peak with a floor.
function CpuChart({ values }) {
  const max = Math.max(40, ...values) * 1.15;
  const pts = values.map((v, i) => (i / (values.length - 1)) * 600 + ',' + (100 - (v / max) * 100).toFixed(1));
  return (
    <svg className="chart" viewBox="0 0 600 100" preserveAspectRatio="none" aria-hidden="true">
      {[25, 50, 75].map((y) => <line key={y} x1="0" x2="600" y1={y} y2={y} className="gridline" />)}
      <polygon points={'0,100 ' + pts.join(' ') + ' 600,100'} className="area" />
      <polyline points={pts.join(' ')} className="line" />
    </svg>
  );
}

// OverviewPage is the engine at a glance: counts, load, disk, busiest containers, recent events.
function OverviewPage({ s }) {
  const up = s.containers.filter((c) => c.state === 'running');
  const failed = s.containers.filter((c) => c.state === 'exited' && c.exit);
  const imgSize = s.images.reduce((a, i) => a + i.size, 0);
  const volSize = s.volumes.reduce((a, v) => a + v.size, 0);
  const [cache, setCache] = React.useState(2150);
  const disk = [[t('镜像', 'Images'), imgSize, 'a'], [t('卷', 'Volumes'), volSize, 'b'], [t('构建缓存', 'Build cache'), cache, 'c'], [t('容器', 'Containers'), 310, 'd']];
  const total = disk.reduce((a, d) => a + d[1], 0);
  const cpu = s.cpuHist[s.cpuHist.length - 1];
  const lastBuild = s.builds[0];
  const idle = s.images.filter((i) => !s.imageUsers(i).length);
  const dead = s.containers.filter((c) => c.state === 'exited');
  const reclaim = idle.reduce((a, i) => a + i.size, 0) + cache;
  const prune = () => s.ask({ title: t('清理未使用的数据？', 'Remove unused data?'),
    body: t('删除已停止的容器、未使用的镜像和构建缓存，可释放 ', 'Removes stopped containers, unused images and the build cache, freeing ') + fbSize(reclaim) + t('。', '.'), ok: t('清理', 'Remove'), danger: true,
    run: () => { const ids = new Set(idle.map((i) => i.id)); s.setImages((is) => is.filter((i) => !ids.has(i.id))); s.dropC(dead); setCache(0); s.toast(t('已释放 ', 'Freed ') + fbSize(reclaim)); } });
  const verb = { start: t('启动', 'started'), stop: t('停止', 'stopped'), die: t('异常退出', 'exited with an error'), build: t('构建完成', 'built'), pull: t('拉取', 'pulled'), create: t('创建卷', 'volume created'), destroy: t('删除', 'removed') };
  const tile = (page, label, big, small, bad) => (
    <button className="tile" onClick={() => s.go(page)}><small>{label}</small><b>{big}</b><span className={bad ? 'st failed' : 'muted'}>{small}</span></button>
  );
  return (
    <div className="overview">
      <div className="tiles">
        {tile('containers', t('容器', 'Containers'), <React.Fragment>{up.length}<u>/ {s.containers.length}</u></React.Fragment>, failed.length ? failed.length + t(' 个异常', ' failed') : t('运行中', 'running'), failed.length)}
        {tile('images', t('镜像', 'Images'), s.images.length, fbSize(imgSize))}
        {tile('volumes', t('卷', 'Volumes'), s.volumes.length, fbSize(volSize))}
        {tile('builds', t('构建', 'Builds'), s.builds.length, lastBuild ? (lastBuild.status === 'running' ? t('构建中…', 'building…') : t('最近 ', 'last ') + fbSince(lastBuild.at)) : '—')}
      </div>
      <div className="panels">
        <section className="panel">
          <header><b>CPU</b><span className="num">{(cpu / s.settings.cpus).toFixed(0)}%</span><span className="muted">{s.settings.cpus} {t('核', 'cores')}</span></header>
          <CpuChart values={s.cpuHist.map((v) => v / s.settings.cpus)} />
          <div className="memline"><b>{t('内存', 'Memory')}</b><span className="num">{s.usedMem.toFixed(1)} / {s.settings.memory} GB</span></div>
          <div className="bar wide"><i style={{ width: (s.usedMem / s.settings.memory) * 100 + '%' }} /></div>
        </section>
        <section className="panel">
          <header><b>{t('磁盘', 'Disk')}</b><span className="num">{fbSize(total)} / 64 GB</span><span className="grow" /><button className="btn ghost" onClick={prune}>{t('清理', 'Clean up')}</button></header>
          <div className="stack">{disk.map(([l, v, k]) => <i key={k} className={'seg-' + k} style={{ width: (v / 65536) * 100 + '%' }} title={l} />)}</div>
          <ul className="legend">{disk.map(([l, v, k]) => <li key={k}><i className={'seg-' + k} />{l}<span className="num">{fbSize(v)}</span></li>)}</ul>
        </section>
        <section className="panel list">
          <header><b>{t('占用最高', 'Busiest')}</b></header>
          {[...up].sort((a, b) => b.cpu - a.cpu).slice(0, 5).map((c) => (
            <button key={c.id} className="lrow" onClick={() => { s.select(c.id, 'containers'); s.go('containers'); }}>
              <i className="dot running" /><span className="name">{c.name}</span><FBSpark values={c.spark} count={20} /><span className="num">{c.cpu.toFixed(1)}%</span><span className="num muted">{fbSize(c.mem)}</span></button>
          ))}
          {!up.length && <div className="files-none muted">{t('没有运行中的容器', 'Nothing is running')}</div>}
        </section>
        <section className="panel list">
          <header><b>{t('最近事件', 'Recent events')}</b></header>
          {s.events.slice(0, 5).map((e) => (
            <div key={e.id} className="lrow"><i className={'dot ' + (e.kind === 'die' ? 'failed' : e.kind === 'stop' || e.kind === 'destroy' ? 'exited' : 'running')} />
              <span className="name">{e.name}</span><span className={e.kind === 'die' ? 'st failed' : 'muted'}>{verb[e.kind]}</span><span className="num muted">{fbSince(e.at)}</span></div>
          ))}
        </section>
      </div>
    </div>
  );
}

// Field is one settings line: label on the left, control on the right.
function Field({ label, hint, children }) {
  return <div className="field"><div><b>{label}</b>{hint && <small>{hint}</small>}</div><div className="ctl">{children}</div></div>;
}

// Range is a labelled slider; it reports numbers.
function Range({ value, min, max, unit, onChange, label }) {
  return <span className="range"><input type="range" min={min} max={max} value={value} aria-label={label} onChange={(e) => onChange(+e.target.value)} /><b className="num">{value} {unit}</b></span>;
}

// SettingsPage groups preferences; resource changes apply only after an explicit engine restart.
function SettingsPage({ s }) {
  const [sec, setSec] = React.useState('general');
  const [cpus, setCpus] = React.useState(s.settings.cpus);
  const [memory, setMemory] = React.useState(s.settings.memory);
  const dirty = cpus !== s.settings.cpus || memory !== s.settings.memory;
  const tog = (k, label) => <Toggle on={s.settings[k]} label={label} onChange={(v) => s.setSetting(k, v)} />;
  const apply = () => { s.setSetting('cpus', cpus); s.setSetting('memory', memory); s.setEngine('starting'); s.later(1300, () => { s.setEngine('running'); s.toast(t('已应用，引擎已重启', 'Applied, engine restarted')); }); };
  const secs = [['general', t('常规', 'General')], ['resources', t('资源', 'Resources')], ['docker', 'Docker'], ['network', t('网络', 'Network')], ['about', t('关于', 'About')]];
  return (
    <div className="settings">
      <nav>{secs.map(([k, l]) => <button key={k} className={sec === k ? 'on' : ''} onClick={() => setSec(k)}>{l}{k === 'resources' && dirty && <i className="dot busy" />}</button>)}</nav>
      <div className="pane">
        {sec === 'general' && <React.Fragment>
          <Field label={t('外观', 'Appearance')}><Seg value={s.prefs.theme} onChange={(v) => s.setPref('theme', v)} items={[['system', t('跟随系统', 'System')], ['light', t('浅色', 'Light')], ['dark', t('深色', 'Dark')]]} /></Field>
          <Field label={t('语言', 'Language')}><Seg value={s.prefs.lang} onChange={(v) => s.setPref('lang', v)} items={[['zh', '中文'], ['en', 'English']]} /></Field>
          <Field label={t('列表密度', 'List density')}><Seg value={s.prefs.density} onChange={(v) => s.setPref('density', v)} items={[['compact', t('紧凑', 'Compact')], ['cozy', t('宽松', 'Cozy')]]} /></Field>
          <Field label={t('登录时启动引擎', 'Start the engine at login')}>{tog('autoStart', t('登录时启动引擎', 'Start the engine at login'))}</Field>
          <Field label={t('空闲时休眠虚拟机', 'Sleep the VM when idle')} hint={t('没有容器运行且 5 分钟无请求', 'No containers running and no requests for 5 minutes')}>{tog('idleSleep', t('空闲时休眠虚拟机', 'Sleep the VM when idle'))}</Field>
          <Field label={t('自动检查更新', 'Check for updates automatically')}>{tog('updates', t('自动检查更新', 'Check for updates automatically'))}</Field>
        </React.Fragment>}
        {sec === 'resources' && <React.Fragment>
          <Field label="CPU"><Range value={cpus} min={1} max={10} unit={t('核', 'cores')} onChange={setCpus} label="CPU" /></Field>
          <Field label={t('内存', 'Memory')}><Range value={memory} min={2} max={32} unit="GB" onChange={setMemory} label={t('内存', 'Memory')} /></Field>
          <Field label={t('数据盘', 'Data disk')} hint={t('稀疏文件，按实际写入占用空间', 'Sparse file, grows as data is written')}><span className="num">64 GB</span></Field>
          <Field label="Rosetta" hint={t('在 Apple 芯片上运行 amd64 镜像', 'Runs amd64 images on Apple silicon')}>{tog('rosetta', 'Rosetta')}</Field>
          <div className="apply"><span className="muted">{dirty ? t('更改在引擎重启后生效', 'Changes take effect after an engine restart') : ''}</span>
            <button className="btn" disabled={!dirty} onClick={() => { setCpus(s.settings.cpus); setMemory(s.settings.memory); }}>{t('还原', 'Revert')}</button>
            <button className="btn primary" disabled={!dirty || s.engine === 'starting'} onClick={apply}>{t('应用并重启引擎', 'Apply and restart')}</button></div>
        </React.Fragment>}
        {sec === 'docker' && <React.Fragment>
          <Field label={t('设为默认 Docker 引擎', 'Default Docker engine')} hint="docker context use fabribay">{tog('defaultContext', t('设为默认 Docker 引擎', 'Default Docker engine'))}</Field>
          <Field label={t('命令行工具', 'Command-line tools')} hint={t('把 docker、compose、buildx 链接到 ~/.fabribay/bin 并加入 PATH', 'Links docker, compose and buildx into ~/.fabribay/bin and adds it to PATH')}>{tog('cliLinks', t('命令行工具', 'Command-line tools'))}</Field>
          <Field label={t('docker build 使用', 'docker build uses')}><Seg value={s.settings.buildWith} onChange={(v) => s.setSetting('buildWith', v)} items={[['fabribay', 'FabriBay'], ['docker', 'Docker']]} /></Field>
          <Field label="Testcontainers" hint="~/.testcontainers.properties">{tog('testcontainers', 'Testcontainers')}</Field>
          <Field label="Socket"><button className="link mono" onClick={() => s.copy('unix://~/.fabribay/run/docker.sock')}>~/.fabribay/run/docker.sock</button></Field>
        </React.Fragment>}
        {sec === 'network' && <React.Fragment>
          <Field label={t('容器域名', 'Container domains')} hint={t('每个容器可用 <名称>.fabribay.local 访问', 'Reach every container at <name>.fabribay.local')}>{tog('domains', t('容器域名', 'Container domains'))}</Field>
          <Field label={t('域名后缀', 'Domain suffix')}><span className="mono">fabribay.local</span></Field>
          <Field label={t('HTTP / HTTPS 端口', 'HTTP / HTTPS ports')}><span className="mono">80 / 443</span></Field>
          <Field label={t('容器子网', 'Container subnet')}><span className="mono">172.17.0.0/12</span></Field>
        </React.Fragment>}
        {sec === 'about' && <React.Fragment>
          <div className="about"><FBMark size={44} /><div><b>FabriBay</b><span className="muted mono">0.4.0 · arm64 · {t('guest 协议', 'guest protocol')} v4 · Linux 6.12</span></div>
            <span className="grow" /><button className="btn" onClick={() => s.toast(t('已是最新版本', 'You are up to date'))}>{t('检查更新', 'Check for updates')}</button></div>
          <Field label={t('首次设置', 'First-run setup')}><button className="btn" onClick={() => s.setOnboard(true)}>{t('重新运行', 'Run again')}</button></Field>
          <Field label={t('撤销本机设置', 'Undo the setup on this Mac')} hint={t('移除 context、命令行链接和后台项目，数据保留', 'Removes the context, CLI links and login item. Data is kept.')}>
            <button className="btn danger-line" onClick={() => s.ask({ title: t('撤销本机设置？', 'Undo the setup?'), body: t('引擎会停止，镜像、容器和卷都会保留。', 'The engine stops. Images, containers and volumes are kept.'), ok: t('撤销', 'Undo'), danger: true, run: () => { s.setEngine('stopped'); s.toast(t('已撤销本机设置', 'Setup undone')); } })}>{t('撤销', 'Undo')}</button></Field>
          <Field label={t('重置全部数据', 'Reset all data')} hint={t('删除所有镜像、容器、卷和构建缓存', 'Deletes every image, container, volume and the build cache')}>
            <button className="btn danger-line" onClick={() => s.ask({ title: t('重置全部数据？', 'Reset all data?'), body: t('此操作无法撤销。', 'This cannot be undone.'), ok: t('重置', 'Reset'), danger: true,
              run: () => { s.setContainers([]); s.setImages([]); s.setVolumes([]); s.setBuilds([]); s.toast(t('数据已重置', 'Data reset')); } })}>{t('重置', 'Reset')}</button></Field>
        </React.Fragment>}
      </div>
    </div>
  );
}

// Onboarding is the first-run flow: welcome, what setup changes on this Mac, resources, then the engine start.
function Onboarding({ s }) {
  const [step, setStep] = React.useState(0);
  const [opt, setOpt] = React.useState({ context: true, cli: true, agent: true, builder: true });
  const [done, setDone] = React.useState(0);
  const items = [
    ['context', t('设为默认 Docker 引擎', 'Make it the default Docker engine'), 'docker context use fabribay'],
    ['cli', t('安装命令行工具', 'Install the command-line tools'), '~/.fabribay/bin → PATH'],
    ['agent', t('登录时启动引擎', 'Start the engine at login'), 'LaunchAgent'],
    ['builder', t('用 FabriBay 构建镜像', 'Build images with FabriBay'), 'docker buildx'],
  ];
  const work = [...items.filter(([k]) => opt[k]).map((i) => i[1]), t('启动 Linux 虚拟机', 'Start the Linux VM')];
  // The last step ticks through the chosen work items one by one.
  React.useEffect(() => {
    if (step !== 3 || done >= work.length) return;
    const id = setTimeout(() => setDone(done + 1), done === work.length - 1 ? 1100 : 420);
    return () => clearTimeout(id);
  }, [step, done]);
  const finish = () => { ['defaultContext', 'cliLinks', 'autoStart'].forEach((k, i) => s.setSetting(k, opt[['context', 'cli', 'agent'][i]])); s.setSetting('buildWith', opt.builder ? 'fabribay' : 'docker'); s.setOnboard(false); if (s.engine !== 'running') s.setEngine('running'); };
  const ready = done >= work.length;
  return (
    <div className="onboard">
      <div className="lights"><i /><i /><i /></div>
      <div className="onboard-lang"><Seg value={s.prefs.lang} onChange={(v) => s.setPref('lang', v)} items={[['zh', '中文'], ['en', 'English']]} /></div>
      <div className="onboard-card">
        {step === 0 && <React.Fragment>
          <FBMark size={72} />
          <h1>FabriBay</h1>
          <p className="muted">{t('本机的容器引擎。docker、compose 和 buildx 照常使用。', 'A container engine for this Mac. docker, compose and buildx work as usual.')}</p>
          <button className="btn primary big" autoFocus onClick={() => setStep(1)}>{t('开始设置', 'Set up')}</button>
          <button className="btn ghost" onClick={() => s.setOnboard(false)}>{t('稍后', 'Later')}</button>
        </React.Fragment>}
        {step === 1 && <React.Fragment>
          <h2>{t('要在这台 Mac 上做的事', 'What setup does on this Mac')}</h2>
          <div className="checks">{items.map(([k, label, code]) => (
            <div key={k} className="check"><div><b>{label}</b><span className="mono muted">{code}</span></div><Toggle on={opt[k]} label={label} onChange={(v) => setOpt({ ...opt, [k]: v })} /></div>
          ))}</div>
        </React.Fragment>}
        {step === 2 && <React.Fragment>
          <h2>{t('分配给引擎的资源', 'Resources for the engine')}</h2>
          <div className="checks">
            <div className="check"><div><b>CPU</b></div><Range value={s.settings.cpus} min={1} max={10} unit={t('核', 'cores')} onChange={(v) => s.setSetting('cpus', v)} label="CPU" /></div>
            <div className="check"><div><b>{t('内存', 'Memory')}</b><span className="muted">{t('空闲时会还给系统', 'Returned to macOS when idle')}</span></div><Range value={s.settings.memory} min={2} max={32} unit="GB" onChange={(v) => s.setSetting('memory', v)} label={t('内存', 'Memory')} /></div>
          </div>
        </React.Fragment>}
        {step === 3 && <React.Fragment>
          <h2>{ready ? t('一切就绪', 'Ready') : t('正在设置…', 'Setting up…')}</h2>
          <div className="checks">{work.map((w, i) => (
            <div key={i} className={'check work' + (i < done ? ' ok' : i === done ? ' busy' : '')}><span className="mark-i">{i < done ? <FBIcon name="check" size={14} sw={2.4} /> : <i className={'dot ' + (i === done ? 'busy' : 'exited')} />}</span><b>{w}</b>
              {i === work.length - 1 && i < done && <span className="mono muted">1.1 s</span>}</div>
          ))}</div>
          {ready && <button className="snippet mono" onClick={() => s.copy('docker run --rm alpine echo hello')}><span className="prompt">$</span> docker run --rm alpine echo hello<FBIcon name="copy" size={14} /></button>}
        </React.Fragment>}
        {step > 0 && (
          <div className="onboard-nav">
            <span className="pips">{[1, 2, 3].map((n) => <i key={n} className={n <= step ? 'on' : ''} />)}</span>
            <span className="grow" />
            {step < 3 && <button className="btn" onClick={() => setStep(step - 1)}>{t('上一步', 'Back')}</button>}
            {step < 3 ? <button className="btn primary" autoFocus onClick={() => setStep(step + 1)}>{step === 2 ? t('完成设置', 'Finish setup') : t('继续', 'Continue')}</button>
              : <button className="btn primary" disabled={!ready} onClick={finish}>{t('打开 FabriBay', 'Open FabriBay')}</button>}
          </div>
        )}
      </div>
    </div>
  );
}

Object.assign(window, { OverviewPage, SettingsPage, Onboarding });
