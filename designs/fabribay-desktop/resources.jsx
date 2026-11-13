// Images, volumes, networks and builds: each is a table with a detail dock.

// VulnCell summarises an image scan in the table.
function VulnCell({ img }) {
  if (img.scanning) return <span className="st busy">{t('扫描中…', 'Scanning…')}</span>;
  if (!img.scan) return <span className="muted">{t('未扫描', 'Not scanned')}</span>;
  const { c, h, m, l } = img.scan;
  if (!(c + h + m + l)) return <span className="st running">{t('无漏洞', 'Clean')}</span>;
  return <span className="vulns">{c > 0 && <b className="sev Critical">{c} {t('严重', 'crit')}</b>}{h > 0 && <b className="sev High">{h} {t('高', 'high')}</b>}{m > 0 && <b className="sev Medium">{m} {t('中', 'med')}</b>}{!(c + h + m) && <b className="sev Low">{l} {t('低', 'low')}</b>}</span>;
}

// PullModal asks for a reference; the pull itself progresses in the table row.
function PullModal({ s, onPull }) {
  const [ref, setRef] = React.useState('');
  const submit = (e) => { e.preventDefault(); if (ref.trim()) { onPull(ref.trim()); s.setModal(null); } };
  return (
    <Modal title={t('拉取镜像', 'Pull an image')} onClose={() => s.setModal(null)}
      footer={<React.Fragment><button className="btn" onClick={() => s.setModal(null)}>{t('取消', 'Cancel')}</button>
        <button className="btn primary" form="pull-form" disabled={!ref.trim()}>{t('拉取', 'Pull')}</button></React.Fragment>}>
      <form id="pull-form" className="form" onSubmit={submit}>
        <label>{t('镜像', 'Image')}<input className="mono" value={ref} onChange={(e) => setRef(e.target.value)} autoFocus placeholder="ubuntu:24.04" /></label>
      </form>
    </Modal>
  );
}

// ImagesPage lists local images with their layers and security scan.
function ImagesPage({ s }) {
  const [filter, setFilter] = React.useState('all');
  const q = s.query.trim().toLowerCase();
  const used = (i) => s.imageUsers(i).length;
  const shown = s.images.filter((i) => (filter === 'all' || (filter === 'used') === used(i) > 0) && (!q || s.imageRef(i).includes(q) || i.id.includes(q)));
  useListKeys(shown.map((i) => i.id), s.selId, s.select);
  const img = s.images.find((i) => i.id === s.selId);
  const patch = (id, p) => s.setImages((is) => is.map((i) => (i.id === id ? { ...i, ...p } : i)));
  const idle = s.images.filter((i) => !used(i) && i.pull == null);

  // The scan result is derived from the image id so repeating a scan gives the same answer.
  const scan = (i) => {
    patch(i.id, { scanning: true });
    s.later(1800, () => { const h = fbHash(i.id); patch(i.id, { scanning: false, scan: { c: h % 7 === 0 ? 1 : 0, h: h % 4, m: h % 9, l: 3 + h % 17 } }); });
  };
  const pull = (ref) => {
    const [repo, tag = 'latest'] = ref.split(':');
    const id = fbId(ref + Date.now());
    s.setImages((is) => [{ id, repo, tag, size: 30 + fbHash(ref) % 300, created: 0, arch: 'arm64', scan: null, pull: 0 }, ...is]);
    const step = (p) => s.later(260, () => { patch(id, { pull: p >= 100 ? null : p }); if (p < 100) step(p + 12 + Math.random() * 14); else { s.event('pull', ref); s.toast(t('已拉取 ', 'Pulled ') + ref); } });
    step(10);
  };
  const remove = (i) => s.ask({ title: t('删除镜像 ', 'Delete image ') + s.imageRef(i) + t('？', '?'), ok: t('删除', 'Delete'), danger: true,
    run: () => { s.setImages((is) => is.filter((x) => x.id !== i.id)); s.select(null); s.toast(t('已删除 ', 'Deleted ') + s.imageRef(i)); } });
  const prune = () => s.ask({ title: t('清理 ', 'Remove ') + idle.length + t(' 个未使用的镜像？', ' unused images?'),
    body: t('可释放 ', 'Frees ') + fbSize(idle.reduce((a, i) => a + i.size, 0)), ok: t('清理', 'Remove'), danger: true,
    run: () => { const ids = new Set(idle.map((i) => i.id)); s.setImages((is) => is.filter((i) => !ids.has(i.id))); s.select(null); s.toast(t('已清理 ', 'Removed ') + idle.length + t(' 个镜像', ' images')); } });
  const layers = img ? fbLayers(img) : [];
  const vulns = img ? fbVulns(img) : [];
  return (
    <React.Fragment>
      <div className="tools">
        <Pills value={filter} onChange={setFilter} items={[['all', t('全部', 'All'), s.images.length], ['used', t('使用中', 'In use'), s.images.filter(used).length], ['unused', t('未使用', 'Unused'), s.images.filter((i) => !used(i)).length]]} />
        <span className="muted total">{fbSize(s.images.reduce((a, i) => a + i.size, 0))}</span>
        <span className="grow" />
        {idle.length > 0 && <button className="btn ghost" onClick={prune}>{t('清理未使用 ', 'Remove unused ')}{idle.length}</button>}
        <button className="btn primary" onClick={() => s.setModal({ kind: 'pull' })}><FBIcon name="download" size={13} sw={2} />{t('拉取镜像', 'Pull image')}</button>
      </div>
      {!s.dockMax && (
        <div className="table" style={{ '--cols': 'minmax(200px,1.4fr) 112px 78px 70px minmax(120px,0.8fr) 72px 78px 110px' }}>
          <div className="row head"><span>{t('仓库 : 标签', 'Repository : tag')}</span><span>ID</span><span className="num">{t('大小', 'Size')}</span><span>{t('平台', 'Platform')}</span>
            <span>{t('安全', 'Security')}</span><span>{t('容器', 'Containers')}</span><span>{t('创建', 'Created')}</span><span /></div>
          <div className="rows">
            {shown.map((i) => (
              <div key={i.id} className={'row' + (i.id === s.selId ? ' sel' : '')} onClick={() => s.select(i.id)}>
                <span className="name"><span>{i.repo}<span className="muted">:{i.tag}</span></span></span>
                <span className="muted">{i.id}</span>
                {i.pull != null
                  ? <span className="bar"><i style={{ width: i.pull + '%' }} /></span>
                  : <span className="num">{fbSize(i.size)}</span>}
                <span className={i.arch === 'amd64' ? 'st busy' : 'muted'} title={i.arch === 'amd64' ? 'Rosetta' : ''}>{i.arch}</span>
                {i.pull != null ? <span className="st busy">{t('拉取中…', 'Pulling…')}</span> : <VulnCell img={i} />}
                <span className={used(i) ? '' : 'muted'}>{used(i) || '—'}</span>
                <span className="muted">{fbSince(i.created)}</span>
                <span className="acts">
                  <IconBtn icon="play" title={t('运行', 'Run')} onClick={() => s.setModal({ kind: 'run', image: s.imageRef(i) })} />
                  <IconBtn icon="shield" title={t('扫描', 'Scan')} disabled={i.scanning} onClick={() => scan(i)} />
                  <IconBtn icon="trash" danger title={used(i) ? t('使用中', 'In use') : t('删除', 'Delete')} disabled={used(i) > 0} onClick={() => remove(i)} />
                </span>
              </div>
            ))}
            {!shown.length && <Empty icon="search" title={t('没有匹配的镜像', 'No matching images')} />}
          </div>
        </div>
      )}
      {img && (
        <Dock s={s} title={s.imageRef(img)} sub={fbSize(img.size)} tab={s.tab} onTab={s.setTab}
          tabs={[['layers', t('分层', 'Layers') + ' ' + layers.length], ['scan', t('安全扫描', 'Security')], ['info', t('信息', 'Info')]]}
          actions={<React.Fragment>
            {s.tab === 'scan' && img.scan && <button className="chip" disabled={img.scanning} onClick={() => scan(img)}>{t('重新扫描', 'Rescan')}</button>}
            <IconBtn icon="play" title={t('运行', 'Run')} onClick={() => s.setModal({ kind: 'run', image: s.imageRef(img) })} /><i className="sep" />
          </React.Fragment>}>
          {s.tab === 'layers' && (
            <div className="sub" style={{ '--cols': '28px 112px minmax(0,1fr) 120px 70px' }}>
              {layers.map((l, n) => (
                <div key={l.id} className="row"><span className="muted num">{n + 1}</span><span className="muted">{l.size ? l.id : t('元数据', 'metadata')}</span><span>{l.cmd}</span>
                  <span className="bar quiet"><i style={{ width: (l.size / img.size) * 100 + '%' }} /></span><span className="num">{l.size ? fbSize(l.size) : '0 B'}</span></div>
              ))}
            </div>
          )}
          {s.tab === 'scan' && (img.scanning ? <Empty icon="shield" title={t('扫描中…', 'Scanning…')} />
            : !img.scan ? <Empty icon="shield" title={t('尚未扫描', 'Not scanned yet')} action={t('开始扫描', 'Scan now')} onAction={() => scan(img)} />
            : !vulns.length ? <Empty icon="check" title={t('未发现已知漏洞', 'No known vulnerabilities')} />
            : (
              <div className="sub" style={{ '--cols': '150px 84px minmax(0,1fr) 110px 110px' }}>
                <div className="row head"><span>CVE</span><span>{t('级别', 'Severity')}</span><span>{t('软件包', 'Package')}</span><span>{t('当前版本', 'Installed')}</span><span>{t('修复版本', 'Fixed in')}</span></div>
                {vulns.map((v, n) => <div key={n} className="row"><span>{v.id}</span><span><b className={'sev ' + v.sev}>{v.sev}</b></span><span>{v.pkg}</span><span className="muted">{v.ver}</span><span className={v.fix ? 'st running' : 'muted'}>{v.fix || '—'}</span></div>)}
              </div>
            ))}
          {s.tab === 'info' && <KV rows={[['ID', 'sha256:' + img.id + fbId(img.id + 'x')], [t('标签', 'Tag'), s.imageRef(img)], [t('平台', 'Platform'), 'linux/' + img.arch + (img.arch === 'amd64' ? ' · Rosetta' : '')],
            [t('大小', 'Size'), fbSize(img.size), false], [t('创建', 'Created'), fbSince(img.created), false], [t('容器', 'Containers'), s.imageUsers(img).map((c) => c.name)]]} />}
        </Dock>
      )}
      {s.modal && s.modal.kind === 'pull' && <PullModal s={s} onPull={pull} />}
      {s.modal && s.modal.kind === 'run' && <RunModal s={s} />}
    </React.Fragment>
  );
}

// NameModal collects one unique name; volumes and networks both create through it.
function NameModal({ s, title, taken, onCreate, placeholder }) {
  const [name, setName] = React.useState('');
  const clash = taken.includes(name.trim());
  const bad = name.trim() && !/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(name.trim());
  const submit = (e) => { e.preventDefault(); if (name.trim() && !clash && !bad) { onCreate(name.trim()); s.setModal(null); } };
  return (
    <Modal title={title} onClose={() => s.setModal(null)}
      footer={<React.Fragment><button className="btn" onClick={() => s.setModal(null)}>{t('取消', 'Cancel')}</button>
        <button className="btn primary" form="name-form" disabled={!name.trim() || clash || bad}>{t('创建', 'Create')}</button></React.Fragment>}>
      <form id="name-form" className="form" onSubmit={submit}>
        <label>{t('名称', 'Name')}<input className="mono" value={name} onChange={(e) => setName(e.target.value)} autoFocus placeholder={placeholder} aria-invalid={clash || bad} />
          {clash && <small className="err">{t('名称已存在', 'This name is taken')}</small>}
          {bad && <small className="err">{t('只能使用字母、数字和 _ . -', 'Letters, digits and _ . - only')}</small>}</label>
      </form>
    </Modal>
  );
}

// UserList shows the containers attached to a volume or network and jumps to them.
function UserList({ s, list }) {
  if (!list.length) return <div className="files-none muted">{t('没有容器在使用', 'No containers use it')}</div>;
  return (
    <div className="sub" style={{ '--cols': 'minmax(0,1fr) minmax(0,1fr) 160px 110px' }}>
      {list.map((c) => (
        <div key={c.id} className="row click" onClick={() => { s.select(c.id, 'containers'); s.go('containers'); }}>
          <span className="name"><StateDotLite c={c} />{c.name}</span><span className="muted">{c.image}</span><span className={'st ' + fbBucket(c)}>{fbStatus(c)}</span><span className="muted">{c.ip}</span>
        </div>
      ))}
    </div>
  );
}

// StateDotLite is the bullet used outside the containers table.
function StateDotLite({ c }) { return <i className={'dot ' + (c.state === 'running' ? 'running' : c.exit ? 'failed' : 'exited')} />; }

// VolumesPage lists named volumes.
function VolumesPage({ s }) {
  const q = s.query.trim().toLowerCase();
  const shown = s.volumes.filter((v) => !q || v.name.toLowerCase().includes(q));
  useListKeys(shown.map((v) => v.name), s.selId, s.select);
  const vol = s.volumes.find((v) => v.name === s.selId);
  const idle = s.volumes.filter((v) => !s.volumeUsers(v).length);
  const remove = (v) => s.ask({ title: t('删除卷 ', 'Delete volume ') + v.name + t('？', '?'), body: t('卷里的数据无法恢复。', 'Its data cannot be recovered.'), ok: t('删除', 'Delete'), danger: true,
    run: () => { s.setVolumes((vs) => vs.filter((x) => x.name !== v.name)); s.select(null); s.toast(t('已删除 ', 'Deleted ') + v.name); } });
  const prune = () => s.ask({ title: t('清理 ', 'Remove ') + idle.length + t(' 个未使用的卷？', ' unused volumes?'), body: t('卷里的数据无法恢复。', 'Their data cannot be recovered.'), ok: t('清理', 'Remove'), danger: true,
    run: () => { const names = new Set(idle.map((v) => v.name)); s.setVolumes((vs) => vs.filter((v) => !names.has(v.name))); s.select(null); s.toast(t('已清理 ', 'Removed ') + idle.length + t(' 个卷', ' volumes')); } });
  return (
    <React.Fragment>
      <div className="tools">
        <span className="muted total">{s.volumes.length} {t('个卷', 'volumes')} · {fbSize(s.volumes.reduce((a, v) => a + v.size, 0))}</span>
        <span className="grow" />
        {idle.length > 0 && <button className="btn ghost" onClick={prune}>{t('清理未使用 ', 'Remove unused ')}{idle.length}</button>}
        <button className="btn primary" onClick={() => s.setModal({ kind: 'volume' })}><FBIcon name="plus" size={13} sw={2.2} />{t('创建卷', 'Create volume')}</button>
      </div>
      {!s.dockMax && (
        <div className="table" style={{ '--cols': 'minmax(180px,1fr) 80px 86px minmax(140px,1.2fr) 86px 60px' }}>
          <div className="row head"><span>{t('名称', 'Name')}</span><span>{t('驱动', 'Driver')}</span><span className="num">{t('大小', 'Size')}</span><span>{t('使用者', 'Used by')}</span><span>{t('创建', 'Created')}</span><span /></div>
          <div className="rows">
            {shown.map((v) => { const users = s.volumeUsers(v); return (
              <div key={v.name} className={'row' + (v.name === s.selId ? ' sel' : '')} onClick={() => s.select(v.name)}>
                <span className="name">{v.name}</span><span className="muted">{v.driver}</span><span className="num">{fbSize(v.size)}</span>
                <span className={users.length ? '' : 'muted'}>{users.map((c) => c.name).join(', ') || '—'}</span><span className="muted">{fbSince(v.created)}</span>
                <span className="acts"><IconBtn icon="trash" danger disabled={users.length > 0} title={users.length ? t('使用中', 'In use') : t('删除', 'Delete')} onClick={() => remove(v)} /></span>
              </div>); })}
            {!shown.length && <Empty icon="search" title={t('没有匹配的卷', 'No matching volumes')} />}
          </div>
        </div>
      )}
      {vol && (
        <Dock s={s} title={vol.name} sub={fbSize(vol.size)} tab={s.tab} onTab={s.setTab}
          tabs={[['info', t('信息', 'Info')], ['files', t('文件', 'Files')], ['users', t('使用者', 'Used by') + ' ' + s.volumeUsers(vol).length]]}>
          {s.tab === 'info' && <KV rows={[[t('名称', 'Name'), vol.name], [t('驱动', 'Driver'), vol.driver], [t('挂载点', 'Mountpoint'), '/var/lib/fabribay/volumes/' + vol.name + '/_data'],
            [t('大小', 'Size'), fbSize(vol.size), false], [t('创建', 'Created'), fbSince(vol.created), false]]} />}
          {s.tab === 'files' && <FileBrowser key={vol.name} root={vol.size < 1 ? {} : FB_FS.var} base={vol.name + '/'} />}
          {s.tab === 'users' && <UserList s={s} list={s.volumeUsers(vol)} />}
        </Dock>
      )}
      {s.modal && s.modal.kind === 'volume' && <NameModal s={s} title={t('创建卷', 'Create a volume')} placeholder="my_data" taken={s.volumes.map((v) => v.name)}
        onCreate={(name) => { s.setVolumes((vs) => [...vs, { name, size: 0, created: 0, driver: 'local' }]); s.select(name); s.toast(t('已创建 ', 'Created ') + name); }} />}
    </React.Fragment>
  );
}

// NetworksPage lists networks; the three built-in ones cannot be removed.
function NetworksPage({ s }) {
  const q = s.query.trim().toLowerCase();
  const shown = s.networks.filter((n) => !q || n.name.toLowerCase().includes(q));
  useListKeys(shown.map((n) => n.name), s.selId, s.select);
  const net = s.networks.find((n) => n.name === s.selId);
  const remove = (n) => s.ask({ title: t('删除网络 ', 'Delete network ') + n.name + t('？', '?'), ok: t('删除', 'Delete'), danger: true,
    run: () => { s.setNetworks((ns) => ns.filter((x) => x.name !== n.name)); s.select(null); s.toast(t('已删除 ', 'Deleted ') + n.name); } });
  const create = (name) => {
    const octet = 20 + s.networks.length;
    s.setNetworks((ns) => [...ns, { name, driver: 'bridge', subnet: '172.' + octet + '.0.0/16', gateway: '172.' + octet + '.0.1', created: 0 }]);
    s.select(name); s.toast(t('已创建 ', 'Created ') + name);
  };
  return (
    <React.Fragment>
      <div className="tools">
        <span className="muted total">{s.networks.length} {t('个网络', 'networks')}</span><span className="grow" />
        <button className="btn primary" onClick={() => s.setModal({ kind: 'network' })}><FBIcon name="plus" size={13} sw={2.2} />{t('创建网络', 'Create network')}</button>
      </div>
      {!s.dockMax && (
        <div className="table" style={{ '--cols': 'minmax(180px,1fr) 80px 150px 120px 70px 86px 60px' }}>
          <div className="row head"><span>{t('名称', 'Name')}</span><span>{t('驱动', 'Driver')}</span><span>{t('子网', 'Subnet')}</span><span>{t('网关', 'Gateway')}</span><span>{t('容器', 'Containers')}</span><span>{t('创建', 'Created')}</span><span /></div>
          <div className="rows">
            {shown.map((n) => { const users = s.networkUsers(n); const locked = n.builtin || users.length > 0; return (
              <div key={n.name} className={'row' + (n.name === s.selId ? ' sel' : '')} onClick={() => s.select(n.name)}>
                <span className="name">{n.name}{n.builtin && <em className="tag">{t('内置', 'built-in')}</em>}</span><span className="muted">{n.driver}</span><span>{n.subnet || '—'}</span><span className="muted">{n.gateway || '—'}</span>
                <span className={users.length ? '' : 'muted'}>{users.length || '—'}</span><span className="muted">{n.builtin ? '—' : fbSince(n.created)}</span>
                <span className="acts"><IconBtn icon="trash" danger disabled={locked} title={n.builtin ? t('内置网络', 'Built-in') : users.length ? t('使用中', 'In use') : t('删除', 'Delete')} onClick={() => remove(n)} /></span>
              </div>); })}
            {!shown.length && <Empty icon="search" title={t('没有匹配的网络', 'No matching networks')} />}
          </div>
        </div>
      )}
      {net && (
        <Dock s={s} title={net.name} sub={net.driver} tab={s.tab === 'users' ? 'users' : 'info'} onTab={s.setTab}
          tabs={[['info', t('信息', 'Info')], ['users', t('容器', 'Containers') + ' ' + s.networkUsers(net).length]]}>
          {s.tab !== 'users' && <KV rows={[['ID', fbId(net.name) + fbId(net.name + 'n')], [t('驱动', 'Driver'), net.driver], [t('子网', 'Subnet'), net.subnet], [t('网关', 'Gateway'), net.gateway],
            ['DNS', net.driver === 'bridge' && !net.builtin ? t('按容器名解析', 'Resolves container names') : '', false]]} />}
          {s.tab === 'users' && <UserList s={s} list={s.networkUsers(net)} />}
        </Dock>
      )}
      {s.modal && s.modal.kind === 'network' && <NameModal s={s} title={t('创建网络', 'Create a network')} placeholder="my_network" taken={s.networks.map((n) => n.name)} onCreate={create} />}
    </React.Fragment>
  );
}

// fbBuildSteps returns the steps a build has reached; a failed build stops at its seventh step.
function fbBuildSteps(b) {
  const end = b.status === 'failed' ? 7 : b.status === 'running' ? b.done + 1 : FB_STEPS.length;
  return FB_STEPS.slice(0, end).map(([cmd, dur, cached], i) => ({ cmd, dur, cached,
    state: i === end - 1 && b.status === 'failed' ? 'failed' : i === end - 1 && b.status === 'running' ? 'busy' : 'ok' }));
}

// BuildsPage is the build history, with steps and output of each record.
function BuildsPage({ s }) {
  const [filter, setFilter] = React.useState('all');
  const q = s.query.trim().toLowerCase();
  const shown = s.builds.filter((b) => (filter === 'all' || b.status === filter) && (!q || b.name.includes(q) || b.context.includes(q)));
  useListKeys(shown.map((b) => b.id), s.selId, s.select);
  const build = s.builds.find((b) => b.id === s.selId);
  const label = { ok: t('成功', 'Succeeded'), failed: t('失败', 'Failed'), running: t('构建中…', 'Building…') };
  const cls = { ok: 'running', failed: 'failed', running: 'busy' };
  const rerun = (b) => { const id = fbId(b.id + Date.now()); s.setBuilds((bs) => [{ ...b, id, status: 'running', done: 0, dur: 0, at: 0 }, ...bs]); s.select(id); };
  const clear = () => s.ask({ title: t('清除构建记录？', 'Clear the build history?'), body: t('构建缓存不受影响。', 'The build cache is kept.'), ok: t('清除', 'Clear'), danger: true,
    run: () => { s.setBuilds((bs) => bs.filter((b) => b.status === 'running')); s.select(null); } });
  const steps = build ? fbBuildSteps(build) : [];
  return (
    <React.Fragment>
      <div className="tools">
        <Pills value={filter} onChange={setFilter} items={[['all', t('全部', 'All'), s.builds.length], ['ok', t('成功', 'Succeeded'), s.builds.filter((b) => b.status === 'ok').length], ['failed', t('失败', 'Failed'), s.builds.filter((b) => b.status === 'failed').length]]} />
        <span className="grow" />
        <span className="muted">docker build →</span>
        <Seg value={s.settings.buildWith} onChange={(v) => s.setSetting('buildWith', v)} items={[['fabribay', 'FabriBay'], ['docker', 'Docker']]} />
        <button className="btn ghost" disabled={!s.builds.length} onClick={clear}>{t('清除记录', 'Clear history')}</button>
      </div>
      {!s.dockMax && (
        <div className="table" style={{ '--cols': 'minmax(160px,1fr) minmax(150px,1fr) 110px 110px 70px 100px 86px 60px' }}>
          <div className="row head"><span>{t('目标', 'Target')}</span><span>{t('上下文', 'Context')}</span><span>{t('状态', 'Status')}</span><span>{t('步骤', 'Steps')}</span><span className="num">{t('耗时', 'Duration')}</span>
            <span>{t('平台', 'Platform')}</span><span>{t('时间', 'When')}</span><span /></div>
          <div className="rows">
            {shown.map((b) => (
              <div key={b.id} className={'row' + (b.id === s.selId ? ' sel' : '')} onClick={() => s.select(b.id)}>
                <span className="name"><i className={'dot ' + cls[b.status]} />{b.name}</span><span className="muted">{b.context}</span><span className={'st ' + cls[b.status]}>{label[b.status]}</span>
                <span className="steps"><span className={'bar ' + (b.status === 'failed' ? 'bad' : '')}><i style={{ width: (fbBuildSteps(b).length / FB_STEPS.length) * 100 + '%' }} /></span>{Math.min(fbBuildSteps(b).length, FB_STEPS.length)}/{FB_STEPS.length}</span>
                <span className="num">{b.status === 'running' ? '—' : b.dur.toFixed(1) + 's'}</span><span className="muted">{b.platform}</span><span className="muted">{b.status === 'running' ? t('现在', 'now') : fbSince(b.at)}</span>
                <span className="acts"><IconBtn icon="restart" title={t('重新构建', 'Rebuild')} disabled={b.status === 'running'} onClick={() => rerun(b)} /></span>
              </div>
            ))}
            {!shown.length && <Empty icon="hammer" title={s.builds.length ? t('没有匹配的构建', 'No matching builds') : t('还没有构建记录', 'No builds yet')} />}
          </div>
        </div>
      )}
      {build && (
        <Dock s={s} title={build.name} sub={<span className={'st ' + cls[build.status]}>{label[build.status]}</span>} tab={s.tab} onTab={s.setTab}
          tabs={[['steps', t('步骤', 'Steps')], ['logs', t('输出', 'Output')], ['info', t('信息', 'Info')]]}>
          {s.tab === 'steps' && (
            <div className="sub" style={{ '--cols': '22px minmax(0,1fr) 70px 70px' }}>
              {steps.map((st, n) => (
                <div key={n} className="row"><span className={'st ' + (st.state === 'ok' ? 'running' : st.state)}><FBIcon name={st.state === 'failed' ? 'x' : st.state === 'busy' ? 'restart' : 'check'} size={13} sw={2.2} /></span>
                  <span>{st.cmd}</span><span className="muted">{st.cached ? 'CACHED' : ''}</span><span className="num">{st.state === 'busy' ? '…' : st.dur.toFixed(1) + 's'}</span></div>
              ))}
              {build.status === 'failed' && <div className="row"><span /><span className="st failed">ERR_PNPM_RECURSIVE_RUN_FIRST_FAIL  build: `tsc -p .` exited with 2</span><span /><span /></div>}
            </div>
          )}
          {s.tab === 'logs' && <pre className="logs">{steps.map((st, n) => (
            <div key={n}><span className="t">#{n + 1}</span><span className={'lv ' + (st.state === 'failed' ? 'ERROR' : st.cached ? 'INFO' : 'GET')}>{st.state === 'failed' ? 'ERROR' : st.cached ? 'CACHED' : st.state === 'busy' ? '…' : 'DONE'}</span>{st.cmd}</div>))}</pre>}
          {s.tab === 'info' && <KV rows={[['ID', build.id], [t('目标', 'Target'), build.name], [t('上下文', 'Context'), build.context], ['Dockerfile', build.context + '/Dockerfile'], [t('平台', 'Platform'), build.platform],
            [t('构建器', 'Builder'), s.settings.buildWith === 'fabribay' ? 'fabribay (BuildKit)' : 'desktop-linux']]} />}
        </Dock>
      )}
    </React.Fragment>
  );
}

Object.assign(window, { ImagesPage, VolumesPage, NetworksPage, BuildsPage });
