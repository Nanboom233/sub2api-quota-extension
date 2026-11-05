package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

type nativeTarget struct {
	path, address, service string
	info                   os.FileInfo
}

func (i *installer) installNative(ctx context.Context) error {
	defaultPath := "sub2api"
	if runtime.GOOS == "linux" {
		defaultPath = "/opt/sub2api/sub2api"
	} else if runtime.GOOS == "windows" {
		defaultPath = "sub2api.exe"
	}
	path, err := i.input(ctx, "Program path", defaultPath)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New(i.msg("The program path must name a regular file."))
	}
	address, err := i.input(ctx, "Application address", "127.0.0.1:8080")
	if err != nil {
		return err
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return fmt.Errorf(i.msg("Invalid application address: %w"), err)
	}
	target := nativeTarget{path: path, address: address, info: info}
	if err := i.detectService(ctx, &target); err != nil {
		return err
	}
	fmt.Fprintf(i.out, i.msg("Program: %s\nAddress: %s\n"), path, address)
	source, cleanup, err := extractPayload(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	defer cleanup()
	return i.replaceNative(ctx, target, source)
}

func (i *installer) detectService(ctx context.Context, target *nativeTarget) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if _, err := os.Stat("/run/systemd/system"); os.IsNotExist(err) {
		return nil
	}
	output, err := i.command(ctx, true, "systemctl", "show", "sub2api.service", "--property=LoadState,ExecStart,WorkingDirectory")
	if err != nil {
		return fmt.Errorf(i.msg("Read the service configuration: %w"), err)
	}
	properties := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			properties[key] = value
		}
	}
	if properties["LoadState"] == "not-found" || properties["LoadState"] == "" {
		return nil
	}
	match := regexp.MustCompile(`(?:^|\{\s*)path=([^;]+?)\s*;`).FindStringSubmatch(properties["ExecStart"])
	if match == nil {
		return fmt.Errorf(i.msg("Cannot identify the service executable: %s"), properties["ExecStart"])
	}
	servicePath, err := filepath.EvalSymlinks(match[1])
	if err != nil {
		return err
	}
	if servicePath == target.path {
		target.service = "sub2api.service"
		fmt.Fprintf(i.out, i.msg("Service: %s\nWorking directory: %s\n"), target.service, properties["WorkingDirectory"])
	}
	return nil
}

func copyProgram(source, destination, original string, info os.FileInfo) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	err = errors.Join(copyErr, out.Close())
	if err != nil {
		return err
	}
	if err := preserveOwner(destination, original, info); err != nil {
		return err
	}
	return os.Chmod(destination, info.Mode())
}

func (i *installer) nativeControl(ctx context.Context, target nativeTarget, start, recovery bool) error {
	if target.service != "" {
		action := "stop"
		stage := "Stop the application"
		if start {
			action = "start"
			stage = "Start the application"
		}
		i.startProgress(stage)
		_, err := i.command(ctx, true, "systemctl", action, target.service)
		return err
	}
	i.stopProgress()
	message := "Stop the application through its original launcher, then continue."
	if start {
		message = "Start the application through its original launcher, then continue."
	}
	fmt.Fprintln(i.out, i.msg(message))
	if err := i.askConfirmation(ctx, "Completed?"); err != nil {
		if recovery {
			return fmt.Errorf(i.msg("Manual recovery is pending: %w"), err)
		}
		return err
	}
	return nil
}

func (i *installer) replaceNative(ctx context.Context, target nativeTarget, source string) error {
	i.startProgress("Check programs")
	defer i.stopProgress()
	for _, path := range []string{target.path, source} {
		if err := i.checkProgram(path, runtime.GOOS, runtime.GOARCH); err != nil {
			return err
		}
	}
	staged, err := os.CreateTemp(filepath.Dir(target.path), ".sub2api-plugins-*"+filepath.Ext(target.path))
	if err != nil {
		return err
	}
	stage := staged.Name()
	if err := staged.Close(); err != nil {
		return err
	}
	defer os.Remove(stage)
	if err := copyProgram(source, stage, target.path, target.info); err != nil {
		return err
	}
	if err := i.confirmVersions(ctx, i.probeVersion(ctx, "", target.path), i.probeVersion(ctx, "", stage)); err != nil {
		return err
	}
	i.startProgress("Back up the original program")
	directory, err := os.MkdirTemp(filepath.Dir(target.path), ".sub2api-plugins-backup-")
	if err != nil {
		return err
	}
	backup := filepath.Join(directory, filepath.Base(target.path))
	if err := copyProgram(target.path, backup, target.path, target.info); err != nil {
		return err
	}
	i.stopProgress()
	fmt.Fprintf(i.out, i.msg("Backup: %s\n"), backup)
	if err := ctx.Err(); err != nil {
		return err
	}
	err = i.nativeControl(ctx, target, false, false)
	if err != nil && target.service == "" {
		return err
	}
	mutated := false
	if err == nil {
		i.startProgress("Replace the program")
		err = ctx.Err()
		if err == nil {
			err = replaceFile(stage, target.path)
			mutated = err == nil
		}
	}
	if err == nil {
		err = i.nativeControl(ctx, target, true, false)
	}
	if err == nil {
		i.startProgress("Wait for application readiness")
		err = i.nativeReadiness(ctx, target.address)
	}
	if err == nil {
		i.stopProgress()
		fmt.Fprintln(i.out, i.msg("Installed. The application is ready."))
		return nil
	}
	i.stopProgress()
	fmt.Fprintln(i.errOut, i.msg("Installation failed; restoring the original executable."))
	// Restoration must remain usable after cancellation of the installation context.
	restoreCtx := context.Background()
	var restoreErr error
	if mutated {
		restoreErr = i.nativeControl(restoreCtx, target, false, true)
	}
	if mutated && restoreErr == nil {
		i.startProgress("Restore the original program")
		restoreErr = copyProgram(backup, stage, backup, target.info)
	}
	if mutated && restoreErr == nil {
		restoreErr = replaceFile(stage, target.path)
	}
	if restoreErr == nil {
		restoreErr = i.nativeControl(restoreCtx, target, true, true)
	}
	if restoreErr == nil {
		i.startProgress("Wait for application readiness")
		restoreErr = i.nativeReadiness(restoreCtx, target.address)
	}
	if restoreErr != nil {
		return fmt.Errorf(i.msg("Installation failed: %w. Restoration failed: %v. Backup: %s"), err, restoreErr, backup)
	}
	i.stopProgress()
	fmt.Fprintln(i.errOut, i.msg("Original executable restored."))
	return err
}

func (i *installer) nativeReadiness(ctx context.Context, address string) error {
	waitCtx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()
	started := time.Now()
	lastError := i.msg("not ready")
	for waitCtx.Err() == nil {
		var err error
		for _, path := range []string{"/health", "/setup/status"} {
			if err = i.requestReady(waitCtx, address, path); err != nil {
				lastError = fmt.Sprintf(i.msg("%s at %s: %v"), path, address, err)
				break
			}
		}
		if err == nil {
			i.stopProgress()
			fmt.Fprintf(i.out, i.msg("Application ready after %.1fs: health OK, setup complete.\n"), time.Since(started).Seconds())
			return nil
		}
		timer := time.NewTimer(i.poll)
		select {
		case <-waitCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf(i.msg("Readiness timed out after %.1fs: %s"), time.Since(started).Seconds(), lastError)
}
