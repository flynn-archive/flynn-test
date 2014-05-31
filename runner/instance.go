package main

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"code.google.com/p/go.crypto/ssh"
)

func NewVMManager() *VMManager {
	return &VMManager{taps: &TapManager{}}
}

type VMManager struct {
	taps   *TapManager
	nextID uint64
}

type VMConfig struct {
	Kernel string
	User   int
	Group  int
	Memory string
	Drives map[string]VMDrive
	Args   []string
	Out    io.Writer

	netFS string
}

type VMDrive struct {
	FS  string
	COW string

	TempCOW bool
}

func (u *VMManager) NewInstance(c *VMConfig) (Instance, error) {
	id := atomic.AddUint64(&u.nextID, 1) - 1
	inst := &vm{
		ID:       fmt.Sprintf("flynn%d", id),
		VMConfig: c,
	}
	if c.Kernel == "" {
		c.Kernel = "vmlinuz"
	}
	if c.Out == nil {
		var err error
		c.Out, err = os.Create(inst.ID + ".log")
		if err != nil {
			return nil, err
		}
	}
	var err error
	inst.tap, err = u.taps.NewTap(c.User, c.Group)
	return inst, err
}

type Instance interface {
	DialSSH() (*ssh.Client, error)
	Start() error
	Wait() error
	Kill() error
	IP() string
}

type vm struct {
	ID string
	*VMConfig
	tap *Tap
	cmd *exec.Cmd

	tempFiles []string
}

func (u *vm) writeInterfaceConfig() error {
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	u.tempFiles = append(u.tempFiles, dir)
	u.netFS = dir

	if err := os.Chmod(dir, 0755); err != nil {
		os.RemoveAll(dir)
		return err
	}

	f, err := os.Create(filepath.Join(dir, "eth0"))
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	defer f.Close()

	return u.tap.WriteInterfaceConfig(f)
}

func (u *vm) cleanup() {
	for _, f := range u.tempFiles {
		os.RemoveAll(f)
	}
	u.tap.Close()
	u.tempFiles = nil
}

func (u *vm) Start() error {
	u.writeInterfaceConfig()
	u.Args = append(u.Args,
		"-kernel", u.Kernel,
		"-append", `"root=/dev/sda"`,
		"-net", "nic",
		"-net", "tap,ifname="+u.tap.Name+",script=no,downscript=no",
		"-virtfs", "fsdriver=local,path="+u.netFS+",security_model=passthrough,readonly,mount_tag=netfs",
		"-nographic",
	)
	if u.Memory != "" {
		u.Args = append(u.Args, "-m", u.Memory)
	}
	var err error
	for i, d := range u.Drives {
		fs := d.FS
		if d.TempCOW {
			fs, err = u.tempCOW(d.FS)
			if err != nil {
				u.cleanup()
				return err
			}
		}
		u.Args = append(u.Args, fmt.Sprintf("-%s", i), fs)
	}
	u.cmd, err = (&execReq{
		Uid:  u.User,
		Gid:  u.Group,
		Path: "/usr/bin/qemu-system-x86_64",
		Argv: append([]string{"/usr/bin/qemu-system-x86_64"}, u.Args...),
		Env:  []string{"HOME=" + os.Getenv("HOME")},
	}).start(u.Out)
	if err != nil {
		u.cleanup()
	}
	return err
}

func (u *vm) tempCOW(image string) (string, error) {
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return "", err
	}
	u.tempFiles = append(u.tempFiles, dir)
	if err := os.Chown(dir, u.User, u.Group); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "fs.img")
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", "-b", image, path)
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to create COW filesystem: %s", err.Error())
	}
	if err := os.Chown(path, u.User, u.Group); err != nil {
		return "", err
	}
	return path, nil
}

func (u *vm) Wait() error {
	defer u.cleanup()
	return u.cmd.Wait()
}

func (u *vm) Kill() error {
	defer u.cleanup()
	return u.cmd.Process.Kill()
}

func (u *vm) DialSSH() (*ssh.Client, error) {
	return ssh.Dial("tcp", u.tap.RemoteIP.String()+":2222", &ssh.ClientConfig{
		User: "ubuntu",
		Auth: []ssh.AuthMethod{ssh.Password("ubuntu")},
	})
}

func (u *vm) IP() string {
	return u.tap.RemoteIP.String()
}

type execReq struct {
	Uid  int
	Gid  int
	Path string
	Argv []string // must include path as Argv[0]
	Env  []string
	Dir  string
}

func (r *execReq) start(out io.Writer) (*exec.Cmd, error) {
	var buf bytes.Buffer
	b64 := base64.NewEncoder(base64.StdEncoding, &buf)
	if err := gob.NewEncoder(b64).Encode(r); err != nil {
		return nil, err
	}
	b64.Close()

	cmd := exec.Command(os.Args[0])
	cmd.Env = []string{"_EXEC_REQ=" + buf.String()}
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd, cmd.Start()
}

func maybeExec() {
	data := os.Getenv("_EXEC_REQ")
	if data == "" {
		return
	}

	var req execReq
	if err := gob.NewDecoder(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))).Decode(&req); err != nil {
		log.Fatalf("failed to decode exec request: %s", err)
	}
	if req.Gid > 0 {
		if err := syscall.Setgid(req.Gid); err != nil {
			log.Fatalf("failed to setgid(%d): %s", req.Gid, err)
		}
	}
	if req.Uid > 0 {
		if err := syscall.Setuid(req.Uid); err != nil {
			log.Fatalf("failed to setuid(%d): %s", req.Uid, err)
		}
	}
	if req.Dir != "" {
		if err := os.Chdir(req.Dir); err != nil {
			log.Fatalf("failed to chdir to %q: %s", req.Dir, err)
		}
	}
	fmt.Println("execing", req.Path, req.Argv)
	if err := syscall.Exec(req.Path, req.Argv, req.Env); err != nil {
		log.Fatalf("failed to exec %q: %s", req.Path, err)
	}
}
