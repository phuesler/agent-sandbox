package main

// Incus operations on single sandboxes.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PATH inside guests (covers NixOS, where su lives in /run/wrappers/bin).
const guestPath = "/run/wrappers/bin:/run/current-system/sw/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

var profileNames = []string{"base", "container", "vm", "nixos-base"}

func incusOut(args ...string) (string, error) {
	out, errOut, err := capture("incus", args...)
	if err != nil {
		return "", fmt.Errorf("incus %s: %s", strings.Join(shortArgs(args), " "), strings.TrimSpace(errOut))
	}
	return strings.TrimSpace(out), nil
}

func incusQuiet(args ...string) error { _, err := incusOut(args...); return err }

// pushFile writes content to <name>:<dest> with the given octal mode.
func pushFile(name, dest, content, mode string) error {
	f, err := os.CreateTemp("", "abox-push-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return incusQuiet("file", "push", f.Name(), name+dest, "--mode", mode)
}

// ---------------------------------------------------------------- instance config lists

func (e *Env) getList(name, key string) []string {
	out, _ := incusOut("config", "get", name, key)
	return splitList(out)
}

func (e *Env) setList(name, key string, list []string) error {
	return incusQuiet("config", "set", name, key+"="+strings.Join(list, ","))
}

func (e *Env) listAdd(name, key string, items ...string) error {
	cur := e.getList(name, key)
	for _, x := range items {
		if !contains(cur, x) {
			cur = append(cur, x)
		}
	}
	return e.setList(name, key, cur)
}

func (e *Env) listDel(name, key string, items ...string) error {
	var keep []string
	for _, x := range e.getList(name, key) {
		if !contains(items, x) {
			keep = append(keep, x)
		}
	}
	return e.setList(name, key, keep)
}

// ---------------------------------------------------------------- readiness / running things as the guest user

// WaitReady waits until incus exec works and the guest can reach the egress
// proxy (after start/restore the network comes up a few seconds after the agent).
func (e *Env) WaitReady(name string) error {
	ok := false
	for i := 0; i < 120 && !ok; i++ {
		if _, _, err := capture("incus", "exec", name, "--", "/bin/sh", "-c", "true"); err == nil {
			ok = true
		} else {
			time.Sleep(2 * time.Second)
		}
	}
	if !ok {
		return fmt.Errorf("%s did not become reachable via incus exec", name)
	}
	// Let boot finish so services (dockerd, ...) are up. Right after start systemd's
	// bus isn't there yet and `--wait` fails immediately, so retry until it reports
	// a final state ("degraded" is common in containers and fine).
	// On a new sandbox this includes cloud-init (a few minutes). Capped, so a hung
	// guest unit can't block abox indefinitely.
	booted := false
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); {
		out, _, _ := capture("incus", "exec", name, "--env", "PATH="+guestPath, "--",
			"timeout", "60", "systemctl", "is-system-running", "--wait")
		if s := strings.TrimSpace(out); s == "running" || s == "degraded" || s == "maintenance" {
			booted = true
			break
		}
		time.Sleep(time.Second)
	}
	if !booted {
		warn("%s has not finished booting after 10 minutes; continuing anyway (check: abox root, systemctl list-jobs)", name)
	}
	probe := fmt.Sprintf("exec 3<>/dev/tcp/%s/%s", e.Cfg.HostIP(), e.Cfg.ProxyPort)
	for i := 0; i < 60; i++ {
		if _, _, err := capture("incus", "exec", name, "--env", "PATH="+guestPath, "--", "bash", "-c", probe); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s cannot reach the egress proxy at %s (is abox-proxy running? `abox doctor`)", name, e.Cfg.ProxyURL())
}

var cwdRe = regexp.MustCompile(`^/work(/.*)?$`)

// userArgs builds `incus exec ... su -l <guest user> -c <script>`: a login shell
// (proxy env), ~/.local/bin on PATH, ~/.abox-env secrets loaded, cwd below /work.
func (e *Env) userArgs(name, cwd, cmd string, tty bool) []string {
	if cwd == "" || !cwdRe.MatchString(cwd) || strings.Contains(cwd, "/..") {
		cwd = "/work"
	}
	script := `export PATH="$HOME/.local/bin:$PATH"; cd ` + shq(cwd) +
		` 2>/dev/null || cd /work 2>/dev/null || cd; [ -f ~/.abox-env ] && . ~/.abox-env; ` + cmd
	mode := "-T"
	if tty {
		mode = "-t"
	}
	return []string{"exec", mode, name, "--env", "PATH=" + guestPath, "--", "su", "-l", e.Cfg.GuestUser, "-c", script}
}

// execIncus replaces the abox process with incus (interactive sessions).
func execIncus(args []string) error {
	bin, err := exec.LookPath("incus")
	if err != nil {
		return err
	}
	return syscall.Exec(bin, append([]string{"incus"}, args...), os.Environ())
}

func interactive() bool { return isTTY(os.Stdin) && isTTY(os.Stdout) }

// RunAgent replaces abox with the agent, started in the given permission mode.
func (e *Env) RunAgent(name, agent, mode, cwd string, args []string) error {
	ai, ok := e.Agents()[agent]
	if !ok {
		return e.CheckAgents([]string{agent})
	}
	start, ok := ai.Cmds[mode]
	if !ok {
		return fmt.Errorf("agent '%s' has no '%s' mode (AGENT_CMD_%s missing in its agent.conf)", agent, mode, strings.ToUpper(mode))
	}
	if !contains(e.getList(name, "user.abox.agents"), agent) {
		return fmt.Errorf("agent '%s' is not installed in %s (abox agent add %s)", agent, name, agent)
	}
	info("starting %s in %s mode", agent, mode)
	cmd := "exec " + start
	if len(args) > 0 {
		cmd += " " + shqJoin(args)
	}
	return execIncus(e.userArgs(name, cwd, cmd, interactive()))
}

func (e *Env) Shell(name, cwd string) error {
	return execIncus(e.userArgs(name, cwd, "exec bash -l", interactive()))
}

func (e *Env) Root(name string) error {
	return execIncus([]string{"exec", name, "--env", "PATH=" + guestPath, "--", "bash", "-l"})
}

func (e *Env) Exec(name, cwd string, argv []string) error {
	return execIncus(e.userArgs(name, cwd, shqJoin(argv), interactive()))
}

// asUser runs a command as the guest user without a terminal.
func (e *Env) asUser(name, cmd string) error {
	return runStdin(nil, "incus", e.userArgs(name, "", cmd, false)...)
}

// ---------------------------------------------------------------- profiles / lifecycle

func (e *Env) InitProfiles() error {
	for _, p := range profileNames {
		prof := "abox-" + p
		if _, _, err := capture("incus", "profile", "show", prof); err != nil {
			if err := incusQuiet("profile", "create", prof); err != nil {
				return err
			}
		}
		yaml := e.Cfg.render(asset("profiles/"+prof+".yaml"), e.UID)
		if err := runStdin(strings.NewReader(yaml), "incus", "profile", "edit", prof); err != nil {
			return err
		}
	}
	return nil
}

type CreateOpts struct {
	Name, Path, Type, Image, Memory, Disk string
	CPUs                                  int
	Agents                                []string
	Secrets                               map[string]string // env var -> host file
}

// Create builds a sandbox: only o.Path is shared (at /work); provisions the OS,
// pushes secrets, installs agents and takes the 'clean' snapshot. A failing
// agent install doesn't prevent the snapshot; the error is returned afterwards.
func (e *Env) Create(o CreateOpts) error {
	if err := e.CheckAgents(o.Agents); err != nil {
		return err
	}
	if err := e.InitProfiles(); err != nil {
		return err
	}
	var img string
	args := []string{"init"}
	switch o.Image {
	case "ubuntu":
		img = e.Cfg.UbuntuImage
		args = append(args, img, o.Name, "-p", "abox-base", "-p", "abox-"+o.Type)
	case "nixos":
		img = e.Cfg.NixosImage
		args = append(args, img, o.Name, "-p", "abox-nixos-base", "-p", "abox-"+o.Type)
		if o.Type == "vm" {
			args = append(args, "-c", "security.secureboot=false")
		}
	default:
		return fmt.Errorf("image must be ubuntu or nixos")
	}
	if o.Type == "vm" {
		args = append(args, "--vm")
	}
	if o.CPUs > 0 {
		args = append(args, "-c", "limits.cpu="+strconv.Itoa(o.CPUs))
	}
	if o.Memory != "" {
		args = append(args, "-c", "limits.memory="+o.Memory)
	}
	if o.Disk != "" {
		args = append(args, "-d", "root,size="+o.Disk)
	}
	args = append(args, "-c", "user.abox=true", "-c", "user.abox.path="+o.Path, "-c", "user.abox.image="+o.Image)

	info("creating %s '%s' from %s", o.Type, o.Name, img)
	if err := runIO("incus", args...); err != nil {
		return err
	}
	dev := []string{"config", "device", "add", o.Name, "work", "disk", "source=" + o.Path, "path=/work"}
	if o.Type == "container" {
		dev = append(dev, "shift=true")
	}
	if err := incusQuiet(dev...); err != nil {
		return err
	}
	info("sharing %s -> %s:/work (nothing else from the host)", o.Path, o.Name)
	if err := runIO("incus", "start", o.Name); err != nil {
		return err
	}
	if err := e.WaitReady(o.Name); err != nil {
		return err
	}

	if o.Image == "ubuntu" {
		info("waiting for cloud-init provisioning (Docker, base tools) ...")
		_, _, _ = capture("incus", "exec", o.Name, "--", "cloud-init", "status", "--wait")
		if _, _, err := capture("incus", "exec", o.Name, "--", "test", "-f", "/var/lib/abox/provisioned"); err != nil {
			_ = runStdin(nil, "incus", "exec", o.Name, "--", "tail", "-n", "40", "/var/log/cloud-init-output.log")
			return fmt.Errorf("provisioning failed. Inspect with `abox root`; blocked downloads show in `abox net log`")
		}
	} else if err := e.provisionNixos(o.Name); err != nil {
		return err
	}

	for _, k := range sortedKeys(o.Secrets) {
		if err := e.SecretSet(o.Name, k, o.Secrets[k]); err != nil {
			return err
		}
	}
	var failed []string
	for _, a := range o.Agents {
		if err := e.AgentInstall(o.Name, []string{a}, false); err != nil {
			warn("%v", err)
			failed = append(failed, a)
		}
	}
	if err := incusQuiet("snapshot", "create", o.Name, "clean"); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("sandbox ready (snapshot 'clean' taken) but these agents failed to install: %s", strings.Join(failed, ", "))
	}
	info("ready. snapshot 'clean' taken")
	return nil
}

func (e *Env) Start(name string) error {
	if err := runIO("incus", "start", name); err != nil {
		return err
	}
	return e.WaitReady(name)
}

func (e *Env) Stop(name string) error { return runIO("incus", "stop", name) }

func (e *Env) Delete(name string) error {
	if insts, err := e.Instances(); err == nil && insts[name].IP != "" {
		_, _, _ = capture("sudo", "-n", aboxNet, "close", insts[name].IP)
	}
	return runIO("incus", "delete", "--force", name)
}

func (e *Env) Snap(name, label string) error {
	if label == "" {
		label = time.Now().Format("20060102-150405")
	}
	return runIO("incus", "snapshot", "create", name, label)
}

func (e *Env) Restore(name, label string) error {
	if err := runIO("incus", "snapshot", "restore", name, label); err != nil {
		return err
	}
	if insts, err := e.Instances(); err == nil && insts[name].Status == "running" {
		return e.WaitReady(name)
	}
	return nil
}

// ---------------------------------------------------------------- NixOS

func (e *Env) nixosRebuild(name string, upgrade bool) error {
	script := fmt.Sprintf("export http_proxy=%s https_proxy=%s\n", e.Cfg.ProxyURL(), e.Cfg.ProxyURL())
	if upgrade {
		script += "nix-channel --update\n"
	}
	script += "nixos-rebuild switch\n"
	return runStdin(nil, "incus", "exec", name, "--env", "PATH="+guestPath, "--", "sh", "-euc", script)
}

// nixosAgentsFile writes /etc/nixos/abox-agents.nix with the agents' nixpkgs.
func (e *Env) nixosAgentsFile(name string, agents []string) error {
	var pkgs []string
	for _, a := range agents {
		p := e.Agents()[a].NixPkg
		if p == "" {
			return fmt.Errorf("agent '%s' has no AGENT_NIXPKG, cannot install it on NixOS", a)
		}
		pkgs = append(pkgs, p)
	}
	nix := "# generated by abox: coding agents of this sandbox\n{ pkgs, ... }:\n{\n  environment.systemPackages = with pkgs; [ " +
		strings.Join(pkgs, " ") + " ];\n}\n"
	return pushFile(name, "/etc/nixos/abox-agents.nix", nix, "0644")
}

func (e *Env) provisionNixos(name string) error {
	info("NixOS: pushing /etc/nixos/abox.nix and rebuilding (first run downloads a lot)")
	if err := pushFile(name, "/etc/nixos/abox.nix", e.Cfg.render(asset("nixos/abox.nix"), e.UID), "0644"); err != nil {
		return err
	}
	if err := e.nixosAgentsFile(name, nil); err != nil { // agents are added by AgentInstall
		return err
	}
	p := e.Cfg.ProxyURL()
	script := `f=/etc/nixos/configuration.nix
grep -q './abox.nix' $f || sed -i '0,/imports = \[/s//imports = [ .\/abox.nix/' $f
grep -q './abox.nix' $f || { echo 'could not add import to configuration.nix' >&2; exit 1; }
systemctl set-environment http_proxy=` + p + ` https_proxy=` + p + `
systemctl restart nix-daemon
export http_proxy=` + p + ` https_proxy=` + p + `
nix-channel --list | grep -q . || nix-channel --add https://nixos.org/channels/nixos-unstable nixos
nix-channel --update
nixos-rebuild switch
systemctl unset-environment http_proxy https_proxy
mkdir -p /var/lib/abox && date -Is > /var/lib/abox/provisioned
`
	return runStdin(nil, "incus", "exec", name, "--env", "PATH="+guestPath, "--", "sh", "-euc", script)
}

// ---------------------------------------------------------------- agents

// AgentInstall installs (or with update: reinstalls/upgrades) agents in a sandbox.
func (e *Env) AgentInstall(name string, agents []string, update bool) error {
	if err := e.CheckAgents(agents); err != nil {
		return err
	}
	if err := e.WaitReady(name); err != nil {
		return err
	}
	if img, _ := incusOut("config", "get", name, "user.abox.image"); img == "nixos" {
		all := e.getList(name, "user.abox.agents")
		for _, a := range agents {
			if !contains(all, a) {
				all = append(all, a)
			}
		}
		info("NixOS: adding %s to /etc/nixos/abox-agents.nix and rebuilding", strings.Join(agents, ", "))
		if err := e.nixosAgentsFile(name, all); err != nil {
			return err
		}
		if err := e.nixosRebuild(name, update); err != nil {
			return err
		}
		update = false // binaries come from nixpkgs; install.sh only does per-user config there
	}
	for _, a := range agents {
		suffix := ""
		if update {
			suffix = " (update)"
		}
		info("installing agent '%s' in %s%s", a, name, suffix)
		script := "/tmp/abox-install-" + a + ".sh"
		if err := pushFile(name, script, e.Agents()[a].Install, "0755"); err != nil {
			return err
		}
		flag := ""
		if update {
			flag = "1"
		}
		if err := e.asUser(name, "cd && ABOX_AGENT_UPDATE="+flag+" bash "+script); err != nil {
			return fmt.Errorf("installing agent '%s' failed. Blocked downloads show in `abox net log`", a)
		}
		if err := e.listAdd(name, "user.abox.agents", a); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- secrets

// SecretSet exports KEY (value read from a host file) in the guest's login
// shells via ~/.abox-env (mode 600). The value never goes into Incus config.
func (e *Env) SecretSet(name, key, file string) error {
	if !envKeyRe.MatchString(key) {
		return fmt.Errorf("invalid variable name '%s'", key)
	}
	data, err := os.ReadFile(expandHome(file))
	if err != nil {
		return fmt.Errorf("cannot read %s: %v", file, err)
	}
	value := strings.TrimRight(strings.ReplaceAll(string(data), "\r", ""), "\n")
	line := "export " + key + "=" + shq(value) + "\n"
	script := `umask 077; f=$HOME/.abox-env; touch $f; { grep -v '^export ` + key + `=' $f || true; cat; } > $f.new && mv $f.new $f`
	if err := runStdin(strings.NewReader(line), "incus", "exec", "-T", name, "--env", "PATH="+guestPath, "--",
		"su", "-l", e.Cfg.GuestUser, "-c", script); err != nil {
		return fmt.Errorf("setting secret %s failed", key)
	}
	info("secret %s set in %s", key, name)
	return e.listAdd(name, "user.abox.secrets", key)
}

func (e *Env) SecretRm(name, key string) error {
	if !envKeyRe.MatchString(key) {
		return fmt.Errorf("invalid variable name '%s'", key)
	}
	script := `f=$HOME/.abox-env; [ -f $f ] || exit 0; umask 077; grep -v '^export ` + key + `=' $f > $f.new || true; mv $f.new $f`
	if err := runStdin(nil, "incus", "exec", "-T", name, "--env", "PATH="+guestPath, "--",
		"su", "-l", e.Cfg.GuestUser, "-c", script); err != nil {
		return fmt.Errorf("removing secret %s failed", key)
	}
	info("secret %s removed from %s", key, name)
	return e.listDel(name, "user.abox.secrets", key)
}

// ---------------------------------------------------------------- egress

func (e *Env) sandboxIP(name string) (string, error) {
	insts, err := e.Instances()
	if err != nil {
		return "", err
	}
	if ip := insts[name].IP; ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("%s has no address on %s (running?)", name, e.Cfg.Bridge)
}

func (e *Env) NetOpen(name string, minutes int) error {
	ip, err := e.sandboxIP(name)
	if err != nil {
		return err
	}
	if minutes <= 0 {
		minutes = e.Cfg.NetOpenMinutes
	}
	return runIO("sudo", aboxNet, "open", ip, strconv.Itoa(minutes))
}

func (e *Env) NetClose(name string) error {
	ip, err := e.sandboxIP(name)
	if err != nil {
		return err
	}
	return runIO("sudo", aboxNet, "close", ip)
}

const proxyLog = "/var/log/abox-proxy/access.log"

type denial struct {
	Who, Host string
	Count     int
}

// parseDenials summarizes squid access-log lines with DENIED results.
func parseDenials(lines []string, names map[string]string) []denial {
	counts := map[[2]string]int{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 7 || !strings.Contains(f[3], "DENIED") {
			continue
		}
		who := f[2]
		if n, ok := names[who]; ok {
			who = n
		}
		host := f[6]
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		if i := strings.IndexAny(host, "/:"); i >= 0 {
			host = host[:i]
		}
		counts[[2]string{who, host}]++
	}
	out := make([]denial, 0, len(counts))
	for k, n := range counts {
		out = append(out, denial{k[0], k[1], n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Who+out[i].Host < out[j].Who+out[j].Host
	})
	return out
}

// NetLog prints the most frequently blocked destinations of the last n log lines.
func (e *Env) NetLog(n int) error {
	f, err := os.Open(proxyLog)
	if err != nil {
		return fmt.Errorf("cannot read %s: %v", proxyLog, err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	names := map[string]string{}
	if insts, err := e.Instances(); err == nil {
		for _, i := range insts {
			if i.IP != "" {
				names[i.IP] = i.Name
			}
		}
	}
	d := parseDenials(lines, names)
	if len(d) == 0 {
		fmt.Println("no blocked requests in the last", n, "log lines")
	}
	for i, x := range d {
		if i == 50 {
			break
		}
		fmt.Printf("%7d %s %s\n", x.Count, x.Who, x.Host)
	}
	return nil
}

// ---------------------------------------------------------------- misc

var errNotRoot = errors.New("this command must run as root: sudo abox host ...")

func lookupSudoUID() (int, error) {
	if s := os.Getenv("SUDO_UID"); s != "" {
		return strconv.Atoi(s)
	}
	return 0, errors.New("no SUDO_UID")
}
