import { useCallback, useEffect, useState } from 'react';
import { Card, Markdown, Page } from '../components/ui.jsx';
import { RefreshIcon } from '../components/Icons.jsx';
import { getJSON } from '../lib/api.js';

const WINDOWS = [
  [6, 'the last 6 hours'],
  [12, 'the last 12 hours'],
  [24, 'the last day'],
  [72, 'the last 3 days'],
  [168, 'the last week']
];

export default function Digest() {
  const [hours, setHours] = useState(24);
  const [text, setText] = useState(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  const load = useCallback(async h => {
    setLoading(true);
    setError('');
    try {
      const j = await getJSON('/v1/digest?format=markdown&hours=' + encodeURIComponent(h));
      setText(j.markdown || '');
    } catch (e) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load(hours);
  }, [hours, load]);

  return (
    <Page
      title="Digest"
      lede="A catch up on what happened while you were away: what broke, what I did about it, and where you can roll back."
      actions={
        <>
          <label className="inline-label">
            Covering
            <select value={hours} onChange={e => setHours(Number(e.target.value))} aria-label="Time window">
              {WINDOWS.map(([h, label]) => (
                <option key={h} value={h}>
                  {label}
                </option>
              ))}
            </select>
          </label>
          <button type="button" className="ghost" onClick={() => load(hours)} disabled={loading}>
            <RefreshIcon /> Refresh
          </button>
        </>
      }
    >
      <Card className="doc">
        {error ? (
          <p className="err">I couldn't put the digest together: {error}</p>
        ) : text == null || loading ? (
          <p className="muted">Pulling it together…</p>
        ) : text.trim() ? (
          <Markdown text={text} />
        ) : (
          <p className="muted">Nothing to report for this stretch. Enjoy the quiet.</p>
        )}
      </Card>
    </Page>
  );
}
