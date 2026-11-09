// Package agentsandbox holds the files abox builds into its binary: agent
// plugins, Incus profiles, egress/systemd templates, the NixOS module and the
// default configuration. Edit them in the repo and rebuild (make install).
package agentsandbox

import "embed"

//go:embed config.env agents profiles egress systemd nixos
var Assets embed.FS
