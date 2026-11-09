package main

// Port forwarding from the host into a sandbox, Docker-style:
//
//   [ADDRESS|INTERFACE:][HOSTPORT:]SANDBOXPORT
//
//   3000                 local only (containers: 127.0.0.1, VMs: the bridge address)
//   8080:3000            local only, host port 8080
//   tailscale0:3000      on the current IPv4 address of an interface (VPNs: tailscale0, wt0, zt...)
//   100.64.1.2:8080:3000 on a specific address
//   0.0.0.0:3000         all interfaces including the LAN (warned)
//
// Implemented as Incus proxy devices named abox-port-*. Containers use a
// userspace proxy (the app may listen on localhost); VMs only support NAT
// proxies, which need a static VM address (pinned to the current lease), the
// app listening on 0.0.0.0, and cannot serve 127.0.0.1 on the host.

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

type Port struct {
	Bind        string // "" = local only; otherwise an IP address or an interface name
	Host, Guest int
}

func (p Port) String() string {
	b := ""
	if p.Bind != "" {
		b = p.Bind + ":"
		if strings.Contains(p.Bind, ":") { // IPv6
			b = "[" + p.Bind + "]:"
		}
	}
	if p.Host == p.Guest {
		return b + strconv.Itoa(p.Host)
	}
	return fmt.Sprintf("%s%d:%d", b, p.Host, p.Guest)
}

var (
	ifaceRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,14}$`)
	devNameRe = regexp.MustCompile(`[^A-Za-z0-9-]+`)
)

func validPort(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 65535
}

// parsePort parses [ADDRESS|INTERFACE:][HOSTPORT:]SANDBOXPORT ("[v6addr]" for IPv6).
func parsePort(spec string) (Port, error) {
	s := strings.TrimSpace(spec)
	bad := fmt.Errorf("invalid port '%s' (use [ADDRESS|INTERFACE:][HOSTPORT:]SANDBOXPORT, ports 1-65535)", spec)
	var p Port
	if strings.HasPrefix(s, "[") { // [IPv6]:...
		end := strings.Index(s, "]:")
		if end < 0 {
			return Port{}, bad
		}
		p.Bind, s = s[1:end], s[end+2:]
		if ip := net.ParseIP(p.Bind); ip == nil || ip.To4() != nil {
			return Port{}, bad
		}
	}
	parts := strings.Split(s, ":")
	switch {
	case len(parts) == 1:
		g, ok := validPort(parts[0])
		if !ok {
			return Port{}, bad
		}
		p.Host, p.Guest = g, g
	case len(parts) == 2 && p.Bind == "":
		if h, ok := validPort(parts[0]); ok { // HOSTPORT:SANDBOXPORT
			g, ok := validPort(parts[1])
			if !ok {
				return Port{}, bad
			}
			p.Host, p.Guest = h, g
		} else { // BIND:PORT
			g, ok := validPort(parts[1])
			if !ok {
				return Port{}, bad
			}
			p.Bind, p.Host, p.Guest = parts[0], g, g
		}
	case len(parts) == 2: // [v6]:HOST:GUEST handled above as Bind + "H:G"
		h, ok1 := validPort(parts[0])
		g, ok2 := validPort(parts[1])
		if !ok1 || !ok2 {
			return Port{}, bad
		}
		p.Host, p.Guest = h, g
	case len(parts) == 3 && p.Bind == "":
		h, ok1 := validPort(parts[1])
		g, ok2 := validPort(parts[2])
		if !ok1 || !ok2 {
			return Port{}, bad
		}
		p.Bind, p.Host, p.Guest = parts[0], h, g
	default:
		return Port{}, bad
	}
	if p.Bind == "localhost" {
		p.Bind = "127.0.0.1"
	}
	if p.Bind != "" && net.ParseIP(p.Bind) == nil && !ifaceRe.MatchString(p.Bind) {
		return Port{}, fmt.Errorf("invalid address or interface '%s' in '%s'", p.Bind, spec)
	}
	return p, nil
}

// device is the Incus device name; the plain local form keeps the historic name.
func (p Port) device() string {
	if p.Bind == "" {
		return "abox-port-" + strconv.Itoa(p.Host)
	}
	return "abox-port-" + strings.Trim(devNameRe.ReplaceAllString(p.Bind, "-"), "-") + "-" + strconv.Itoa(p.Host)
}

func wildcard(ip string) bool { return ip == "0.0.0.0" || ip == "::" }

// interfaceIPv4 returns the first IPv4 address of a network interface.
func interfaceIPv4(name string) (string, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("interface %s not found (VPN down?)", name)
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String(), nil
		}
	}
	return "", fmt.Errorf("interface %s has no IPv4 address", name)
}

// bindIP resolves where on the host a forward listens.
func (e *Env) bindIP(p Port, typ string) (string, error) {
	switch {
	case p.Bind == "" && typ == "vm":
		return e.Cfg.HostIP(), nil // NAT proxies cannot serve 127.0.0.1
	case p.Bind == "":
		return "127.0.0.1", nil
	case net.ParseIP(p.Bind) != nil:
		if typ == "vm" && net.ParseIP(p.Bind).IsLoopback() {
			return "", fmt.Errorf("VM sandboxes cannot forward on %s (Incus only supports NAT for VMs); "+
				"use the default (bridge address %s) or an interface", p.Bind, e.Cfg.HostIP())
		}
		if typ == "vm" && net.ParseIP(p.Bind).To4() == nil {
			return "", fmt.Errorf("VM sandboxes have no IPv6 on the bridge; use an IPv4 address or interface")
		}
		return p.Bind, nil
	default:
		return interfaceIPv4(p.Bind)
	}
}

func hostport(ip string, port int) string { return net.JoinHostPort(ip, strconv.Itoa(port)) }

// portDev is an abox-port-* proxy device as Incus reports it.
type portDev struct{ Listen, Connect string }

// desiredDevice computes the proxy device for p.
func (e *Env) desiredDevice(p Port, inst Instance) (portDev, error) {
	ip, err := e.bindIP(p, inst.Type)
	if err != nil {
		return portDev{}, err
	}
	d := portDev{Listen: "tcp:" + hostport(ip, p.Host)}
	if inst.Type == "vm" {
		if inst.IP == "" {
			return portDev{}, fmt.Errorf("VM has no address yet")
		}
		d.Connect = "tcp:" + hostport(inst.IP, p.Guest)
	} else {
		d.Connect = "tcp:" + hostport("127.0.0.1", p.Guest)
	}
	return d, nil
}

// SyncPorts makes the sandbox's abox-port-* devices match want: stale or
// changed ones (e.g. a VPN address changed) are replaced, missing ones added.
// Problems with single forwards are warnings, so they never block the sandbox.
func (e *Env) SyncPorts(name string, want []Port) error {
	insts, err := e.Instances()
	if err != nil {
		return err
	}
	inst, ok := insts[name]
	if !ok {
		return fmt.Errorf("no sandbox %s", name)
	}
	desired := map[string]portDev{}
	byDev := map[string]Port{}
	for _, p := range want {
		d, err := e.desiredDevice(p, inst)
		if err != nil {
			warn("port %s not forwarded: %v", p, err)
			continue
		}
		desired[p.device()], byDev[p.device()] = d, p
	}
	for dn, have := range inst.PortDevs {
		if want, ok := desired[dn]; !ok || want != have {
			if err := incusQuiet("config", "device", "remove", name, dn); err != nil {
				return err
			}
			if !ok {
				info("stopped forwarding %s", strings.TrimPrefix(have.Listen, "tcp:"))
			}
		}
	}
	for dn, d := range desired {
		if have, ok := inst.PortDevs[dn]; ok && have == d {
			continue
		}
		p := byDev[dn]
		args := []string{"config", "device", "add", name, dn, "proxy", "listen=" + d.Listen, "connect=" + d.Connect}
		if inst.Type == "vm" {
			if err := e.pinVMAddress(name, inst.IP); err != nil {
				return err
			}
			args = append(args, "nat=true")
		} else {
			args = append(args, "bind=host")
		}
		if err := incusQuiet(args...); err != nil {
			warn("forwarding %s: %v (is the host port already in use?)", p, err)
			continue
		}
		listen := strings.TrimPrefix(d.Listen, "tcp:")
		info("forwarding http://%s -> %s:%d", listen, name, p.Guest)
		if ip, _, _ := net.SplitHostPort(listen); wildcard(ip) {
			warn("port %d listens on ALL interfaces (LAN and every VPN); anyone who can reach this machine can connect", p.Host)
		}
	}
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

func (a *App) projectPorts(name string) []Port {
	var out []Port
	for _, ps := range a.reg.Projects[name].Ports {
		if p, err := parsePort(ps); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func cmdPort(a *App) error {
	name, _, err := a.resolve(a.args.Project, false)
	if err != nil {
		return err
	}
	pr := a.reg.Projects[name]
	pos := a.args.Pos
	if len(pos) == 0 || (pos[0] == "ls" && len(pos) == 1) {
		e, err := a.Env()
		if err != nil {
			return err
		}
		insts, err := e.Instances()
		if err != nil {
			return err
		}
		if len(pr.Ports) == 0 {
			fmt.Println("no forwarded ports - e.g. `abox port 3000`, `abox port tailscale0:3000`, `abox port 8080:3000`")
		}
		inst, hasInst := insts[name]
		for _, p := range a.projectPorts(name) {
			where, state := p.String(), "inactive (applied when the sandbox runs)"
			if hasInst {
				if d, err := e.desiredDevice(p, inst); err != nil {
					state = "not forwarded: " + err.Error()
				} else {
					where = "http://" + strings.TrimPrefix(d.Listen, "tcp:")
					if inst.PortDevs[p.device()] == d {
						state = "active"
					}
				}
			}
			fmt.Printf("%-32s -> sandbox :%-6d %-8s %s\n", where, p.Guest, "("+p.String()+")", state)
		}
		return nil
	}
	remove := pos[0] == "rm"
	if remove {
		pos = pos[1:]
	}
	if len(pos) != 1 {
		return usageErr{fmt.Errorf("usage: abox port [ls] | abox port [ADDRESS|INTERFACE:][HOSTPORT:]SANDBOXPORT | abox port rm <spec>")}
	}
	p, err := parsePort(pos[0])
	if err != nil {
		return usageErr{err}
	}
	var keep []string
	found := false
	for _, q := range a.projectPorts(name) {
		if q.device() == p.device() { // same bind address and host port
			found = true
			continue
		}
		keep = append(keep, q.String())
	}
	if remove {
		if !found {
			return fmt.Errorf("port %s is not forwarded for '%s' (see `abox port`)", p, name)
		}
		pr.Ports = keep
	} else {
		// reject what can never work for this sandbox type (e.g. 127.0.0.1 on a VM);
		// an interface that is merely down right now is fine (warned when applied)
		if e, err := a.Env(); err == nil && (p.Bind == "" || net.ParseIP(p.Bind) != nil) {
			if _, err := e.bindIP(p, pr.Type); err != nil {
				return err
			}
		}
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
	return a.ensure(name, agentsSkip) // starts the sandbox if needed and syncs the forwards
}
