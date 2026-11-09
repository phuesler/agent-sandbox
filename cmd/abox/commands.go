package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// cmdRun runs an agent (default: the project's first agent) in autonomous mode.
func cmdRun(a *App) error {
	name, rel, err := a.resolve(a.args.Project, true)
	if err != nil {
		return err
	}
	e, err := a.Env()
	if err != nil {
		return err
	}
	agents := a.agentsOf(name)
	agent := agents[0]
	if len(a.args.Pos) == 1 {
		agent = a.args.Pos[0]
	}
	if err := e.CheckAgents([]string{agent}); err != nil {
		return err
	}
	if !contains(agents, agent) {
		if err := confirm(fmt.Sprintf("Agent '%s' is not enabled for '%s'. Enable and install it?", agent, name), a.args.Has("--yes")); err != nil {
			return err
		}
		a.reg.Projects[name].Agents = append(append([]string{}, agents...), agent)
		if err := a.reg.Save(); err != nil {
			return err
		}
	}
	if err := a.ensure(name, agentsLenient, agent); err != nil {
		return err
	}
	return e.sbxExec(workdirEnv(rel), append([]string{"run", name, agent}, a.args.Rest...)...)
}

func cmdAgents(a *App) error {
	e, err := a.Env()
	if err != nil {
		return err
	}
	avail := e.Agents()
	var enabled []string
	project := ""
	if name, _, err := a.resolve(a.args.Project, false); err == nil {
		project, enabled = name, a.agentsOf(name)
	}
	hdr := "AGENT      ENABLED   DESCRIPTION"
	if project == "" {
		hdr = "AGENT      DESCRIPTION"
	}
	fmt.Println(bold(hdr))
	for _, n := range sortedKeys(avail) {
		if project == "" {
			fmt.Printf("%-10s %s\n", n, avail[n].Desc)
			continue
		}
		mark := "-"
		if len(enabled) > 0 && enabled[0] == n {
			mark = color("32", "default")
		} else if contains(enabled, n) {
			mark = color("32", "yes    ")
		}
		fmt.Printf("%-10s %-9s %s\n", n, mark, avail[n].Desc)
	}
	if project != "" {
		fmt.Printf("\n(project '%s'; enable more with `abox agent add <agent>`)\n", project)
	}
	fmt.Println(bold("\nauthentication"))
	for _, n := range sortedKeys(avail) {
		if avail[n].Auth != "" {
			fmt.Printf("  %-9s %s\n", n+":", avail[n].Auth)
		}
	}
	return nil
}

func cmdAgent(a *App) error {
	sub, list := a.args.Pos[0], a.args.Pos[1:]
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	e, err := a.Env()
	if err != nil {
		return err
	}
	if err := e.CheckAgents(list); err != nil {
		return err
	}
	pr := a.reg.Projects[name]
	cur := append([]string{}, a.agentsOf(name)...)
	insts, err := a.instances()
	if err != nil {
		return err
	}
	_, hasInst := insts[name]
	switch sub {
	case "add":
		if len(list) == 0 {
			return usageErr{fmt.Errorf("usage: abox agent add <agent...>")}
		}
		for _, ag := range list {
			if !contains(cur, ag) {
				cur = append(cur, ag)
			}
		}
		pr.Agents = cur
		if err := a.reg.Save(); err != nil {
			return err
		}
		info("agents for '%s': %s", name, strings.Join(cur, ", "))
		if hasInst {
			return a.ensureUp(name)
		}
		return nil
	case "rm":
		if len(list) == 0 {
			return usageErr{fmt.Errorf("usage: abox agent rm <agent...>")}
		}
		var keep []string
		for _, ag := range cur {
			if !contains(list, ag) {
				keep = append(keep, ag)
			}
		}
		if len(keep) == 0 {
			return fmt.Errorf("a project needs at least one agent")
		}
		pr.Agents = keep
		if err := a.reg.Save(); err != nil {
			return err
		}
		info("agents for '%s': %s", name, strings.Join(keep, ", "))
		if hasInst {
			warn("removed agents stay installed in the sandbox until `abox rebuild`")
		}
		return nil
	case "default":
		if len(list) != 1 {
			return usageErr{fmt.Errorf("usage: abox agent default <agent>")}
		}
		next := []string{list[0]}
		for _, ag := range cur {
			if ag != list[0] {
				next = append(next, ag)
			}
		}
		pr.Agents = next
		if err := a.reg.Save(); err != nil {
			return err
		}
		info("default agent for '%s': %s", name, list[0])
		if hasInst && !contains(cur, list[0]) {
			return a.ensureUp(name)
		}
		return nil
	case "update":
		if len(list) == 0 {
			list = cur
		}
		for _, ag := range list {
			if !contains(cur, ag) {
				return fmt.Errorf("agent '%s' is not enabled for '%s'", ag, name)
			}
		}
		if err := a.ensureUp(name); err != nil {
			return err
		}
		return e.sbx(nil, append(append([]string{"agent-install", name}, list...), "--update")...)
	}
	return usageErr{fmt.Errorf("usage: abox agent add|rm|default|update <agent...>")}
}

func cmdSecret(a *App) error {
	sub := a.args.Pos[0]
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	pr := a.reg.Projects[name]
	insts, err := a.instances()
	if err != nil {
		return err
	}
	inst, hasInst := insts[name]
	switch sub {
	case "ls":
		if len(a.args.Pos) != 1 {
			return usageErr{fmt.Errorf("usage: abox secret ls")}
		}
		if len(pr.Secrets) == 0 {
			fmt.Println("no secrets - add one with `abox secret set KEY FILE`")
		}
		for _, k := range sortedKeys(pr.Secrets) {
			state := "pending (applied on next abox run/up)"
			if hasInst && contains(inst.Secrets, k) {
				state = "set in sandbox"
			}
			fmt.Printf("%-24s <- %s  [%s]\n", k, pr.Secrets[k], state)
		}
		return nil
	case "set":
		if len(a.args.Pos) != 3 {
			return usageErr{fmt.Errorf("usage: abox secret set KEY FILE   (the file holds the value; only its path is stored)")}
		}
		key, file := a.args.Pos[1], canon(a.args.Pos[2])
		if !envKeyRe.MatchString(key) {
			return fmt.Errorf("invalid variable name '%s'", key)
		}
		if f, err := os.Open(file); err != nil {
			return fmt.Errorf("cannot read %s: %v", file, err)
		} else {
			f.Close()
		}
		if pr.Secrets == nil {
			pr.Secrets = map[string]string{}
		}
		pr.Secrets[key] = file
		if err := a.reg.Save(); err != nil {
			return err
		}
		if !hasInst {
			info("secret %s registered; it is set when the sandbox is created", key)
			return nil
		}
		if err := a.ensure(name, agentsSkip); err != nil { // pushes secrets the sandbox doesn't have yet
			return err
		}
		if contains(inst.Secrets, key) { // existed before: push the (possibly new) value
			return a.env.sbx(nil, "secret", "set", name, key, file)
		}
		return nil
	case "rm":
		if len(a.args.Pos) != 2 {
			return usageErr{fmt.Errorf("usage: abox secret rm KEY")}
		}
		key := a.args.Pos[1]
		if _, ok := pr.Secrets[key]; !ok && !(hasInst && contains(inst.Secrets, key)) {
			return fmt.Errorf("no secret '%s' for '%s'", key, name)
		}
		delete(pr.Secrets, key)
		if err := a.reg.Save(); err != nil {
			return err
		}
		if hasInst {
			if err := a.ensure(name, agentsSkip); err != nil {
				return err
			}
			return a.env.sbx(nil, "secret", "rm", name, key)
		}
		return nil
	case "sync":
		if len(a.args.Pos) != 1 {
			return usageErr{fmt.Errorf("usage: abox secret sync")}
		}
		if err := a.ensure(name, agentsSkip); err != nil {
			return err
		}
		for _, k := range sortedKeys(pr.Secrets) {
			if err := a.env.sbx(nil, "secret", "set", name, k, expandHome(pr.Secrets[k])); err != nil {
				return err
			}
		}
		info("re-pushed %d secret(s) to '%s'", len(pr.Secrets), name)
		return nil
	}
	return usageErr{fmt.Errorf("usage: abox secret set KEY FILE | rm KEY | ls | sync")}
}

func cmdShell(a *App) error {
	name, rel, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	if err := a.ensure(name, agentsSkip); err != nil {
		return err
	}
	return a.env.sbxExec(workdirEnv(rel), a.args.Cmd, name) // "shell" or "root"
}

func cmdExec(a *App) error {
	if len(a.args.Rest) == 0 {
		return usageErr{fmt.Errorf("usage: abox exec [-p P] -- <command...>")}
	}
	name, rel, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	if err := a.ensure(name, agentsSkip); err != nil {
		return err
	}
	return a.env.sbxExec(workdirEnv(rel), append([]string{"exec", name, "--"}, a.args.Rest...)...)
}

func cmdAdd(a *App) error {
	path := "."
	if len(a.args.Pos) == 1 {
		path = a.args.Pos[0]
	}
	o := ProjectOpts{Name: a.args.Val("--name"), Image: a.args.Val("--image"), Memory: a.args.Val("--memory"),
		Disk: a.args.Val("--disk"), Agents: splitAgents(a.args.Val("--agent"))}
	if a.args.Has("--vm") && a.args.Has("--container") {
		return usageErr{fmt.Errorf("--vm and --container are mutually exclusive")}
	}
	if a.args.Has("--vm") {
		o.Type = "vm"
	} else if a.args.Has("--container") {
		o.Type = "container"
	}
	if o.Image != "" && o.Image != "ubuntu" && o.Image != "nixos" {
		return usageErr{fmt.Errorf("--image must be ubuntu or nixos")}
	}
	if v := a.args.Val("--cpus"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return usageErr{fmt.Errorf("--cpus must be a positive integer")}
		}
		o.CPUs = n
	}
	name, err := a.register(canon(path), o)
	if err != nil {
		return err
	}
	if a.args.Has("--no-create") {
		return nil
	}
	return a.ensureUp(name)
}

func cmdLs(a *App) error {
	insts, err := a.instances()
	if err != nil {
		return err
	}
	egress := egressStatus()
	home, _ := os.UserHomeDir()
	names := map[string]bool{}
	for n := range a.reg.Projects {
		names[n] = true
	}
	for n := range insts {
		names[n] = true
	}
	if len(names) == 0 {
		fmt.Println("no projects yet - run `abox` inside a project directory")
		return nil
	}
	hdr := []string{"NAME", "TYPE", "IMAGE", "AGENTS", "STATE", "IP", "EGRESS", "SNAPS", "PATH"}
	rows := [][]string{}
	unregistered := false
	for _, n := range sortedKeys(names) {
		pr, isReg := a.reg.Projects[n]
		inst, hasInst := insts[n]
		row := make([]string, len(hdr))
		row[0] = n
		agents := []string{}
		if isReg {
			agents = a.agentsOf(n)
		} else {
			row[0] += " *"
			unregistered = true
			pr = &Project{Path: inst.Path, Type: inst.Type, Image: inst.Image}
			agents = inst.Agents
		}
		row[1], row[2], row[3], row[4], row[8] = pr.Type, pr.Image, strings.Join(agents, ","), "absent", pr.Path
		row[6] = "-"
		if hasInst {
			row[1], row[4], row[5], row[7] = inst.Type, inst.Status, inst.IP, strconv.Itoa(inst.Snaps)
			if inst.IP != "" {
				switch st, ok := egress[inst.IP]; {
				case egress == nil:
					row[6] = "?"
				case ok:
					row[6] = st
				default:
					row[6] = "allowlist"
				}
			}
		}
		if home != "" && strings.HasPrefix(row[8], home+"/") {
			row[8] = "~" + row[8][len(home):]
		}
		rows = append(rows, row)
	}
	w := make([]int, len(hdr))
	for i, h := range hdr {
		w[i] = len(h)
		for _, r := range rows {
			w[i] = max(w[i], len(r[i]))
		}
	}
	pad := func(s string, n int) string { return s + strings.Repeat(" ", n-len(s)) }
	line := make([]string, len(hdr))
	for i, h := range hdr {
		line[i] = pad(h, w[i])
	}
	fmt.Println(bold(strings.TrimRight(strings.Join(line, "  "), " ")))
	stateColor := map[string]string{"running": "32", "stopped": "2", "absent": "33"}
	for _, r := range rows {
		for i, x := range r {
			line[i] = pad(x, w[i])
		}
		if c, ok := stateColor[r[4]]; ok {
			line[4] = color(c, line[4])
		}
		if strings.HasPrefix(r[6], "open") {
			line[6] = color("1;31", line[6])
		}
		fmt.Println(strings.TrimRight(strings.Join(line, "  "), " "))
	}
	if unregistered {
		fmt.Println("\n* sandbox exists but is not registered: `abox import`")
	}
	return nil
}

func cmdStatus(a *App) error {
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	pr := a.reg.Projects[name]
	fmt.Println(bold(name))
	kv := [][2]string{{"path", pr.Path}, {"type", pr.Type}, {"image", pr.Image},
		{"agents", strings.Join(a.agentsOf(name), ", ") + " (first = default)"}, {"memory", pr.Memory}, {"disk", pr.Disk}}
	for _, k := range sortedKeys(pr.Secrets) {
		kv = append(kv, [2]string{"secret", k + " <- " + pr.Secrets[k]})
	}
	if pr.CPUs > 0 {
		kv = append(kv, [2]string{"cpus", strconv.Itoa(pr.CPUs)})
	}
	for _, x := range kv {
		if x[1] != "" {
			fmt.Printf("  %-13s%s\n", x[0], x[1])
		}
	}
	insts, err := a.instances()
	if err != nil {
		return err
	}
	inst, ok := insts[name]
	if !ok {
		fmt.Println("  sandbox      absent (created on first `abox` / `abox up`)")
		return nil
	}
	ip := inst.IP
	if ip == "" {
		ip = "-"
	}
	fmt.Printf("  state        %s  ip %s\n", inst.Status, ip)
	eg := "?"
	if e := egressStatus(); e != nil {
		eg = "allowlist"
		if st, ok := e[inst.IP]; ok {
			eg = st
		}
	}
	fmt.Printf("  egress       %s\n", eg)
	return runIO("incus", "snapshot", "list", name, "-c", "nT")
}

func cmdUp(a *App) error {
	names := a.reg.Names()
	if !a.args.Has("--all") {
		name, _, err := a.resolve(a.args.Project, false)
		if err != nil {
			return err
		}
		names = []string{name}
	}
	for _, n := range names {
		if err := a.ensureUp(n); err != nil {
			return err
		}
	}
	return nil
}

func cmdDown(a *App) error {
	insts, err := a.instances()
	if err != nil {
		return err
	}
	names := a.reg.Names()
	if !a.args.Has("--all") {
		name, _, err := a.resolve(a.args.Project, false)
		if err != nil {
			return err
		}
		names = []string{name}
	}
	for _, n := range names {
		if inst, ok := insts[n]; ok && inst.Status == "running" {
			info("stopping '%s'", n)
			if err := runIO("incus", "stop", n); err != nil {
				return err
			}
		}
	}
	return nil
}

func cmdSnap(a *App) error {
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	e, err := a.Env()
	if err != nil {
		return err
	}
	return e.sbx(nil, append([]string{"snap", name}, a.args.Pos...)...)
}

func cmdRestore(a *App) error {
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	label := "clean"
	if len(a.args.Pos) == 1 {
		label = a.args.Pos[0]
	}
	if err := confirm(fmt.Sprintf("Restore '%s' to snapshot '%s'? Changes to the sandbox OS since then are lost (files in the project dir are NOT affected).", name, label), a.args.Has("--yes")); err != nil {
		return err
	}
	e, err := a.Env()
	if err != nil {
		return err
	}
	if err := e.sbx(nil, "restore", name, label); err != nil {
		return err
	}
	return a.ensure(name, agentsLenient) // reinstall agents/secrets added after that snapshot
}

func cmdRebuild(a *App) error {
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	if err := confirm(fmt.Sprintf("Delete and recreate sandbox '%s' from its registry settings? (project dir is untouched; logins inside the sandbox are lost)", name), a.args.Has("--yes")); err != nil {
		return err
	}
	insts, err := a.instances()
	if err != nil {
		return err
	}
	if _, ok := insts[name]; ok {
		if err := a.env.sbx(nil, "rm", name, "-f"); err != nil {
			return err
		}
	}
	return a.ensure(name, agentsLenient)
}

func cmdRm(a *App) error {
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	insts, err := a.instances()
	if err != nil {
		return err
	}
	_, exists := insts[name]
	keep := a.args.Has("--keep-sandbox")
	if exists && !keep {
		if err := confirm(fmt.Sprintf("Delete sandbox '%s' (and its snapshots) and unregister it? Project dir is untouched.", name), a.args.Has("--yes")); err != nil {
			return err
		}
		if err := a.env.sbx(nil, "rm", name, "-f"); err != nil {
			return err
		}
	}
	delete(a.reg.Projects, name)
	if err := a.reg.Save(); err != nil {
		return err
	}
	if exists && keep {
		info("unregistered '%s' (sandbox kept)", name)
	} else {
		info("unregistered '%s'", name)
	}
	return nil
}

var (
	settable   = []string{"type", "image", "cpus", "memory", "disk"}
	liveKeys   = map[string]string{"cpus": "limits.cpu", "memory": "limits.memory"}
	rebuildKey = map[string]bool{"type": true, "image": true, "disk": true}
)

type setting struct{ key, val string }

// parseSettings validates all key=value pairs before anything is applied.
func parseSettings(pairs []string) ([]setting, error) {
	var out []setting
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		valid := false
		for _, s := range settable {
			valid = valid || s == k
		}
		if !ok || !valid {
			return nil, fmt.Errorf("bad setting '%s' (keys: %s)", kv, strings.Join(settable, ", "))
		}
		switch k {
		case "type":
			if v != "container" && v != "vm" {
				return nil, fmt.Errorf("type must be container or vm")
			}
		case "image":
			if v != "ubuntu" && v != "nixos" {
				return nil, fmt.Errorf("image must be ubuntu or nixos")
			}
		case "cpus":
			if n, err := strconv.Atoi(v); v != "" && (err != nil || n < 1) {
				return nil, fmt.Errorf("cpus must be a positive integer")
			}
		}
		out = append(out, setting{k, v})
	}
	return out, nil
}

func applySetting(pr *Project, s setting) {
	switch s.key {
	case "type":
		pr.Type = s.val
	case "image":
		pr.Image = s.val
	case "cpus":
		pr.CPUs, _ = strconv.Atoi(s.val) // "" -> 0 (unset)
	case "memory":
		pr.Memory = s.val
	case "disk":
		pr.Disk = s.val
	}
}

func cmdSet(a *App) error {
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	settings, err := parseSettings(a.args.Pos)
	if err != nil {
		return usageErr{err}
	}
	insts, err := a.instances()
	if err != nil {
		return err
	}
	_, hasInst := insts[name]
	pr := a.reg.Projects[name]
	needRebuild := false
	for _, s := range settings {
		applySetting(pr, s)
		if !hasInst {
			continue
		}
		if key, live := liveKeys[s.key]; live {
			var err error
			if s.val == "" {
				err = runIO("incus", "config", "unset", name, key) // back to profile default
			} else {
				err = runIO("incus", "config", "set", name, key+"="+s.val)
			}
			if err != nil {
				return err
			}
			info("%s applied live", s.key)
		} else if rebuildKey[s.key] {
			needRebuild = true
		}
	}
	if err := a.reg.Save(); err != nil {
		return err
	}
	if needRebuild {
		warn("some changes only take effect after `abox -p %s rebuild`", name)
	}
	return nil
}

func cmdNet(a *App) error {
	e, err := a.Env()
	if err != nil {
		return err
	}
	action := a.args.Pos[0]
	var arg []string
	if len(a.args.Pos) == 2 {
		if n, err := strconv.Atoi(a.args.Pos[1]); err != nil || n < 1 {
			return usageErr{fmt.Errorf("expected a positive number, got '%s'", a.args.Pos[1])}
		}
		arg = a.args.Pos[1:]
	}
	switch action {
	case "log":
		return e.sbx(nil, append([]string{"net", "log"}, arg...)...)
	case "open", "close":
		if action == "close" && arg != nil {
			return usageErr{fmt.Errorf("usage: abox net close")}
		}
		name, _, err := a.resolve(a.args.Project, false)
		if err != nil {
			return err
		}
		if err := a.ensure(name, agentsSkip); err != nil {
			return err
		}
		return e.sbx(nil, append([]string{"net", action, name}, arg...)...)
	}
	return usageErr{fmt.Errorf("usage: abox net open [minutes] | close | log [lines]")}
}

func cmdImport(a *App) error {
	insts, err := a.instances()
	if err != nil {
		return err
	}
	n := 0
	for _, name := range sortedKeys(insts) {
		inst := insts[name]
		if _, ok := a.reg.Projects[name]; ok || inst.Path == "" {
			continue
		}
		a.reg.Projects[name] = &Project{Path: inst.Path, Type: inst.Type, Image: inst.Image}
		info("imported '%s' -> %s", name, inst.Path)
		n++
	}
	if n == 0 {
		fmt.Println("nothing to import")
		return nil
	}
	return a.reg.Save()
}

func cmdDoctor(a *App) error {
	ok := true
	check := func(label string, good bool, hint string) {
		ok = ok && good
		mark := color("32", "ok ")
		if !good {
			mark = color("31", "BAD")
		}
		fmt.Printf("  %s %s\n", mark, label)
		if !good && hint != "" {
			fmt.Printf("      -> %s\n", hint)
		}
	}
	fmt.Println(bold("host"))
	e, envErr := a.Env()
	check("agent-sandbox repo found", envErr == nil, fmt.Sprint(envErr))
	_, lookErr := exec.LookPath("incus")
	check("incus installed", lookErr == nil, "sudo ./install-host.sh")
	groups, _, _ := capture("id", "-nG")
	check("incus-admin group active in this shell", strings.Contains(" "+groups+" ", " incus-admin "), "log out/in or `newgrp incus-admin`")
	var insts map[string]Instance
	if envErr == nil {
		var err error
		insts, err = e.Instances()
		hint := ""
		if err != nil {
			hint = strings.SplitN(err.Error(), "\n", 2)[0]
		}
		check("incus reachable", err == nil, hint)
	}
	repo := "<repo>"
	if e != nil {
		repo = e.Repo
	}
	for _, unit := range []string{"sbx-proxy", "sbx-egress"} {
		out, _, _ := capture("systemctl", "is-active", unit)
		check(unit+".service active", strings.TrimSpace(out) == "active",
			fmt.Sprintf("sudo systemctl status %s; sudo %s/egress/apply.sh", unit, repo))
	}
	check("sbx-net usable via sudo", egressStatus() != nil, fmt.Sprintf("sudo %s/egress/apply.sh (installs the sudoers rule)", repo))
	_, _, profErr := capture("incus", "profile", "show", "sbx-base")
	check("sbx profiles loaded", profErr == nil, "sbx init")

	fmt.Println(bold("projects"))
	if len(a.reg.Projects) == 0 {
		fmt.Println("  (none)")
	}
	for _, name := range a.reg.Names() {
		pr := a.reg.Projects[name]
		fi, err := os.Stat(pr.Path)
		check(fmt.Sprintf("%s: %s exists", name, pr.Path), err == nil && fi.IsDir(),
			fmt.Sprintf("`abox -p %s rm` or edit %s", name, registryPath()))
		if e != nil {
			err := e.CheckAgents(a.agentsOf(name))
			check(fmt.Sprintf("%s: agents %s available", name, strings.Join(a.agentsOf(name), ",")), err == nil,
				fmt.Sprint(err))
		}
		for _, k := range sortedKeys(pr.Secrets) {
			f, err := os.Open(expandHome(pr.Secrets[k]))
			if err == nil {
				f.Close()
			}
			check(fmt.Sprintf("%s: secret %s readable (%s)", name, k, pr.Secrets[k]), err == nil,
				fmt.Sprintf("abox -p %s secret set %s <file>", name, k))
		}
		if inst, ok := insts[name]; ok && inst.Path != "" && inst.Path != pr.Path {
			check(fmt.Sprintf("%s: sandbox shares %s", name, inst.Path), false, fmt.Sprintf("abox -p %s rebuild", name))
		}
	}
	for _, name := range sortedKeys(insts) {
		if _, ok := a.reg.Projects[name]; !ok {
			check(fmt.Sprintf("sandbox '%s' registered", name), false, "abox import")
		}
	}
	if !ok {
		return exitCode(1)
	}
	return nil
}

func cmdEdit(a *App) error {
	if _, err := os.Stat(registryPath()); err != nil {
		if err := a.reg.Save(); err != nil {
			return err
		}
	}
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "nano"
	}
	parts := strings.Fields(ed)
	bin, err := exec.LookPath(parts[0])
	if err != nil {
		return fmt.Errorf("editor %q not found", parts[0])
	}
	return syscall.Exec(bin, append(parts, registryPath()), os.Environ())
}

func cmdNames(a *App) error {
	for _, n := range a.reg.Names() {
		fmt.Println(n)
	}
	return nil
}

func cmdAgentNames(a *App) error {
	if e, err := a.Env(); err == nil {
		for _, n := range sortedKeys(e.Agents()) {
			fmt.Println(n)
		}
	}
	return nil
}

func cmdVersion(*App) error {
	fmt.Println("abox", version)
	return nil
}

func cmdCompletion(*App) error {
	fmt.Printf(`# bash completion for abox - add to ~/.bashrc:  eval "$(abox completion)"
_abox() {
  local cur=${COMP_WORDS[COMP_CWORD]} prev=${COMP_WORDS[COMP_CWORD-1]}
  if [[ $prev == -p || $prev == --project ]]; then
    COMPREPLY=($(compgen -W "$(abox _names 2>/dev/null)" -- "$cur")); return
  fi
  if [[ $COMP_CWORD -eq 1 ]]; then
    COMPREPLY=($(compgen -W "%s $(abox _agents 2>/dev/null) -p" -- "$cur")); return
  fi
  case ${COMP_WORDS[1]} in
    net) [[ $COMP_CWORD -eq 2 ]] && COMPREPLY=($(compgen -W "open close log" -- "$cur")) ;;
    agent) if [[ $COMP_CWORD -eq 2 ]]; then COMPREPLY=($(compgen -W "add rm default update" -- "$cur"))
           else COMPREPLY=($(compgen -W "$(abox _agents 2>/dev/null)" -- "$cur")); fi ;;
    run) COMPREPLY=($(compgen -W "$(abox _agents 2>/dev/null)" -- "$cur")) ;;
    secret) [[ $COMP_CWORD -eq 2 ]] && COMPREPLY=($(compgen -W "set rm ls sync" -- "$cur")) ;;
    add) COMPREPLY=($(compgen -d -- "$cur")) ;;
    set) COMPREPLY=($(compgen -W "type= image= cpus= memory= disk=" -- "$cur")); compopt -o nospace ;;
    *)   COMPREPLY=($(compgen -W "-p" -- "$cur")) ;;
  esac
}
complete -F _abox abox
`, strings.Join(commandOrder, " "))
	return nil
}
