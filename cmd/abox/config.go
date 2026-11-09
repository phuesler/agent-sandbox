package main

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	agentsandbox "github.com/phuesler/agent-sandbox"
)

// systemConfigPath overrides the built-in config.env (root-owned, so nothing
// inside a sandbox can change it). ABOX_CONFIG overrides the path (tests).
func systemConfigPath() string {
	if p := os.Getenv("ABOX_CONFIG"); p != "" {
		return p
	}
	return "/etc/abox/config.env"
}

type Config struct {
	Bridge, NetPrefix, ProxyPort, Pool, PoolSize string
	UbuntuImage, NixosImage, GuestUser           string
	DefaultAgents, AgentsEnabled, ImageAgents    []string
	ImageAddons                                  []string
	NetOpenMinutes                               int
	Source                                       string // where settings came from (doctor)

	agents map[string]AgentInfo
	addons map[string]AddonInfo
}

func (c *Config) HostIP() string   { return c.NetPrefix + ".1" }
func (c *Config) ProxyURL() string { return "http://" + c.HostIP() + ":" + c.ProxyPort }
func (c *Config) IPPrefix() string { return c.NetPrefix + "." }

const noProxy = "localhost,127.0.0.1,::1,.local,.internal,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"

func parseEnvFile(data string) map[string]string {
	cfg := map[string]string{}
	for _, raw := range strings.Split(data, "\n") {
		line, _, _ := strings.Cut(raw, "#")
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			cfg[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return cfg
}

func loadConfig() (*Config, error) {
	def, err := agentsandbox.Assets.ReadFile("config.env")
	if err != nil {
		return nil, err
	}
	vals := parseEnvFile(string(def))
	src := "built-in defaults"
	if data, err := os.ReadFile(systemConfigPath()); err == nil {
		src = systemConfigPath()
		for k, v := range parseEnvFile(string(data)) {
			if _, known := vals[k]; !known { // typos (or pre-rename SBX_* keys) must not be silently ignored
				return nil, fmt.Errorf("%s: unknown setting %s (valid settings are listed in the built-in config.env)", src, k)
			}
			vals[k] = v
		}
	}
	c := &Config{
		Bridge: vals["ABOX_BRIDGE"], NetPrefix: vals["ABOX_NET_PREFIX"], ProxyPort: vals["ABOX_PROXY_PORT"],
		Pool: vals["ABOX_POOL"], PoolSize: vals["ABOX_POOL_SIZE"],
		UbuntuImage: vals["ABOX_UBUNTU_IMAGE"], NixosImage: vals["ABOX_NIXOS_IMAGE"], GuestUser: vals["ABOX_GUEST_USER"],
		DefaultAgents: strings.Fields(vals["ABOX_DEFAULT_AGENTS"]), AgentsEnabled: strings.Fields(vals["ABOX_AGENTS_ENABLED"]),
		ImageAgents: strings.Fields(vals["ABOX_IMAGE_AGENTS"]), ImageAddons: strings.Fields(vals["ABOX_IMAGE_ADDONS"]),
		Source: src,
	}
	if c.NetOpenMinutes, err = strconv.Atoi(vals["ABOX_NET_OPEN_MINUTES"]); err != nil {
		return nil, fmt.Errorf("%s: ABOX_NET_OPEN_MINUTES must be a number", src)
	}
	if !regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}$`).MatchString(c.NetPrefix) {
		return nil, fmt.Errorf("%s: ABOX_NET_PREFIX must look like 10.88.0", src)
	}
	if len(c.DefaultAgents) == 0 {
		c.DefaultAgents = []string{"claude"}
	}
	return c, nil
}

// render substitutes the @ABOX_*@ placeholders used by the embedded templates.
func (c *Config) render(tmpl string, uid int) string {
	return strings.NewReplacer(
		"@ABOX_BRIDGE@", c.Bridge,
		"@ABOX_NET_PREFIX@", c.NetPrefix,
		"@ABOX_HOST_IP@", c.HostIP(),
		"@ABOX_PROXY_PORT@", c.ProxyPort,
		"@ABOX_PROXY_URL@", c.ProxyURL(),
		"@ABOX_NO_PROXY@", noProxy,
		"@ABOX_POOL@", c.Pool,
		"@ABOX_UID@", strconv.Itoa(uid),
		"@ABOX_GUEST_USER@", c.GuestUser,
		"@ABOX_NET_OPEN_MINUTES@", strconv.Itoa(c.NetOpenMinutes),
	).Replace(tmpl)
}

func asset(path string) string {
	b, err := agentsandbox.Assets.ReadFile(path)
	if err != nil {
		panic("missing embedded asset " + path) // build-time invariant
	}
	return string(b)
}

// ---------------------------------------------------------------- agents

// AgentInfo is parsed from the embedded agents/<name>/agent.conf.
type AgentInfo struct {
	Name, Desc, Auth, NixPkg string
	Cmds                     map[string]string // permission mode -> start command
	Install, Allowlist       string
}

// Permission modes, from most to least supervised. "auto" is the default:
// the agent's own automatic mode (routine actions run, risky ones are checked).
var permModes = []string{"ask", "auto", "bypass"}

const defaultMode = "auto"

func validMode(m string) bool { return contains(permModes, m) }

var confLineRe = regexp.MustCompile(`^\s*(AGENT_[A-Z_]+)=(.*)$`)

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

func parseAgentConf(name, conf string) AgentInfo {
	ai := AgentInfo{Name: name, Cmds: map[string]string{}}
	for _, line := range strings.Split(conf, "\n") {
		if m := confLineRe.FindStringSubmatch(line); m != nil {
			switch v := unquote(m[2]); m[1] {
			case "AGENT_DESC":
				ai.Desc = v
			case "AGENT_CMD_ASK":
				ai.Cmds["ask"] = v
			case "AGENT_CMD_AUTO":
				ai.Cmds["auto"] = v
			case "AGENT_CMD_BYPASS":
				ai.Cmds["bypass"] = v
			case "AGENT_AUTH":
				ai.Auth = v
			case "AGENT_NIXPKG":
				ai.NixPkg = v
			}
		}
	}
	return ai
}

// Agents lists the built-in agent plugins (dirs with agent.conf + install.sh).
func (c *Config) Agents() map[string]AgentInfo {
	if c.agents != nil {
		return c.agents
	}
	c.agents = map[string]AgentInfo{}
	dirs, _ := fs.ReadDir(agentsandbox.Assets, "agents")
	for _, d := range dirs {
		if !d.IsDir() || !agentNameRe.MatchString(d.Name()) || commands[d.Name()] != nil {
			continue
		}
		conf, err1 := agentsandbox.Assets.ReadFile("agents/" + d.Name() + "/agent.conf")
		inst, err2 := agentsandbox.Assets.ReadFile("agents/" + d.Name() + "/install.sh")
		if err1 != nil || err2 != nil {
			continue
		}
		ai := parseAgentConf(d.Name(), string(conf))
		if len(ai.Cmds) == 0 {
			continue
		}
		ai.Install = string(inst)
		if al, err := agentsandbox.Assets.ReadFile("agents/" + d.Name() + "/allowlist.txt"); err == nil {
			ai.Allowlist = string(al)
		}
		c.agents[ai.Name] = ai
	}
	return c.agents
}

// CheckAgents returns an error naming unknown agents.
func (c *Config) CheckAgents(names []string) error {
	avail := c.Agents()
	for _, n := range names {
		if _, ok := avail[n]; !ok {
			return fmt.Errorf("unknown agent '%s' (available: %s)", n, strings.Join(sortedKeys(avail), ", "))
		}
	}
	return nil
}

// ---------------------------------------------------------------- egress allowlist

// buildAllowlist merges the base allowlist and the enabled agents' lists and
// prunes entries covered by a wildcard (squid refuses "quay.io" + ".quay.io").
func (c *Config) buildAllowlist() (string, []string, error) {
	enabled := c.AgentsEnabled
	if len(enabled) == 0 {
		enabled = sortedKeys(c.Agents())
	}
	texts := []string{asset("egress/allowlist.txt")}
	for _, a := range enabled {
		ai, ok := c.Agents()[a]
		if !ok {
			return "", nil, fmt.Errorf("ABOX_AGENTS_ENABLED names unknown agent '%s'", a)
		}
		texts = append(texts, ai.Allowlist)
	}
	for _, ad := range sortedKeys(c.Addons()) { // add-ons are opt-in per project; their download hosts are always allowed
		texts = append(texts, c.Addons()[ad].Allowlist)
	}
	return "# generated by abox host apply - edit egress/allowlist.txt or agents/*/allowlist.txt and rebuild\n" +
		strings.Join(pruneDomains(texts...), "\n") + "\n", enabled, nil
}

func pruneDomains(texts ...string) []string {
	set := map[string]bool{}
	for _, t := range texts {
		for _, raw := range strings.Split(t, "\n") {
			line, _, _ := strings.Cut(raw, "#")
			if d := strings.ToLower(strings.Join(strings.Fields(line), "")); d != "" {
				set[d] = true
			}
		}
	}
	var out []string
	for d := range set {
		host := strings.TrimPrefix(d, ".")
		parts := strings.Split(host, ".")
		covered := false
		for k := range parts { // is d covered by a different wildcard entry for a parent (or itself)?
			if w := "." + strings.Join(parts[k:], "."); w != d && set[w] {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}
