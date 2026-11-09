// abox - project-centric front end for sbx (coding-agent sandboxes on Incus).
//
// Run `abox` inside any project directory: it registers the project (asking
// once), creates/starts its sandbox and launches the project's coding agent
// (Claude Code, Codex, opencode, ... - see agents/) in the matching /work
// subdirectory. The registry lives on the host
// (~/.config/abox/projects.toml) so nothing inside a sandbox can change it.
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

var version = "dev"

// ---------------------------------------------------------------- output

var colorOK = isTTY(os.Stdout)

func color(code, s string) string {
	if !colorOK {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}
func bold(s string) string { return color("1", s) }
func info(format string, a ...any) {
	fmt.Fprintln(os.Stderr, color("1;34", "==> ")+fmt.Sprintf(format, a...))
}
func warn(format string, a ...any) {
	fmt.Fprintln(os.Stderr, color("1;33", "warn: ")+fmt.Sprintf(format, a...))
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ---------------------------------------------------------------- args

type Args struct {
	Cmd     string
	Project string
	Pos     []string
	Rest    []string // after a bare `--`
	HasRest bool
	Flags   map[string]string
}

var (
	flagAlias  = map[string]string{"-y": "--yes", "-p": "--project", "-h": "--help", "-v": "--version"}
	valueFlags = map[string]bool{"--project": true, "--name": true, "--image": true, "--cpus": true,
		"--memory": true, "--disk": true, "--agent": true}
	boolFlags = map[string]bool{"--yes": true, "--all": true, "--vm": true, "--container": true,
		"--no-create": true, "--keep-sandbox": true, "--help": true, "--version": true, "--distro-incus": true}
)

func (a *Args) Has(f string) bool   { _, ok := a.Flags[f]; return ok }
func (a *Args) Val(f string) string { return a.Flags[f] }
func isNumber(s string) bool        { _, err := strconv.Atoi(s); return err == nil }

// parseArgs accepts flags anywhere (before or after the command); everything
// after a bare `--` is passed through verbatim.
func parseArgs(argv []string) (*Args, error) {
	a := &Args{Flags: map[string]string{}}
	for i, x := range argv {
		if x == "--" {
			a.Rest, a.HasRest, argv = argv[i+1:], true, argv[:i]
			break
		}
	}
	for i := 0; i < len(argv); i++ {
		x := argv[i]
		if strings.HasPrefix(x, "-") && len(x) > 1 && !isNumber(x) {
			name, val, hasVal := strings.Cut(x, "=")
			if c, ok := flagAlias[name]; ok {
				name = c
			}
			switch {
			case valueFlags[name]:
				if !hasVal {
					if i+1 >= len(argv) {
						return nil, fmt.Errorf("%s needs a value", name)
					}
					i++
					val = argv[i]
				}
				a.Flags[name] = val
			case boolFlags[name]:
				if hasVal {
					return nil, fmt.Errorf("%s takes no value", name)
				}
				a.Flags[name] = ""
			default:
				return nil, fmt.Errorf("unknown option %s", x)
			}
			continue
		}
		if a.Cmd == "" {
			a.Cmd = x
		} else {
			a.Pos = append(a.Pos, x)
		}
	}
	a.Project = a.Flags["--project"]
	return a, nil
}

// ---------------------------------------------------------------- commands table

type command struct {
	run              func(*App) error
	usage, help      string
	flags            []string
	minPos           int
	maxPos           int
	rest, noRegistry bool
}

var commands map[string]*command
var commandOrder = []string{"run", "agents", "agent", "shell", "root", "exec", "add", "ls", "status", "up", "down",
	"snap", "restore", "rebuild", "rm", "set", "secret", "net", "import", "doctor", "edit", "host", "completion", "version"}

func init() {
	commands = map[string]*command{
		"run":        {run: cmdRun, usage: "run [agent] [-y] [-- agent args]", help: "run an agent (default command; default agent). Shortcut: abox <agent>", flags: []string{"--yes"}, maxPos: 1, rest: true},
		"agents":     {run: cmdAgents, usage: "agents", help: "list available agents (and which this project uses) + auth hints"},
		"agent":      {run: cmdAgent, usage: "agent add|rm|default|update <agent...>", help: "manage this project's agents", minPos: 1, maxPos: 99},
		"shell":      {run: cmdShell, usage: "shell", help: "login shell in the sandbox (same subdir)"},
		"root":       {run: cmdShell, usage: "root", help: "root shell"},
		"exec":       {run: cmdExec, usage: "exec -- <cmd...>", help: "run a command in the sandbox", rest: true},
		"add":        {run: cmdAdd, usage: "add [path] [--name N] [--agent a,b] [--vm|--container] [--image ubuntu|nixos] [--cpus N] [--memory 8GiB] [--disk 60GiB] [--no-create]", help: "register a project (and create its sandbox)", flags: []string{"--name", "--agent", "--vm", "--container", "--image", "--cpus", "--memory", "--disk", "--no-create"}, maxPos: 1},
		"ls":         {run: cmdLs, usage: "ls", help: "list projects: state, IP, egress, snapshots"},
		"status":     {run: cmdStatus, usage: "status", help: "details for one project"},
		"up":         {run: cmdUp, usage: "up [--all]", help: "create/start sandbox", flags: []string{"--all"}},
		"down":       {run: cmdDown, usage: "down [--all]", help: "stop sandbox", flags: []string{"--all"}},
		"snap":       {run: cmdSnap, usage: "snap [label]", help: "snapshot the sandbox", maxPos: 1},
		"restore":    {run: cmdRestore, usage: "restore [label] [-y]", help: "restore snapshot (default: clean)", flags: []string{"--yes"}, maxPos: 1},
		"rebuild":    {run: cmdRebuild, usage: "rebuild [-y]", help: "delete + recreate sandbox from settings", flags: []string{"--yes"}},
		"rm":         {run: cmdRm, usage: "rm [-y] [--keep-sandbox]", help: "delete sandbox and unregister project", flags: []string{"--yes", "--keep-sandbox"}},
		"set":        {run: cmdSet, usage: "set key=value...", help: "change settings (cpus/memory live; type/image/disk need rebuild)", minPos: 1, maxPos: 99},
		"secret":     {run: cmdSecret, usage: "secret set KEY FILE | rm KEY | ls | sync", help: "env vars (API keys) for the sandbox; values stay in host files", minPos: 1, maxPos: 3},
		"net":        {run: cmdNet, usage: "net open [minutes] | close | log [lines]", help: "egress control", minPos: 1, maxPos: 2},
		"import":     {run: cmdImport, usage: "import", help: "register sandboxes created outside abox"},
		"doctor":     {run: cmdDoctor, usage: "doctor", help: "check host setup and registry"},
		"edit":       {run: cmdEdit, usage: "edit", help: "open the registry in $EDITOR"},
		"completion": {run: cmdCompletion, usage: "completion", help: "print bash completion script", noRegistry: true},
		"version":    {run: cmdVersion, usage: "version", help: "print version", noRegistry: true},
		"host":       {run: cmdHost, usage: "host install [--distro-incus] | host apply   (run with sudo)", help: "set up this machine (Incus, egress proxy/firewall); needs sudo", flags: []string{"--distro-incus"}, minPos: 1, maxPos: 1, noRegistry: true},
		"_names":     {run: cmdNames},
		"_agents":    {run: cmdAgentNames},
	}
}

func usage() string {
	var b strings.Builder
	b.WriteString(`abox - coding-agent sandboxes per project (Claude Code, Codex, opencode, ...)

usage: abox [-p project] [command | agent] [args]
       -p/--project: project name or path (default: current directory)
       no command = run the project's default agent; 'abox <agent>' = run that agent

commands:
`)
	for _, n := range commandOrder {
		fmt.Fprintf(&b, "  %-11s %s\n", n, commands[n].help)
	}
	b.WriteString(`
examples:
  abox                         # in a project dir: register (once), create/start sandbox, run default agent
  abox codex                   # run another agent (offers to enable it for this project)
  abox claude -- --resume      # pass arguments to the agent
  abox agents                  # available agents, which this project uses, how to log in
  abox agent default opencode  # change the project's default agent
  abox secret set OPENAI_API_KEY ~/.secrets/openai
  abox ls                      # all projects, state, egress, snapshots
  abox shell                   # shell in the sandbox (same subdir as here)
  abox exec -- make test       # one-off command
  abox net open 20             # unrestricted internet for this project's sandbox for 20 min
  abox snap before-upgrade && abox restore before-upgrade
  abox set memory=16GiB cpus=8 # live; type/image/disk need ` + "`abox rebuild`" + `
  abox -p other-project shell  # any command for another project (name or path)
`)
	return b.String()
}

func run(argv []string) error {
	args, err := parseArgs(argv)
	if err != nil {
		return usageErr{err}
	}
	if args.Has("--version") {
		return cmdVersion(nil)
	}
	if args.Cmd == "" {
		if args.Has("--help") {
			fmt.Print(usage())
			return nil
		}
		args.Cmd = "run"
	}
	if args.Cmd == "help" {
		fmt.Print(usage())
		return nil
	}
	c, ok := commands[args.Cmd]
	if !ok {
		// `abox <agent> ...` is shorthand for `abox run <agent> ...`
		if e, err := loadEnv(); err == nil {
			if _, isAgent := e.Agents()[args.Cmd]; isAgent {
				args.Pos = append([]string{args.Cmd}, args.Pos...)
				args.Cmd, c = "run", commands["run"]
				ok = true
			}
		}
	}
	if !ok {
		return usageErr{fmt.Errorf("unknown command or agent '%s'", args.Cmd)}
	}
	if args.Has("--help") {
		fmt.Printf("usage: abox [-p project] %s\n  %s\n", c.usage, c.help)
		return nil
	}
	allowed := map[string]bool{"--project": true}
	for _, f := range c.flags {
		allowed[f] = true
	}
	for f := range args.Flags {
		if !allowed[f] {
			return usageErr{fmt.Errorf("%s is not valid for '%s' (usage: abox %s)", f, args.Cmd, c.usage)}
		}
	}
	if len(args.Pos) < c.minPos || len(args.Pos) > c.maxPos {
		return usageErr{fmt.Errorf("usage: abox %s", c.usage)}
	}
	if args.HasRest && !c.rest {
		return usageErr{fmt.Errorf("`--` arguments are only valid for run/<agent> and exec")}
	}
	if c.noRegistry {
		return c.run(&App{reg: &Registry{Projects: map[string]*Project{}}, args: args})
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	return c.run(&App{reg: reg, args: args})
}

type usageErr struct{ error }

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, color("1;31", "abox: ")+err.Error())
	var ue usageErr
	if errors.As(err, &ue) {
		fmt.Fprintln(os.Stderr, "(abox help for usage)")
		os.Exit(2)
	}
	var ex exitCode
	if errors.As(err, &ex) {
		os.Exit(int(ex))
	}
	os.Exit(1)
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }
