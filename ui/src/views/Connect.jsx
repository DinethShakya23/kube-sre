import { useState } from 'react';
import Aurora from '../components/reactbits/Aurora/Aurora.jsx';
import BlurText from '../components/reactbits/BlurText/BlurText.jsx';
import { Logo } from '../components/Icons.jsx';
import { setKey } from '../lib/api.js';

export default function Connect({ state, error, canCancel, adminLogin, onCancel, connect }) {
  const [value, setValue] = useState('');
  const [remember, setRemember] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState('');

  async function submit(e) {
    e.preventDefault();
    setBusy(true);
    setProblem('');
    setKey(value.trim(), remember);
    const ok = await connect();
    setBusy(false);
    if (!ok) setProblem(value.trim() ? "Hmm, the server didn't accept that key. Double check it and try again." : 'This server needs a key.');
  }

  const down = state === 'down';
  const checking = state === 'checking';

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
        <BlurText text="Your cluster, explained in plain words." className="connect-title" delay={90} animateBy="words" direction="top" trigger="load" />
        <p className="connect-lede">
          I watch the cluster, figure out why things break, and suggest fixes. I never change anything without your say so.
        </p>

        {checking ? (
          <p className="connect-note">Saying hello to the server…</p>
        ) : down ? (
          <div className="connect-down">
            <p>
              <b>I can't reach the server right now.</b> It might be restarting, or the tunnel might be closed.
            </p>
            {error && <p className="small mono">{error}</p>}
            <button type="button" onClick={() => connect()}>
              Try again
            </button>
          </div>
        ) : (
          <form className="connect-form" onSubmit={submit}>
            <label htmlFor="key">Your API key</label>
            <input
              id="key"
              type="password"
              autoComplete="off"
              spellCheck="false"
              placeholder="Paste it here"
              value={value}
              onChange={e => setValue(e.target.value)}
              autoFocus
            />
            <label className="check">
              <input type="checkbox" checked={remember} onChange={e => setRemember(e.target.checked)} /> Remember me on this device
            </label>
            {(problem || error) && <p className="err">{problem || error}</p>}
            <div className="row end">
              {canCancel && (
                <button type="button" className="ghost" onClick={onCancel}>
                  Never mind
                </button>
              )}
              <button type="submit" disabled={busy}>
                {busy ? 'Checking…' : "Let's go"}
              </button>
            </div>
            <p className="small muted">
              Don't have one? Whoever runs this server can make one with <code>kube-sre set KUBESRE_READONLY_KEYS=...</code>. A read only key is
              plenty for looking around.
            </p>
            {adminLogin && (
              <p className="small muted">
                Have an admin account? <a href="/admin">Sign in with a password</a> instead.
              </p>
            )}
          </form>
        )}
      </div>
    </div>
  );
}
