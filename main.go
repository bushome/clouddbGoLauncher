// Command clouddbgo-launcher is the solo-player/SQLite portable launcher for
// ark-cloud-storage-no-overflow. It embeds a portable Node.js runtime plus the
// compiled SQLite-only app tree (see clouddbGo in CLAUDE.md), extracts them
// next to the exe on first run, generates a minimal config.json if one isn't
// already present, and then hands off supervision entirely to the existing,
// already-tested watchdog.js.
//
// Design decisions this file implements (see CLAUDE.md "Go-Launcher" and
// "Go-Launcher Foundation" sections for the full rationale — not repeated
// here as comments beyond a one-line pointer per decision):
//   - Extraction happens next to the exe, not %LOCALAPPDATA%.
//   - Re-extraction is version-marker gated, not every-launch.
//   - Config generation only fires on a missing/empty Auth.RegisterClusters,
//     and never overwrites a populated or malformed config.json.
//   - The Go stub does NOT reimplement crash/backoff logic — it spawns
//     watchdog.js under the extracted Node runtime and waits.
//
// Additionally: every run mirrors its console output to launcher.log next to
// the exe, and — on any exit the program makes on its own (an error, or
// watchdog.js itself giving up) — prints a clear banner explaining that
// before pausing for acknowledgment, rather than letting the window vanish
// unexplained. This deliberately does NOT try to intercept the console's own
// X-button close: Windows force-kills the process before Go code can react
// to that, and a player closing the window on purpose doesn't need to
// confirm it anyway. Added after a double-click test run crashed (empty
// node_modules from a payload assembled before running sync-payload.cmd)
// and closed before the error was readable.
//
// Payload layout expected under ./payload at build time (see BUILD.md):
//
//	payload/
//	  node/            portable Node.js runtime for Windows x64 (node.exe + deps)
//	  app/
//	    dist/
//	    generated/sqlite-client/
//	    node_modules/
//	    watchdog/
//	    config.json.example
package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

//go:embed all:payload
var payloadFS embed.FS

// embeddedVersion must be bumped any time payload/ contents change, so that
// existing installs re-extract instead of running stale files forever.
// sync-payload.cmd bumps this automatically — see BUILD.md.
const embeddedVersion = "0.1.0"

const versionMarkerFilename = ".payload-version"
const logFilename = "launcher.log"

// out is where all launcher-authored output goes: console plus launcher.log,
// once setupLogging succeeds. Falls back to stdout-only if the log file
// can't be opened (e.g. a read-only extraction folder), since a launcher
// that can't log shouldn't also refuse to run.
var out io.Writer = os.Stdout

func main() {
	exitCode := run()
	pauseBeforeExit(exitCode)
	os.Exit(exitCode)
}

// run contains the actual launcher sequence and returns an exit code rather
// than calling os.Exit directly, so main() can guarantee the pause-before-
// exit behavior fires on every path — success, handled failure, or panic
// recovery.
func run() (exitCode int) {
	defer func() {
		if r := recover(); r != nil {
			logf("PANIC: %v", r)
			exitCode = 1
		}
	}()

	exeDir, err := exeDir()
	if err != nil {
		// Can't resolve our own directory — no sensible place to put a log
		// file either, so this one genuinely is console (+ pause) only.
		fmt.Fprintf(os.Stderr, "Could not determine own executable path: %v\n", err)
		return 1
	}

	closeLog := setupLogging(exeDir)
	defer closeLog()

	appDir := filepath.Join(exeDir, "app")
	markerPath := filepath.Join(exeDir, versionMarkerFilename)

	if needsExtraction(exeDir, appDir, markerPath) {
		logf("First run (or updated build) detected — extracting application files...")
		if err := extractPayload(exeDir); err != nil {
			logf("ERROR: failed to extract embedded payload: %v", err)
			return 1
		}
		if err := os.WriteFile(markerPath, []byte(embeddedVersion), 0644); err != nil {
			logf("ERROR: failed to write version marker: %v", err)
			return 1
		}
		logf("Extraction complete.")
	}

	if err := ensureConfig(filepath.Join(appDir, "config.json")); err != nil {
		logf("ERROR: failed to prepare config.json: %v", err)
		return 1
	}

	if err := ensureWatchdogConfig(appDir); err != nil {
		logf("ERROR: failed to prepare watchdog-config.json: %v", err)
		return 1
	}

	if err := launchWatchdog(exeDir, appDir); err != nil {
		logf("ERROR: %v", err)
		return 1
	}

	// A clean return from launchWatchdog means the watchdog process itself
	// exited on its own (not killed by the user) — that's unexpected during
	// normal operation, since the watchdog is supposed to run indefinitely.
	// pauseBeforeExit's banner covers explaining this to the player; nothing
	// more to add here.
	return 0
}

// setupLogging opens launcher.log next to the exe (truncating any previous
// run's log) and points `out` at both it and the console. Returns a closer
// to defer. If the log file can't be opened, `out` stays console-only and a
// warning is printed — logging failure should degrade, not block startup.
func setupLogging(exeDir string) (closer func()) {
	logPath := filepath.Join(exeDir, logFilename)
	f, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not open %s for logging: %v\n", logPath, err)
		return func() {}
	}
	out = io.MultiWriter(os.Stdout, f)
	return func() { _ = f.Close() }
}

func logf(format string, args ...interface{}) {
	fmt.Fprintf(out, format+"\n", args...)
}

// pauseBeforeExit keeps the console window open until the user acknowledges
// it, instead of letting Windows close it the instant the process exits.
//
// This only ever runs on a VOLUNTARY exit — run() returning on its own,
// whether from a handled error or from watchdog.js itself exiting. Windows
// force-kills the process directly on a console-close (X button) before any
// Go code gets a chance to run at all, so this deliberately does not try to
// cover that case — a player closing the window on purpose already knows
// why and shouldn't be asked to confirm it.
//
// Every path that reaches here is inherently the ABNORMAL case: watchdog.js
// is designed to run indefinitely and recovers from ordinary child crashes
// internally (see the native Node crash CLAUDE.md documents watchdog
// surviving without ever reaching this code) — reaching this function at
// all means either a real error occurred, or watchdog itself gave up after
// repeated restart failures (its own crash-count/backoff limit), which is
// exactly the kind of thing worth a config fix or a bug report. The banner
// below says so explicitly and points at launcher.log, rather than a bare
// "press Enter" that gives no hint whether anything is actually wrong.
func pauseBeforeExit(exitCode int) {
	logf("")
	logf("========================================")
	if exitCode == 0 {
		logf("The watchdog exited on its own — this shouldn't normally happen")
		logf("while the server is meant to be running.")
	} else {
		logf("The launcher is exiting because of a problem.")
	}
	logf("See the messages above for details.")
	logf("A copy of this output is also saved in launcher.log, next to this")
	logf("exe, in case the window closes before you're done reading it.")
	logf("If this looks like a bug rather than something in config.json,")
	logf("launcher.log is exactly what's useful to include when reporting it.")
	logf("========================================")
	logf("Press Enter to close this window...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

// exeDir resolves the directory the exe itself lives in, following symlinks.
// Everything (extraction target, node/, app/, launcher.log) is anchored
// here, matching every other deployment target's "config.json sits alongside
// the app" convention (see CLAUDE.md Config.json Path Resolution section).
func exeDir() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	return filepath.Dir(exePath), nil
}

// needsExtraction is true when the version marker is missing or stale, OR
// when a couple of files extraction is supposed to have produced are
// themselves missing. The marker alone isn't sufficient — it only proves a
// PREVIOUS run's extraction completed, not that the extracted files are
// still actually there right now. Confirmed as a real gap, not just
// theoretical: deleting everything under app\ by hand while leaving
// .payload-version untouched left the marker "valid," so this used to skip
// re-extraction entirely and fail later inside launchWatchdog with a
// confusing "not found" error instead of just fixing itself. Spot-checks
// the same two files launchWatchdog itself requires, rather than walking
// the whole tree — cheap, and those two are exactly the ones whose absence
// would otherwise surface as a launchWatchdog failure instead of a clean
// re-extraction.
func needsExtraction(exeDir, appDir, markerPath string) bool {
	data, err := os.ReadFile(markerPath)
	if err != nil || string(data) != embeddedVersion {
		return true
	}

	criticalPaths := []string{
		filepath.Join(exeDir, "node", "node.exe"),
		filepath.Join(appDir, "watchdog", "watchdog.js"),
	}
	for _, p := range criticalPaths {
		if _, err := os.Stat(p); err != nil {
			return true
		}
	}

	return false
}

// extractPayload walks the embedded payload/ tree and writes it out under
// destRoot, preserving the payload/node -> destRoot/node and
// payload/app -> destRoot/app structure.
//
// Note: this can only extract whatever was actually embedded at build time.
// If payload/app/node_modules (or any other subfolder) only contained a
// placeholder file when `go build` ran, that placeholder is exactly what
// gets extracted here — this function has no way to detect that the payload
// it was given is incomplete. Run sync-payload.cmd before every `go build`
// to avoid shipping a stale or placeholder-only payload (see BUILD.md).
func extractPayload(destRoot string) error {
	return fs.WalkDir(payloadFS, "payload", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("payload", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(destRoot, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		data, err := payloadFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

// ensureConfig generates a minimal config.json with a random ClusterId/Secret
// ONLY if config.json doesn't exist yet, or exists but Auth.RegisterClusters
// is empty/absent. It never regenerates once populated — ClusterId/Secret
// must stay stable across relaunches (see CLAUDE.md "Config generation"
// decision: the player pastes these once into their own
// GameUserSettings.ini — a single game client here, not a server cluster).
// The pasteable [CloudStorage] block is printed on EVERY launch, not just
// generation, since most players will never open config.json themselves —
// see the reprint branch below for why.
//
// Note config GENERATION specifically is a first-run UX nicety, not a
// requirement for the app to boot at all — self-registration via
// POST /auth/register already works against a genuinely empty
// Auth.RegisterClusters (see CLAUDE.md's 2026-09-05 note under Go-Launcher
// "Decisions locked in"). If config.json already exists with real (or
// intentionally empty) values, or is malformed, this function leaves the
// file itself alone and lets the app's own AppConfigDto validation surface
// the real error — it may still print the reminder block, though.
func ensureConfig(configPath string) error {
	existing, readErr := os.ReadFile(configPath)

	cfg := map[string]interface{}{}
	needsGeneration := readErr != nil

	if readErr == nil {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			// Malformed JSON: don't touch it. The app's own validation will
			// give a clearer error than anything we'd produce here.
			logf("config.json exists but isn't valid JSON — leaving it untouched.")
			return nil
		}
		auth, ok := cfg["Auth"].(map[string]interface{})
		if !ok {
			needsGeneration = true
		} else if clusters, ok := auth["RegisterClusters"].([]interface{}); !ok || len(clusters) == 0 {
			needsGeneration = true
		}
	}

	if needsGeneration {
		clusterId := randomHex(8)
		secret := randomHex(24)

		cfg["Auth"] = map[string]interface{}{
			"RegisterClusters": []map[string]string{
				{"ClusterId": clusterId, "Secret": secret},
			},
		}

		cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return fmt.Errorf("building generated config.json: %w", err)
		}
		if err := os.WriteFile(configPath, cfgBytes, 0644); err != nil {
			return fmt.Errorf("writing config.json: %w", err)
		}

		logf("")
		logf("First run: generated cluster credentials.")
		printCloudStorageBlock(clusterId, secret, resolvePort(cfg))
		return nil
	}

	// Not a fresh generation — config.json already has a real bootstrap
	// cluster. Most players will never open config.json themselves, so
	// this reprints the same pasteable block on EVERY launch, not just the
	// first — otherwise a player who loses their GameUserSettings.ini's
	// [CloudStorage] section (reinstalling ARK, verifying game files,
	// moving to a new PC) would have no easy way back to these values
	// without knowing to go find and read a JSON file they've never seen.
	// Silently does nothing if config.json's structure doesn't match what
	// we'd expect (e.g. a hand-edited or unusual entry) — this is a
	// convenience, not something worth failing boot over.
	if clusterId, secret, ok := firstBootstrapCluster(cfg); ok {
		logf("")
		logf("Your cluster credentials (already in config.json — shown every launch as a reminder):")
		printCloudStorageBlock(clusterId, secret, resolvePort(cfg))
	}
	return nil
}

// resolvePort reads config.json's Server.Port if present, falling back to
// AppConfigDto's own documented zero-config default. Given most players
// never touch config.json at all, this will almost always resolve to the
// default in practice.
func resolvePort(cfg map[string]interface{}) int {
	if server, ok := cfg["Server"].(map[string]interface{}); ok {
		if p, ok := server["Port"].(float64); ok { // JSON numbers decode as float64
			return int(p)
		}
	}
	return 3000
}

// firstBootstrapCluster extracts the ClusterId/Secret of config.json's
// first Auth.RegisterClusters entry, if present and well-formed.
func firstBootstrapCluster(cfg map[string]interface{}) (clusterId, secret string, ok bool) {
	auth, ok := cfg["Auth"].(map[string]interface{})
	if !ok {
		return "", "", false
	}
	clusters, ok := auth["RegisterClusters"].([]interface{})
	if !ok || len(clusters) == 0 {
		return "", "", false
	}
	entry, ok := clusters[0].(map[string]interface{})
	if !ok {
		return "", "", false
	}
	clusterId, idOk := entry["ClusterId"].(string)
	secret, secretOk := entry["Secret"].(string)
	if !idOk || !secretOk {
		return "", "", false
	}
	return clusterId, secret, true
}

// printCloudStorageBlock prints a directly pasteable [CloudStorage] section
// matching GameUserSettings.ini's real format, so a player never has to
// reconstruct it by hand from separately labeled values. Safe to include
// the raw secret here — launcher.log persists this on disk regardless of
// whether the console window itself stays open.
func printCloudStorageBlock(clusterId, secret string, port int) {
	logf("Paste this into your GameUserSettings.ini:")
	logf("")
	logf("[CloudStorage]")
	logf("ID=%s", clusterId)
	logf("Secret=%s", secret)
	logf("URL=\"ws://127.0.0.1:%d\"", port)
	logf("")
}

func randomHex(n int) string {
	const chars = "0123456789abcdef"
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}

// ensureWatchdogConfig rewrites the extracted watchdog/watchdog-config.json's
// appDir to this machine's actual extracted app path every launch.
//
// watchdog-config.json's appDir is documented in CLAUDE.md as "an absolute,
// environment-specific path" that each real deployment configures by hand —
// that's fine for clouddb/clouddbSEA, which live at a fixed, known install
// location. It doesn't hold for a portable launcher a player can drop and
// run from anywhere, so the Go stub owns keeping this field correct instead
// of shipping a static value. isExe/appEntry are also forced here so the
// shipped watchdog-config.json.example can stay a template without needing
// hand-editing.
func ensureWatchdogConfig(appDir string) error {
	path := filepath.Join(appDir, "watchdog", "watchdog-config.json")

	cfg := map[string]interface{}{}
	if existing, err := os.ReadFile(path); err == nil {
		// Preserve any operator-tunable fields (backoff, health endpoint,
		// crash-window) already present; only appDir/isExe/appEntry are
		// forced below.
		_ = json.Unmarshal(existing, &cfg)
	}

	cfg["appDir"] = appDir
	cfg["isExe"] = false
	cfg["appEntry"] = "dist/main.js"

	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("building watchdog-config.json: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating watchdog directory: %w", err)
	}
	if err := os.WriteFile(path, cfgBytes, 0644); err != nil {
		return fmt.Errorf("writing watchdog-config.json: %w", err)
	}
	return nil
}

// launchWatchdog spawns the already-built, already-tested watchdog.js under
// the extracted portable Node runtime, and waits for it. Per CLAUDE.md's
// "Supervision" decision, the Go stub deliberately does not reimplement
// crash-detection/backoff itself — watchdog.js already does this correctly
// and has zero third-party dependencies, so reusing it avoids a second,
// parallel implementation of the same logic in a different language.
func launchWatchdog(exeDir, appDir string) error {
	nodeDir := filepath.Join(exeDir, "node")
	nodeExe := filepath.Join(nodeDir, "node.exe")
	watchdogScript := filepath.Join(appDir, "watchdog", "watchdog.js")

	if _, err := os.Stat(nodeExe); err != nil {
		return fmt.Errorf("bundled Node runtime not found at %s: %w", nodeExe, err)
	}
	if _, err := os.Stat(watchdogScript); err != nil {
		return fmt.Errorf("watchdog.js not found at %s: %w", watchdogScript, err)
	}

	cmd := exec.Command(nodeExe, watchdogScript)
	cmd.Dir = appDir
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = os.Stdin

	// watchdog.js's launchChild() spawns its child via the bare command
	// 'node' (not an absolute path) when isExe is false — see CLAUDE.md's
	// "isExe spawn mode" section. That relies on 'node' resolving on PATH,
	// which won't be true for a portable install with no system-wide Node.
	// Prepending our bundled node/ directory to PATH here makes watchdog.js's
	// own child spawn resolve to the bundled runtime with zero end-user
	// install step, preserving the "download one file, run it" promise.
	cmd.Env = append(os.Environ(), "PATH="+nodeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("watchdog exited with error: %w", err)
	}
	return nil
}
