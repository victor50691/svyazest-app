//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	restored := false
	if windows.GetConsoleMode(h, &mode) == nil {
		if windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT) == nil {
			defer func() {
				if !restored {
					_ = windows.SetConsoleMode(h, mode)
				}
			}()
		}
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	_ = windows.SetConsoleMode(h, mode)
	restored = true
	fmt.Fprintln(os.Stderr)
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
