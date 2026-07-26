'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import {
  api,
  ApiError,
  getToken,
  setToken,
  clearToken,
  type App,
  type CodeInfo,
  type Proxy,
  type Session,
  type SessionState,
  type Token,
  type Worker,
} from '../lib/api';

// ---------------------------------------------------------------------------
// Root: gate on a token, then render the console.
// ---------------------------------------------------------------------------

export default function Page() {
  const [authed, setAuthed] = useState<boolean | null>(null);

  useEffect(() => {
    setAuthed(!!getToken());
  }, []);

  if (authed === null) return null; // avoid SSR/localStorage flash

  if (!authed) return <Gate onDone={() => setAuthed(true)} />;
  return <Console onSignOut={() => setAuthed(false)} />;
}

// ---------------------------------------------------------------------------
// Token gate
// ---------------------------------------------------------------------------

function Gate({ onDone }: { onDone: () => void }) {
  const [value, setValue] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!value.trim()) return;
    setBusy(true);
    setErr('');
    setToken(value.trim());
    try {
      await api('GET', '/v1/workers'); // validates the (master) token
      onDone();
    } catch (e) {
      clearToken();
      setErr(e instanceof ApiError ? e.message : 'Connection error');
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="gate">
      <form className="card" onSubmit={submit}>
        <h1>TG Control API Console</h1>
        <p>Enter the master API token to sign in.</p>
        <label className="field">
          <span>API token</span>
          <input
            type="password"
            value={value}
            autoFocus
            onChange={(e) => setValue(e.target.value)}
            placeholder="Bearer token"
          />
        </label>
        {err && <div className="err">{err}</div>}
        <button className="btn" style={{ width: '100%' }} disabled={busy}>
          {busy ? 'Checking…' : 'Sign in'}
        </button>
      </form>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Console
// ---------------------------------------------------------------------------

type Tab = 'sessions' | 'apps' | 'proxies' | 'tokens';

type Modal =
  | null
  | { kind: 'app' }
  | { kind: 'proxy' }
  | { kind: 'bot' }
  | { kind: 'user' }
  | { kind: 'login'; id: string; status: string }
  | { kind: 'webhook'; session: Session }
  | { kind: 'delete'; session: Session }
  | { kind: 'editSession'; session: Session }
  | { kind: 'rename'; entity: 'apps' | 'proxies'; id: string; label: string }
  | { kind: 'token'; token?: Token }
  | { kind: 'secret'; secret: string };

function Console({ onSignOut }: { onSignOut: () => void }) {
  const [tab, setTab] = useState<Tab>('sessions');
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [apps, setApps] = useState<App[]>([]);
  const [proxies, setProxies] = useState<Proxy[]>([]);
  const [tokens, setTokens] = useState<Token[]>([]);
  const [modal, setModal] = useState<Modal>(null);
  const [toast, setToast] = useState<{ msg: string; err?: boolean } | null>(null);

  const flash = useCallback((msg: string, err = false) => {
    setToast({ msg, err });
    window.setTimeout(() => setToast(null), 3200);
  }, []);

  const signOut = useCallback(() => {
    clearToken();
    onSignOut();
  }, [onSignOut]);

  const refresh = useCallback(async () => {
    try {
      const [w, s, a, p, t] = await Promise.all([
        api<Worker[]>('GET', '/v1/workers'),
        api<Session[]>('GET', '/v1/sessions'),
        api<App[]>('GET', '/v1/apps'),
        api<Proxy[]>('GET', '/v1/proxies'),
        api<Token[]>('GET', '/v1/tokens'),
      ]);
      setWorkers(w || []);
      setSessions(s || []);
      setApps(a || []);
      setProxies(p || []);
      setTokens(t || []);
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        signOut();
        return;
      }
      // transient network error — keep last data
    }
  }, [signOut]);

  useEffect(() => {
    refresh();
    const t = window.setInterval(refresh, 4000);
    return () => window.clearInterval(t);
  }, [refresh]);

  return (
    <div className="wrap">
      <header className="top">
        <h1>
          TG Control API <span className="dim">Console</span>
        </h1>
        <div className="spacer" />
        <button className="btn ghost sm" onClick={() => refresh()}>
          Refresh
        </button>
        <button className="btn ghost sm" style={{ marginLeft: 8 }} onClick={signOut}>
          Sign out
        </button>
      </header>

      <WorkerStrip workers={workers} />

      <div className="tabs">
        <button className={tab === 'sessions' ? 'active' : ''} onClick={() => setTab('sessions')}>
          Sessions ({sessions.length})
        </button>
        <button className={tab === 'apps' ? 'active' : ''} onClick={() => setTab('apps')}>
          Apps ({apps.length})
        </button>
        <button className={tab === 'proxies' ? 'active' : ''} onClick={() => setTab('proxies')}>
          Proxies ({proxies.length})
        </button>
        <button className={tab === 'tokens' ? 'active' : ''} onClick={() => setTab('tokens')}>
          Tokens ({tokens.length})
        </button>
      </div>

      {tab === 'sessions' && (
        <SessionsTab
          sessions={sessions}
          onCreateBot={() => setModal({ kind: 'bot' })}
          onCreateUser={() => setModal({ kind: 'user' })}
          onLogin={(s) => setModal({ kind: 'login', id: s.id, status: s.status })}
          onEdit={(s) => setModal({ kind: 'editSession', session: s })}
          onWebhook={(s) => setModal({ kind: 'webhook', session: s })}
          onDelete={(s) => setModal({ kind: 'delete', session: s })}
        />
      )}
      {tab === 'apps' && (
        <AppsTab
          apps={apps}
          onCreate={() => setModal({ kind: 'app' })}
          onRename={(a) => setModal({ kind: 'rename', entity: 'apps', id: a.id, label: a.label || '' })}
        />
      )}
      {tab === 'proxies' && (
        <ProxiesTab
          proxies={proxies}
          onCreate={() => setModal({ kind: 'proxy' })}
          onRename={(p) =>
            setModal({ kind: 'rename', entity: 'proxies', id: p.id, label: p.label || '' })
          }
        />
      )}
      {tab === 'tokens' && (
        <TokensTab
          tokens={tokens}
          apps={apps}
          sessions={sessions}
          onCreate={() => setModal({ kind: 'token' })}
          onEdit={(t) => setModal({ kind: 'token', token: t })}
          onDelete={async (t) => {
            try {
              await api('DELETE', `/v1/tokens/${t.id}`);
              flash('Token deleted');
              refresh();
            } catch (e) {
              flash(e instanceof ApiError ? e.message : 'Error', true);
            }
          }}
          onToggle={async (t) => {
            try {
              await api('PATCH', `/v1/tokens/${t.id}`, { enabled: !t.enabled });
              refresh();
            } catch (e) {
              flash(e instanceof ApiError ? e.message : 'Error', true);
            }
          }}
        />
      )}

      {modal?.kind === 'app' && (
        <AppModal onClose={() => setModal(null)} onDone={refresh} flash={flash} />
      )}
      {modal?.kind === 'proxy' && (
        <ProxyModal onClose={() => setModal(null)} onDone={refresh} flash={flash} />
      )}
      {modal?.kind === 'bot' && (
        <BotModal
          apps={apps}
          proxies={proxies}
          onClose={() => setModal(null)}
          onDone={refresh}
          flash={flash}
        />
      )}
      {modal?.kind === 'user' && (
        <UserModal
          apps={apps}
          proxies={proxies}
          onClose={() => setModal(null)}
          onDone={refresh}
          onLogin={(id, status) => setModal({ kind: 'login', id, status })}
          flash={flash}
        />
      )}
      {modal?.kind === 'login' && (
        <LoginModal
          id={modal.id}
          initialStatus={modal.status}
          onClose={() => setModal(null)}
          onDone={refresh}
          flash={flash}
        />
      )}
      {modal?.kind === 'editSession' && (
        <EditSessionModal
          session={modal.session}
          proxies={proxies}
          onClose={() => setModal(null)}
          onDone={refresh}
          flash={flash}
        />
      )}
      {modal?.kind === 'webhook' && (
        <WebhookModal
          session={modal.session}
          onClose={() => setModal(null)}
          onDone={refresh}
          flash={flash}
        />
      )}
      {modal?.kind === 'delete' && (
        <DeleteModal
          session={modal.session}
          onClose={() => setModal(null)}
          onDone={refresh}
          flash={flash}
        />
      )}
      {modal?.kind === 'rename' && (
        <RenameModal
          entity={modal.entity}
          id={modal.id}
          current={modal.label}
          onClose={() => setModal(null)}
          onDone={refresh}
          flash={flash}
        />
      )}
      {modal?.kind === 'token' && (
        <TokenModal
          token={modal.token}
          apps={apps}
          sessions={sessions}
          onClose={() => setModal(null)}
          onDone={refresh}
          onSecret={(secret) => setModal({ kind: 'secret', secret })}
          flash={flash}
        />
      )}
      {modal?.kind === 'secret' && (
        <SecretModal secret={modal.secret} onClose={() => setModal(null)} />
      )}

      {toast && <div className={'toast' + (toast.err ? ' err' : '')}>{toast.msg}</div>}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Worker strip
// ---------------------------------------------------------------------------

function WorkerStrip({ workers }: { workers: Worker[] }) {
  return (
    <div className="section">
      <div className="section-head">
        <h2>Workers</h2>
      </div>
      {workers.length === 0 ? (
        <div className="card empty">No live workers</div>
      ) : (
        <div className="workers">
          {workers.map((w) => (
            <div className="worker" key={w.id}>
              <div className="wid">
                <span className={'dot ' + (w.alive ? 'on' : 'off')} />
                {w.id}
              </div>
              <div className="meta">{w.addr}</div>
              <div className="meta">
                {w.sessions} sessions · {w.alive ? 'alive' : 'dead'} · {relTime(w.last_seen_at)}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Sessions tab
// ---------------------------------------------------------------------------

type SessionFilter = 'all' | 'bot' | 'user';

function SessionsTab(props: {
  sessions: Session[];
  onCreateBot: () => void;
  onCreateUser: () => void;
  onLogin: (s: Session) => void;
  onEdit: (s: Session) => void;
  onWebhook: (s: Session) => void;
  onDelete: (s: Session) => void;
}) {
  const { sessions, onCreateBot, onCreateUser, onLogin, onEdit, onWebhook, onDelete } = props;
  const [filter, setFilter] = useState<SessionFilter>('all');
  const shown = sessions.filter((s) => filter === 'all' || s.kind === filter);

  return (
    <div className="section">
      <div className="section-head">
        <h2>Sessions</h2>
        <div className="segmented">
          {(['all', 'bot', 'user'] as SessionFilter[]).map((f) => (
            <button
              key={f}
              className={filter === f ? 'active' : ''}
              onClick={() => setFilter(f)}
            >
              {f === 'all' ? 'All' : f === 'bot' ? 'Bots' : 'Users'}
            </button>
          ))}
        </div>
        <div className="spacer" />
        <button className="btn sm" onClick={onCreateBot}>
          + Bot
        </button>
        <button className="btn sm" style={{ marginLeft: 8 }} onClick={onCreateUser}>
          + User
        </button>
      </div>
      <div className="card" style={{ padding: 0 }}>
        {shown.length === 0 ? (
          <div className="empty">No sessions</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Kind</th>
                <th>Label / phone</th>
                <th>Status</th>
                <th>App</th>
                <th>Proxy</th>
                <th>Worker</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {shown.map((s) => {
                const awaiting =
                  s.status === 'awaiting_code' || s.status === 'awaiting_password';
                return (
                  <tr key={s.id}>
                    <td>
                      <span className={'badge kind-' + s.kind}>{s.kind}</span>
                    </td>
                    <td>
                      <div>{s.label || <span className="mono">—</span>}</div>
                      {s.phone && <div className="mono">{s.phone}</div>}
                      <div>
                        <CopyID id={s.id} />
                      </div>
                    </td>
                    <td className={'status-' + s.status}>{s.status}</td>
                    <td>{s.app_label || <span className="mono">{s.app_id.slice(0, 8)}</span>}</td>
                    <td>
                      {s.proxy ? (
                        <span className="mono">{s.proxy}</span>
                      ) : (
                        <span className="mono">—</span>
                      )}
                    </td>
                    <td className="mono">{s.worker_id || '—'}</td>
                    <td>
                      <div className="row-actions">
                        {awaiting && (
                          <button className="btn sm" onClick={() => onLogin(s)}>
                            Log in
                          </button>
                        )}
                        <button className="btn ghost sm" onClick={() => onEdit(s)}>
                          Edit
                        </button>
                        <button className="btn ghost sm" onClick={() => onWebhook(s)}>
                          Webhook
                        </button>
                        <button className="btn danger sm" onClick={() => onDelete(s)}>
                          Delete
                        </button>
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Apps tab
// ---------------------------------------------------------------------------

function AppsTab({
  apps,
  onCreate,
  onRename,
}: {
  apps: App[];
  onCreate: () => void;
  onRename: (a: App) => void;
}) {
  return (
    <div className="section">
      <div className="section-head">
        <h2>Apps</h2>
        <div className="spacer" />
        <button className="btn sm" onClick={onCreate}>
          + App
        </button>
      </div>
      <div className="card" style={{ padding: 0 }}>
        {apps.length === 0 ? (
          <div className="empty">No apps</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Label</th>
                <th>api_id</th>
                <th>id</th>
                <th>Created</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {apps.map((a) => (
                <tr key={a.id}>
                  <td>{a.label || '—'}</td>
                  <td className="mono">{a.api_id}</td>
                  <td className="mono">{a.id}</td>
                  <td className="mono">{relTime(a.created_at)}</td>
                  <td>
                    <div className="row-actions">
                      <button className="btn ghost sm" onClick={() => onRename(a)}>
                        Rename
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Proxies tab
// ---------------------------------------------------------------------------

function ProxiesTab({
  proxies,
  onCreate,
  onRename,
}: {
  proxies: Proxy[];
  onCreate: () => void;
  onRename: (p: Proxy) => void;
}) {
  return (
    <div className="section">
      <div className="section-head">
        <h2>Proxies</h2>
        <div className="spacer" />
        <button className="btn sm" onClick={onCreate}>
          + Proxy
        </button>
      </div>
      <div className="card" style={{ padding: 0 }}>
        {proxies.length === 0 ? (
          <div className="empty">No proxies</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Label</th>
                <th>Type</th>
                <th>Address</th>
                <th>User</th>
                <th>id</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {proxies.map((p) => (
                <tr key={p.id}>
                  <td>{p.label || '—'}</td>
                  <td>{p.type}</td>
                  <td className="mono">
                    {p.host}:{p.port}
                  </td>
                  <td className="mono">{p.username || '—'}</td>
                  <td>
                    <CopyID id={p.id} />
                  </td>
                  <td>
                    <div className="row-actions">
                      <button className="btn ghost sm" onClick={() => onRename(p)}>
                        Rename
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Tokens tab
// ---------------------------------------------------------------------------

function TokensTab({
  tokens,
  apps,
  sessions,
  onCreate,
  onEdit,
  onDelete,
  onToggle,
}: {
  tokens: Token[];
  apps: App[];
  sessions: Session[];
  onCreate: () => void;
  onEdit: (t: Token) => void;
  onDelete: (t: Token) => void;
  onToggle: (t: Token) => void;
}) {
  const appName = (id: string) => apps.find((a) => a.id === id)?.label || id.slice(0, 8);
  const sessName = (id: string) => {
    const s = sessions.find((x) => x.id === id);
    return s ? s.label || s.id.slice(0, 8) : id.slice(0, 8);
  };
  const scope = (t: Token) => {
    if (t.all_sessions) return 'all sessions';
    const parts: string[] = [];
    if (t.app_ids.length) parts.push('apps: ' + t.app_ids.map(appName).join(', '));
    if (t.session_ids.length) parts.push('sessions: ' + t.session_ids.map(sessName).join(', '));
    return parts.length ? parts.join(' · ') : 'no access';
  };

  return (
    <div className="section">
      <div className="section-head">
        <h2>API tokens</h2>
        <div className="spacer" />
        <button className="btn sm" onClick={onCreate}>
          + Token
        </button>
      </div>
      <div className="hint" style={{ marginTop: 0 }}>
        Scoped tokens may only call a session&apos;s API and read its status, limited to the
        scope below. Management stays on the master token.
      </div>
      <div className="card" style={{ padding: 0 }}>
        {tokens.length === 0 ? (
          <div className="empty">No tokens</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Enabled</th>
                <th>Scope</th>
                <th>Created</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {tokens.map((t) => (
                <tr key={t.id}>
                  <td>{t.name || <span className="mono">{t.id.slice(0, 8)}</span>}</td>
                  <td>
                    <button
                      className={'toggle ' + (t.enabled ? 'on' : 'off')}
                      onClick={() => onToggle(t)}
                      title="Toggle enabled"
                    >
                      {t.enabled ? 'enabled' : 'disabled'}
                    </button>
                  </td>
                  <td style={{ maxWidth: 360 }}>{scope(t)}</td>
                  <td className="mono">{relTime(t.created_at)}</td>
                  <td>
                    <div className="row-actions">
                      <button className="btn ghost sm" onClick={() => onEdit(t)}>
                        Edit
                      </button>
                      <button className="btn danger sm" onClick={() => onDelete(t)}>
                        Delete
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Modal shell + submit helper
// ---------------------------------------------------------------------------

function Modal({
  title,
  onClose,
  children,
}: {
  title: string;
  onClose: () => void;
  children: React.ReactNode;
}) {
  return (
    <div className="overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>{title}</h3>
        {children}
      </div>
    </div>
  );
}

type Flash = (msg: string, err?: boolean) => void;

function useSubmit() {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const run = useCallback(async (fn: () => Promise<void>) => {
    setBusy(true);
    setErr('');
    try {
      await fn();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : 'Error');
      throw e;
    } finally {
      setBusy(false);
    }
  }, []);
  return { busy, err, run };
}

// ---------------------------------------------------------------------------
// Create app
// ---------------------------------------------------------------------------

function AppModal({
  onClose,
  onDone,
  flash,
}: {
  onClose: () => void;
  onDone: () => void;
  flash: Flash;
}) {
  const [apiId, setApiId] = useState('');
  const [apiHash, setApiHash] = useState('');
  const [label, setLabel] = useState('');
  const { busy, err, run } = useSubmit();

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await run(async () => {
        await api('POST', '/v1/apps', {
          api_id: Number(apiId),
          api_hash: apiHash.trim(),
          label: label.trim(),
        });
      });
      flash('App created');
      onDone();
      onClose();
    } catch {
      /* err shown inline */
    }
  }

  return (
    <Modal title="New app" onClose={onClose}>
      <form onSubmit={submit}>
        <label className="field">
          <span>api_id</span>
          <input value={apiId} onChange={(e) => setApiId(e.target.value)} inputMode="numeric" />
        </label>
        <label className="field">
          <span>api_hash</span>
          <input value={apiHash} onChange={(e) => setApiHash(e.target.value)} />
        </label>
        <label className="field">
          <span>Label</span>
          <input value={label} onChange={(e) => setLabel(e.target.value)} />
        </label>
        {err && <div className="err">{err}</div>}
        <div className="modal-actions">
          <button type="button" className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" disabled={busy || !apiId || !apiHash}>
            {busy ? 'Creating…' : 'Create'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Create proxy
// ---------------------------------------------------------------------------

function ProxyModal({
  onClose,
  onDone,
  flash,
}: {
  onClose: () => void;
  onDone: () => void;
  flash: Flash;
}) {
  const [type, setType] = useState('socks5');
  const [host, setHost] = useState('');
  const [port, setPort] = useState('');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [secret, setSecret] = useState('');
  const [label, setLabel] = useState('');
  const { busy, err, run } = useSubmit();

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await run(async () => {
        await api('POST', '/v1/proxies', {
          type,
          host: host.trim(),
          port: Number(port),
          username: username.trim(),
          password,
          secret,
          label: label.trim(),
        });
      });
      flash('Proxy created');
      onDone();
      onClose();
    } catch {
      /* inline */
    }
  }

  const mt = type === 'mtproto';

  return (
    <Modal title="New proxy" onClose={onClose}>
      <form onSubmit={submit}>
        <div className="form-row">
          <label className="field">
            <span>Type</span>
            <select value={type} onChange={(e) => setType(e.target.value)}>
              <option value="socks5">socks5</option>
              <option value="http">http</option>
              <option value="mtproto">mtproto</option>
            </select>
          </label>
          <label className="field">
            <span>Port</span>
            <input value={port} onChange={(e) => setPort(e.target.value)} inputMode="numeric" />
          </label>
        </div>
        <label className="field">
          <span>Host</span>
          <input value={host} onChange={(e) => setHost(e.target.value)} />
        </label>
        {mt ? (
          <label className="field">
            <span>secret (mtproto)</span>
            <input value={secret} onChange={(e) => setSecret(e.target.value)} />
          </label>
        ) : (
          <div className="form-row">
            <label className="field">
              <span>User</span>
              <input value={username} onChange={(e) => setUsername(e.target.value)} />
            </label>
            <label className="field">
              <span>Password</span>
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </label>
          </div>
        )}
        <label className="field">
          <span>Label</span>
          <input value={label} onChange={(e) => setLabel(e.target.value)} />
        </label>
        {err && <div className="err">{err}</div>}
        <div className="modal-actions">
          <button type="button" className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" disabled={busy || !host || !port}>
            {busy ? 'Creating…' : 'Create'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Create bot
// ---------------------------------------------------------------------------

function BotModal({
  apps,
  proxies,
  onClose,
  onDone,
  flash,
}: {
  apps: App[];
  proxies: Proxy[];
  onClose: () => void;
  onDone: () => void;
  flash: Flash;
}) {
  const [token, setTok] = useState('');
  const [appId, setAppId] = useState(apps[0]?.id || '');
  const [proxyId, setProxyId] = useState('');
  const [label, setLabel] = useState('');
  const { busy, err, run } = useSubmit();

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await run(async () => {
        await api('POST', '/v1/bot', {
          token: token.trim(),
          app_id: appId,
          proxy_id: proxyId,
          label: label.trim(),
        });
      });
      flash('Bot authorized');
      onDone();
      onClose();
    } catch {
      /* inline */
    }
  }

  return (
    <Modal title="New bot" onClose={onClose}>
      <form onSubmit={submit}>
        <label className="field">
          <span>Bot token</span>
          <input value={token} onChange={(e) => setTok(e.target.value)} />
        </label>
        <AppSelect apps={apps} value={appId} onChange={setAppId} />
        <ProxySelect proxies={proxies} value={proxyId} onChange={setProxyId} />
        <label className="field">
          <span>Label</span>
          <input value={label} onChange={(e) => setLabel(e.target.value)} />
        </label>
        <div className="hint">Bot login is synchronous — this takes a few seconds.</div>
        {err && <div className="err">{err}</div>}
        <div className="modal-actions">
          <button type="button" className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" disabled={busy || !token || !appId}>
            {busy ? 'Logging in…' : 'Create'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Create user
// ---------------------------------------------------------------------------

function UserModal({
  apps,
  proxies,
  onClose,
  onDone,
  onLogin,
  flash,
}: {
  apps: App[];
  proxies: Proxy[];
  onClose: () => void;
  onDone: () => void;
  onLogin: (id: string, status: string) => void;
  flash: Flash;
}) {
  const [phone, setPhone] = useState('');
  const [appId, setAppId] = useState(apps[0]?.id || '');
  const [proxyId, setProxyId] = useState('');
  const [label, setLabel] = useState('');
  // Set when the gateway refuses because this number already has a login
  // waiting for input; resuming it is free, creating another costs a code.
  const [pending, setPending] = useState('');
  const { busy, err, run } = useSubmit();

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setPending('');
    try {
      let res: SessionState | null = null;
      await run(async () => {
        res = await api<SessionState>('POST', '/v1/user', {
          app_id: appId,
          phone: phone.trim(),
          proxy_id: proxyId,
          label: label.trim(),
        });
      });
      onDone();
      if (res) {
        const r = res as SessionState;
        if (r.status === 'authorized') {
          flash('User authorized');
          onClose();
        } else {
          onLogin(r.id, r.status); // step into code / password
        }
      }
    } catch (e) {
      if (e instanceof ApiError && e.sessionId) setPending(e.sessionId);
    }
  }

  return (
    <Modal title="New user" onClose={onClose}>
      <form onSubmit={submit}>
        <label className="field">
          <span>Phone</span>
          <input value={phone} onChange={(e) => setPhone(e.target.value)} placeholder="+1…" />
        </label>
        <AppSelect apps={apps} value={appId} onChange={setAppId} />
        <ProxySelect proxies={proxies} value={proxyId} onChange={setProxyId} />
        <label className="field">
          <span>Label</span>
          <input value={label} onChange={(e) => setLabel(e.target.value)} />
        </label>
        {err && <div className="err">{err}</div>}
        <div className="modal-actions">
          {pending && (
            <button
              type="button"
              className="btn"
              style={{ marginRight: 'auto' }}
              onClick={() => onLogin(pending, 'awaiting_code')}
            >
              Resume that login
            </button>
          )}
          <button type="button" className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" disabled={busy || !phone || !appId}>
            {busy ? 'Sending…' : 'Continue'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Login flow (code -> password)
//
// Telegram rations code sends, so an open login is worth more than the dialog
// around it. Nothing here throws one away by accident: the state is read live
// from the owning worker rather than guessed, a rejected code or password
// leaves the login standing so it can be retyped, a lost code is resent on the
// same attempt, closing the dialog only hides it (the sessions list leads back),
// and giving up is a separate, explicit, confirmed action.
// ---------------------------------------------------------------------------

// codeDelivery describes where Telegram put the code, from td_api's
// authenticationCodeInfo: "via Sms to +1…, 5 digits".
function codeDelivery(info?: CodeInfo): string {
  if (!info) return '';
  const kind = (info.type?.['@type'] || '').replace(/^authenticationCodeType/, '');
  const parts = [kind ? `via ${kind}` : 'sent'];
  if (info.phone_number) parts.push(`to ${info.phone_number}`);
  if (info.type?.length) parts.push(`${info.type.length} digits`);
  return 'Code ' + parts.join(', ');
}

function LoginModal({
  id,
  initialStatus,
  onClose,
  onDone,
  flash,
}: {
  id: string;
  initialStatus: string;
  onClose: () => void;
  onDone: () => void;
  flash: Flash;
}) {
  const [state, setState] = useState<SessionState>({ id, status: initialStatus });
  const [value, setValue] = useState('');
  const [confirmCancel, setConfirmCancel] = useState(false);
  const [resendIn, setResendIn] = useState(0);
  const { busy, err, run } = useSubmit();
  const codeKey = useRef('');
  const resendAt = useRef(0);

  // The worker holds the real state; the modal must never disagree with it. It
  // also survives a reload this way — reopening the dialog picks the flow back
  // up wherever it actually is.
  const settle = (s: SessionState) => {
    setState(s);
    if (s.status === 'authorized') {
      flash('User authorized');
      onDone();
      onClose();
    }
  };
  // The poll reaches the current callbacks through a ref, so a parent re-render
  // does not tear the interval down and restart it.
  const settleRef = useRef(settle);
  settleRef.current = settle;

  useEffect(() => {
    let stop = false;
    const poll = async () => {
      try {
        const s = await api<SessionState>('GET', `/v1/user/${id}`);
        if (!stop) settleRef.current(s);
      } catch {
        // Transient, or the session is gone — keep showing the last state.
      }
    };
    poll();
    const t = window.setInterval(poll, 2000);
    return () => {
      stop = true;
      window.clearInterval(t);
    };
  }, [id]);

  // Anchor the resend countdown the first time each code_info is seen: polling
  // repeats the same object, and re-reading its timeout would freeze the clock.
  useEffect(() => {
    const key = state.code_info ? JSON.stringify(state.code_info) : '';
    if (key === codeKey.current) return;
    codeKey.current = key;
    const t = state.code_info?.timeout;
    resendAt.current = t ? Date.now() + t * 1000 : 0;
  }, [state.code_info]);

  useEffect(() => {
    const tick = () =>
      setResendIn(Math.max(0, Math.ceil((resendAt.current - Date.now()) / 1000)));
    tick();
    const t = window.setInterval(tick, 1000);
    return () => window.clearInterval(t);
  }, []);

  const isPassword = state.status === 'awaiting_password';
  const waiting = state.status === 'awaiting_code' || isPassword;
  // Typing means the operator is still working on this login, so an armed
  // "confirm discard" from an earlier click must not stay armed behind it.
  const onValue = (v: string) => {
    setValue(v);
    setConfirmCancel(false);
  };
  const nextType = (state.code_info?.next_type?.['@type'] || '').replace(
    /^authenticationCodeType/,
    ''
  );

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const path = isPassword ? `/v1/user/${id}/login/password` : `/v1/user/${id}/login/code`;
    const body = isPassword ? { password: value } : { code: value.trim() };
    try {
      let next: SessionState | null = null;
      await run(async () => {
        next = await api<SessionState>('POST', path, body);
      });
      setValue('');
      onDone();
      if (next) settle(next);
    } catch {
      // Rejected: the login is still standing, so the value can be retyped.
    }
  }

  async function resend() {
    try {
      await run(async () => {
        setState(await api<SessionState>('POST', `/v1/user/${id}/login/resend`));
      });
      flash('Code resent');
    } catch {
      /* inline */
    }
  }

  async function cancel() {
    try {
      await run(async () => {
        await api('DELETE', `/v1/user/${id}`);
      });
      flash('Login canceled');
      onDone();
      onClose();
    } catch {
      /* inline */
    }
  }

  return (
    <Modal title="Account login" onClose={onClose}>
      <form onSubmit={submit}>
        <div className="hint">
          Session <span className="mono">{id.slice(0, 8)}</span> · status:{' '}
          <span className={'status-' + state.status}>{state.status}</span>
        </div>

        {waiting && !isPassword && state.code_info && (
          <div className="hint">{codeDelivery(state.code_info)}</div>
        )}

        {!waiting && (
          <p>
            This login is <span className={'status-' + state.status}>{state.status}</span> and
            no longer accepts input. Start a new login for this number — that asks Telegram for
            a fresh code.
          </p>
        )}

        {waiting &&
          (isPassword ? (
            <label className="field">
              <span>2FA password</span>
              <input
                type="password"
                value={value}
                autoFocus
                onChange={(e) => onValue(e.target.value)}
              />
            </label>
          ) : (
            <label className="field">
              <span>Telegram code</span>
              <input
                value={value}
                autoFocus
                inputMode="numeric"
                onChange={(e) => onValue(e.target.value)}
              />
            </label>
          ))}

        {err && <div className="err">{err}</div>}
        {!err && state.last_error && <div className="err">{state.last_error}</div>}

        <div className="modal-actions">
          {waiting && (
            <button
              type="button"
              className="btn danger"
              style={{ marginRight: 'auto' }}
              disabled={busy}
              onClick={() => (confirmCancel ? cancel() : setConfirmCancel(true))}
            >
              {confirmCancel ? 'Confirm — discard login' : 'Cancel login'}
            </button>
          )}
          <button type="button" className="btn ghost" onClick={onClose}>
            {waiting ? 'Minimize' : 'Close'}
          </button>
          {waiting && !isPassword && (
            <button type="button" className="btn ghost" disabled={busy || resendIn > 0} onClick={resend}>
              {resendIn > 0
                ? `Resend in ${resendIn}s`
                : nextType
                  ? `Resend via ${nextType}`
                  : 'Resend code'}
            </button>
          )}
          {waiting && (
            <button className="btn" disabled={busy || !value}>
              {busy ? 'Checking…' : 'Submit'}
            </button>
          )}
        </div>

        {waiting && (
          <div className="hint" style={{ marginTop: 12, marginBottom: 0 }}>
            Minimize keeps this login open — reopen it with “Log in” in the sessions list. A
            wrong code or password can be retyped here; only Cancel throws the attempt away, and
            a new one costs another Telegram code.
          </div>
        )}
      </form>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Edit session (label + proxy, applied live)
// ---------------------------------------------------------------------------

function EditSessionModal({
  session,
  proxies,
  onClose,
  onDone,
  flash,
}: {
  session: Session;
  proxies: Proxy[];
  onClose: () => void;
  onDone: () => void;
  flash: Flash;
}) {
  const [label, setLabel] = useState(session.label || '');
  const [proxyId, setProxyId] = useState(session.proxy_id || '');
  const { busy, err, run } = useSubmit();

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await run(async () => {
        await api('PATCH', `/v1/${session.kind}/${session.id}`, {
          label: label.trim(),
          proxy_id: proxyId,
        });
      });
      flash('Session updated');
      onDone();
      onClose();
    } catch {
      /* inline */
    }
  }

  return (
    <Modal title="Edit session" onClose={onClose}>
      <form onSubmit={submit}>
        <div className="hint">
          {session.kind} · <span className="mono">{session.id.slice(0, 8)}</span>
        </div>
        <label className="field">
          <span>Label</span>
          <input value={label} onChange={(e) => setLabel(e.target.value)} />
        </label>
        <ProxySelect proxies={proxies} value={proxyId} onChange={setProxyId} />
        <div className="hint">A proxy change is applied to the live connection immediately.</div>
        {err && <div className="err">{err}</div>}
        <div className="modal-actions">
          <button type="button" className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" disabled={busy}>
            {busy ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Rename app / proxy
// ---------------------------------------------------------------------------

function RenameModal({
  entity,
  id,
  current,
  onClose,
  onDone,
  flash,
}: {
  entity: 'apps' | 'proxies';
  id: string;
  current: string;
  onClose: () => void;
  onDone: () => void;
  flash: Flash;
}) {
  const [label, setLabel] = useState(current);
  const { busy, err, run } = useSubmit();

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await run(async () => {
        await api('PATCH', `/v1/${entity}/${id}`, { label: label.trim() });
      });
      flash('Renamed');
      onDone();
      onClose();
    } catch {
      /* inline */
    }
  }

  return (
    <Modal title={entity === 'apps' ? 'Rename app' : 'Rename proxy'} onClose={onClose}>
      <form onSubmit={submit}>
        <label className="field">
          <span>Label</span>
          <input value={label} autoFocus onChange={(e) => setLabel(e.target.value)} />
        </label>
        {err && <div className="err">{err}</div>}
        <div className="modal-actions">
          <button type="button" className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" disabled={busy}>
            {busy ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Webhook set / clear
// ---------------------------------------------------------------------------

function WebhookModal({
  session,
  onClose,
  onDone,
  flash,
}: {
  session: Session;
  onClose: () => void;
  onDone: () => void;
  flash: Flash;
}) {
  const [url, setUrl] = useState('');
  const [secret, setSecret] = useState('');
  const [types, setTypes] = useState('');
  const { busy, err, run } = useSubmit();
  const base = `/v1/${session.kind}/${session.id}`;

  async function save(e: React.FormEvent) {
    e.preventDefault();
    const filterTypes = types
      .split(',')
      .map((t) => t.trim())
      .filter(Boolean);
    try {
      await run(async () => {
        await api('PUT', `${base}/updates/webhook`, {
          url: url.trim(),
          secret,
          filters: { types: filterTypes },
        });
      });
      flash('Webhook saved');
      onDone();
      onClose();
    } catch {
      /* inline */
    }
  }

  async function remove() {
    try {
      await run(async () => {
        await api('DELETE', `${base}/updates/webhook`);
      });
      flash('Webhook removed');
      onDone();
      onClose();
    } catch {
      /* inline */
    }
  }

  return (
    <Modal title="Session webhook" onClose={onClose}>
      <form onSubmit={save}>
        <div className="hint">
          {session.kind} · <span className="mono">{session.id.slice(0, 8)}</span>
        </div>
        <label className="field">
          <span>Receiver URL</span>
          <input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://…" />
        </label>
        <label className="field">
          <span>Secret (for HMAC signature)</span>
          <input value={secret} onChange={(e) => setSecret(e.target.value)} />
        </label>
        <label className="field">
          <span>Filter @type, comma-separated (empty = all)</span>
          <input
            value={types}
            onChange={(e) => setTypes(e.target.value)}
            placeholder="updateNewMessage, updateMessageSendSucceeded"
          />
        </label>
        {err && <div className="err">{err}</div>}
        <div className="modal-actions">
          <button type="button" className="btn danger" onClick={remove} disabled={busy}>
            Remove
          </button>
          <div style={{ flex: 1 }} />
          <button type="button" className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" disabled={busy || !url}>
            {busy ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Delete confirmation
// ---------------------------------------------------------------------------

function DeleteModal({
  session,
  onClose,
  onDone,
  flash,
}: {
  session: Session;
  onClose: () => void;
  onDone: () => void;
  flash: Flash;
}) {
  const { busy, err, run } = useSubmit();

  async function confirm() {
    try {
      await run(async () => {
        await api('DELETE', `/v1/${session.kind}/${session.id}`);
      });
      flash('Session deleted');
      onDone();
      onClose();
    } catch {
      /* inline */
    }
  }

  return (
    <Modal title="Delete session?" onClose={onClose}>
      <p>
        {session.kind} <b>{session.label || session.id.slice(0, 8)}</b> will be closed and its
        on-disk data and registry row removed. This cannot be undone.
      </p>
      {err && <div className="err">{err}</div>}
      <div className="modal-actions">
        <button className="btn ghost" onClick={onClose}>
          Cancel
        </button>
        <button className="btn danger" onClick={confirm} disabled={busy}>
          {busy ? 'Deleting…' : 'Delete'}
        </button>
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Token create / edit
// ---------------------------------------------------------------------------

function TokenModal({
  token,
  apps,
  sessions,
  onClose,
  onDone,
  onSecret,
  flash,
}: {
  token?: Token;
  apps: App[];
  sessions: Session[];
  onClose: () => void;
  onDone: () => void;
  onSecret: (secret: string) => void;
  flash: Flash;
}) {
  const editing = !!token;
  const [name, setName] = useState(token?.name || '');
  const [enabled, setEnabled] = useState(token ? token.enabled : true);
  const [allSessions, setAllSessions] = useState(token?.all_sessions || false);
  const [appIds, setAppIds] = useState<string[]>(token?.app_ids || []);
  const [sessionIds, setSessionIds] = useState<string[]>(token?.session_ids || []);
  const { busy, err, run } = useSubmit();

  const toggle = (arr: string[], id: string) =>
    arr.includes(id) ? arr.filter((x) => x !== id) : [...arr, id];

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const body = {
      name: name.trim(),
      enabled,
      all_sessions: allSessions,
      app_ids: appIds,
      session_ids: allSessions ? [] : sessionIds,
    };
    try {
      if (editing) {
        await run(async () => {
          await api('PATCH', `/v1/tokens/${token!.id}`, body);
        });
        flash('Token updated');
        onDone();
        onClose();
      } else {
        let secret = '';
        await run(async () => {
          const res = await api<{ id: string; token: string }>('POST', '/v1/tokens', body);
          secret = res.token;
        });
        onDone();
        onSecret(secret); // reveal once
      }
    } catch {
      /* inline */
    }
  }

  return (
    <Modal title={editing ? 'Edit token' : 'New token'} onClose={onClose}>
      <form onSubmit={submit}>
        <label className="field">
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} />
        </label>

        <label className="check">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          <span>Enabled</span>
        </label>
        <label className="check">
          <input
            type="checkbox"
            checked={allSessions}
            onChange={(e) => setAllSessions(e.target.checked)}
          />
          <span>Grant all sessions</span>
        </label>

        <div className="scope">
          <div className="scope-title">Apps (grants all their sessions)</div>
          {apps.length === 0 && <div className="hint">No apps</div>}
          {apps.map((a) => (
            <label className="check" key={a.id}>
              <input
                type="checkbox"
                checked={appIds.includes(a.id)}
                onChange={() => setAppIds((p) => toggle(p, a.id))}
              />
              <span>
                {a.label || a.id.slice(0, 8)} <span className="mono">({a.api_id})</span>
              </span>
            </label>
          ))}
        </div>

        <div className="scope" style={{ opacity: allSessions ? 0.4 : 1 }}>
          <div className="scope-title">Specific sessions</div>
          {sessions.length === 0 && <div className="hint">No sessions</div>}
          {sessions.map((s) => (
            <label className="check" key={s.id}>
              <input
                type="checkbox"
                disabled={allSessions}
                checked={sessionIds.includes(s.id)}
                onChange={() => setSessionIds((p) => toggle(p, s.id))}
              />
              <span>
                <span className={'badge kind-' + s.kind}>{s.kind}</span>{' '}
                {s.label || s.id.slice(0, 8)}{' '}
                <span className="mono">{s.app_label || ''}</span>
              </span>
            </label>
          ))}
        </div>

        {err && <div className="err">{err}</div>}
        <div className="modal-actions">
          <button type="button" className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" disabled={busy}>
            {busy ? 'Saving…' : editing ? 'Save' : 'Create'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Click-to-copy id
// ---------------------------------------------------------------------------

// Tables show ids truncated to stay readable, but every API call needs the whole
// UUID — so the prefix is a button that copies the full value (also available as
// the tooltip, for when the clipboard is blocked).
function CopyID({ id }: { id: string }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(id);
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    } catch {
      /* clipboard blocked; the full id is still in the tooltip */
    }
  }
  return (
    <button type="button" className="mono copy-id" title={id} onClick={copy}>
      {copied ? 'copied' : id.slice(0, 8)}
    </button>
  );
}

// ---------------------------------------------------------------------------
// Secret reveal (shown once after creation)
// ---------------------------------------------------------------------------

function SecretModal({ secret, onClose }: { secret: string; onClose: () => void }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied(true);
    } catch {
      /* clipboard blocked; user can select manually */
    }
  }
  return (
    <Modal title="Token created" onClose={onClose}>
      <p className="hint" style={{ marginTop: 0 }}>
        Copy this token now — it is shown only once and cannot be recovered.
      </p>
      <div className="secret">{secret}</div>
      <div className="modal-actions">
        <button className="btn ghost" onClick={copy}>
          {copied ? 'Copied' : 'Copy'}
        </button>
        <button className="btn" onClick={onClose}>
          Done
        </button>
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Shared selects + helpers
// ---------------------------------------------------------------------------

function AppSelect({
  apps,
  value,
  onChange,
}: {
  apps: App[];
  value: string;
  onChange: (v: string) => void;
}) {
  return (
    <label className="field">
      <span>App</span>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        {apps.length === 0 && <option value="">— no apps —</option>}
        {apps.map((a) => (
          <option key={a.id} value={a.id}>
            {a.label || a.id.slice(0, 8)} (api_id {a.api_id})
          </option>
        ))}
      </select>
    </label>
  );
}

function ProxySelect({
  proxies,
  value,
  onChange,
}: {
  proxies: Proxy[];
  value: string;
  onChange: (v: string) => void;
}) {
  return (
    <label className="field">
      <span>Proxy (optional)</span>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">— no proxy —</option>
        {proxies.map((p) => (
          <option key={p.id} value={p.id}>
            {p.label || `${p.type} ${p.host}:${p.port}`}
          </option>
        ))}
      </select>
    </label>
  );
}

function relTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return '';
  const diff = Math.floor((Date.now() - t) / 1000);
  if (diff < 60) return `${diff}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}
