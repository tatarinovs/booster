@echo off

setlocal

echo === Booster Build Script (macOS) ===
echo.

:: Build for macOS Intel (amd64)
echo [1/3] Building booster (darwin/amd64)...
set GOOS=darwin
set GOARCH=amd64
set EXE_AMD=booster-macos-amd64
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE_AMD% ./cmd/booster
if errorlevel 1 (
    echo [ERROR] Build failed for darwin/amd64
    pause
    exit /b 1
)

:: Build for macOS Apple Silicon (arm64)
echo [2/3] Building booster (darwin/arm64)...
set GOARCH=arm64
set EXE_ARM=booster-macos-arm64
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE_ARM% ./cmd/booster
if errorlevel 1 (
    echo [ERROR] Build failed for darwin/arm64
    pause
    exit /b 1
)

:: To create a universal (fat) binary, run on a real Mac:
::   lipo -create -output booster-macos booster-macos-amd64 booster-macos-arm64

echo.
echo === Build complete ===
echo   Intel Mac : %EXE_AMD%
echo   Apple Silicon: %EXE_ARM%
echo.
echo   Hint: to combine into a universal binary, run on a Mac:
echo   lipo -create -output booster-macos %EXE_AMD% %EXE_ARM%
pause
