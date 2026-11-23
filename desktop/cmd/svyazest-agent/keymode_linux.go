//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// keyMode: the terminal hands over every key at once and does not echo it;
// Ctrl+C / Ctrl+D come as keys (0x03 / 0x04), so the menu can restore the
// terminal before leaving. Returns the function that puts it back.
func keyMode() func() {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}
	}
	t := *old
	t.Lflag &^= unix.ICANON | unix.ECHO | unix.ISIG
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if unix.IoctlSetTermios(fd, unix.TCSETS, &t) != nil {
		return func() {}
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }
}
