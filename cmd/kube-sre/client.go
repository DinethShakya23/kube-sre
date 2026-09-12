package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/client"
)

func clientFlags(name string) (*flag.FlagSet, *string, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	server := fs.String("server", envOr("KUBESRE_URL", "http://localhost:8000"), "server URL")
	key := fs.String("key", os.Getenv("KUBESRE_API_KEY"), "API key")
	user := fs.String("user", "default", "user name")
	return fs, server, key, user
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func chat(args []string) int {
	fs, server, key, user := clientFlags("chat")
	one := fs.String("q", "", "send one message and exit")
	session := fs.String("session", fmt.Sprintf("cli-%d", time.Now().UnixNano()), "session id")
	verbose := fs.Bool("v", false, "show side events")
	auto := fs.Bool("auto-approve", false, "skip approval for the one message (with -q)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := client.New(*server, *key, *user)
	if *one != "" {
		turn, err := c.Send(ctx, *session, *one, *auto, *verbose, os.Stdout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if turn.Approval != nil {
			fmt.Fprintf(os.Stderr, "\nwaiting for approval; reply in the same session: kube-sre chat --session %s -q yes\n", *session)
		}
		return 0
	}
	s := &client.Session{C: c, ID: *session, Verbose: *verbose, In: os.Stdin, Out: os.Stdout}
	if err := s.Loop(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func status(args []string) int {
	fs, server, key, user := clientFlags("status")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	text, ok, err := client.New(*server, *key, *user).Status(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Print(text)
	if !ok {
		return 1
	}
	return 0
}

func replay(args []string) int {
	fs, server, key, user := clientFlags("replay")
	pos, perr := parseMixed(fs, args)
	if perr != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: kube-sre replay [flags] EPISODE_ID")
		return 2
	}
	code, err := client.New(*server, *key, *user).Replay(context.Background(), pos[0], os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if code == 0 {
			code = 1
		}
	}
	switch code {
	case 3:
		fmt.Fprintln(os.Stderr, "chain BROKEN: the records do not match their hashes or their anchor")
	case 4:
		fmt.Fprintln(os.Stderr, "chain NOT VERIFIED: nothing contradicted the records, but the check could not be completed")
	}
	return code
}

func digestCmd(args []string) int {
	fs, server, key, user := clientFlags("digest")
	hours := fs.Float64("hours", 24, "window in hours (up to 168)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	md, err := client.New(*server, *key, *user).Digest(context.Background(), *hours)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(md)
	return 0
}

func postmortemCmd(args []string) int {
	fs, server, key, user := clientFlags("postmortem")
	pos, perr := parseMixed(fs, args)
	if perr != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: kube-sre postmortem [flags] EPISODE_ID")
		return 2
	}
	md, code, err := client.New(*server, *key, *user).Postmortem(context.Background(), pos[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(md)
	return code
}

// detectorCmd manages the detector queue: list, new, promote, demote, shadow.
func detectorCmd(args []string) int {
	fs, server, key, user := clientFlags("detector")
	status := fs.String("status", "", "list: only this status (candidate, shadow, active, demoted)")
	name := fs.String("name", "", "new: name for the detector")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kube-sre detector list|new|promote|demote|shadow [flags] [DESCRIPTION|NAME]")
		return 2
	}
	sub := args[0]
	posArgs, perr := parseMixed(fs, args[1:])
	if perr != nil {
		return 2
	}
	c := client.New(*server, *key, *user)
	var method, path string
	var body []byte
	switch sub {
	case "list":
		method, path = "GET", "/v1/detectors"
		if *status != "" {
			path += "?status=" + *status
		}
	case "new":
		if len(posArgs) < 1 {
			fmt.Fprintln(os.Stderr, "usage: kube-sre detector new [--name N] \"description of the failure\"")
			return 2
		}
		method, path = "POST", "/v1/detectors"
		body, _ = json.Marshal(map[string]string{"description": strings.Join(posArgs, " "), "name": *name})
	case "promote", "demote", "shadow":
		if len(posArgs) != 1 {
			fmt.Fprintf(os.Stderr, "usage: kube-sre detector %s NAME\n", sub)
			return 2
		}
		method, path = "POST", "/v1/detectors/"+posArgs[0]+"/"+sub
		if sub == "shadow" {
			method, path = "GET", "/v1/detectors/"+posArgs[0]+"/shadow-findings"
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown detector command %q\n", sub)
		return 2
	}
	code, out, err := c.Raw(context.Background(), method, path, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		out = pretty.Bytes()
	}
	fmt.Println(string(out))
	if code >= 400 {
		return 1
	}
	return 0
}
