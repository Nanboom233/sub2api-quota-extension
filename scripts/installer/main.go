package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const binaryPath = "/app/sub2api"
const stagedPath = "/app/sub2api.plugins-new"
const readinessTimeout = 120 * time.Second

//go:embed payloads licenses
var assets embed.FS

var buildVersion string
var versionRecord = regexp.MustCompile(`^Sub2API (\S+) \(commit: [^,\r\n]+, built: [^)\r\n]+\)$`)
var versionToken = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
var standardLogPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? `)

type container struct {
	ID    string `json:"Id"`
	Name  string
	State struct {
		Running, Restarting, Paused, OOMKilled bool
		Status, Error                          string
		ExitCode                               int
	}
	RestartCount    int
	Config          struct{ Env []string }
	HostConfig      struct{ NetworkMode string }
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIp, HostPort string }
		Networks map[string]struct{ IPAddress string }
	}
}

type installer struct {
	in           io.Reader
	out, errOut  io.Writer
	timeout      time.Duration
	poll         time.Duration
	lines        chan lineInput
	probeTimeout time.Duration
	ui           *terminalUI
	apple        bool
	arch         string
}

type lineInput struct {
	line string
	err  error
}

func (i *installer) readLine(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	i.prepareInput()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case value, ok := <-i.lines:
		if !ok {
			return "", io.EOF
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return strings.TrimSpace(value.line), value.err
	}
}

func (i *installer) docker(ctx context.Context, quiet bool, args ...string) (string, error) {
	name := "docker"
	if i.apple {
		name = "container"
	}
	return i.command(ctx, quiet, name, args...)
}

func (i *installer) command(ctx context.Context, quiet bool, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	isolateCommand(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf(i.msg("%s %s failed: %w: %s"), name, strings.Join(args[:min(2, len(args))], " "), err, message)
	}
	if !quiet {
		if stdout.Len()+stderr.Len() > 0 {
			i.stopProgress()
		}
		fmt.Fprint(i.out, stdout.String())
		fmt.Fprint(i.errOut, stderr.String())
	}
	return stdout.String(), nil
}

func (i *installer) inspect(ctx context.Context, cid string) (container, error) {
	if i.apple {
		return i.inspectApple(ctx, cid)
	}
	var items []container
	output, err := i.docker(ctx, true, "inspect", cid)
	if err != nil {
		return container{}, err
	}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		return container{}, fmt.Errorf(i.msg("Read container details: %w"), err)
	}
	if len(items) != 1 {
		return container{}, errors.New(i.msg("Expected one container."))
	}
	return items[0], nil
}

func (i *installer) selectContainer(ctx context.Context) (container, error) {
	list := "ps"
	if i.apple {
		list = "list"
	}
	ids, err := i.docker(ctx, true, list, "-q")
	if err != nil {
		return container{}, err
	}
	var all, matches []container
	for _, cid := range strings.Fields(ids) {
		item, err := i.inspect(ctx, cid)
		if err != nil {
			return container{}, err
		}
		all = append(all, item)
		if strings.Contains(strings.ToLower(strings.TrimPrefix(item.Name, "/")), "sub2api") {
			matches = append(matches, item)
		}
	}
	if len(all) == 0 {
		return container{}, errors.New(i.msg("No running containers."))
	}
	shown := matches
	if len(shown) == 0 {
		shown = all
	}
	for {
		var options []string
		for n, item := range shown {
			options = append(options, fmt.Sprintf("[%d] %s (%s)", n+1, strings.TrimPrefix(item.Name, "/"), shortID(item.ID)))
		}
		more := len(shown) < len(all)
		if more {
			options = append(options, fmt.Sprintf("[%d] %s", len(shown)+1, i.msg("More")))
		}
		for {
			n, err := i.choose(ctx, options)
			if err != nil {
				return container{}, err
			}
			if more && n == len(shown)+1 {
				shown = all
				break
			}
			if n > 0 && n <= len(shown) {
				return shown[n-1], nil
			}
		}
	}
}

func shortID(cid string) string { return cid[:min(12, len(cid))] }

func endpoint(item container) (string, error) {
	port := 8080
	for _, entry := range item.Config.Env {
		if value, ok := strings.CutPrefix(entry, "SERVER_PORT="); ok {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return "", fmt.Errorf("Invalid SERVER_PORT: %w", err)
			}
			port = parsed
		}
	}
	if bindings := item.NetworkSettings.Ports[fmt.Sprintf("%d/tcp", port)]; len(bindings) > 0 {
		host := bindings[0].HostIp
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		if _, err := strconv.Atoi(bindings[0].HostPort); err != nil {
			return "", err
		}
		return net.JoinHostPort(host, bindings[0].HostPort), nil
	}
	if item.HostConfig.NetworkMode == "host" {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	for _, network := range item.NetworkSettings.Networks {
		if network.IPAddress != "" {
			return net.JoinHostPort(network.IPAddress, strconv.Itoa(port)), nil
		}
	}
	return "", errors.New("No reachable application address in container details.")
}

func (i *installer) containerEndpoint(item container) (string, error) {
	address, err := endpoint(item)
	if err != nil {
		if strings.HasPrefix(err.Error(), "Invalid SERVER_PORT:") {
			return "", fmt.Errorf(i.msg("Invalid SERVER_PORT: %w"), errors.Unwrap(err))
		}
		return "", errors.New(i.msg(err.Error()))
	}
	return address, nil
}

func (i *installer) requestReady(ctx context.Context, address, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
	if err != nil {
		return err
	}
	// Inspect supplies a direct container address. Do not send local checks through a host proxy.
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var data struct {
		Status string
		Data   struct {
			NeedsSetup *bool `json:"needs_setup"`
		}
	}
	if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
		return err
	}
	if path == "/health" && data.Status != "ok" {
		return fmt.Errorf(i.msg("Health status: %s"), data.Status)
	}
	if path == "/setup/status" && (data.Data.NeedsSetup == nil || *data.Data.NeedsSetup) {
		return errors.New(i.msg("Application setup is incomplete."))
	}
	return nil
}

func (i *installer) readiness(ctx context.Context, cid string) error {
	started := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()
	lastError, previous := i.msg("not ready"), ""
	nextProgress := time.Time{}
	var item container
	for waitCtx.Err() == nil {
		var err error
		item, err = i.inspect(waitCtx, cid)
		if err != nil {
			if waitCtx.Err() != nil {
				break
			}
			return err
		}
		state := item.State
		if !state.Running && !state.Restarting {
			detail := state.Error
			if detail == "" {
				detail = i.msg("no runtime error reported")
			}
			return fmt.Errorf(i.msg("Container %s after %.1fs: exit=%d, OOMKilled=%t, error=%s; last request: %s"), state.Status, time.Since(started).Seconds(), state.ExitCode, state.OOMKilled, detail, lastError)
		}
		if state.Paused {
			return fmt.Errorf(i.msg("Container paused after %.1fs; last request: %s"), time.Since(started).Seconds(), lastError)
		}
		address := i.msg("no address yet")
		if state.Restarting {
			err = errors.New(i.msg("Container is restarting"))
		} else {
			address, err = i.containerEndpoint(item)
			if err != nil {
				address = i.msg("no address yet")
			}
			if err == nil {
				for _, path := range []string{"/health", "/setup/status"} {
					if err = i.requestReady(waitCtx, address, path); err != nil {
						err = fmt.Errorf(i.msg("%s at %s: %w"), path, address, err)
						break
					}
				}
			}
		}
		if err == nil {
			i.stopProgress()
			fmt.Fprintf(i.out, i.msg("Application ready after %.1fs: health OK, setup complete.\n"), time.Since(started).Seconds())
			return nil
		}
		lastError = err.Error()
		current := fmt.Sprintf(i.msg("state=%s, restarts=%d, address=%s"), state.Status, item.RestartCount, address)
		if time.Now().After(nextProgress) || current != previous {
			i.progressDetail(i.msg("Application is starting; waiting for health and setup [%.1f/%gs, %s]."), time.Since(started).Seconds(), i.timeout.Seconds(), current)
			nextProgress, previous = time.Now().Add(5*time.Second), current
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
	return fmt.Errorf(i.msg("Readiness timed out after %.1fs: state=%s, restarts=%d, last request: %s"), time.Since(started).Seconds(), item.State.Status, item.RestartCount, lastError)
}

func (i *installer) replace(ctx context.Context, cid, source string) (bool, error) {
	item, err := i.inspect(ctx, cid)
	if err != nil {
		return false, err
	}
	if !item.State.Running {
		if i.apple {
			if _, err := i.docker(ctx, true, "start", cid); err != nil {
				return false, err
			}
		} else {
			_, err = i.docker(ctx, false, "cp", source, cid+":"+binaryPath)
			return true, err
		}
	}
	path, stage := i.containerPaths()
	if _, err = i.docker(ctx, false, "cp", source, cid+":"+stage); err != nil {
		return false, err
	}
	defer func() {
		_, _ = i.docker(context.Background(), true, "exec", "-u", "0", cid, "rm", "-f", stage)
	}()
	_, err = i.docker(ctx, false, "exec", "-u", "0", cid, "sh", "-c", "chown $(stat -c '%u:%g' "+path+") "+stage+" && chmod 755 "+stage+" && mv -f "+stage+" "+path)
	return true, err
}

type versionResult struct {
	version string
	err     error
}

func parseVersion(stdout, stderr string) (string, error) {
	version := ""
	for _, stream := range []string{stdout, stderr} {
		for _, line := range strings.Split(stream, "\n") {
			message := strings.TrimSpace(line)
			fields := strings.Split(message, "\t")
			if len(fields) >= 4 && fields[1] == "INFO" && fields[2] == "stdlog" {
				message = fields[3]
			} else {
				message = standardLogPrefix.ReplaceAllString(message, "")
			}
			record := versionRecord.FindStringSubmatch(message)
			if record == nil {
				continue
			}
			if !versionToken.MatchString(record[1]) {
				return "", fmt.Errorf("Unresolved executable version: %s", record[1])
			}
			current := strings.TrimPrefix(record[1], "v")
			if version != "" && current != version {
				return "", errors.New("Conflicting executable version records")
			}
			version = current
		}
	}
	if version == "" {
		return "", errors.New("No executable version record")
	}
	return version, nil
}

func (i *installer) probeVersion(ctx context.Context, cid, path string) versionResult {
	timeout := i.probeTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	name := "docker"
	if i.apple {
		name = "container"
	}
	args := []string{"exec", cid, path, "-version"}
	if cid == "" {
		name, args = path, []string{"-version"}
	}
	command := exec.CommandContext(probeCtx, name, args...)
	isolateCommand(command)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if probeCtx.Err() != nil {
		err = probeCtx.Err()
	}
	if err != nil {
		return versionResult{err: fmt.Errorf(i.msg("Version command failed: %w. %s"), err, strings.TrimSpace(stdout.String()+"\n"+stderr.String()))}
	}
	version, err := parseVersion(stdout.String(), stderr.String())
	if err != nil {
		if value, ok := strings.CutPrefix(err.Error(), "Unresolved executable version: "); ok {
			err = fmt.Errorf(i.msg("Unresolved executable version: %s"), value)
		} else {
			err = errors.New(i.msg(err.Error()))
		}
	}
	return versionResult{version: version, err: err}
}

func (result versionResult) String() string {
	if result.err != nil {
		return result.err.Error()
	}
	return result.version
}

func (i *installer) confirm(ctx context.Context) error {
	return i.askConfirmation(ctx, "Continue installation?")
}

func (i *installer) confirmVersions(ctx context.Context, installed, candidate versionResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if installed.err == nil && candidate.err == nil && installed.version == buildVersion && candidate.version == buildVersion {
		return nil
	}
	i.stopProgress()
	fmt.Fprintf(i.errOut, i.msg("Version warning. Build: %s\nInstalled: %s\nCandidate: %s\n"), buildVersion, installed, candidate)
	return i.confirm(ctx)
}

func (i *installer) validateCandidate(ctx context.Context, cid, source string) (version versionResult, result error) {
	if err := i.checkProgram(source, "linux", i.containerArch()); err != nil {
		return version, err
	}
	_, stage := i.containerPaths()
	if _, err := i.docker(ctx, false, "cp", source, cid+":"+stage); err != nil {
		return version, err
	}
	defer func() {
		// An interrupt must not skip removal of the staged candidate.
		_, err := i.docker(context.Background(), false, "exec", "-u", "0", cid, "rm", "-f", stage)
		result = errors.Join(result, err)
	}()
	if _, err := i.docker(ctx, false, "exec", "-u", "0", cid, "chmod", "755", stage); err != nil {
		return version, err
	}
	return i.probeVersion(ctx, cid, stage), nil
}

func (i *installer) install(ctx context.Context, source string) error {
	item, err := i.selectContainer(ctx)
	if err != nil {
		return err
	}
	cid := item.ID
	path, _ := i.containerPaths()
	fmt.Fprintf(i.out, i.msg("Container: %s (%s)\nProgram: %s\n"), strings.TrimPrefix(item.Name, "/"), shortID(cid), path)
	if !item.State.Running {
		return errors.New(i.msg("Select a running container."))
	}
	if err := i.checkContainerArch(ctx, cid); err != nil {
		return err
	}
	i.startProgress("Check programs")
	defer i.stopProgress()
	installed := i.probeVersion(ctx, cid, path)
	if err := ctx.Err(); err != nil {
		return err
	}
	candidate, err := i.validateCandidate(ctx, cid, source)
	if err != nil {
		return err
	}
	if err := i.confirmVersions(ctx, installed, candidate); err != nil {
		return err
	}
	i.startProgress("Back up the original program")
	directory, err := os.MkdirTemp("", "sub2api-plugins-"+shortID(cid)+"-")
	if err != nil {
		return err
	}
	backup := filepath.Join(directory, "sub2api")
	if _, err := i.docker(ctx, false, "cp", cid+":"+path, backup); err != nil {
		return err
	}
	if err := i.checkProgram(backup, "linux", i.containerArch()); err != nil {
		return fmt.Errorf(i.msg("Check the existing executable: %w"), err)
	}
	i.stopProgress()
	fmt.Fprintf(i.out, i.msg("Backup: %s\n"), backup)
	if err := ctx.Err(); err != nil {
		return err
	}
	i.startProgress("Replace the program")
	mutated, err := i.replace(ctx, cid, source)
	if err != nil && !mutated {
		return err
	}
	if err == nil {
		i.startProgress("Restart the application")
		err = i.restartContainer(ctx, cid)
	}
	if err == nil {
		i.startProgress("Wait for application readiness")
		err = i.readiness(ctx, cid)
	}
	if err != nil {
		i.stopProgress()
		fmt.Fprintln(i.errOut, i.msg("Installation failed; restoring the original executable."))
		// Rollback uses a separate context so Ctrl-C cannot cancel restoration.
		restoreCtx := context.Background()
		i.startProgress("Restore the original program")
		_, restoreErr := i.replace(restoreCtx, cid, backup)
		if restoreErr == nil {
			restoreErr = i.restartContainer(restoreCtx, cid)
		}
		if restoreErr == nil {
			restoreErr = i.readiness(restoreCtx, cid)
		}
		if restoreErr != nil {
			return fmt.Errorf(i.msg("Installation failed: %w. Restoration failed: %v. Backup: %s"), err, restoreErr, backup)
		}
		i.stopProgress()
		fmt.Fprintln(i.errOut, i.msg("Original executable restored."))
		return err
	}
	i.stopProgress()
	fmt.Fprintln(i.out, i.msg("Installed. The application is ready."))
	return nil
}

func run(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	i := installer{in: in, out: out, errOut: errOut, timeout: readinessTimeout, poll: time.Second}
	i.ui = newTerminalUI(in, out)
	defer i.stopProgress()
	fmt.Fprintf(out, "Sub2API Plugins v%s\n\n", buildVersion)
	options := []string{"[1] Docker / Compose", "[2] " + i.msg("Native executable")}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		options = append(options, "[3] Apple container")
	}
	choice, err := i.choose(ctx, options)
	if err != nil {
		return err
	}
	if choice == 2 {
		fmt.Fprintf(out, i.msg("Installation: %s\n"), i.msg("Native executable"))
		return i.installNative(ctx)
	}
	i.apple = choice == 3
	form := "Docker / Compose"
	if i.apple {
		form = "Apple container"
	}
	fmt.Fprintf(out, i.msg("Installation: %s\n"), form)
	i.arch = runtime.GOARCH
	source, cleanup, err := extractPayload("linux", i.arch)
	if err != nil {
		return err
	}
	defer cleanup()
	return i.install(ctx, source)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Stdin, os.Stdout, os.Stderr); err != nil {
		i := installer{ui: &terminalUI{chinese: chineseLocale(os.Getenv)}}
		fmt.Fprintln(os.Stderr, i.msg("Error:"), err)
		os.Exit(1)
	}
}
