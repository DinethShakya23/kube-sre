import { useCallback, useEffect, useState } from 'react';
import GradientText from './components/reactbits/GradientText/GradientText.jsx';
import { Toast } from './components/ui.jsx';
import { BookIcon, ChatIcon, KeyIcon, Logo, PulseIcon, RadarIcon, SlidersIcon, SunriseIcon } from './components/Icons.jsx';
import Connect from './views/Connect.jsx';
import Chat from './views/Chat.jsx';
import Findings from './views/Findings.jsx';
import Digest from './views/Digest.jsx';
import Postmortems from './views/Postmortems.jsx';
import Status from './views/Status.jsx';
import Manage from './views/Manage.jsx';
import { getJSON, hasKey, loadKey } from './lib/api.js';

const VIEWS = [
  { id: 'chat', label: 'Chat', icon: ChatIcon },
  { id: 'findings', label: 'Findings', icon: RadarIcon },
  { id: 'digest', label: 'Digest', icon: SunriseIcon },
  { id: 'reports', label: 'Postmortems', icon: BookIcon },
  { id: 'status', label: 'Status', icon: PulseIcon },
  { id: 'manage', label: 'Manage', icon: SlidersIcon }
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
  const [view, setView] = useState(viewFromHash);
  // checking, ok, needkey or down
  const [conn, setConn] = useState('checking');
  const [connError, setConnError] = useState('');
  const [role, setRole] = useState('');
  const [askKey, setAskKey] = useState(false);
  const [notice, setNotice] = useState(null);
  const [draft, setDraft] = useState(null);
  const [reportId, setReportId] = useState('');

  const notify = useCallback((text, bad = false) => setNotice({ text, bad, at: Date.now() }), []);
  const clearNotice = useCallback(() => setNotice(null), []);

  const connect = useCallback(async () => {
    try {
      const j = await getJSON('/v1/auth/whoami');
      const r = j.role || '';
      // Some builds answer whoami without a key and call the caller "public".
      // That caller can read nothing, so it is the same as being asked for a key.
      if (!r || r === 'public') {
        setRole('');
        setConn('needkey');
        return false;
      }
      setRole(r);
      setConn('ok');
      setConnError('');
      return true;
    } catch (e) {
      setRole('');
      if (e.status === 401 || e.status === 403) {
        setConn('needkey');
        if (hasKey()) setConnError("That key didn't work. It may have been rotated.");
      } else {
        setConn('down');
        setConnError(e.message);
      }
      return false;
    }
  }, []);

  useEffect(() => {
    loadKey();
    connect();
  }, [connect]);

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

  if (conn !== 'ok' || askKey) {
    return (
      <Connect
        state={conn}
        error={connError}
        canCancel={askKey && conn === 'ok'}
        onCancel={() => setAskKey(false)}
        connect={async () => {
          const ok = await connect();
          if (ok) setAskKey(false);
          return ok;
        }}
      />
    );
  }

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
          {VIEWS.map(({ id, label, icon: Icon }) => (
            <button key={id} type="button" className={view === id ? 'on' : ''} onClick={() => go(id)} aria-current={view === id ? 'page' : undefined}>
              <Icon />
              <span>{label}</span>
            </button>
          ))}
        </nav>
        <div className="side-foot">
          <div className="who">
            <span className="dot up" aria-hidden="true" />
            <span>
              Signed in as <b>{ROLE_LABEL[role] || role}</b>
            </span>
          </div>
          <button type="button" className="link" onClick={() => setAskKey(true)}>
            <KeyIcon /> Change key
          </button>
        </div>
      </aside>

      <main className="main">
        <div className={view === 'chat' ? 'view on' : 'view'}>
          <Chat role={role} draft={draft} active={view === 'chat'} openReport={openReport} notify={notify} />
        </div>
        {view === 'findings' && <Findings askAbout={askAbout} notify={notify} />}
        {view === 'digest' && <Digest />}
        {view === 'reports' && <Postmortems initialId={reportId} />}
        {view === 'status' && <Status />}
        {view === 'manage' && <Manage role={role} notify={notify} />}
      </main>

      <Toast notice={notice} onClose={clearNotice} />
    </div>
  );
}
