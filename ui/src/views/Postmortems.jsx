import { useCallback, useEffect, useState } from 'react';
import DecryptedText from '../components/reactbits/DecryptedText/DecryptedText.jsx';
import { Card, Markdown, Page } from '../components/ui.jsx';
import { ShieldIcon } from '../components/Icons.jsx';
import { getJSON, getText } from '../lib/api.js';

function verdictOf(j) {
  if (j.chain_valid === false) {
    return { tone: 'bad', text: "The record doesn't check out. Something in it changed after it was written, so don't trust this timeline." };
  }
  if (j.chain_verified === false) {
    return { tone: 'warn', text: "I couldn't verify the record this time. That doesn't mean anyone tampered with it." };
  }
  return { tone: 'good', text: 'The record checks out. Nothing was changed after the fact.' };
}

function parseReplay(t) {
  const rows = [];
  for (const l of t.split('\n')) {
    if (!l.startsWith('data:')) continue;
    const d = l.slice(5).trim();
    if (!d || d === '[DONE]') continue;
    try {
      const j = JSON.parse(d);
      const s = JSON.stringify(j.payload != null ? j.payload : j);
      rows.push({ seq: j.seq != null ? j.seq : rows.length + 1, type: j.type || j.event_type || '', detail: s.length > 400 ? s.slice(0, 400) + '…' : s });
    } catch {
      // A malformed line is skipped; the rest still tells the story.
    }
  }
  return rows;
}

export default function Postmortems({ initialId }) {
  const [id, setId] = useState(initialId || '');
  const [report, setReport] = useState(null);
  const [replay, setReplay] = useState([]);
  const [state, setState] = useState('idle');
  const [error, setError] = useState('');

  const open = useCallback(async raw => {
    const v = (raw || '').trim();
    if (!v) return;
    const enc = encodeURIComponent(v);
    setState('loading');
    setReport(null);
    setReplay([]);
    try {
      setReport(await getJSON('/v1/episodes/' + enc + '/postmortem?format=markdown'));
      setState('done');
      getText('/v1/episodes/' + enc + '/replay')
        .then(t => setReplay(parseReplay(t)))
        .catch(() => setReplay([]));
    } catch (e) {
      setState('error');
      setError(e.status === 404 ? "I don't have a record for that id. It might be from a different server, or the chat never got going." : e.message);
    }
  }, []);

  useEffect(() => {
    if (initialId) {
      setId(initialId);
      open(initialId);
    }
  }, [initialId, open]);

  const v = report && verdictOf(report);
  const extra = [];
  if (report?.events_lost) extra.push(report.events_lost + ' events went missing');
  if (report?.gaps?.length) extra.push(report.gaps.length + (report.gaps.length === 1 ? ' gap' : ' gaps'));
  if (report?.enrichment_failed) extra.push("some extra detail couldn't be added");

  return (
    <Page
      title="Postmortems"
      lede="The story of one investigation, step by step, checked against a record that can't be quietly edited."
      actions={
        <form
          className="row"
          onSubmit={e => {
            e.preventDefault();
            open(id);
          }}
        >
          <input value={id} onChange={e => setId(e.target.value)} placeholder="Conversation or episode id" aria-label="Episode id" spellCheck="false" className="mono" />
          <button type="submit" disabled={!id.trim()}>
            Open
          </button>
        </form>
      }
    >
      {v && (
        <div className={'verdict ' + v.tone}>
          <ShieldIcon />
          <DecryptedText text={v.text + (extra.length ? ' (' + extra.join(', ') + ')' : '')} animateOn="view" sequential speed={18} revealDirection="start" />
        </div>
      )}

      <Card className="doc">
        {state === 'idle' && <p className="muted">Paste an id above, or hit Postmortem in a chat to land here with it filled in.</p>}
        {state === 'loading' && <p className="muted">Reading the record…</p>}
        {state === 'error' && <p className="err">{error}</p>}
        {state === 'done' && <Markdown text={report.markdown || report.postmortem || ''} />}
      </Card>

      {replay.length > 0 && (
        <details className="raw">
          <summary>Everything that was recorded ({replay.length} events)</summary>
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>#</th>
                  <th>What</th>
                  <th>Detail</th>
                </tr>
              </thead>
              <tbody>
                {replay.map(r => (
                  <tr key={r.seq}>
                    <td className="mono">{r.seq}</td>
                    <td className="mono">{r.type}</td>
                    <td className="mono small">{r.detail}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </details>
      )}
    </Page>
  );
}
