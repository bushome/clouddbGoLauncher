# clouddbGo Launcher — Build Instructions

This is the Go source for the solo-player/SQLite portable launcher described
in CLAUDE.md's "Go-Launcher" sections. Two other trees are prerequisites:

- `C:\Dev\clouddbGo\` — build this first if you haven't already
  (`npm install`, `npm run prisma:generate`, `npm run build`).
- `C:\Dev\ark-cloud-storage-no-overflow\` — the dev-tree repo, needed only
  for its `watchdog\` folder (see step 2b below — `watchdog\` is never
  symlinked into `clouddbGo`, so `sync-payload.cmd` pulls it from here
  directly).

Suggested location for this Go source: a new sibling folder,
`C:\Dev\clouddbGoLauncher\`, so it doesn't get tangled in `clouddbGo`'s own
`src`/`prisma`/`scripts` symlink set — this tree has no reason to symlink
anything, since it never touches TypeScript source directly, only
`clouddbGo`'s **build output**.

## 1. Install Go

Download the Windows installer from https://go.dev/dl/ (go1.22+), run it,
then confirm in a new PowerShell window:

```powershell
go version
```

## 2. Assemble the payload folder

Every subfolder under `payload\` currently contains a placeholder
`PLACE_..._HERE.txt` file — the first run of `sync-payload.cmd` (step 2b
below) clears these out via `/MIR`, so you don't need to delete them by
hand. `go:embed` will refuse to build if a `payload\` subfolder is
completely empty, so don't manually delete a placeholder without something
already in its place.

### 2a. Portable Node runtime (one-time, manual — but read this before picking a version)

Download `node-vX.Y.Z-win-x64.zip` from nodejs.org, extract it, and copy its
**contents** into `payload\node\` — `node.exe` should end up directly in
`payload\node\`, not nested one level deeper. **This step must happen
before step 2b** — `sync-payload.cmd` now depends on `payload\node\node.exe`
already existing.

**Whichever version you pick, re-run step 2b (`sync-payload.cmd`) any time
you change it** — swapping `node.exe` alone is not sufficient. See the
`CRITICAL` comment in `sync-payload.cmd`'s node_modules section for the full
story, but in short: `better-sqlite3` uses Node's legacy `node::ObjectWrap`
C++ pattern, which is not covered by Node's stable-ABI (N-API) guarantee —
a compiled `better-sqlite3` binary is only reliably compatible with the
exact Node build it was compiled against, not just "any Node with a
matching `NODE_MODULE_VERSION`." `sync-payload.cmd` now builds
`node_modules` using this exact `payload\node\node.exe`, specifically to
keep the compiled native binary matched to the runtime that will actually
load it — discovered the hard way after a native crash
(`RemoveEnvironmentCleanupHook` assertion failure) that persisted
identically across several different bundled Node versions, because none
of those earlier tests had actually changed what the native binary was
built against.

### 2b. Compiled app + production node_modules (every rebuild)

Run `sync-payload.cmd` (in this same folder) after every `npm run build` in
`clouddbGo`:

```powershell
cd C:\Dev\clouddbGoLauncher
.\sync-payload.cmd
```

This copies `dist\` and `generated\sqlite-client\` from `clouddbGo`,
`watchdog\` from the **dev-tree repo root**
(`C:\Dev\ark-cloud-storage-no-overflow\watchdog` — not `clouddbGo`, since
`watchdog\` is never symlinked into any tree and only ever exists at the
dev-tree repo root or hand-copied into a deployment target, per the same
convention `clouddb`/`clouddbSEA` already follow), and builds a
production-only `node_modules` in a **separate scratch staging folder**
(`.prod-modules-staging`, inside the launcher tree) before copying that into
`payload\app\node_modules`.

**Note on how `sync-payload.cmd` handles `node_modules`:** it never runs
`npm ci --omit=dev` inside `C:\Dev\clouddbGo` itself — only against a copy
of `package.json`/`package-lock.json` in its own scratch staging folder.
Running `--omit=dev` directly in `clouddbGo` would strip devDependencies
from your actual dev tree in place (`@nestjs/cli` and friends are
devDependencies), breaking your ability to run `npm run build` there again
until you `npm install` to restore them. Staging in a separate folder avoids
that entirely — `clouddbGo`'s own `node_modules` is never touched by this
script.

`sync-payload.cmd` also bumps `main.go`'s `embeddedVersion` to a fresh
timestamp automatically on every run — you never need to edit that constant
by hand. (If you ever assemble the payload manually instead of via the
script, remember to bump `embeddedVersion` yourself; existing installs only
re-extract when that string changes, per the version-marker design in
CLAUDE.md.)

**Do not commit a real `config.json`** into `payload\app\` — leave that
folder for `main.go`'s `ensureWatchdogConfig`/`ensureConfig` to populate at
runtime on the player's machine. If you want a template for reference, name
it `config.json.example`, not `config.json`.

## 3. Build

```powershell
cd C:\Dev\clouddbGoLauncher
go build -o clouddbgo-launcher.exe .
```

This produces a single `clouddbgo-launcher.exe`. Everything under `payload\`
is compiled directly into it — nothing else needs to ship alongside it for
distribution.

## 4. Test

Copy `clouddbgo-launcher.exe` alone into an empty scratch folder (deliberately
not the source tree, to prove it's not silently reading anything from
`payload\` at runtime) and run it:

```powershell
cd C:\Users\<you>\Desktop\launcher-test
.\clouddbgo-launcher.exe
```

Expected on first run:
1. `First run (or updated build) detected — extracting application files...`
2. `node\` and `app\` appear alongside the exe, plus a `.payload-version`
   marker file and a `launcher.log` file.
3. `First run: generated cluster credentials.` with a ClusterId/Secret
   printed (only if you didn't ship a pre-populated `config.json.example`
   that would've been copied in as `config.json` — if `payload\app\` had no
   `config.json` at all, this always fires).
4. Watchdog output showing it launching `node dist/main.js`, followed by a
   normal Nest boot log — same `Database: SQLite at ...` /
   `SQLite database is uninitialized — applying initial schema.` lines you
   already saw testing `clouddbGo` directly.
5. The window stays open with a `Press Enter to close this window...`
   prompt rather than closing on its own — this fires on every exit path
   (clean or crashed), so a double-click launch always gives you a chance to
   read what happened. `launcher.log` next to the exe has the same output
   for later reference, in case the window did get closed before you could
   read it.

If the app crashes immediately after the watchdog starts it (e.g. an
incomplete payload — see the node_modules note in step 2b above), you'll now
see the actual error instead of a vanishing window; check `launcher.log` if
you missed it.

Kill it, relaunch, and confirm:
- No re-extraction happens (marker matches).
- ClusterId/Secret are unchanged (config.json already populated).
- `POST /auth/register` against the port in `config.json` still works if you
  want to double check the DB itself survived.

## Known gaps not yet handled by this version

These are deliberately out of scope for the first working version — flagging
them so they don't get mistaken for oversights when you review the code:

- **No graceful shutdown handling.** Closing the console window will hit the
  same "not equivalent to a clean stop" behavior documented in CLAUDE.md's
  watchdog section — `cmd.Run()` here doesn't intercept `SIGINT`/console
  close events specially. Worth revisiting once the exe is closer to real
  distribution.
- **No `--target=node24`-equivalent version pin check.** If a player's
  extracted `node\` folder ever gets partially overwritten by something
  else on their machine, nothing here re-validates it beyond the version
  marker (which only tracks *your* payload version, not the Node binary's
  own integrity).
- **SQLite Resilience (WAL mode, `PRAGMA quick_check`, backup/restore) lives
  in the NestJS app itself** per CLAUDE.md's design ("keeps the stub's job
  narrow"), not in this Go code — nothing to add here for that.
