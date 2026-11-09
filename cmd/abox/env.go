package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Env is the backend: configuration plus the Incus operations in sandbox.go.
type Env struct {
	Cfg *Config
	UID int
}

func loadEnv() (*Env, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	uid := os.Getuid()
	if uid == 0 { // running via sudo (host commands): sandboxes belong to the invoking user
		if u, err := lookupSudoUID(); err == nil {
			uid = u
		}
	}
	return &Env{Cfg: cfg, UID: uid}, nil
}

func (e *Env) Agents() map[string]AgentInfo     { return e.Cfg.Agents() }
func (e *Env) CheckAgents(names []string) error { return e.Cfg.CheckAgents(names) }

// ---------------------------------------------------------------- process helpers

func capture(name string, args ...string) (string, string, error) {
	var out, errb strings.Builder
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func runIO(name string, args ...string) error {
	return runStdin(os.Stdin, name, args...)
}

// runStdin runs a command with the given stdin and the terminal's stdout/stderr.
func runStdin(stdin io.Reader, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(shortArgs(args), " "), err)
	}
	return nil
}

// shortArgs keeps error messages readable (guest scripts can be long).
func shortArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if len(a) > 60 {
			a = a[:57] + "..."
		}
		out[i] = a
	}
	return out
}

var shSafe = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// shq quotes a string for POSIX shells.
func shq(s string) string {
	if shSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shqJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shq(a)
	}
	return strings.Join(q, " ")
}

// ---------------------------------------------------------------- incus

type Instance struct {
	Name, Status, Type, Path, Image, IP string
	Snaps                               int
	Agents, Secrets                     []string // installed agents / secret names set in the guest
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

type incusJSON struct {
	Name   string            `json:"name"`
	Status string            `json:"status"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
	State  *struct {
		Network map[string]struct {
			Addresses []struct {
				Family  string `json:"family"`
				Address string `json:"address"`
			} `json:"addresses"`
		} `json:"network"`
	} `json:"state"`
	Snapshots []json.RawMessage `json:"snapshots"`
}

func (e *Env) Instances() (map[string]Instance, error) {
	out, errOut, err := capture("incus", "list", "user.abox=true", "-f", "json")
	if err != nil {
		msg := strings.TrimSpace(errOut)
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(strings.ToLower(msg), "permission denied") || strings.Contains(msg, "incus-admin") {
			msg += "\n  -> log out/in (or `newgrp incus-admin`) so your incus-admin membership applies"
		}
		return nil, fmt.Errorf("cannot talk to incus: %s", msg)
	}
	return parseInstances(out, e.Cfg.IPPrefix())
}

func parseInstances(js, prefix string) (map[string]Instance, error) {
	var raw []incusJSON
	if err := json.Unmarshal([]byte(js), &raw); err != nil {
		return nil, fmt.Errorf("parsing incus output: %w", err)
	}
	res := map[string]Instance{}
	for _, i := range raw {
		inst := Instance{
			Name: i.Name, Status: strings.ToLower(i.Status), Type: "container",
			Path: i.Config["user.abox.path"], Image: i.Config["user.abox.image"], Snaps: len(i.Snapshots),
			Agents: splitList(i.Config["user.abox.agents"]), Secrets: splitList(i.Config["user.abox.secrets"]),
		}
		if i.Type == "virtual-machine" {
			inst.Type = "vm"
		}
		if inst.Image == "" {
			inst.Image = "?"
		}
		if i.State != nil {
			for _, nic := range i.State.Network {
				for _, a := range nic.Addresses {
					if a.Family == "inet" && strings.HasPrefix(a.Address, prefix) {
						inst.IP = a.Address
					}
				}
			}
		}
		res[i.Name] = inst
	}
	return res, nil
}

// ---------------------------------------------------------------- egress

var (
	egressRe = regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+)[^,}\n]*?expires ([0-9hmsd.]+)`)
	msRe     = regexp.MustCompile(`\d+ms$`)
)

// parseEgress turns `abox-net status` output into ip -> "open 12m3s".
func parseEgress(s string) map[string]string {
	res := map[string]string{}
	for _, m := range egressRe.FindAllStringSubmatch(s, -1) {
		left := msRe.ReplaceAllString(strings.SplitN(m[2], ".", 2)[0], "")
		if left == "" {
			left = "<1s"
		}
		res[m[1]] = "open " + left
	}
	return res
}

const aboxNet = "/usr/local/sbin/abox-net" // root helper installed by `abox host apply`

// egressStatus returns nil if abox-net can't be used (no sudoers rule etc.).
func egressStatus() map[string]string {
	out, _, err := capture("sudo", "-n", aboxNet, "status")
	if err != nil {
		return nil
	}
	return parseEgress(out)
}
