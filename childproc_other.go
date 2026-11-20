//go:build !linux

// childproc_other.go — no-op off Linux: syscall.SysProcAttr has no
// Pdeathsig field there. makeMKV targets Linux; this stub only keeps the
// package portable so foreign builds (and vet/lint runs over all GOOSes)
// do not break.
package main

import "os/exec"

// setChildPdeathsig is a no-op on non-Linux platforms (see
// childproc_linux.go for the semantics on the supported platform).
func setChildPdeathsig(cmd *exec.Cmd) {
}
