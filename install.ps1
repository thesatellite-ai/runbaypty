# runbaypty installer for Windows (PowerShell).
#
# Usage:
#   irm https://raw.githubusercontent.com/thesatellite-ai/runbaypty/main/install.ps1 | iex
#
# Downloads the latest released binary from GitHub Releases, installs it
# to %LOCALAPPDATA%\runbaypty, adds that dir to the user PATH, and

$ErrorActionPreference = "Stop"

$Repo    = "thesatellite-ai/runbaypty"
$Binary  = "runbaypty"
$InstallDir = Join-Path $env:LOCALAPPDATA "runbaypty"

$arch = if ([Environment]::Is64BitOperatingSystem) {
  if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
} else { throw "Unsupported architecture" }

$asset = "${Binary}_windows_${arch}.zip"
# /releases/latest/download/ redirects to the newest asset with no call
# to the rate-limited GitHub API.
$url   = "https://github.com/$Repo/releases/latest/download/$asset"

Write-Host "Downloading latest $Binary for windows/$arch..."
$tmp = Join-Path $env:TEMP ([System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
$zip = Join-Path $tmp "$asset"
Invoke-WebRequest -Uri $url -OutFile $zip
Expand-Archive -Path $zip -DestinationPath $tmp -Force

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item (Join-Path $tmp "$Binary.exe") (Join-Path $InstallDir "$Binary.exe") -Force

# Add install dir to user PATH if missing.
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$InstallDir*") {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$InstallDir", "User")
  Write-Host "Added $InstallDir to user PATH (restart your shell)."
}

}

Remove-Item -Recurse -Force $tmp

Write-Host ""
Write-Host "Installed. Next steps:"
Write-Host "  cd <your-project>"
Write-Host "  $Binary version              # verify the install"
Write-Host "  $Binary setup                # build FTS5 search index"
Write-Host ""
Write-Host "Verify: $Binary version"
