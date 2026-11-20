//go:build darwin || linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/scripts/buildbench/fixture"
	"github.com/stencil-hq/ovnfs/scripts/guestbench/wire"
)

// results is everything a run measured, as -out writes it.
type results struct {
	Started  time.Time       `json:"started"`
	Host     hostFacts       `json:"host"`
	Binaries []binaryFacts   `json:"binaries"`
	Flags    *options        `json:"flags"`
	Fixture  fixtureFacts    `json:"fixture"`
	Erofs    *erofsImage     `json:"erofs,omitempty"`  // set when the erofs config ran
	Module   *kernelModule   `json:"module,omitempty"` // set when the kmod config ran
	Configs  []*configResult `json:"configs"`
}

// hostFacts identifies the machine the VMs ran on.
type hostFacts struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUs     int    `json:"cpus"`
	Hostname string `json:"hostname"`
	Go       string `json:"go"` // the toolchain guestbench runs on
}

// binaryFacts identifies one input of the run: what it is, where it was and
// which build it is.
type binaryFacts struct {
	Role    string `json:"role"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256,omitempty"`
}

// fixtureFacts describes the tree every config measured.
type fixtureFacts struct {
	Project string `json:"project,omitempty"` // empty: the synthetic crate
	Files   int    `json:"bulk_files"`
	Sources int    `json:"sources"`
	Touch   string `json:"touch"`
}

// configResult is one config's VM run.
type configResult struct {
	Config   string       `json:"config"`
	Plan     wire.Plan    `json:"plan"`
	Checkout string       `json:"checkout,omitempty"` // empty for ext4
	Disk     string       `json:"disk,omitempty"`     // how the rootfs was copied: clone or overlay
	Seconds  float64      `json:"seconds"`
	Events   []wire.Event `json:"events"`
	Runs     []phaseRun   `json:"runs"`
	Error    string       `json:"error,omitempty"`
}

// phaseRun is one round of one phase: its events and what the daemon did
// between them (nil for ext4 or a failed sample).
type phaseRun struct {
	Phase  string       `json:"phase"`
	Round  int          `json:"round"`
	Begin  *wire.Event  `json:"begin,omitempty"`
	End    wire.Event   `json:"end"`
	Daemon *daemonDelta `json:"daemon,omitempty"`
}

// newResults records the host, the VM's inputs and the fixture.
func newResults(o *options, repo *fixture.Repo) *results {
	hostname, _ := os.Hostname()
	r := &results{
		Started: time.Now(),
		Host:    hostFacts{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Hostname: hostname, Go: runtime.Version()},
		Flags:   o,
		Fixture: fixtureFacts{Project: o.Project, Files: o.Files, Sources: len(repo.Sources), Touch: repo.Touch},
	}
	version := ""
	if out, err := exec.Command(o.VMON, "--version").Output(); err == nil {
		version = strings.TrimSpace(string(out))
	}
	r.addBinary("vmon", o.VMON, version)
	r.addBinary("kernel", o.Kernel, "")
	if sum, err := fileSHA256(o.Kernel); err == nil {
		r.Binaries[len(r.Binaries)-1].SHA256 = sum
	}
	// The rootfs is gigabytes; its size and path identify it well enough.
	r.addBinary("rootfs", o.Rootfs, "")
	return r
}

// addBinary records one input file.
func (r *results) addBinary(role, path, version string) {
	b := binaryFacts{Role: role, Path: path, Version: version}
	if info, err := os.Stat(path); err == nil {
		b.Size = info.Size()
	}
	r.Binaries = append(r.Binaries, b)
}

// fileSHA256 hashes a file's content.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// write saves the raw results as indented JSON.
func (r *results) write(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// phases lists the rows of the tables: every phase of the selected
// workloads, in run order.
func (r *results) phases() []string {
	var rows []string
	for _, w := range r.Flags.Workloads {
		rows = append(rows, wire.Phases[w]...)
	}
	return rows
}

// print writes the tables as GitHub-flavored markdown: median wall times
// against ext4, then per-op units and the daemon's share; failed configs
// follow. A config's missing phases stay blank.
func (r *results) print(w io.Writer) {
	ext4 := slices.IndexFunc(r.Configs, func(c *configResult) bool { return c.Config == cfgExt4 })
	header := []string{"phase"}
	for _, c := range r.Configs {
		header = append(header, c.Config)
	}
	var rows [][]string
	for _, phase := range r.phases() {
		// net.* calibrate the network, which no filesystem config changes.
		base, hasBase := 0.0, false
		if ext4 >= 0 && !slices.Contains(wire.Phases[wire.Net], phase) {
			base, hasBase = median(r.Configs[ext4].values(phase, wallSeconds))
		}
		row := []string{phase}
		for _, c := range r.Configs {
			m, ok := median(c.values(phase, wallSeconds))
			cell := ""
			if ok {
				cell = formatSeconds(m)
				if hasBase && base > 0 && c.Config != cfgExt4 {
					cell += fmt.Sprintf(" (%.2fx)", m/base)
				}
			}
			row = append(row, cell)
		}
		rows = append(rows, row)
	}
	fmt.Fprintln(w, "### Wall time (median)")
	fmt.Fprintln(w)
	markdownTable(w, header, rows)

	header = []string{"phase"}
	for _, c := range r.Configs {
		header = append(header, c.Config+" unit")
		if c.Checkout != "" {
			header = append(header, c.Config+" calls/op", c.Config+" daemon ms")
		}
	}
	rows = nil
	for _, phase := range r.phases() {
		row := []string{phase}
		for _, c := range r.Configs {
			row = append(row, c.unitCell(phase))
			if c.Checkout != "" {
				row = append(row, medianCell(c.values(phase, c.callsPerOp), "%.2f"), medianCell(c.values(phase, c.daemonMillis), "%.1f"))
			}
		}
		rows = append(rows, row)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "### Units and daemon (median)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "calls/op is the checkout's daemon file calls divided by the phase's guest operations; daemon ms sums those calls' durations, so it exceeds wall time when calls overlap.")
	fmt.Fprintln(w)
	markdownTable(w, header, rows)

	for _, c := range r.Configs {
		if c.Error != "" {
			first, _, _ := strings.Cut(c.Error, "\n")
			fmt.Fprintf(w, "\n**%s failed**: %s\n", c.Config, first)
		}
	}
}

// metric derives one number from a phase's round, or reports none.
type metric func(phaseRun) (float64, bool)

// values collects metric over every round of phase.
func (c *configResult) values(phase string, m metric) []float64 {
	var vs []float64
	for _, pr := range c.Runs {
		if pr.Phase != phase {
			continue
		}
		if v, ok := m(pr); ok {
			vs = append(vs, v)
		}
	}
	return vs
}

// wallSeconds is the round's duration in the guest.
func wallSeconds(pr phaseRun) (float64, bool) { return float64(pr.End.NS) / 1e9, pr.End.NS > 0 }

// microsPerOp is the round's guest time per operation.
func microsPerOp(pr phaseRun) (float64, bool) {
	return float64(pr.End.NS) / 1e3 / float64(pr.End.Ops), pr.End.Ops > 0
}

// mibPerSecond is the round's throughput.
func mibPerSecond(pr phaseRun) (float64, bool) {
	return float64(pr.End.Bytes) / (1 << 20) / (float64(pr.End.NS) / 1e9), pr.End.Bytes > 0 && pr.End.NS > 0
}

// callsPerOp is the checkout's daemon calls per guest operation.
func (c *configResult) callsPerOp(pr phaseRun) (float64, bool) {
	if pr.Daemon == nil || pr.End.Ops == 0 {
		return 0, false
	}
	return float64(pr.Daemon.timer("calls", c.Checkout).Count) / float64(pr.End.Ops), true
}

// daemonMillis is the time the daemon spent in the checkout's calls.
func (c *configResult) daemonMillis(pr phaseRun) (float64, bool) {
	if pr.Daemon == nil {
		return 0, false
	}
	return float64(pr.Daemon.timer("calls", c.Checkout).Us) / 1e3, true
}

// unitCell renders the phase's derived units: microseconds per op where the
// phase counts ops, MiB/s where it counts bytes.
func (c *configResult) unitCell(phase string) string {
	var parts []string
	if cell := medianCell(c.values(phase, microsPerOp), "%.1f µs/op"); cell != "" {
		parts = append(parts, cell)
	}
	if cell := medianCell(c.values(phase, mibPerSecond), "%.1f MiB/s"); cell != "" {
		parts = append(parts, cell)
	}
	return strings.Join(parts, ", ")
}

// medianCell renders the median of vs with format, blank without values.
func medianCell(vs []float64, format string) string {
	m, ok := median(vs)
	if !ok {
		return ""
	}
	return fmt.Sprintf(format, m)
}

// median returns the middle of vs, averaging the two middles of an even
// count; false when vs is empty.
func median(vs []float64) (float64, bool) {
	if len(vs) == 0 {
		return 0, false
	}
	s := slices.Sorted(slices.Values(vs))
	n := len(s)
	if n%2 == 1 {
		return s[n/2], true
	}
	return (s[n/2-1] + s[n/2]) / 2, true
}

// formatSeconds renders a duration at a precision fit for phase times.
func formatSeconds(s float64) string {
	if s < 1 {
		return fmt.Sprintf("%.1fms", s*1e3)
	}
	return fmt.Sprintf("%.3fs", s)
}

// markdownTable writes a GitHub-flavored markdown table with the first
// column left-aligned and the rest right-aligned.
func markdownTable(w io.Writer, header []string, rows [][]string) {
	line := func(cells []string) { fmt.Fprintln(w, "| "+strings.Join(cells, " | ")+" |") }
	line(header)
	sep := make([]string, len(header))
	for i := range sep {
		sep[i] = "---:"
	}
	sep[0] = "---"
	line(sep)
	for _, row := range rows {
		line(row)
	}
}
