package main

// Add-ons (extra tooling for agents, e.g. a headless browser) and port
// forwarding from the host to a sandbox.

import (
	"fmt"
	"io/fs"
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
		if err := a.ensure(name, agentsSkip); err != nil { // start only; the update below installs
			return err
		}
		return e.AddonInstall(name, list, true)
	}
	return usageErr{fmt.Errorf("usage: abox addon ls | add|rm|update <addon...>")}
}
