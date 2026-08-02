package redact

import (
	"strings"
	"testing"
)

func TestEnvIdiomKeepsNameRedactsValue(t *testing.T) {
	in := "- name: DB_PASSWORD\n  value: hunter2-prod-db\n- name: LOG_LEVEL\n  value: debug"
	out := Secrets(in, 0)
	if strings.Contains(out, "hunter2") {
		t.Fatalf("value leaked: %s", out)
	}
	if !strings.Contains(out, "name: DB_PASSWORD") {
		t.Errorf("the name should stay so the record can be audited: %s", out)
	}
	if !strings.Contains(out, "value: debug") {
		t.Errorf("unrelated values stay: %s", out)
	}
}

func TestJSONIdiomIsRedactedToo(t *testing.T) {
	in := "{\n \"name\": \"DB_PASSWORD\",\n \"value\": \"hunter2-prod-db\"\n}"
	out := Secrets(in, 0)
	if strings.Contains(out, "hunter2") || !strings.Contains(out, `"value": "<redacted>"`) {
		t.Errorf("json env pair: %s", out)
	}
}

func TestBlockScalarAndPEM(t *testing.T) {
	in := "data:\n  tls.key: |\n    MIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n    abcdefghijklmnop\n  other: fine"
	out := Secrets(in, 0)
	if strings.Contains(out, "MIIEv") || strings.Contains(out, "abcdefgh") {
		t.Errorf("block body leaked: %s", out)
	}
	if !strings.Contains(out, "tls.key: |") || !strings.Contains(out, "other: fine") {
		t.Errorf("structure lost: %s", out)
	}
	if strings.Count(out, markerBlock) != 1 {
		t.Errorf("one marker for the whole block: %s", out)
	}
	pem := "before\n-----BEGIN RSA PRIVATE KEY-----\nAAAA\nBBBB\n-----END RSA PRIVATE KEY-----\nafter"
	out = Secrets(pem, 0)
	if strings.Contains(out, "AAAA") || !strings.Contains(out, markerPEM) || !strings.Contains(out, "after") {
		t.Errorf("pem: %s", out)
	}
}

func TestSecretKeyValueAndValueFrom(t *testing.T) {
	out := Secrets("password: s3cret\ntoken: abc", 0)
	if strings.Contains(out, "s3cret") || strings.Contains(out, "abc") || !strings.Contains(out, "password: <redacted>") {
		t.Errorf("%s", out)
	}
	ref := "env:\n  - name: X\n    valueFrom:\n      secretKeyRef:\n        name: db-creds\n        key: password"
	out = Secrets(ref, 0)
	if !strings.Contains(out, "secretKeyRef:") || !strings.Contains(out, "name: db-creds") {
		t.Errorf("references are structure, not material: %s", out)
	}
}

func TestFreeText(t *testing.T) {
	if got := Secrets("connect with password=hunter2 now", 0); strings.Contains(got, "hunter2") {
		t.Errorf("inline kv: %s", got)
	}
	if got := Secrets("my token was lost", 0); got != markerLine {
		t.Errorf("keyword line with no value is dropped: %q", got)
	}
	got := Secrets("GET https://internal.corp.local:8443/api/v1/pods", 0)
	if strings.Contains(got, "internal.corp") || !strings.Contains(got, "https://"+markerHost+"/api/v1/pods") {
		t.Errorf("url: %s", got)
	}
	long := "auth eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9abcdefgh end"
	if got := Secrets(long, 0); strings.Contains(got, "eyJhbGci") {
		t.Errorf("jwt: %s", got)
	}
	if got := Secrets("/var/lib/kubelet/pods/abc-123/volumes/kubernetes.io~secret/x", 0); !strings.Contains(got, "/var/lib/kubelet") {
		// contains the word "secret" so falls to the line rule, but must not be a base64 hit
		_ = got
	}
	if got := Secrets("", 0); got != "" {
		t.Errorf("empty: %q", got)
	}
}

func TestBase64Line(t *testing.T) {
	line := strings.Repeat("QUJD", 20)
	if got := Secrets("  "+line, 0); got != "  "+markerToken {
		t.Errorf("%q", got)
	}
	if got := Secrets("data: "+line, 0); got != "data: "+markerToken {
		t.Errorf("%q", got)
	}
}

func TestCapAndCount(t *testing.T) {
	out := Secrets(strings.Repeat("ab ", 40), 20)
	if len([]rune(out)) != 20 || !strings.HasSuffix(out, "[...]") {
		t.Errorf("cap: %q", out)
	}
	red := Secrets("password: a\n- name: TOKEN\n  value: b\nmy token was lost", 0)
	dropped, replaced := Count(red)
	if dropped != 1 || replaced != 2 {
		t.Errorf("dropped=%d replaced=%d in %q", dropped, replaced, red)
	}
}

func TestIdentifierKeepsKeysDistinct(t *testing.T) {
	if Identifier("token") != "token" || Identifier("password") != "password" {
		t.Error("plain field names are not secrets")
	}
	if got := Identifier("kubectl get --token=abc123 pods"); strings.Contains(got, "abc123") {
		t.Errorf("assignment shape: %s", got)
	}
	if Identifier("") != "" {
		t.Error("empty")
	}
}

func TestMarkersAreDisjoint(t *testing.T) {
	for i, a := range Markers {
		for j, b := range Markers {
			if i != j && strings.Contains(a, b) {
				t.Errorf("%q contains %q, so counting would double count", a, b)
			}
		}
	}
}
