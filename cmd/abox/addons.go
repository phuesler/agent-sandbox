package main

// Add-ons (extra tooling for agents, e.g. a headless browser) and port
// forwarding from the host to a sandbox.

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	agentsandbox "github.com/phuesler/agent-sandbox"
)

// ---------------------------------------------------------------- add-ons

// AddonInfo is parsed from the embedded addons/<name>/addon.conf.
type AddonInfo struct {
	Name, Desc         string
	Install, Allowlist string
}

func (c *Config) Addons() map[string]AddonInfo {
	if c.addons != nil {
		return c.addons
	}
	c.addons = map[string]AddonInfo{}
	dirs, _ := fs.ReadDir(agentsandbox.Assets, "addons")
	for _, d := range dirs {
		if !d.IsDir() || !agentNameRe.MatchString(d.Name()) {
			continue
		}
		conf, err1 := agentsandbox.Assets.ReadFile("addons/" + d.Name() + "/addon.conf")
		inst, err2 := agentsandbox.Assets.ReadFile("addons/" + d.Name() + "/install.sh")
		if err1 != nil || err2 != nil {
			continue
		}
		ai := AddonInfo{Name: d.Name(), Install: string(inst)}
		for _, line := range strings.Split(string(conf), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k == "ADDON_DESC" {
				ai.Desc = unquote(v)
			}
		}
		if al, err := agentsandbox.Assets.ReadFile("addons/" + d.Name() + "/allowlist.txt"); err == nil {
			ai.Allowlist = string(al)
		}
		c.addons[ai.Name] = ai
	}
	return c.addons
}

func (c *Config) CheckAddons(names []string) error {
	avail := c.Addons()
	for _, n := range names {
		if _, ok := avail[n]; !ok {
			return fmt.Errorf("unknown add-on '%s' (available: %s)", n, strings.Join(sortedKeys(avail), ", "))
		}
	}
	return nil
}

// AddonInstall installs (or with update: upgrades) add-ons in a sandbox. Add-on
// installers also register themselves with the installed agents, so they are
// re-run after an agent is added.
func (e *Env) AddonInstall(name string, addons []string, update bool) error {
	if err := e.Cfg.CheckAddons(addons); err != nil {
		return err
	}
	if img, _ := incusOut("config", "get", name, "user.abox.image"); img == "nixos" {
		return fmt.Errorf("add-ons are not supported on NixOS sandboxes yet")
	}
	if err := e.WaitReady(name); err != nil {
		return err
	}
	flag := ""
	if update {
		flag = "1"
	}
	for _, a := range addons {
		info("installing add-on '%s' in %s", a, name)
		script := "/tmp/abox-addon-" + a + ".sh"
		if err := pushFile(name, script, e.Cfg.Addons()[a].Install, "0755"); err != nil {
			return err
		}
		if err := e.asUser(name, "cd && ABOX_AGENT_UPDATE="+flag+" bash "+script); err != nil {
			return fmt.Errorf("installing add-on '%s' failed. Blocked downloads show in `abox net log`", a)
		}
		if err := e.listAdd(name, "user.abox.addons", a); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- ports

// Port forwards a host port (127.0.0.1 only) to a port inside the sandbox.
type Port struct{ Host, Guest int }

func (p Port) String() string {
	if p.Host == p.Guest {
		return strconv.Itoa(p.Host)
	}
	return fmt.Sprintf("%d:%d", p.Host, p.Guest)
}

func (p Port) device() string { return "abox-port-" + strconv.Itoa(p.Host) }

// parsePort accepts "3000" or "8080:3000" (host:sandbox).
func parsePort(s string) (Port, error) {
	h, g, pair := strings.Cut(strings.TrimSpace(s), ":")
	if !pair {
		g = h
	}
	hp, err1 := strconv.Atoi(h)
	gp, err2 := strconv.Atoi(g)
	if err1 != nil || err2 != nil || hp < 1 || hp > 65535 || gp < 1 || gp > 65535 {
		return Port{}, fmt.Errorf("invalid port '%s' (use PORT or HOSTPORT:SANDBOXPORT, 1-65535)", s)
	}
	return Port{hp, gp}, nil
}

// portFromDevice recognizes the proxy devices abox creates.
func portFromDevice(name string, dev map[string]string) (Port, bool) {
	if !strings.HasPrefix(name, "abox-port-") || dev["type"] != "proxy" {
		return Port{}, false
	}
	l := dev["listen"][strings.LastIndex(dev["listen"], ":")+1:]
	c := dev["connect"][strings.LastIndex(dev["connect"], ":")+1:]
	p, err := parsePort(l + ":" + c)
	return p, err == nil
}

// PortAdd forwards 127.0.0.1:<host> on the host to <guest> inside the sandbox.
// Containers: connects to 127.0.0.1 in the sandbox (apps may listen on
// localhost). VMs only support NAT-style proxying, so the app must listen on
// 0.0.0.0 and the VM's address is pinned to its current lease.
func (e *Env) PortAdd(name string, p Port) error {
	insts, err := e.Instances()
	if err != nil {
		return err
	}
	inst := insts[name]
	args := []string{"config", "device", "add", name, p.device(), "proxy",
		fmt.Sprintf("listen=tcp:127.0.0.1:%d", p.Host)}
	if inst.Type == "vm" {
		if inst.IP == "" {
			return fmt.Errorf("%s has no address yet; start it first", name)
		}
		if err := e.pinVMAddress(name, inst.IP); err != nil {
			return err
		}
		args = append(args, fmt.Sprintf("connect=tcp:%s:%d", inst.IP, p.Guest), "nat=true")
	} else {
		args = append(args, fmt.Sprintf("connect=tcp:127.0.0.1:%d", p.Guest), "bind=host")
	}
	if err := incusQuiet(args...); err != nil {
		return fmt.Errorf("forwarding port %s: %v (is host port %d already in use?)", p, err, p.Host)
	}
	info("forwarding http://localhost:%d -> %s:%d", p.Host, name, p.Guest)
	return nil
}

func (e *Env) PortRm(name string, p Port) error {
	if err := incusQuiet("config", "device", "remove", name, p.device()); err != nil {
		return err
	}
	info("stopped forwarding localhost:%d", p.Host)
	return nil
}

// pinVMAddress makes the VM's current DHCP lease static (NAT proxying needs it).
func (e *Env) pinVMAddress(name, ip string) error {
	if cur, _ := incusOut("config", "device", "get", name, "eth0", "ipv4.address"); cur == ip {
		return nil
	}
	// eth0 comes from the profile; override it on the instance with a fixed address
	if err := incusQuiet("config", "device", "override", name, "eth0", "ipv4.address="+ip); err != nil {
		return incusQuiet("config", "device", "set", name, "eth0", "ipv4.address="+ip)
	}
	return nil
}

// ---------------------------------------------------------------- commands

func cmdAddon(a *App) error {
	e, err := a.Env()
	if err != nil {
		return err
	}
	sub, list := a.args.Pos[0], a.args.Pos[1:]
	if sub == "ls" {
		var enabled []string
		project := ""
		if name, _, err := a.resolve(a.args.Project, false); err == nil {
			project, enabled = name, a.reg.Projects[name].Addons
		}
		for _, n := range sortedKeys(e.Cfg.Addons()) {
			mark := ""
			if project != "" {
				mark = "-       "
				if contains(enabled, n) {
					mark = color("32", "enabled ")
				}
			}
			fmt.Printf("%-10s %s%s\n", n, mark, e.Cfg.Addons()[n].Desc)
		}
		return nil
	}
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	if err := e.Cfg.CheckAddons(list); err != nil {
		return err
	}
	pr := a.reg.Projects[name]
	insts, err := a.instances()
	if err != nil {
		return err
	}
	_, hasInst := insts[name]
	switch sub {
	case "add":
		if len(list) == 0 {
			return usageErr{fmt.Errorf("usage: abox addon add <addon...>")}
		}
		for _, ad := range list {
			if !contains(pr.Addons, ad) {
				pr.Addons = append(pr.Addons, ad)
			}
		}
		if err := a.reg.Save(); err != nil {
			return err
		}
		info("add-ons for '%s': %s", name, strings.Join(pr.Addons, ", "))
		if hasInst {
			return a.ensureUp(name)
		}
		return nil
	case "rm":
		if len(list) == 0 {
			return usageErr{fmt.Errorf("usage: abox addon rm <addon...>")}
		}
		var keep []string
		for _, ad := range pr.Addons {
			if !contains(list, ad) {
				keep = append(keep, ad)
			}
		}
		pr.Addons = keep
		if err := a.reg.Save(); err != nil {
			return err
		}
		if hasInst {
			warn("removed add-ons stay installed in the sandbox until `abox rebuild`")
		}
		return nil
	case "update":
		if len(list) == 0 {
			list = pr.Addons
		}
		if err := a.ensureUp(name); err != nil {
			return err
		}
		return e.AddonInstall(name, list, true)
	}
	return usageErr{fmt.Errorf("usage: abox addon ls | add|rm|update <addon...>")}
}

func cmdPort(a *App) error {
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	pr := a.reg.Projects[name]
	pos := a.args.Pos
	if len(pos) == 0 || (pos[0] == "ls" && len(pos) == 1) {
		insts, err := a.instances()
		if err != nil {
			return err
		}
		if len(pr.Ports) == 0 {
			fmt.Println("no forwarded ports - add one with `abox port 3000` (or HOSTPORT:SANDBOXPORT)")
		}
		for _, ps := range pr.Ports {
			p, _ := parsePort(ps)
			state := "inactive (applied when the sandbox runs)"
			for _, ip := range insts[name].Ports {
				if ip == p {
					state = "active"
				}
			}
			fmt.Printf("http://localhost:%-6d -> sandbox :%-6d %s\n", p.Host, p.Guest, state)
		}
		return nil
	}
	remove := pos[0] == "rm"
	if remove {
		pos = pos[1:]
	}
	if len(pos) != 1 {
		return usageErr{fmt.Errorf("usage: abox port [ls] | abox port <port>|<hostport>:<sandboxport> | abox port rm <port>")}
	}
	p, err := parsePort(pos[0])
	if err != nil {
		return usageErr{err}
	}
	var keep []string
	found := false
	for _, ps := range pr.Ports {
		q, _ := parsePort(ps)
		if q.Host == p.Host {
			found = true
			continue
		}
		keep = append(keep, ps)
	}
	if remove {
		if !found {
			return fmt.Errorf("port %d is not forwarded for '%s'", p.Host, name)
		}
		pr.Ports = keep
	} else {
		pr.Ports = append(keep, p.String())
	}
	if err := a.reg.Save(); err != nil {
		return err
	}
	insts, err := a.instances()
	if err != nil {
		return err
	}
	if _, ok := insts[name]; !ok {
		info("saved; the port is forwarded once the sandbox exists")
		return nil
	}
	return a.ensure(name, agentsSkip) // reconciles ports (starts the sandbox if needed)
}
