package experience

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Run provides a host-independent CLI. JSON results go to stdout; diagnostics to stderr.
func Run(args []string, in io.Reader, out, errOut io.Writer) int {
	root := ""
	remaining := []string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--root" {
			if i+1 == len(args) {
				fmt.Fprintln(errOut, "--root requires a directory")
				return 2
			}
			i++
			root = args[i]
		} else if strings.HasPrefix(args[i], "--root=") {
			root = strings.TrimPrefix(args[i], "--root=")
		} else {
			remaining = append(remaining, args[i])
		}
	}
	if len(remaining) == 0 || remaining[0] == "help" || remaining[0] == "--help" {
		fmt.Fprintln(out, "usage: experience --root DIRECTORY <list|propose|approve|revise|retire|search|get|history|export|serve|mcp>\nlist/export --project PROJECT\npropose FILE.json\nrevise ID FILE.json\napprove/retire/get/history ID\nsearch --project PROJECT [--query TEXT] [--modules comma,separated] [--limit 10]\nserve: line-delimited pigo JSON-RPC on stdio (propose/search/get/list tools)\nmcp: standard MCP stdio server (protocol 2025-11-25; no approval tools)")
		return 0
	}
	if strings.TrimSpace(root) == "" {
		fmt.Fprintln(errOut, "--root is required")
		return 2
	}
	store := Store{Root: root}
	command := remaining[0]
	rest := remaining[1:]
	log := func(status string) {
		fmt.Fprintf(errOut, "%s phase=experience operation=%s result=%s\n", time.Now().UTC().Format(time.RFC3339), command, status)
	}
	if command == "serve" || command == "mcp" {
		if len(rest) > 0 {
			fmt.Fprintf(errOut, "%s takes no positional arguments\n", command)
			return 2
		}
		serve := Serve
		if command == "mcp" {
			serve = ServeMCP
		}
		if err := serve(store, in, out, errOut); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		return 0
	}
	log("started")
	var result any
	var err error
	switch command {
	case "list", "export", "search":
		flags := flag.NewFlagSet(command, flag.ContinueOnError)
		flags.SetOutput(errOut)
		project := flags.String("project", "", "project scope (required)")
		query := flags.String("query", "", "query text")
		modules := flags.String("modules", "", "comma-separated modules")
		limit := flags.Int("limit", 10, "maximum matches")
		if flags.Parse(rest) != nil {
			return 2
		}
		if flags.NArg() != 0 {
			fmt.Fprintln(errOut, "unexpected positional arguments")
			return 2
		}
		if command == "search" {
			result, err = store.Search(*project, *query, strings.Split(*modules, ","), *limit)
		} else {
			result, err = store.List(*project)
		}
	case "propose":
		if len(rest) != 1 {
			fmt.Fprintln(errOut, "propose requires a JSON file (or - for stdin)")
			return 2
		}
		var e Entry
		e, err = readEntry(rest[0], in)
		if err == nil {
			result, err = store.Propose(e)
		}
	case "revise":
		if len(rest) != 2 {
			fmt.Fprintln(errOut, "revise requires ID and a complete JSON entry file")
			return 2
		}
		var e Entry
		e, err = readEntry(rest[1], in)
		if err == nil {
			result, err = store.Revise(rest[0], e)
		}
	case "approve", "retire", "get", "history":
		if len(rest) != 1 {
			fmt.Fprintf(errOut, "%s requires an ID\n", command)
			return 2
		}
		switch command {
		case "approve":
			result, err = store.Approve(rest[0])
		case "retire":
			result, err = store.Retire(rest[0])
		case "get":
			result, err = store.Get(rest[0])
		case "history":
			result, err = store.History(rest[0])
		}
	default:
		fmt.Fprintf(errOut, "unknown experience command %q\n", command)
		return 2
	}
	if err != nil {
		log("failed")
		fmt.Fprintln(errOut, err)
		return 1
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err = enc.Encode(result); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	log("completed")
	return 0
}
func readEntry(path string, in io.Reader) (Entry, error) {
	var e Entry
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return e, err
		}
		defer f.Close()
		in = f
	}
	d := json.NewDecoder(io.LimitReader(in, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return e, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return e, fmt.Errorf("expected exactly one JSON entry")
	}
	return e, nil
}
