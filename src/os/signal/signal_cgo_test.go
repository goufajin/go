// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (darwin || dragonfly || freebsd || (linux && !android) || netbsd || openbsd) && cgo

// Note that this test does not work on Solaris: issue #22849.
// Don't run the test on Android because at least some versions of the
// C library do not define the posix_openpt function.

package signal_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	ptypkg "os/signal/internal/pty"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
	"testing"
	"time"
)

const (
	ptyFD     = 3  // child end of pty.
	controlFD = 4  // child end of control pipe.
)

// TestTerminalSignal tests that read from a pseudo-terminal does not return an
// error if the process is SIGSTOP'd and put in the background during the read.
//
// This test simulates stopping a Go process running in a shell with ^Z and
// then resuming with `fg`.
//
// This is a regression test for https://go.dev/issue/22838. On Darwin, PTY
// reads return EINTR when this occurs, and Go should automatically retry.
func TestTerminalSignal(t *testing.T) {
	// This test simulates stopping a Go process running in a shell with ^Z
	// and then resuming with `fg`. This sounds simple, but is actually
	// quite complicated.
	//
	// In principle, what we are doing is:
	// 1. Creating a new PTY parent/child FD pair.
	// 2. Create a child that is in the foreground process group of the PTY, and read() from that process.
	// 3. Stop the child with ^Z.
	// 4. Take over as foreground process group of the PTY from the parent.
	// 5. Make the child foreground process group again.
	// 6. Continue the child.
	//
	// On Darwin, step 4 results in the read() returning EINTR once the
	// process continues. internal/poll should automatically retry the
	// read.
	//
	// These steps are complicated by the rules around foreground process
	// groups. A process group cannot be foreground if it is "orphaned",
	// unless it masks SIGTTOU.  i.e., to be foreground the process group
	// must have a parent process group in the same session or mask SIGTTOU
	// (which we do). An orphaned process group cannot receive
	// terminal-generated SIGTSTP at all.
	//
	// Achieving this requires three processes total:
	// - Top-level process: this is the main test process and creates the
	// pseudo-terminal.
	// - GO_TEST_TERMINAL_SIGNALS=1: This process creates a new process
	// group and session. The PTY is the controlling terminal for this
	// session. This process masks SIGTTOU, making it eligible to be a
	// foreground process group. This process will take over as foreground
	// from subprocess 2 (step 4 above).
	// - GO_TEST_TERMINAL_SIGNALS=2: This process create a child process
	// group of subprocess 1, and is the original foreground process group
	// for the PTY. This subprocess is the one that is SIGSTOP'd.

	if runtime.GOOS == "dragonfly" {
		t.Skip("skipping: wait hangs on dragonfly; see https://go.dev/issue/56132")
	}

	scale := 1
	if s := os.Getenv("GO_TEST_TIMEOUT_SCALE"); s != "" {
		if sc, err := strconv.Atoi(s); err == nil {
			scale = sc
		}
	}
	pause := time.Duration(scale) * 10 * time.Millisecond

	lvl := os.Getenv("GO_TEST_TERMINAL_SIGNALS")
	switch lvl {
	case "":
		// Main test process, run code below.
		break
	case "1":
		runSessionLeader(pause)
		panic("unreachable")
	case "2":
		runStoppingChild()
		panic("unreachable")
	default:
		fmt.Fprintf(os.Stderr, "unknown subprocess level %s\n", lvl)
		os.Exit(1)
	}

	t.Parallel()

	pty, procTTYName, err := ptypkg.Open()
	if err != nil {
		ptyErr := err.(*ptypkg.PtyError)
		if ptyErr.FuncName == "posix_openpt" && ptyErr.Errno == syscall.EACCES {
			t.Skip("posix_openpt failed with EACCES, assuming chroot and skipping")
		}
		t.Fatal(err)
	}
	defer pty.Close()
	procTTY, err := os.OpenFile(procTTYName, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer procTTY.Close()

	// Control pipe. GO_TEST_TERMINAL_SIGNALS=2 send the PID of
	// GO_TEST_TERMINAL_SIGNALS=3 here. After SIGSTOP, it also writes a
	// byte to indicate that the foreground cycling is complete.
	controlR, controlW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestTerminalSignal")
	cmd.Env = append(os.Environ(), "GO_TEST_TERMINAL_SIGNALS=1")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout // for logging
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{procTTY, controlW}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    ptyFD,
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	if err := procTTY.Close(); err != nil {
		t.Errorf("closing procTTY: %v", err)
	}

	if err := controlW.Close(); err != nil {
		t.Errorf("closing controlW: %v", err)
	}

	// Wait for first child to send the second child's PID.
	b := make([]byte, 8)
	n, err := controlR.Read(b)
	if err != nil {
		t.Fatalf("error reading child pid: %v\n", err)
	}
	if n != 8 {
		t.Fatalf("unexpected short read n = %d\n", n)
	}
	pid := binary.LittleEndian.Uint64(b[:])
	process, err := os.FindProcess(int(pid))
	if err != nil {
		t.Fatalf("unable to find child process: %v", err)
	}

	// Wait for the third child to write a byte indicating that it is
	// entering the read.
	b = make([]byte, 1)
	_, err = pty.Read(b)
	if err != nil {
		t.Fatalf("error reading from child: %v", err)
	}

	// Give the program time to enter the read call.
	// It doesn't matter much if we occasionally don't wait long enough;
	// we won't be testing what we want to test, but the overall test
	// will pass.
	time.Sleep(pause)

	t.Logf("Sending ^Z...")

	// Send a ^Z to stop the program.
	if _, err := pty.Write([]byte{26}); err != nil {
		t.Fatalf("writing ^Z to pty: %v", err)
	}

	// Wait for subprocess 1 to cycle the foreground process group.
	if _, err := controlR.Read(b); err != nil {
		t.Fatalf("error reading readiness: %v", err)
	}

	t.Logf("Sending SIGCONT...")

	// Restart the stopped program.
	if err := process.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("Signal(SIGCONT) got err %v want nil", err)
	}

	// Write some data for the program to read, which should cause it to
	// exit.
	if _, err := pty.Write([]byte{'\n'}); err != nil {
		t.Fatalf("writing %q to pty: %v", "\n", err)
	}

	t.Logf("Waiting for exit...")

	if err = cmd.Wait(); err != nil {
		t.Errorf("subprogram failed: %v", err)
	}
}

// GO_TEST_TERMINAL_SIGNALS=1 subprocess above.
func runSessionLeader(pause time.Duration) {
	// "Attempts to use tcsetpgrp() from a process which is a
	// member of a background process group on a fildes associated
	// with its controlling terminal shall cause the process group
	// to be sent a SIGTTOU signal. If the calling thread is
	// blocking SIGTTOU signals or the process is ignoring SIGTTOU
	// signals, the process shall be allowed to perform the
	// operation, and no signal is sent."
	//  -https://pubs.opengroup.org/onlinepubs/9699919799/functions/tcsetpgrp.html
	//
	// We are changing the terminal to put us in the foreground, so
	// we must ignore SIGTTOU. We are also an orphaned process
	// group (see above), so we must mask SIGTTOU to be eligible to
	// become foreground at all.
	signal.Ignore(syscall.SIGTTOU)

	pty := os.NewFile(ptyFD, "pty")
	controlW := os.NewFile(controlFD, "control-pipe")

	// Slightly shorter timeout than in the parent.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestTerminalSignal")
	cmd.Env = append(os.Environ(), "GO_TEST_TERMINAL_SIGNALS=2")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{pty}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Foreground: true,
		Ctty:       ptyFD,
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error starting second subprocess: %v\n", err)
		os.Exit(1)
	}

	fn := func() error {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(cmd.Process.Pid))
		_, err := controlW.Write(b[:])
		if err != nil {
			return fmt.Errorf("error writing child pid: %w", err)
		}

		// Wait for stop.
		var status syscall.WaitStatus
		var errno syscall.Errno
		for {
			_, _, errno = syscall.Syscall6(syscall.SYS_WAIT4, uintptr(cmd.Process.Pid), uintptr(unsafe.Pointer(&status)), syscall.WUNTRACED, 0, 0, 0)
			if errno != syscall.EINTR {
				break
			}
		}
		if errno != 0 {
			return fmt.Errorf("error waiting for stop: %w", errno)
		}

		if !status.Stopped() {
			return fmt.Errorf("unexpected wait status: %v", status)
		}

		// Take TTY.
		pgrp := syscall.Getpgrp()
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, ptyFD, syscall.TIOCSPGRP, uintptr(unsafe.Pointer(&pgrp)))
		if errno != 0 {
			return fmt.Errorf("error setting tty process group: %w", errno)
		}

		// Give the kernel time to potentially wake readers and have
		// them return EINTR (darwin does this).
		time.Sleep(pause)

		// Give TTY back.
		pid := uint64(cmd.Process.Pid)
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, ptyFD, syscall.TIOCSPGRP, uintptr(unsafe.Pointer(&pid)))
		if errno != 0 {
			return fmt.Errorf("error setting tty process group back: %w", errno)
		}

		// Report that we are done and SIGCONT can be sent. Note that
		// the actual byte we send doesn't matter.
		if _, err := controlW.Write(b[:1]); err != nil {
			return fmt.Errorf("error writing readiness: %w", err)
		}

		return nil
	}

	err := fn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "session leader error: %v\n", err)
		cmd.Process.Kill()
		// Wait for exit below.
	}

	werr := cmd.Wait()
	if werr != nil {
		fmt.Fprintf(os.Stderr, "error running second subprocess: %v\n", err)
	}

	if err != nil || werr != nil {
		os.Exit(1)
	}

	os.Exit(0)
}

// GO_TEST_TERMINAL_SIGNALS=2 subprocess above.
func runStoppingChild() {
	pty := os.NewFile(ptyFD, "pty")

	var b [1]byte
	if _, err := pty.Write(b[:]); err != nil {
		fmt.Fprintf(os.Stderr, "error writing byte to PTY: %v\n", err)
		os.Exit(1)
	}

	_, err := pty.Read(b[:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if b[0] == '\n' {
		// This is what we expect
		fmt.Println("read newline")
	} else {
		fmt.Fprintf(os.Stderr, "read 1 unexpected byte: %q\n", b)
		os.Exit(1)
	}
	os.Exit(0)
}
