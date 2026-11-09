package main

import (
	"os"
	"path/filepath"
	"reflect"
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
	s, err := parseSettings([]string{"cpus=8", "memory=", "type=vm"})
	if err != nil || len(s) != 3 {
		t.Fatalf("%v %v", s, err)
	}
	for _, bad := range [][]string{{"cpus=8", "type=banana"}, {"cpus=0"}, {"cpus=x"}, {"path=/x"}, {"nokey"}, {"image=arch"}} {
		if _, err := parseSettings(bad); err == nil {
			t.Errorf("parseSettings(%v) accepted", bad)
		}
	}
	pr := &Project{CPUs: 4, Memory: "8GiB"}
	for _, x := range s {
		applySetting(pr, x)
	}
	if pr.CPUs != 8 || pr.Memory != "" || pr.Type != "vm" {
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
	js := `[{"name":"a","status":"Running","type":"container","config":{"user.sbx.path":"/p","user.sbx.image":"ubuntu"},
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

func TestFindRepoViaEnv(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "sbx"), nil, 0o755)
	os.WriteFile(filepath.Join(d, "config.env"), []byte("SBX_NET_PREFIX=10.99.1 # comment\n"), 0o644)
	t.Setenv("ABOX_REPO", d)
	e, err := loadEnv()
	if err != nil || e.Repo != d || e.NetPrefix != "10.99.1." {
		t.Fatalf("%+v %v", e, err)
	}
}

func TestAgentsDiscovery(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "sbx"), nil, 0o755)
	os.WriteFile(filepath.Join(d, "config.env"), []byte("SBX_DEFAULT_AGENTS=\"codex claude\"\n"), 0o644)
	mk := func(name, conf string, install bool) {
		os.MkdirAll(filepath.Join(d, "agents", name), 0o755)
		os.WriteFile(filepath.Join(d, "agents", name, "agent.conf"), []byte(conf), 0o644)
		if install {
			os.WriteFile(filepath.Join(d, "agents", name, "install.sh"), nil, 0o755)
		}
	}
	mk("codex", "# c\nAGENT_DESC=\"Codex CLI (OpenAI)\"\nAGENT_CMD='codex --yolo'\nAGENT_AUTH=\"use # sign\"\n", true)
	mk("claude", "AGENT_DESC=\"Claude\"\nAGENT_CMD=\"claude --dangerously-skip-permissions\"\n", true)
	mk("broken", "AGENT_DESC=\"no installer\"\n", false)
	mk("Bad_Name", "AGENT_DESC=x\n", true)
	t.Setenv("ABOX_REPO", d)
	e, err := loadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.DefaultAgents, []string{"codex", "claude"}) {
		t.Errorf("default agents %v", e.DefaultAgents)
	}
	ag := e.Agents()
	if len(ag) != 2 || ag["codex"].Cmd != "codex --yolo" || ag["codex"].Auth != "use # sign" || ag["claude"].Desc != "Claude" {
		t.Errorf("agents %+v", ag)
	}
	if e.CheckAgents([]string{"claude", "codex"}) != nil || e.CheckAgents([]string{"broken"}) == nil {
		t.Error("CheckAgents")
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
	m, _ := parseInstances(`[{"name":"a","status":"Running","type":"container","config":{"user.sbx.agents":"claude,codex","user.sbx.secrets":""}}]`, "10.88.0.")
	if !reflect.DeepEqual(m["a"].Agents, []string{"claude", "codex"}) || len(m["a"].Secrets) != 0 {
		t.Errorf("%+v", m["a"])
	}
}
