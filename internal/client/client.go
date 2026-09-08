// Package client is the command line side of the API: a chat session with
// approval prompts, a status check and episode replay.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to a kube-sre server.
type Client struct {
	Base string
	Key  string
	User string
	HTTP *http.Client
}

func New(base, key, user string) *Client {
	if user == "" {
		user = "default"
	}
	return &Client{Base: strings.TrimRight(base, "/"), Key: key, User: user, HTTP: &http.Client{}}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, header map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return c.HTTP.Do(req)
}

func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Detail any `json:"detail"`
	}
	if json.Unmarshal(b, &e) == nil && e.Detail != nil {
		return fmt.Errorf("server said %d: %v", resp.StatusCode, e.Detail)
	}
	return fmt.Errorf("server said %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

// Approval is a pending write that waits for the operator.
type Approval struct {
	ActionID string
	Risk     string
	Summary  string
}

// Turn is what one chat request produced.
type Turn struct {
	Text     string
	Approval *Approval
	Usage    map[string]any
	Error    string
}

// Send posts one message and streams the answer to out as it arrives. Tool
// activity and other side events are shown only when verbose.
func (c *Client) Send(ctx context.Context, session, message string, autoApprove, verbose bool, out io.Writer) (*Turn, error) {
	body, _ := json.Marshal(map[string]any{
		"model": "kube-sre", "stream": true, "user": c.User, "auto_approve": autoApprove,
		"messages": []map[string]string{{"role": "user", "content": message}},
	})
	resp, err := c.do(ctx, "POST", "/v1/chat/completions", body, map[string]string{"X-Session-ID": session})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	turn := &Turn{}
	err = readSSE(resp.Body, func(data string) bool {
		if data == "[DONE]" {
			return false
		}
		var f struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			HitlRequired bool           `json:"hitl_required"`
			ActionID     string         `json:"action_id"`
			Risk         string         `json:"risk_level"`
			Summary      string         `json:"human_summary"`
			KIEvent      map[string]any `json:"ki_event"`
			Usage        map[string]any `json:"usage"`
			Error        any            `json:"error"`
		}
		if json.Unmarshal([]byte(data), &f) != nil {
			return true
		}
		if f.HitlRequired {
			turn.Approval = &Approval{ActionID: f.ActionID, Risk: f.Risk, Summary: f.Summary}
		}
		if f.Usage != nil {
			turn.Usage = f.Usage
		}
		if f.Error != nil {
			turn.Error = fmt.Sprint(f.Error)
		}
		if verbose && f.KIEvent != nil {
			fmt.Fprintf(out, "\n  [%v]", f.KIEvent["type"])
		}
		for _, ch := range f.Choices {
			turn.Text += ch.Delta.Content
			fmt.Fprint(out, ch.Delta.Content)
		}
		return true
	})
	fmt.Fprintln(out)
	return turn, err
}

// readSSE calls fn with each data payload until fn returns false or the stream ends.
func readSSE(r io.Reader, fn func(data string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // blank lines and keepalive comments
		}
		if !fn(strings.TrimSpace(strings.TrimPrefix(line, "data:"))) {
			return nil
		}
	}
	return sc.Err()
}

// Status fetches /healthz and returns a readable summary and whether the server
// answered ok.
func (c *Client) Status(ctx context.Context) (string, bool, error) {
	resp, err := c.do(ctx, "GET", "/healthz", nil, nil)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, apiError(resp)
	}
	var h map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return "", false, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "status   %v (version %v)\n", h["status"], h["version"])
	for _, name := range []string{"sensorium", "recorder", "audit", "memory", "leader", "db_schema"} {
		if v, ok := h[name]; ok {
			b, _ := json.Marshal(v)
			fmt.Fprintf(&sb, "%-8s %s\n", name, b)
		}
	}
	return sb.String(), h["status"] == "ok", nil
}

// Replay streams a durable episode. It returns the exit code the caller should
// use: 0 for a verified intact chain, 3 for a broken one and 4 for a chain that
// could not be verified, so "nobody looked" is never confused with "all clear".
func (c *Client) Replay(ctx context.Context, episode string, out io.Writer) (int, error) {
	resp, err := c.do(ctx, "GET", "/v1/episodes/"+episode+"/replay", nil, nil)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1, apiError(resp)
	}
	code := 0
	err = readSSE(resp.Body, func(data string) bool {
		if data == "[DONE]" {
			return false
		}
		var f map[string]any
		if json.Unmarshal([]byte(data), &f) != nil {
			return true
		}
		if f["type"] == "replay_meta" {
			valid, verified := f["chain_valid"] == true, f["chain_verified"] == true
			switch {
			case !verified:
				code = 4
			case !valid:
				code = 3
			}
			fmt.Fprintf(out, "episode %v: %v records, chain valid=%v verified=%v\n", f["episode_id"], f["records"], valid, verified)
			return true
		}
		fmt.Fprintf(out, "  %-14v %s\n", f["type"], compact(f))
		return true
	})
	return code, err
}

func compact(f map[string]any) string {
	rest := map[string]any{}
	for k, v := range f {
		if k != "type" {
			rest[k] = v
		}
	}
	b, _ := json.Marshal(rest)
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// Session is an interactive chat loop.
type Session struct {
	C       *Client
	ID      string
	Verbose bool
	In      io.Reader
	Out     io.Writer
}

// Loop reads lines until EOF or /exit. After an approval request the next line is
// sent as the answer; the server treats only a recognised approval phrase as a yes.
func (s *Session) Loop(ctx context.Context) error {
	in := bufio.NewScanner(s.In)
	pending := false
	for {
		if pending {
			fmt.Fprint(s.Out, "approve? (yes / anything else cancels) > ")
		} else {
			fmt.Fprint(s.Out, "> ")
		}
		if !in.Scan() {
			return in.Err()
		}
		line := strings.TrimSpace(in.Text())
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		case "/new":
			s.ID = fmt.Sprintf("cli-%d", time.Now().UnixNano())
			pending = false
			fmt.Fprintln(s.Out, "new session", s.ID)
			continue
		}
		turn, err := s.C.Send(ctx, s.ID, line, false, s.Verbose, s.Out)
		if err != nil {
			fmt.Fprintln(s.Out, "error:", err)
			continue
		}
		pending = turn.Approval != nil
		if turn.Error != "" {
			fmt.Fprintln(s.Out, "error:", turn.Error)
		}
	}
}

// Digest fetches the morning digest as markdown.
func (c *Client) Digest(ctx context.Context, hours float64) (string, error) {
	resp, err := c.do(ctx, "GET", fmt.Sprintf("/v1/digest?format=markdown&hours=%g", hours), nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}
	var out struct {
		Markdown string `json:"markdown"`
	}
	return out.Markdown, json.NewDecoder(resp.Body).Decode(&out)
}

// Postmortem fetches an episode's postmortem as markdown, with the exit code the
// caller should use: 3 for a broken chain and 4 for one that was not verified,
// the same convention as replay.
func (c *Client) Postmortem(ctx context.Context, episode string) (string, int, error) {
	resp, err := c.do(ctx, "GET", "/v1/episodes/"+episode+"/postmortem?format=markdown", nil, nil)
	if err != nil {
		return "", 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 1, apiError(resp)
	}
	var out struct {
		Markdown      string `json:"markdown"`
		ChainValid    bool   `json:"chain_valid"`
		ChainVerified bool   `json:"chain_verified"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 1, err
	}
	code := 0
	switch {
	case !out.ChainVerified:
		code = 4
	case !out.ChainValid:
		code = 3
	}
	return out.Markdown, code, nil
}

// Raw sends a request and returns the status and body, for the detector commands
// whose answers are printed as they come.
func (c *Client) Raw(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	resp, err := c.do(ctx, method, path, body, nil)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}
