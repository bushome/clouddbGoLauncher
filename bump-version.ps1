# Sets main.go's embeddedVersion to a fresh timestamp so installed copies of
# the Solo launcher re-extract their payload on next launch. Called by
# sync-payload.cmd; kept as its own file because cmd mangles inline regexes.
param([Parameter(Mandatory)][string]$MainGo)
$ErrorActionPreference = 'Stop'
if (-not (Test-Path -LiteralPath $MainGo)) { Write-Error "main.go not found at $MainGo"; exit 1 }
$pattern = 'const embeddedVersion = "[^"]*"'
$content = [IO.File]::ReadAllText($MainGo)
if ($content -notmatch $pattern) { Write-Error 'embeddedVersion const not found in main.go'; exit 1 }
$ts = Get-Date -Format 'yyyyMMdd-HHmmss'
$content = [regex]::Replace($content, $pattern, "const embeddedVersion = `"$ts`"", 1)
[IO.File]::WriteAllText($MainGo, $content, (New-Object Text.UTF8Encoding($false)))
Write-Host "  embeddedVersion set to $ts"
