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

func NewUMLManager() *UMLManager {
	return &UMLManager{taps: &TapManager{}}
}

type UMLManager struct {
	taps   *TapManager
	nextID uint64
}

type UMLConfig struct {
	Path   string
	User   int
	Group  int
	Memory string
	Drives []UMLDrive
	Args   []string
	Out    io.Writer

	hostFS string
}

type UMLDrive struct {
	FS  string
	COW string

	TempCOW bool
}

func (u *UMLManager) NewInstance(c *UMLConfig) (Instance, error) {
	id := atomic.AddUint64(&u.nextID, 1) - 1
	inst := &uml{
		ID:        fmt.Sprintf("flynn%d", id),
		UMLConfig: c,
	}
	if c.Path == "" {
		c.Path = "linux"
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

type uml struct {
	ID string
	*UMLConfig
	tap *Tap
	cmd *exec.Cmd

	tempFiles []string
}

//  linux mem=512M ubd0=uml0.cow1,rootfs.img umid=uml0 con0=fd:0,fd:1 con=pts rw eth0=tuntap,flynntap0 hostfs=`pwd`/net

func (u *uml) writeInterfaceConfig() error {
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	u.tempFiles = append(u.tempFiles, dir)
	u.hostFS = dir

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

func (u *uml) cleanup() {
	for _, f := range u.tempFiles {
		os.RemoveAll(f)
	}
	u.tap.Close()
	u.tempFiles = nil
}

func (u *uml) Start() error {
	u.writeInterfaceConfig()
	u.Args = append(u.Args,
		"umid="+u.ID,
		"con0=fd:0,fd:1",
		"con=pts",
		"rw",
		"eth0=tuntap,"+u.tap.Name,
		"hostfs="+u.hostFS,
	)
	if u.Memory != "" {
		u.Args = append(u.Args, "mem="+u.Memory)
	}
	var err error
	for i, d := range u.Drives {
		if d.TempCOW {
			d.COW, err = u.tempCOW()
			if err != nil {
				u.cleanup()
				return err
			}
		}
		if d.COW != "" {
			u.Args = append(u.Args, fmt.Sprintf("ubd%d=%s,%s", i, d.COW, d.FS))
		} else {
			u.Args = append(u.Args, fmt.Sprintf("ubd%d=%s", i, d.FS))
		}
	}
	u.cmd, err = (&execReq{
		Uid:  u.User,
		Gid:  u.Group,
		Path: u.Path,
		Argv: append([]string{"linux"}, u.Args...),
		Env:  []string{"HOME=" + os.Getenv("HOME")},
	}).start(u.Out)
	if err != nil {
		u.cleanup()
	}
	return err
}

func (u *uml) tempCOW() (string, error) {
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return "", err
	}
	if err := os.Chown(dir, u.User, u.Group); err != nil {
		return "", err
	}
	u.tempFiles = append(u.tempFiles, dir)
	return filepath.Join(dir, "fs.cow1"), nil
}

func (u *uml) Wait() error {
	defer u.cleanup()
	return u.cmd.Wait()
}

func (u *uml) Kill() error {
	defer u.cleanup()
	return u.cmd.Process.Kill()
}

func (u *uml) DialSSH() (*ssh.Client, error) {
	return ssh.Dial("tcp", u.tap.RemoteIP.String()+":2222", &ssh.ClientConfig{
		User: "ubuntu",
		Auth: []ssh.AuthMethod{ssh.Password("ubuntu")},
	})
}

func (u *uml) IP() string {
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
