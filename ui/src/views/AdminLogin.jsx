import { useEffect, useState } from 'react';
import Aurora from '../components/reactbits/Aurora/Aurora.jsx';
import BlurText from '../components/reactbits/BlurText/BlurText.jsx';
import { Logo } from '../components/Icons.jsx';
import { errText, getJSON, headers, publicConfig } from '../lib/api.js';

export default function AdminLogin() {
  const [enabled, setEnabled] = useState(null);
  const [already, setAlready] = useState(false);
  const [user, setUser] = useState('');
  const [pass, setPass] = useState('');
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState('');

  useEffect(() => {
    publicConfig().then(c => setEnabled(!!c.admin_login));
    getJSON('/v1/auth/whoami')
      .then(j => setAlready((j.role === 'admin' || j.role === 'superadmin') && j.session === 'cookie'))
      .catch(() => {});
  }, []);

  async function submit(e) {
    e.preventDefault();
    setBusy(true);
    setProblem('');
    try {
      const r = await fetch('/v1/admin/login', {
        method: 'POST',
        headers: headers({ 'Content-Type': 'application/json' }),
        body: JSON.stringify({ username: user.trim(), password: pass })
      });
      if (r.ok) {
        setPass('');
        location.href = '/ui/';
        return;
      }
      setProblem(errText(await r.text()) || "That didn't work.");
      setPass('');
    } catch {
      setProblem("I can't reach the server right now.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="connect">
      <div className="connect-bg" aria-hidden="true">
        <Aurora colorStops={['#3b4bff', '#2fd3bd', '#7a3cff']} amplitude={0.9} blend={0.55} speed={0.6} opacity={0.75} mouseInteraction={false} />
      </div>
      <div className="connect-card">
        <div className="connect-brand">
          <Logo size={40} />
          <span>kube-sre</span>
        </div>
        <BlurText text="Welcome back." className="connect-title" delay={90} animateBy="words" direction="top" trigger="load" />
        <p className="connect-lede">Sign in to manage detectors and preferences, and to make changes with approval.</p>

        {already && (
          <p className="connect-note">
            You're already signed in. <a href="/ui/">Head to the dashboard</a>.
          </p>
        )}

        {enabled === false ? (
          <p className="connect-note">Admin sign in isn't set up on this server.</p>
        ) : (
          <form className="connect-form" onSubmit={submit}>
            <label htmlFor="user">Username</label>
            <input id="user" autoComplete="username" autoCapitalize="none" spellCheck="false" value={user} onChange={e => setUser(e.target.value)} required autoFocus />
            <label htmlFor="pass">Password</label>
            <input id="pass" type="password" autoComplete="current-password" value={pass} onChange={e => setPass(e.target.value)} required />
            {problem && <p className="err">{problem}</p>}
            <div className="row end">
              <button type="submit" disabled={busy || enabled === null}>
                {busy ? 'Signing in…' : 'Sign in'}
              </button>
            </div>
          </form>
        )}
        <p className="small muted">
          <a href="/ui/">Back to the public demo</a>
        </p>
      </div>
    </div>
  );
}
