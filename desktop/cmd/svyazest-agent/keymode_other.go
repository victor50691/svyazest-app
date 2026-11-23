//go:build !linux

package main

// keyMode: Linux only (the menu is Linux only); elsewhere input stays
// line by line.
func keyMode() func() { return func() {} }
