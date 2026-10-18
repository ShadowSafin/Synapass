// Command installer is the Synapass setup program.
//
// It is a setup-only executable: it shows the wizard, installs the standalone
// application (Synapass.exe), prepares the local database and the first
// administrator, creates shortcuts and registers an uninstall entry, then
// exits. It never runs the application itself — that is the job of the
// Synapass.exe it installs.
package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unsafe"

	webview "github.com/webview/webview_go"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/shadowsafin/synapass/desktop/internal/payload"
	"github.com/shadowsafin/synapass/desktop/pkg/dbembed"
	"github.com/shadowsafin/synapass/desktop/pkg/supervisor"
	"github.com/shadowsafin/synapass/desktop/pkg/winproc"
)

//go:embed installer.html
var installerHTML string

// The console's type (Geist, Geist Mono, Instrument Serif) is embedded so the
// wizard matches the dashboard's auth screens exactly and still renders offline.
//
//go:embed fonts/*.woff2
var installerFonts embed.FS

// brand.png is the product icon shown next to the wordmark. It is embedded
// because the wizard renders before anything is installed, so the file the
// installer would otherwise extract does not exist yet. `cmd/icongen` writes it.
//
//go:embed brand.png
var installerBrand []byte

//go:embed uninstall.ps1
var uninstallScript []byte

const appName = "Synapass"
const appExeName = appName + ".exe"

// fatal reports a startup failure the user can actually see.
func fatal(err error) {
	msg := err.Error()
	fmt.Fprintln(os.Stderr, "synapass-setup:", msg)
	if !hasConsole() {
		user32 := windows.NewLazySystemDLL("user32.dll")
		proc := user32.NewProc("MessageBoxW")
		t, _ := windows.UTF16PtrFromString(appName + " Setup could not start")
		m, _ := windows.UTF16PtrFromString(msg)
		const mbOK, mbIconError, mbSetForeground, mbTopMost = 0x0, 0x10, 0x10000, 0x40000
		_, _, _ = proc.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)),
			mbOK|mbIconError|mbSetForeground|mbTopMost)
	}
	os.Exit(1)
}

func hasConsole() bool {
	h, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	return windows.GetConsoleMode(h, &mode) == nil
}

func main() {
	silent := flag.Bool("silent", false, "run the install without a window (unattended)")
	configPath := flag.String("config", "", "JSON file of install options for --silent")
	flag.Parse()

	exe, err := os.Executable()
	if err != nil {
		fatal(err)
	}

	if *silent {
		opts, err := readSilentOptions(*configPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "synapass-setup:", err)
			os.Exit(2)
		}
		app := &installerApp{self: exe, silent: true}
		app.runInstallSilent(opts)
		if !app.ok {
			os.Exit(1)
		}
		return
	}

	app := &installerApp{w: webview.New(true), self: exe, quit: make(chan struct{})}
	defer app.w.Destroy()
	app.w.SetTitle(appName + " Setup")
	app.w.SetSize(1060, 720, webview.HintNone)
	app.w.SetSize(920, 620, webview.HintMin)
	app.w.Navigate("data:text/html," + url.PathEscape(installerPage()))

	app.w.Bind("syn_info", app.info)
	app.w.Bind("syn_browse", app.browse)
	app.w.Bind("syn_install", app.install)
	app.w.Bind("syn_launch", app.launch)
	app.w.Bind("syn_open_folder", app.openFolder)
	app.w.Bind("syn_close", app.close)

	go func() {
		<-app.quit
		app.closeWindow()
	}()
	app.w.Run()
}

// closeWindow ends the wizard.
//
// It must reach the UI thread to do it. On Windows the backend stops the loop
// with PostQuitMessage, which delivers WM_QUIT to the *calling* thread's message
// queue; called straight from a background goroutine it posts onto a thread
// that has no message loop, so the window never closes and setup appears to
// hang after a successful install. Dispatch marshals the call onto the thread
// that actually runs the loop.
func (a *installerApp) closeWindow() {
	if a.w == nil {
		return
	}
	a.w.Dispatch(func() { a.w.Terminate() })
}

// installerApp bridges the wizard UI and the install pipeline. In --silent mode
// w is nil and progress is written to stderr instead of the window.
type installerApp struct {
	w      webview.WebView
	self   string
	quit   chan struct{}
	silent bool
	ok     bool
}

// installOptions is the choice set collected by the wizard.
type installOptions struct {
	InstallDir      string `json:"installDir"`
	DataDir         string `json:"dataDir"` // the app root: native.env, logs, data
	DesktopShortcut bool   `json:"desktopShortcut"`
	StartMenu       bool   `json:"startMenu"`
	Startup         bool   `json:"startup"`
	LaunchAfter     bool   `json:"launchAfter"`
	AdminUser       string `json:"adminUser"`
	AdminPassword   string `json:"adminPassword"`
}

// installerPage returns the wizard HTML with the embedded fonts inlined as
// @font-face data URIs, so the page is one self-contained document.
func installerPage() string {
	page := strings.Replace(installerHTML, "/*__FONT_FACE__*/", fontFaceCSS(), 1)
	return strings.Replace(page, `src="__BRAND_ICON__"`,
		`src="data:image/png;base64,`+base64.StdEncoding.EncodeToString(installerBrand)+`"`, 1)
}

// fontFaceCSS builds the @font-face block from the woff2 files compiled into
// the installer. Missing files are skipped rather than fatal: the family stacks
// in the page fall back to system fonts.
func fontFaceCSS() string {
	faces := []struct {
		family, file, style, weight string
	}{
		{"Geist", "geist-latin.woff2", "normal", "100 900"},
		{"Geist Mono", "geist-mono-latin.woff2", "normal", "100 900"},
		{"Instrument Serif", "instrument-serif-latin.woff2", "normal", "400"},
		{"Instrument Serif", "instrument-serif-italic-latin.woff2", "italic", "400"},
	}
	var b strings.Builder
	for _, f := range faces {
		data, err := installerFonts.ReadFile("fonts/" + f.file)
		if err != nil {
			continue
		}
		b.WriteString(fmt.Sprintf(
			"@font-face{font-family:'%s';font-style:%s;font-weight:%s;font-display:block;"+
				"src:url(data:font/woff2;base64,%s) format('woff2');}\n",
			f.family, f.style, f.weight, base64.StdEncoding.EncodeToString(data)))
	}
	return b.String()
}

// eval runs JavaScript on the UI thread. In silent mode there is no window.
func (a *installerApp) eval(js string) {
	if a.w == nil {
		return
	}
	a.w.Dispatch(func() { a.w.Eval(js) })
}

// readSilentOptions loads install options for an unattended run.
func readSilentOptions(path string) (installOptions, error) {
	var opts installOptions
	if path == "" {
		return opts, fmt.Errorf("--silent needs --config <file.json>")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return opts, err
	}
	if err := json.Unmarshal(data, &opts); err != nil {
		return opts, fmt.Errorf("parse %s: %w", path, err)
	}
	return opts, nil
}

// info returns the wizard's starting state as JSON.
func (a *installerApp) info() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = filepath.Dir(a.self)
	}
	target := filepath.Join(base, appName)
	_, statErr := os.Stat(filepath.Join(target, "native.env"))
	data := map[string]any{
		"product":    appName,
		"version":    payload.Version(),
		"source":     a.self,
		"installDir": target,
		"dataDir":    target,
		"installed":  statErr == nil,
		"tagline":    "Your private inference gateway — routing, failover and cost control in one place.",
	}
	out, _ := json.Marshal(data)
	return string(out)
}

// browse opens a native folder chooser and returns the chosen path, or "".
func (a *installerApp) browse(initial string) string {
	if initial == "" {
		initial = os.Getenv("LOCALAPPDATA")
	}
	script := fmt.Sprintf(
		`Add-Type -AssemblyName System.Windows.Forms; `+
			`$d = New-Object System.Windows.Forms.FolderBrowserDialog; `+
			`$d.Description = 'Choose a folder'; $d.ShowNewFolderButton = $true; `+
			`if (Test-Path %s) { $d.SelectedPath = %s }; `+
			`if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.Write($d.SelectedPath) }`,
		psQuote(initial), psQuote(initial))
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
	winproc.Hide(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// install performs the installation, pushing progress to the UI as it goes.
func (a *installerApp) install(input string) string {
	var opts installOptions
	if err := json.Unmarshal([]byte(input), &opts); err != nil {
		a.done(false, "The setup options could not be read.", "", "", false, false)
		return ""
	}
	go a.runInstall(opts)
	return ""
}

// runInstall is the install pipeline. Every stage reports before and after, so
// the UI never looks frozen.
func (a *installerApp) runInstall(opts installOptions) {
	installDir := strings.TrimSpace(opts.InstallDir)
	rootDir := strings.TrimSpace(opts.DataDir)
	if rootDir == "" {
		rootDir = installDir
	}
	if installDir == "" {
		a.done(false, "Choose an install folder to continue.", "", "", false, false)
		return
	}
	stage := func(pct int, label string) { a.progress(pct, label) }

	// 1) Folders.
	stage(8, "Preparing folders")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		a.fail("The install folder could not be created.", err)
		return
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "logs"), 0o750); err != nil {
		a.fail("The data folder could not be created.", err)
		return
	}
	logger := openInstallLog(filepath.Join(rootDir, "logs", "install.log"))

	// 2) Runtime: the gateway, node and dashboard the app runs on.
	stage(20, "Installing the application files")
	if err := payload.Extract(rootDir); err != nil {
		a.fail("The application files could not be installed.", err)
		return
	}
	appExe, err := payload.AppExecutable()
	if err != nil {
		a.fail("The application executable is missing from this installer.", err)
		return
	}
	appPath := filepath.Join(installDir, appExeName)
	if err := writeExecutable(appPath, appExe); err != nil {
		a.fail("The application could not be installed.", err)
		return
	}
	logf(logger, "installed %s and runtime under %s", appPath, rootDir)

	// 3) Configuration. An existing native.env is preserved: regenerating its
	// ports or database password would lock the app out of the database already
	// there.
	stage(36, "Writing configuration")
	cfg := installConfig{installDir: installDir, rootDir: rootDir, dataDir: rootDir}
	envPath := filepath.Join(rootDir, "native.env")
	if _, err := os.Stat(envPath); err == nil {
		// Upgrade in place: reuse the ports and secrets already in the file.
		// Regenerating them would lock the app out of the database that is
		// already there. Without reading them back the config would stay
		// zeroed and provisioning would start the database on port 0.
		existing, lerr := loadExistingConfig(installDir, rootDir)
		if lerr != nil {
			a.fail("The existing configuration could not be read.", lerr)
			return
		}
		cfg = existing
		logf(logger, "keeping the existing native.env (upgrade in place): gateway %d, dashboard %d, postgres %d", cfg.gwPort, cfg.dashPort, cfg.pgPort)
	} else {
		if cfg, err = allocateConfig(installDir, rootDir); err != nil {
			a.fail("Could not pick local ports or generate secrets.", err)
			return
		}
		if err := writeInstallConfig(cfg); err != nil {
			a.fail("The configuration files could not be written.", err)
			return
		}
		logf(logger, "wrote native.env and config.yaml (gateway %d, dashboard %d, postgres %d)", cfg.gwPort, cfg.dashPort, cfg.pgPort)
	}
	if !strings.EqualFold(installDir, rootDir) {
		if err := os.WriteFile(filepath.Join(installDir, "root.txt"), []byte(rootDir), 0o644); err != nil {
			a.fail("The data folder location could not be recorded.", err)
			return
		}
	}

	// 4) Database, migrations and the first administrator, all while the
	// embedded database is up.
	//
	// Check the ports first. A copy that is still open — or one that was killed
	// and left its embedded database behind — holds these ports, and without a
	// preflight the install dies deep in the pipeline on a bare "port already
	// in use", after copying files and with nothing to act on.
	stage(46, "Checking the local services")
	if busy := busyPort(cfg.pgPort, cfg.gwPort, cfg.dashPort); busy != "" {
		a.fail("Synapass is already running.",
			fmt.Errorf("%s is still in use; quit Synapass from its tray icon, then run setup again", busy))
		return
	}
	stage(50, "Setting up the local database")
	passwordFile := ""
	if strings.TrimSpace(opts.AdminUser) != "" && opts.AdminPassword != "" {
		passwordFile = filepath.Join(rootDir, "logs", ".admin-password")
		if err := os.WriteFile(passwordFile, []byte(opts.AdminPassword), 0o600); err != nil {
			a.fail("The administrator password could not be staged.", err)
			return
		}
		defer os.Remove(passwordFile)
	}
	if err := a.provision(cfg, opts.AdminUser, passwordFile, logger); err != nil {
		a.fail("The local database could not be set up.", err)
		return
	}
	stage(76, "Database ready")

	// 5) Shortcuts.
	stage(84, "Creating shortcuts")
	a.makeShortcuts(appPath, installDir, rootDir, opts, logger)

	// 6) Uninstaller + registration.
	stage(90, "Registering the application")
	if err := os.WriteFile(filepath.Join(installDir, "uninstall.ps1"), uninstallScript, 0o644); err != nil {
		logf(logger, "could not write uninstall.ps1: %v", err)
	}
	if err := a.registerUninstall(appPath, installDir, rootDir); err != nil {
		logf(logger, "uninstall registration failed: %v", err)
	}

	// 7) Done.
	stage(100, "Finished")
	launched := false
	if opts.LaunchAfter {
		if err := a.launchInstalled(appPath, installDir, rootDir); err == nil {
			launched = true
		}
	}
	// When the app will open after install, the installer should disappear so
	// users do not sit on top of the app wondering whether setup is still going.
	closeOnLaunch := launched && a.w != nil
	a.done(true, "", installDir, rootDir, launched, closeOnLaunch)
	if closeOnLaunch {
		// The page closes itself once it has shown the finish screen, but the
		// install worker owns the close as well: a wedged page or a raised dialog
		// must not leave the wizard sitting on top of the app that just opened.
		time.AfterFunc(3*time.Second, func() { a.close() })
	}
}

// runInstallSilent is the unattended install pipeline. It reuses the same stages
// as the windowed install but reports to stderr instead of the wizard.
func (a *installerApp) runInstallSilent(opts installOptions) {
	installDir := strings.TrimSpace(opts.InstallDir)
	rootDir := strings.TrimSpace(opts.DataDir)
	if rootDir == "" {
		rootDir = installDir
	}
	if installDir == "" {
		a.ok = false
		fmt.Fprintln(os.Stderr, "Choose an install folder to continue.")
		return
	}
	stage := func(pct int, label string) {
		fmt.Fprintf(os.Stderr, "[%3d%%] %s\n", pct, label)
	}

	// 1) Folders.
	stage(8, "Preparing folders")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		a.fail("The install folder could not be created.", err)
		return
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "logs"), 0o750); err != nil {
		a.fail("The data folder could not be created.", err)
		return
	}
	logger := openInstallLog(filepath.Join(rootDir, "logs", "install.log"))

	// 2) Runtime: the gateway, node and dashboard the app runs on.
	stage(20, "Installing the application files")
	if err := payload.Extract(rootDir); err != nil {
		a.fail("The application files could not be installed.", err)
		return
	}
	appExe, err := payload.AppExecutable()
	if err != nil {
		a.fail("The application executable is missing from this installer.", err)
		return
	}
	appPath := filepath.Join(installDir, appExeName)
	if err := writeExecutable(appPath, appExe); err != nil {
		a.fail("The application could not be installed.", err)
		return
	}
	logf(logger, "installed %s and runtime under %s", appPath, rootDir)

	// 3) Configuration. An existing native.env is preserved: regenerating its
	// ports or database password would lock the app out of the database already
	// there.
	stage(36, "Writing configuration")
	cfg := installConfig{installDir: installDir, rootDir: rootDir, dataDir: rootDir}
	envPath := filepath.Join(rootDir, "native.env")
	if _, err := os.Stat(envPath); err == nil {
		existing, lerr := loadExistingConfig(installDir, rootDir)
		if lerr != nil {
			a.fail("The existing configuration could not be read.", lerr)
			return
		}
		cfg = existing
		logf(logger, "keeping the existing native.env (upgrade in place): gateway %d, dashboard %d, postgres %d", cfg.gwPort, cfg.dashPort, cfg.pgPort)
	} else {
		if cfg, err = allocateConfig(installDir, rootDir); err != nil {
			a.fail("Could not pick local ports or generate secrets.", err)
			return
		}
		if err := writeInstallConfig(cfg); err != nil {
			a.fail("The configuration files could not be written.", err)
			return
		}
		logf(logger, "wrote native.env and config.yaml (gateway %d, dashboard %d, postgres %d)", cfg.gwPort, cfg.dashPort, cfg.pgPort)
	}
	if !strings.EqualFold(installDir, rootDir) {
		if err := os.WriteFile(filepath.Join(installDir, "root.txt"), []byte(rootDir), 0o644); err != nil {
			a.fail("The data folder location could not be recorded.", err)
			return
		}
	}

	// 4) Database, migrations and the first administrator, all while the
	// embedded database is up.
	stage(46, "Checking the local services")
	if busy := busyPort(cfg.pgPort, cfg.gwPort, cfg.dashPort); busy != "" {
		a.fail("Synapass is already running.",
			fmt.Errorf("%s is still in use; quit Synapass from its tray icon, then run setup again", busy))
		return
	}
	stage(50, "Setting up the local database")
	passwordFile := ""
	if strings.TrimSpace(opts.AdminUser) != "" && opts.AdminPassword != "" {
		passwordFile = filepath.Join(rootDir, "logs", ".admin-password")
		if err := os.WriteFile(passwordFile, []byte(opts.AdminPassword), 0o600); err != nil {
			a.fail("The administrator password could not be staged.", err)
			return
		}
		defer os.Remove(passwordFile)
	}
	if err := a.provision(cfg, opts.AdminUser, passwordFile, logger); err != nil {
		a.fail("The local database could not be set up.", err)
		return
	}
	stage(76, "Database ready")

	// 5) Shortcuts.
	stage(84, "Creating shortcuts")
	a.makeShortcuts(appPath, installDir, rootDir, opts, logger)

	// 6) Uninstaller + registration.
	stage(90, "Registering the application")
	if err := os.WriteFile(filepath.Join(installDir, "uninstall.ps1"), uninstallScript, 0o644); err != nil {
		logf(logger, "could not write uninstall.ps1: %v", err)
	}
	if err := a.registerUninstall(appPath, installDir, rootDir); err != nil {
		logf(logger, "uninstall registration failed: %v", err)
	}

	// 7) Done.
	stage(100, "Finished")
	launched := false
	if opts.LaunchAfter {
		if err := a.launchInstalled(appPath, installDir, rootDir); err == nil {
			launched = true
		}
	}
	closeOnLaunch := launched && a.w != nil
	a.done(true, "", installDir, rootDir, launched, closeOnLaunch)
}

// ---------------------------------------------------------------------------
// provisioning
// ---------------------------------------------------------------------------

// installConfig is the generated configuration for a fresh installation.
type installConfig struct {
	installDir string
	rootDir    string
	dataDir    string // database and logs root; usually rootDir
	gwPort     int
	dashPort   int
	pgPort     int
	adminKey   string
	pgPassword string
}

// allocateConfig picks free ports and generates secrets.
func allocateConfig(installDir, rootDir string) (installConfig, error) {
	cfg := installConfig{installDir: installDir, rootDir: rootDir, dataDir: rootDir}
	var err error
	if cfg.gwPort, err = freePort(18081); err != nil {
		return cfg, err
	}
	if cfg.dashPort, err = freePort(3100); err != nil {
		return cfg, err
	}
	if cfg.pgPort, err = freePort(5433); err != nil {
		return cfg, err
	}
	if cfg.adminKey, err = randomHex(24); err != nil {
		return cfg, err
	}
	cfg.pgPassword, err = randomHex(16)
	return cfg, err
}

// writeInstallConfig writes native.env and config.yaml into the app root from
// the templates embedded in the installer.
func writeInstallConfig(cfg installConfig) error {
	envTmpl, err := payload.Template("native.env.template")
	if err != nil {
		return fmt.Errorf("read env template: %w", err)
	}
	cfgTmpl, err := payload.Template("config.yaml")
	if err != nil {
		return fmt.Errorf("read config template: %w", err)
	}
	repl := func(s string) string {
		for k, v := range map[string]string{
			"__ADMIN_KEY__":   cfg.adminKey,
			"__GW_PORT__":     strconv.Itoa(cfg.gwPort),
			"__DASH_PORT__":   strconv.Itoa(cfg.dashPort),
			"__PG_PORT__":     strconv.Itoa(cfg.pgPort),
			"__PG_PASSWORD__": cfg.pgPassword,
		} {
			s = strings.ReplaceAll(s, k, v)
		}
		return s
	}
	if err := os.WriteFile(filepath.Join(cfg.rootDir, "native.env"), []byte(repl(string(envTmpl))), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.rootDir, "config.yaml"), []byte(repl(string(cfgTmpl))), 0o640)
}

// loadExistingConfig reads the ports and secrets from an installation already
// present at rootDir so an upgrade reuses them. Regenerating them would leave
// the app unable to reach the database it already has.
func loadExistingConfig(installDir, rootDir string) (installConfig, error) {
	cfg := installConfig{installDir: installDir, rootDir: rootDir, dataDir: rootDir}
	vars, err := supervisor.LoadEnvFile(filepath.Join(rootDir, "native.env"))
	if err != nil {
		return cfg, err
	}
	cfg.adminKey = strings.TrimSpace(vars["SYNAPASS_ADMIN_KEY"])
	cfg.pgPassword = strings.TrimSpace(vars["SYNAPASS_POSTGRES_PASSWORD"])
	if v := strings.TrimSpace(vars["SYNAPASS_DATA_DIR"]); v != "" {
		cfg.dataDir = v
	}
	if p, perr := portFromAddr(vars["SYNAPASS_HTTP_ADDR"]); perr == nil {
		cfg.gwPort = p
	}
	if p, aerr := strconv.Atoi(strings.TrimSpace(vars["PORT"])); aerr == nil {
		cfg.dashPort = p
	}
	if p, aerr := strconv.Atoi(strings.TrimSpace(vars["SYNAPASS_POSTGRES_PORT"])); aerr == nil {
		cfg.pgPort = p
	}
	if cfg.pgPort == 0 || cfg.pgPassword == "" {
		return cfg, fmt.Errorf("the existing native.env is missing SYNAPASS_POSTGRES_PORT or SYNAPASS_POSTGRES_PASSWORD")
	}
	return cfg, nil
}

// portFromAddr returns the port from a host:port address ("127.0.0.1:8080" or
// ":8080").
func portFromAddr(addr string) (int, error) {
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		addr = addr[i+1:]
	}
	return strconv.Atoi(addr)
}

// provision starts the embedded database, applies migrations and creates the
// first administrator. It is the installation's database work, so it lives in
// the installer, not in the runtime app.
func (a *installerApp) provision(cfg installConfig, adminUser, passwordFile string, logger *os.File) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	slogLogger := slog.New(slog.NewTextHandler(logger, nil))
	dataRoot := cfg.dataDir
	if dataRoot == "" {
		dataRoot = cfg.rootDir
	}
	pgDir := filepath.Join(dataRoot, "data", "postgres")
	pg, err := dbembed.Start(ctx, dbembed.Options{
		Dir:      pgDir,
		Port:     uint32(cfg.pgPort),
		User:     "synapass",
		Password: cfg.pgPassword,
		Database: "synapass",
		Log:      logger,
	}, slogLogger)
	if err != nil {
		return err
	}
	defer func() { _ = pg.Stop() }()

	// The gateway validates its configuration, so it needs the same secrets and
	// ports the app will use: load the native.env the installer just wrote.
	env := os.Environ()
	if vars, err := supervisor.LoadEnvFile(filepath.Join(cfg.rootDir, "native.env")); err == nil {
		for k, v := range vars {
			env = append(env, k+"="+v)
		}
	}
	env = append(env,
		"SYNAPASS_POSTGRES_DSN="+pg.DSN,
		"SYNAPASS_CONFIG_FILE="+filepath.Join(cfg.rootDir, "config.yaml"),
	)
	gateway := filepath.Join(cfg.rootDir, "bin", "synapass.exe")

	if err := runChild(ctx, gateway, []string{"migrate"}, env, logger); err != nil {
		return fmt.Errorf("migrations failed: %w", err)
	}
	if strings.TrimSpace(adminUser) != "" && passwordFile != "" {
		args := []string{"native", "admin", "set", "--username", adminUser, "--password-file", passwordFile}
		if err := runChild(ctx, gateway, args, env, logger); err != nil {
			return fmt.Errorf("administrator setup failed: %w", err)
		}
	}
	return nil
}

// runChild runs the gateway for a one-shot step, streaming to the install log.
func runChild(ctx context.Context, bin string, args, env []string, logger *os.File) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	winproc.Hide(cmd)
	cmd.Env = env
	cmd.Stdout = logger
	cmd.Stderr = logger
	return cmd.Run()
}

// ---------------------------------------------------------------------------
// Windows integration
// ---------------------------------------------------------------------------

// makeShortcuts creates the desktop, Start Menu and startup shortcuts. Failures
// are logged, never fatal.
func (a *installerApp) makeShortcuts(appPath, workDir, rootDir string, opts installOptions, logger *os.File) {
	args := ""
	if !strings.EqualFold(rootDir, workDir) {
		args = fmt.Sprintf("--root %q", rootDir)
	}
	appdata := os.Getenv("APPDATA")
	var links []string
	if opts.DesktopShortcut {
		if desktop := desktopDir(); desktop != "" {
			links = append(links, filepath.Join(desktop, appName+".lnk"))
		}
	}
	if opts.StartMenu && appdata != "" {
		links = append(links, filepath.Join(appdata, `Microsoft\Windows\Start Menu\Programs\`+appName+".lnk"))
	}
	if opts.Startup && appdata != "" {
		links = append(links, filepath.Join(appdata, `Microsoft\Windows\Start Menu\Programs\Startup\`+appName+".lnk"))
	}
	for _, link := range links {
		if err := createShortcut(link, appPath, workDir, args); err != nil {
			logf(logger, "shortcut %s: %v", link, err)
		}
	}
}

// createShortcut writes one .lnk through the Windows Script Host shell.
func createShortcut(linkPath, target, workDir, args string) error {
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf(
		`$w = New-Object -ComObject WScript.Shell; `+
			`$s = $w.CreateShortcut(%s); $s.TargetPath = %s; $s.WorkingDirectory = %s; `+
			`$s.IconLocation = %s; $s.Arguments = %s; $s.Save()`,
		psQuote(linkPath), psQuote(target), psQuote(workDir), psQuote(target+",0"), psQuote(args))
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	winproc.Hide(cmd)
	return cmd.Run()
}

// registerUninstall writes the per-user Add/Remove Programs entry.
func (a *installerApp) registerUninstall(appPath, installDir, rootDir string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Uninstall\`+appName, registry.WRITE)
	if err != nil {
		return err
	}
	defer key.Close()

	set := func(name, value string) {
		if value != "" {
			_ = key.SetStringValue(name, value)
		}
	}
	set("DisplayName", appName)
	set("Publisher", appName)
	set("DisplayVersion", payload.Version())
	set("InstallLocation", installDir)
	set("DisplayIcon", appPath+",0")
	// A single-file uninstaller is written beside the app so the Run-a-program
	// path and the Settings app both work.
	uninstall := filepath.Join(installDir, "uninstall.ps1")
	if _, err := os.Stat(uninstall); err == nil {
		set("UninstallString", fmt.Sprintf(`powershell -ExecutionPolicy Bypass -File "%s"`, uninstall))
	}
	_ = key.SetDWordValue("NoModify", 1)
	_ = key.SetDWordValue("NoRepair", 1)
	return nil
}

// launchInstalled starts the standalone app.
func (a *installerApp) launchInstalled(appPath, workDir, rootDir string) error {
	var cmd *exec.Cmd
	if strings.EqualFold(rootDir, workDir) {
		cmd = exec.Command(appPath)
	} else {
		cmd = exec.Command(appPath, "--root", rootDir)
	}
	cmd.Dir = workDir
	return cmd.Start()
}

// ---------------------------------------------------------------------------
// UI bindings
// ---------------------------------------------------------------------------

func (a *installerApp) launch(appPath, workDir, rootDir string) string {
	if appPath == "" {
		return "no target"
	}
	if err := a.launchInstalled(appPath, workDir, rootDir); err != nil {
		return err.Error()
	}
	return ""
}

func (a *installerApp) openFolder(path string) string {
	if path == "" {
		return ""
	}
	_ = exec.Command("explorer", path).Start()
	return ""
}

func (a *installerApp) close() string {
	select {
	case <-a.quit:
	default:
		close(a.quit)
	}
	return ""
}

func (a *installerApp) progress(pct int, label string) {
	if a.silent {
		fmt.Fprintf(os.Stderr, "[%3d%%] %s\n", pct, label)
		return
	}
	a.eval(fmt.Sprintf("window.__ar && __ar.onProgress(%d, %s)", pct, jsString(label)))
}

func (a *installerApp) fail(summary string, err error) {
	detail := summary
	if err != nil {
		detail += " (" + err.Error() + ")"
	}
	if a.silent {
		fmt.Fprintln(os.Stderr, "install failed:", detail)
		return
	}
	a.eval("window.__ar && __ar.onError(" + jsString(detail) + ")")
}

func (a *installerApp) done(ok bool, msg, installDir, dataDir string, launched bool, closeOnLaunch bool) {
	a.ok = ok
	if a.silent {
		if ok {
			fmt.Printf("installed to %s (data %s, launched=%v)\n", installDir, dataDir, launched)
		} else {
			fmt.Fprintln(os.Stderr, "install failed:", msg)
		}
		return
	}
	out, _ := json.Marshal(map[string]any{
		"ok":            ok,
		"message":       msg,
		"installDir":    installDir,
		"dataDir":       dataDir,
		"launched":      launched,
		"closeOnLaunch": closeOnLaunch,
		"version":       payload.Version(),
	})
	a.eval("window.__ar && __ar.onDone(" + string(out) + ")")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeExecutable(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Rename(tmp, path)
}

// desktopFolderGUID is the Desktop known folder. Explorer stores the resolved
// location under this GUID; most installs also keep the legacy "Desktop" name.
const desktopFolderGUID = `{B4BFCC3A-DB2C-424C-B029-7FE99A87C641}`

// desktopDir returns the user's real Desktop folder.
//
// It deliberately does not assume %USERPROFILE%\Desktop: Known Folder Move
// relocates the desktop to OneDrive on most accounts, and then that path does
// not exist at all, so a shortcut built from it is silently skipped and the
// user gets no desktop icon. The registry value is what Explorer itself
// resolves, so read that instead.
func desktopDir() string {
	const key = `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`
	if k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE); err == nil {
		defer k.Close()
		for _, name := range []string{desktopFolderGUID, "Desktop"} {
			if v, _, err := k.GetStringValue(name); err == nil {
				if dir := resolveDesktop(v); dir != "" {
					return dir
				}
			}
		}
	}
	return resolveDesktop(filepath.Join(os.Getenv("USERPROFILE"), "Desktop"))
}

// resolveDesktop expands the environment variables Windows stores in shell
// folder values and returns the path only when it names a folder that exists.
func resolveDesktop(raw string) string {
	p := strings.TrimSpace(expandEnv(raw))
	if p == "" {
		return ""
	}
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	return ""
}

// envRef matches the %VAR% form these values use. Go's os.ExpandEnv only
// understands $VAR and ${VAR}, so using it here would leave the reference
// literal and the folder would look missing.
var envRef = regexp.MustCompile(`%([^%]+)%`)

// expandEnv resolves both Windows (%VAR%) and shell ($VAR) references.
func expandEnv(s string) string {
	s = envRef.ReplaceAllStringFunc(s, func(ref string) string {
		if v, ok := os.LookupEnv(ref[1 : len(ref)-1]); ok {
			return v
		}
		return ref
	})
	return os.Expand(s, os.Getenv)
}

// busyPort names the first of the given ports that already accepts
// connections, or "" when all are free.
func busyPort(ports ...int) string {
	for _, p := range ports {
		if p <= 0 {
			continue
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 500*time.Millisecond)
		if err != nil {
			continue
		}
		_ = conn.Close()
		return fmt.Sprintf("port %d", p)
	}
	return ""
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func jsString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`) + "'"
}

func openInstallLog(path string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil
	}
	return f
}

func logf(f *os.File, format string, args ...any) {
	if f == nil {
		return
	}
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
