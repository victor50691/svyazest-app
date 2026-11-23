package main

// One reader of the terminal for the whole program: the menu (single keys,
// 1.0.5), the login code prompt and the password prompt all take input from
// it, so a reader left behind by one screen never swallows the input meant
// for the next. Whatever a screen took but did not use goes back (unread).

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

var (
	stdinOnce sync.Once
	stdinCh   chan []byte // chunks as read(2) returned them; closed at EOF
	pendMu    sync.Mutex
	pending   []byte
)

func stdinChunks() <-chan []byte {
	stdinOnce.Do(func() {
		stdinCh = make(chan []byte)
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					stdinCh <- append([]byte(nil), buf[:n]...)
				}
				if err != nil {
					close(stdinCh)
					return
				}
			}
		}()
	})
	return stdinCh
}

func unread(b []byte) {
	if len(b) == 0 {
		return
	}
	pendMu.Lock()
	pending = append(append([]byte(nil), b...), pending...)
	pendMu.Unlock()
}

// nextChunk: pending input first, then the terminal. ok=false: EOF or ctx done.
func nextChunk(ctx context.Context) ([]byte, bool) {
	pendMu.Lock()
	if len(pending) > 0 {
		b := pending
		pending = nil
		pendMu.Unlock()
		return b, true
	}
	pendMu.Unlock()
	select {
	case b, ok := <-stdinChunks():
		return b, ok
	case <-ctx.Done():
		return nil, false
	}
}

// readLineCtx: one line without \r\n; false at EOF (Ctrl+D) or when ctx ends
// (the part already typed stays for the next reader).
func readLineCtx(ctx context.Context) (string, bool) {
	var acc []byte
	for {
		b, ok := nextChunk(ctx)
		if !ok {
			unread(acc)
			return "", false
		}
		acc = append(acc, b...)
		if i := bytes.IndexByte(acc, '\n'); i >= 0 {
			unread(acc[i+1:])
			return strings.TrimRight(string(acc[:i]), "\r"), true
		}
	}
}

func readLine() (string, bool) { return readLineCtx(context.Background()) }

// linesCtx: lines as a channel for select (login: code or Telegram,
// whichever comes first); stops reading when ctx ends.
func linesCtx(ctx context.Context) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		for {
			line, ok := readLineCtx(ctx)
			if !ok {
				return
			}
			select {
			case ch <- line:
			case <-ctx.Done():
				unread([]byte(line + "\n"))
				return
			}
		}
	}()
	return ch
}

// readKeyCtx: one key press without Enter (the terminal is switched to
// single-key mode meanwhile). Returns the first character typed; the rest
// of the same press (arrow-key sequences, a paste) is dropped. ok=false at
// EOF / ctx end.
func readKeyCtx(ctx context.Context) (string, bool) {
	restore := keyMode()
	defer restore()
	b, ok := nextChunk(ctx)
	if !ok || len(b) == 0 {
		return "", false
	}
	r, size := utf8.DecodeRune(b)
	if r == utf8.RuneError && size <= 1 {
		return string(b[:1]), true
	}
	return string(b[:size]), true
}

func readKey() (string, bool) { return readKeyCtx(context.Background()) }
