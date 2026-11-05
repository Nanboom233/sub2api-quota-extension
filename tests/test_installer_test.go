// sub2api-package: installer
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	buildVersion = os.Getenv("SUB2API_BUILD_VERSION")
	if len(os.Args) == 2 && os.Args[1] == "-version" {
		fmt.Print(consoleVersion(buildVersion))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type fakeState struct {
	Containers                 []container
	Calls                      [][]string
	Installed, Candidate, Mode string
	Current                    string
	Restarts, SinceRestart     int
	Address                    string
	BadBackup                  bool
	Image                      string
	VersionStdErr              bool
	Apple                      bool
	Arch                       string
}

func fixtureELF(arch string) []byte {
	header := make([]byte, 64)
	copy(header, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(header[16:], 2)
	machine := uint16(0x3e)
	if arch == "arm64" {
		machine = 0xb7
	}
	binary.LittleEndian.PutUint16(header[18:], machine)
	binary.LittleEndian.PutUint32(header[20:], 1)
	binary.LittleEndian.PutUint16(header[52:], 64)
	return header
}

func readState(path string) fakeState {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	var state fakeState
	if err := json.Unmarshal(data, &state); err != nil {
		panic(err)
	}
	return state
}

func writeState(path string, state fakeState) {
	data, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		panic(err)
	}
}

// The test executable acts as Docker. Each command records its arguments and state.
func TestDockerHelper(t *testing.T) {
	path := os.Getenv("INSTALLER_FAKE_DOCKER_STATE")
	if path == "" {
		for _, quiet := range []bool{false, true} {
			t.Run(fmt.Sprintf("quiet=%t", quiet), func(t *testing.T) {
				f := newFixture(t, fakeState{}, "")
				output, err := f.i.docker(f.ctx, quiet, "restart", "immutable-target-123456")
				if err != nil || output != "immutable-target-123456\n" {
					t.Fatalf("Successful command: output=%q error=%v", output, err)
				}
				if quiet && f.out.Len() != 0 || !quiet && f.out.String() != output || f.errOut.Len() != 0 {
					t.Fatalf("Successful output: stdout=%q stderr=%q", f.out.String(), f.errOut.String())
				}
				f.out.Reset()
				_, err = f.i.docker(f.ctx, quiet, "fixture-error")
				if err == nil || !strings.Contains(err.Error(), "docker fixture-error failed:") {
					t.Fatalf("Command context missing: %v", err)
				}
				if f.out.Len() != 0 || f.errOut.Len() != 0 || strings.Count(err.Error(), "Unexpected Docker command") != 1 {
					t.Fatalf("Failure detail must have one owner: stdout=%q stderr=%q error=%v", f.out.String(), f.errOut.String(), err)
				}
			})
		}
		return
	}
	var args []string
	for n, value := range os.Args {
		if value == "--" {
			args = os.Args[n+1:]
			break
		}
	}
	state := readState(path)
	state.Calls = append(state.Calls, args)
	output, err := fakeCommand(&state, args)
	writeState(path, state)
	if state.VersionStdErr && args[0] == "exec" && args[len(args)-1] == "-version" {
		fmt.Fprint(os.Stderr, output)
	} else {
		fmt.Print(output)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeCommand(s *fakeState, args []string) (string, error) {
	switch args[0] {
	case "ps":
		var ids []string
		for _, item := range s.Containers {
			ids = append(ids, item.ID)
		}
		return strings.Join(ids, "\n"), nil
	case "inspect":
		for _, item := range s.Containers {
			if args[1] != item.ID {
				continue
			}
			if s.Restarts > 0 {
				s.SinceRestart++
				if s.Mode == "stopped" && s.Current == "candidate" {
					item.State.Running, item.State.Status = false, "exited"
				}
				if s.Mode == "restart" && s.Current == "candidate" && s.SinceRestart == 1 {
					item.State.Running, item.State.Restarting, item.State.Status = false, true, "restarting"
				}
				_, port, _ := net.SplitHostPort(s.Address)
				item.NetworkSettings.Ports = map[string][]struct{ HostIp, HostPort string }{"8080/tcp": {{"0.0.0.0", port}}}
				item.RestartCount = s.Restarts
			}
			if s.Apple {
				_, port, _ := net.SplitHostPort(s.Address)
				hostPort, _ := strconv.Atoi(port)
				data, _ := json.Marshal([]any{map[string]any{
					"configuration": map[string]any{
						"id": item.ID, "platform": map[string]any{"os": "linux", "architecture": "arm64"},
						"initProcess":    map[string]any{"environment": []string{"SERVER_PORT=8080"}},
						"publishedPorts": []any{map[string]any{"hostAddress": "127.0.0.1", "hostPort": hostPort, "containerPort": 8080, "proto": "tcp", "count": 1}},
					}, "status": map[string]any{"state": item.State.Status, "networks": []any{}},
				}})
				return string(data), nil
			}
			data, _ := json.Marshal([]container{item})
			if s.Image != "" {
				var records []map[string]any
				json.Unmarshal(data, &records)
				records[0]["Config"].(map[string]any)["Image"] = s.Image
				data, _ = json.Marshal(records)
			}
			return string(data), nil
		}
		return "", fmt.Errorf("Unknown immutable ID: %s", args[1])
	case "cp":
		if strings.HasSuffix(args[1], ":"+binaryPath) || strings.HasSuffix(args[1], ":"+appleBinaryPath) {
			data := fixtureELF(s.Arch)
			if s.BadBackup {
				data[0] = 0
			}
			return "", os.WriteFile(args[2], data, 0755)
		}
		isBackup := !strings.Contains(args[1], "payload-")
		if isBackup && s.Mode == "restore-failure" {
			return "", fmt.Errorf("Synthetic restore copy failure")
		}
		if strings.HasSuffix(args[2], ":"+binaryPath) || strings.HasSuffix(args[2], ":"+appleBinaryPath) {
			if isBackup {
				s.Current = "original"
			} else {
				s.Current = "candidate"
			}
		}
		return "", nil
	case "exec":
		if args[2] == "uname" {
			if s.Arch == "arm64" {
				return "aarch64\n", nil
			}
			return "x86_64\n", nil
		}
		if args[1] == "-u" {
			switch args[4] {
			case "chmod":
				if s.Mode == "chmod-failure" {
					return "", fmt.Errorf("Synthetic chmod failure")
				}
			case "sh":
				if s.Current == "candidate" {
					s.Current = "original"
				} else {
					s.Current = "candidate"
				}
			}
			return "", nil
		}
		if args[2] == stagedPath || args[2] == appleBinaryPath+".plugins-new" {
			return consoleVersion(s.Candidate), nil
		}
		if s.Mode == "probe-timeout" {
			time.Sleep(time.Second)
		}
		if s.Mode == "probe-failure" {
			return consoleVersion(s.Installed), fmt.Errorf("Synthetic version command failure")
		}
		return consoleVersion(s.Installed), nil
	case "restart":
		s.Restarts++
		s.SinceRestart = 0
		return args[1] + "\n", nil
	case "list":
		return fakeCommand(s, []string{"ps", "-q"})
	case "stop":
		for n := range s.Containers {
			s.Containers[n].State.Running = false
			s.Containers[n].State.Status = "stopped"
		}
		return "", nil
	case "start":
		if s.Mode == "apple-start-failure" {
			return "", fmt.Errorf("Synthetic Apple runtime failure")
		}
		for n := range s.Containers {
			s.Containers[n].State.Running = true
			s.Containers[n].State.Status = "running"
		}
		s.Restarts++
		s.SinceRestart = 0
		return "", nil
	case "systemctl":
		return "", nil
	}
	return "", fmt.Errorf("Unexpected Docker command: %v", args)
}

type fixture struct {
	i           *installer
	statePath   string
	out, errOut bytes.Buffer
	cancel      context.CancelFunc
	ctx         context.Context
	source      string
}

func newFixture(t *testing.T, state fakeState, input string) *fixture {
	t.Helper()
	f := &fixture{}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	t.Cleanup(f.cancel)
	root := t.TempDir()
	f.statePath = filepath.Join(root, "state.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := readState(f.statePath)
		if current.Current == "candidate" {
			if current.Mode == "redirect" && r.URL.Path == "/health" {
				http.Redirect(w, r, "/redirected", http.StatusFound)
				return
			}
			if current.Mode == "interrupt" {
				f.cancel()
				http.Error(w, "interrupted", 503)
				return
			}
			if current.Mode == "timeout" || current.Mode == "restore-failure" {
				if r.URL.Path == "/health" {
					fmt.Fprint(w, `{"status":"ok"}`)
				} else {
					fmt.Fprint(w, `{"data":{"needs_setup":true}}`)
				}
				return
			}
		}
		if r.URL.Path == "/health" || r.URL.Path == "/redirected" {
			fmt.Fprint(w, `{"status":"ok"}`)
		} else {
			fmt.Fprint(w, `{"data":{"needs_setup":false}}`)
		}
	}))
	t.Cleanup(server.Close)
	state.Address = strings.TrimPrefix(server.URL, "http://")
	if state.Containers == nil {
		state.Containers = []container{runningContainer("immutable-target-123456", "/Sub2API-main")}
	}
	if state.Installed == "" {
		state.Installed = buildVersion
	}
	if state.Candidate == "" {
		state.Candidate = buildVersion
	}
	state.Current = "original"
	writeState(f.statePath, state)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	shim := "#!/bin/sh\nexec " + quote(executable) + " -test.run '^TestDockerHelper$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte(shim), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "container"), []byte(shim), 0755); err != nil {
		t.Fatal(err)
	}
	serviceShim := strings.Replace(shim, "-- \"$@\"", "-- systemctl \"$@\"", 1)
	if err := os.WriteFile(filepath.Join(root, "systemctl"), []byte(serviceShim), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INSTALLER_FAKE_DOCKER_STATE", f.statePath)
	f.i = &installer{in: strings.NewReader(input), out: &f.out, errOut: &f.errOut, timeout: 600 * time.Millisecond, poll: 10 * time.Millisecond}
	payloadDirectory := filepath.Join(root, "payload-test")
	if err := os.Mkdir(payloadDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	f.source = filepath.Join(payloadDirectory, "sub2api")
	if err := os.WriteFile(f.source, fixtureELF(state.Arch), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, line := range strings.Split(f.out.String(), "\n") {
			if path, ok := strings.CutPrefix(line, "Backup: "); ok {
				os.RemoveAll(filepath.Dir(path))
			}
		}
	})
	return f
}

func runningContainer(id, name string) container {
	item := container{ID: id, Name: name}
	item.State.Running, item.State.Status = true, "running"
	return item
}

func hasCall(calls [][]string, values ...string) bool {
	for _, call := range calls {
		if strings.Join(call, "\x00") == strings.Join(values, "\x00") {
			return true
		}
	}
	return false
}

func TestSelection(t *testing.T) {
	cases := []struct {
		name, input, wanted string
		items               []container
		more                bool
	}{
		{"filtered", "\n99\n1\n", "target", []container{runningContainer("other", "/db"), runningContainer("target", "/SUB2API-main")}, true},
		{"numbered-more", "2\n1\n", "other", []container{runningContainer("other", "/db"), runningContainer("target", "/sub2api-main")}, true},
		{"no-match", "2\n", "second", []container{runningContainer("first", "/db"), runningContainer("second", "/server")}, false},
		{"all-match", "1\n", "target", []container{runningContainer("target", "/sub2api")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fakeState{Containers: tc.items}, tc.input)
			item, err := f.i.selectContainer(f.ctx)
			if err != nil || item.ID != tc.wanted {
				t.Fatalf("Selected %q, error %v", item.ID, err)
			}
			if strings.Contains(f.out.String(), "More") != tc.more {
				t.Fatal(f.out.String())
			}
			if strings.Contains(f.out.String(), "Cancel") {
				t.Fatal(f.out.String())
			}
		})
	}
	t.Run("empty-does-not-select", func(t *testing.T) {
		f := newFixture(t, fakeState{}, "\n")
		if _, err := f.i.selectContainer(f.ctx); err != io.EOF {
			t.Fatalf("Expected EOF, got %v", err)
		}
	})
	t.Run("none", func(t *testing.T) {
		f := newFixture(t, fakeState{Containers: []container{}}, "1\n")
		if _, err := f.i.selectContainer(f.ctx); err == nil || !strings.Contains(err.Error(), "No running containers") {
			t.Fatal(err)
		}
	})
}

func TestInstallTransaction(t *testing.T) {
	cases := []struct{ name, installed, mode, expected string }{
		{"reinstall", buildVersion, "", ""},
		{"restart-address-change", buildVersion, "restart", ""},
		{"redirect-rejected", buildVersion, "redirect", "HTTP 302"},
		{"setup-timeout-restores", buildVersion, "timeout", "Readiness timed out"},
		{"stopped-restores-directly", buildVersion, "stopped", "Container exited"},
		{"interrupt-restores", buildVersion, "interrupt", "context canceled"},
		{"restore-failure-keeps-path", buildVersion, "restore-failure", "Restoration failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fakeState{Installed: tc.installed, Mode: tc.mode}, "1\n")
			err := f.i.install(f.ctx, f.source)
			if tc.expected == "" && err != nil {
				t.Fatal(err, f.out.String(), f.errOut.String())
			}
			if tc.expected != "" && (err == nil || !strings.Contains(err.Error(), tc.expected)) {
				t.Fatalf("Expected %s, got %v", tc.expected, err)
			}
			s := readState(f.statePath)
			cid := s.Containers[0].ID
			if !hasCall(s.Calls, "exec", "-u", "0", cid, "rm", "-f", stagedPath) {
				t.Fatal("Candidate was not cleaned")
			}
			backupIndex, mutationIndex := -1, -1
			for n, call := range s.Calls {
				if call[0] == "cp" && call[1] == cid+":"+binaryPath {
					backupIndex = n
				}
				if call[0] == "exec" && len(call) > 4 && call[4] == "sh" && mutationIndex < 0 {
					mutationIndex = n
				}
				for _, arg := range call {
					if arg == "Sub2API-main" || strings.HasPrefix(arg, "Sub2API-main:") {
						t.Fatal("Used mutable name", call)
					}
				}
			}
			if backupIndex < 0 || mutationIndex <= backupIndex {
				t.Fatal("Missing backup before replacement", s.Calls)
			}
			if tc.expected == "" {
				if s.Current != "candidate" || !strings.Contains(f.out.String(), "Installed.") {
					t.Fatal(f.out.String(), s)
				}
			} else if tc.mode != "restore-failure" {
				if s.Current != "original" || !strings.Contains(f.errOut.String(), "Original executable restored.") {
					t.Fatal(f.errOut.String(), s)
				}
			}
			if tc.mode == "stopped" {
				found := false
				for _, call := range s.Calls {
					if call[0] == "cp" && call[2] == cid+":"+binaryPath {
						found = true
					}
				}
				if !found {
					t.Fatal("Stopped restoration did not copy directly")
				}
			}
			if tc.mode == "restart" && !strings.Contains(f.out.String(), "state=restarting") {
				t.Fatal(f.out.String())
			}
			if tc.mode == "restore-failure" && !strings.Contains(err.Error(), "Backup: "+os.TempDir()) {
				t.Fatal(err)
			}
			for _, line := range strings.Split(f.out.String(), "\n") {
				if path, ok := strings.CutPrefix(line, "Backup: "); ok {
					info, err := os.Stat(filepath.Dir(path))
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatal("Backup directory is not private", err)
					}
					if err := checkExecutable(path, "linux", "amd64"); err != nil {
						t.Fatal("Backup was not retained", err)
					}
				}
			}
		})
	}
}

func TestRejectionBeforeReplacement(t *testing.T) {
	cases := []struct {
		name, installed, candidate, mode, expected string
		badELF, badBackup                          bool
	}{
		{name: "candidate-elf", badELF: true, expected: "linux/amd64"},
		{name: "backup-elf", badBackup: true, expected: "existing executable"},
		{name: "chmod-cleanup", mode: "chmod-failure", expected: "Synthetic chmod failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fakeState{Installed: tc.installed, Candidate: tc.candidate, Mode: tc.mode, BadBackup: tc.badBackup}, "1\n")
			if tc.badELF {
				if err := os.WriteFile(f.source, []byte("wrong architecture"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			err := f.i.install(f.ctx, f.source)
			if err == nil || !strings.Contains(err.Error(), tc.expected) {
				t.Fatalf("Expected %s, got %v", tc.expected, err)
			}
			s := readState(f.statePath)
			if s.Restarts != 0 || s.Current != "original" {
				t.Fatal("Rejected installation mutated executable", s)
			}
			if tc.name == "chmod-cleanup" {
				if !hasCall(s.Calls, "exec", "-u", "0", s.Containers[0].ID, "rm", "-f", stagedPath) {
					t.Fatal("Missing candidate cleanup")
				}
			}
			// The backup-ELF rejection happens before its path is printed.
			for _, call := range s.Calls {
				if call[0] == "cp" && strings.HasSuffix(call[1], ":"+binaryPath) {
					t.Cleanup(func() { os.RemoveAll(filepath.Dir(call[2])) })
				}
			}
		})
	}
}

func TestEntryAndEmbeddedPayload(t *testing.T) {
	f := newFixture(t, fakeState{Arch: runtime.GOARCH}, "1\n1\n")
	t.Setenv("LC_ALL", "C")
	if err := run(f.ctx, strings.NewReader("1\n1\n"), &f.out, &f.errOut); err != nil {
		t.Fatal(err)
	}
	for _, call := range readState(f.statePath).Calls {
		if call[0] == "cp" && strings.Contains(call[1], "payload-") {
			if _, err := os.Stat(filepath.Dir(call[1])); !os.IsNotExist(err) {
				t.Fatal("Entry retained extracted payload", err)
			}
		}
	}
	err := filepath.WalkDir("licenses", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		expected, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		actual, err := assets.ReadFile(filepath.ToSlash(path))
		if err != nil || !bytes.Equal(actual, expected) {
			return fmt.Errorf("Embedded license differs: %s: %v", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func consoleVersion(version string) string {
	return "2026-10-24T03:43:48.337+0800\tINFO\tstdlog\tSub2API " + version +
		" (commit: ac1b8f1271be0d674fa6f074403c1410f30a3a81, built: 2026-10-23T07:37:15Z)\t" +
		`{"service":"sub2api","env":"bootstrap","legacy_stdlog":true}` + "\n"
}

func TestVersionParsing(t *testing.T) {
	cases := []struct{ name, stdout, stderr, want string }{
		{"official-console", consoleVersion("0.2.18"), "", "0.2.18"},
		{"stderr-v", "", consoleVersion("v0.2.18"), "0.2.18"},
		{"standard-log", "", "2026/10/24 03:43:48 Sub2API 0.2.18 (commit: unknown, built: unknown)", "0.2.18"},
		{"prerelease", consoleVersion("0.2.18-rc.1"), "", "0.2.18-rc.1"},
		{"build-suffix", consoleVersion("0.2.18+custom"), "", "0.2.18+custom"},
		{"exact-token", consoleVersion("0.2.180"), "", "0.2.180"},
		{"commit-is-not-version", "Sub2API 0.2.17 (commit: 0.2.18, built: unknown)", "", "0.2.17"},
		{"unrelated-log", "request contains Sub2API 0.2.18", "", ""},
		{"literal-latest", consoleVersion("latest"), "", ""},
		{"invalid-token", consoleVersion("0.2.18.1"), "", ""},
		{"missing", "", "", ""},
		{"repeated", consoleVersion("0.2.18"), consoleVersion("v0.2.18"), "0.2.18"},
		{"conflicting", consoleVersion("0.2.17"), consoleVersion("0.2.18"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVersion(tc.stdout, tc.stderr)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("Version %q, error %v. Expected %q", got, err, tc.want)
			}
		})
	}
}

func TestVersionProbeFailures(t *testing.T) {
	for _, mode := range []string{"probe-failure", "probe-timeout"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, fakeState{Mode: mode}, "")
			wanted := "Synthetic version command failure"
			if mode == "probe-timeout" {
				f.i.probeTimeout = 50 * time.Millisecond
				wanted = "context deadline exceeded"
			}
			result := f.i.probeVersion(f.ctx, "immutable-target-123456", binaryPath)
			if result.err == nil || result.version != "" || !strings.Contains(result.err.Error(), wanted) {
				t.Fatal(result)
			}
		})
	}
}

func TestConfirmation(t *testing.T) {
	for _, input := range []string{"y\n", " YES \n", "\n", "no\n", "", "yes"} {
		t.Run(fmt.Sprintf("input-%q", input), func(t *testing.T) {
			i := installer{in: strings.NewReader(input), out: io.Discard}
			err := i.confirm(context.Background())
			accepted := input == "y\n" || input == " YES \n"
			if (err == nil) != accepted {
				t.Fatalf("Input %q, error %v", input, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	i := installer{in: strings.NewReader("yes\n"), out: io.Discard}
	if err := i.confirm(ctx); err == nil {
		t.Fatal("Interrupt authorized installation")
	}
}

func TestVersionWarningDecision(t *testing.T) {
	t.Run("latest-matching", func(t *testing.T) {
		f := newFixture(t, fakeState{Image: "sub2api:latest", VersionStdErr: true}, "1\n")
		if err := f.i.install(f.ctx, f.source); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(f.out.String(), "Continue installation?") || strings.Contains(f.errOut.String(), "Version warning") {
			t.Fatal("Matching executable prompted for image metadata")
		}
	})
	for _, accept := range []bool{false, true} {
		t.Run(fmt.Sprintf("continue-%t", accept), func(t *testing.T) {
			input := "1\n\n"
			if accept {
				input = "1\nyes\n"
			}
			f := newFixture(t, fakeState{Installed: "0.2.17", Candidate: "0.2.19", Image: "sub2api:0.2.18"}, input)
			err := f.i.install(f.ctx, f.source)
			if (err == nil) != accept {
				t.Fatal(err, f.out.String(), f.errOut.String())
			}
			if strings.Count(f.out.String(), "Continue installation?") != 1 || strings.Count(f.errOut.String(), "Version warning") != 1 {
				t.Fatal("Expected one combined warning and prompt")
			}
			state := readState(f.statePath)
			cid := state.Containers[0].ID
			if !hasCall(state.Calls, "exec", "-u", "0", cid, "rm", "-f", stagedPath) {
				t.Fatal("Candidate was not cleaned")
			}
			if !accept {
				for _, call := range state.Calls {
					if call[0] == "restart" || (call[0] == "cp" && call[1] == cid+":"+binaryPath) || (len(call) > 4 && call[4] == "sh") {
						t.Fatal("Cancellation reached backup or replacement", call)
					}
				}
			} else if state.Current != "candidate" || state.Restarts != 1 {
				t.Fatal("Explicit confirmation did not install", state)
			}
		})
	}
}

func TestEndpoint(t *testing.T) {
	item := runningContainer("id", "/sub2api")
	item.Config.Env = []string{"SERVER_PORT=9000"}
	item.HostConfig.NetworkMode = "host"
	if address, err := endpoint(item); err != nil || address != "127.0.0.1:9000" {
		t.Fatal(address, err)
	}
	item.HostConfig.NetworkMode = "bridge"
	item.NetworkSettings.Networks = map[string]struct{ IPAddress string }{"default": {"172.17.0.2"}}
	if address, err := endpoint(item); err != nil || address != "172.17.0.2:9000" {
		t.Fatal(address, err)
	}
	item.NetworkSettings.Ports = map[string][]struct{ HostIp, HostPort string }{"9000/tcp": {{"::", strconv.Itoa(19000)}}}
	if address, err := endpoint(item); err != nil || address != "127.0.0.1:19000" {
		t.Fatal(address, err)
	}
}

func TestNativeInstallation(t *testing.T) {
	for _, mode := range []string{"manual", "systemd", "manual-interrupt", "systemd-interrupt", "manual-no-recovery-input"} {
		t.Run(mode, func(t *testing.T) {
			input := "yes\nyes\n"
			if mode == "manual-interrupt" {
				input += "yes\nyes\n"
			}
			f := newFixture(t, fakeState{}, input)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(executable)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "sub2api")
			if err := os.WriteFile(path, original, 0750); err != nil {
				t.Fatal(err)
			}
			candidate := append(append([]byte{}, original...), []byte("candidate payload")...)
			if err := os.WriteFile(f.source, candidate, 0750); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			var canceled atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				current, err := os.ReadFile(path)
				if err != nil {
					t.Error(err)
					return
				}
				if (strings.HasSuffix(mode, "interrupt") || mode == "manual-no-recovery-input") && bytes.Equal(current, candidate) && !canceled.Swap(true) {
					f.cancel()
					http.Error(w, "interrupted", 503)
					return
				}
				if r.URL.Path == "/health" {
					fmt.Fprint(w, `{"status":"ok"}`)
				} else {
					fmt.Fprint(w, `{"data":{"needs_setup":false}}`)
				}
			}))
			defer server.Close()
			target := nativeTarget{path: path, address: strings.TrimPrefix(server.URL, "http://"), info: info}
			if strings.HasPrefix(mode, "systemd") {
				target.service = "sub2api.service"
			}
			err = f.i.replaceNative(f.ctx, target, f.source)
			result, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.HasSuffix(mode, "interrupt") || mode == "manual-no-recovery-input" {
				if err == nil || !strings.Contains(err.Error(), "context canceled") {
					t.Fatalf("Expected interruption: %v", err)
				}
				if strings.HasSuffix(mode, "interrupt") && (!bytes.Equal(result, original) || !strings.Contains(f.errOut.String(), "Original executable restored.")) {
					t.Fatal(err, f.errOut.String())
				}
				if mode == "manual-no-recovery-input" && (!strings.Contains(err.Error(), "Manual recovery is pending") || !bytes.Equal(result, candidate)) {
					t.Fatal(err)
				}
			} else if err != nil || !bytes.Equal(result, candidate) {
				t.Fatal(err, f.out.String(), f.errOut.String())
			}
			if strings.HasPrefix(mode, "systemd") {
				calls := readState(f.statePath).Calls
				if !hasCall(calls, "systemctl", "stop", "sub2api.service") || !hasCall(calls, "systemctl", "start", "sub2api.service") {
					t.Fatal(calls)
				}
			}
			for _, line := range strings.Split(f.out.String(), "\n") {
				if backup, ok := strings.CutPrefix(line, "Backup: "); ok {
					data, err := os.ReadFile(backup)
					if err != nil || !bytes.Equal(data, original) {
						t.Fatal("Backup changed", err)
					}
					os.RemoveAll(filepath.Dir(backup))
				}
			}
			current, err := os.Stat(path)
			if err != nil || current.Mode().Perm() != 0750 {
				t.Fatal("Executable access changed", err)
			}
		})
	}
}

func TestAppleInstallation(t *testing.T) {
	for _, mode := range []string{"success", "stopped", "apple-start-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, fakeState{Apple: true, Arch: "arm64", Mode: mode}, "1\n")
			f.i.apple, f.i.arch = true, "arm64"
			err := f.i.install(f.ctx, f.source)
			state := readState(f.statePath)
			cid := state.Containers[0].ID
			if mode == "success" && (err != nil || state.Current != "candidate") {
				t.Fatal(err, f.out.String())
			}
			if mode == "stopped" && (err == nil || state.Current != "original" || !strings.Contains(f.errOut.String(), "Original executable restored.")) {
				t.Fatal(err, f.errOut.String())
			}
			if mode == "apple-start-failure" && (err == nil || !strings.Contains(err.Error(), "Restoration failed") || !strings.Contains(err.Error(), "Backup:")) {
				t.Fatal(err)
			}
			if !hasCall(state.Calls, "stop", "--time", "30", cid) || !hasCall(state.Calls, "start", cid) {
				t.Fatal(state.Calls)
			}
			for _, call := range state.Calls {
				joined := strings.Join(call, " ")
				if strings.Contains(joined, "base-image-id") || hasCall([][]string{call}, "restart", cid) {
					t.Fatal("Changed the image marker or Docker lifecycle", call)
				}
				if call[0] == "cp" && call[1] == cid+":"+appleBinaryPath {
					if _, err := os.Stat(call[2]); err != nil {
						t.Fatal("Host backup missing", err)
					}
				}
			}
		})
	}
}

func TestTerminalLocaleAndPlainInput(t *testing.T) {
	for _, tc := range []struct {
		all, messages, lang string
		chinese             bool
	}{
		{"", "", "zh_CN.UTF-8", true}, {"ZH-tw.UTF-8", "", "en_US", true},
		{"C", "", "zh_CN", false}, {"", "POSIX", "zh_CN", false}, {"", "zh_HK", "en_US", true}, {"", "", "", false},
	} {
		env := map[string]string{"LC_ALL": tc.all, "LC_MESSAGES": tc.messages, "LANG": tc.lang}
		if got := chineseLocale(func(key string) string { return env[key] }); got != tc.chinese {
			t.Fatalf("Locale %v: %t", env, got)
		}
	}
	var output bytes.Buffer
	i := installer{in: strings.NewReader("\n2\n"), out: &output}
	i.ui = newTerminalUI(i.in, i.out)
	if i.ui.dynamic {
		t.Fatal("Injected streams enabled cursor control")
	}
	choice, err := i.choose(context.Background(), []string{"[1] first", "[2] second"})
	if err != nil || choice != 2 || strings.Contains(output.String(), "\x1b") {
		t.Fatal(choice, err, output.String())
	}
}
