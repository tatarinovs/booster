//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// enableVirtualTerminalProcessing — флаг SetConsoleMode, включающий
// поддержку ANSI-escape последовательностей.
const enableVirtualTerminalProcessing = 0x0004

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

// ansiSupported включает обработку ANSI в консоли и сообщает, удалось ли.
// Старые консоли (до Windows 10) флаг не принимают — тогда цвета и индикатор
// прогресса выключаются, а не сыплют в вывод мусором вида «[K[33m».
func ansiSupported() bool {
	h, err := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	if ret, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); ret == 0 {
		return false // не консоль: вывод перенаправлен
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	ret, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	return ret != 0
}

// isTerminal сообщает, подключён ли файл к настоящей консоли.
// Проверка через GetConsoleMode, а не по ModeCharDevice: NUL тоже символьное
// устройство, и при запуске из планировщика ожидание Enter печаталось бы зря.
func isTerminal(f *os.File) bool {
	var mode uint32
	ret, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return ret != 0
}

func terminalWidth() int {
	h, err := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE)
	if err != nil {
		return 100
	}
	var csbi struct {
		Size              struct{ X, Y int16 }
		CursorPosition    struct{ X, Y int16 }
		Attributes        uint16
		Window            struct{ Left, Top, Right, Bottom int16 }
		MaximumWindowSize struct{ X, Y int16 }
	}
	ret, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&csbi)))
	if ret != 0 {
		return int(csbi.Window.Right - csbi.Window.Left + 1)
	}
	return 100
}
