import { useEffect, useRef, useState } from 'react';
import Aurora from '../components/reactbits/Aurora/Aurora.jsx';
import BlurText from '../components/reactbits/BlurText/BlurText.jsx';
import ShinyText from '../components/reactbits/ShinyText/ShinyText.jsx';
import StarBorder from '../components/reactbits/StarBorder/StarBorder.jsx';
import { Markdown } from '../components/ui.jsx';
import HumanCheck from '../components/HumanCheck.jsx';
import { AlertIcon, BookIcon, Logo, PlusIcon, SendIcon, StopIcon } from '../components/Icons.jsx';
import { canWrite, errText, headers, newSessionId, readSSE } from '../lib/api.js';

const SUGGESTIONS = [
  'Is anything broken right now?',
  'Which pods are restarting the most and why?',
  'Is any node under memory or CPU pressure?',
  'What changed in the last hour?'
];

// Cluster wide questions the demo's planted faults can answer: restarts, OOM, probes, image pulls, replicas, quota.
const PUBLIC_SUGGESTIONS = [
  'Which pods are restarting the most and why?',
  'Which containers are hitting their memory limits?',
  'Is anything failing its readiness probes?',
  'Are there image pull errors anywhere in the cluster?',
  'Are all deployments running their desired replicas?',
  'How close is the shop namespace to its resource quota?'
];

const RISK_WORD = { high: 'High risk', medium: 'Medium risk', low: 'Low risk' };

function stepText(ev) {
  switch (ev.type) {
    case 'status':
      return ev.message || ev.phase || 'Thinking it through';
    case 'tool_call':
      return 'Asking ' + (ev.tool || 'a tool') + (ev.message ? ': ' + ev.message : '');
    case 'tool_result':
      return 'Heard back from ' + (ev.tool || 'the tool');
    case 'plan': {
      const n = (ev.steps || []).length;
      return 'Made a plan with ' + n + (n === 1 ? ' step' : ' steps');
    }
    default:
      return null;
  }
}

let nextId = 1;

export default function Chat({ role, cfg = {}, draft, active, openReport, notify }) {
  const [session, setSession] = useState(newSessionId);
  const [messages, setMessages] = useState([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const [approval, setApproval] = useState(null);
  const [verified, setVerified] = useState(!!cfg.verified);
  const [checking, setChecking] = useState(null); // the question waiting on the human check
  const abortRef = useRef(null);
  const threadRef = useRef(null);
  const inputRef = useRef(null);

  const isPublic = role === 'public';
  const maxChars = isPublic ? cfg.max_question_chars || 0 : 0;
  const needsCheck = isPublic && !!cfg.turnstile_site_key && !verified;

  useEffect(() => setVerified(!!cfg.verified), [cfg.verified]);

  useEffect(() => {
    if (!draft) return;
    setInput(draft.text);
    requestAnimationFrame(() => inputRef.current?.focus());
  }, [draft]);

  useEffect(() => {
    const t = threadRef.current;
    if (t) t.scrollTop = t.scrollHeight;
  }, [messages, approval]);

  useEffect(() => {
    if (active) inputRef.current?.focus();
  }, [active]);

  function patch(id, fn) {
    setMessages(ms => ms.map(m => (m.id === id ? { ...m, ...fn(m) } : m)));
  }

  function send(text) {
    text = text.trim();
    if (!text || busy) return;
    if (maxChars && text.length > maxChars) {
      notify('Please keep questions under ' + maxChars + ' characters.', true);
      return;
    }
    if (needsCheck) {
      setChecking(text);
      return;
    }
    ask(text);
  }

  async function ask(text) {
    const botId = nextId++;
    setMessages(ms => [
      ...ms,
      { id: nextId++, kind: 'user', text },
      { id: botId, kind: 'bot', text: '', steps: [], streaming: true }
    ]);
    setApproval(null);
    setBusy(true);
    const ctl = new AbortController();
    abortRef.current = ctl;
    let answer = '';

    const onData = data => {
      if (data === '[DONE]') return;
      let j;
      try {
        j = JSON.parse(data);
      } catch {
        return;
      }
      if (j.object === 'stream.start') return;
      const ev = j.ki_event;
      if (ev) {
        if (ev.type === 'usage' && ev.total_tokens) patch(botId, () => ({ usage: ev.total_tokens }));
        const s = stepText(ev);
        if (s) patch(botId, m => ({ steps: [...m.steps, s] }));
      }
      if (j.hitl_required) {
        setApproval({ summary: j.human_summary || 'I want to make a change to the cluster.', risk: j.risk_level || '' });
      }
      const d = j.choices?.[0]?.delta?.content;
      if (d) {
        answer += d;
        patch(botId, () => ({ text: answer }));
      }
    };

    try {
      const r = await fetch('/v1/chat/completions', {
        method: 'POST',
        signal: ctl.signal,
        headers: headers({ 'Content-Type': 'application/json', 'X-Session-ID': session }),
        body: JSON.stringify({ model: 'kube-sre', stream: true, user: 'web', messages: [{ role: 'user', content: text }] })
      });
      if (!r.ok) {
        const e = new Error(errText(await r.text()) || r.statusText);
        e.status = r.status;
        throw e;
      }
      await readSSE(r, onData);
      patch(botId, () => ({ text: answer || "_I didn't get an answer back. Mind trying again?_", streaming: false }));
    } catch (e) {
      if (e.name === 'AbortError') {
        patch(botId, () => ({ text: answer + '\n\n_Okay, I stopped there._', streaming: false }));
      } else if (e.status === 403 && /verification_required/.test(e.message)) {
        setVerified(false);
        patch(botId, () => ({ text: 'Please do the quick check first, then ask again.', failed: true, streaming: false }));
      } else if (isPublic && (e.status === 429 || e.status === 503 || e.status === 422)) {
        // The demo's own limits already explain themselves in plain words.
        patch(botId, () => ({ text: e.message, failed: true, streaming: false }));
      } else {
        const why =
          e.status === 401 || e.status === 403
            ? "The server didn't accept your key for this. "
            : e.status === 429
              ? "I'm being rate limited, so give it a few seconds. "
              : '';
        patch(botId, () => ({ text: why + 'Something went wrong: ' + e.message, failed: true, streaming: false }));
      }
    } finally {
      setBusy(false);
      abortRef.current = null;
    }
  }

  function submit(e) {
    e.preventDefault();
    const t = input;
    setInput('');
    send(t);
  }

  function decide(word) {
    setApproval(null);
    send(word);
  }

  function reset() {
    abortRef.current?.abort();
    setMessages([]);
    setApproval(null);
    setSession(newSessionId());
    notify('Fresh start. This is a new conversation.');
  }

  const empty = messages.length === 0;

  return (
    <section className="chat">
      <header className="chat-head">
        <div>
          <h1>Chat</h1>
          <p className="lede">
            {isPublic
              ? 'This is a public demo, so many features are turned off on purpose. Access is read only: I can inspect pods, events, logs and metrics, but no write reaches the cluster.' +
                (cfg.questions_per_hour ? ' You get ' + cfg.questions_per_hour + ' questions an hour.' : '')
              : 'I answer from live reads of the cluster. Any change waits for your okay.'}
          </p>
        </div>
        <div className="page-actions">
          <span className="chip mono" title={'Conversation id ' + session}>
            {session.slice(0, 8)}
          </span>
          <button type="button" className="ghost" onClick={() => openReport(session)} disabled={empty} title="See the timeline of this conversation">
            <BookIcon /> Postmortem
          </button>
          <button type="button" className="ghost" onClick={reset}>
            <PlusIcon /> New chat
          </button>
        </div>
      </header>

      <div className="thread" ref={threadRef} aria-live="polite">
        {empty ? (
          <div className="hello">
            <div className="hello-bg" aria-hidden="true">
              {active && <Aurora colorStops={['#3346ff', '#28c7b2', '#6a2cff']} amplitude={0.7} blend={0.6} speed={0.45} opacity={0.4} mouseInteraction={false} />}
            </div>
            <BlurText text="Hey there. What's going on in your cluster?" className="hello-title" delay={80} animateBy="words" direction="top" trigger="load" />
            <p className="hello-lede">
              Ask me anything about what's running. I'll look at pods, events, logs and metrics, then tell you what I think is wrong and how to fix it.
            </p>
            <div className="suggestions">
              {(isPublic ? PUBLIC_SUGGESTIONS : SUGGESTIONS).map(s => (
                <button key={s} type="button" className="suggestion" onClick={() => send(s)}>
                  {s}
                </button>
              ))}
            </div>
            {isPublic ? (
              <p className="small muted">The cluster runs workloads with planted faults: crash loops, image pull errors, failing readiness probes and OOM kills. Pick a question above or ask your own.</p>
            ) : (
              !canWrite(role) && <p className="small muted">Your key is read only, so I can look and explain, but I can't apply fixes for you.</p>
            )}
          </div>
        ) : (
          messages.map(m =>
            m.kind === 'user' ? (
              <div key={m.id} className="msg user">
                {m.text}
              </div>
            ) : (
              <div key={m.id} className={'msg bot' + (m.failed ? ' failed' : '')}>
                <div className="avatar" aria-hidden="true">
                  {m.failed ? <AlertIcon /> : <Logo size={26} />}
                </div>
                <div className="bubble">
                  {m.steps.length > 0 && (
                    <details className="steps">
                      <summary>
                        {m.streaming ? 'Working on it' : 'What I did'} ({m.steps.length} {m.steps.length === 1 ? 'step' : 'steps'})
                      </summary>
                      <ol>
                        {m.steps.map((s, i) => (
                          <li key={i}>{s}</li>
                        ))}
                      </ol>
                    </details>
                  )}
                  {m.streaming && !m.text && (
                    <ShinyText text={(m.steps[m.steps.length - 1] || 'Thinking it through') + '…'} color="#8a93a6" shineColor="#e9ecff" speed={1.8} className="thinking" />
                  )}
                  {m.text && <Markdown text={m.text} className={m.streaming ? 'typing' : ''} />}
                  {m.usage && <div className="usage">{m.usage.toLocaleString()} tokens</div>}
                </div>
              </div>
            )
          )
        )}

        {approval && (
          <StarBorder as="div" className="approval" color="#ffcf5a" trailColor="#ff8a3d" duration={5} radius={16} hover="none" clickPulse={false} backgroundColor="#14121a">
            <div className="approval-body">
              <div className="approval-head">
                <span className="approval-kicker">I need your okay</span>
                {approval.risk && <span className={'badge ' + (approval.risk === 'high' ? 'bad' : 'warn')}>{RISK_WORD[approval.risk] || approval.risk}</span>}
              </div>
              <p>{approval.summary}</p>
              <div className="row">
                <button type="button" className="approve" onClick={() => decide('yes')}>
                  Yes, go ahead
                </button>
                <button type="button" className="ghost danger" onClick={() => decide('no')}>
                  No, leave it
                </button>
              </div>
            </div>
          </StarBorder>
        )}
      </div>

      <form className="composer" onSubmit={submit}>
        <textarea
          ref={inputRef}
          rows={2}
          value={input}
          onChange={e => setInput(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault();
              e.currentTarget.form.requestSubmit();
            }
          }}
          placeholder={isPublic ? 'Ask about a pod, a namespace or a failing workload' : 'Ask about a pod, a namespace, or just say what feels off'}
          aria-label="Message"
          maxLength={maxChars || undefined}
        />
        {busy ? (
          <button type="button" className="ghost icon-btn" onClick={() => abortRef.current?.abort()} title="Stop">
            <StopIcon />
            <span>Stop</span>
          </button>
        ) : (
          <button type="submit" className="icon-btn" disabled={!input.trim()} title="Send (Enter)">
            <SendIcon />
            <span>Send</span>
          </button>
        )}
      </form>
      <p className="composer-hint">
        Enter sends, Shift+Enter adds a new line.
        {maxChars > 0 && input.length > maxChars * 0.8 && ' ' + (maxChars - input.length) + ' characters left.'}
      </p>
      {checking && (
        <HumanCheck
          siteKey={cfg.turnstile_site_key}
          onPassed={() => {
            const q = checking;
            setVerified(true);
            setChecking(null);
            ask(q);
          }}
          onCancel={() => setChecking(null)}
        />
      )}
    </section>
  );
}
