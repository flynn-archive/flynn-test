package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
)

func init() {
	log.SetFlags(log.Lshortfile)
}

func main() {
	maybeExec()
	username := flag.String("user", "ubuntu", "user to run UML as")
	rootfs := flag.String("rootfs", "rootfs.img", "fs image to use with UML")
	linux := flag.String("linux", "linux", "path to the UML binary")
	flag.Parse()
	u, err := user.Lookup(*username)
	if err != nil {
		log.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	if err := initNetworking(); err != nil {
		log.Fatalf("net init error: %s", err)
	}

	// create 16GB fs image to store docker data on
	dockerRoot, err := createFS(17179869184, uid, gid)
	if err != nil {
		log.Fatal(err)
	}

	um := NewUMLManager()

	build, err := um.NewInstance(&UMLConfig{
		Path:   *linux,
		User:   uid,
		Group:  gid,
		Memory: "512MB",
		Drives: []UMLDrive{
			{FS: *rootfs, TempCOW: true},
			{FS: dockerRoot},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := build.Start(); err != nil {
		log.Fatalf("error starting build instance: %s", err)
	}

	// download and compile flynn components

	if err := build.Wait(); err != nil {
		log.Fatalf("error while stopping build instance: %s", err)
	}

	instances := make([]Instance, 5)
	for i := 0; i < 5; i++ {
		inst, err := um.NewInstance(&UMLConfig{
			Path:   *linux,
			User:   uid,
			Group:  gid,
			Memory: "512MB",
			Drives: []UMLDrive{
				{FS: *rootfs, TempCOW: true},
				{FS: dockerRoot, TempCOW: true},
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

	res, err := exec.Command("mkfs.ext4", "-Fq", f.Name()).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mkfs.ext4 error %s - %q", err, res)
	}
	return f.Name(), nil
}
