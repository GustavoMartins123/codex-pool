package main

import (
	"bufio"
	"codex-pool-proxy/internal/runnerproto"
	"context"
	"errors"
	"flag"
	"fmt"
	qrcode "github.com/skip2/go-qrcode"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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
		// A double-clicked console window would close before the error is read.
		if runtime.GOOS == "windows" && len(os.Args) < 2 && !errors.Is(err, context.Canceled) {
			fmt.Fprint(os.Stderr, "Press Enter to close.")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
		os.Exit(1)
	}
}
func run() error {
	defaultState, err := runner.DefaultStatePath()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) < 2 {
		return start(ctx, defaultState)
	}
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
	case "install":
		flags := flag.NewFlagSet("install", flag.ContinueOnError)
		state := flags.String("state", defaultState, "runner credential file")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || flags.Arg(0) != "codex" {
			return errors.New("usage: runnerd install codex")
		}
		// Naming the command is the consent: install even if Claude Code exists.
		return installCodex(ctx, &runner.AgentInstaller{Root: runner.ManagedAgentsDir(*state)})
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

// start is runnerd without arguments: pair from the app on first use, finish
// an interrupted pairing, or connect with the saved state.
func start(ctx context.Context, state string) error {
	unlock, err := runner.LockState(state)
	if err != nil {
		return errors.New("another runnerd is already using this machine's runner state")
	}
	defer unlock()
	config, err := runner.ReadConfig(state)
	if _, statErr := os.Stat(state); errors.Is(statErr, os.ErrNotExist) {
		fmt.Println("This machine is not paired yet.")
		config, err = runner.Onboard(ctx, runner.OnboardingOptions{StatePath: state, Report: showOnboarding})
		if err != nil {
			return err
		}
	} else if err != nil {
		// Never overwrite a state that might hold the only copy of a credential.
		return fmt.Errorf("%v in %s; fix or move that file, revoke the old runner in the app, and start again", err, state)
	}
	executables, err := agents(ctx, state)
	if err != nil {
		return err
	}
	fmt.Printf("Connecting runner %s to %s.\n", config.RunnerID, config.PoolURL)
	return runner.Run(ctx, runner.Options{Config: config, WorkspaceRoot: filepath.Join(filepath.Dir(state), "workspaces"), Executables: executables})
}

// agents finds Codex and Claude Code. With none, it offers the pinned Codex
// build and installs it only after the person at this machine agrees.
func agents(ctx context.Context, state string) (map[string]string, error) {
	lookup := runner.AgentLookup{ManagedRoot: runner.ManagedAgentsDir(state)}
	found, tried := lookup.Resolve(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(found) > 0 {
		return found, nil
	}
	for _, c := range tried {
		where := c.Path
		if where == "" {
			where = c.Source
		}
		fmt.Printf("  %s (%s): %v\n", c.Agent, where, c.Err)
	}
	installer := &runner.AgentInstaller{Root: runner.ManagedAgentsDir(state)}
	release, ok := installer.Release("codex")
	unavailable := errors.New("paired, but no agent is available: install Codex or Claude Code, run `runnerd install codex`, or point `runnerd run --config` at them")
	if !ok {
		return nil, unavailable
	}
	if info, err := os.Stdin.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return nil, unavailable
	}
	fmt.Printf("No agent found. Install Codex %s for this runner?\n  OpenAI's native build from registry.npmjs.org, checked against a pinned SHA-512\n  About %d MB in %s; PATH and your Codex settings are not changed.\nInstall? [y/N] ",
		release.Version, release.UnpackedSize>>20, runner.ManagedAgentsDir(state))
	consent, err := readInstallConsent(ctx, os.Stdin)
	if err != nil {
		return nil, err
	}
	if !consent {
		return nil, unavailable
	}
	if err := installCodex(ctx, installer); err != nil {
		return nil, fmt.Errorf("paired, but installing Codex failed: %w", err)
	}
	found, _ = lookup.Resolve(ctx)
	if len(found) == 0 {
		return nil, errors.New("paired, but the installed Codex did not start; remove " + runner.ManagedAgentsDir(state) + " and try again")
	}
	return found, nil
}

func readInstallConsent(ctx context.Context, input io.Reader) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	type answer struct {
		line string
		err  error
	}
	read := make(chan answer, 1)
	go func() {
		line, err := bufio.NewReader(input).ReadString('\n')
		read <- answer{line, err}
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case result := <-read:
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if result.err != nil && result.err != io.EOF {
			return false, result.err
		}
		return strings.EqualFold(strings.TrimSpace(result.line), "y"), nil
	}
}

func installCodex(ctx context.Context, installer *runner.AgentInstaller) error {
	last := int64(-1)
	installer.Progress = func(done, total int64) {
		if total > 0 && done*20/total != last {
			last = done * 20 / total
			fmt.Printf("\rDownloading Codex: %d%%", last*5)
		}
	}
	path, err := installer.Install(ctx, "codex")
	fmt.Println()
	if err == nil {
		fmt.Println("Installed", path)
	}
	return err
}

func showOnboarding(e runner.OnboardingEvent) {
	switch e.Kind {
	case runner.OnboardingRecovering:
		fmt.Printf("Finishing the pairing with %s that was interrupted.\n", e.PoolURL)
	case runner.OnboardingQR:
		qr, err := qrcode.New(e.Link, qrcode.Medium)
		if err != nil {
			fmt.Println("The pairing QR could not be drawn:", err)
			return
		}
		fmt.Println(qr.ToSmallString(false))
		fmt.Printf("Scan in the codex-pool app: Runners > Connect machine. The phone must reach %s.\nThis QR is a secret: do not photograph or share it. Expires %s.\n",
			strings.Join(e.Endpoints, ", "), e.ExpiresAt.Local().Format(time.Kitchen))
	case runner.OnboardingPairing:
		fmt.Printf("Pool %s is waiting for the app to approve %q (code %s).\n", e.PoolURL, e.Name, e.UserCode)
	case runner.OnboardingPaired:
		fmt.Printf("Paired runner %s with %s.\n", e.RunnerID, e.PoolURL)
	case runner.OnboardingRetrying:
		fmt.Printf("Pairing attempt ended: %v. Showing a new QR.\n", e.Err)
	case runner.OnboardingAbandoned:
		fmt.Println(e.Err)
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
