// Copyright 2018 Mikhail Klementev. All rights reserved.
// Use of this source code is governed by a AGPLv3 license
// (or later) that can be found in the LICENSE file.

package qemukernel

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

func readUntilEOF(pipe io.ReadCloser, buf *[]byte) (err error) {
	bufSize := 1024
	for err != io.EOF {
		stdout := make([]byte, bufSize)
		var n int

		n, err = pipe.Read(stdout)
		if err != nil && err != io.EOF {
			return
		}

		*buf = append(*buf, stdout[:n]...)
	}

	if err == io.EOF {
		err = nil
	}
	return
}

type arch string

const (
	// X86_64 must be exactly same as in qemu-system-${HERE}
	X86_64 arch = "x86_64"
	I386        = "i386"
	// TODO add other

	unsupported = "unsupported" // for test purposes
)

// Kernel describe kernel parameters for qemu
type Kernel struct {
	Name       string
	KernelPath string
	InitrdPath string
}

// QemuSystem describe qemu parameters and runned process
type QemuSystem struct {
	arch      arch
	kernel    Kernel
	drivePath string

	Cpus   int
	Memory int

	debug bool
	gdb   string // tcp::1234

	// Timeout works after Start invocation
	Timeout         time.Duration
	KilledByTimeout bool

	KernelPanic bool

	Died        bool
	sshAddrPort string

	// accessible while qemu is runned
	cmd  *exec.Cmd
	pipe struct {
		stdin  io.WriteCloser
		stderr io.ReadCloser
		stdout io.ReadCloser
	}

	Stdout, Stderr []byte

	// accessible after qemu is closed
	exitErr error
}

// NewQemuSystem constructor
func NewQemuSystem(arch arch, kernel Kernel, drivePath string) (q *QemuSystem, err error) {
	if _, err = exec.LookPath("qemu-system-" + string(arch)); err != nil {
		return
	}
	q = &QemuSystem{}
	q.arch = arch

	if _, err = os.Stat(kernel.KernelPath); err != nil {
		return
	}
	q.kernel = kernel

	if _, err = os.Stat(drivePath); err != nil {
		return
	}
	q.drivePath = drivePath

	// Default values
	q.Cpus = 1
	q.Memory = 512 // megabytes

	return
}

func getRandomAddrPort() (addr string) {
	// 127.1-255.0-255.0-255:10000-50000
	ip := fmt.Sprintf("127.%d.%d.%d",
		rand.Int()%254+1, rand.Int()%255, rand.Int()%254)
	port := rand.Int()%40000 + 10000
	return fmt.Sprintf("%s:%d", ip, port)
}

func getRandomPort(ip string) (addr string) {
	// ip:1024-65535
	port := rand.Int()%(65536-1024) + 1024
	return fmt.Sprintf("%s:%d", ip, port)
}

func getFreeAddrPort() (addrPort string) {
	timeout := time.Now().Add(time.Second)
	for {
		if runtime.GOOS == "linux" {
			addrPort = getRandomAddrPort()
		} else {
			addrPort = getRandomPort("127.0.0.1")
		}
		ln, err := net.Listen("tcp", addrPort)
		if err == nil {
			ln.Close()
			return
		}

		if time.Now().After(timeout) {
			panic("Can't found free address:port on localhost")
		}
	}
}

func kvmExists() bool {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return false
	}
	return true
}

func (q *QemuSystem) panicWatcher() {
	for {
		time.Sleep(time.Second)
		if bytes.Contains(q.Stdout, []byte("Kernel panic")) {
			time.Sleep(time.Second)
			// There is no reason to stay alive after kernel panic
			q.Stop()
			q.KernelPanic = true
			return
		}
	}
}

// Start qemu process
func (q *QemuSystem) Start() (err error) {
	rand.Seed(time.Now().UnixNano()) // Are you sure?
	q.sshAddrPort = getFreeAddrPort()
	hostfwd := fmt.Sprintf("hostfwd=tcp:%s-:22", q.sshAddrPort)
	qemuArgs := []string{"-snapshot", "-nographic",
		"-hda", q.drivePath,
		"-kernel", q.kernel.KernelPath,
		"-append", "root=/dev/sda ignore_loglevel console=ttyS0 rw",
		"-smp", fmt.Sprintf("%d", q.Cpus),
		"-m", fmt.Sprintf("%d", q.Memory),
		"-device", "e1000,netdev=n1",
		"-netdev", "user,id=n1," + hostfwd,
	}

	if q.debug {
		qemuArgs = append(qemuArgs, "-gdb", q.gdb)
	}

	if q.kernel.InitrdPath != "" {
		qemuArgs = append(qemuArgs, "-initrd", q.kernel.InitrdPath)
	}

	if (q.arch == X86_64 || q.arch == I386) && kvmExists() {
		qemuArgs = append(qemuArgs, "-enable-kvm")
	}

	if q.arch == X86_64 && runtime.GOOS == "darwin" {
		qemuArgs = append(qemuArgs, "-accel", "hvf", "-cpu", "host")
	}

	q.cmd = exec.Command("qemu-system-"+string(q.arch), qemuArgs...)

	if q.pipe.stdin, err = q.cmd.StdinPipe(); err != nil {
		return
	}

	if q.pipe.stdout, err = q.cmd.StdoutPipe(); err != nil {
		return
	}

	if q.pipe.stderr, err = q.cmd.StderrPipe(); err != nil {
		return
	}

	err = q.cmd.Start()
	if err != nil {
		return
	}

	go readUntilEOF(q.pipe.stdout, &q.Stdout)
	go readUntilEOF(q.pipe.stderr, &q.Stderr)

	go func() {
		q.exitErr = q.cmd.Wait()
		q.Died = true
	}()

	time.Sleep(time.Second / 10) // wait for immediately die

	if q.Died {
		err = errors.New("qemu died immediately: " + string(q.Stderr))
	}

	go q.panicWatcher()

	if q.Timeout != 0 {
		go func() {
			time.Sleep(q.Timeout)
			q.KilledByTimeout = true
			q.Stop()
		}()
	}

	return
}

// Stop qemu process
func (q *QemuSystem) Stop() {
	// 1  00/01   01  01  SOH  (Ctrl-A)  START OF HEADING
	fmt.Fprintf(q.pipe.stdin, "%cx", 1)
	// wait for die
	time.Sleep(time.Second / 10)
	if !q.Died {
		q.cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(time.Second / 10)
		q.cmd.Process.Signal(syscall.SIGKILL)
	}
}

func (q QemuSystem) ssh(user string) (client *ssh.Client, err error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	client, err = ssh.Dial("tcp", q.sshAddrPort, cfg)
	return
}

// Command executes shell commands on qemu system
func (q QemuSystem) Command(user, cmd string) (output string, err error) {
	client, err := q.ssh(user)
	if err != nil {
		return
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return
	}

	bytesOutput, err := session.CombinedOutput(cmd)
	output = string(bytesOutput)
	return
}

// AsyncCommand executes command on qemu system but does not wait for exit
func (q QemuSystem) AsyncCommand(user, cmd string) (err error) {
	client, err := q.ssh(user)
	if err != nil {
		return
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return
	}

	return session.Run(fmt.Sprintf(
		"nohup sh -c '%s' > /dev/null 2> /dev/null < /dev/null &", cmd))
}

// CopyFile is copy file from local machine to remote through ssh/scp
func (q *QemuSystem) CopyFile(user, localPath, remotePath string) (err error) {
	addrPort := strings.Split(q.sshAddrPort, ":")
	addr := addrPort[0]
	port := addrPort[1]

	cmd := exec.Command("scp", "-P", port,
		"-o", "StrictHostKeyChecking=no",
		"-o", "LogLevel=error",
		localPath, user+"@"+addr+":"+remotePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(string(output))
	}

	return
}

// CopyAndInsmod copy kernel module to temporary file on qemu then insmod it
func (q *QemuSystem) CopyAndInsmod(localKoPath string) (output string, err error) {
	remoteKoPath := fmt.Sprintf("/tmp/module_%d.ko", rand.Int())
	err = q.CopyFile("root", localKoPath, remoteKoPath)
	if err != nil {
		return
	}

	return q.Command("root", "insmod "+remoteKoPath)
}

// CopyAndRun is copy local file to qemu vm then run it
func (q *QemuSystem) CopyAndRun(user, path string) (output string, err error) {
	remotePath := fmt.Sprintf("/tmp/executable_%d", rand.Int())
	err = q.CopyFile(user, path, remotePath)
	if err != nil {
		return
	}

	return q.Command(user, "chmod +x "+remotePath+" && "+remotePath)
}

// Debug is for enable qemu debug and set hostname and port for listen
func (q *QemuSystem) Debug(conn string) {
	q.debug = true
	q.gdb = conn
}

// GetSshCommand returns command for connect to qemu machine over ssh
func (q QemuSystem) GetSshCommand() (cmd string) {
	addrPort := strings.Split(q.sshAddrPort, ":")
	addr := addrPort[0]
	port := addrPort[1]

	cmd = "ssh -o StrictHostKeyChecking=no"
	cmd += " -p " + port + " root@" + addr
	return
}
