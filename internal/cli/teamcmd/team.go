// Package teamcmd provides the local frontend/backend coordination CLI.
package teamcmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/experience"
	"github.com/smallnest/pigo/internal/team"
	"github.com/spf13/pflag"
)

const usage = `usage: pigo team <run|resume|status|contract|experience> [options]
  run        --spec FILE --project DIR --model MODEL [--run-dir DIR]
  resume     --run-dir DIR --model MODEL
  status     --run-dir DIR
  contract   --run-dir DIR --file CONTRACT.json
  experience --root DIR <list|propose|approve|revise|retire|search|get|history|export|serve|mcp>

Run/resume model options: --base-url URL --protocol PROTOCOL --provider PROVIDER
                         --api-key-env ENV_NAME --thinking LEVEL
Run artifacts default to PROJECT/.pigo/team/runs/TIMESTAMP-RANDOM.
Stdout contains a JSON result. Stage logs are written to stderr and events.jsonl.
`

// Run returns 0 on success/help, 2 for argument errors, and 1 for execution errors.
func Run(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(out, usage)
		return 0
	}
	command := args[0]
	if command == "experience" {
		return experience.Run(args[1:], os.Stdin, out, errOut)
	}
	if command != "run" && command != "resume" && command != "status" && command != "contract" {
		fmt.Fprintf(errOut, "unknown team command %q\n%s", command, usage)
		return 2
	}
	flags := pflag.NewFlagSet("team "+command, pflag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.Usage = func() { fmt.Fprint(errOut, usage); flags.PrintDefaults() }
	runDir := flags.String("run-dir", "", "directory containing run state and artifacts")
	help := flags.BoolP("help", "h", false, "show help")
	var specFile, project, contractFile, keyEnv string
	worker := &team.RuntimeWorker{}
	if command == "run" {
		flags.StringVar(&specFile, "spec", "", "task specification JSON (required)")
		flags.StringVar(&project, "project", "", "existing Git project directory (required)")
	}
	if command == "contract" {
		flags.StringVar(&contractFile, "file", "", "explicitly accepted next contract JSON (required)")
	}
	if command == "run" || command == "resume" {
		flags.StringVar(&worker.Model, "model", "", "model identifier (required)")
		flags.StringVar(&worker.BaseURL, "base-url", "", "provider endpoint")
		flags.StringVar(&worker.Protocol, "protocol", "", "provider API protocol")
		flags.StringVar(&worker.Provider, "provider", "", "provider name")
		flags.StringVar(&worker.Thinking, "thinking", "", "reasoning level")
		flags.StringVar(&keyEnv, "api-key-env", "", "environment variable containing API key; never saved in run state")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *help {
		fmt.Fprint(out, usage)
		flags.SetOutput(out)
		flags.PrintDefaults()
		return 0
	}
	invalid := func(message string) int { fmt.Fprintln(errOut, message); return 2 }
	failure := func(err error) int { fmt.Fprintln(errOut, err); return 1 }
	if flags.NArg() != 0 {
		return invalid("unexpected positional arguments")
	}
	if command != "run" && strings.TrimSpace(*runDir) == "" {
		return invalid("--run-dir is required")
	}
	if command == "run" && (strings.TrimSpace(specFile) == "" || strings.TrimSpace(project) == "") {
		return invalid("--spec and --project are required")
	}
	if command == "contract" && strings.TrimSpace(contractFile) == "" {
		return invalid("--file is required")
	}
	if (command == "run" || command == "resume") && strings.TrimSpace(worker.Model) == "" {
		return invalid("--model is required")
	}
	if keyEnv != "" {
		worker.APIKey = os.Getenv(keyEnv)
		if worker.APIKey == "" {
			return invalid("the environment variable selected by --api-key-env is unset or empty")
		}
	}
	if command == "status" {
		state, err := team.LoadState(*runDir)
		if err != nil {
			return failure(err)
		}
		return printJSON(out, errOut, state)
	}
	if command == "contract" {
		c, err := loadContract(contractFile)
		if err != nil {
			return invalid(err.Error())
		}
		if err = team.AcceptContract(*runDir, c); err != nil {
			return failure(err)
		}
		state, err := team.LoadState(*runDir)
		if err != nil {
			return failure(err)
		}
		fmt.Fprintf(errOut, "phase=alignment status=accepted contract_version=%d; resume explicitly to continue\n", c.Version)
		return printJSON(out, errOut, state)
	}
	var state *team.State
	var err error
	if command == "run" {
		spec, e := team.LoadSpec(specFile)
		if e != nil {
			return invalid(e.Error())
		}
		project, err = filepath.Abs(project)
		if err != nil {
			return failure(err)
		}
		if *runDir == "" {
			suffix := make([]byte, 4)
			if _, err = rand.Read(suffix); err != nil {
				return failure(err)
			}
			*runDir = filepath.Join(project, ".pigo", "team", "runs", time.Now().UTC().Format("20060102T150405Z")+"-"+hex.EncodeToString(suffix))
		}
		*runDir, err = filepath.Abs(*runDir)
		if err != nil {
			return failure(err)
		}
		fmt.Fprintf(errOut, "%s [setup] preparing isolated workspaces; run directory: %s\n", time.Now().UTC().Format(time.RFC3339), *runDir)
		state, err = team.Create(ctx, project, *runDir, spec)
	} else {
		state, err = team.LoadState(*runDir)
	}
	if err != nil {
		return failure(err)
	}
	logger, err := team.NewLogger(filepath.Join(*runDir, "events.jsonl"), state.ID, errOut)
	if err != nil {
		return failure(err)
	}
	logger.Emit("cli", "", command, state.Spec.Contract.Version, state.Attempt, "run directory: "+*runDir)
	memory := &experience.Store{Root: filepath.Join(state.ProjectDir, ".pigo", "team", "experience")}
	state, runErr := team.Execute(ctx, *runDir, worker, logger, memory)
	closeErr := logger.Close()
	code := 0
	if state != nil {
		code = printJSON(out, errOut, state)
	}
	if runErr != nil {
		return failure(runErr)
	}
	if closeErr != nil {
		return failure(closeErr)
	}
	return code
}

func printJSON(out, errOut io.Writer, value any) int {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}

func loadContract(file string) (team.Contract, error) {
	f, err := os.Open(file)
	if err != nil {
		return team.Contract{}, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 2<<20))
	dec.DisallowUnknownFields()
	var c team.Contract
	if err = dec.Decode(&c); err != nil {
		return c, err
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("contract must contain exactly one JSON object")
	}
	return c, c.Validate()
}
