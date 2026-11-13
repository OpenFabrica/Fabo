// Shared primitives: icons, brand mark, table helpers, dock, dialogs, menus.

// Line icons on a 24 grid; stroke comes from currentColor.
const FB_ICON_PATHS = {
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
  x: <path d="M6 6l12 12M18 6L6 18" />,
  check: <path d="M5 12.5l4.5 4.5L19 7.5" />,
  copy: <g><rect x="8" y="8" width="12" height="12" rx="2" /><path d="M16 8V5a1 1 0 00-1-1H5a1 1 0 00-1 1v10a1 1 0 001 1h3" /></g>,
  folder: <path d="M3 7a1 1 0 011-1h5l2 2h8a1 1 0 011 1v9a1 1 0 01-1 1H4a1 1 0 01-1-1V7z" />,
  file: <path d="M7 3h7l4 4v14H7zM14 3v4h4" />,
  download: <path d="M12 4v11M8 11l4 4 4-4M5 20h14" />,
  shield: <path d="M12 3l7 3v5c0 4.5-3 8-7 10-4-2-7-5.5-7-10V6l7-3z" />,
  expand: <path d="M5 15l7-7 7 7" />,
  collapse: <path d="M5 9l7 7 7-7" />,
  sun: <g><circle cx="12" cy="12" r="4" /><path d="M12 2.5v2.5M12 19v2.5M2.5 12H5M19 12h2.5M5.3 5.3l1.8 1.8M16.9 16.9l1.8 1.8M5.3 18.7l1.8-1.8M16.9 7.1l1.8-1.8" /></g>,
  moon: <path d="M20 14.5A8 8 0 019.5 4a8 8 0 1010.5 10.5z" />,
  broom: <path d="M14 4l6 6M12 8l4 4-7 8H4v-5l8-7zM8 12l4 4" />,
  stack: <path d="M4 7h16M4 12h16M4 17h16" />,
  sidebar: <g><rect x="3.5" y="5" width="17" height="14" rx="2.5" /><path d="M9.5 5v14" /></g>,
  power: <path d="M12 3v9M7 6.5a7.5 7.5 0 1010 0" />,
};

// FBIcon renders one of FB_ICON_PATHS at the given pixel size.
function FBIcon({ name, size = 16, sw = 1.7 }) {
  return (
    <svg className="icon" width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor"
      strokeWidth={sw} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{FB_ICON_PATHS[name]}</svg>
  );
}

// FBMark draws the FabriBay mark: a cobalt bay holding an orange container.
function FBMark({ size = 20 }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" aria-hidden="true" className="mark">
      <path d="M3 4h5v12h8V4h5v14l-3 3H6l-3-3z" fill="var(--bay)" />
      <path d="M9.5 7.5h3.5l1.5 1.5v5.5h-5z" fill="#FF6A1F" />
    </svg>
  );
}

// FBSpark draws a CPU history as bars scaled to its own peak (with a floor so idle stays flat).
function FBSpark({ values, height = 14, count = 12 }) {
  const v = values.slice(-count);
  if (!v.length) return <span className="spark off" style={{ height }} />;
  const max = Math.max(4, ...v);
  return <span className="spark" style={{ height }}>{v.map((x, i) => <i key={i} style={{ height: Math.max(2, Math.round((x / max) * height)) }} />)}</span>;
}

// IconBtn is a square icon button that never lets the click reach the row behind it.
function IconBtn({ icon, title, onClick, danger, disabled }) {
  return (
    <button className={'ibtn' + (danger ? ' danger' : '')} title={title} aria-label={title} disabled={disabled}
      onClick={(e) => { e.stopPropagation(); onClick && onClick(e); }}><FBIcon name={icon} size={15} /></button>
  );
}

// Pills is the filter row above a table: [key, label, count] triples.
function Pills({ items, value, onChange }) {
  return items.map(([k, label, n]) => (
    <button key={k} className={'pill' + (value === k ? ' on' : '')} onClick={() => onChange(k)}>{label}{n != null && <em>{n}</em>}</button>
  ));
}

// Menu is the "more" popover. It is positioned with fixed coordinates so the scrolling table cannot clip it.
function Menu({ items }) {
  const [pos, setPos] = React.useState(null);
  React.useEffect(() => {
    if (!pos) return;
    const close = () => setPos(null);
    window.addEventListener('pointerdown', close);
    window.addEventListener('keydown', close);
    return () => { window.removeEventListener('pointerdown', close); window.removeEventListener('keydown', close); };
  }, [pos]);
  const open = (e) => {
    e.stopPropagation();
    const r = e.currentTarget.getBoundingClientRect();
    // Flip upward when the menu would run past the bottom of the viewport.
    const h = items.length * 28 + 10;
    setPos(pos ? null : { right: window.innerWidth - r.right, top: r.bottom + h > window.innerHeight ? r.top - h - 2 : r.bottom + 2 });
  };
  return (
    <React.Fragment>
      <button className="ibtn" title={t('更多', 'More')} aria-label={t('更多', 'More')} onClick={open} onPointerDown={(e) => e.stopPropagation()}><FBIcon name="more" size={15} /></button>
      {pos && (
        <div className="menu" style={pos} onPointerDown={(e) => e.stopPropagation()}>
          {items.filter(Boolean).map((it, i) => it === '-' ? <hr key={i} /> : (
            <button key={i} className={it.danger ? 'danger' : ''} disabled={it.disabled} onClick={(e) => { e.stopPropagation(); setPos(null); it.run(); }}>
              <FBIcon name={it.icon} size={14} />{it.label}
            </button>
          ))}
        </div>
      )}
    </React.Fragment>
  );
}

// Dock is the detail panel pinned under a table: title, tabs, actions, and a drag handle to resize it.
function Dock({ s, title, sub, tabs, tab, onTab, actions, children }) {
  const drag = (e) => {
    e.preventDefault();
    const y0 = e.clientY, h0 = s.dockH;
    const move = (ev) => s.setDockH(Math.max(140, Math.min(520, h0 + y0 - ev.clientY)));
    const up = () => { window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up); };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
  };
  return (
    <section className={'dock' + (s.dockMax ? ' max' : '')} style={s.dockMax ? null : { height: s.dockH }}>
      <div className="dock-grip" onPointerDown={drag} />
      <div className="dock-bar">
        <b className="dock-title">{title}</b>
        {sub && <span className="muted">{sub}</span>}
        <nav role="tablist">{tabs.map(([k, label]) => <button key={k} role="tab" aria-selected={tab === k} className={tab === k ? 'on' : ''} onClick={() => onTab(k)}>{label}</button>)}</nav>
        <span className="grow" />
        {actions}
        <IconBtn icon={s.dockMax ? 'collapse' : 'expand'} title={s.dockMax ? t('还原', 'Restore') : t('最大化', 'Maximize')} onClick={() => s.setDockMax(!s.dockMax)} />
        <IconBtn icon="x" title={t('关闭', 'Close')} onClick={() => s.select(null)} />
      </div>
      <div className="dock-body">{children}</div>
    </section>
  );
}

// KV renders a definition grid for the "info" tabs; rows are [label, value, mono?].
function KV({ rows }) {
  return (
    <dl className="kv">{rows.filter(Boolean).map(([k, v, mono], i) => (
      <React.Fragment key={i}><dt>{k}</dt><dd className={mono === false ? '' : 'mono'}>{Array.isArray(v) ? (v.length ? v.map((x, j) => <div key={j}>{x}</div>) : '—') : (v || '—')}</dd></React.Fragment>
    ))}</dl>
  );
}

// Empty is the centered state for an empty list, a stopped engine, or a page that is not built yet.
function Empty({ icon, title, action, onAction }) {
  return (
    <div className="empty"><FBIcon name={icon} size={30} sw={1.3} /><b>{title}</b>
      {action && <button className="btn" onClick={onAction}>{action}</button>}</div>
  );
}

// Modal is a centered dialog inside the window; Esc and the backdrop close it.
function Modal({ title, onClose, children, footer, width = 440 }) {
  React.useEffect(() => {
    const key = (e) => { if (e.key === 'Escape') onClose(); };
    window.addEventListener('keydown', key);
    return () => window.removeEventListener('keydown', key);
  }, []);
  return (
    <div className="scrim" onPointerDown={onClose}>
      <div className="modal" role="dialog" aria-modal="true" aria-label={title} style={{ width }} onPointerDown={(e) => e.stopPropagation()}>
        <header><b>{title}</b><IconBtn icon="x" title={t('关闭', 'Close')} onClick={onClose} /></header>
        <div className="modal-body">{children}</div>
        {footer && <footer>{footer}</footer>}
      </div>
    </div>
  );
}

// Toggle is a switch with an accessible state.
function Toggle({ on, onChange, label }) {
  return <button role="switch" aria-checked={on} aria-label={label} className={'toggle' + (on ? ' on' : '')} onClick={() => onChange(!on)}><i /></button>;
}

// Seg is a segmented control over [key, label] pairs.
function Seg({ items, value, onChange }) {
  return <div className="seg">{items.map(([k, label]) => <button key={k} className={value === k ? 'on' : ''} onClick={() => onChange(k)}>{label}</button>)}</div>;
}

// FileBrowser walks FB_FS-shaped trees: breadcrumb on top, folders first.
function FileBrowser({ root, base = '/' }) {
  const [path, setPath] = React.useState([]);
  let dir = root;
  for (const p of path) dir = dir[p];
  const names = Object.keys(dir).sort((a, b) => (Array.isArray(dir[a]) - Array.isArray(dir[b])) || a.localeCompare(b));
  return (
    <div className="files">
      <div className="crumbs mono">
        <button onClick={() => setPath([])}>{base}</button>
        {path.map((p, i) => <React.Fragment key={i}><button onClick={() => setPath(path.slice(0, i + 1))}>{p}</button><span>/</span></React.Fragment>)}
      </div>
      {names.length === 0 && <div className="files-none muted">{t('空目录', 'Empty directory')}</div>}
      {names.map((n) => {
        const isFile = Array.isArray(dir[n]);
        return (
          <div key={n} className={'frow' + (isFile ? '' : ' dir')} onClick={() => !isFile && setPath([...path, n])}>
            <FBIcon name={isFile ? 'file' : 'folder'} size={14} /><span className="mono">{n}</span>
            <span className="num muted">{isFile ? (dir[n][0] > 1048576 ? (dir[n][0] / 1048576).toFixed(1) + ' MB' : dir[n][0] > 1024 ? (dir[n][0] / 1024).toFixed(1) + ' KB' : dir[n][0] + ' B') : ''}</span>
            <span className="muted">{isFile ? fbSince(dir[n][1]) : ''}</span>
          </div>
        );
      })}
    </div>
  );
}

// useListKeys moves the selection with ↑/↓ and clears it with Esc, unless focus is in a text field.
function useListKeys(ids, sel, select) {
  React.useEffect(() => {
    const key = (e) => {
      if (/INPUT|TEXTAREA|SELECT/.test(document.activeElement.tagName) || document.querySelector('.scrim, .onboard')) return;
      if (e.key === 'Escape') return select(null);
      if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
      e.preventDefault();
      const i = ids.indexOf(sel);
      const next = ids[Math.max(0, Math.min(ids.length - 1, i < 0 ? 0 : i + (e.key === 'ArrowDown' ? 1 : -1)))];
      if (next != null) select(next);
      requestAnimationFrame(() => { const el = document.querySelector('.rows > .row.sel'); if (el) el.scrollIntoView({ block: 'nearest' }); });
    };
    window.addEventListener('keydown', key);
    return () => window.removeEventListener('keydown', key);
  });
}

Object.assign(window, { FBIcon, FBMark, FBSpark, IconBtn, Pills, Menu, Dock, KV, Empty, Modal, Toggle, Seg, FileBrowser, useListKeys });
