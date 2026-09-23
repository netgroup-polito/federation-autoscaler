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
	"os/exec"
	"strings"
)

// TCDelay manages a scoped netem delay on a remote host (typically a Provider
// cluster node). The delay is applied ONLY to UDP egress on sport 30100 (the
// udpecho NodePort) via an HTB root + u32 classifier. TCP and all other
// traffic is completely unaffected.
//
// Packet path (egress): the udpecho container replies with sport=30100 after
// conntrack reverse-SNAT, so a u32 classifier matching ip protocol 17 AND
// sport 30100 captures exactly the echo responses.
//
// Safety:
//   - Uses handle 42: to avoid colliding with system qdiscs.
//   - Saves the original qdisc state before any changes.
//   - Restore() reinstates the saved state instead of blindly deleting root.
type TCDelay struct {
	Host      string // SSH target, e.g. "user@provider-node"
	Interface string // typically "eth0"
	DelayMs   int
	SSHKey    string // path to SSH private key, empty = default

	savedQdisc string // original `tc qdisc show dev <iface>` output
}

// Apply installs the HTB + netem qdisc tree scoped to UDP sport 30100.
// It saves the current qdisc state first so Restore() can revert cleanly.
func (t *TCDelay) Apply() error {
	var err error
	t.savedQdisc, err = t.sshCmd("tc", "qdisc", "show", "dev", t.Interface)
	if err != nil {
		return fmt.Errorf("save original qdisc: %w", err)
	}

	cmds := [][]string{
		// HTB root qdisc with handle 42:
		{"tc", "qdisc", "add", "dev", t.Interface, "root", "handle", "42:", "htb", "default", "1"},
		// Default class (no shaping, just passes traffic through)
		{"tc", "class", "add", "dev", t.Interface, "parent", "42:", "classid", "42:1", "htb", "rate", "10gbit"},
		// Delayed class for UDP echo traffic
		{"tc", "class", "add", "dev", t.Interface, "parent", "42:", "classid", "42:2", "htb", "rate", "10gbit"},
		// Netem leaf on the delayed class
		{"tc", "qdisc", "add", "dev", t.Interface, "parent", "42:2", "netem", "delay", fmt.Sprintf("%dms", t.DelayMs)},
		// u32 filter: match UDP (protocol 17) AND source port 30100
		{"tc", "filter", "add", "dev", t.Interface, "parent", "42:", "protocol", "ip", "prio", "1", "u32",
			"match", "ip", "protocol", "17", "0xff",
			"match", "ip", "sport", "30100", "0xffff",
			"flowid", "42:2"},
	}

	for _, cmd := range cmds {
		if _, err := t.sshCmd(cmd[0], cmd[1:]...); err != nil {
			_ = t.Restore()
			return fmt.Errorf("tc command %v: %w", cmd, err)
		}
	}
	return nil
}

// Restore removes the handle 42: qdisc tree, reverting to the saved state.
// Safe to call multiple times.
func (t *TCDelay) Restore() error {
	// Only remove our handle 42: — do not destroy the entire root qdisc.
	_, err := t.sshCmd("tc", "qdisc", "del", "dev", t.Interface, "root", "handle", "42:")
	if err != nil && !strings.Contains(err.Error(), "No such file or directory") &&
		!strings.Contains(err.Error(), "RTNETLINK answers: Invalid argument") {
		return fmt.Errorf("remove handle 42: %w", err)
	}
	return nil
}

// Verify checks that the u32 filter is installed and matching packets.
func (t *TCDelay) Verify() (string, error) {
	out, err := t.sshCmd("tc", "-s", "filter", "show", "dev", t.Interface, "parent", "42:")
	if err != nil {
		return "", fmt.Errorf("tc filter show: %w", err)
	}
	return out, nil
}

func (t *TCDelay) sshCmd(name string, args ...string) (string, error) {
	sshArgs := []string{"-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=5"}
	if t.SSHKey != "" {
		sshArgs = append(sshArgs, "-i", t.SSHKey)
	}
	sshArgs = append(sshArgs, t.Host)
	sshArgs = append(sshArgs, name)
	sshArgs = append(sshArgs, args...)

	cmd := exec.Command("ssh", sshArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
