//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func isolateCommand(command *exec.Cmd) {
	// Terminal signals must not cancel commands that restore the original program.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func preserveOwner(path, original string, info os.FileInfo) error {
	owner := info.Sys().(*syscall.Stat_t)
	return os.Chown(path, int(owner.Uid), int(owner.Gid))
}

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
