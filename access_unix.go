//go:build !windows

package main

import "golang.org/x/sys/unix"

func accessW(path string) bool { return unix.Access(path, unix.W_OK) == nil }
