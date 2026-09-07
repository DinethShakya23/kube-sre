package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: kube-sre replay [flags] EPISODE_ID")
		return 2
	}
	code, err := client.New(*server, *key, *user).Replay(context.Background(), fs.Arg(0), os.Stdout)
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
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: kube-sre postmortem [flags] EPISODE_ID")
		return 2
	}
	md, code, err := client.New(*server, *key, *user).Postmortem(context.Background(), fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(md)
	return code
}
