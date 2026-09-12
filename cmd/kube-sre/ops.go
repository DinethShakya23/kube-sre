package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/chainops"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

const opsUsage = `usage:
  kube-sre chain-export CHAIN SCOPE [--through N] [--note T] [-o FILE]   archive a hash chain (decision_log EPISODE, memory_audit CLUSTER)
  kube-sre chain-verify FILE                                              check an archive; needs no database
  kube-sre chain-truncate FILE [--note T]                                 remove exactly the archived rows, after declaring the gap
  kube-sre backup-manifest [--note T] [-o FILE]                           record what the database holds, beside your dump
  kube-sre backup-verify FILE                                             check a restored database against a manifest
exit codes: 0 ok, 1 error, 3 verification failed`

func openDB() (*store.DB, int) {
	cfg := load()
	db, err := store.Open(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database error:", err)
		return nil, 1
	}
	return db, 0
}

func writeJSON(path string, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	if path == "" || path == "-" {
		_, err := os.Stdout.Write(b.Bytes())
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0o600)
}

func readDoc(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return chainops.DecodeDoc(raw)
}

func report(res chainops.Result, what string) int {
	if res.OK {
		fmt.Printf("%s verified (%d checks)\n", what, res.Checked)
		return 0
	}
	fmt.Fprintf(os.Stderr, "%s FAILED verification:\n", what)
	for _, p := range res.Problems {
		fmt.Fprintln(os.Stderr, "  -", p)
	}
	return 3
}

func chainExport(args []string) int {
	fs := flag.NewFlagSet("chain-export", flag.ContinueOnError)
	through := fs.Int64("through", -1, "archive through this seq, inclusive")
	note := fs.String("note", "", "note recorded in the archive")
	out := fs.String("o", "-", "output file")
	pos, err := parseMixed(fs, args)
	if err != nil || len(pos) != 2 {
		fmt.Fprintln(os.Stderr, opsUsage)
		return 2
	}
	db, code := openDB()
	if db == nil {
		return code
	}
	defer db.Close()
	var bound *int64
	if *through >= 0 {
		bound = through
	}
	doc, err := chainops.BuildExport(context.Background(), db, pos[0], pos[1], time.Now().UTC().Format(time.RFC3339), bound, *note)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := writeJSON(*out, doc); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if *out != "-" {
		fmt.Fprintf(os.Stderr, "archived %v row(s) to %s\n%s\n", doc["row_count"], *out, chainops.ArchiveLimit)
	}
	return 0
}

func chainVerify(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, opsUsage)
		return 2
	}
	doc, err := readDoc(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return report(chainops.VerifyExport(doc), "archive")
}

func chainTruncate(args []string) int {
	fs := flag.NewFlagSet("chain-truncate", flag.ContinueOnError)
	note := fs.String("note", "", "why the rows are being removed")
	pos, err := parseMixed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, opsUsage)
		fmt.Fprintln(os.Stderr, "\nbefore removing anything:")
		for _, p := range chainops.TruncationPrerequisites {
			fmt.Fprintln(os.Stderr, "  -", p)
		}
		return 2
	}
	doc, err := readDoc(pos[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	db, code := openDB()
	if db == nil {
		return code
	}
	defer db.Close()
	tr, err := chainops.TruncateChain(context.Background(), db, doc, *note)
	var refused *chainops.ErrRefused
	switch {
	case errors.As(err, &refused):
		fmt.Fprintln(os.Stderr, "refused, nothing was changed:", strings.TrimSpace(err.Error()))
		return 3
	case err != nil:
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("removed %d row(s) of %s/%s through seq %d; the gap is declared (resume at seq %d)\nkeep the archive somewhere the database's operators cannot replace it.\n",
		tr.RowsRemoved, tr.Chain, tr.ScopeID, tr.ThroughSeq, tr.ResumeSeq)
	return 0
}

func backupManifest(args []string) int {
	fs := flag.NewFlagSet("backup-manifest", flag.ContinueOnError)
	note := fs.String("note", "", "note recorded in the manifest")
	out := fs.String("o", "-", "output file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	db, code := openDB()
	if db == nil {
		return code
	}
	defer db.Close()
	m, err := chainops.BuildManifest(context.Background(), db, time.Now().UTC().Format(time.RFC3339), *note)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := writeJSON(*out, m); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func backupVerify(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, opsUsage)
		return 2
	}
	m, err := readDoc(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	db, code := openDB()
	if db == nil {
		return code
	}
	defer db.Close()
	return report(chainops.VerifyManifest(context.Background(), db, m), "restore")
}

// parseMixed parses flags that may come before, between or after the positional
// arguments, which the standard flag package does not: it stops at the first one.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
