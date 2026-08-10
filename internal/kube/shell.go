package kube

import (
	"errors"
	"strings"
)

// Split breaks a command line into words the way a POSIX shell would, without
// running one. It follows Python's shlex in POSIX mode: a backslash outside
// quotes escapes the next character, single quotes are literal, and inside
// double quotes a backslash only escapes a quote or another backslash (so the
// jsonpath separator "\n" survives as backslash-n).
func Split(s string) ([]string, error) {
	var (
		out     []string
		tok     strings.Builder
		state   = ' ' // ' ' between words, 'a' inside a word, a quote char, or '\\'
		escaped = ' '
		quoted  bool
	)
	emit := func() {
		if tok.Len() > 0 || quoted {
			out = append(out, tok.String())
		}
		tok.Reset()
		quoted = false
	}
	isSpace := func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }
	for _, r := range s {
		switch {
		case state == ' ':
			switch {
			case isSpace(r):
				emit()
			case r == '\\':
				escaped, state = 'a', '\\'
			case r == '\'' || r == '"':
				state = r
			default:
				tok.WriteRune(r)
				state = 'a'
			}
		case state == 'a':
			switch {
			case isSpace(r):
				emit()
				state = ' '
			case r == '\'' || r == '"':
				state = r
			case r == '\\':
				escaped, state = 'a', '\\'
			default:
				tok.WriteRune(r)
			}
		case state == '\'' || state == '"':
			quoted = true
			switch {
			case r == state:
				state = 'a'
			case r == '\\' && state == '"':
				escaped, state = state, '\\'
			default:
				tok.WriteRune(r)
			}
		case state == '\\':
			if (escaped == '"' || escaped == '\'') && r != '\\' && r != escaped {
				tok.WriteRune('\\')
			}
			tok.WriteRune(r)
			state = escaped
		}
	}
	switch state {
	case '\'', '"':
		return nil, errors.New("No closing quotation")
	case '\\':
		return nil, errors.New("No escaped character")
	}
	emit()
	return out, nil
}

// splitPipes splits a command on unquoted '|' characters.
func splitPipes(command string) []string {
	var parts []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	for _, ch := range command {
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteRune(ch)
		case ch == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteRune(ch)
		case ch == '|' && !inSingle && !inDouble:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(ch)
		}
	}
	return append(parts, cur.String())
}
