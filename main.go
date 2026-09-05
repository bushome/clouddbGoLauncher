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
// the exe, and pauses on exit rather than letting the console window vanish
// instantly — added after a double-click test run crashed (empty
// node_modules from a payload assembled before running sync-payload.cmd) and
// closed before the error was readable. A solo player double-clicking this
// with no console experience needs a window that stays open and a log file
// that survives after they close it, on ANY exit path, not just this one.
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
	pauseBeforeExit()
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

	if needsExtraction(markerPath) {
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
	// normal operation, since the watchdog is supposed to run indefinitely,
	// so it's worth calling out rather than silently returning success.
	logf("watchdog exited on its own. Check the output above for details.")
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
// it, instead of letting Windows close it the instant the process exits —
// the actual bug this whole change addresses. Always runs, on every exit
// path, since a double-click launch has no other way to see what happened.
func pauseBeforeExit() {
	fmt.Fprintln(out, "\nPress Enter to close this window...")
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

// needsExtraction is true when the version marker is missing or stale.
// A missing marker covers both "genuinely first run" and "someone deleted
// app/ or node/ by hand" — either way, re-extracting is the safe response.
func needsExtraction(markerPath string) bool {
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return true
	}
	return string(data) != embeddedVersion
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
// decision: the player pastes these once into GameUserSettings.ini).
//
// Note this is a first-run UX nicety, not a requirement for the app to boot
// at all — self-registration via POST /auth/register already works against
// a genuinely empty Auth.RegisterClusters (see CLAUDE.md's 2026-09-05 note
// under Go-Launcher "Decisions locked in"). If config.json already exists
// with real (or intentionally empty) values, or is malformed, this function
// leaves it alone and lets the app's own AppConfigDto validation surface the
// real error.
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

	if !needsGeneration {
		return nil
	}

	clusterId := randomHex(8)
	secret := randomHex(24)

	if _, ok := cfg["UseMySQL"]; !ok {
		cfg["UseMySQL"] = false
	}
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
	logf("  ClusterId: %s", clusterId)
	logf("  Secret:    (see Auth.RegisterClusters in app/config.json)")
	logf("Paste both into GameUserSettings.ini on each server in your cluster.")
	logf("")
	return nil
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
