//go:build windows

package cliagent

import "os/exec"

// setupProcGroup: Windows has no process groups across arbitrary binaries;
// exec.CommandContext's default Process.Kill is the best available.
func setupProcGroup(*exec.Cmd) {}
