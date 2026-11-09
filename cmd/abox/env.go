package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Env locates the agent-sandbox repo (for sbx and config.env).
type Env struct {
	Repo          string
	Sbx           string
	NetPrefix     string // e.g. "10.88.0."
	DefaultAgents []string
	agents        map[string]AgentInfo
}

// AgentInfo is parsed from agents/<name>/agent.conf (sbx sources the same file).
type AgentInfo struct {
	Name, Desc, Cmd, Auth, NixPkg string
}

var confLineRe = regexp.MustCompile(`^\s*(AGENT_[A-Z_]+)=(.*)$`)

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

// Agents lists the agent plugins in <repo>/agents (dirs with agent.conf + install.sh).
func (e *Env) Agents() map[string]AgentInfo {
	if e.agents != nil {
		return e.agents
	}
	e.agents = map[string]AgentInfo{}
	dirs, _ := os.ReadDir(filepath.Join(e.Repo, "agents"))
	for _, d := range dirs {
		dir := filepath.Join(e.Repo, "agents", d.Name())
		if !d.IsDir() || !agentNameRe.MatchString(d.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "install.sh")); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "agent.conf"))
		if err != nil {
			continue
		}
		ai := AgentInfo{Name: d.Name()}
		for _, line := range strings.Split(string(data), "\n") {
			if m := confLineRe.FindStringSubmatch(line); m != nil {
				switch v := unquote(m[2]); m[1] {
				case "AGENT_DESC":
					ai.Desc = v
				case "AGENT_CMD":
					ai.Cmd = v
				case "AGENT_AUTH":
					ai.Auth = v
				case "AGENT_NIXPKG":
					ai.NixPkg = v
				}
			}
		}
		e.agents[ai.Name] = ai
	}
	return e.agents
}

// CheckAgents returns an error naming unknown agents.
func (e *Env) CheckAgents(names []string) error {
	avail := e.Agents()
	for _, n := range names {
		if _, ok := avail[n]; !ok {
			return fmt.Errorf("unknown agent '%s' (available: %s)", n, strings.Join(sortedKeys(avail), ", "))
		}
	}
	return nil
}

func isRepo(dir string) bool {
	for _, f := range []string{"sbx", "config.env"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

// findRepo: $ABOX_REPO, else walk up from the (symlink-resolved) binary, else `sbx` on PATH.
func findRepo() (string, error) {
	if r := os.Getenv("ABOX_REPO"); r != "" {
		if isRepo(r) {
			return r, nil
		}
		return "", fmt.Errorf("ABOX_REPO=%s does not contain sbx and config.env", r)
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
				if isRepo(dir) {
					return dir, nil
				}
				if dir == filepath.Dir(dir) {
					break
				}
			}
		}
	}
	if p, err := exec.LookPath("sbx"); err == nil {
		if p, err = filepath.EvalSymlinks(p); err == nil && isRepo(filepath.Dir(p)) {
			return filepath.Dir(p), nil
		}
	}
	return "", errors.New("cannot find the agent-sandbox repo (keep the binary in <repo>/bin, or set ABOX_REPO)")
}

func readConfigEnv(path string) map[string]string {
	cfg := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return cfg
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			cfg[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return cfg
}

func loadEnv() (*Env, error) {
	repo, err := findRepo()
	if err != nil {
		return nil, err
	}
	cfg := readConfigEnv(filepath.Join(repo, "config.env"))
	prefix := cfg["SBX_NET_PREFIX"]
	if prefix == "" {
		prefix = "10.88.0"
	}
	defAgents := strings.Fields(cfg["SBX_DEFAULT_AGENTS"])
	if len(defAgents) == 0 {
		defAgents = []string{"claude"}
	}
	return &Env{Repo: repo, Sbx: filepath.Join(repo, "sbx"), NetPrefix: prefix + ".", DefaultAgents: defAgents}, nil
}

// ---------------------------------------------------------------- process helpers

func capture(name string, args ...string) (string, string, error) {
	var out, errb strings.Builder
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func runIO(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func mergeEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func (e *Env) sbx(extra map[string]string, args ...string) error {
	cmd := exec.Command(e.Sbx, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = mergeEnv(extra)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sbx %s failed (%v)", args[0], err)
	}
	return nil
}

// sbxExec replaces the abox process (interactive sessions: agents, shell).
func (e *Env) sbxExec(extra map[string]string, args ...string) error {
	return syscall.Exec(e.Sbx, append([]string{e.Sbx}, args...), mergeEnv(extra))
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
	out, errOut, err := capture("incus", "list", "user.sbx=true", "-f", "json")
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
	return parseInstances(out, e.NetPrefix)
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
			Path: i.Config["user.sbx.path"], Image: i.Config["user.sbx.image"], Snaps: len(i.Snapshots),
			Agents: splitList(i.Config["user.sbx.agents"]), Secrets: splitList(i.Config["user.sbx.secrets"]),
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

// parseEgress turns `sbx-net status` output into ip -> "open 12m3s".
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

// egressStatus returns nil if sbx-net can't be used (no sudoers rule etc.).
func egressStatus() map[string]string {
	out, _, err := capture("sudo", "-n", "/usr/local/sbin/sbx-net", "status")
	if err != nil {
		return nil
	}
	return parseEgress(out)
}
