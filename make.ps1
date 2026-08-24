# PowerShell front end for the Makefile targets.
#
# GNU make on Windows resolves recipes through /bin/sh, and the sh on this
# machine's PATH is WSL's, which does not share the Windows filesystem view.
# Rather than fight that, this script implements the same targets natively.
#
#   .\make.ps1 build
#   .\make.ps1 test
#   .\make.ps1 run
#   .\make.ps1 cross

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('build', 'test', 'test-race', 'cover', 'vet', 'lint', 'fmt', 'tidy',
        'check', 'run', 'dev', 'cross', 'web', 'web-install', 'bench-mem', 'plugin', 'plugin-10', 'plugin-12', 'plugin-test', 'test-all', 'clean', 'help')]
    [string]$Target = 'build'
)

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$Binary = 'reelay'
$Pkg = './cmd/reelay'
$Module = 'github.com/TechXTT/reelay'
$DotnetCommand = Get-Command dotnet -ErrorAction SilentlyContinue
$Dotnet = if ($DotnetCommand) { $DotnetCommand.Source } else { $null }
$LocalDotnet = Join-Path $env:LOCALAPPDATA 'Microsoft\dotnet10\dotnet.exe'
if ((-not $Dotnet -or -not (& $Dotnet --list-sdks 2>$null | Select-String '^10\.')) -and (Test-Path $LocalDotnet)) {
    $Dotnet = $LocalDotnet
}

function Get-GitValue([string[]]$GitArgs, [string]$Fallback) {
    try {
        $v = & git @GitArgs 2>$null
        if ($LASTEXITCODE -eq 0 -and $v) { return ($v | Select-Object -First 1).Trim() }
    }
    catch { }
    return $Fallback
}

$Version = Get-GitValue @('describe', '--tags', '--always', '--dirty') 'dev'
$Commit = Get-GitValue @('rev-parse', '--short', 'HEAD') 'none'
$Date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$PluginVersion = if ($Version -match '^v?(\d+\.\d+\.\d+)') { $Matches[1] } else { '0.1.0' }

$LdFlags = "-s -w " +
"-X $Module/internal/buildinfo.Version=$Version " +
"-X $Module/internal/buildinfo.Commit=$Commit " +
"-X $Module/internal/buildinfo.Date=$Date"

# Pure-Go build everywhere: this is what keeps the ARM cross-compiles working.
$env:CGO_ENABLED = '0'

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Host "==> $Label" -ForegroundColor Cyan
    & $Body
    if ($LASTEXITCODE -ne 0) { throw "$Label failed with exit code $LASTEXITCODE" }
}

function Build-Local {
    New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
    Invoke-Step "build bin/$Binary.exe ($Version)" {
        go build -trimpath -ldflags $LdFlags -o "bin/$Binary.exe" $Pkg
    }
}

function Build-Cross {
    New-Item -ItemType Directory -Force -Path 'dist' | Out-Null

    # linux/arm GOARM=7 is the Synology DS214se (Marvell Armada 370, ARMv7).
    $targets = @(
        @{ os = 'linux'; arch = 'amd64'; arm = ''; out = "$Binary-linux-amd64" },
        @{ os = 'linux'; arch = 'arm64'; arm = ''; out = "$Binary-linux-arm64" },
        @{ os = 'linux'; arch = 'arm'; arm = '7'; out = "$Binary-linux-armv7" },
        @{ os = 'windows'; arch = 'amd64'; arm = ''; out = "$Binary-windows-amd64.exe" }
    )

    foreach ($t in $targets) {
        $env:GOOS = $t.os
        $env:GOARCH = $t.arch
        if ($t.arm) { $env:GOARM = $t.arm } else { Remove-Item Env:GOARM -ErrorAction SilentlyContinue }
        Invoke-Step "build dist/$($t.out)" {
            go build -trimpath -ldflags $LdFlags -o "dist/$($t.out)" $Pkg
        }
    }

    Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
    Remove-Item Env:GOARM -ErrorAction SilentlyContinue
    Get-ChildItem dist | Select-Object Name, @{n = 'MB'; e = { [math]::Round($_.Length / 1MB, 1) } }
}

function Build-Plugin([string]$Line) {
    $folder = if ($Line -eq '12') { 'plugin-12' } else { 'plugin-10.11' }
    $archive = "dist/reelay-jellyfin-$($Line)-$Version.zip"
    New-Item -ItemType Directory -Force -Path "dist/$folder" | Out-Null
    Invoke-Step "publish Jellyfin $Line plugin" {
        & $Dotnet publish plugin/Jellyfin.Plugin.Reelay/Jellyfin.Plugin.Reelay.csproj -c Release "-p:JellyfinLine=$Line" "-p:Version=$PluginVersion" -o "dist/$folder"
    }
    Remove-Item -LiteralPath $archive -Force -ErrorAction SilentlyContinue
    Compress-Archive -LiteralPath "dist/$folder/Jellyfin.Plugin.Reelay.dll" -DestinationPath $archive
}

switch ($Target) {
    'build' { Build-Local }
    'test' { Invoke-Step 'go test' { go test ./... } }
    'test-all' {
        Invoke-Step 'go test' { go test ./... }
        & $PSCommandPath plugin-test
    }
    'test-race' {
        # -race needs cgo plus a 64-bit host C compiler. This machine has a
        # 32-bit MinGW, so say so plainly instead of emitting a wall of
        # "unimplemented: 64-bit mode not compiled in".
        $gcc = (Get-Command gcc -ErrorAction SilentlyContinue)
        if ($gcc) {
            $machine = & gcc -dumpmachine 2>$null
            if ($machine -notmatch '64') {
                Write-Warning "gcc at $($gcc.Source) targets '$machine'; -race needs a 64-bit compiler."
                Write-Warning "Install MSYS2 UCRT64 or TDM-GCC-64, or rely on CI (Linux) for the race check."
                exit 1
            }
        }
        else {
            Write-Warning 'no gcc on PATH; -race requires cgo. Rely on CI (Linux) for the race check.'
            exit 1
        }
        $env:CGO_ENABLED = '1'
        Invoke-Step 'go test -race' { go test -race ./... }
    }
    'cover' {
        Invoke-Step 'go test -coverprofile' { go test -coverprofile=coverage.out ./... }
        go tool cover -func=coverage.out | Select-Object -Last 25
    }
    'vet' { Invoke-Step 'go vet' { go vet ./... } }
    'lint' {
        Invoke-Step 'go vet' { go vet ./... }
        if (Get-Command staticcheck -ErrorAction SilentlyContinue) {
            Invoke-Step 'staticcheck' { staticcheck ./... }
        }
        else {
            Write-Warning 'staticcheck not installed: go install honnef.co/go/tools/cmd/staticcheck@latest'
        }
    }
    'fmt' { Invoke-Step 'gofmt' { gofmt -l -w . } }
    'tidy' { Invoke-Step 'go mod tidy' { go mod tidy } }
    'check' { Build-Local; Invoke-Step 'config check' { & "bin/$Binary.exe" --config config.yaml --check } }
    'run' { Build-Local; & "bin/$Binary.exe" --config config.yaml }
    'dev' { Build-Local; & "bin/$Binary.exe" --config config.yaml --dev }
    'cross' { Build-Cross }
    'web-install' { Push-Location web; try { Invoke-Step 'npm ci' { npm ci } } finally { Pop-Location } }
    'web' { Push-Location web; try { Invoke-Step 'npm run build' { npm run build } } finally { Pop-Location } }
    'plugin' { Build-Plugin '10.11'; Build-Plugin '12' }
    'plugin-10' { Build-Plugin '10.11' }
    'plugin-12' { Build-Plugin '12' }
    'plugin-test' {
        $env:DOTNET_ROLL_FORWARD = 'Major'
        Invoke-Step 'Jellyfin 10.11 plugin tests' { & $Dotnet test plugin/Jellyfin.Plugin.Reelay.Tests/Jellyfin.Plugin.Reelay.Tests.csproj -c Release -p:JellyfinLine=10.11 }
        Invoke-Step 'Jellyfin 12 plugin tests' { & $Dotnet test plugin/Jellyfin.Plugin.Reelay.Tests/Jellyfin.Plugin.Reelay.Tests.csproj -c Release -p:JellyfinLine=12 }
    }
    'bench-mem' {
		New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
		$testBinary = Join-Path $PSScriptRoot 'bin\reelay-engine-membench.test.exe'
		try {
			Invoke-Step 'build isolated engine memory test' { go test -c -o $testBinary ./internal/engine }
			$start = [Diagnostics.ProcessStartInfo]::new()
			$start.FileName = $testBinary
			$start.Arguments = '-test.count=1 -test.run TestWantedToImportedCycle'
			$start.UseShellExecute = $false
			$start.CreateNoWindow = $true
			$start.RedirectStandardOutput = $true
			$start.RedirectStandardError = $true
			$p = [Diagnostics.Process]::new()
			$p.StartInfo = $start
			if (-not $p.Start()) { throw 'could not start isolated engine memory test' }
			$peak = 0L
			while (-not $p.HasExited) {
				$p.Refresh()
				$peak = [math]::Max($peak, $p.WorkingSet64)
				Start-Sleep -Milliseconds 10
			}
			$stdout = $p.StandardOutput.ReadToEnd()
			$stderr = $p.StandardError.ReadToEnd()
			$p.WaitForExit()
			$exitCode = $p.ExitCode
			Write-Host $stdout.Trim()
			if ($stderr) { Write-Host $stderr.Trim() }
			if ($exitCode -ne 0) { throw "simulated search cycle failed with exit code $exitCode" }
			Write-Host ("Simulated search-cycle peak RSS: {0:N1} MB" -f ($peak / 1MB))
		}
		finally { Remove-Item -LiteralPath $testBinary -Force -ErrorAction SilentlyContinue }
    }
    'clean' {
        Remove-Item -Recurse -Force bin, dist, coverage.out -ErrorAction SilentlyContinue
        Write-Host 'cleaned'
    }
    'help' {
        Write-Host 'Targets: build test test-all test-race cover vet lint fmt tidy check run dev cross web web-install plugin plugin-10 plugin-12 plugin-test bench-mem clean'
    }
}
