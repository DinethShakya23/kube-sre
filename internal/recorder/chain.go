package recorder

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/DinethShakya23/kube-sre/internal/redact"
)

const (
	maxFieldChars = 1500
	maxScrubDepth = 6
	tooDeep       = "<redacted-unscannable-depth>"
)

// canonical is the exact byte form that gets hashed: sorted keys, no spaces.
// Payloads are round-tripped first so the hashed bytes are the same bytes a
// verifier reads back from the database.
func canonical(episode string, seq int64, kind string, payload map[string]any) (string, error) {
	return marshal(map[string]any{
		"episode_id": episode, "seq": seq, "kind": kind, "payload": payload,
	})
}

func marshal(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(b.Bytes(), "\n")), nil
}

// ComputeHash is sha256(prev_hash + canonical form). The genesis prev_hash is "".
func ComputeHash(prev, episode string, seq int64, kind string, payload map[string]any) string {
	c, _ := canonical(episode, seq, kind, payload)
	sum := sha256.Sum256([]byte(prev + c))
	return hex.EncodeToString(sum[:])
}

// normalize turns any payload into plain JSON types, keeping numbers exact.
func normalize(payload map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return decode(raw)
}

func decode(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// Row is one persisted decision_log record.
type Row struct {
	EpisodeID string
	Seq       int64
	Kind      string
	Payload   map[string]any
	PrevHash  string
	Hash      string
}

// VerifyChain recomputes the links over rows ordered by seq. It cannot see a
// truncation: deleting the newest rows leaves a shorter chain whose links all
// still verify. Callers that show a tamper verdict must also check the head.
//
// startSeq and startPrev say where the chain is expected to begin. The defaults
// (0, "") mean the whole chain from its origin, so a chain whose first rows are
// gone fails. They are non-default only for a front that was removed on
// purpose, described by a truncation record.
func VerifyChain(rows []Row, startSeq int64, startPrev string) bool {
	prev, want := startPrev, startSeq
	for _, r := range rows {
		if r.Seq != want || r.PrevHash != prev {
			return false
		}
		if ComputeHash(prev, r.EpisodeID, r.Seq, r.Kind, r.Payload) != r.Hash {
			return false
		}
		prev = r.Hash
		want++
	}
	return true
}

// ── payload hygiene ──────────────────────────────────────────────────────────

// scrubKey redacts a map key without merging two distinct keys: two bearer
// tokens both redact to the same string, and losing a field to hide something
// in it is the wrong trade.
func scrubKey(key string, seen map[string]bool) string {
	scrubbed := redact.Identifier(key)
	if scrubbed == key {
		return key
	}
	candidate, n := scrubbed, 1
	for seen[candidate] {
		n++
		candidate = scrubbed + "#" + strconv.Itoa(n)
	}
	seen[candidate] = true
	return candidate
}

// scrubValue redacts every string anywhere in a payload, keys included. A depth
// bound that returned the rest untouched would fail open, so past the bound a
// subtree is replaced instead.
func scrubValue(v any, capChars, depth int) any {
	if s, ok := v.(string); ok {
		if s == "" {
			return s
		}
		return redact.Secrets(s, capChars)
	}
	if depth >= maxScrubDepth {
		switch v.(type) {
		case map[string]any, []any:
			return tooDeep
		}
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		seen := map[string]bool{}
		out := make(map[string]any, len(x))
		for _, k := range keys {
			out[scrubKey(k, seen)] = scrubValue(x[k], 0, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = scrubValue(e, 0, depth+1)
		}
		return out
	}
	return v
}

// scrub redacts secrets in a payload and caps long top level strings.
func scrub(payload map[string]any) map[string]any {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	out := make(map[string]any, len(payload))
	for _, k := range keys {
		out[scrubKey(k, seen)] = scrubValue(payload[k], maxFieldChars, 0)
	}
	return out
}
