@echo off
setlocal

echo === Booster Build Script (Linux) ===
echo.

:: Собираем бинарник
echo [1/2] Building booster (linux/amd64)...
set GOOS=linux
set GOARCH=amd64
set EXE=booster-linux-amd64
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE% ./cmd/booster
if errorlevel 1 (
    echo [ERROR] Build failed
    pause
    exit /b 1
)

:: Сжимаем UPX (опционально)
where upx >nul 2>&1
if not errorlevel 1 (
    echo [2/2] Compressing with UPX...
    upx --best --lzma %EXE%
) else (
    echo [2/2] UPX not found, skipping compression.
)

echo.
echo === Build complete: %EXE% ===
pause
