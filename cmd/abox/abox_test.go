package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"My Demo_App": "my-demo-app", "123abc": "p-123abc", "--x--": "project",
		"ÄÖÜ": "project", "a": "project", "foo.bar": "foo-bar",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
		if got := slug(in); !nameRe.MatchString(got) {
			t.Errorf("slug(%q) = %q is not a valid name", in, got)
		}
	}
}

func TestParseArgs(t *testing.T) {
	a, err := parseArgs([]string{"net", "open", "5", "-p", "foo"})
	if err != nil || a.Cmd != "net" || a.Project != "foo" || !reflect.DeepEqual(a.Pos, []string{"open", "5"}) {
		t.Fatalf("got %+v, %v", a, err)
	}
	a, _ = parseArgs([]string{"-p=bar", "exec", "--", "ls", "-p", "--all"})
	if a.Project != "bar" || a.Cmd != "exec" || !reflect.DeepEqual(a.Rest, []string{"ls", "-p", "--all"}) || a.Has("--all") {
		t.Fatalf("got %+v", a)
	}
	a, _ = parseArgs([]string{"--", "--resume"})
	if a.Cmd != "" || !a.HasRest || a.Rest[0] != "--resume" {
		t.Fatalf("got %+v", a)
	}
	if _, err := parseArgs([]string{"ls", "--bogus"}); err == nil {
		t.Fatal("expected unknown option error")
	}
	if _, err := parseArgs([]string{"add", "--name"}); err == nil {
		t.Fatal("expected missing value error")
	}
}

func TestRunRejects(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, argv := range [][]string{
		{"shell", "--", "x"}, // -- only for claude/exec
		{"ls", "--all"},      // flag not valid for ls
		{"snap", "a", "b"},   // too many positionals
		{"net"},              // missing action
		{"frobnicate"},       // unknown command
	} {
		if err := run(argv); err == nil {
			t.Errorf("run(%v) succeeded, want error", argv)
		}
	}
}

func TestFindByPath(t *testing.T) {
	reg := &Registry{Projects: map[string]*Project{
		"outer": {Path: "/src/outer"}, "inner": {Path: "/src/outer/inner"}, "other": {Path: "/src/outer2"},
	}}
	cases := []struct{ p, name, rel string }{
		{"/src/outer", "outer", "."},
		{"/src/outer/a/b", "outer", "a/b"},
		{"/src/outer/inner/x", "inner", "x"},
		{"/src/outer2", "other", "."},
	}
	for _, c := range cases {
		name, rel, ok := findByPath(reg, c.p)
		if !ok || name != c.name || rel != c.rel {
			t.Errorf("findByPath(%s) = %s,%s,%v want %s,%s", c.p, name, rel, ok, c.name, c.rel)
		}
	}
	if _, _, ok := findByPath(reg, "/src"); ok {
		t.Error("/src should not match")
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	r, err := loadRegistry()
	if err != nil || r.Defaults.Type != "container" || r.Defaults.Image != "ubuntu" {
		t.Fatalf("defaults: %+v %v", r, err)
	}
	r.Projects["a"] = &Project{Path: "/x/a b", Type: "vm", Image: "ubuntu", CPUs: 8, Memory: "16GiB"}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	r2, err := loadRegistry()
	if err != nil || !reflect.DeepEqual(r2.Projects["a"], r.Projects["a"]) {
		t.Fatalf("roundtrip: %+v %v", r2.Projects["a"], err)
	}
	// unknown keys are rejected (typo protection)
	os.WriteFile(registryPath(), []byte("[projects.a]\npath = \"/x\"\nmemroy = \"1GiB\"\n"), 0o644)
	if _, err := loadRegistry(); err == nil {
		t.Fatal("expected unknown key error")
	}
}

func TestParseSettings(t *testing.T) {
	s, err := parseSettings([]string{"cpus=8", "memory=", "type=vm", "mode=bypass"})
	if err != nil || len(s) != 4 {
		t.Fatalf("%v %v", s, err)
	}
	for _, bad := range [][]string{{"mode=yolo"}, {"cpus=8", "type=banana"}, {"cpus=0"}, {"cpus=x"}, {"path=/x"}, {"nokey"}, {"image=arch"}} {
		if _, err := parseSettings(bad); err == nil {
			t.Errorf("parseSettings(%v) accepted", bad)
		}
	}
	pr := &Project{CPUs: 4, Memory: "8GiB"}
	for _, x := range s {
		applySetting(pr, x)
	}
	if pr.CPUs != 8 || pr.Memory != "" || pr.Type != "vm" || pr.Mode != "bypass" {
		t.Fatalf("apply: %+v", pr)
	}
}

func TestParseEgress(t *testing.T) {
	out := "# proxy open clients\n10.88.0.219\n# firewall open set\n\t\telements = { 10.88.0.219 timeout 2m expires 1m59s932ms,\n\t\t\t     10.88.0.7 timeout 30m expires 12m }\n"
	got := parseEgress(out)
	want := map[string]string{"10.88.0.219": "open 1m59s", "10.88.0.7": "open 12m"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestParseInstances(t *testing.T) {
	js := `[{"name":"a","status":"Running","type":"container","config":{"user.abox.path":"/p","user.abox.image":"ubuntu"},
	  "state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"10.88.0.5"}]},
	                      "docker0":{"addresses":[{"family":"inet","address":"172.17.0.1"}]}}},
	  "snapshots":[{},{}]},
	 {"name":"b","status":"Stopped","type":"virtual-machine","config":{},"state":null,"snapshots":null}]`
	m, err := parseInstances(js, "10.88.0.")
	if err != nil {
		t.Fatal(err)
	}
	if a := m["a"]; a.IP != "10.88.0.5" || a.Status != "running" || a.Snaps != 2 || a.Path != "/p" {
		t.Errorf("a = %+v", a)
	}
	if b := m["b"]; b.Type != "vm" || b.Status != "stopped" || b.Image != "?" || b.IP != "" {
		t.Errorf("b = %+v", b)
	}
}

func TestConfigOverride(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "config.env")
	t.Setenv("ABOX_CONFIG", p)
	c, err := loadConfig()
	if err != nil || c.Source != "built-in defaults" || c.NetPrefix != "10.88.0" || c.GuestUser != "agent" ||
		!reflect.DeepEqual(c.DefaultAgents, []string{"claude"}) || c.NetOpenMinutes != 30 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	os.WriteFile(p, []byte("ABOX_NET_PREFIX=10.99.1 # comment\nABOX_DEFAULT_AGENTS=\"codex claude\"\n"), 0o644)
	c, err = loadConfig()
	if err != nil || c.Source != p || c.ProxyURL() != "http://10.99.1.1:3128" ||
		!reflect.DeepEqual(c.DefaultAgents, []string{"codex", "claude"}) || c.Bridge != "aboxbr0" {
		t.Fatalf("override: %+v %v", c, err)
	}
	os.WriteFile(p, []byte("SBX_NET_PREFIX=10.1.2\n"), 0o644)
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "unknown setting SBX_NET_PREFIX") {
		t.Fatalf("expected unknown-key error, got %v", err)
	}
	os.WriteFile(p, []byte("ABOX_NET_PREFIX=not-an-ip\n"), 0o644)
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid prefix error")
	}
}

func TestEmbeddedAgents(t *testing.T) {
	t.Setenv("ABOX_CONFIG", filepath.Join(t.TempDir(), "none"))
	c, _ := loadConfig()
	ag := c.Agents()
	for _, n := range []string{"claude", "codex", "opencode", "gemini"} {
		a, ok := ag[n]
		if !ok || a.Desc == "" || a.Install == "" || a.Allowlist == "" || a.NixPkg == "" {
			t.Errorf("agent %s incomplete: %+v", n, a)
		}
		for _, m := range permModes {
			if a.Cmds[m] == "" {
				t.Errorf("agent %s has no %s mode", n, m)
			}
		}
		if strings.Contains(a.Cmds["auto"], "dangerously") || strings.Contains(a.Cmds["auto"], "yolo") {
			t.Errorf("agent %s: auto mode must not bypass permissions: %s", n, a.Cmds["auto"])
		}
	}
	if c.CheckAgents([]string{"claude", "nope"}) == nil {
		t.Error("CheckAgents accepted unknown agent")
	}
	if got := parseAgentConf("x", "# c\nAGENT_DESC=\"A # b\"\nAGENT_CMD_AUTO='x --y'\n"); got.Desc != "A # b" || got.Cmds["auto"] != "x --y" || got.Cmds["ask"] != "" {
		t.Errorf("parseAgentConf: %+v", got)
	}
}

func TestPruneDomains(t *testing.T) {
	got := pruneDomains("quay.io\n.quay.io\nFoo.Example.com # c\n.example.com\n", "a.b.c.org\n.c.org\n.b.c.org\nexact.net\n")
	want := []string{".c.org", ".example.com", ".quay.io", "exact.net"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestShq(t *testing.T) {
	cases := map[string]string{"abc": "abc", "a b": "'a b'", "it's": `'it'\''s'`, "": "''", "$HOME": "'$HOME'", "--x=1": "--x=1"}
	for in, want := range cases {
		if got := shq(in); got != want {
			t.Errorf("shq(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestUserArgsCwd(t *testing.T) {
	e := &Env{Cfg: &Config{GuestUser: "agent"}}
	for cwd, want := range map[string]string{"/work/a b": "cd '/work/a b'", "/etc": "cd /work ", "/work/../etc": "cd /work ", "": "cd /work "} {
		args := e.userArgs("n", cwd, "true", false)
		if script := args[len(args)-1]; !strings.Contains(script, want) {
			t.Errorf("cwd %q: script %q lacks %q", cwd, script, want)
		}
	}
}

func TestParseDenials(t *testing.T) {
	lines := []string{
		"1.0 5 10.88.0.5 TCP_DENIED/403 3900 CONNECT example.com:443 - HIER_NONE/- text/html",
		"1.0 5 10.88.0.5 TCP_DENIED/403 3900 CONNECT example.com:443 - HIER_NONE/- text/html",
		"1.0 5 10.88.0.9 TCP_DENIED/403 3900 GET http://evil.test/x - HIER_NONE/- text/html",
		"1.0 5 10.88.0.5 TCP_TUNNEL/200 3900 CONNECT api.anthropic.com:443 - HIER_DIRECT/1.2.3.4 -",
		"garbage",
	}
	got := parseDenials(lines, map[string]string{"10.88.0.5": "proj"})
	want := []denial{{"proj", "example.com", 2}, {"10.88.0.9", "evil.test", 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

// TestHostFilesParity compares the rendered host files with a directory of
// previously installed copies. Run with ABOX_PARITY_DIR=<dir with copies>.
func TestHostFilesParity(t *testing.T) {
	dir := os.Getenv("ABOX_PARITY_DIR")
	if dir == "" {
		t.Skip("ABOX_PARITY_DIR not set")
	}
	t.Setenv("ABOX_CONFIG", filepath.Join(t.TempDir(), "none"))
	c, _ := loadConfig()
	files, _, err := hostFiles(c, "someuser")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Path, "/etc/sudoers.d/") { // baseline not readable; same basename as the helper
			if f.Content != "someuser ALL=(root) NOPASSWD: /usr/local/sbin/abox-net\n" || f.Mode != 0o440 {
				t.Errorf("sudoers rule: %q %o", f.Content, f.Mode)
			}
			continue
		}
		want, err := os.ReadFile(filepath.Join(dir, filepath.Base(f.Path)))
		if err != nil {
			t.Logf("no baseline for %s", f.Path)
			continue
		}
		got, wantS := f.Content, string(want)
		if filepath.Base(f.Path) == "allowlist.squid" { // order/header differ (locale sort): compare as sets
			got, wantS = strings.Join(pruneDomains(got), "\n"), strings.Join(pruneDomains(wantS), "\n")
		}
		if got != wantS {
			t.Errorf("%s differs from baseline:\n--- got\n%s\n--- want\n%s", f.Path, got, wantS)
		}
	}
	root := t.TempDir()
	if err := writeHostFiles(root, files); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "usr/local/sbin/abox-net")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("abox-net mode: %v %v", fi, err)
	}
}

func TestSplitAgents(t *testing.T) {
	if got := splitAgents("codex, claude,,codex opencode"); !reflect.DeepEqual(got, []string{"codex", "claude", "opencode"}) {
		t.Errorf("got %v", got)
	}
}

func TestRegistryAgentsSecrets(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	r, _ := loadRegistry()
	r.Projects["p"] = &Project{Path: "/p", Type: "container", Image: "ubuntu", Agents: []string{"codex", "claude"},
		Secrets: map[string]string{"OPENAI_API_KEY": "/k/openai"}}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	r2, err := loadRegistry()
	if err != nil || !reflect.DeepEqual(r2.Projects["p"], r.Projects["p"]) {
		t.Fatalf("%+v %v", r2.Projects["p"], err)
	}
}

func TestParseInstancesAgents(t *testing.T) {
	m, _ := parseInstances(`[{"name":"a","status":"Running","type":"container","config":{"user.abox.agents":"claude,codex","user.abox.secrets":""}}]`, "10.88.0.")
	if !reflect.DeepEqual(m["a"].Agents, []string{"claude", "codex"}) || len(m["a"].Secrets) != 0 {
		t.Errorf("%+v", m["a"])
	}
}

func TestModes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := &App{reg: &Registry{Projects: map[string]*Project{"p": {Path: "/p"}, "q": {Path: "/q", Mode: "ask"}}}}
	if a.modeOf("p") != "auto" || a.modeOf("q") != "ask" {
		t.Errorf("default modes: %s %s", a.modeOf("p"), a.modeOf("q"))
	}
	a.reg.Defaults.Mode = "bypass"
	if a.modeOf("p") != "bypass" || a.modeOf("q") != "ask" {
		t.Errorf("defaults.mode not applied")
	}
	os.MkdirAll(filepath.Dir(registryPath()), 0o755)
	os.WriteFile(registryPath(), []byte("[projects.p]\npath = \"/p\"\nmode = \"yolo\"\n"), 0o644)
	if _, err := loadRegistry(); err == nil {
		t.Error("invalid mode accepted")
	}
	args, err := parseArgs([]string{"codex", "--bypass", "--", "--help"})
	if err != nil || !args.Has("--bypass") || args.Rest[0] != "--help" {
		t.Errorf("flags: %+v %v", args, err)
	}
	if err := run([]string{"run", "--ask", "--bypass"}); err == nil {
		t.Error("conflicting mode flags accepted")
	}
}

func TestPorts(t *testing.T) {
	for in, want := range map[string]Port{"3000": {3000, 3000}, "8080:3000": {8080, 3000}, " 80:8000 ": {80, 8000}} {
		if got, err := parsePort(in); err != nil || got != want {
			t.Errorf("parsePort(%q) = %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x", "0", "70000", "1:2:3", "80:"} {
		if _, err := parsePort(bad); err == nil {
			t.Errorf("parsePort(%q) accepted", bad)
		}
	}
	if (Port{3000, 3000}).String() != "3000" || (Port{8080, 3000}).String() != "8080:3000" {
		t.Error("Port.String")
	}
	p, ok := portFromDevice("abox-port-8080", map[string]string{"type": "proxy", "listen": "tcp:127.0.0.1:8080", "connect": "tcp:10.88.0.5:3000"})
	if !ok || p != (Port{8080, 3000}) {
		t.Errorf("portFromDevice: %v %v", p, ok)
	}
	if _, ok := portFromDevice("myproxy", map[string]string{"type": "proxy", "listen": "tcp:0.0.0.0:1", "connect": "tcp:127.0.0.1:1"}); ok {
		t.Error("foreign proxy device treated as abox port")
	}
	m, _ := parseInstances(`[{"name":"a","status":"Running","type":"container","config":{},"devices":{"abox-port-3000":{"type":"proxy","listen":"tcp:127.0.0.1:3000","connect":"tcp:127.0.0.1:3000"},"work":{"type":"disk"}}}]`, "10.88.0.")
	if len(m["a"].Ports) != 1 || m["a"].Ports[0] != (Port{3000, 3000}) {
		t.Errorf("instance ports: %+v", m["a"].Ports)
	}
}

func TestBrowserAddon(t *testing.T) {
	t.Setenv("ABOX_CONFIG", filepath.Join(t.TempDir(), "none"))
	c, _ := loadConfig()
	b, ok := c.Addons()["browser"]
	if !ok || b.Desc == "" || !strings.Contains(b.Install, "playwright") {
		t.Fatalf("browser add-on: %+v", b)
	}
	allow, _, err := c.buildAllowlist()
	if err != nil || !strings.Contains(allow, "dl.google.com") || strings.Contains(allow, "storage.googleapis.com") {
		t.Errorf("allowlist must contain dl.google.com and not storage.googleapis.com: %v", err)
	}
	if c.CheckAddons([]string{"browser"}) != nil || c.CheckAddons([]string{"nope"}) == nil {
		t.Error("CheckAddons")
	}
}

func TestUserArgsPty(t *testing.T) {
	e := &Env{Cfg: &Config{GuestUser: "agent"}}
	tty := strings.Join(e.userArgs("n", "/work", "exec bash -l", true), " ")
	if !strings.Contains(tty, "su --pty -l") || !strings.Contains(tty, "exec -t") {
		t.Errorf("interactive session must use su --pty: %s", tty)
	}
	plain := strings.Join(e.userArgs("n", "/work", "true", false), " ")
	if strings.Contains(plain, "--pty") || !strings.Contains(plain, "exec -T") {
		t.Errorf("non-interactive session must not use a pty: %s", plain)
	}
}

func TestImageHash(t *testing.T) {
	t.Setenv("ABOX_CONFIG", filepath.Join(t.TempDir(), "none"))
	c, _ := loadConfig()
	e := &Env{Cfg: c, UID: 1000}
	h := e.imageHash("container")
	if h != e.imageHash("container") || len(h) != 16 {
		t.Fatalf("hash not stable: %s", h)
	}
	if h == e.imageHash("vm") {
		t.Error("container and vm images must differ")
	}
	e2 := &Env{Cfg: c, UID: 1001}
	if h == e2.imageHash("container") {
		t.Error("uid must be part of the hash (the agent user is created with it)")
	}
	c.agents["claude"] = AgentInfo{Name: "claude", Install: "changed", Cmds: c.agents["claude"].Cmds}
	if h == e.imageHash("container") {
		t.Error("changed agent installer must change the hash")
	}
	h2 := e.imageHash("container")
	c.addons["browser"] = AddonInfo{Name: "browser", Install: "changed"}
	if h2 == e.imageHash("container") {
		t.Error("changed add-on installer must change the hash")
	}
	if got := e.imageAddons(); len(got) != 1 || got[0] != "browser" {
		t.Errorf("default image add-ons: %v", got)
	}
	c.ImageAddons = []string{"none"}
	if len(e.imageAddons()) != 0 {
		t.Error(`ABOX_IMAGE_ADDONS="none" must disable add-ons`)
	}
	c.ImageAgents = []string{"claude"}
	if got := e.imageAgents(); len(got) != 1 || got[0] != "claude" {
		t.Errorf("ABOX_IMAGE_AGENTS ignored: %v", got)
	}
}
