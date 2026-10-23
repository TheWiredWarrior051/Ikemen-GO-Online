$ErrorActionPreference = "Stop"
$env:Path = "C:\Program Files\Go\bin;C:\msys64\mingw64\bin;C:\msys64\usr\bin;" + $env:Path
$env:CGO_ENABLED = "1"
$env:CC = "C:\msys64\mingw64\bin\gcc.exe"
$env:CXX = "C:\msys64\mingw64\bin\g++.exe"
$env:PKG_CONFIG_PATH = "C:\msys64\mingw64\lib\pkgconfig"

$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $repoRoot

$delayDir = Join-Path $repoRoot "build\delaylib"
$dllSource = Join-Path (Split-Path -Parent $repoRoot) "SHADES_3_Ikemen\lib"
$localLibDir = Join-Path $repoRoot "lib"

if ((Test-Path $dllSource) -and !(Test-Path "$localLibDir\SDL2.dll")) {
    Write-Host "==> Copying runtime DLLs into Ikemen-GO\lib..."
    New-Item -ItemType Directory -Force -Path $localLibDir | Out-Null
    Copy-Item -Force "$dllSource\*.dll" $localLibDir
}

if (!(Test-Path (Join-Path $repoRoot "data\ikemen1\system.def"))) {
    Write-Host "==> Fetching official Ikemen-GO-Screenpack..."
    $spDir = Join-Path $repoRoot "build\screenpack"
    if (!(Test-Path "$spDir\.git")) {
        git clone --depth=1 https://github.com/ikemen-engine/Ikemen-GO-Screenpack.git $spDir
    }
    foreach ($folder in @("chars", "data", "font", "sound", "stages", "video")) {
        $dst = Join-Path $repoRoot $folder
        New-Item -ItemType Directory -Force -Path $dst | Out-Null
        Get-ChildItem -Path "$spDir\$folder" -Force | ForEach-Object {
            Copy-Item -Path $_.FullName -Destination $dst -Recurse -Force
        }
    }
    $sysDef = Join-Path $repoRoot "data\ikemen1\system.def"
    if (Test-Path $sysDef) {
        $txt = [System.IO.File]::ReadAllText($sysDef)
        $oldBlock = "; Submenu: Network`r`nmenu.itemname.menunetwork = NETWORK`r`nmenu.itemname.menunetwork.serverhost = HOST GAME`r`nmenu.itemname.menunetwork.serverjoin = JOIN GAME`r`nmenu.itemname.menunetwork.back = BACK"
        $newBlock = "; Submenu: Network`r`nmenu.itemname.menunetwork = ONLINE`r`nmenu.itemname.menunetwork.lobbycreate = CREATE LOBBY`r`nmenu.itemname.menunetwork.lobbybrowse = BROWSE LOBBIES`r`nmenu.itemname.menunetwork.lobbyjoinid = JOIN BY ROOM CODE`r`nmenu.itemname.menunetwork.serverhost = DIRECT IP HOST`r`nmenu.itemname.menunetwork.serverjoin = DIRECT IP JOIN`r`nmenu.itemname.menunetwork.back = BACK"
        if ($txt.Contains($oldBlock)) {
            $txt = $txt.Replace($oldBlock, $newBlock)
            [System.IO.File]::WriteAllText($sysDef, $txt)
        }
    }
}

if (!(Test-Path "$delayDir\libavcodec.dll.a")) {
    New-Item -ItemType Directory -Force -Path $delayDir | Out-Null
    Get-ChildItem "$localLibDir\*.dll" | ForEach-Object {
        $dllPath = $_.FullName
        $base = $_.Name
        $name = $_.BaseName
        $libname = ($name -split '-')[0] -replace '^lib', ''
        Push-Location $delayDir
        & "C:\msys64\mingw64\bin\gendef.exe" $dllPath 2>&1 | Out-Null
        & "C:\msys64\mingw64\bin\dlltool.exe" --dllname $base --def "$delayDir\$name.def" --output-delaylib "$delayDir\lib$libname.dll.a"
        Remove-Item -Force "$delayDir\$name.def" -ErrorAction SilentlyContinue
        Pop-Location
    }
}

$cflags = (& "C:\msys64\mingw64\bin\pkg-config.exe" --cflags libavformat libavcodec libavutil libswscale libswresample libavfilter libxmp sdl2) -join " "
$libs = (& "C:\msys64\mingw64\bin\pkg-config.exe" --libs libavformat libavcodec libavutil libswscale libswresample libavfilter libxmp sdl2) -join " "
$env:CGO_CFLAGS = $cflags
$env:CGO_LDFLAGS = "-Lbuild/delaylib $libs"

Write-Host "==> Building lobby-server.exe..."
& "C:\Program Files\Go\bin\go.exe" build -trimpath -ldflags "-s -w" -o lobby-server.exe ./cmd/lobby-server

Write-Host "==> Building Ikemen_GO.exe..."
& "C:\Program Files\Go\bin\go.exe" build -trimpath -ldflags "-H windowsgui -s -w" -o Ikemen_GO.exe ./src
Write-Host "==> Done!"
