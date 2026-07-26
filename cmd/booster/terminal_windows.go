//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func init() {
	// Включаем поддержку ANSI-escape последовательностей в Windows.
	h, err := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE)
	if err == nil {
		var mode uint32
		kernel32 := syscall.NewLazyDLL("kernel32.dll")
		procGet := kernel32.NewProc("GetConsoleMode")
		procSet := kernel32.NewProc("SetConsoleMode")
		ret, _, _ := procGet.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
		if ret != 0 {
			mode |= 0x0004 // ENABLE_VIRTUAL_TERMINAL_PROCESSING
			procSet.Call(uintptr(h), uintptr(mode))
		}
	}
}

// isTerminal сообщает, подключён ли файл к настоящей консоли.
// Проверка через GetConsoleMode, а не по ModeCharDevice: NUL тоже символьное
// устройство, и при запуске из планировщика ожидание Enter печаталось бы зря.
func isTerminal(f *os.File) bool {
	var mode uint32
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procGet := kernel32.NewProc("GetConsoleMode")
	ret, _, _ := procGet.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
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
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetConsoleScreenBufferInfo")
	ret, _, _ := proc.Call(uintptr(h), uintptr(unsafe.Pointer(&csbi)))
	if ret != 0 {
		return int(csbi.Window.Right - csbi.Window.Left + 1)
	}
	return 100
}
