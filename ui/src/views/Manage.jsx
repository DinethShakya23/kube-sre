import { useCallback, useEffect, useState } from 'react';
import { Badge, Card, Empty, Page } from '../components/ui.jsx';
import { api, canWrite, getJSON, isAdmin, sendJSON } from '../lib/api.js';

function Detectors({ admin, notify }) {
  const [rows, setRows] = useState(null);
  const [problem, setProblem] = useState('');
  const [desc, setDesc] = useState('');
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const j = await getJSON('/v1/detectors');
      setRows(j.detectors || []);
      setProblem('');
    } catch (e) {
      setRows([]);
      setProblem(
        e.status === 404
          ? "Custom detectors aren't switched on for this server."
          : e.status === 403
            ? 'Your key can see a lot, but not this part.'
            : "I couldn't list detectors: " + e.message
      );
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  async function create(e) {
    e.preventDefault();
    if (!desc.trim()) return;
    setBusy(true);
    try {
      const j = await (await sendJSON('/v1/detectors', 'POST', { description: desc.trim(), name: name.trim() })).json();
      if (j.staged === false) {
        notify("I couldn't turn that into a detector: " + ((j.errors || []).join('; ') || 'it did not describe something I can check'), true);
      } else {
        notify("Done. It's watching quietly in shadow mode until someone promotes it.");
        setDesc('');
        setName('');
      }
      load();
    } catch (err) {
      notify(err.message, true);
    } finally {
      setBusy(false);
    }
  }

  async function flip(d) {
    const verb = d.status === 'active' ? 'demote' : 'promote';
    const question =
      verb === 'promote'
        ? 'Promote "' + d.name + '"? It will start raising real findings.'
        : 'Demote "' + d.name + '"? It goes back to watching quietly in shadow mode.';
    if (!window.confirm(question)) return;
    try {
      await api('/v1/detectors/' + encodeURIComponent(d.name) + '/' + verb, { method: 'POST' });
      notify(verb === 'promote' ? d.name + ' is live now.' : d.name + ' is back in shadow mode.');
      load();
    } catch (err) {
      notify(err.message, true);
    }
  }

  return (
    <Card className="panel">
      <h2>Detectors</h2>
      <p className="small muted">
        Describe a problem in everyday words and I'll build a check for it. New ones start in <b>shadow mode</b>: they watch, but nothing happens until a
        person promotes them.
      </p>
      {admin ? (
        <form className="stack" onSubmit={create}>
          <textarea rows={2} value={desc} onChange={e => setDesc(e.target.value)} placeholder="For example: pods getting killed for running out of memory" aria-label="What should I watch for?" />
          <div className="row">
            <input value={name} onChange={e => setName(e.target.value)} placeholder="Give it a name (optional)" aria-label="Name" />
            <button type="submit" disabled={busy || !desc.trim()}>
              {busy ? 'Building…' : 'Create'}
            </button>
          </div>
        </form>
      ) : (
        <p className="small muted">Creating and promoting detectors needs an admin key.</p>
      )}
      {problem && <p className="muted">{problem}</p>}
      {rows && rows.length === 0 && !problem && <Empty>No custom detectors yet. The built in ones are always on.</Empty>}
      <ul className="items">
        {(rows || []).map(d => (
          <li key={d.name} className="item">
            <div>
              <div className="item-name">{d.name}</div>
              <div className="item-meta">
                <Badge tone={d.status === 'active' ? 'good' : 'warn'}>{d.status === 'active' ? 'Live' : d.status || 'shadow'}</Badge>
                {d.source && <span className="muted small">{d.source}</span>}
              </div>
            </div>
            {admin && (
              <button type="button" className="ghost" onClick={() => flip(d)}>
                {d.status === 'active' ? 'Demote' : 'Promote'}
              </button>
            )}
          </li>
        ))}
      </ul>
    </Card>
  );
}

function Preferences({ writer, notify }) {
  const [rows, setRows] = useState(null);
  const [problem, setProblem] = useState('');
  const [key, setKey] = useState('');
  const [value, setValue] = useState('');

  const load = useCallback(async () => {
    try {
      const j = await getJSON('/v1/preferences');
      setRows(j.preferences || []);
      setProblem('');
    } catch (e) {
      setRows([]);
      setProblem(e.status === 404 || e.status === 503 ? "Memory is switched off, so I can't keep preferences." : "I couldn't list preferences: " + e.message);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  async function save(e) {
    e.preventDefault();
    if (!key.trim()) return;
    try {
      await sendJSON('/v1/preferences', 'PUT', { key: key.trim(), value });
      notify("Got it, I'll remember that.");
      setKey('');
      setValue('');
      load();
    } catch (err) {
      notify(err.message, true);
    }
  }

  async function forget(p) {
    try {
      await api('/v1/preferences/' + encodeURIComponent(p.key), { method: 'DELETE' });
      notify('Forgotten.');
      load();
    } catch (err) {
      notify(err.message, true);
    }
  }

  return (
    <Card className="panel">
      <h2>Preferences</h2>
      <p className="small muted">Little things I should keep in mind, like which namespace you usually mean. I also pick some up from our chats.</p>
      {writer ? (
        <form className="row" onSubmit={save}>
          <input value={key} onChange={e => setKey(e.target.value)} placeholder="What, e.g. default_namespace" aria-label="Key" />
          <input value={value} onChange={e => setValue(e.target.value)} placeholder="Value, e.g. shop" aria-label="Value" />
          <button type="submit" disabled={!key.trim()}>
            Remember
          </button>
        </form>
      ) : (
        <p className="small muted">Changing preferences needs an operator key or higher.</p>
      )}
      {problem && <p className="muted">{problem}</p>}
      {rows && rows.length === 0 && !problem && <Empty>Nothing remembered yet.</Empty>}
      <ul className="items">
        {(rows || []).map(p => (
          <li key={p.key} className="item">
            <div>
              <div className="item-name mono">
                {p.key} = {p.value}
              </div>
              <div className="item-meta muted small">
                {p.source === 'explicit' ? 'You told me' : p.source === 'inferred' ? 'I picked this up' : p.source}
                {p.occurrence_count ? ', seen ' + p.occurrence_count + (p.occurrence_count === 1 ? ' time' : ' times') : ''}
              </div>
            </div>
            {writer && (
              <button type="button" className="ghost" onClick={() => forget(p)}>
                Forget
              </button>
            )}
          </li>
        ))}
      </ul>
    </Card>
  );
}

export default function Manage({ role, notify }) {
  return (
    <Page title="Manage" lede="Teach me what to watch for, and what to remember about how you work.">
      <div className="split">
        <Detectors admin={isAdmin(role)} notify={notify} />
        <Preferences writer={canWrite(role)} notify={notify} />
      </div>
    </Page>
  );
}
