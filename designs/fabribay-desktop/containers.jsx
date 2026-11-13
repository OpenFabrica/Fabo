// Containers page: grouped table, the detail dock (info / logs / terminal / files), and the run dialog.

// fbBucket sorts a container into the filter it belongs to.
function fbBucket(c) { return c.state === 'exited' ? (c.exit ? 'failed' : 'stopped') : 'running'; }

// StateDot is the status bullet; transient states pulse.
function StateDot({ c }) {
  const k = c.state === 'exited' ? (c.exit ? 'failed' : 'exited') : c.state === 'running' ? 'running' : 'busy';
  return <i className={'dot ' + k} />;
}

// ContainerActions is the start / stop / restart cluster used by rows and the dock.
function ContainerActions({ s, c }) {
  const busy = c.state === 'starting' || c.state === 'stopping';
  return c.state === 'exited'
    ? <IconBtn icon="play" title={t('启动', 'Start')} onClick={() => s.startC(c)} />
    : <React.Fragment>
        <IconBtn icon="restart" title={t('重启', 'Restart')} disabled={busy} onClick={() => s.restartC(c)} />
        <IconBtn icon="stop" title={t('停止', 'Stop')} disabled={busy} onClick={() => s.stopC(c)} />
      </React.Fragment>;
}

// LogView prints a log buffer and keeps the newest line in view while following.
function LogView({ lines, follow, filter }) {
  const ref = React.useRef(null);
  const shown = filter ? lines.filter((l) => (l.lv + ' ' + l.m).toLowerCase().includes(filter.toLowerCase())) : lines;
  React.useEffect(() => { if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight; }, [shown.length, follow]);
  if (!shown.length) return <div className="files-none muted">{filter ? t('没有匹配的日志', 'No matching lines') : t('暂无日志', 'No logs yet')}</div>;
  return <pre className="logs" ref={ref}>{shown.map((l, i) => <div key={i}><span className="t">{l.t}</span><span className={'lv ' + l.lv}>{l.lv}</span>{l.m}</div>)}</pre>;
}

// Terminal is a pretend shell inside the container: a handful of commands answer, the rest report "not found".
function Terminal({ s, c }) {
  const [lines, setLines] = React.useState([]);
  const [cmd, setCmd] = React.useState('');
  const box = React.useRef(null);
  React.useEffect(() => { if (box.current) box.current.scrollTop = box.current.scrollHeight; }, [lines.length]);
  if (c.state !== 'running') return <Empty icon="terminal" title={t('容器未运行', 'The container is not running')} action={t('启动', 'Start')} onAction={() => s.startC(c)} />;
  const answers = {
    help: 'ls  pwd  ps  env  whoami  hostname  uname  clear', ls: 'dist  node_modules  package.json  pnpm-lock.yaml', pwd: '/app', whoami: 'root',
    hostname: c.id, uname: 'Linux ' + c.id + ' 6.12.9-fabribay aarch64', ps: 'PID   USER     COMMAND\n  1   root     ' + c.cmd + '\n 42   root     sh',
    env: ['PATH=/usr/local/bin:/usr/bin:/bin', 'HOSTNAME=' + c.id, ...c.env].join('\n'),
  };
  const run = (e) => {
    e.preventDefault();
    const word = cmd.trim().split(' ')[0];
    if (word === 'clear') setLines([]);
    else setLines((l) => [...l, { in: cmd }, ...(word ? [{ out: answers[word] || 'sh: ' + word + ': not found' }] : [])]);
    setCmd('');
  };
  return (
    <form className="term mono" ref={box} onSubmit={run} onClick={(e) => e.currentTarget.querySelector('input').focus()}>
      {lines.map((l, i) => l.in != null ? <div key={i}><span className="prompt">/app #</span> {l.in}</div> : <div key={i} className="out">{l.out}</div>)}
      <div className="term-in"><span className="prompt">/app #</span><input value={cmd} onChange={(e) => setCmd(e.target.value)} autoFocus spellCheck="false" aria-label={t('命令', 'Command')} /></div>
    </form>
  );
}

// ContainerDock is the detail panel of the selected container.
function ContainerDock({ s, c }) {
  const [follow, setFollow] = React.useState(true);
  const [filter, setFilter] = React.useState('');
  const url = c.ports[0] && 'localhost:' + c.ports[0][0];
  return (
    <Dock s={s} title={c.name} sub={<span className={'st ' + fbBucket(c)}>{fbStatus(c)}</span>} tab={s.tab} onTab={s.setTab}
      tabs={[['info', t('信息', 'Info')], ['logs', t('日志', 'Logs')], ['terminal', t('终端', 'Terminal')], ['files', t('文件', 'Files')]]}
      actions={<React.Fragment>
        {s.tab === 'logs' && <React.Fragment>
          <input className="mini" value={filter} onChange={(e) => setFilter(e.target.value)} placeholder={t('筛选日志', 'Filter logs')} aria-label={t('筛选日志', 'Filter logs')} />
          <button className={'chip' + (follow ? ' on' : '')} aria-pressed={follow} onClick={() => setFollow(!follow)}>{t('跟随', 'Follow')}</button>
          <IconBtn icon="broom" title={t('清空', 'Clear')} onClick={() => s.setLogs((l) => ({ ...l, [c.id]: [] }))} />
          <i className="sep" />
        </React.Fragment>}
        {url && c.state === 'running' && <IconBtn icon="out" title={t('在浏览器打开', 'Open in browser')} onClick={() => s.toast(t('已打开 ', 'Opened ') + url)} />}
        <ContainerActions s={s} c={c} />
        <IconBtn icon="trash" danger title={t('删除', 'Delete')} onClick={() => s.removeC(c)} />
        <i className="sep" />
      </React.Fragment>}>
      {s.tab === 'info' && <KV rows={[
        ['ID', c.id], [t('镜像', 'Image'), c.image], [t('命令', 'Command'), c.cmd], [t('创建', 'Created'), fbSince(c.created), false],
        [t('端口', 'Ports'), c.ports.map(([h, p]) => 'localhost:' + h + ' → ' + p + '/tcp')],
        s.settings.domains && [t('域名', 'Domain'), c.name + '.fabribay.local'], [t('网络', 'Network'), c.network + ' · ' + c.ip],
        [t('挂载', 'Mounts'), c.mounts], [t('环境变量', 'Environment'), c.env], c.project && ['Compose', c.project],
      ]} />}
      {s.tab === 'logs' && <LogView lines={s.logs[c.id] || []} follow={follow} filter={filter} />}
      {s.tab === 'terminal' && <Terminal key={c.id} s={s} c={c} />}
      {s.tab === 'files' && <FileBrowser key={c.id} root={FB_FS} />}
    </Dock>
  );
}

// RunModal creates a container from an image; the name must be unique.
function RunModal({ s }) {
  const [f, setF] = React.useState({ image: s.modal.image || '', name: '', host: '', port: '', cmd: '' });
  const set = (k) => (e) => setF({ ...f, [k]: e.target.value });
  const name = f.name.trim() || (f.image.split('/').pop().split(':')[0] || 'container') + '-' + (s.containers.length + 1);
  const clash = s.containers.some((c) => c.name === name);
  const submit = (e) => {
    e.preventDefault();
    if (!f.image.trim() || clash) return;
    s.runContainer({ image: f.image.trim(), name, cmd: f.cmd, ports: f.host && f.port ? [[+f.host, +f.port]] : [] });
    s.setModal(null);
    if (s.page !== 'containers') s.go('containers');
    s.toast(t('已启动 ', 'Started ') + name);
  };
  return (
    <Modal title={t('运行容器', 'Run a container')} onClose={() => s.setModal(null)}
      footer={<React.Fragment><button className="btn" onClick={() => s.setModal(null)}>{t('取消', 'Cancel')}</button>
        <button className="btn primary" form="run-form" disabled={!f.image.trim() || clash}>{t('运行', 'Run')}</button></React.Fragment>}>
      <form id="run-form" className="form" onSubmit={submit}>
        <label>{t('镜像', 'Image')}<input className="mono" list="run-images" value={f.image} onChange={set('image')} autoFocus placeholder="nginx:1.27-alpine" />
          <datalist id="run-images">{s.images.map((i) => <option key={i.id} value={s.imageRef(i)} />)}</datalist></label>
        <label>{t('名称', 'Name')}<input className="mono" value={f.name} onChange={set('name')} placeholder={name} aria-invalid={clash} />
          {clash && <small className="err">{t('已有同名容器', 'A container with this name exists')}</small>}</label>
        <label>{t('端口', 'Port')}<span className="pair"><input className="mono" value={f.host} onChange={set('host')} placeholder="8080" inputMode="numeric" aria-label={t('主机端口', 'Host port')} /><span>→</span>
          <input className="mono" value={f.port} onChange={set('port')} placeholder="80" inputMode="numeric" aria-label={t('容器端口', 'Container port')} /></span></label>
        <label>{t('命令', 'Command')}<input className="mono" value={f.cmd} onChange={set('cmd')} placeholder={t('使用镜像默认值', 'Image default')} /></label>
      </form>
    </Modal>
  );
}

// ContainersPage lists containers grouped by Compose project.
function ContainersPage({ s }) {
  const [filter, setFilter] = React.useState('all');
  const [closed, setClosed] = React.useState({});
  const q = s.query.trim().toLowerCase();
  const count = (k) => s.containers.filter((c) => fbBucket(c) === k).length;
  const shown = s.containers.filter((c) => (filter === 'all' || fbBucket(c) === filter) && (!q || c.name.includes(q) || c.image.includes(q)));
  // Groups keep the order of their first member; standalone containers are one-row groups.
  const groups = [];
  shown.forEach((c) => {
    const g = c.project && groups.find((x) => x.project === c.project);
    if (g) g.items.push(c); else groups.push({ project: c.project, items: [c] });
  });
  const visible = groups.flatMap((g) => (g.project && closed[g.project] ? [] : g.items.map((c) => c.id)));
  useListKeys(visible, s.selId, s.select);
  const selected = s.containers.find((c) => c.id === s.selId);
  const dead = s.containers.filter((c) => c.state === 'exited');
  const prune = () => s.ask({ title: t('清理 ', 'Remove ') + dead.length + t(' 个已停止的容器？', ' stopped containers?'), ok: t('清理', 'Remove'), danger: true,
    run: () => { s.dropC(dead); s.toast(t('已清理 ', 'Removed ') + dead.length + t(' 个容器', ' containers')); } });
  const menu = (c) => [
    { icon: 'stack', label: t('日志', 'Logs'), run: () => { s.select(c.id); s.setTab('logs'); } },
    { icon: 'terminal', label: t('终端', 'Terminal'), run: () => { s.select(c.id); s.setTab('terminal'); } },
    { icon: 'folder', label: t('文件', 'Files'), run: () => { s.select(c.id); s.setTab('files'); } },
    { icon: 'copy', label: t('复制 ID', 'Copy ID'), run: () => s.copy(c.id) }, '-',
    { icon: 'trash', label: t('删除', 'Delete'), danger: true, run: () => s.removeC(c) },
  ];
  return (
    <React.Fragment>
      <div className="tools">
        <Pills value={filter} onChange={setFilter} items={[['all', t('全部', 'All'), s.containers.length], ['running', t('运行中', 'Running'), count('running')],
          ['stopped', t('已停止', 'Stopped'), count('stopped')], ['failed', t('异常', 'Failed'), count('failed')]]} />
        <span className="grow" />
        {dead.length > 0 && <button className="btn ghost" onClick={prune}>{t('清理已停止 ', 'Remove stopped ')}{dead.length}</button>}
        <button className="btn primary" onClick={() => s.setModal({ kind: 'run' })}><FBIcon name="plus" size={13} sw={2.2} />{t('运行容器', 'Run container')}</button>
      </div>
      {!s.dockMax && (
        <div className="table" style={{ '--cols': 'minmax(160px,1.1fr) minmax(110px,1fr) 172px 104px 52px 58px 62px 72px 84px' }}>
          <div className="row head"><span>{t('名称', 'Name')}</span><span>{t('镜像', 'Image')}</span><span>{t('状态', 'Status')}</span><span>{t('端口', 'Ports')}</span>
            <span className="num">CPU</span><span /><span className="num">{t('内存', 'Memory')}</span><span>{t('创建', 'Created')}</span><span /></div>
          <div className="rows">
            {groups.map((g) => (
              <React.Fragment key={g.project || g.items[0].id}>
                {g.project && (
                  <div className="ghead" onClick={() => setClosed({ ...closed, [g.project]: !closed[g.project] })}>
                    <FBIcon name={closed[g.project] ? 'chevron' : 'down'} size={12} /><b>{g.project}</b>
                    <span>compose · {g.items.filter((c) => c.state === 'running').length}/{g.items.length}</span><span className="grow" />
                    <span className="acts">
                      {g.items.some((c) => c.state === 'exited') && <IconBtn icon="play" title={t('全部启动', 'Start all')} onClick={() => g.items.filter((c) => c.state === 'exited').forEach(s.startC)} />}
                      {g.items.some((c) => c.state === 'running') && <IconBtn icon="restart" title={t('全部重启', 'Restart all')} onClick={() => g.items.filter((c) => c.state === 'running').forEach(s.restartC)} />}
                      {g.items.some((c) => c.state === 'running') && <IconBtn icon="stop" title={t('全部停止', 'Stop all')} onClick={() => g.items.filter((c) => c.state === 'running').forEach(s.stopC)} />}
                    </span>
                  </div>
                )}
                {!(g.project && closed[g.project]) && g.items.map((c) => (
                  <div key={c.id} className={'row' + (c.id === s.selId ? ' sel' : '') + (g.project ? ' child' : '')} onClick={() => s.select(c.id)}>
                    <span className="name"><StateDot c={c} />{c.name}</span>
                    <span className="muted">{c.image}</span>
                    <span className={'st ' + fbBucket(c)}>{fbStatus(c)}</span>
                    <span>{c.ports[0] && <button className="link" onClick={(e) => { e.stopPropagation(); s.toast(t('已打开 ', 'Opened ') + 'localhost:' + c.ports[0][0]); }}>{c.ports[0][0]}→{c.ports[0][1]}</button>}
                      {c.ports.length > 1 && <span className="muted"> +{c.ports.length - 1}</span>}</span>
                    <span className="num">{c.state === 'running' ? c.cpu.toFixed(1) + '%' : '—'}</span>
                    <FBSpark values={c.spark} />
                    <span className="num">{c.state === 'running' ? fbSize(c.mem) : '—'}</span>
                    <span className="muted">{fbSince(c.created)}</span>
                    <span className="acts"><ContainerActions s={s} c={c} /><Menu items={menu(c)} /></span>
                  </div>
                ))}
              </React.Fragment>
            ))}
            {!shown.length && (s.containers.length
              ? <Empty icon="search" title={t('没有匹配的容器', 'No matching containers')} />
              : <Empty icon="box" title={t('还没有容器', 'No containers yet')} action={t('运行容器', 'Run container')} onAction={() => s.setModal({ kind: 'run' })} />)}
          </div>
        </div>
      )}
      {selected && <ContainerDock s={s} c={selected} />}
      {s.modal && s.modal.kind === 'run' && <RunModal s={s} />}
    </React.Fragment>
  );
}

Object.assign(window, { ContainersPage, RunModal, fbBucket });
