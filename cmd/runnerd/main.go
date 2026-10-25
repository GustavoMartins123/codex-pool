package main

import (
	"codex-pool-proxy/internal/runnerproto"
	"context"
	"errors"
	"flag"
	"fmt"
	qrcode "github.com/skip2/go-qrcode"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

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
		return errors.New("usage: runnerd pair|rotate|run (use --help for flags)")
	}
	defaultState, err := runner.DefaultStatePath()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch os.Args[1] {
	case "pair", "rotate":
		flags := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
		state := flags.String("state", defaultState, "runner credential file")
		launch := flags.String("config", "", "runner launch JSON file (recommended)")
		origin := flags.String("url", "", "HTTPS pool origin")
		qr := flags.String("qr", "", "also save the pairing QR as a PNG")
		name := flags.String("name", "", "runner name")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected enrollment arguments")
		}
		if *launch != "" {
			return runConfiguredEnrollment(ctx, os.Args[1], *launch, *qr)
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
		config, err := pairMachine(ctx, *origin, *name, previous.Token, *qr)
		if err != nil {
			return err
		}
		if rotating && config.RunnerID != previous.RunnerID {
			return errors.New("rotation changed runner identity; revoke the issued credential")
		}
		if err := runner.WriteConfig(*state, config); err != nil {
			return errors.New("enrollment succeeded but saving the runner credential failed; revoke this runner and enroll again")
		}
		fmt.Printf("Paired runner %s\n", config.RunnerID)
		return nil
	case "run":
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		state := flags.String("state", defaultState, "runner credential file")
		launch := flags.String("config", "", "runner launch JSON file (recommended)")
		origin := flags.String("url", "", "HTTPS pool origin for first pairing")
		name := flags.String("name", "", "machine name for first pairing")
		root := flags.String("workspaces", "", "absolute workspace root")
		codex := flags.String("codex", "", "absolute native Codex executable")
		claude := flags.String("claude", "", "absolute native Claude executable")
		allowAuto := flags.Bool("allow-auto", false, "accept auto permission sessions (no approvals, no sandbox)")
		var paths, labels stringsFlag
		flags.Var(&paths, "allow-path", "allowed existing workspace directory (repeatable)")
		flags.Var(&labels, "label", "runner label (repeatable)")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected runner arguments")
		}
		if *launch != "" {
			return runConfiguredRunner(ctx, *launch)
		}
		unlock, err := runner.LockState(*state)
		if err != nil {
			return err
		}
		defer unlock()
		var config runner.Config
		if *origin != "" {
			if _, err := os.Stat(*state); !errors.Is(err, os.ErrNotExist) {
				return errors.New("first pairing requires a new state file; use rotate for an existing runner")
			}
			if *name == "" {
				return errors.New("--name is required for first pairing")
			}
			config, err = pairMachine(ctx, *origin, *name, "", "")
			if err != nil {
				return err
			}
			if err := runner.WriteConfig(*state, config); err != nil {
				return errors.New("saving paired runner failed; revoke this runner before pairing again")
			}
		} else {
			if *name != "" {
				return errors.New("--name requires --url for first pairing")
			}
			config, err = runner.ReadConfig(*state)
			if err != nil {
				return err
			}
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
		return runner.Run(ctx, runner.Options{Config: config, WorkspaceRoot: *root, AllowedPaths: paths, Labels: labels, Executables: executables, AllowAutoPermissions: *allowAuto})
	default:
		return errors.New("unknown runnerd command")
	}
}

func runConfiguredEnrollment(ctx context.Context, command, path, qrPath string) error {
	launch, err := runner.ReadLaunchConfig(path)
	if err != nil {
		return err
	}
	if err := runner.ValidateOrigin(launch.PoolURL); err != nil {
		return err
	}
	unlock, err := runner.LockState(launch.State)
	if err != nil {
		return err
	}
	defer unlock()
	rotating := command == "rotate"
	var previous runner.Config
	if rotating {
		previous, err = runner.ReadConfig(launch.State)
		if err != nil {
			return err
		}
		if previous.PoolURL != launch.PoolURL {
			return errors.New("rotation pool URL differs from enrolled state")
		}
	} else if _, err := os.Stat(launch.State); err == nil {
		return errors.New("runner state already exists; revoke the old runner before replacing its credential")
	}
	config, err := pairMachine(ctx, launch.PoolURL, launch.Name, previous.Token, qrPathOrConfig(qrPath, launch.QR))
	if err != nil {
		return err
	}
	if rotating && config.RunnerID != previous.RunnerID {
		return errors.New("rotation changed runner identity; revoke the issued credential")
	}
	if err := runner.WriteConfig(launch.State, config); err != nil {
		return errors.New("enrollment succeeded but saving the runner credential failed; revoke this runner and enroll again")
	}
	fmt.Printf("Paired runner %s\n", config.RunnerID)
	return nil
}

func qrPathOrConfig(cli, configured string) string {
	if cli != "" {
		return cli
	}
	return configured
}

func runConfiguredRunner(ctx context.Context, path string) error {
	launch, err := runner.ReadLaunchConfig(path)
	if err != nil {
		return err
	}
	unlock, err := runner.LockState(launch.State)
	if err != nil {
		return err
	}
	defer unlock()
	config, err := runner.ReadConfig(launch.State)
	if err != nil {
		return err
	}
	if config.PoolURL != launch.PoolURL {
		return errors.New("launch config pool URL differs from enrolled state")
	}
	executables := map[string]string{}
	if launch.Codex != "" {
		executables["codex"] = launch.Codex
	}
	if launch.Claude != "" {
		executables["claude"] = launch.Claude
	}
	return runner.Run(ctx, runner.Options{Config: config, WorkspaceRoot: launch.Workspaces, AllowedPaths: launch.AllowPaths, Labels: launch.Labels, Executables: executables, AllowAutoPermissions: launch.AllowAuto})
}

func pairMachine(ctx context.Context, origin, name, rotationToken, qrPath string) (runner.Config, error) {
	p, err := runner.BeginPairing(ctx, nil, origin, runnerproto.PairingRequest{Name: name, OS: runtime.GOOS, Arch: runtime.GOARCH}, rotationToken)
	if err != nil {
		return runner.Config{}, err
	}
	commandKey, err := runner.NewCommandKey()
	if err != nil {
		return runner.Config{}, err
	}
	link, err := runner.PairingLink(origin, p.ID, commandKey)
	if err != nil {
		return runner.Config{}, err
	}
	qr, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return runner.Config{}, err
	}
	fmt.Println(qr.ToSmallString(false))
	fmt.Printf("Scan in the codex-pool app ? Runners ? Connect runner.\n%s\nMachine code: %s\nExpires: %s\nOnly approve a request you initiated. Waiting for confirmation?\n", link, p.UserCode, p.ExpiresAt.Local().Format(time.RFC3339))
	if qrPath != "" {
		if err := qr.WriteFile(512, qrPath); err != nil {
			return runner.Config{}, err
		}
		// The image holds the command key; it must not outlive the pairing.
		defer os.Remove(qrPath)
	}
	config, err := runner.AwaitPairing(ctx, nil, origin, *p)
	if err != nil {
		return runner.Config{}, err
	}
	config.CommandKey = commandKey
	return config, nil
}
