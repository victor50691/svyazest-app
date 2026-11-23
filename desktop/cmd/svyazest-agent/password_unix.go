//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// readPassword: no echo via stty (x/term is not a dependency). Ctrl+C while
// typing turns echo back on before leaving, so the terminal stays usable.
func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	off := exec.Command("stty", "-echo")
	off.Stdin = os.Stdin
	echoOff := off.Run() == nil
	echoOn := func() {
		if echoOff {
			on := exec.Command("stty", "echo")
			on.Stdin = os.Stdin
			_ = on.Run()
		}
	}
	sig := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			echoOn()
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		case <-done:
		}
	}()
	line, ok := readLine()
	close(done)
	signal.Stop(sig)
	echoOn()
	fmt.Fprintln(os.Stderr)
	if !ok {
		return "", errors.New("ввод прерван")
	}
	return line, nil
}
