package main

import "syscall"

// noQuickEdit turns off QuickEdit in this console window. With it on, one click in the window starts a text selection
// and Windows pauses the program at its next line of output (the timer redraws 5 times a second) until a key is
// pressed: to a person it looks frozen. Only this window changes; the cost is no copying text with the mouse.
func noQuickEdit() {
	const (
		quickEdit     = 0x0040 // ENABLE_QUICK_EDIT_MODE
		extendedFlags = 0x0080 // ENABLE_EXTENDED_FLAGS: needed for the quick edit bit to be taken
	)
	in, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil {
		return
	}
	var mode uint32
	if syscall.GetConsoleMode(in, &mode) != nil {
		return // not a console (redirected input): nothing to do
	}
	setConsoleMode := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")
	setConsoleMode.Call(uintptr(in), uintptr(mode&^quickEdit|extendedFlags))
}
