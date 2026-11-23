//go:build !linux

package main

// The terminal menu is for Linux; on Windows the agent page and the Start
// menu shortcuts do this job.

func menuAvailable() bool { return false }

func cmdMenu(o *opts) error { return nil }
