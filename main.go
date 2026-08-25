// Command scalebridge-sync syncs Withings scale measurements to Garmin Connect.
// It runs a small local web server (setup wizard + dashboard) with a built-in
// scheduler; every other subcommand is a thin client of that server, falling
// back to doing the work in-process when nothing is listening.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kardianos/service"

	"github.com/ulfdalen/scalebridge-sync/internal/store"
	"github.com/ulfdalen/scalebridge-sync/internal/syncer"
	"github.com/ulfdalen/scalebridge-sync/internal/webui"
)

// Frozen: users register http://localhost:8723/callback with Withings, so a bad
// state file must never make us auto-pick a free port.
const defaultPort = 8723

const shutdownTimeout = 5 * time.Second

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	// The subcommand is the first non-flag argument, so --config works on either
	// side of it: flag stops parsing at the first non-flag word.
	cmd := ""
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		cmd, argv = argv[0], argv[1:]
	}

	defaultDir, _ := store.DefaultDir()
	fs := flag.NewFlagSet("scalebridge-sync", flag.ContinueOnError)
	fs.Usage = func() { usage(os.Stderr, defaultDir) }
	configDir := fs.String("config", defaultDir, "configuration directory")
	bindAddr := fs.String("bind", "127.0.0.1", "address the web UI listens on")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout, defaultDir)
			return 0
		}
		return 2
	}
	if cmd == "" && fs.NArg() > 0 {
		cmd = fs.Arg(0)
	}

	if *configDir == "" {
		fmt.Fprintln(os.Stderr, "cannot determine a configuration directory - pass --config <dir>")
		return 1
	}
	dir, err := filepath.Abs(*configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve --config %s: %v\n", *configDir, err)
		return 1
	}

	switch cmd {
	case "", "run":
		// Under launchd/systemd/SCM there is no terminal and no signal handling
		// of our own: kardianos drives start/stop instead.
		if !service.Interactive() {
			return runUnderServiceManager(dir)
		}
		return cmdRun(dir, *bindAddr)
	case "sync":
		return cmdSync(dir)
	case "install":
		return cmdInstall(dir)
	case "uninstall":
		return cmdUninstall(dir)
	case "start", "stop":
		return cmdControl(dir, cmd)
	case "status":
		return cmdStatus(dir)
	case "open":
		return cmdOpen(dir)
	case "version":
		fmt.Printf("scalebridge-sync %s (%s)\n", version, commit)
		return 0
	case "help":
		usage(os.Stdout, defaultDir)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr, defaultDir)
		return 2
	}
}

func usage(w *os.File, defaultDir string) {
	if defaultDir == "" {
		defaultDir = "unavailable"
	}
	fmt.Fprintf(w, `scalebridge-sync - sync Withings measurements to Garmin Connect

Usage:
  scalebridge-sync [command] [--config <dir>]

Commands:
  run          serve the web UI and sync on a schedule, in the foreground (default)
  sync         sync once now and exit (non-zero exit if the sync failed)
  install      register as an OS service so it starts at login/boot
  uninstall    remove the OS service
  start        start the installed service
  stop         stop the installed service
  status       report the service state and whether the web UI answers
  open         open the web UI in a browser
  version      print version and commit
  help         print this help

Flags:
  --config <dir>   configuration directory (default %s)
  --bind <addr>    address the web UI listens on (default 127.0.0.1; the UI has
                   no login, so bind another address only behind a firewall or
                   a container port mapping)
`, defaultDir)
}

// ── app: the server both "run" and the OS service drive ──

type app struct {
	st   *store.Store
	sy   *syncer.Syncer
	srv  *http.Server
	ln   net.Listener
	bind string
	port int
	url  string

	cancel   context.CancelFunc
	syncDone chan struct{}
	serveErr chan error
}

// openStore loads the state file and reports the port to use.
func openStore(dir string) (*store.Store, int, error) {
	st, err := store.Open(dir)
	if err != nil {
		return nil, 0, err
	}
	port := st.State.Settings.Port
	if port <= 0 || port > 65535 {
		port = defaultPort
	}
	return st, port, nil
}

func newApp(dir, bind string) (*app, error) {
	st, port, err := openStore(dir)
	if err != nil {
		return nil, err
	}
	sy := syncer.New(st)
	return &app{
		st:   st,
		sy:   sy,
		srv:  &http.Server{Handler: webui.New(st, sy, version).Handler()},
		bind: bind,
		port: port,
		url:  localURL(port),
	}, nil
}

func localURL(port int) string { return fmt.Sprintf("http://localhost:%d", port) }

// Something is already listening on our port - almost always another copy of
// this binary.
var errPortBusy = errors.New("port already in use")

func (a *app) listen() error {
	ln, err := net.Listen("tcp", net.JoinHostPort(a.bind, fmt.Sprint(a.port)))
	if err == nil {
		a.ln = ln
		return nil
	}
	// Probing beats matching errno: Windows reports WSAEADDRINUSE, which is not
	// syscall.EADDRINUSE, and what we want to know is whether a sibling instance
	// is already serving the UI.
	if dialable(a.port) {
		return errPortBusy
	}
	return err
}

func (a *app) start() {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	a.syncDone = make(chan struct{})
	go func() {
		defer close(a.syncDone)
		a.sy.Run(ctx)
	}()

	a.serveErr = make(chan error, 1)
	go func() { a.serveErr <- a.srv.Serve(a.ln) }()
}

func (a *app) stop() {
	a.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	a.srv.Shutdown(ctx)
	select {
	case <-a.syncDone:
	case <-time.After(2 * time.Second):
	}
}

// ── run ──

func cmdRun(dir, bind string) int {
	a, err := newApp(dir, bind)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := a.listen(); err != nil {
		if errors.Is(err, errPortBusy) {
			fmt.Printf("already running at %s - opening browser\n", a.url)
			browse(a.url)
			return 0
		}
		fmt.Fprintf(os.Stderr, "cannot listen on %s:%d: %v\n", a.bind, a.port, err)
		return 1
	}

	fmt.Printf("ScaleBridge Sync %s -> %s\n", version, a.url)
	a.start()

	// Only nudge a human at a terminal, and only into an unfinished wizard.
	if !a.st.State.SetupComplete && isTerminal() {
		browse(a.url)
	}

	sig, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	select {
	case err := <-a.serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "web server stopped: %v\n", err)
			return 1
		}
	case <-sig.Done():
		fmt.Println("shutting down")
		a.stop()
	}
	return 0
}

// ── sync ──

func cmdSync(dir string) int {
	st, port, err := openStore(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if code, handled := syncViaRunningInstance(port); handled {
		return code
	}

	sy := syncer.New(st)
	if !sy.TriggerSync() {
		fmt.Println("a sync is already running")
		return 0
	}
	// No deadline: this process owns the sync, and abandoning it mid-upload is
	// worse than waiting.
	for {
		s := sy.Status()
		if !s.Running {
			return reportSync(s.LastFetched, s.LastUploaded, s.LastError)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Returns handled=false when nothing is listening, so the caller can do the
// sync itself.
func syncViaRunningInstance(port int) (code int, handled bool) {
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	req, err := http.NewRequest(http.MethodPost, base+"/api/sync/now", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	req.Header.Set("X-ScaleBridge-Local", "1")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		fmt.Println("sync requested from the running instance")
		return waitForRemoteSync(base), true
	case http.StatusConflict:
		fmt.Println("a sync is already running")
		return 0, true
	default:
		fmt.Fprintf(os.Stderr, "the running instance refused the sync: HTTP %d\n", resp.StatusCode)
		return 1, true
	}
}

type remoteStatus struct {
	Version string `json:"version"`
	Sync    struct {
		Running      bool   `json:"running"`
		LastError    string `json:"last_error"`
		LastFetched  int    `json:"last_fetched"`
		LastUploaded int    `json:"last_uploaded"`
	} `json:"sync"`
}

func waitForRemoteSync(base string) int {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	for {
		st, err := fetchStatus(client, base)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lost contact with the running instance: %v\n", err)
			return 1
		}
		if !st.Sync.Running {
			return reportSync(st.Sync.LastFetched, st.Sync.LastUploaded, st.Sync.LastError)
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "sync is still running after 90s - check the dashboard")
			return 1
		}
		time.Sleep(time.Second)
	}
}

func fetchStatus(client *http.Client, base string) (remoteStatus, error) {
	var st remoteStatus
	resp, err := client.Get(base + "/api/status")
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("HTTP %d from /api/status", resp.StatusCode)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

func reportSync(fetched, uploaded int, syncErr string) int {
	if syncErr != "" {
		fmt.Fprintf(os.Stderr, "sync failed: %s\n", syncErr)
		return 1
	}
	fmt.Printf("sync finished: %d fetched, %d uploaded\n", fetched, uploaded)
	return 0
}

// ── open ──

func cmdOpen(dir string) int {
	_, port, err := openStore(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		fmt.Println("not running - start it with: scalebridge-sync start (or just: scalebridge-sync)")
		return 1
	}
	resp.Body.Close()
	browse(localURL(port))
	return 0
}

// ── helpers ──

func browse(url string) {
	if err := openBrowser(url); err != nil {
		fmt.Printf("could not open a browser (%v) - open %s yourself\n", err, url)
	}
}

func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func dialable(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
