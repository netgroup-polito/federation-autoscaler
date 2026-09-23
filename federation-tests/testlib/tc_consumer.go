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

// ProviderDelayEntry pairs a provider's Docker IP with its simulated delay.
type ProviderDelayEntry struct {
	ProviderIP string
	DelayMs    int
	Label      string // e.g. "provider-1", for logging
}

// TCConsumerDelay manages per-provider netem delays on a consumer's Docker
// container. Each provider gets its own HTB class with a child netem qdisc,
// matched by a u32 filter on dst IP + UDP dport 30100. Traffic to providers
// not in the list flows through the default class undelayed.
type TCConsumerDelay struct {
	ContainerName  string
	Interface      string
	ProviderDelays []ProviderDelayEntry
}

// Apply installs the HTB + per-provider netem tree on the consumer container.
func (t *TCConsumerDelay) Apply() error {
	cmds := [][]string{
		{"tc", "qdisc", "add", "dev", t.Interface, "root", "handle", "42:", "htb", "default", "1"},
		{"tc", "class", "add", "dev", t.Interface, "parent", "42:", "classid", "42:1", "htb", "rate", "10gbit"},
	}

	for i, pd := range t.ProviderDelays {
		classID := i + 2
		netemHandle := 100 + i
		cmds = append(cmds,
			[]string{"tc", "class", "add", "dev", t.Interface, "parent", "42:", "classid",
				fmt.Sprintf("42:%d", classID), "htb", "rate", "10gbit"},
			[]string{"tc", "qdisc", "add", "dev", t.Interface, "parent",
				fmt.Sprintf("42:%d", classID), "handle", fmt.Sprintf("%d:", netemHandle),
				"netem", "delay", fmt.Sprintf("%dms", pd.DelayMs)},
			[]string{"tc", "filter", "add", "dev", t.Interface, "parent", "42:",
				"protocol", "ip", "prio", fmt.Sprintf("%d", i+1), "u32",
				"match", "ip", "dst", fmt.Sprintf("%s/32", pd.ProviderIP),
				"match", "ip", "protocol", "17", "0xff",
				"match", "ip", "dport", "30100", "0xffff",
				"flowid", fmt.Sprintf("42:%d", classID)},
		)
	}

	log.Printf("[tc-consumer] applying per-provider delays on %s (iface=%s, %d providers)",
		t.ContainerName, t.Interface, len(t.ProviderDelays))
	LogProviderDelays("[tc-consumer]  ", t.ProviderDelays)

	if err := t.dockerExecScript(cmds); err != nil {
		_ = t.Restore()
		return err
	}
	return nil
}

// UpdateDelays changes the netem delay on each per-provider qdisc in place.
func (t *TCConsumerDelay) UpdateDelays(newDelays []ProviderDelayEntry) error {
	cmds := make([][]string, 0, len(newDelays))
	for i, pd := range newDelays {
		if i >= len(t.ProviderDelays) {
			break
		}
		netemHandle := 100 + i
		cmds = append(cmds, []string{"tc", "qdisc", "change", "dev", t.Interface, "parent",
			fmt.Sprintf("42:%d", i+2), "handle", fmt.Sprintf("%d:", netemHandle),
			"netem", "delay", fmt.Sprintf("%dms", pd.DelayMs)})
	}
	if err := t.dockerExecScript(cmds); err != nil {
		return err
	}
	for i := range cmds {
		t.ProviderDelays[i].DelayMs = newDelays[i].DelayMs
	}
	return nil
}

// Restore removes the handle 42: qdisc tree.
func (t *TCConsumerDelay) Restore() error {
	err := t.dockerExec("tc", "qdisc", "del", "dev", t.Interface, "root", "handle", "42:")
	if err != nil && !strings.Contains(err.Error(), "No such file or directory") &&
		!strings.Contains(err.Error(), "RTNETLINK answers: Invalid argument") {
		return fmt.Errorf("remove handle 42: %w", err)
	}
	return nil
}

func (t *TCConsumerDelay) dockerExec(args ...string) error {
	cmdArgs := append([]string{"exec", t.ContainerName}, args...)
	cmd := exec.Command("docker", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (output: %s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerExecScript runs cmds as a single `sh -c` script inside the container
// instead of one `docker exec` per command.
//
// The delay matrix is per (consumer, provider), so the per-command form cost
// one exec per pair: at 30 consumers x 70 providers that is 6300 execs to
// apply and 2100 per refresh tick. At ~0.1-0.2s each a tick took minutes
// against a 30s ticker, so the delays stopped being refreshed at the intended
// cadence. Batching makes the cost per consumer, not per pair.
//
// `set -e` keeps the previous fail-fast behaviour, and the combined output is
// folded into the error, so a failing tc command still identifies itself even
// though the batch no longer reports per-command status.
func (t *TCConsumerDelay) dockerExecScript(cmds [][]string) error {
	if len(cmds) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("set -e\n")
	for _, c := range cmds {
		quoted := make([]string, len(c))
		for i, a := range c {
			quoted[i] = shellQuote(a)
		}
		sb.WriteString(strings.Join(quoted, " "))
		sb.WriteString("\n")
	}
	cmd := exec.Command("docker", "exec", t.ContainerName, "sh", "-c", sb.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%d tc commands on %s: %w (output: %s)",
			len(cmds), t.ContainerName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// shellQuote makes one argument safe for the sh -c script built by
// dockerExecScript. Passing args through a shell rather than straight to
// exec.Command hands them back to the shell's parser, and provider IPs come
// from `docker inspect` rather than from this package.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// logProviderDelaysThreshold is the largest per-consumer delay matrix still
// logged provider-by-provider. Above it the detail is replaced by a one-line
// summary: at 70 providers the per-provider form emitted 70 lines per consumer
// per refresh tick (2100 at 30 consumers, every 30s), burying everything else.
const logProviderDelaysThreshold = 10

// LogProviderDelays prints one line per provider for a small matrix, or a
// single min/max summary for a large one.
func LogProviderDelays(prefix string, delays []ProviderDelayEntry) {
	if len(delays) == 0 {
		return
	}
	if len(delays) <= logProviderDelaysThreshold {
		for _, pd := range delays {
			log.Printf("%s → %s (%s): +%dms", prefix, pd.Label, pd.ProviderIP, pd.DelayMs)
		}
		return
	}
	minDelay, maxDelay := delays[0].DelayMs, delays[0].DelayMs
	for _, pd := range delays {
		if pd.DelayMs < minDelay {
			minDelay = pd.DelayMs
		}
		if pd.DelayMs > maxDelay {
			maxDelay = pd.DelayMs
		}
	}
	log.Printf("%s %d providers, delays %d-%dms", prefix, len(delays), minDelay, maxDelay)
}
