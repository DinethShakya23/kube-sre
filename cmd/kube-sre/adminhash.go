package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/api"
)

// adminHashCmd hashes a password for ADMIN_PASSWORD_HASH. It reads from the
// terminal, asking twice, or from stdin: one line, or two that must match.
func adminHashCmd(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: kube-sre admin-hash   (reads the password from the terminal or stdin)")
		return 2
	}
	tty := isTerminal(os.Stdin)
	in := bufio.NewReader(os.Stdin)
	read := func(prompt string) (string, bool) {
		if tty {
			fmt.Fprint(os.Stderr, prompt)
			echo(false)
			defer func() { echo(true); fmt.Fprintln(os.Stderr) }()
		}
		line, err := in.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		return line, err == nil || line != ""
	}
	pw, ok := read("Password: ")
	if !ok || pw == "" {
		fmt.Fprintln(os.Stderr, "error: no password given")
		return 1
	}
	if again, ok := read("Again: "); ok || tty {
		if again != pw {
			fmt.Fprintln(os.Stderr, "error: the two passwords differ")
			return 1
		}
	}
	h, err := api.HashPassword(pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Put this in ~/.kube-sre/.env, with ADMIN_USERNAME:\n  ADMIN_PASSWORD_HASH=<the line below>")
	fmt.Println(h)
	return 0
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// echo turns terminal echo on or off where stty exists; elsewhere the password shows.
func echo(on bool) {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}
