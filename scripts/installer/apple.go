package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const appleBinaryPath = "/app/storage/runtime/sub2api"

func (i *installer) containerPaths() (string, string) {
	if i.apple {
		return appleBinaryPath, appleBinaryPath + ".plugins-new"
	}
	return binaryPath, stagedPath
}

func (i *installer) containerArch() string {
	if i.arch == "" {
		return "amd64"
	}
	return i.arch
}

func (i *installer) restartContainer(ctx context.Context, cid string) error {
	if !i.apple {
		_, err := i.docker(ctx, true, "restart", cid)
		return err
	}
	if _, err := i.docker(ctx, true, "stop", "--time", "30", cid); err != nil {
		return err
	}
	_, err := i.docker(ctx, true, "start", cid)
	return err
}

func (i *installer) inspectApple(ctx context.Context, cid string) (container, error) {
	output, err := i.docker(ctx, true, "inspect", cid)
	if err != nil {
		return container{}, err
	}
	var records []struct {
		Configuration struct {
			ID             string
			InitProcess    struct{ Environment []string }
			PublishedPorts []struct {
				HostAddress                    string
				HostPort, ContainerPort, Count int
				Proto                          string
			}
		}
		Status struct {
			State    string
			Networks []struct{ IPv4Address string }
		}
	}
	if err := json.Unmarshal([]byte(output), &records); err != nil {
		return container{}, fmt.Errorf(i.msg("Read container details: %w"), err)
	}
	if len(records) != 1 {
		return container{}, errors.New(i.msg("Expected one container."))
	}
	record := records[0]
	item := container{ID: record.Configuration.ID, Name: record.Configuration.ID}
	item.State.Running = record.Status.State == "running"
	item.State.Status = record.Status.State
	item.Config.Env = record.Configuration.InitProcess.Environment
	item.NetworkSettings.Ports = map[string][]struct{ HostIp, HostPort string }{}
	for _, port := range record.Configuration.PublishedPorts {
		if port.Proto != "tcp" {
			continue
		}
		for offset := 0; offset < max(port.Count, 1); offset++ {
			key := fmt.Sprintf("%d/tcp", port.ContainerPort+offset)
			item.NetworkSettings.Ports[key] = append(item.NetworkSettings.Ports[key], struct{ HostIp, HostPort string }{
				HostIp: port.HostAddress, HostPort: strconv.Itoa(port.HostPort + offset),
			})
		}
	}
	item.NetworkSettings.Networks = map[string]struct{ IPAddress string }{}
	for n, network := range record.Status.Networks {
		address, _, err := net.ParseCIDR(network.IPv4Address)
		if err == nil {
			item.NetworkSettings.Networks[strconv.Itoa(n)] = struct{ IPAddress string }{address.String()}
		}
	}
	return item, nil
}

func (i *installer) checkContainerArch(ctx context.Context, cid string) error {
	output, err := i.docker(ctx, true, "exec", cid, "uname", "-m")
	if err != nil {
		return err
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[strings.TrimSpace(output)]
	if arch != i.containerArch() {
		return fmt.Errorf(i.msg("Unsupported container platform: %s/%s"), "linux", strings.TrimSpace(output))
	}
	return nil
}
