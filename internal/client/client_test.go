package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sseServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)
	return New(ts.URL, "k1", "bob")
}

func frame(w http.ResponseWriter, s string) { io.WriteString(w, "data: "+s+"\n\n") }

func TestSendStreamsContentAndCapturesApproval(t *testing.T) {
	var gotAuth, gotSession, gotBody string
	c := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotSession = r.Header.Get("Authorization"), r.Header.Get("X-Session-ID")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		frame(w, `{"object":"stream.start"}`)
		io.WriteString(w, ": keepalive\n\n")
		frame(w, `{"choices":[{"delta":{"content":"Restart "}}]}`)
		frame(w, `{"choices":[{"delta":{"content":"web?"}}],"hitl_required":true,"action_id":"a1","risk_level":"high","human_summary":"kubectl delete pod web"}`)
		frame(w, `[DONE]`)
	})
	var out bytes.Buffer
	turn, err := c.Send(context.Background(), "s1", "restart web", false, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "Restart web?" || !strings.Contains(out.String(), "Restart web?") {
		t.Errorf("text %q out %q", turn.Text, out.String())
	}
	if turn.Approval == nil || turn.Approval.ActionID != "a1" || turn.Approval.Risk != "high" {
		t.Errorf("%+v", turn.Approval)
	}
	if gotAuth != "Bearer k1" || gotSession != "s1" || !strings.Contains(gotBody, `"user":"bob"`) || !strings.Contains(gotBody, `"stream":true`) {
		t.Errorf("%q %q %s", gotAuth, gotSession, gotBody)
	}
}

func TestSendSurfacesServerErrors(t *testing.T) {
	c := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"detail":"slow down"}`)
	})
	_, err := c.Send(context.Background(), "s", "hi", false, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") {
		t.Errorf("%v", err)
	}
}

func TestLoopAsksForApprovalThenResets(t *testing.T) {
	var sent []string
	c := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = append(sent, string(b))
		if len(sent) == 1 {
			frame(w, `{"choices":[{"delta":{"content":"ok?"}}],"hitl_required":true,"action_id":"a","risk_level":"low","human_summary":"x"}`)
		} else {
			frame(w, `{"choices":[{"delta":{"content":"done"}}]}`)
		}
		frame(w, `[DONE]`)
	})
	var out bytes.Buffer
	s := &Session{C: c, ID: "s", In: strings.NewReader("scale web\nyes\n/exit\n"), Out: &out}
	if err := s.Loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || !strings.Contains(sent[1], "yes") {
		t.Errorf("%v", sent)
	}
	if strings.Count(out.String(), "approve?") != 1 {
		t.Errorf("the prompt should show only after an approval request:\n%s", out.String())
	}
}

func replayServer(t *testing.T, meta string) *Client {
	return sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		frame(w, meta)
		frame(w, `{"type":"tool_call","tool":"kubectl"}`)
		frame(w, `[DONE]`)
	})
}

func TestReplayExitCodesKeepUnverifiedApartFromBroken(t *testing.T) {
	for _, tc := range []struct {
		meta string
		code int
	}{
		{`{"type":"replay_meta","episode_id":"e","records":1,"chain_valid":true,"chain_verified":true}`, 0},
		{`{"type":"replay_meta","episode_id":"e","records":1,"chain_valid":false,"chain_verified":true}`, 3},
		{`{"type":"replay_meta","episode_id":"e","records":1,"chain_valid":true,"chain_verified":false}`, 4},
	} {
		var out bytes.Buffer
		code, err := replayServer(t, tc.meta).Replay(context.Background(), "e", &out)
		if err != nil || code != tc.code {
			t.Errorf("%s: code %d err %v", tc.meta, code, err)
		}
		if !strings.Contains(out.String(), "tool_call") {
			t.Errorf("rows missing: %s", out.String())
		}
	}
}

func TestStatusSummary(t *testing.T) {
	c := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"ok","version":"0.1.0","recorder":{"state":"connected"}}`)
	})
	text, ok, err := c.Status(context.Background())
	if err != nil || !ok || !strings.Contains(text, "0.1.0") || !strings.Contains(text, "recorder") {
		t.Errorf("%q %v %v", text, ok, err)
	}
}

func TestPostmortemExitCodes(t *testing.T) {
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"markdown":"# pm","chain_valid":true,"chain_verified":true}`, 0},
		{`{"markdown":"# pm","chain_valid":false,"chain_verified":true}`, 3},
		{`{"markdown":"# pm","chain_valid":false,"chain_verified":false}`, 4},
	} {
		c := sseServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) })
		md, code, err := c.Postmortem(context.Background(), "e")
		if err != nil || code != tc.code || md != "# pm" {
			t.Errorf("%s: %q %d %v", tc.body, md, code, err)
		}
	}
}

func TestDigestFetchesMarkdown(t *testing.T) {
	var path string
	c := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		io.WriteString(w, `{"markdown":"# digest"}`)
	})
	md, err := c.Digest(context.Background(), 12)
	if err != nil || md != "# digest" || !strings.Contains(path, "hours=12") || !strings.Contains(path, "format=markdown") {
		t.Errorf("%q %v %s", md, err, path)
	}
}
