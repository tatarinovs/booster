@echo off
setlocal

set OUTDIR=builds
if not exist "%OUTDIR%" mkdir "%OUTDIR%"

echo === Booster Multi-Platform Release Builder ===
echo.

:: Убеждаемся что goversioninfo установлен
where goversioninfo >nul 2>&1
if errorlevel 1 (
    echo [*] goversioninfo not found, installing...
    go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
)

:: 1. Windows
echo [*] Generating Windows (amd64) resources...
cd cmd\booster
goversioninfo -icon=favicon.ico -manifest=booster.exe.manifest -64=true
cd ..\..
if errorlevel 1 exit /b 1

echo [*] Building Windows (amd64)...
set GOOS=windows
set GOARCH=amd64
set EXE_WIN=%OUTDIR%\booster.exe
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE_WIN% ./cmd/booster
if errorlevel 1 exit /b 1

where upx >nul 2>&1
if not errorlevel 1 (
    echo     Compressing %EXE_WIN%...
    upx --best --lzma %EXE_WIN%
)

echo [*] Generating Windows (x86) resources...
cd cmd\booster
goversioninfo -icon=favicon.ico -manifest=booster.exe.manifest -64=false
cd ..\..
if errorlevel 1 exit /b 1

echo [*] Building Windows (x86)...
set GOOS=windows
set GOARCH=386
set EXE_WIN_X86=%OUTDIR%\booster-x86.exe
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE_WIN_X86% ./cmd/booster
if errorlevel 1 exit /b 1

where upx >nul 2>&1
if not errorlevel 1 (
    echo     Compressing %EXE_WIN_X86%...
    upx --best --lzma %EXE_WIN_X86%
)

:: Убираем Windows-ресурс, чтобы он не ломал линковку под Linux/macOS
if exist cmd\booster\resource.syso del cmd\booster\resource.syso

:: 2. Linux amd64
echo [*] Building Linux (amd64)...
set GOOS=linux
set GOARCH=amd64
set EXE_LINUX_AMD=%OUTDIR%\booster-linux-amd64
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE_LINUX_AMD% ./cmd/booster
if errorlevel 1 exit /b 1

where upx >nul 2>&1
if not errorlevel 1 (
    echo     Compressing %EXE_LINUX_AMD%...
    upx --best --lzma %EXE_LINUX_AMD%
)

:: 3. Linux arm64
echo [*] Building Linux (arm64)...
set GOOS=linux
set GOARCH=arm64
set EXE_LINUX_ARM=%OUTDIR%\booster-linux-arm64
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE_LINUX_ARM% ./cmd/booster
if errorlevel 1 exit /b 1

where upx >nul 2>&1
if not errorlevel 1 (
    echo     Compressing %EXE_LINUX_ARM%...
    upx --best --lzma %EXE_LINUX_ARM%
)

echo [*] Building Linux (x86)...
set GOOS=linux
set GOARCH=386
set EXE_LINUX_X86=%OUTDIR%\booster-linux-386
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE_LINUX_X86% ./cmd/booster
if errorlevel 1 exit /b 1

where upx >nul 2>&1
if not errorlevel 1 (
    echo     Compressing %EXE_LINUX_X86%...
    upx --best --lzma %EXE_LINUX_X86%
)

:: 4. macOS amd64
echo [*] Building macOS (amd64)...
set GOOS=darwin
set GOARCH=amd64
set EXE_MAC_AMD=%OUTDIR%\booster-macos-amd64
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE_MAC_AMD% ./cmd/booster
if errorlevel 1 exit /b 1

:: 5. macOS arm64
echo [*] Building macOS (arm64)...
set GOOS=darwin
set GOARCH=arm64
set EXE_MAC_ARM=%OUTDIR%\booster-macos-arm64
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE_MAC_ARM% ./cmd/booster
if errorlevel 1 exit /b 1

echo.
echo === All builds completed successfully! ===
echo Output directory: %OUTDIR%
echo.
echo Hint for macOS: to combine into a universal binary, run on a Mac:
echo lipo -create -output %OUTDIR%\booster-macos %EXE_MAC_AMD% %EXE_MAC_ARM%
pause
