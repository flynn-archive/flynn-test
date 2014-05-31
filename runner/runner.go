package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"time"

	"code.google.com/p/go.crypto/ssh"
	"github.com/flynn/go-flynn/attempt"
)

func init() {
	log.SetFlags(log.Lshortfile)
}

func main() {
	maybeExec()
	username := flag.String("user", "ubuntu", "user to run QEMU as")
	rootfs := flag.String("rootfs", "rootfs.img", "fs image to use with QEMU")
	kernel := flag.String("kernel", "vmlinuz", "path to the Linux binary")
	flag.Parse()
	u, err := user.Lookup(*username)
	if err != nil {
		log.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	fmt.Println("Initializing networking...")
	if err := initNetworking(); err != nil {
		log.Fatalf("net init error: %s", err)
	}

	fmt.Println("Creating docker fs...")
	// create 16GB sparse fs image to store docker data on
	dockerRoot, err := createFS(17179869184, uid, gid)
	if err != nil {
		log.Fatal(err)
	}

	vm := NewVMManager()

	build, err := vm.NewInstance(&VMConfig{
		Kernel: *kernel,
		User:   uid,
		Group:  gid,
		Memory: "512",
		Drives: map[string]VMDrive{
			"hda": VMDrive{FS: *rootfs, TempCOW: true},
			"hdb": VMDrive{FS: dockerRoot},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Booting build instance...")
	if err := build.Start(); err != nil {
		log.Fatalf("error starting build instance: %s", err)
	}

	fmt.Println("Waiting for instance to boot...")
	if err := buildFlynn(build); err != nil {
		build.Kill()
		log.Fatal(err)
	}

	if err := build.Kill(); err != nil {
		log.Fatalf("error while stopping build instance: %s", err)
	}

	os.Exit(0)

	instances := make([]Instance, 5)
	for i := 0; i < 5; i++ {
		inst, err := vm.NewInstance(&VMConfig{
			User:   uid,
			Group:  gid,
			Memory: "512MB",
			Drives: map[string]VMDrive{
				"hda": VMDrive{FS: *rootfs, TempCOW: true},
				"hdb": VMDrive{FS: dockerRoot, TempCOW: true},
			},
		})
		if err != nil {
			log.Fatalf("error starting instance %d: %s", i, err)
		}
		instances[i] = inst
	}

	// ssh connect, bootstrap layer 0
	// bootstrap layer 1
	// run tests

	for i, inst := range instances {
		if err := inst.Kill(); err != nil {
			log.Println("error killing instance %d: %s", i, err)
		}
	}
}

var attempts = attempt.Strategy{
	Min:   5,
	Total: 5 * time.Minute,
	Delay: time.Second,
}

func buildFlynn(inst Instance) error {
	buildScript := bytes.NewReader([]byte(`
#!/bin/bash
set -e -x

flynn=~/go/src/github.com/flynn
mkdir -p $flynn
export GOPATH=~/go

git clone https://github.com/flynn/flynn-devbox
cd flynn-devbox
./checkout-flynn manifest.txt $flynn
./build-flynn $flynn

sudo umount /var/lib/docker
`[1:]))

	var sc *ssh.Client
	err := attempts.Run(func() (err error) {
		fmt.Printf("Attempting to ssh to %s:2222...\n", inst.IP())
		sc, err = inst.DialSSH()
		return
	})
	if err != nil {
		return err
	}
	defer sc.Close()
	sess, err := sc.NewSession()
	sess.Stdin = buildScript
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr
	if err := sess.Run("bash"); err != nil {
		return fmt.Errorf("build error: %s", err)
	}
	return nil
}

func createFS(size int64, uid, gid int) (string, error) {
	f, err := ioutil.TempFile("", "")
	if err != nil {
		return "", err
	}
	if _, err := f.Seek(size, 0); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if _, err := f.Write([]byte{0}); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	f.Chown(uid, gid)
	f.Close()

	res, err := exec.Command("mkfs.btrfs", f.Name()).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mkfs.btrfs error %s - %q", err, res)
	}
	return f.Name(), nil
}
