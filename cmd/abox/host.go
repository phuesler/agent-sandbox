package main

// Host setup. Runs as root.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const zabblyFingerprint = "4EFC590696CB15B87C73A3AD82CC8797C838DCFD"

type hostFile struct {
	Path    string
	Content string
	Mode    os.FileMode
}

// hostFiles renders everything `abox host apply` installs (pure; tested).
// The 192.0.2.1 (TEST-NET) placeholder keeps squid's open_clients ACL non-empty.
func hostFiles(c *Config, sudoUser string) ([]hostFile, []string, error) {
	allow, agents, err := c.buildAllowlist()
	if err != nil {
		return nil, nil, err
	}
	r := func(p string) string { return c.render(asset(p), 0) }
	files := []hostFile{
		{"/etc/abox/squid.conf", r("egress/squid.conf.in"), 0o644},
		{"/etc/abox/egress.nft", r("egress/egress.nft.in"), 0o644},
		{"/etc/abox/allowlist.squid", allow, 0o644},
		{"/etc/abox/open-clients", "# managed by abox-net\n192.0.2.1\n", 0o644},
		{"/usr/local/sbin/abox-net", r("egress/abox-net.in"), 0o755},
		{"/usr/local/lib/abox/docker-user.sh", r("systemd/docker-user.sh.in"), 0o755},
		{"/etc/systemd/system/abox-egress.service", asset("systemd/abox-egress.service"), 0o644},
		{"/etc/systemd/system/abox-proxy.service", asset("systemd/abox-proxy.service"), 0o644},
		{"/etc/systemd/system/docker.service.d/abox-incus-forward.conf", asset("systemd/docker-abox-incus-forward.conf"), 0o644},
		{"/etc/logrotate.d/abox-proxy", asset("systemd/logrotate-abox-proxy"), 0o644},
	}
	if sudoUser != "" {
		files = append(files, hostFile{"/etc/sudoers.d/abox-net",
			sudoUser + " ALL=(root) NOPASSWD: " + aboxNet + "\n", 0o440})
	}
	return files, agents, nil
}

// writeHostFiles writes files below root ("/" for real). sudoers files are
// validated with visudo before they are installed.
func writeHostFiles(root string, files []hostFile) error {
	for _, f := range files {
		dst := filepath.Join(root, f.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		tmp := dst + ".abox-tmp"
		if err := os.WriteFile(tmp, []byte(f.Content), f.Mode); err != nil {
			return err
		}
		if err := os.Chmod(tmp, f.Mode); err != nil {
			return err
		}
		if strings.HasPrefix(f.Path, "/etc/sudoers.d/") && root == "/" {
			if out, err := exec.Command("visudo", "-cf", tmp).CombinedOutput(); err != nil {
				os.Remove(tmp)
				return fmt.Errorf("generated sudoers file invalid: %s", out)
			}
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
	}
	return nil
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errNotRoot
	}
	return nil
}

func sudoUser() string {
	if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
		return u
	}
	return ""
}

func runCmd(name string, args ...string) error { return runIO(name, args...) }

// hostApply (re)installs the egress layer: nftables firewall, squid allowlist
// proxy, abox-net helper + sudoers rule, systemd units. Idempotent; closes all
// temporarily opened sandboxes.
func hostApply(e *Env) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if _, err := exec.LookPath("squid"); err != nil {
		return errors.New("squid is not installed (run: sudo abox host install)")
	}
	if _, err := os.Stat(systemConfigPath()); errors.Is(err, os.ErrNotExist) {
		if err := writeHostFiles("/", []hostFile{{systemConfigPath(), asset("config.env"), 0o644}}); err != nil {
			return err
		}
		info("wrote default settings to %s", systemConfigPath())
	}
	files, agents, err := hostFiles(e.Cfg, sudoUser())
	if err != nil {
		return err
	}
	if err := os.MkdirAll("/var/log/abox-proxy", 0o755); err != nil {
		return err
	}
	if pu, err := user.Lookup("proxy"); err == nil {
		uid, _ := strconv.Atoi(pu.Uid)
		gid, _ := strconv.Atoi(pu.Gid)
		_ = os.Chown("/var/log/abox-proxy", uid, gid)
	}
	info("installing egress layer (config: %s)", e.Cfg.Source)
	if err := writeHostFiles("/", files); err != nil {
		return err
	}
	info("allowlist: base + agents: %s", strings.Join(agents, ", "))
	if u := sudoUser(); u != "" {
		info("sudoers: %s may run %s without password", u, aboxNet)
	}
	// clear leftover close-timers from earlier `net open`s
	if out, _, err := capture("systemctl", "list-units", "--plain", "--no-legend", "abox-net-close-*"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) > 0 {
				_, _, _ = capture("systemctl", "stop", f[0])
			}
		}
	}
	steps := [][]string{
		{"systemctl", "daemon-reload"},
		{"nft", "-c", "-f", "/etc/abox/egress.nft"},
		{"squid", "-k", "parse", "-f", "/etc/abox/squid.conf"},
		{"systemctl", "enable", "--quiet", "abox-egress.service", "abox-proxy.service"},
		{"systemctl", "restart", "abox-egress.service"},
		{"systemctl", "restart", "abox-proxy.service"},
	}
	for _, s := range steps {
		if out, err := exec.Command(s[0], s[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s failed: %v\n%s", strings.Join(s, " "), err, lastLines(string(out), 15))
		}
	}
	if out, _, _ := capture("systemctl", "is-active", "docker"); strings.TrimSpace(out) == "active" {
		_, _, _ = capture("/usr/local/lib/abox/docker-user.sh")
	}
	info("egress layer active: proxy %s, firewall table 'inet abox'", e.Cfg.ProxyURL())
	return nil
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

func osRelease(key string) string {
	data, _ := os.ReadFile("/etc/os-release")
	return parseEnvFile(string(data))[key]
}

// installZabbly adds the Zabbly Incus repository after verifying its key fingerprint.
func installZabbly() error {
	info("adding Zabbly Incus repository")
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Get("https://pkgs.zabbly.com/key.asc")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("downloading Zabbly key: HTTP %d", resp.StatusCode)
	}
	key, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "zabbly-*.asc")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.Write(key)
	tmp.Close()
	out, _, err := capture("gpg", "--show-keys", "--with-colons", tmp.Name())
	if err != nil {
		return fmt.Errorf("gpg --show-keys failed: %v", err)
	}
	fpr := ""
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Split(l, ":"); f[0] == "fpr" && len(f) > 9 {
			fpr = f[9]
			break
		}
	}
	if fpr != zabblyFingerprint {
		return fmt.Errorf("Zabbly key fingerprint mismatch: got %q", fpr)
	}
	arch, _, _ := capture("dpkg", "--print-architecture")
	sources := fmt.Sprintf("Enabled: yes\nTypes: deb\nURIs: https://pkgs.zabbly.com/incus/stable\nSuites: %s\n"+
		"Components: main\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/zabbly.asc\n",
		osRelease("VERSION_CODENAME"), strings.TrimSpace(arch))
	return writeHostFiles("/", []hostFile{
		{"/etc/apt/keyrings/zabbly.asc", string(key), 0o644},
		{"/etc/apt/sources.list.d/zabbly-incus-stable.sources", sources, 0o644},
	})
}

// hostInstall: one-time setup of Incus (+ storage pool and bridge), squid and
// the egress layer. Leaves the host's Docker alone except for two DOCKER-USER
// accept rules for the sandbox bridge.
func hostInstall(e *Env, distroIncus bool) error {
	if err := requireRoot(); err != nil {
		return err
	}
	u := sudoUser()
	if u == "" {
		return errors.New("run via sudo from your normal user account (sudo abox host install)")
	}
	os.Setenv("DEBIAN_FRONTEND", "noninteractive")
	c := e.Cfg
	if _, err := exec.LookPath("incus"); err != nil {
		if distroIncus {
			if err := runCmd("apt-get", "update"); err != nil {
				return err
			}
			if err := runCmd("apt-get", "install", "-y", "incus", "qemu-system-x86", "ovmf"); err != nil {
				return err
			}
		} else {
			if err := installZabbly(); err != nil {
				return err
			}
			if err := runCmd("apt-get", "update"); err != nil {
				return err
			}
			if err := runCmd("apt-get", "install", "-y", "incus"); err != nil {
				return err
			}
		}
	} else {
		out, _, _ := capture("incus", "--version")
		info("incus already installed: %s", strings.TrimSpace(out))
	}
	info("installing squid, nftables, btrfs-progs")
	if err := runCmd("apt-get", "install", "-y", "squid", "nftables", "btrfs-progs"); err != nil {
		return err
	}
	// abox runs its own squid instance (abox-proxy.service); the distro one would grab the port
	_, _, _ = capture("systemctl", "disable", "--now", "squid.service")
	_, _, _ = capture("systemctl", "mask", "squid.service")
	if _, _, err := capture("systemctl", "enable", "--now", "incus.socket", "incus.service"); err != nil {
		_, _, _ = capture("systemctl", "enable", "--now", "incus")
	}
	_, _, _ = capture("incus", "admin", "waitready", "--timeout=60")
	if err := runCmd("usermod", "-aG", "incus-admin", u); err != nil {
		return err
	}
	if _, _, err := capture("incus", "storage", "show", c.Pool); err != nil {
		info("storage pool '%s' (btrfs loop, %s)", c.Pool, c.PoolSize)
		if err := runCmd("incus", "storage", "create", c.Pool, "btrfs", "size="+c.PoolSize); err != nil {
			return err
		}
	}
	if _, _, err := capture("incus", "network", "show", c.Bridge); err != nil {
		info("network '%s' (%s/24, NAT, no IPv6)", c.Bridge, c.HostIP())
		if err := runCmd("incus", "network", "create", c.Bridge, "ipv4.address="+c.HostIP()+"/24", "ipv4.nat=true",
			"ipv4.dhcp=true", "ipv6.address=none", "dns.domain=abox"); err != nil {
			return err
		}
	}
	if err := hostApply(e); err != nil {
		return err
	}
	fmt.Printf(`
Done. Next steps (as %s):
  1. Log out/in (or run: newgrp incus-admin) so the incus-admin group applies.
  2. cd ~/code/some-project && abox     # register, create sandbox, run the default agent
     abox doctor                          # if anything looks off
`, u)
	return nil
}

func cmdHost(a *App) error {
	e, err := a.Env()
	if err != nil {
		return err
	}
	switch a.args.Pos[0] {
	case "install":
		if len(a.args.Pos) != 1 {
			return usageErr{fmt.Errorf("usage: sudo abox host install [--distro-incus]")}
		}
		return hostInstall(e, a.args.Has("--distro-incus"))
	case "apply":
		if len(a.args.Pos) != 1 || a.args.Has("--distro-incus") {
			return usageErr{fmt.Errorf("usage: sudo abox host apply")}
		}
		return hostApply(e)
	}
	return usageErr{fmt.Errorf("usage: sudo abox host install [--distro-incus] | sudo abox host apply")}
}
