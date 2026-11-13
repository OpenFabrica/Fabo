// Three visual directions for the Containers page, each a 1120×720 window.

const STATE_LABEL = { running: '运行中', exited: '已停止', failed: '异常' };

// RowActions is the per-container action cluster shared by A and B.
function RowActions({ state }) {
  return (
    <span className="fb-actions">
      {state === 'running'
        ? <React.Fragment><button title="重启"><FBIcon name="restart" size={15} /></button><button title="停止"><FBIcon name="stop" size={15} /></button></React.Fragment>
        : <button title="启动"><FBIcon name="play" size={15} /></button>}
      <button title="更多"><FBIcon name="more" size={15} /></button>
    </span>
  );
}

// ───────────── A · 船坞：macOS 原生感，钴蓝强调，留白充足 ─────────────
function DirectionA({ theme = 'light' }) {
  return (
    <div className="va" data-theme={theme} data-screen-label="A 船坞 · 容器">
      <aside className="va-side">
        <FBLights />
        <div className="va-brand"><FBMark size={22} bay="var(--bay)" /><b>FabriBay</b></div>
        <nav>
          <a><FBIcon name="grid" />概览</a>
          <h6>Docker</h6>
          {FB_NAV.map((n) => (
            <a key={n.key} className={n.key === 'containers' ? 'on' : ''}><FBIcon name={n.icon} />{n.label}<em>{n.count}</em></a>
          ))}
          <h6>扩展</h6>
          <a className="dim"><FBIcon name="helm" />Kubernetes<em>关</em></a>
          <a className="dim"><FBIcon name="usb" />USB 设备</a>
          <a className="dim"><FBIcon name="share" />共享</a>
        </nav>
        <div className="va-engine">
          <div className="va-engine-top"><i className="dot ok" />引擎运行中<button title="停止引擎"><FBIcon name="stop" size={13} /></button></div>
          <div className="va-meter"><span style={{ width: '47%' }} /></div>
          <div className="va-engine-nums"><span>内存 3.8 / 8 GB</span><span>CPU 12%</span></div>
        </div>
        <a className="va-settings"><FBIcon name="sliders" />设置</a>
      </aside>
      <main className="va-main">
        <header className="va-bar">
          <h1>容器</h1>
          <div className="va-seg">
            {FB_FILTERS.map(([l, n], i) => <button key={l} className={i === 0 ? 'on' : ''}>{l}<em>{n}</em></button>)}
          </div>
          <span className="grow" />
          <label className="va-search"><FBIcon name="search" size={14} /><span>搜索容器</span><kbd>⌘K</kbd></label>
          <button className="va-primary"><FBIcon name="plus" size={14} sw={2.2} />运行</button>
        </header>
        <div className="va-table">
          <div className="va-row va-head"><span>名称</span><span>状态</span><span>端口</span><span>CPU</span><span>内存</span><span>创建</span><span /></div>
          {FB_GROUPS.map((g) => (
            <section key={g.id} className={g.compose ? 'va-group' : ''}>
              {g.compose && (
                <div className="va-ghead">
                  <FBIcon name="down" size={13} /><FBIcon name="stack" size={14} /><b>{g.name}</b>
                  <span>Compose · {g.items.filter((c) => c.state === 'running').length}/{g.items.length} 运行中</span>
                  <span className="grow" /><RowActions state="running" />
                </div>
              )}
              {g.items.map((c) => (
                <div key={c.name} className={'va-row' + (c.name === 'shop-api-1' ? ' sel' : '')}>
                  <span className="va-name"><i className={'tile ' + c.state}><FBIcon name="box" size={15} /></i><span><b>{c.name}</b><small>{c.image}</small></span></span>
                  <span className={'va-state ' + c.state}><i className="dot" />{c.status}</span>
                  <span className="mono link">{c.ports}</span>
                  <span className="mono">{c.state === 'running' ? c.cpu.toFixed(1) + '%' : '—'}</span>
                  <span className="mono">{c.mem}</span>
                  <span className="muted">{c.created}</span>
                  <RowActions state={c.state} />
                </div>
              ))}
            </section>
          ))}
        </div>
      </main>
    </div>
  );
}

// ───────────── B · 调度台：深色高密度，橙色强调，底部日志坞 ─────────────
function DirectionB({ theme = 'dark' }) {
  return (
    <div className="vb" data-theme={theme} data-screen-label="B 调度台 · 容器">
      <header className="vb-strip">
        <FBLights />
        <span className="vb-crumb"><FBMark size={16} bay="var(--bay)" />FabriBay<FBIcon name="chevron" size={12} /><b>容器</b></span>
        <label className="vb-search"><FBIcon name="search" size={13} /><span>搜索容器、镜像、卷…</span><kbd>⌘K</kbd></label>
        <span className="vb-engine"><i className="dot ok" />VM 运行中 · 4 核 · 3.8/8 GB</span>
      </header>
      <div className="vb-body">
        <aside className="vb-rail">
          <a title="概览"><FBIcon name="grid" size={18} /></a>
          {FB_NAV.map((n) => <a key={n.key} title={n.label} className={n.key === 'containers' ? 'on' : ''}><FBIcon name={n.icon} size={18} /><em>{n.label}</em></a>)}
          <hr />
          <a className="dim" title="Kubernetes"><FBIcon name="helm" size={18} /></a>
          <a className="dim" title="USB"><FBIcon name="usb" size={18} /></a>
          <a className="dim" title="共享"><FBIcon name="share" size={18} /></a>
          <span className="grow" />
          <a title="设置"><FBIcon name="sliders" size={18} /></a>
        </aside>
        <main className="vb-main">
          <div className="vb-tools">
            {FB_FILTERS.map(([l, n], i) => <button key={l} className={'vb-pill' + (i === 0 ? ' on' : '')}>{l} <em>{n}</em></button>)}
            <span className="grow" />
            <button className="vb-ghost">清理 1 个异常</button>
            <button className="vb-primary"><FBIcon name="plus" size={13} sw={2.2} />运行容器</button>
          </div>
          <div className="vb-table">
            <div className="vb-row vb-head"><span>名称</span><span>镜像</span><span>状态</span><span>端口</span><span>CPU</span><span /><span>内存</span><span>创建</span><span /></div>
            {FB_GROUPS.map((g) => (
              <React.Fragment key={g.id}>
                {g.compose && (
                  <div className="vb-ghead"><FBIcon name="down" size={12} /><b>{g.name}</b><span>compose · {g.items.filter((c) => c.state === 'running').length}/{g.items.length}</span></div>
                )}
                {g.items.map((c, i) => (
                  <div key={c.name} className={'vb-row' + (c.name === 'shop-api-1' ? ' sel' : '') + (g.compose ? ' child' : '') + (g.compose && i === g.items.length - 1 ? ' last' : '')}>
                    <span className="vb-name"><i className={'dot ' + c.state} />{c.name}</span>
                    <span className="muted">{c.image}</span>
                    <span className={'st ' + c.state}>{c.status}</span>
                    <span className="link">{c.ports}</span>
                    <span className="num">{c.state === 'running' ? c.cpu.toFixed(1) + '%' : '—'}</span>
                    <FBSpark values={c.spark} height={14} />
                    <span className="num">{c.mem}</span>
                    <span className="muted">{c.created}</span>
                    <RowActions state={c.state} />
                  </div>
                ))}
              </React.Fragment>
            ))}
          </div>
          <section className="vb-dock">
            <div className="vb-dock-bar">
              <b>shop-api-1</b>
              <nav><a>信息</a><a className="on">日志</a><a>终端</a><a>文件</a></nav>
              <span className="grow" /><span className="muted">跟随中</span>
            </div>
            <pre>{FB_LOGS.slice(0, 7).map(([t, lv, m], i) => <div key={i}><span className="t">{t}</span><span className={'lv ' + lv}>{lv}</span>{m}</div>)}</pre>
          </section>
        </main>
      </div>
      <footer className="vb-status"><span>v0.4.0</span><span>8 / 11 运行中</span><span>docker → fabribay</span><span className="grow" /><span>↓ 1.2 MB/s　↑ 84 KB/s</span></footer>
    </div>
  );
}

// ───────────── C · 货单：顶部页签 + 主从分栏，纸面与墨色，直角 ─────────────
function DirectionC({ theme = 'light' }) {
  return (
    <div className="vc" data-theme={theme} data-screen-label="C 货单 · 容器">
      <header className="vc-top">
        <FBLights />
        <span className="vc-brand"><FBMark size={20} bay="var(--bay)" /></span>
        <nav>
          <a>概览</a>
          {FB_NAV.map((n) => <a key={n.key} className={n.key === 'containers' ? 'on' : ''}>{n.label}<sup>{n.count}</sup></a>)}
          <a className="dim">Kubernetes</a>
        </nav>
        <span className="grow" />
        <label className="vc-search"><FBIcon name="search" size={14} /><span>搜索</span><kbd>⌘K</kbd></label>
        <span className="vc-engine"><i className="dot ok" />引擎 3.8 / 8 GB</span>
        <a className="vc-gear"><FBIcon name="sliders" size={17} /></a>
      </header>
      <div className="vc-body">
        <aside className="vc-list">
          <div className="vc-filters">{FB_FILTERS.map(([l, n], i) => <button key={l} className={i === 0 ? 'on' : ''}>{l} {n}</button>)}</div>
          {FB_GROUPS.map((g) => (
            <section key={g.id}>
              <h6>{g.compose ? <React.Fragment><b>{g.name}</b><span>Compose · {g.items.filter((c) => c.state === 'running').length}/{g.items.length}</span></React.Fragment> : <b>独立容器</b>}</h6>
              {g.items.map((c) => (
                <a key={c.name} className={'vc-item' + (c.name === 'shop-api-1' ? ' sel' : '')}>
                  <i className={'bar ' + c.state} />
                  <span><b>{c.name}</b><small>{c.image}</small></span>
                  <span className="r"><b>{c.ports || STATE_LABEL[c.state]}</b><small>{c.status}</small></span>
                </a>
              ))}
            </section>
          ))}
        </aside>
        <main className="vc-detail">
          <div className="vc-title">
            <div><small>shop · Compose</small><h1>shop-api-1</h1><p><i className="dot ok" />已运行 4 分钟　·　shop/api:dev　·　<span className="link">shop-api-1.fabribay.local</span></p></div>
            <div className="vc-btns"><button><FBIcon name="restart" size={15} />重启</button><button><FBIcon name="stop" size={15} />停止</button><button><FBIcon name="out" size={15} />打开</button><button className="icon"><FBIcon name="more" size={15} /></button></div>
          </div>
          <div className="vc-stats">
            <div><small>CPU</small><b>12.6<u>%</u></b><FBSpark values={FB_GROUPS[0].items[1].spark} height={22} /></div>
            <div><small>内存</small><b>212<u>MB</u></b><FBSpark values={[8, 8, 9, 9, 10, 10, 10, 11, 11, 11, 12, 12]} height={22} /></div>
            <div><small>网络</small><b>1.2<u>MB/s</u></b><FBSpark values={[2, 5, 3, 8, 6, 4, 9, 12, 7, 5, 8, 6]} height={22} /></div>
            <div><small>端口</small><b className="link">3000<u>→ 3000</u></b></div>
          </div>
          <nav className="vc-tabs"><a>信息</a><a className="on">日志</a><a>终端</a><a>文件</a><span className="grow" /><span className="muted">跟随中</span></nav>
          <pre className="vc-logs">{FB_LOGS.map(([t, lv, m], i) => <div key={i}><span className="t">{t}</span><span className={'lv ' + lv}>{lv}</span>{m}</div>)}</pre>
        </main>
      </div>
    </div>
  );
}

Object.assign(window, { DirectionA, DirectionB, DirectionC });
