package main

// Base image: a fully provisioned Ubuntu sandbox (packages, Docker, Node, the
// agents) published as a local Incus image. New sandboxes are copy-on-write
// clones of it, so creating one takes seconds instead of minutes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const imageStaleAfter = 14 * 24 * time.Hour

func imageAlias(typ string) string { return "abox-base-" + typ }

// imageAgents: agents preinstalled in the base image.
func (e *Env) imageAgents() []string {
	if len(e.Cfg.ImageAgents) > 0 {
		return e.Cfg.ImageAgents
	}
	return sortedKeys(e.Agents())
}

// imageAddons: add-ons whose binaries are preinstalled in the base image.
func (e *Env) imageAddons() []string {
	switch {
	case len(e.Cfg.ImageAddons) == 1 && e.Cfg.ImageAddons[0] == "none":
		return nil
	case len(e.Cfg.ImageAddons) > 0:
		return e.Cfg.ImageAddons
	}
	return sortedKeys(e.Cfg.Addons())
}

// imageHash fingerprints everything the image is built from; a different hash
// means the image is outdated and gets rebuilt.
func (e *Env) imageHash(typ string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\n%s\n%s\n%d\n", typ, e.Cfg.UbuntuImage, e.UID)
	h.Write([]byte(e.Cfg.render(asset("profiles/abox-base.yaml"), e.UID)))
	for _, a := range e.imageAgents() {
		fmt.Fprintf(h, "\nagent %s\n%s", a, e.Agents()[a].Install)
	}
	for _, a := range e.imageAddons() {
		fmt.Fprintf(h, "\naddon %s\n%s", a, e.Cfg.Addons()[a].Install)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

type BaseImage struct {
	Exists      bool
	Fingerprint string
	Hash        string
	Agents      []string
	Created     time.Time
	Size        int64
}

func (e *Env) baseImage(typ string) BaseImage {
	out, _, err := capture("incus", "image", "list", imageAlias(typ), "-f", "json")
	if err != nil {
		return BaseImage{}
	}
	var imgs []struct {
		Fingerprint string            `json:"fingerprint"`
		Properties  map[string]string `json:"properties"`
		CreatedAt   time.Time         `json:"created_at"`
		Size        int64             `json:"size"`
		Aliases     []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if json.Unmarshal([]byte(out), &imgs) != nil {
		return BaseImage{}
	}
	for _, im := range imgs {
		for _, al := range im.Aliases {
			if al.Name == imageAlias(typ) {
				return BaseImage{Exists: true, Fingerprint: im.Fingerprint, Hash: im.Properties["abox.hash"],
					Agents: splitList(im.Properties["abox.agents"]), Created: im.CreatedAt, Size: im.Size}
			}
		}
	}
	return BaseImage{}
}

// imageLock serializes image builds (two `abox add` at once would build twice).
func imageLock(typ string) (func(), error) {
	dir := filepath.Join(os.TempDir(), "abox-"+fmt.Sprint(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "image-"+typ+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		info("another abox is building the base image; waiting for it ...")
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// ensureBaseImage returns the image alias to clone from, building or rebuilding
// it when missing or outdated.
func (e *Env) ensureBaseImage(typ string) (string, []string, error) {
	img := e.baseImage(typ)
	if img.Exists && img.Hash == e.imageHash(typ) {
		if age := time.Since(img.Created); age > imageStaleAfter {
			info("base image is %d days old; `abox image build` refreshes packages and agents", int(age.Hours()/24))
		}
		return imageAlias(typ), img.Agents, nil
	}
	unlock, err := imageLock(typ)
	if err != nil {
		return "", nil, err
	}
	defer unlock()
	if img = e.baseImage(typ); img.Exists && img.Hash == e.imageHash(typ) { // built while we waited
		return imageAlias(typ), img.Agents, nil
	}
	if img.Exists {
		info("base image is outdated (abox or its settings changed); rebuilding it")
	} else {
		info("building the abox base image for %ss (one-time, a few minutes; later sandboxes start in seconds)", typ)
	}
	if err := e.BuildImage(typ); err != nil {
		return "", nil, err
	}
	img = e.baseImage(typ)
	return imageAlias(typ), img.Agents, nil
}

// BuildImage provisions a temporary sandbox (no project directory, no secrets,
// no logins), scrubs per-instance state and publishes it as the base image.
func (e *Env) BuildImage(typ string) error {
	if err := e.InitProfiles(); err != nil {
		return err
	}
	name := fmt.Sprintf("abox-imgbuild-%s-%04d", typ, rand.Intn(10000))
	args := []string{"init", e.Cfg.UbuntuImage, name, "-p", "abox-base", "-p", "abox-" + typ,
		"-c", "user.abox.image=ubuntu"} // not user.abox=true: build instances stay out of `abox ls`
	if typ == "vm" {
		args = append(args, "--vm")
	}
	start := time.Now()
	if err := runIO("incus", args...); err != nil {
		return err
	}
	defer func() { _, _, _ = capture("incus", "delete", "--force", name) }()
	if err := runIO("incus", "start", name); err != nil {
		return err
	}
	if err := e.WaitReady(name); err != nil {
		return err
	}
	info("provisioning base image (packages, Docker) ...")
	_, _, _ = capture("incus", "exec", name, "--", "cloud-init", "status", "--wait")
	if _, _, err := capture("incus", "exec", name, "--", "test", "-f", "/var/lib/abox/provisioned"); err != nil {
		_ = runStdin(nil, "incus", "exec", name, "--", "tail", "-n", "30", "/var/log/cloud-init-output.log")
		return fmt.Errorf("base image provisioning failed; blocked downloads show in `abox net log`")
	}
	agents := e.imageAgents()
	for _, a := range agents {
		if err := e.AgentInstall(name, []string{a}, false); err != nil {
			return fmt.Errorf("base image: %v", err)
		}
	}
	addons := e.imageAddons()
	if err := e.Cfg.CheckAddons(addons); err != nil {
		return fmt.Errorf("ABOX_IMAGE_ADDONS: %v", err)
	}
	for _, ad := range addons {
		info("preinstalling add-on '%s' binaries (enabled per project with `abox addon add %s`)", ad, ad)
		script := "/tmp/abox-addon-" + ad + ".sh"
		if err := pushFile(name, script, e.Cfg.Addons()[ad].Install, "0755"); err != nil {
			return err
		}
		if err := e.asUser(name, "cd && ABOX_ADDON_PHASE=binaries bash "+script); err != nil {
			return fmt.Errorf("base image: preinstalling add-on '%s' failed (blocked downloads show in `abox net log`)", ad)
		}
	}
	info("cleaning up and publishing the base image ...")
	// Per-instance state must not be shared by all clones: machine-id, cloud-init
	// state (so clones run it once with the minimal abox-image config, which
	// also regenerates SSH host keys), caches, logs, shell history.
	scrub := `set -e
apt-get clean
rm -rf /tmp/* /var/tmp/* /root/.cache /root/.bash_history
U=$(getent passwd ` + e.Cfg.GuestUser + ` | cut -d: -f6)
rm -rf "$U/.npm/_cacache" "$U/.cache/node-gyp" "$U/.bash_history" "$U/.abox-env"
find /var/log -type f \( -name '*.gz' -o -name '*.[0-9]' \) -delete
find /var/log -type f -exec truncate -s 0 {} +
cloud-init clean --logs --machine-id 2>/dev/null || { cloud-init clean --logs; truncate -s 0 /etc/machine-id; }
rm -f /var/lib/dbus/machine-id
`
	if err := runStdin(strings.NewReader(scrub), "incus", "exec", name, "--", "sh"); err != nil {
		return fmt.Errorf("base image cleanup failed: %v", err)
	}
	if err := runIO("incus", "stop", name); err != nil {
		return err
	}
	props := []string{"abox.hash=" + e.imageHash(typ), "abox.agents=" + strings.Join(agents, ","),
		"abox.addons=" + strings.Join(addons, ","),
		"abox.type=" + typ, "description=abox base image (" + typ + ", agents: " + strings.Join(agents, ",") + ")"}
	pub := append([]string{"publish", name, "--alias", imageAlias(typ), "--reuse", "--compression", "zstd"}, props...)
	if err := runIO("incus", pub...); err != nil {
		return err
	}
	info("base image %s ready (%s)", imageAlias(typ), time.Since(start).Round(time.Second))
	return nil
}

func cmdImage(a *App) error {
	e, err := a.Env()
	if err != nil {
		return err
	}
	typ := "container"
	if a.args.Has("--vm") {
		typ = "vm"
	}
	switch a.args.Pos[0] {
	case "ls":
		for _, t := range []string{"container", "vm"} {
			img := e.baseImage(t)
			if !img.Exists {
				fmt.Printf("%-20s not built (built automatically when the first %s sandbox is created)\n", imageAlias(t), t)
				continue
			}
			state := "current"
			if img.Hash != e.imageHash(t) {
				state = "outdated (rebuilt on next create)"
			} else if time.Since(img.Created) > imageStaleAfter {
				state = "old (consider `abox image build`)"
			}
			fmt.Printf("%-20s %s, %d days old, %d MiB, agents: %s - %s\n", imageAlias(t), img.Created.Local().Format("2006-01-02 15:04"),
				int(time.Since(img.Created).Hours()/24), img.Size>>20, strings.Join(img.Agents, ","), state)
		}
		return nil
	case "build":
		unlock, err := imageLock(typ)
		if err != nil {
			return err
		}
		defer unlock()
		return e.BuildImage(typ)
	case "rm":
		if !e.baseImage(typ).Exists {
			return fmt.Errorf("%s does not exist", imageAlias(typ))
		}
		return runIO("incus", "image", "delete", imageAlias(typ))
	}
	return usageErr{fmt.Errorf("usage: abox image ls | build [--vm] | rm [--vm]")}
}
