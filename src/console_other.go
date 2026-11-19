//go:build !windows

package main

// noQuickEdit: only Windows consoles pause a program on a mouse click.
func noQuickEdit() {}
