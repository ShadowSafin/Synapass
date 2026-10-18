package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/bootstrap"
	"github.com/shadowsafin/synapass/internal/config"
	nativecfg "github.com/shadowsafin/synapass/internal/config/native"
	"github.com/shadowsafin/synapass/internal/dashboardauth"
	"github.com/shadowsafin/synapass/internal/logging"
	"github.com/shadowsafin/synapass/internal/runtime"
	"github.com/shadowsafin/synapass/internal/storage"
)

// nativeOptions holds the flags shared by every native subcommand.
type nativeOptions struct {
	root               string
	envFile            string
	withWorkers        bool
	skipBuild          bool
	skipDashboardBuild bool
	skipMigrate        bool
	skipDashboard      bool
	consoleLogs        bool
	dashboardPort      int
	dashboardHost      string
	readinessTimeout   time.Duration
}

// addNativeFlags registers the shared flags on fs.
func addNativeFlags(fs *flag.FlagSet, opts *nativeOptions) {
	fs.StringVar(&opts.root, "root", "", "installation root holding config.yaml, native.env, data/ and logs/ (NATIVE_ROOT)")
	fs.StringVar(&opts.envFile, "env-file", "", "env file to load before anything else (defaults to <root>/native.env)")
	fs.BoolVar(&opts.withWorkers, "with-workers", false, "include the Python intelligence workers (needs a venv, see NATIVE_WORKER_VENV)")
	fs.BoolVar(&opts.skipBuild, "skip-build", false, "do not build the gateway binary")
	fs.BoolVar(&opts.skipDashboardBuild, "skip-dashboard-build", false, "do not run npm ci/build for the dashboard")
	fs.BoolVar(&opts.skipMigrate, "skip-migrate", false, "do not apply database migrations")
	fs.BoolVar(&opts.skipDashboard, "skip-dashboard", false, "gateway only: do not start or check the dashboard")
	fs.BoolVar(&opts.consoleLogs, "console-logs", false, "stream child output to the terminal instead of log files")
	fs.IntVar(&opts.dashboardPort, "dashboard-port", 0, "dashboard listen port (default 3000, PORT wins)")
	fs.StringVar(&opts.dashboardHost, "dashboard-host", "", "dashboard bind address (default 127.0.0.1: this machine only; use 0.0.0.0 for LAN)")
	fs.DurationVar(&opts.readinessTimeout, "readiness-timeout", 90*time.Second, "how long to wait for services to answer")
}

// nativeContext carries everything a native subcommand needs: the resolved
// installation, the configuration, and the validation verdict, kept separate
// from load errors so each subcommand can decide what an invalid configuration
// means (report it, fix its files first, or refuse to start).
type nativeContext struct {
	cfg           *config.Config
	logger        *slog.Logger
	paths         nativecfg.Paths
	envPath       string
	validationErr error
}

// nativeSetup applies installation scoping, loads the env file, and resolves
// the gateway configuration without finalizing it. Resolution and parse errors
// are fatal; validation problems ride along in validationErr for the caller.
func nativeSetup(configPath, logLevel, logFormat string, opts *nativeOptions) (*nativeContext, error) {
	if opts.root != "" {
		if err := os.Setenv("NATIVE_ROOT", opts.root); err != nil {
			return nil, &exitError{code: exitFailure, err: err}
		}
	}
	paths := nativecfg.DefaultPaths()

	// The native config file is discovered like any --config/SYNAPASS_CONFIG_FILE
	// value: an explicit choice always wins, otherwise a written native config
	// is picked up automatically so install, up and doctor agree on the file.
	if configPath == "" && os.Getenv("SYNAPASS_CONFIG_FILE") == "" {
		if _, err := os.Stat(paths.ConfigFile); err == nil {
			_ = os.Setenv("SYNAPASS_CONFIG_FILE", paths.ConfigFile)
		}
	}

	envPath := opts.envFile
	if envPath == "" {
		envPath = paths.EnvFile
	}
	if opts.envFile != "" {
		if _, err := nativecfg.ApplyEnvFile(envPath); err != nil {
			return nil, &exitError{code: exitConfig, err: fmt.Errorf("load env file %s: %w", envPath, err)}
		}
	} else if _, err := nativecfg.ApplyEnvFile(envPath); err != nil && !os.IsNotExist(err) {
		return nil, &exitError{code: exitConfig, err: fmt.Errorf("load env file %s: %w", envPath, err)}
	}

	cfg, err := config.LoadUnchecked(configPath, configPath != "")
	if err != nil {
		return nil, &exitError{code: exitConfig, err: err}
	}
	if logLevel != "" {
		cfg.Logging.Level = logLevel
	}
	if logFormat != "" {
		cfg.Logging.Format = logFormat
	}
	logger := logging.New(cfg.Logging, logging.Options{
		Service:     cfg.App.Name,
		Instance:    cfg.App.InstanceID,
		Environment: cfg.App.Environment,
	})
	nctx := &nativeContext{cfg: cfg, logger: logger, paths: paths, envPath: envPath}
	// Finalize materializes derived values (DSN, first NATS URL) and then
	// validates. Materialization is infallible, so the config stays usable for
	// dial targets and display even when validation reports problems.
	nctx.validationErr = cfg.Finalize()
	return nctx, nil
}

// runNative dispatches the native deployment wrapper.
func runNative(cfg *config.Config, logger *slog.Logger, configPath, logLevel, logFormat string, args []string) error {
	if len(args) == 0 {
		return &exitError{code: exitUsage, err: fmt.Errorf("usage: synapass native [install|up|doctor]")}
	}
	switch args[0] {
	case "install":
		return runNativeInstall(cfg, logger, configPath, logLevel, logFormat, args[1:])
	case "up":
		return runNativeUp(cfg, logger, configPath, logLevel, logFormat, args[1:])
	case "doctor":
		return runNativeDoctor(cfg, logger, configPath, logLevel, logFormat, args[1:])
	case "admin":
		return runNativeAdmin(cfg, logger, configPath, logLevel, logFormat, args[1:])
	default:
		return &exitError{code: exitUsage, err: fmt.Errorf("unknown native command %q (want install, up, doctor or admin)", args[0])}
	}
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

// runNativeDoctor validates a native installation without changing anything:
// configuration loads, required binaries exist, datastores answer, and the
// ports the supervisor is about to use are free. Every failure names its fix.
func runNativeDoctor(_ *config.Config, _ *slog.Logger, configPath, logLevel, logFormat string, args []string) error {
	fs := flag.NewFlagSet("native doctor", flag.ContinueOnError)
	var opts nativeOptions
	addNativeFlags(fs, &opts)
	if err := fs.Parse(args); err != nil {
		return &exitError{code: exitUsage, err: err}
	}

	nctx, err := nativeSetup(configPath, logLevel, logFormat, &opts)
	if err != nil {
		return err
	}
	cfg, paths, envPath := nctx.cfg, nctx.paths, nctx.envPath

	fmt.Printf("native root: %s\n", filepath.Dir(paths.ConfigFile))
	fmt.Printf("env file:    %s\n", envPath)
	fmt.Printf("config:      %s\n\n", configSource(cfg))

	type entry struct {
		check    nativecfg.Check
		required bool
	}
	var checks []entry
	add := func(c nativecfg.Check, required bool) { checks = append(checks, entry{c, required}) }

	// Configuration validity is a first-class finding, not a startup crash:
	// doctor exists to diagnose a broken config, so it reports each problem
	// with the same text `serve` would refuse to start with.
	if nctx.validationErr != nil {
		add(nativecfg.Check{
			Name:   "config",
			Target: configSource(cfg),
			Hint:   nctx.validationErr.Error(),
		}, true)
	} else {
		add(nativecfg.Check{
			Name:   "config",
			Target: configSource(cfg),
			OK:     true,
			Detail: "valid",
		}, true)
	}

	pgAddr := postgresAddr(cfg)
	add(nativecfg.TCPCheck("postgres", pgAddr, 5*time.Second,
		"start PostgreSQL 16 and create the role/database from documentation/installation/native.md#datastores"), true)
	add(nativecfg.TCPCheck("redis", redisAddr(cfg), 5*time.Second,
		"start Redis 7 with `appendonly yes`"), true)

	if cfg.ClickHouse.Addr != "" || cfg.ClickHouse.DSN != "" {
		add(nativecfg.TCPCheck("clickhouse", clickhouseAddr(cfg), 5*time.Second,
			"start ClickHouse and create the database, or unset it: inference works without it"), false)
	} else {
		checks = append(checks, entry{nativecfg.Check{Name: "clickhouse", Target: "(not configured)", OK: true, Detail: "skipped, inference works without it"}, false})
	}
	if natsURL := firstNATSURL(cfg); natsURL != "" {
		addr, perr := nativecfg.SplitNATSAddr(natsURL)
		if perr != nil {
			checks = append(checks, entry{nativecfg.Check{Name: "nats", Target: natsURL, Hint: "set a dialable NATS URL (nats://host:4222) or unset it", Detail: perr.Error()}, false})
		} else {
			add(nativecfg.TCPCheck("nats", addr, 5*time.Second,
				"start NATS with JetStream enabled (`nats-server -js`), or unset it: only eval and replay need it"), false)
		}
	} else {
		checks = append(checks, entry{nativecfg.Check{Name: "nats", Target: "(not configured)", OK: true, Detail: "skipped, only eval and replay need it"}, false})
	}

	add(nativecfg.PortFreeCheck("gateway-port", cfg.HTTP.Addr,
		"free the port or set SYNAPASS_HTTP_ADDR to another one"), true)

	if !opts.skipDashboard {
		add(nativecfg.ExecutableCheck("node", "node",
			"install Node.js 20+ (https://nodejs.org) — only needed for the dashboard"), true)
		add(nativecfg.DashboardBuildCheck(paths.DashboardDir), true)
		dashAddr := net.JoinHostPort(dashboardHost(&opts), strconv.Itoa(dashboardPort(&opts)))
		add(nativecfg.PortFreeCheck("dashboard-port", dashAddr,
			"free the port or set --dashboard-port / PORT"), true)
	}
	if opts.withWorkers {
		python, perr := workerPython(paths)
		if perr != nil {
			checks = append(checks, entry{nativecfg.Check{Name: "workers-venv", Target: paths.WorkerVenv, Hint: perr.Error(), Detail: "missing"}, false})
		} else {
			add(nativecfg.ExecutableCheck("workers-python", python,
				"create the venv and `pip install ./workers` (see documentation/installation/native.md)"), true)
		}
	}

	if os.Getenv("SYNAPASS_ADMIN_KEY") == "" {
		fmt.Printf("%-16s %s\n", "admin-key", "WARN: SYNAPASS_ADMIN_KEY is empty (gateway validation may refuse to start in production; generate one with `openssl rand -hex 24`)")
	}

	failedRequired := 0
	for _, e := range checks {
		mark := "ok  "
		if !e.check.OK {
			mark = "warn"
			if e.required {
				mark = "FAIL"
			}
		}
		fmt.Printf("%-5s %-16s %-28s %s\n", mark, e.check.Name, e.check.Target, firstNonEmpty(e.check.Detail, ""))
		if !e.check.OK {
			fmt.Printf("       -> %s\n", e.check.Hint)
			if e.required {
				failedRequired++
			}
		}
	}
	if failedRequired > 0 {
		return &exitError{code: exitFailure, err: fmt.Errorf("doctor: %d required check(s) failed", failedRequired)}
	}
	fmt.Println("doctor: all required checks passed")
	return nil
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

// runNativeInstall prepares a host: writes env and config templates without
// clobbering existing files, builds the gateway and dashboard, and migrates.
// Every step is idempotent, so re-running install after fixing a failure
// resumes rather than starting over.
func runNativeInstall(_ *config.Config, _ *slog.Logger, configPath, logLevel, logFormat string, args []string) error {
	fs := flag.NewFlagSet("native install", flag.ContinueOnError)
	var opts nativeOptions
	addNativeFlags(fs, &opts)
	if err := fs.Parse(args); err != nil {
		return &exitError{code: exitUsage, err: err}
	}

	if opts.root != "" {
		if err := os.Setenv("NATIVE_ROOT", opts.root); err != nil {
			return &exitError{code: exitFailure, err: err}
		}
	}
	paths := nativecfg.DefaultPaths()
	envPath := opts.envFile
	if envPath == "" {
		envPath = paths.EnvFile
	}

	fmt.Printf("[1/5] installation root: %s\n", filepath.Dir(paths.ConfigFile))
	if err := paths.EnsureStateDirs(); err != nil {
		return &exitError{code: exitFailure, err: err}
	}

	fmt.Printf("[2/5] environment file: %s\n", envPath)
	if _, err := os.Stat(envPath); err == nil {
		fmt.Println("      keeping the existing file (delete it to regenerate)")
	} else if !os.IsNotExist(err) {
		return &exitError{code: exitFailure, err: err}
	} else {
		template, err := installTemplate("native.env.example")
		if err != nil {
			return &exitError{code: exitFailure, err: err}
		}
		key, err := randomHex(24)
		if err != nil {
			return &exitError{code: exitFailure, err: err}
		}
		content := strings.Replace(string(template), "SYNAPASS_ADMIN_KEY=\n", "SYNAPASS_ADMIN_KEY="+key+"\n", 1)
		if err := writePrivateFile(envPath, []byte(content)); err != nil {
			return &exitError{code: exitFailure, err: err}
		}
		fmt.Println("      wrote template with a fresh SYNAPASS_ADMIN_KEY (mode 0600)")
	}

	fmt.Printf("[3/5] gateway config: %s\n", paths.ConfigFile)
	if configPath != "" {
		fmt.Printf("      using --config %s instead\n", configPath)
	} else if _, err := os.Stat(paths.ConfigFile); err == nil {
		fmt.Println("      keeping the existing file")
	} else if !os.IsNotExist(err) {
		return &exitError{code: exitFailure, err: err}
	} else {
		template, err := installTemplate("config.yaml")
		if err != nil {
			return &exitError{code: exitFailure, err: err}
		}
		if err := writeConfigFile(paths.ConfigFile, template); err != nil {
			return &exitError{code: exitFailure, err: err}
		}
		fmt.Println("      wrote minimal localhost template (edit it, or point SYNAPASS_CONFIG_FILE at config.example.yaml)")
	}

	// Reload now that the env file exists: the generated admin key and any
	// other template values participate from here on, so the build, migrate
	// and summary steps see exactly what `native up` will see.
	nctx, err := nativeSetup(configPath, logLevel, logFormat, &opts)
	if err != nil {
		return err
	}
	if nctx.validationErr != nil {
		return &exitError{code: exitConfig, err: fmt.Errorf("configuration still invalid after writing templates: %w (run `synapass native doctor` for the full report)", nctx.validationErr)}
	}
	cfg, logger := nctx.cfg, nctx.logger

	fmt.Print("[4/5] build: ")
	if opts.skipBuild && opts.skipDashboardBuild {
		fmt.Println("skipped (--skip-build --skip-dashboard-build)")
	} else {
		if err := nativeBuild(paths, &opts); err != nil {
			return err
		}
	}

	fmt.Print("[5/5] migrate: ")
	if opts.skipMigrate {
		fmt.Println("skipped (--skip-migrate)")
	} else {
		fmt.Println()
		if err := runMigrate(cfg, logger); err != nil {
			return err
		}
	}

	gwPort := httpPort(cfg.HTTP.Addr, "8080")
	fmt.Printf(`
native install complete.

  gateway:   http://127.0.0.1:%s  (health: /health, readiness: /ready)
  dashboard: http://127.0.0.1:%d  (after 'synapass native up'; LAN: http://<this-machine-ip>:%d)
  secrets:   %s

next:
  synapass native up            # foreground supervisor: gateway + dashboard
  synapass native up --with-workers

then open the dashboard: first visit shows the setup screen that creates the
single console administrator and closes itself permanently.
`, gwPort, dashboardPort(&opts), dashboardPort(&opts), envPath)
	return nil
}

// nativeBuild compiles the gateway and the dashboard from a source checkout.
// Outside a checkout there is nothing to compile: the installed binaries are
// already the build, so this step is a no-op with an explanation.
func nativeBuild(paths nativecfg.Paths, opts *nativeOptions) error {
	fmt.Println()
	if !opts.skipBuild {
		root, err := findRepoRoot()
		if err != nil {
			if _, lookErr := exec.LookPath(paths.GatewayBin); lookErr == nil {
				fmt.Printf("      no source checkout found; using installed %s\n", paths.GatewayBin)
			} else {
				return &exitError{code: exitFailure, err: fmt.Errorf("no source checkout and no %s on PATH: install the binary or run from the repository", paths.GatewayBin)}
			}
		} else {
			fmt.Println("      building the gateway (go build ./cmd/synapass) ...")
			build := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(root, "bin", "synapass"), "./cmd/synapass")
			build.Dir = root
			build.Stdout = os.Stdout
			build.Stderr = os.Stderr
			if err := build.Run(); err != nil {
				return &exitError{code: exitFailure, err: fmt.Errorf("go build: %w (install Go 1.27+ or pass --skip-build)", err)}
			}
			fmt.Println("      gateway built: bin/synapass")
		}
	} else {
		fmt.Println("      gateway build skipped")
	}

	if !opts.skipDashboardBuild {
		if _, err := os.Stat(filepath.Join(paths.DashboardDir, "package.json")); err != nil {
			return &exitError{code: exitFailure, err: fmt.Errorf("no dashboard checkout at %s (set NATIVE_DASHBOARD_DIR or pass --skip-dashboard-build)", paths.DashboardDir)}
		}
		if _, err := exec.LookPath("node"); err != nil {
			return &exitError{code: exitFailure, err: fmt.Errorf("node not found: install Node.js 20+ or pass --skip-dashboard-build")}
		}
		if _, err := exec.LookPath("npm"); err != nil {
			return &exitError{code: exitFailure, err: fmt.Errorf("npm not found: install Node.js 20+ or pass --skip-dashboard-build")}
		}
		for _, step := range [][]string{{"npm", "ci", "--no-audit", "--no-fund"}, {"npm", "run", "build"}} {
			fmt.Printf("      running %s in %s ...\n", strings.Join(step, " "), paths.DashboardDir)
			cmd := exec.Command(step[0], step[1:]...)
			cmd.Dir = paths.DashboardDir
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return &exitError{code: exitFailure, err: fmt.Errorf("%s: %w", strings.Join(step, " "), err)}
			}
		}
		fmt.Println("      dashboard built")
	} else {
		fmt.Println("      dashboard build skipped")
	}
	return nil
}

// installTemplate reads a template from install/native/ of the source checkout.
// Outside a checkout there is nothing to copy from: the caller reports that
// plainly instead of writing a half-known file.
func installTemplate(name string) ([]byte, error) {
	root, err := findRepoRoot()
	if err != nil {
		return nil, fmt.Errorf("not a source checkout: fetch install/native/%s from the release and place it explicitly", name)
	}
	data, err := os.ReadFile(filepath.Join(root, "install", "native", name))
	if err != nil {
		return nil, fmt.Errorf("read install/native/%s: %w", name, err)
	}
	return data, nil
}

// findRepoRoot walks up from the working directory looking for go.mod.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found upwards of the working directory")
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// up
// ---------------------------------------------------------------------------

// runNativeUp starts the gateway, the dashboard and optionally the workers
// under a foreground supervisor and blocks until they are ready, then until
// they stop. Ctrl-C (SIGINT) or SIGTERM shuts everything down gracefully,
// newest first.
func runNativeUp(_ *config.Config, _ *slog.Logger, configPath, logLevel, logFormat string, args []string) error {
	fs := flag.NewFlagSet("native up", flag.ContinueOnError)
	var opts nativeOptions
	addNativeFlags(fs, &opts)
	if err := fs.Parse(args); err != nil {
		return &exitError{code: exitUsage, err: err}
	}

	nctx, err := nativeSetup(configPath, logLevel, logFormat, &opts)
	if err != nil {
		return err
	}
	cfg, logger, paths := nctx.cfg, nctx.logger, nctx.paths
	if nctx.validationErr != nil {
		return &exitError{code: exitConfig, err: fmt.Errorf("invalid configuration, refusing to start half a stack: %w (run `synapass native doctor` for the full report)", nctx.validationErr)}
	}
	if err := paths.EnsureStateDirs(); err != nil {
		return &exitError{code: exitFailure, err: err}
	}

	gwPort := httpPort(cfg.HTTP.Addr, "8080")
	dashPort := dashboardPort(&opts)
	dashHost := dashboardHost(&opts)

	if !opts.skipDashboard && !dashboardHostIsLoopback(dashHost) {
		fmt.Printf("WARNING: the dashboard is bound to %s. Until the first-run administrator is created,\n"+
			"         anyone who can reach this port can claim that account — open the console and\n"+
			"         complete setup now, or bind loopback with --dashboard-host 127.0.0.1.\n", dashHost)
	}

	children, err := nativeChildren(cfg, paths, &opts, gwPort, dashPort, dashHost)
	if err != nil {
		return err
	}

	sup := runtime.New(children, logger)
	ctx, stop := signalContext()
	defer stop()

	supErr := make(chan error, 1)
	go func() { supErr <- sup.Run(ctx) }()

	gwURL := "http://127.0.0.1:" + gwPort
	fmt.Printf("waiting for the gateway at %s/ready ...\n", gwURL)
	if err := runtime.WaitForReady(ctx, gwURL+"/ready", opts.readinessTimeout); err != nil {
		stop()
		<-supErr
		return &exitError{code: exitUnavailable, err: err}
	}
	fmt.Println("gateway is ready")

	if !opts.skipDashboard {
		dashURL := "http://" + dashDisplayHost(dashHost) + ":" + strconv.Itoa(dashPort)
		fmt.Printf("waiting for the dashboard at %s ...\n", dashURL)
		if err := runtime.WaitForReady(ctx, dashURL+"/", opts.readinessTimeout); err != nil {
			stop()
			<-supErr
			return &exitError{code: exitUnavailable, err: err}
		}
		fmt.Println("dashboard is ready")
	}

	fmt.Printf(`
Synapass is up (native, foreground — Ctrl-C stops everything).

  gateway:   %s  (/health liveness, /ready readiness, /metrics Prometheus)
  dashboard: %s%s
  logs:      %s
`, gwURL, dashURLOrNone(&opts, dashHost, dashPort), setupHint(), paths.LogDir)
	return <-supErr
}

// nativeChildren assembles the supervised processes in startup order. Anything
// missing fails here, before the first process starts, so `up` never leaves a
// half-running stack behind a late error.
func nativeChildren(cfg *config.Config, paths nativecfg.Paths, opts *nativeOptions, gwPort string, dashPort int, dashHost string) ([]runtime.Process, error) {
	logTo := func(name string) (string, string) {
		if opts.consoleLogs {
			return "", ""
		}
		return filepath.Join(paths.LogDir, name+".log"), filepath.Join(paths.LogDir, name+".log")
	}

	gatewayBin, err := resolveGatewayBin(paths)
	if err != nil {
		return nil, err
	}
	gwOut, gwErr := logTo("gateway")
	children := []runtime.Process{{
		Name: "gateway", Argv: []string{gatewayBin, "serve"},
		Env:          gatewayChildEnv(cfg),
		StdoutPath:   gwOut,
		StderrPath:   gwErr,
		Policy:       runtime.RestartOnFailure,
		RestartDelay: 3 * time.Second,
		StopTimeout:  60 * time.Second,
	}}

	if !opts.skipDashboard {
		server := filepath.Join(paths.DashboardDir, ".next", "standalone", "server.js")
		if _, err := os.Stat(server); err != nil {
			return nil, &exitError{code: exitFailure, err: fmt.Errorf("dashboard not built at %s: run `npm ci && npm run build` there or `synapass native install`", server)}
		}
		node, err := exec.LookPath("node")
		if err != nil {
			return nil, &exitError{code: exitFailure, err: fmt.Errorf("node not found: install Node.js 20+ or pass --skip-dashboard")}
		}
		dashOut, dashErr := logTo("dashboard")
		env := []string{
			"PORT=" + strconv.Itoa(dashPort),
			"HOSTNAME=" + dashHost,
		}
		if os.Getenv("SYNAPASS_API_URL") == "" {
			env = append(env, "SYNAPASS_API_URL=http://127.0.0.1:"+gwPort)
		}
		children = append(children, runtime.Process{
			Name: "dashboard", Argv: []string{node, server}, Dir: paths.DashboardDir,
			Env:          env,
			StdoutPath:   dashOut,
			StderrPath:   dashErr,
			Policy:       runtime.RestartOnFailure,
			RestartDelay: 3 * time.Second,
			StopTimeout:  30 * time.Second,
		})
	}

	if opts.withWorkers {
		python, err := workerPython(paths)
		if err != nil {
			return nil, &exitError{code: exitFailure, err: err}
		}
		workerOut, workerErr := logTo("worker")
		children = append(children, runtime.Process{
			Name: "worker", Argv: []string{python, "-m", "synapass_workers.cli", "serve"}, Dir: paths.StateDir,
			StdoutPath:   workerOut,
			StderrPath:   workerErr,
			Policy:       runtime.RestartOnFailure,
			RestartDelay: 5 * time.Second,
			StopTimeout:  30 * time.Second,
		})
	}
	return children, nil
}

// resolveGatewayBin prefers the configured binary and falls back to the
// running executable: `native up` invoked from an installed binary supervises
// itself, so an installation never depends on PATH containing a second copy.
func resolveGatewayBin(paths nativecfg.Paths) (string, error) {
	if paths.GatewayBin != "" && paths.GatewayBin != "synapass" {
		if _, err := os.Stat(paths.GatewayBin); err != nil {
			return "", &exitError{code: exitFailure, err: fmt.Errorf("gateway binary %s: %w", paths.GatewayBin, err)}
		}
		return paths.GatewayBin, nil
	}
	if path, err := exec.LookPath("synapass"); err == nil {
		return path, nil
	}
	if self, err := os.Executable(); err == nil {
		return self, nil
	}
	return "", &exitError{code: exitFailure, err: fmt.Errorf("no gateway binary found: build one (`make build`) or set NATIVE_GATEWAY_BIN")}
}

// gatewayChildEnv carries the resolved config file into the child. Without it
// the child would re-resolve configuration on its own and could disagree with
// the supervisor about which file won.
func gatewayChildEnv(cfg *config.Config) []string {
	if cfg.ConfigFile != "" {
		return []string{"SYNAPASS_CONFIG_FILE=" + cfg.ConfigFile}
	}
	return nil
}

// workerPython resolves the interpreter that runs the workers: the venv first,
// then the system python3. The venv layout differs per OS (bin/ vs Scripts/).
func workerPython(paths nativecfg.Paths) (string, error) {
	candidates := []string{
		filepath.Join(paths.WorkerVenv, "bin", "python"),
		filepath.Join(paths.WorkerVenv, "Scripts", "python.exe"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	if path, err := exec.LookPath("python3"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("no Python for the workers: create %s and `pip install ./workers`, or drop --with-workers (checked %s)",
		paths.WorkerVenv, strings.Join(candidates, ", "))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// postgresAddr dials the configured host and port, or derives them from the
// DSN when the parts are unset.
func postgresAddr(cfg *config.Config) string {
	if cfg.Database.Host != "" {
		port := cfg.Database.Port
		if port <= 0 {
			port = 5432
		}
		return net.JoinHostPort(cfg.Database.Host, strconv.Itoa(port))
	}
	if cfg.Database.DSN != "" {
		if addr := hostPortFromDSN(cfg.Database.DSN, "5432"); addr != "" {
			return addr
		}
	}
	return "127.0.0.1:5432"
}

// redisAddr dials the configured address as-is.
func redisAddr(cfg *config.Config) string {
	if cfg.Redis.Addr != "" {
		return cfg.Redis.Addr
	}
	return "127.0.0.1:6379"
}

// clickhouseAddr dials the native protocol port. A bare hostname gets :9000;
// anything already carrying a port is used verbatim.
func clickhouseAddr(cfg *config.Config) string {
	addr := cfg.ClickHouse.Addr
	if addr == "" {
		if host := hostPortFromDSN(cfg.ClickHouse.DSN, ""); host != "" {
			addr = host
		} else {
			addr = "127.0.0.1"
		}
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "9000")
	}
	return addr
}

// firstNATSURL returns the first configured NATS server URL, if any.
func firstNATSURL(cfg *config.Config) string {
	if cfg.NATS.URL != "" {
		return cfg.NATS.URL
	}
	if len(cfg.NATS.URLs) > 0 {
		return cfg.NATS.URLs[0]
	}
	return ""
}

// hostPortFromDSN extracts host:port from a URL-style DSN, defaulting the port
// when absent. Empty means the DSN was not parseable as a URL.
func hostPortFromDSN(dsn, defaultPort string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// httpPort reduces a listen address to its port for display.
func httpPort(addr, fallback string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	if strings.HasPrefix(addr, ":") && len(addr) > 1 {
		return addr[1:]
	}
	return fallback
}

// dashboardPort resolves the dashboard listen port: flag, then PORT, then 3000.
func dashboardPort(opts *nativeOptions) int {
	if opts.dashboardPort > 0 {
		return opts.dashboardPort
	}
	if raw := os.Getenv("PORT"); raw != "" {
		if port, err := strconv.Atoi(raw); err == nil && port > 0 {
			return port
		}
	}
	return 3000
}

// dashboardHost resolves the dashboard bind address, defaulting to loopback.
//
// The console is authenticated, but its first-run setup endpoint is not: until
// an operator exists, whoever can reach the dashboard can create the account and
// become the administrator. Binding to all interfaces by default therefore made
// "install and open the console later" a race that a LAN attacker could win.
// LAN access remains available and documented — it is now an explicit choice
// (--dashboard-host 0.0.0.0 or HOSTNAME=0.0.0.0), which is also the point at
// which the operator can be warned about the setup window.
func dashboardHost(opts *nativeOptions) string {
	if opts.dashboardHost != "" {
		return opts.dashboardHost
	}
	if host := os.Getenv("HOSTNAME"); host != "" {
		return host
	}
	return "127.0.0.1"
}

// dashboardHostIsLoopback reports whether a bind address keeps the console on
// this machine. It accepts the loopback names Go resolves as loopback.
func dashboardHostIsLoopback(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	default:
		return false
	}
}

// dashDisplayHost maps a wildcard bind to a clickable address: 0.0.0.0 and ::
// are not dialable, so reports use localhost while the LAN hint names the
// network address separately.
func dashDisplayHost(host string) string {
	if host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	if host == "" {
		return "127.0.0.1"
	}
	return host
}

// dashURLOrNone renders the dashboard line of the readiness summary.
func dashURLOrNone(opts *nativeOptions, host string, port int) string {
	if opts.skipDashboard {
		return "(skipped)"
	}
	return "http://" + dashDisplayHost(host) + ":" + strconv.Itoa(port) + "  (LAN: http://<this-machine-ip>:" + strconv.Itoa(port) + ")"
}

// setupHint reminds the operator where the console credential comes from.
func setupHint() string {
	return `
  first visit: the dashboard shows the setup screen that creates the single
  console administrator and closes itself permanently.`
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// randomHex returns n random bytes as hex, for generated secrets.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// writePrivateFile writes data with owner-only permissions, creating parents.
func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create parent of %s: %w", path, err)
	}
	return os.WriteFile(path, data, 0o600)
}

// writeConfigFile writes data without clobbering: callers check absence first.
func writeConfigFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create parent of %s: %w", path, err)
	}
	return os.WriteFile(path, data, 0o640)
}

// ---------------------------------------------------------------------------
// admin
// ---------------------------------------------------------------------------

// runNativeAdmin seeds console state that the gateway would otherwise only
// create through its first-run setup screen. It exists so an installer can
// create the operator account as part of installation rather than leaving the
// user with a second form to fill in on first launch.
func runNativeAdmin(cfg *config.Config, logger *slog.Logger, configPath, logLevel, logFormat string, args []string) error {
	if len(args) == 0 {
		return &exitError{code: exitUsage, err: fmt.Errorf("usage: synapass native admin set --username U --password-file F")}
	}
	switch args[0] {
	case "set":
		return runNativeAdminSet(cfg, logger, configPath, logLevel, logFormat, args[1:])
	default:
		return &exitError{code: exitUsage, err: fmt.Errorf("unknown native admin command %q (want set)", args[0])}
	}
}

// runNativeAdminSet creates the single console administrator if none exists.
//
// It is idempotent on purpose: re-running the installer, or running this command
// on an installation that already has an operator, leaves the existing account
// untouched and exits successfully. Only the first call creates a user.
func runNativeAdminSet(cfg *config.Config, logger *slog.Logger, configPath, logLevel, logFormat string, args []string) error {
	fs := flag.NewFlagSet("native admin set", flag.ContinueOnError)
	var opts nativeOptions
	addNativeFlags(fs, &opts)
	username := fs.String("username", "", "administrator username")
	password := fs.String("password", "", "administrator password (prefer --password-file)")
	passwordFile := fs.String("password-file", "", "read the administrator password from this file")
	if err := fs.Parse(args); err != nil {
		return &exitError{code: exitUsage, err: err}
	}

	nctx, err := nativeSetup(configPath, logLevel, logFormat, &opts)
	if err != nil {
		return err
	}
	cfg, logger = nctx.cfg, nctx.logger
	if nctx.validationErr != nil {
		return &exitError{code: exitConfig, err: fmt.Errorf("invalid configuration: %w", nctx.validationErr)}
	}

	user := strings.TrimSpace(*username)
	if user == "" {
		return &exitError{code: exitUsage, err: fmt.Errorf("--username is required")}
	}
	secret, err := readAdminPassword(*password, *passwordFile)
	if err != nil {
		return &exitError{code: exitUsage, err: err}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	postgres, err := storage.NewPostgres(ctx, cfg.Database, logger)
	if err != nil {
		return &exitError{code: exitUnavailable, err: err}
	}
	defer postgres.Close()

	repos := storage.NewRepositories(postgres.Pool(), logger)
	service := bootstrap.BuildDashboardAuth(cfg, repos, logger)
	if service == nil {
		return &exitError{code: exitConfig, err: fmt.Errorf("dashboard authentication is disabled; enable admin.dashboard_auth or create the account in the dashboard")}
	}

	client := dashboardauth.ClientInfo{IP: "127.0.0.1", UserAgent: "synapass-native-admin"}
	result, err := service.Setup(ctx, user, secret, secret, client)
	if err != nil {
		if errors.Is(err, dashboardauth.ErrSetupClosed) {
			fmt.Println("admin: an administrator already exists; leaving it unchanged")
			return nil
		}
		return &exitError{code: exitFailure, err: err}
	}
	fmt.Printf("admin: created %s\n", result.User.Username)
	return nil
}

// readAdminPassword resolves the password from a file or a flag. The file form
// is preferred because an argument is visible in the process list.
func readAdminPassword(inline, path string) (string, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read --password-file: %w", err)
		}
		secret := strings.TrimRight(string(data), "\r\n")
		if secret == "" {
			return "", fmt.Errorf("--password-file is empty")
		}
		return secret, nil
	}
	if inline == "" {
		return "", fmt.Errorf("a password is required (--password-file is preferred over --password)")
	}
	return inline, nil
}
