// Package redact strips secrets from text before it is stored.
//
// It is line aware, not line wise. YAML puts a name and its value on different
// lines, so dropping any line that holds a keyword would remove the label and
// keep the credential. The rules, in order of preference:
//
//  1. Keep the key, redact the value. A key name is not a credential, and it is
//     what keeps a stored record auditable.
//  2. Values may live on later lines: a secret key opening a block scalar
//     (`tls.key: |`), and the name/value pair Kubernetes uses for env vars.
//  3. Some keys are secrets by convention with no keyword in them (`tls.key`).
//  4. PEM armour is redacted wherever it appears.
//
// Stated limits: a value with no secret looking key near it is kept, a base64
// blob in the middle of a line survives unless the whole line is base64, and
// this guards what is stored, not what is sent to the model provider.
package redact

import (
	"regexp"
	"strings"
)

var secretKeywords = []string{
	"password", "passwd", "secret", "api_key", "apikey", "token", "bearer",
	"authorization", "credentials", "client_secret", "private_key", "privatekey",
	"ssh_key", "passphrase",
}

var secretKeyNames = map[string]bool{
	"tls.key": true, "ca.key": true, "server.key": true, "client.key": true, "key.pem": true,
	".dockerconfigjson": true, ".dockercfg": true, ".netrc": true, ".pgpass": true,
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	"htpasswd": true, "keystore": true, "truststore": true, "keystore.jks": true,
}

var (
	urlRe   = regexp.MustCompile(`(?i)\b(https?)://[a-zA-Z0-9.\-_]+(?::\d+)?`)
	tokenRe = regexp.MustCompile(`\b(?:eyJ[a-zA-Z0-9_\-\.]{20,}|[a-zA-Z0-9_\-]{32,})\b`)
	kvRe    = regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|token|bearer|authorization|client[_-]?secret|private[_-]?key)\s*[:=]\s*\S+`)
	// `  - name: FOO`, `  key: value`, and the same as kubectl writes with -o json:
	// `  "name": "FOO",`. Group 3 is a quoted key, group 4 a bare one.
	lineRe        = regexp.MustCompile(`^(\s*)(-\s+)?(?:"([A-Za-z0-9_.\-/]+)"|([A-Za-z0-9_.\-/]+))\s*:\s*(.*?)\s*$`)
	quotedValueRe = regexp.MustCompile(`^"(.*)"(,?)$`)
	armorBegin    = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----`)
	armorEnd      = regexp.MustCompile(`-----END [A-Z0-9 ]+-----`)
	base64LineRe  = regexp.MustCompile(`^\s*[A-Za-z0-9+/]{40,}={0,2}\s*$`)
	blockScalarRe = regexp.MustCompile(`^[|>][+\-]?\d*$`)
)

const (
	marker      = "<redacted>"
	markerBlock = "<redacted-block>"
	markerPEM   = "<redacted-pem-block>"
	markerToken = "<redacted-token>"
	markerHost  = "<redacted-host>"
	markerLine  = "# <redacted-line>"
)

// Markers is every marker this package can leave behind. Anything that reports
// on a redaction has to count these.
var Markers = []string{markerLine, markerPEM, markerBlock, markerToken, markerHost, marker}

// Count returns (lines dropped, values replaced) in already redacted text.
func Count(text string) (dropped, replaced int) {
	for _, m := range Markers {
		n := strings.Count(text, m)
		if m == markerLine {
			dropped += n
		} else {
			replaced += n
		}
	}
	return
}

func isSecretName(token string) bool {
	low := strings.ToLower(strings.Trim(strings.TrimSpace(token), `"'`))
	if secretKeyNames[low] {
		return true
	}
	for _, kw := range secretKeywords {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func scrubInline(line string) string {
	line = urlRe.ReplaceAllString(line, "${1}://"+markerHost)
	return tokenRe.ReplaceAllString(line, markerToken)
}

// Identifier redacts a name (a map key, a label, an env var name), not a line.
// It applies only the substitutions that identify secret material, never the
// line rules: `Secrets("token")` returns a drop marker, so using it on keys
// would rename two ordinary fields to the same string and merge them.
func Identifier(name string) string {
	if name == "" {
		return name
	}
	return scrubInline(kvRe.ReplaceAllString(name, "${1}=<redacted>"))
}

// unwrap splits a captured value into (open, inner, close) so a redacted line
// keeps its shape. Without it, JSON output would slip through a YAML only rule.
func unwrap(raw string) (open, inner, close string) {
	if m := quotedValueRe.FindStringSubmatch(raw); m != nil {
		return `"`, m[1], `"` + m[2]
	}
	if strings.HasSuffix(raw, ",") {
		return "", raw[:len(raw)-1], ","
	}
	return "", raw, ""
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	text = strings.TrimSuffix(text, "\n")
	return strings.Split(text, "\n")
}

// Secrets returns a redacted copy of text. maxChars <= 0 means no cap.
func Secrets(text string, maxChars int) string {
	if text == "" {
		return ""
	}
	var out []string
	inArmor := false
	pending := -1 // indent of the key whose value is still to come

	for _, line := range splitLines(text) {
		indent := len(line) - len(strings.TrimLeft(line, " \t\v\f"))
		pad := strings.Repeat(" ", indent)

		if inArmor {
			if armorEnd.MatchString(line) {
				inArmor = false
			}
			continue
		}
		if armorBegin.MatchString(line) {
			out = append(out, pad+markerPEM)
			inArmor, pending = true, -1
			continue
		}

		m := lineRe.FindStringSubmatch(line)
		var quote, key, raw, openQ, value, closeQ string
		if m != nil {
			if m[3] != "" {
				quote, key = `"`, m[3]
			} else {
				key = m[4]
			}
			raw = m[5]
			openQ, value, closeQ = unwrap(raw)
		}

		if pending >= 0 {
			if m != nil && key == "value" && indent >= pending {
				out = append(out, pad+quote+"value"+quote+": "+openQ+marker+closeQ)
				pending = -1
				continue
			}
			if indent > pending && strings.TrimSpace(line) != "" {
				if len(out) == 0 || strings.TrimSpace(out[len(out)-1]) != markerBlock {
					out = append(out, pad+markerBlock)
				}
				continue
			}
			pending = -1
		}

		if m == nil {
			low := strings.ToLower(line)
			for _, kw := range secretKeywords {
				if strings.Contains(low, kw) {
					red := kvRe.ReplaceAllString(line, "${1}: "+marker)
					if red == line {
						red = markerLine
					}
					out = append(out, red)
					goto next
				}
			}
			if base64LineRe.MatchString(line) {
				out = append(out, pad+markerToken)
			} else {
				out = append(out, scrubInline(line))
			}
			continue
		}

		{
			prefix := pad + m[2] + quote + key + quote + ":"
			switch {
			case isSecretName(key):
				switch {
				case blockScalarRe.MatchString(value):
					out = append(out, prefix+" "+raw)
					pending = indent
				case value == "" || value == "{" || value == "[":
					// A mapping follows (valueFrom, secretKeyRef): references, not
					// material. Children are judged on their own lines.
					if raw != "" {
						out = append(out, prefix+" "+raw)
					} else {
						out = append(out, prefix)
					}
				default:
					out = append(out, prefix+" "+openQ+marker+closeQ)
				}
			case (key == "name" || key == "key") && isSecretName(value):
				// The name is kept: it is the label that makes the record auditable.
				out = append(out, prefix+" "+raw)
				pending = indent
			case base64LineRe.MatchString(value):
				out = append(out, prefix+" "+openQ+markerToken+closeQ)
			default:
				out = append(out, scrubInline(line))
			}
		}
	next:
	}

	result := strings.Join(out, "\n")
	if maxChars > 0 {
		if r := []rune(result); len(r) > maxChars {
			result = string(r[:maxChars-5]) + "[...]"
		}
	}
	return result
}
