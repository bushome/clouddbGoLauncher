@echo off
REM sync-payload.cmd
REM
REM Copies clouddbGo's build output (dist, generated sqlite-client, watchdog,
REM production-only node_modules) into the Go launcher's payload\ folder, and
REM bumps main.go's embeddedVersion so existing installs re-extract on next
REM launch. Run this after every `npm run build` in clouddbGo, before
REM `go build`.
REM
REM go:embed does not follow symlinks/junctions (see golang/go#46311), so
REM these have to be real copies, not the symlink pattern used elsewhere in
REM this project (clouddbSEA/clouddbGo's src\/prisma\/scripts\).
REM
REM Edit the two paths below if your trees live somewhere else.

setlocal

set "CLOUDDBGO=C:\Dev\clouddbGo"
set "DEVTREE=C:\Dev\ark-cloud-storage-no-overflow"
set "LAUNCHER=C:\Dev\clouddbGoLauncher"
set "PAYLOAD=%LAUNCHER%\payload"
set "STAGING=%LAUNCHER%\.prod-modules-staging"

if not exist "%CLOUDDBGO%\dist" (
    echo ERROR: %CLOUDDBGO%\dist not found. Run "npm run build" in clouddbGo first.
    exit /b 1
)

if not exist "%DEVTREE%\watchdog" (
    echo ERROR: %DEVTREE%\watchdog not found.
    exit /b 1
)

echo.
echo === Syncing compiled app into payload ===

robocopy "%CLOUDDBGO%\dist" "%PAYLOAD%\app\dist" /MIR
if %ERRORLEVEL% GEQ 8 goto :robocopy_failed

robocopy "%CLOUDDBGO%\generated\sqlite-client" "%PAYLOAD%\app\generated\sqlite-client" /MIR
if %ERRORLEVEL% GEQ 8 goto :robocopy_failed

REM watchdog/ is never symlinked into clouddbGo (only src\/prisma\/scripts\
REM are, per the existing clouddbSEA/clouddbGo convention) — it's plain JS,
REM manually copied into each deployment target from the dev-tree repo root.
REM Sourcing it directly from there avoids needing a one-time manual copy
REM into clouddbGo that's easy to forget and easy to let go stale.
robocopy "%DEVTREE%\watchdog" "%PAYLOAD%\app\watchdog" /MIR
if %ERRORLEVEL% GEQ 8 goto :robocopy_failed

echo.
echo === Building production-only node_modules ===
REM Staged in a scratch folder rather than run in clouddbGo directly, so this
REM never touches clouddbGo's own dev node_modules (which you still want
REM eslint/jest/etc in for day-to-day dev work).

if not exist "%STAGING%" mkdir "%STAGING%"
copy /Y "%CLOUDDBGO%\package.json" "%STAGING%\package.json" >nul
copy /Y "%CLOUDDBGO%\package-lock.json" "%STAGING%\package-lock.json" >nul

pushd "%STAGING%"
call npm ci --omit=dev
if errorlevel 1 (
    echo ERROR: npm ci --omit=dev failed. Aborting.
    popd
    exit /b 1
)
popd

robocopy "%STAGING%\node_modules" "%PAYLOAD%\app\node_modules" /MIR
if %ERRORLEVEL% GEQ 8 goto :robocopy_failed

echo.
echo === Bumping embeddedVersion in main.go ===

powershell -NoProfile -Command ^
    "$ts = Get-Date -Format 'yyyyMMdd-HHmmss'; " ^
    "$path = Join-Path '%LAUNCHER%' 'main.go'; " ^
    "if (-not (Test-Path $path)) { Write-Error \"main.go not found at $path\"; exit 1 }; " ^
    "$content = Get-Content $path -Raw; " ^
    "if ($content -notmatch 'const embeddedVersion = \"[^\"]*\"') { Write-Error 'embeddedVersion const not found in main.go'; exit 1 }; " ^
    "$content = $content -replace 'const embeddedVersion = \"[^\"]*\"', ('const embeddedVersion = \"' + $ts + '\"'); " ^
    "Set-Content -Path $path -Value $content -NoNewline; " ^
    "Write-Host ('  embeddedVersion set to ' + $ts)"

if errorlevel 1 (
    echo ERROR: version bump step failed. Check main.go's embeddedVersion line by hand.
    exit /b 1
)

echo.
echo === Done ===
echo Review the diff on main.go, then:
echo   cd /d "%LAUNCHER%"
echo   go build -o clouddbgo-launcher.exe .

endlocal
exit /b 0

:robocopy_failed
echo.
echo ERROR: robocopy reported a failure (exit code %ERRORLEVEL%^). Aborting.
exit /b 1
