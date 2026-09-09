package memory

import (
	"encoding/json"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

func migrationsOf(version int, name, sql string) []store.Migration {
	return []store.Migration{{Version: version, Name: name, SQL: sql}}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func parseStrings(raw []byte) []string {
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

// distinctFrom is the null safe inequality of each dialect.
func (s *Store) distinctFrom() string {
	if s.DB.Dialect == store.Postgres {
		return "IS DISTINCT FROM"
	}
	return "IS NOT"
}
