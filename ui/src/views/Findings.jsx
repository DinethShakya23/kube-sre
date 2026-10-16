import { useCallback, useEffect, useState } from 'react';
import CountUp from '../components/reactbits/CountUp/CountUp.jsx';
import { Badge, Card, Empty, Page } from '../components/ui.jsx';
import { ChatIcon, RefreshIcon } from '../components/Icons.jsx';
import { fullTime, getJSON, when } from '../lib/api.js';

function tone(sev) {
  if (sev === 'predicted') return 'warn';
  if (sev === 'info') return 'neutral';
  return sev === 'warning' ? 'warn' : 'bad';
}

export default function Findings({ askAbout, notify }) {
  const [data, setData] = useState(null);
  const [error, setError] = useState('');
  const [auto, setAuto] = useState(true);

  const load = useCallback(async () => {
    try {
      setData(await getJSON('/v1/findings?limit=100'));
      setError('');
    } catch (e) {
      setError(e.message);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => {
    if (!auto) return undefined;
    const t = setInterval(load, 10000);
    return () => clearInterval(t);
  }, [auto, load]);

  const rows = data?.findings || [];
  const q = data?.queue || {};
  const off = data?.sensorium === 'disabled' || (data && !data.sensorium);
  const objects = new Set(rows.map(f => (f.namespace || '') + '/' + (f.object || ''))).size;

  return (
    <Page
      title="Findings"
      lede="Problems my detectors spotted on their own. These run without the model, so they're free and fast."
      actions={
        <>
          <label className="check">
            <input type="checkbox" checked={auto} onChange={e => setAuto(e.target.checked)} /> Keep it fresh
          </label>
          <button
            type="button"
            className="ghost"
            onClick={() => {
              load();
              notify('Refreshed.');
            }}
          >
            <RefreshIcon /> Refresh
          </button>
        </>
      }
    >
      {error && <p className="err">I couldn't load findings: {error}</p>}

      <div className="stats">
        <Card>
          <div className="stat-label">Watching the cluster</div>
          <div className="stat-value">
            <Badge tone={off ? 'warn' : 'good'}>{off ? 'Not right now' : 'Yes'}</Badge>
          </div>
          {data?.sensorium_reason && <div className="small muted">{data.sensorium_reason}</div>}
        </Card>
        <Card>
          <div className="stat-label">Recent findings</div>
          <div className="stat-value">{data ? <CountUp to={rows.length} duration={1.2} trigger="load" /> : '…'}</div>
          <div className="small muted">across {objects} {objects === 1 ? 'thing' : 'things'}</div>
        </Card>
        <Card>
          <div className="stat-label">Dropped under load</div>
          <div className="stat-value">{data ? <CountUp to={q.shed_total || 0} duration={1.2} trigger="load" /> : '…'}</div>
          <div className="small muted">
            busiest the queue got: {q.high_water || 0} of {q.maxsize || 0}
          </div>
        </Card>
        {data?.predictive_error && (
          <Card>
            <div className="stat-label">Predictions</div>
            <div className="small">{data.predictive_error}</div>
          </Card>
        )}
      </div>

      {data && rows.length === 0 ? (
        <Empty>{off ? "Detection is switched off, so I'm not collecting findings." : 'All quiet. Nothing has fired recently.'}</Empty>
      ) : (
        <div className="finding-list">
          {rows.map((f, i) => {
            const obj = (f.namespace ? f.namespace + '/' : '') + (f.object || '');
            return (
              <Card key={(f.id || '') + obj + f.fired_at + i} className="finding">
                <div className="finding-main">
                  <div className="finding-top">
                    <span className="finding-name">{f.playbook || 'Unknown detector'}</span>
                    <Badge tone={tone(f.severity)}>{f.severity || 'unknown'}</Badge>
                    {f.eta_minutes != null && <span className="small muted">likely in about {Math.round(f.eta_minutes)} min</span>}
                  </div>
                  <div className="mono finding-obj">{obj}</div>
                  {f.evidence && <div className="finding-evidence">{f.evidence}</div>}
                </div>
                <div className="finding-side">
                  <span className="small muted" title={fullTime(f.fired_at)}>
                    {when(f.fired_at)}
                  </span>
                  <button
                    type="button"
                    className="ghost"
                    onClick={() =>
                      askAbout('Can you look into ' + (f.playbook || 'this') + ' on ' + obj + '?' + (f.evidence ? ' Here is what the detector saw: ' + f.evidence : ''))
                    }
                  >
                    <ChatIcon /> Ask about this
                  </button>
                </div>
              </Card>
            );
          })}
        </div>
      )}
    </Page>
  );
}
