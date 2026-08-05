// Package recorder is the flight recorder: an append only, hash chained log of
// every typed event the server emits, one chain per episode.
//
// Writes never block and never raise. Events go onto a queue and a background
// task batches them into the database, so a recorder outage costs auditability,
// not availability.
//
// What the chain proves: no persisted row was modified, reordered or removed. It
// cannot prove the log is complete, because the write path is fire and forget.
// Those are different failures and are kept apart:
//
//   - A lost batch must not leave a seq gap. A database blip is not tampering, so
//     on failure the cached head is dropped and the next batch continues from the
//     last persisted row.
//   - A lost batch must not be invisible either. Every loss is carried forward and
//     written into the chain as a recorder_gap record on the next good flush, so
//     it cannot be removed without breaking verification.
//   - A recorder that never started loses events too. The connection is retried,
//     and a loss with no queue is carried exactly like a failed flush.
package recorder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

const (
	GapKind          = "recorder_gap"
	batchMax         = 50
	flushInterval    = 500 * time.Millisecond
	queueSize        = 10000
	maxTrackedEps    = 1000
	defaultRetryWait = 30 * time.Second
)

// ErrUnavailable means the log could not be read, which is not the same as an
// episode having no rows. A caller must not render it as absence.
var ErrUnavailable = errors.New("the flight recorder is not available")

type item struct {
	episode, kind string
	payload       map[string]any
}

type head struct {
	seq  int64
	hash string
}

type gap struct {
	count  int
	reason string
}

// Recorder writes the decision log.
type Recorder struct {
	db      *store.DB
	enabled bool
	redact  bool

	RetryInterval time.Duration

	mu      sync.Mutex
	state   string // starting | flag | ready | unavailable
	reason  string
	lost    int64
	chains  map[string]head // episode -> (next seq, last hash)
	pending map[string]gap
	queue   chan item

	cancel context.CancelFunc
	done   sync.WaitGroup
}

// New builds a recorder. enabled=false leaves it inert (state "flag").
func New(db *store.DB, enabled, redactSecrets bool) *Recorder {
	return &Recorder{
		db: db, enabled: enabled, redact: redactSecrets, state: "starting",
		RetryInterval: defaultRetryWait,
		chains:        map[string]head{}, pending: map[string]gap{},
	}
}

// Status is the shape reported on /healthz. An operator must be able to see that
// nothing is recorded, and how much was lost while it was down.
func (r *Recorder) Status() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{
		"enabled": r.state == "ready", "state": r.state,
		"reason": r.reason, "lost_while_down": r.lost,
	}
}

// Start connects and starts the drain task. A failed connect retries in the
// background instead of giving up.
func (r *Recorder) Start(ctx context.Context) {
	if !r.enabled {
		r.setState("flag", "FLIGHT_RECORDER_ENABLED=false")
		slog.Info("flight recorder disabled by flag")
		return
	}
	ctx, r.cancel = context.WithCancel(ctx)
	if r.connect(ctx) {
		return
	}
	r.done.Add(1)
	go func() {
		defer r.done.Done()
		t := time.NewTicker(r.RetryInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if r.connect(ctx) {
					r.mu.Lock()
					lost := r.lost
					r.mu.Unlock()
					if lost > 0 {
						slog.Warn("flight recorder recovered, lost events are written into the chain as gap records", "lost", lost)
					}
					return
				}
			}
		}
	}()
}

func (r *Recorder) setState(state, reason string) {
	r.mu.Lock()
	r.state, r.reason = state, reason
	r.mu.Unlock()
}

// connect makes one attempt. On success the queue and drain task start.
func (r *Recorder) connect(ctx context.Context) bool {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.db.PingContext(pctx); err != nil {
		r.setState("unavailable", err.Error())
		slog.Warn("flight recorder could not connect, nothing is being recorded", "retry", r.RetryInterval, "err", err)
		return false
	}
	r.mu.Lock()
	r.queue = make(chan item, queueSize)
	q := r.queue
	r.state, r.reason = "ready", ""
	r.mu.Unlock()
	r.done.Add(1)
	go func() {
		defer r.done.Done()
		r.drain(ctx, q)
	}()
	slog.Info("flight recorder ready")
	return true
}

// Close flushes what is queued and stops the background tasks.
func (r *Recorder) Close() {
	if r.cancel != nil {
		r.cancel()
	}
	r.done.Wait()
	r.mu.Lock()
	r.state, r.reason = "starting", ""
	r.queue = nil
	r.chains = map[string]head{}
	r.pending = map[string]gap{}
	r.mu.Unlock()
}

// Record queues one event. It never blocks and never panics. Token frames are
// skipped: they would bloat the table for no audit value.
func (r *Recorder) Record(episode, kind string, payload map[string]any) {
	if kind == "token" {
		return
	}
	r.mu.Lock()
	q, state, reason := r.queue, r.state, r.reason
	r.mu.Unlock()
	if q == nil {
		// Off by flag there is no chain to be honest about. A recorder that
		// failed is the case the gap ledger exists for.
		if state == "unavailable" {
			if reason == "" {
				reason = "the recorder was not running"
			}
			r.noteLoss(episode, kind, payload, reason)
		}
		return
	}
	select {
	case q <- item{episode, kind, payload}:
	default:
		r.noteLoss(episode, kind, payload, "the recorder queue was full")
	}
}

// noteLoss carries an event that never reached the queue, so the next good flush
// writes it into the chain as a gap. Bounded: overflow is counted, not tracked.
func (r *Recorder) noteLoss(episode, kind string, payload map[string]any, reason string) {
	r.mu.Lock()
	r.lost++
	lost, state, why := r.lost, r.state, r.reason
	if _, tracked := r.pending[episode]; tracked || len(r.pending) < maxTrackedEps {
		r.carryLoss([]item{{episode, kind, payload}}, reason)
	}
	r.mu.Unlock()
	if lost == 1 || lost%1000 == 0 {
		slog.Warn("flight recorder events not recorded", "count", lost, "state", state, "reason", why)
	}
}

// carryLoss is called with r.mu held. A batch was not persisted, so:
//  1. drop the cached head for each episode, so the next flush continues from the
//     last persisted row (a stale head would write a seq gap that reads as tamper);
//  2. carry the count forward, so the next flush writes a recorder_gap row
//     (step 1 alone would make loss undetectable).
func (r *Recorder) carryLoss(batch []item, reason string) {
	for _, it := range batch {
		delete(r.chains, it.episode)
		lost := 1
		if it.kind == GapKind {
			if n, ok := it.payload["dropped"].(int); ok {
				lost = n
			}
		}
		g, ok := r.pending[it.episode]
		if !ok {
			g.reason = reason
		}
		g.count += lost
		r.pending[it.episode] = g
	}
}

func (r *Recorder) drain(ctx context.Context, q chan item) {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	var batch []item
	lastGapTry := time.Now()
	flush := func() {
		if len(batch) > 0 {
			r.flush(context.Background(), batch)
			batch = nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case it := <-q:
					batch = append(batch, it)
					continue
				default:
				}
				break
			}
			r.flush(context.Background(), batch)
			return
		case it := <-q:
			batch = append(batch, it)
			if len(batch) >= batchMax {
				flush()
			}
		case <-t.C:
			flush()
			// Loss with nothing queued still needs writing once the store is back.
			// Retried slowly so an outage does not become a log flood.
			if time.Since(lastGapTry) >= 5*time.Second {
				lastGapTry = time.Now()
				r.flush(ctx, nil)
			}
		}
	}
}

func dropReason(err error) string {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	low := strings.ToLower(msg)
	if strings.Contains(low, "decision_log") && (strings.Contains(low, "does not exist") || strings.Contains(low, "no such table")) {
		return "the decision_log table was missing"
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	if msg == "" {
		return "unknown error"
	}
	return msg
}

func gapPayload(count int, reason string) map[string]any {
	return map[string]any{
		"type": GapKind, "dropped": count, "reason": reason,
		"message": fmt.Sprintf("%d recorded event(s) were LOST at this point: the flight recorder could not write them (%s). "+
			"This episode is incomplete; the chain below is still verifiable, but it is not the whole story.", count, reason),
	}
}

type pendingRow struct {
	episode, kind, payload string
	seq                    int64
	prev, hash             string
	trace, span, parent    any
}

// flush writes a batch. Loss from an earlier flush goes first, so a replay shows
// the hole in the right place rather than at the end.
func (r *Recorder) flush(ctx context.Context, items []item) {
	r.mu.Lock()
	if len(items) == 0 && len(r.pending) == 0 {
		r.mu.Unlock()
		return
	}
	batch := make([]item, 0, len(r.pending)+len(items))
	eps := make([]string, 0, len(r.pending))
	for ep := range r.pending {
		eps = append(eps, ep)
	}
	sortStrings(eps)
	for _, ep := range eps {
		g := r.pending[ep]
		batch = append(batch, item{ep, GapKind, gapPayload(g.count, g.reason)})
	}
	r.pending = map[string]gap{}
	r.mu.Unlock()
	batch = append(batch, items...)

	rows, err := r.build(ctx, batch)
	if err == nil {
		err = r.insert(ctx, rows)
	}
	if err != nil {
		reason := dropReason(err)
		r.mu.Lock()
		r.carryLoss(batch, reason)
		r.mu.Unlock()
		if reason == "the decision_log table was missing" {
			slog.Warn("flight recorder: the decision_log table is missing, run: kube-sre db-init", "events_lost", len(batch))
		} else {
			slog.Warn("flight recorder batch insert failed, recorded as a gap once writes recover", "events_lost", len(batch), "err", err)
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// build assigns seq numbers and hashes. The cached head advances as it goes and
// is dropped by carryLoss if the insert then fails.
func (r *Recorder) build(ctx context.Context, batch []item) ([]pendingRow, error) {
	rows := make([]pendingRow, 0, len(batch))
	for _, it := range batch {
		payload, err := normalize(it.payload)
		if err != nil {
			return nil, err
		}
		if r.redact {
			payload = scrub(payload)
		}
		seq, prev, err := r.chainState(ctx, it.episode)
		if err != nil {
			return nil, err
		}
		digest := ComputeHash(prev, it.episode, seq, it.kind, payload)
		r.mu.Lock()
		r.chains[it.episode] = head{seq + 1, digest}
		r.mu.Unlock()
		body, err := marshal(payload)
		if err != nil {
			return nil, err
		}
		pr := pendingRow{episode: it.episode, kind: it.kind, payload: body, seq: seq, prev: prev, hash: digest}
		if it.kind == "ki_otel_span" {
			pr.trace, pr.span, pr.parent = payload["trace_id"], payload["span_id"], payload["parent_span_id"]
		}
		rows = append(rows, pr)
	}
	return rows, nil
}

// chainState returns (next seq, last hash), loading from the database the first
// time an episode is seen. It returns an error if the lookup fails: an unknown
// head is not a genesis head, and caching (0, "") would restart the chain for an
// episode that already has rows.
func (r *Recorder) chainState(ctx context.Context, episode string) (int64, string, error) {
	r.mu.Lock()
	if h, ok := r.chains[episode]; ok {
		r.mu.Unlock()
		return h.seq, h.hash, nil
	}
	r.mu.Unlock()

	next, last := int64(0), ""
	var seq int64
	var hash string
	err := r.db.QueryRowContext(ctx, r.db.Q(
		`SELECT seq, hash FROM decision_log WHERE episode_id = ? ORDER BY seq DESC LIMIT 1`), episode).Scan(&seq, &hash)
	switch {
	case err == nil:
		next, last = seq+1, hash
	case !errors.Is(err, sql.ErrNoRows):
		return 0, "", err
	}
	// If the head is ahead of the surviving rows, events were removed. Continue
	// past the head rather than reusing consumed numbers: re-anchoring would heal
	// the chain and erase the only evidence that the truncation happened.
	var hseq int64
	var hhash string
	herr := r.db.QueryRowContext(ctx, r.db.Q(`SELECT seq, hash FROM decision_log_head WHERE episode_id = ?`), episode).Scan(&hseq, &hhash)
	if herr != nil && !errors.Is(herr, sql.ErrNoRows) {
		slog.Warn("flight recorder chain head read failed", "episode", episode, "err", herr)
	} else if herr == nil && hseq+1 > next {
		slog.Warn("flight recorder episode resumes behind its head, events were removed; continuing past the head so the gap stays visible",
			"episode", episode, "resumes", next, "head", hseq)
		next = hseq + 1
	}
	r.mu.Lock()
	r.chains[episode] = head{next, last}
	r.mu.Unlock()
	return next, last, nil
}

func (r *Recorder) insert(ctx context.Context, rows []pendingRow) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt := r.db.Q(`INSERT INTO decision_log
		(episode_id, seq, kind, payload, prev_hash, hash, trace_id, span_id, parent_span_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	for _, p := range rows {
		if _, err = tx.ExecContext(ctx, stmt, p.episode, p.seq, p.kind, p.payload, p.prev, p.hash,
			nullable(p.trace), nullable(p.span), nullable(p.parent)); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Anchor each episode's newest row. Separate from the insert: the events are
	// already durable, and a head that failed to write must not become a lost
	// batch. A lagging head reads as "ahead of its head", which warns and does not
	// accuse.
	heads := map[string]head{}
	for _, p := range rows {
		if h, ok := heads[p.episode]; !ok || p.seq > h.seq {
			heads[p.episode] = head{p.seq, p.hash}
		}
	}
	up := r.db.Q(`INSERT INTO decision_log_head (episode_id, seq, hash, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (episode_id) DO UPDATE SET seq = excluded.seq, hash = excluded.hash, updated_at = CURRENT_TIMESTAMP`)
	for ep, h := range heads {
		if _, err := r.db.ExecContext(ctx, up, ep, h.seq, h.hash); err != nil {
			slog.Warn("flight recorder anchor write failed, the events are persisted but a truncation would go unnoticed until the next flush re-anchors",
				"episode", ep, "err", err)
		}
	}
	return nil
}

func nullable(v any) any {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return nil
}

// FetchEpisode returns every row of an episode ordered by seq. An empty result
// means the episode has no rows, and nothing else; a failure is ErrUnavailable.
func (r *Recorder) FetchEpisode(ctx context.Context, episode string) ([]Row, error) {
	rows, err := r.db.QueryContext(ctx, r.db.Q(
		`SELECT episode_id, seq, kind, payload, prev_hash, hash FROM decision_log WHERE episode_id = ? ORDER BY seq`), episode)
	if err != nil {
		slog.Warn("flight recorder fetch failed", "err", err)
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var row Row
		var raw []byte
		if err := rows.Scan(&row.EpisodeID, &row.Seq, &row.Kind, &raw, &row.PrevHash, &row.Hash); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if row.Payload, err = decode(raw); err != nil {
			return nil, fmt.Errorf("%w: payload of seq %d is not JSON: %v", ErrUnavailable, row.Seq, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return out, nil
}

// Verdict is a tamper verdict with the third state a boolean cannot hold. Valid
// answers "did anything contradict the records"; Verified answers "was the
// question actually asked". An anchor that could not be read leaves Valid true
// and Verified false: nothing contradicted the records, but nothing could have.
type Verdict struct {
	Valid    bool `json:"valid"`
	Verified bool `json:"verified"`
}

// HeadVerdict compares an episode's surviving chain against its persisted head.
// A missing head is verified: the anchor was read and there is none to contradict
// these rows, which is a performed check with a benign answer.
func (r *Recorder) HeadVerdict(ctx context.Context, episode string, rows []Row) Verdict {
	var hseq int64
	var hhash string
	err := r.db.QueryRowContext(ctx, r.db.Q(`SELECT seq, hash FROM decision_log_head WHERE episode_id = ?`), episode).Scan(&hseq, &hhash)
	if errors.Is(err, sql.ErrNoRows) {
		return Verdict{true, true}
	}
	if err != nil {
		slog.Warn("flight recorder chain head read failed", "episode", episode, "err", err)
		return Verdict{true, false}
	}
	if len(rows) == 0 {
		slog.Warn("flight recorder episode has no events but its head records some, every event was removed", "episode", episode, "head_seq", hseq)
		return Verdict{false, true}
	}
	last := rows[len(rows)-1]
	switch {
	case last.Seq < hseq:
		slog.Warn("flight recorder episode ends before its head, newest events were removed", "episode", episode, "ends", last.Seq, "head", hseq)
		return Verdict{false, true}
	case last.Seq > hseq:
		// Rows the head never saw: a crash between the two writes, or an append
		// that bypassed this package. Not truncation.
		slog.Warn("flight recorder episode is ahead of its head", "episode", episode, "ends", last.Seq, "head", hseq)
		return Verdict{true, true}
	}
	return Verdict{last.Hash == hhash, true}
}

// VerifyEpisode is the full verdict: links and the truncation anchor. Anything
// that renders a verdict must show Verified as well as Valid.
func (r *Recorder) VerifyEpisode(ctx context.Context, episode string, rows []Row) Verdict {
	if VerifyChain(rows, 0, "") {
		return r.HeadVerdict(ctx, episode, rows)
	}
	// The links did not recompute from the origin. That is a positive finding
	// unless the front of the chain was removed on purpose, which can only be
	// said if a truncation was declared with the hash of an archive.
	if len(rows) == 0 || rows[0].Seq == 0 {
		return Verdict{false, true}
	}
	d := DeclaredStart(ctx, r.db, "decision_log", episode)
	if !d.Read {
		slog.Warn("flight recorder episode does not start at seq 0 and the truncation record could not be read, so it is not verified", "episode", episode)
		return Verdict{true, false}
	}
	if !d.Found {
		slog.Warn("flight recorder episode starts past seq 0 with no recorded truncation, its earliest events were removed", "episode", episode, "starts", rows[0].Seq)
		return Verdict{false, true}
	}
	if rows[0].Seq != d.Seq || rows[0].PrevHash != d.PrevHash {
		slog.Warn("flight recorder truncation record does not describe the surviving chain", "episode", episode, "record", d.Seq, "starts", rows[0].Seq)
		return Verdict{false, true}
	}
	if !VerifyChain(rows, d.Seq, d.PrevHash) {
		return Verdict{false, true}
	}
	return r.HeadVerdict(ctx, episode, rows)
}

// MarshalPayload renders a row's payload as JSON for exports and the API.
func (row Row) MarshalPayload() string {
	b, _ := json.Marshal(row.Payload)
	return string(b)
}
