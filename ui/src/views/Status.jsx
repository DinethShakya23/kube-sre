import { useCallback, useEffect, useState } from 'react';
import CountUp from '../components/reactbits/CountUp/CountUp.jsx';
import { Badge, Card, Page } from '../components/ui.jsx';
import { RefreshIcon } from '../components/Icons.jsx';
import { getJSON } from '../lib/api.js';

const GOOD = ['ok', 'ready', 'running', 'enabled', 'current'];

function Part({ title, state, okText, offText, children }) {
  const fine = GOOD.includes(state);
  const off = !state || state === 'off' || state === 'disabled';
  return (
    <Card className="part">
      <div className="part-head">
        <h3>{title}</h3>
        <Badge tone={fine ? 'good' : off ? 'neutral' : 'warn'}>{fine ? okText || 'Healthy' : off ? offText || 'Off' : state}</Badge>
      </div>
      {children}
    </Card>
  );
}

export default function Status({ isPublic = false }) {
  const [health, setHealth] = useState(null);
  const [v5, setV5] = useState(null);
  const [error, setError] = useState('');

  const load = useCallback(async () => {
    try {
      // The safety brakes are not shown to the public demo, so it does not ask.
      const [h, s] = await Promise.all([getJSON('/healthz'), isPublic ? null : getJSON('/v1/v5/status').catch(() => null)]);
      setHealth(h);
      setV5(s);
      setError('');
    } catch (e) {
      setError(e.message);
    }
  }, [isPublic]);

  useEffect(() => {
    load();
  }, [load]);

  const h = health || {};
  const mem = h.memory || {};
  const recall = mem.recall_attempts ? Math.round((100 * (mem.recall_hits || 0)) / mem.recall_attempts) : null;
  const flags = v5?.active_flags || h.experimental_flags || [];
  const unwired = v5?.set_but_unwired_flags || h.set_but_unwired_flags || [];
  const allGood = h.status === 'ok';

  return (
    <Page
      title="Status"
      lede="What's running, how it's holding up, and what I'm allowed to do on my own."
      actions={
        <button type="button" className="ghost" onClick={load}>
          <RefreshIcon /> Refresh
        </button>
      }
    >
      {error && <p className="err">I couldn't read the status: {error}</p>}

      {health && (
        <div className={'summary ' + (allGood ? 'good' : 'warn')}>
          <span className="dot" aria-hidden="true" />
          {allGood ? 'Everything looks healthy.' : "Something needs a look. The cards below say what."}
          <span className="muted small">Version {h.version || 'unknown'}</span>
        </div>
      )}

      <div className="parts">
        <Part title="Watching the cluster" state={h.sensorium?.state} okText="Watching">
          <p className="small muted">{h.sensorium?.watching ? 'Following pods and events as they change.' : h.sensorium?.reason || 'Not following changes right now.'}</p>
        </Part>
        <Part title="Memory" state={mem.state}>
          <div className="mini-stats">
            <div>
              <span className="mini-value">
                <CountUp to={mem.episodes_written || 0} duration={1} trigger="load" />
              </span>
              <span className="mini-label">incidents remembered</span>
            </div>
            <div>
              <span className="mini-value">{recall == null ? 'n/a' : <CountUp to={recall} duration={1} suffix="%" trigger="load" />}</span>
              <span className="mini-label">of lookups found something</span>
            </div>
          </div>
          {mem.reason && <p className="small muted">{mem.reason}</p>}
        </Part>
        <Part title="Flight recorder" state={h.recorder?.state} okText="Recording">
          <p className="small muted">
            {h.recorder?.lost_while_down ? h.recorder.lost_while_down + ' events were lost while it was down.' : 'Every step is written to a tamper evident log.'}
          </p>
        </Part>
        <Part title="Audit log" state={h.audit?.state}>
          <p className="small muted">{h.audit?.dropped ? h.audit.dropped + ' entries were dropped.' : 'Nothing dropped.'}</p>
        </Part>
        <Part title="Database" state={h.db_schema?.state} okText="Up to date">
          <p className="small muted">
            Schema v{h.db_schema?.applied_version ?? '?'}
            {h.db_schema && !h.db_schema.matches ? ', but this build expects v' + h.db_schema.expected_version : ''}
          </p>
        </Part>
        <Part title="Background work" state={h.leader ? 'ok' : ''} okText={h.leader?.is_leader ? 'This server' : 'Another server'}>
          <p className="small muted">{h.leader?.reason || ''}</p>
        </Part>
      </div>

      {v5 && (
        <>
          <h2 className="section">Safety brakes</h2>
          <div className="parts">
            <Card className="part">
              <div className="part-head">
                <h3>Kill switch</h3>
                <Badge tone={v5.kill_switch_engaged ? 'bad' : 'good'}>{v5.kill_switch_engaged ? 'Pulled' : 'Not pulled'}</Badge>
              </div>
              <p className="small muted">{v5.kill_switch_engaged ? 'All autonomous changes are stopped.' : 'Autonomous fixes can run when they qualify.'}</p>
            </Card>
            <Card className="part">
              <div className="part-head">
                <h3>Change freeze</h3>
                <Badge tone={v5.change_freeze ? 'warn' : 'good'}>{v5.change_freeze ? 'On' : 'Off'}</Badge>
              </div>
              <p className="small muted">{v5.change_freeze ? "I won't change anything until the freeze lifts." : 'Changes are allowed, with approval.'}</p>
            </Card>
            <Card className="part">
              <div className="part-head">
                <h3>Spending limit</h3>
                <Badge tone="neutral">{v5.spend_cap_usd ? '$' + v5.spend_cap_usd : 'None set'}</Badge>
              </div>
              <p className="small muted">How much model usage I'm allowed before I stop on my own.</p>
            </Card>
            {v5.autonomy_promotion && (
              <Card className="part">
                <div className="part-head">
                  <h3>Earning trust</h3>
                  <Badge tone="neutral">{v5.autonomy_promotion.state || v5.autonomy_promotion.status || 'reported'}</Badge>
                </div>
                <p className="small muted">{(v5.autonomy_promotion.reason || JSON.stringify(v5.autonomy_promotion)).slice(0, 220)}</p>
              </Card>
            )}
          </div>
        </>
      )}

      <h2 className="section">Experimental features</h2>
      <Card>
        <dl className="kv">
          <dt>Switched on</dt>
          <dd>{flags.length ? flags.map(f => <code key={f}>{f}</code>) : <span className="muted">None</span>}</dd>
          <dt>Set, but nothing uses them</dt>
          <dd>{unwired.length ? unwired.map(f => <code key={f}>{f}</code>) : <span className="muted">None, good</span>}</dd>
        </dl>
        {v5?.unenforceable_guard_config?.length > 0 && (
          <p className="err">These guard settings don't actually protect anything: {v5.unenforceable_guard_config.join('; ')}</p>
        )}
      </Card>
    </Page>
  );
}
