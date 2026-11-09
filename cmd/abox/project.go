package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	nameRe      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	agentNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	envKeyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	slugBad     = regexp.MustCompile(`[^a-z0-9-]+`)
	slugDash    = regexp.MustCompile(`-{2,}`)
	stdin       = bufio.NewReader(os.Stdin)
)

func slug(s string) string {
	s = slugBad.ReplaceAllString(strings.ToLower(s), "-")
	s = slugDash.ReplaceAllString(strings.Trim(s, "-"), "-")
	if s != "" && (s[0] < 'a' || s[0] > 'z') {
		s = "p-" + s
	}
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.TrimRight(s, "-")
	if len(s) < 2 {
		return "project"
	}
	return s
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// canon makes a path absolute with symlinks resolved (falls back to Abs if it doesn't exist).
func canon(p string) string {
	abs, err := filepath.Abs(expandHome(p))
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}

func within(p, root string) (string, bool) {
	if p == root {
		return ".", true
	}
	if strings.HasPrefix(p, strings.TrimRight(root, "/")+"/") {
		rel, _ := filepath.Rel(root, p)
		return rel, true
	}
	return "", false
}

// findByPath: registered project containing p (longest root wins) and p relative to it.
func findByPath(reg *Registry, p string) (string, string, bool) {
	best, bestRel := "", ""
	for _, name := range reg.Names() {
		root := reg.Projects[name].Path
		if rel, ok := within(p, root); ok && (best == "" || len(root) > len(reg.Projects[best].Path)) {
			best, bestRel = name, rel
		}
	}
	return best, bestRel, best != ""
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var forbidden = func() map[string]bool {
	home, _ := os.UserHomeDir()
	m := map[string]bool{}
	for _, p := range []string{"/", home, "/home", "/etc", "/usr", "/var", "/root", "/tmp"} {
		m[p] = true
	}
	return m
}()

func validateRoot(p string) error {
	if forbidden[p] {
		return fmt.Errorf("refusing to use %s as a project (pick the project directory itself)", p)
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", p)
	}
	return nil
}

func ask(prompt, def string, choices ...string) (string, error) {
	for {
		fmt.Printf("  %s [%s]: ", prompt, def)
		line, err := stdin.ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("aborted")
		}
		ans := strings.TrimSpace(line)
		if ans == "" {
			ans = def
		}
		if len(choices) == 0 {
			return ans, nil
		}
		for _, c := range choices {
			if ans == c {
				return ans, nil
			}
		}
		fmt.Printf("    choose one of: %s\n", strings.Join(choices, ", "))
	}
}

func confirm(msg string, yes bool) error {
	if yes {
		return nil
	}
	if !isTTY(os.Stdin) {
		return fmt.Errorf("%s - pass -y to confirm non-interactively", msg)
	}
	fmt.Printf("%s [y/N] ", msg)
	line, _ := stdin.ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return nil
	}
	return errors.New("aborted")
}

// ---------------------------------------------------------------- app

type App struct {
	reg  *Registry
	env  *Env
	args *Args
}

func (a *App) Env() (*Env, error) {
	if a.env == nil {
		e, err := loadEnv()
		if err != nil {
			return nil, err
		}
		a.env = e
	}
	return a.env, nil
}

func (a *App) instances() (map[string]Instance, error) {
	e, err := a.Env()
	if err != nil {
		return nil, err
	}
	return e.Instances()
}

func (a *App) uniqueName(base string) (string, error) {
	insts, err := a.instances()
	if err != nil {
		return "", err
	}
	name := base
	for n := 2; ; n++ {
		_, reg := a.reg.Projects[name]
		_, inst := insts[name]
		if !reg && !inst {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
}

// adopt registers an existing sbx sandbox (created without abox) whose shared dir contains p.
func (a *App) adopt(p string) (bool, error) {
	insts, err := a.instances()
	if err != nil {
		return false, err
	}
	for _, name := range sortedKeys(insts) {
		inst := insts[name]
		if _, known := a.reg.Projects[name]; known || inst.Path == "" {
			continue
		}
		if _, ok := within(p, inst.Path); ok {
			a.reg.Projects[name] = &Project{Path: inst.Path, Type: inst.Type, Image: inst.Image}
			if err := a.reg.Save(); err != nil {
				return false, err
			}
			info("adopted existing sandbox '%s' for %s", name, inst.Path)
			return true, nil
		}
	}
	return false, nil
}

// resolve: target is a project name, a path, or "" (cwd). Returns name and subdir.
func (a *App) resolve(target string, registerIfMissing bool) (string, string, error) {
	if target != "" {
		if _, ok := a.reg.Projects[target]; ok {
			return target, ".", nil
		}
	}
	var p string
	if target != "" {
		p = canon(target)
		if _, err := os.Stat(p); err != nil {
			return "", "", fmt.Errorf("'%s' is neither a registered project nor an existing path", target)
		}
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return "", "", err
		}
		p = canon(wd)
	}
	if name, rel, ok := findByPath(a.reg, p); ok {
		return name, rel, nil
	}
	if ok, err := a.adopt(p); err != nil {
		return "", "", err
	} else if ok {
		name, rel, _ := findByPath(a.reg, p)
		return name, rel, nil
	}
	if registerIfMissing {
		name, err := a.registerInteractive(p)
		return name, ".", err
	}
	return "", "", fmt.Errorf("%s is not inside a registered project (run `abox add` there, or see `abox ls`)", p)
}

type ProjectOpts struct {
	Name, Type, Image, Memory, Disk string
	CPUs                            int
	Agents                          []string
}

func (a *App) defaultAgents() []string {
	if len(a.reg.Defaults.Agents) > 0 {
		return a.reg.Defaults.Agents
	}
	if e, err := a.Env(); err == nil {
		return e.DefaultAgents
	}
	return []string{"claude"}
}

// agentsOf returns the project's agents (default first).
func (a *App) agentsOf(name string) []string {
	if pr := a.reg.Projects[name]; pr != nil && len(pr.Agents) > 0 {
		return pr.Agents
	}
	return a.defaultAgents()
}

func contains(list []string, x string) bool {
	for _, y := range list {
		if y == x {
			return true
		}
	}
	return false
}

func splitAgents(s string) []string {
	var out []string
	for _, x := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func (a *App) register(p string, o ProjectOpts) (string, error) {
	if err := validateRoot(p); err != nil {
		return "", err
	}
	if name, rel, ok := findByPath(a.reg, p); ok && rel == "." {
		return "", fmt.Errorf("%s is already registered as '%s'", p, name)
	}
	e, err := a.Env()
	if err != nil {
		return "", err
	}
	if len(o.Agents) == 0 {
		o.Agents = a.defaultAgents()
	}
	if err := e.CheckAgents(o.Agents); err != nil {
		return "", err
	}
	name := o.Name
	if name == "" {
		if name, err = a.uniqueName(slug(filepath.Base(p))); err != nil {
			return "", err
		}
	}
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("invalid name '%s' (lowercase letters, digits, dashes; 2-63 chars)", name)
	}
	if _, ok := a.reg.Projects[name]; ok {
		return "", fmt.Errorf("project '%s' already exists", name)
	}
	pr := &Project{Path: p, Type: o.Type, Image: o.Image, Agents: o.Agents, CPUs: o.CPUs, Memory: o.Memory, Disk: o.Disk}
	if pr.Type == "" {
		pr.Type = a.reg.Defaults.Type
	}
	if pr.Image == "" {
		pr.Image = a.reg.Defaults.Image
	}
	a.reg.Projects[name] = pr
	if err := a.reg.Save(); err != nil {
		return "", err
	}
	info("registered '%s' -> %s (agents: %s)", name, p, strings.Join(pr.Agents, ", "))
	return name, nil
}

func (a *App) registerInteractive(p string) (string, error) {
	if err := validateRoot(p); err != nil {
		return "", err
	}
	if !isTTY(os.Stdin) {
		return "", fmt.Errorf("%s is not a registered project (run `abox add %s`)", p, p)
	}
	e, err := a.Env()
	if err != nil {
		return "", err
	}
	fmt.Printf("%s is not a abox project yet.\n", bold(p))
	def, err := a.uniqueName(slug(filepath.Base(p)))
	if err != nil {
		return "", err
	}
	name, err := ask("name", def)
	if err != nil {
		return "", err
	}
	typ, err := ask("type (container|vm)", a.reg.Defaults.Type, "container", "vm")
	if err != nil {
		return "", err
	}
	img, err := ask("image (ubuntu|nixos)", a.reg.Defaults.Image, "ubuntu", "nixos")
	if err != nil {
		return "", err
	}
	var agents []string
	for {
		ans, err := ask(fmt.Sprintf("agents, first = default (%s)", strings.Join(sortedKeys(e.Agents()), ",")),
			strings.Join(a.defaultAgents(), ","))
		if err != nil {
			return "", err
		}
		agents = splitAgents(ans)
		if err := e.CheckAgents(agents); err != nil || len(agents) == 0 {
			fmt.Printf("    %v\n", err)
			continue
		}
		break
	}
	return a.register(p, ProjectOpts{Name: name, Type: typ, Image: img, Agents: agents})
}

// Agent reconciliation policy for ensure().
type agentPolicy int

const (
	agentsSkip    agentPolicy = iota // don't install missing agents (shell/exec/net/...)
	agentsLenient                    // install missing agents, failures are warnings
	agentsStrict                     // install missing agents, failures are errors
)

// ensureUp: create/start the sandbox and install everything the registry asks for.
func (a *App) ensureUp(name string) error { return a.ensure(name, agentsStrict) }

// ensure creates the sandbox if absent, starts it if stopped, pushes secrets it
// is missing and - depending on policy - installs missing agents (e.g. after a
// restore to an older snapshot or after `abox agent add`). Agents listed in
// required must install successfully regardless of policy.
func (a *App) ensure(name string, policy agentPolicy, required ...string) error {
	e, err := a.Env()
	if err != nil {
		return err
	}
	pr := a.reg.Projects[name]
	agents := a.agentsOf(name)
	if err := e.CheckAgents(agents); err != nil {
		return err
	}
	insts, err := e.Instances()
	if err != nil {
		return err
	}
	inst, ok := insts[name]
	if !ok {
		if fi, err := os.Stat(pr.Path); err != nil || !fi.IsDir() {
			return fmt.Errorf("project dir %s is missing", pr.Path)
		}
		info("creating sandbox '%s' (%s, %s, agents: %s) - first time takes a few minutes",
			name, pr.Type, pr.Image, strings.Join(agents, ", "))
		args := []string{"new", name, "--path", pr.Path, "--" + pr.Type, "--image", pr.Image,
			"--agent", strings.Join(agents, ",")}
		if pr.CPUs > 0 {
			args = append(args, "--cpus", strconv.Itoa(pr.CPUs))
		}
		if pr.Memory != "" {
			args = append(args, "--memory", pr.Memory)
		}
		if pr.Disk != "" {
			args = append(args, "--disk", pr.Disk)
		}
		for _, k := range sortedKeys(pr.Secrets) {
			args = append(args, "--secret", k+"="+expandHome(pr.Secrets[k]))
		}
		if err := e.sbx(nil, args...); err != nil {
			if _, exists := mustInstances(e)[name]; !exists {
				return err
			}
			// sandbox exists but some agent failed: continue with reconciliation below
			warn("sandbox created, but not everything installed; retrying missing parts")
			insts, _ = e.Instances()
			inst = insts[name]
		} else {
			return nil
		}
	}
	if inst.Status != "running" {
		info("starting '%s'", name)
		if err := e.sbx(nil, "start", name); err != nil { // waits for network
			return err
		}
	}
	for _, k := range sortedKeys(pr.Secrets) {
		if !contains(inst.Secrets, k) {
			if err := e.sbx(nil, "secret", "set", name, k, expandHome(pr.Secrets[k])); err != nil {
				return err
			}
		}
	}
	for _, ag := range agents {
		if contains(inst.Agents, ag) || (policy == agentsSkip && !contains(required, ag)) {
			continue
		}
		if err := e.sbx(nil, "agent-install", name, ag); err != nil {
			hint := fmt.Sprintf("agent '%s' could not be installed. Blocked downloads show in `abox net log`; "+
				"after adding agents to the host allowlist run `sudo %s/egress/apply.sh`", ag, e.Repo)
			if policy == agentsStrict || contains(required, ag) {
				return fmt.Errorf("%s", hint)
			}
			warn("%s", hint)
		}
	}
	return nil
}

func mustInstances(e *Env) map[string]Instance {
	m, _ := e.Instances()
	return m
}

func workdirEnv(rel string) map[string]string {
	if rel == "." || rel == "" {
		return nil
	}
	return map[string]string{"SBX_CWD": filepath.Join("/work", rel)}
}
