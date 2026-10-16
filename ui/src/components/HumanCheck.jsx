import { useEffect, useRef, useState } from 'react';
import { sendJSON } from '../lib/api.js';

const SCRIPT = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit';

let loading = null;
function loadTurnstile() {
  if (window.turnstile) return Promise.resolve(window.turnstile);
  if (!loading) {
    loading = new Promise((resolve, reject) => {
      const sc = document.createElement('script');
      sc.src = SCRIPT;
      sc.async = true;
      sc.onload = () => resolve(window.turnstile);
      sc.onerror = () => {
        loading = null;
        reject(new Error('load failed'));
      };
      document.head.appendChild(sc);
    });
  }
  return loading;
}

// HumanCheck shows Cloudflare's Turnstile and trades a solved check for the
// server's ks_human cookie, then calls onPassed.
export default function HumanCheck({ siteKey, onPassed, onCancel }) {
  const box = useRef(null);
  const passed = useRef(onPassed);
  passed.current = onPassed;
  const [problem, setProblem] = useState('');

  useEffect(() => {
    let widget = null;
    let gone = false;
    loadTurnstile()
      .then(ts => {
        if (gone || !box.current) return;
        widget = ts.render(box.current, {
          sitekey: siteKey,
          theme: 'dark',
          callback: async token => {
            try {
              await sendJSON('/v1/public/verify', 'POST', { token });
              passed.current();
            } catch (e) {
              setProblem("That didn't go through: " + e.message + '. Give it another try.');
              ts.reset(widget);
            }
          },
          'error-callback': () => setProblem("The check couldn't load. Try again in a moment.")
        });
      })
      .catch(() => setProblem("The check couldn't load. A blocker might be stopping it."));
    return () => {
      gone = true;
      if (widget != null) window.turnstile?.remove(widget);
    };
  }, [siteKey]);

  return (
    <div className="modal-back" role="dialog" aria-modal="true" aria-labelledby="hc-title">
      <div className="modal">
        <h2 id="hc-title">Quick check</h2>
        <p className="muted">Just making sure you're a person. It keeps the demo free for everyone.</p>
        <div ref={box} className="captcha" />
        {problem && <p className="err">{problem}</p>}
        <div className="row end">
          <button type="button" className="ghost" onClick={onCancel}>
            Not now
          </button>
        </div>
      </div>
    </div>
  );
}
