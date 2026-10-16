import { useCallback, useEffect, useState } from 'react';
import GradientText from './components/reactbits/GradientText/GradientText.jsx';
import { Toast } from './components/ui.jsx';
import { BookIcon, ChatIcon, KeyIcon, Logo, PulseIcon, RadarIcon, ShieldIcon, SlidersIcon, SunriseIcon } from './components/Icons.jsx';
import Connect from './views/Connect.jsx';
import AdminLogin from './views/AdminLogin.jsx';
import Chat from './views/Chat.jsx';
import Findings from './views/Findings.jsx';
import Digest from './views/Digest.jsx';
import Postmortems from './views/Postmortems.jsx';
import Status from './views/Status.jsx';
import Manage from './views/Manage.jsx';
import { api, getJSON, hasKey, loadKey, publicConfig, setKey } from './lib/api.js';

const VIEWS = [
  { id: 'chat', label: 'Chat', icon: ChatIcon },
  { id: 'findings', label: 'Findings', icon: RadarIcon },
  { id: 'digest', label: 'Digest', icon: SunriseIcon },
  { id: 'reports', label: 'Postmortems', icon: BookIcon },
  { id: 'status', label: 'Status', icon: PulseIcon },
  { id: 'manage', label: 'Manage', icon: SlidersIcon, private: true }
];

const ROLE_LABEL = {
  superadmin: 'Super admin',
  admin: 'Admin',
  operator: 'Operator',
  readonly: 'Read only'
};

function viewFromHash() {
  const v = (location.hash || '').replace('#', '');
  return VIEWS.some(x => x.id === v) ? v : 'chat';
}

export default function App() {
  if (location.pathname.replace(/\/+$/, '') === '/admin') return <AdminLogin />;
  return <Console />;
}

function Console() {
  const [view, setView] = useState(viewFromHash);
  // checking, ok, needkey or down
  const [conn, setConn] = useState('checking');
  const [connError, setConnError] = useState('');
  const [role, setRole] = useState('');
  const [via, setVia] = useState('none');
  const [cfg, setCfg] = useState({});
  const [askKey, setAskKey] = useState(false);
  const [notice, setNotice] = useState(null);
  const [draft, setDraft] = useState(null);
  const [reportId, setReportId] = useState('');

  const notify = useCallback((text, bad = false) => setNotice({ text, bad, at: Date.now() }), []);
  const clearNotice = useCallback(() => setNotice(null), []);
  const isPublic = role === 'public';

  // explicit is true when the person just typed a key: a bad one is then their
  // mistake to fix, not something to quietly fall back from.
  const connect = useCallback(
    async (explicit = false, demo = cfg) => {
      try {
        const j = await getJSON('/v1/auth/whoami');
        setRole(j.role || '');
        setVia(j.session || 'none');
        setConn('ok');
        setConnError('');
        return true;
      } catch (e) {
        setRole('');
        if (e.status === 401 || e.status === 403) {
          if (!explicit && hasKey() && demo.public_demo) {
            setKey('', false);
            const ok = await connect(false, demo);
            if (ok) notify("Your saved key didn't work, so you're on the public demo for now.", true);
            return ok;
          }
          setConn('needkey');
          if (hasKey()) setConnError("That key didn't work. It may have been rotated.");
        } else {
          setConn('down');
          setConnError(e.message);
        }
        return false;
      }
    },
    [cfg, notify]
  );

  useEffect(() => {
    loadKey();
    publicConfig().then(c => {
      setCfg(c);
      connect(false, c);
    });
    // Run once on load; connect is rebuilt when cfg arrives.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const onHash = () => setView(viewFromHash());
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  const go = useCallback(id => {
    setView(id);
    try {
      history.replaceState(null, '', '#' + id);
    } catch {
      // A sandboxed frame may refuse; the view still changes.
    }
  }, []);

  const askAbout = useCallback(
    text => {
      setDraft({ text, at: Date.now() });
      go('chat');
    },
    [go]
  );

  const openReport = useCallback(
    id => {
      setReportId(id);
      go('reports');
    },
    [go]
  );

  async function signOut() {
    setKey('', false);
    if (via === 'cookie') await api('/v1/admin/logout', { method: 'POST' }).catch(() => {});
    location.href = '/ui/';
  }

  if (conn !== 'ok' || askKey) {
    return (
      <Connect
        state={conn}
        error={connError}
        canCancel={askKey && conn === 'ok'}
        adminLogin={!!cfg.admin_login}
        onCancel={() => setAskKey(false)}
        connect={async () => {
          const ok = await connect(true);
          if (ok) setAskKey(false);
          return ok;
        }}
      />
    );
  }

  const shown = VIEWS.filter(v => !(isPublic && v.private));
  const current = shown.some(v => v.id === view) ? view : 'chat';

  return (
    <div className="shell">
      <aside className="side">
        <div className="brand">
          <Logo />
          <GradientText colors={['#8f9bff', '#4fe0cb', '#b18cff', '#8f9bff']} animationSpeed={10} className="brand-name">
            kube-sre
          </GradientText>
        </div>
        <nav className="nav" aria-label="Sections">
          {shown.map(({ id, label, icon: Icon }) => (
            <button key={id} type="button" className={current === id ? 'on' : ''} onClick={() => go(id)} aria-current={current === id ? 'page' : undefined}>
              <Icon />
              <span>{label}</span>
            </button>
          ))}
        </nav>
        <div className="side-foot">
          <div className="who">
            <span className="dot up" aria-hidden="true" />
            {isPublic ? (
              <span>
                You're on the <b>public demo</b>
              </span>
            ) : (
              <span>
                Signed in as <b>{ROLE_LABEL[role] || role}</b>
              </span>
            )}
          </div>
          {isPublic ? (
            <>
              <button type="button" className="link" onClick={() => setAskKey(true)}>
                <KeyIcon /> Use an API key
              </button>
              {cfg.admin_login && (
                <a className="link" href="/admin">
                  <ShieldIcon /> Admin sign in
                </a>
              )}
            </>
          ) : (
            <>
              {via !== 'cookie' && (
                <button type="button" className="link" onClick={() => setAskKey(true)}>
                  <KeyIcon /> Change key
                </button>
              )}
              {(via === 'cookie' || hasKey()) && (
                <button type="button" className="link" onClick={signOut}>
                  Sign out
                </button>
              )}
            </>
          )}
        </div>
      </aside>

      <main className="main">
        {isPublic && cfg.public_demo && cfg.available === false && (
          <div className="banner bad" role="status">
            The demo has used up today's budget. It resets at midnight UTC, so please come back tomorrow.
          </div>
        )}
        <div className={current === 'chat' ? 'view on' : 'view'}>
          <Chat role={role} cfg={cfg} draft={draft} active={current === 'chat'} openReport={openReport} notify={notify} />
        </div>
        {current === 'findings' && <Findings askAbout={askAbout} notify={notify} />}
        {current === 'digest' && <Digest />}
        {current === 'reports' && <Postmortems initialId={reportId} />}
        {current === 'status' && <Status isPublic={isPublic} />}
        {current === 'manage' && <Manage role={role} notify={notify} />}
      </main>

      <Toast notice={notice} onClose={clearNotice} />
    </div>
  );
}
