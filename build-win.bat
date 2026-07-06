@echo off
setlocal

echo === Booster Build Script ===
echo.

:: Убеждаемся что goversioninfo установлен
where goversioninfo >nul 2>&1
if errorlevel 1 (
    echo [INFO] goversioninfo not found, installing...
    go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
    if errorlevel 1 (
        echo [ERROR] Failed to install goversioninfo
        pause
        exit /b 1
    )
)

:: Генерируем Windows-ресурс (иконка + версия)
echo [1/3] Generating Windows resources (icon + version info)...
go generate ./cmd/booster
if errorlevel 1 (
    echo [ERROR] go generate failed
    pause
    exit /b 1
)

:: Собираем бинарник
echo [2/3] Building booster.exe (windows/amd64)...
set GOOS=windows
set GOARCH=amd64
set EXE=booster.exe
go build -trimpath -ldflags="-s -w -buildid=" -o %EXE% ./cmd/booster
if errorlevel 1 (
    echo [ERROR] Build failed
    pause
    exit /b 1
)

:: Сжимаем UPX (опционально)
where upx >nul 2>&1
if not errorlevel 1 (
    echo [3/3] Compressing with UPX...
    upx --best --lzma %EXE%
) else (
    echo [3/3] UPX not found, skipping compression.
)

echo.
echo === Build complete: %EXE% ===
pause
