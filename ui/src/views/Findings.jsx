import { useCallback, useEffect, useMemo, useState } from 'react';
import CountUp from '../components/reactbits/CountUp/CountUp.jsx';
import { Badge, Card, Empty, Page } from '../components/ui.jsx';
import { ChatIcon, RefreshIcon, SearchIcon } from '../components/Icons.jsx';
import { fullTime, getJSON, when } from '../lib/api.js';

const PAGE = 50;
const PREFS = 'kubesre.findings';

function tone(sev) {
  if (sev === 'predicted') return 'warn';
  if (sev === 'info') return 'neutral';
  return sev === 'warning' ? 'warn' : 'bad';
}

const objectOf = f => (f.namespace ? f.namespace + '/' : '') + (f.object || '');

// Filters are a per browser convenience, so a failed read or write just means defaults.
function loadPrefs() {
  try {
    return JSON.parse(localStorage.getItem(PREFS)) || {};
  } catch {
    return {};
  }
}

function savePrefs(p) {
  try {
    localStorage.setItem(PREFS, JSON.stringify(p));
  } catch {
    // Private windows may refuse storage; the filters still work for this visit.
  }
}

// group folds repeat firings of one detector on one object into a single row.
function group(rows) {
  const by = new Map();
  for (const f of rows) {
    const k = f.playbook + '|' + objectOf(f);
    const g = by.get(k);
    if (!g) by.set(k, { ...f, count: 1, first: f.fired_at });
    else {
      g.count++;
      if (f.fired_at > g.fired_at) Object.assign(g, { ...f, count: g.count, first: g.first });
      if (f.fired_at < g.first) g.first = f.fired_at;
    }
  }
  return [...by.values()];
}

function Select({ label, value, onChange, options }) {
  return (
    <label className="filter">
      <span>{label}</span>
      <select value={value} onChange={e => onChange(e.target.value)} aria-label={label}>
        <option value="">All</option>
        {options.map(o => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
    </label>
  );
}

export default function Findings({ askAbout, notify }) {
  const saved = useMemo(loadPrefs, []);
  const [data, setData] = useState(null);
  const [error, setError] = useState('');
  const [auto, setAuto] = useState(true);
  const [query, setQuery] = useState('');
  const [severity, setSeverity] = useState(saved.severity || '');
  const [namespace, setNamespace] = useState(saved.namespace || '');
  const [detector, setDetector] = useState(saved.detector || '');
  const [newestFirst, setNewestFirst] = useState(saved.newestFirst !== false);
  const [grouped, setGrouped] = useState(!!saved.grouped);
  const [shown, setShown] = useState(PAGE);

  useEffect(() => savePrefs({ severity, namespace, detector, newestFirst, grouped }), [severity, namespace, detector, newestFirst, grouped]);
  useEffect(() => setShown(PAGE), [query, severity, namespace, detector, grouped]);

  const load = useCallback(async () => {
    try {
      setData(await getJSON('/v1/findings?limit=500'));
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

  const all = useMemo(() => data?.findings || [], [data]);
  const options = useMemo(() => {
    const uniq = key => [...new Set(all.map(key).filter(Boolean))].sort();
    return { severity: uniq(f => f.severity), namespace: uniq(f => f.namespace), detector: uniq(f => f.playbook) };
  }, [all]);

  const rows = useMemo(() => {
    const words = query.toLowerCase().split(/\s+/).filter(Boolean);
    let out = all.filter(
      f =>
        (!severity || f.severity === severity) &&
        (!namespace || f.namespace === namespace) &&
        (!detector || f.playbook === detector) &&
        words.every(w => [f.playbook, objectOf(f), f.evidence, f.severity].join(' ').toLowerCase().includes(w))
    );
    if (grouped) out = group(out);
    return out.sort((a, b) => (newestFirst ? b.fired_at - a.fired_at : a.fired_at - b.fired_at));
  }, [all, query, severity, namespace, detector, grouped, newestFirst]);

  const filtering = query || severity || namespace || detector;
  const q = data?.queue || {};
  const off = data?.sensorium === 'disabled' || (data && !data.sensorium);
  const objects = new Set(all.map(objectOf)).size;

  function clearFilters() {
    setQuery('');
    setSeverity('');
    setNamespace('');
    setDetector('');
  }

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
          <div className="stat-value">{data ? <CountUp to={all.length} duration={1.2} trigger="load" /> : '…'}</div>
          <div className="small muted">
            across {objects} {objects === 1 ? 'thing' : 'things'}
          </div>
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

      {all.length > 0 && (
        <div className="toolbar">
          <label className="search">
            <SearchIcon />
            <input
              type="search"
              value={query}
              onChange={e => setQuery(e.target.value)}
              placeholder="Search detector, pod or evidence"
              aria-label="Search findings"
            />
          </label>
          <Select label="Severity" value={severity} onChange={setSeverity} options={options.severity} />
          <Select label="Namespace" value={namespace} onChange={setNamespace} options={options.namespace} />
          <Select label="Detector" value={detector} onChange={setDetector} options={options.detector} />
          <label className="filter">
            <span>Order</span>
            <select value={newestFirst ? 'new' : 'old'} onChange={e => setNewestFirst(e.target.value === 'new')} aria-label="Order">
              <option value="new">Newest first</option>
              <option value="old">Oldest first</option>
            </select>
          </label>
          <label className="check">
            <input type="checkbox" checked={grouped} onChange={e => setGrouped(e.target.checked)} /> Group repeats
          </label>
        </div>
      )}

      {all.length > 0 && (
        <div className="result-line small muted">
          <span>
            {grouped ? 'Showing ' + rows.length + (rows.length === 1 ? ' problem' : ' problems') : 'Showing ' + Math.min(shown, rows.length) + ' of ' + rows.length}
            {filtering ? ' that match' : ''}
            {rows.length !== all.length || grouped ? ', from ' + all.length + ' findings' : ''}.
          </span>
          {filtering && (
            <button type="button" className="link" onClick={clearFilters}>
              Clear filters
            </button>
          )}
        </div>
      )}

      {data && all.length === 0 ? (
        <Empty>{off ? "Detection is switched off, so I'm not collecting findings." : 'All quiet. Nothing has fired recently.'}</Empty>
      ) : rows.length === 0 && data ? (
        <Empty>Nothing matches those filters. Try a different word, or clear them.</Empty>
      ) : (
        <div className="finding-list">
          {rows.slice(0, shown).map((f, i) => {
            const obj = objectOf(f);
            return (
              <Card key={(f.id || '') + obj + f.fired_at + i} className="finding">
                <div className="finding-main">
                  <div className="finding-top">
                    <span className="finding-name">{f.playbook || 'Unknown detector'}</span>
                    <Badge tone={tone(f.severity)}>{f.severity || 'unknown'}</Badge>
                    {f.count > 1 && <Badge tone="neutral">fired {f.count} times</Badge>}
                    {f.eta_minutes != null && <span className="small muted">likely in about {Math.round(f.eta_minutes)} min</span>}
                  </div>
                  <button type="button" className="mono finding-obj" title="Show only this namespace" onClick={() => f.namespace && setNamespace(f.namespace)}>
                    {obj}
                  </button>
                  {f.evidence && <div className="finding-evidence">{f.evidence}</div>}
                </div>
                <div className="finding-side">
                  <span className="small muted" title={fullTime(f.fired_at)}>
                    {f.count > 1 ? 'last ' + when(f.fired_at) + ', first ' + when(f.first) : when(f.fired_at)}
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
          {shown < rows.length && (
            <button type="button" className="ghost more" onClick={() => setShown(n => n + PAGE)}>
              Show {Math.min(PAGE, rows.length - shown)} more
            </button>
          )}
        </div>
      )}
    </Page>
  );
}
