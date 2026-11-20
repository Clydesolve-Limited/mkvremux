//go:build linux

// childproc_linux.go — production children die with makeMKV.
//
// Every child (ffprobe/ffmpeg) is spawned through exec.CommandContext and
// normally waited synchronously, but makeMKV has no signal handler: if the
// process is killed outright (kill -9, OOM killer, a terminal going away)
// the context cancellation never runs — a mid-flight child (the motivating
// case is a long batch re-encode burning GPU for an hour) would be
// orphaned and keep running. Pdeathsig makes the kernel SIGKILL the child
// the moment makeMKV's process dies, closing that window for kill -9-style
// orphaning of long batches. Because every production call site blocks in
// the parent's wait (Run/Output/CombinedOutput), the forking OS thread
// cannot be reaped while the child lives — the known Go caveat around
// Pdeathsig, acceptable here since the wait is synchronous.
//
// Linux-only field — the non-linux build gets the no-op stub
// (childproc_other.go); this stub only keeps foreign builds (and vet/lint
// runs over all GOOSes) working.
package main

import (
	"os/exec"
	"syscall"
)

// setChildPdeathsig arranges SIGKILL for cmd's child if this process exits
// first. Safe to call on every spawn: synchronous waiters keep the forking
// thread alive for the child's whole lifetime.
func setChildPdeathsig(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
