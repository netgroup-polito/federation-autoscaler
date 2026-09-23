/*
Copyright 2026 Politecnico di Torino - NetGroup.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package testlib

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// TCDelayKind manages a scoped netem delay on a Kind cluster's Docker
// container. Uses `docker exec` instead of SSH. The delay targets only
// UDP egress on sport 30100 (udpecho NodePort).
type TCDelayKind struct {
	ContainerName string // e.g. "cmp-20260816-143025-provider-2-control-plane"
	Interface     string // typically "eth0"
	DelayMs       int
}

// Apply installs the HTB + netem qdisc tree on the Kind container.
func (t *TCDelayKind) Apply() error {
	cmds := [][]string{
		{"tc", "qdisc", "add", "dev", t.Interface, "root", "handle", "42:", "htb", "default", "1"},
		{"tc", "class", "add", "dev", t.Interface, "parent", "42:", "classid", "42:1", "htb", "rate", "10gbit"},
		{"tc", "class", "add", "dev", t.Interface, "parent", "42:", "classid", "42:2", "htb", "rate", "10gbit"},
		{"tc", "qdisc", "add", "dev", t.Interface, "parent", "42:2", "netem", "delay", fmt.Sprintf("%dms", t.DelayMs)},
		{"tc", "filter", "add", "dev", t.Interface, "parent", "42:", "protocol", "ip", "prio", "1", "u32",
			"match", "ip", "protocol", "17", "0xff",
			"match", "ip", "sport", "30100", "0xffff",
			"flowid", "42:2"},
	}

	log.Printf("[tc] applying %dms delay on %s (iface=%s, UDP:30100 only)", t.DelayMs, t.ContainerName, t.Interface)
	for _, cmd := range cmds {
		if err := t.dockerExec(cmd...); err != nil {
			_ = t.Restore()
			return fmt.Errorf("tc command %v: %w", cmd, err)
		}
	}
	return nil
}

// UpdateDelay changes the netem delay on the existing qdisc in place.
func (t *TCDelayKind) UpdateDelay(newDelayMs int) error {
	if err := t.dockerExec("tc", "qdisc", "change", "dev", t.Interface, "parent", "42:2",
		"netem", "delay", fmt.Sprintf("%dms", newDelayMs)); err != nil {
		return fmt.Errorf("update delay to %dms: %w", newDelayMs, err)
	}
	t.DelayMs = newDelayMs
	return nil
}

// Restore removes the handle 42: qdisc tree.
func (t *TCDelayKind) Restore() error {
	err := t.dockerExec("tc", "qdisc", "del", "dev", t.Interface, "root", "handle", "42:")
	if err != nil && !strings.Contains(err.Error(), "No such file or directory") &&
		!strings.Contains(err.Error(), "RTNETLINK answers: Invalid argument") {
		return fmt.Errorf("remove handle 42: %w", err)
	}
	return nil
}

func (t *TCDelayKind) dockerExec(args ...string) error {
	cmdArgs := append([]string{"exec", t.ContainerName}, args...)
	cmd := exec.Command("docker", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (output: %s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
