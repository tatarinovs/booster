//go:build windows

package main

import (
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
