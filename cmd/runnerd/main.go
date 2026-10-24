package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"codex-pool-proxy/internal/runner"
)

type stringsFlag []string

func (s *stringsFlag) String() string         { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(value string) error { *s = append(*s, value); return nil }

func main() {
	log.SetFlags(log.LstdFlags)
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: runnerd enroll|rotate|run (use --help for flags)")
	}
	defaultState, err := runner.DefaultStatePath()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch os.Args[1] {
	case "enroll", "rotate":
		flags := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
		state := flags.String("state", defaultState, "runner credential file")
		origin := flags.String("url", "", "HTTPS pool origin")
		nonce := flags.String("nonce", "", "single-use enrollment nonce")
		name := flags.String("name", "", "runner name")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected enrollment arguments")
		}
		unlock, err := runner.LockState(*state)
		if err != nil {
			return err
		}
		defer unlock()
		rotating := os.Args[1] == "rotate"
		var previous runner.Config
		if rotating {
			previous, err = runner.ReadConfig(*state)
			if err != nil {
				return err
			}
			if *origin != "" {
				return errors.New("rotation uses the pool URL in the enrolled state")
			}
			*origin = previous.PoolURL
		} else if _, err := os.Stat(*state); err == nil {
			return errors.New("runner state already exists; revoke the old runner before replacing its credential")
		}
		if *name == "" {
			return errors.New("--name is required")
		}
		config, err := runner.Enroll(ctx, nil, *origin, *nonce, *name)
		if err != nil {
			return err
		}
		if rotating && config.RunnerID != previous.RunnerID {
			return errors.New("rotation nonce belongs to another runner; revoke the issued credential")
		}
		if err := runner.WriteConfig(*state, config); err != nil {
			return errors.New("enrollment succeeded but saving the runner credential failed; revoke this runner and enroll again")
		}
		fmt.Printf("Enrolled runner %s\n", config.RunnerID)
		return nil
	case "run":
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		state := flags.String("state", defaultState, "runner credential file")
		root := flags.String("workspaces", "", "absolute workspace root")
		codex := flags.String("codex", "", "absolute native Codex executable")
		claude := flags.String("claude", "", "absolute native Claude executable")
		var paths, labels stringsFlag
		flags.Var(&paths, "allow-path", "allowed existing workspace directory (repeatable)")
		flags.Var(&labels, "label", "runner label (repeatable)")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected runner arguments")
		}
		unlock, err := runner.LockState(*state)
		if err != nil {
			return err
		}
		defer unlock()
		config, err := runner.ReadConfig(*state)
		if err != nil {
			return err
		}
		if *root == "" {
			*root = filepath.Join(filepath.Dir(*state), "workspaces")
		}
		executables := map[string]string{}
		if *codex != "" {
			executables["codex"] = *codex
		}
		if *claude != "" {
			executables["claude"] = *claude
		}
		return runner.Run(ctx, runner.Options{Config: config, WorkspaceRoot: *root, AllowedPaths: paths, Labels: labels, Executables: executables})
	default:
		return errors.New("unknown runnerd command")
	}
}
