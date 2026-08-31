package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	BaseStorageDir = "/var/lib/funnybunny"
	BridgeName     = "br0"
)

func main() {
	// Re-execution check: if the first argument is "init", we are running
	// inside the container's PID/mount/network namespaces.
	if len(os.Args) > 1 && os.Args[1] == "init" {
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "[-] Invalid init arguments.")
			os.Exit(1)
		}
		runContainer(os.Args[2], os.Args[3])
		return
	}

	if os.Geteuid() != 0 {
		fmt.Println("[-] Please run this program with sudo/root privileges.")
		os.Exit(1)
	}

	if err := os.MkdirAll(BaseStorageDir, 0755); err != nil {
		fmt.Printf("[-] Failed to create storage directory: %v\n", err)
		os.Exit(1)
	}

	// Ensure bridge br0 exists on the host.
	if err := ensureBridgeExists(); err != nil {
		fmt.Printf("[-] Failed to configure bridge %s: %v\n", BridgeName, err)
		os.Exit(1)
	}

	clientID := generateClientID()
	fmt.Printf("[+] Generated Client ID: %s\n", clientID)

	clientDir := filepath.Join(BaseStorageDir, clientID)
	lowerDir := filepath.Join(clientDir, "lower")   // extracted base rootfs
	upperDir := filepath.Join(clientDir, "upper")   // writable layer
	workDir := filepath.Join(clientDir, "work")     // OverlayFS workdir
	mergedDir := filepath.Join(clientDir, "merged") // final rootfs view

	for _, dir := range []string{lowerDir, upperDir, workDir, mergedDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Printf("[-] Failed to create %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	sourceRoot := sourceRootDir()
	osPath := filepath.Join(sourceRoot, "os_images", "alpine-minirootfs-3.24.0-x86_64.tar.gz")

	// The directories are created before extraction, so checking only whether
	// lowerDir exists would incorrectly skip extraction. Use a marker instead.[cite: 2]
	if err := extractOS(lowerDir, osPath); err != nil {
		fmt.Printf("[-] Error extracting OS '%v': %v\n", filepath.Base(osPath), err)
		os.Exit(1)
	}

	// OverlayFS requires upperdir and workdir to be on the same filesystem.[cite: 2]
	overlayOpts := fmt.Sprintf(
		"lowerdir=%s,upperdir=%s,workdir=%s",
		lowerDir, upperDir, workDir,
	)
	if err := syscall.Mount("overlay", mergedDir, "overlay", 0, overlayOpts); err != nil {
		fmt.Printf("[-] Failed to mount OverlayFS: %v\n", err)
		os.Exit(1)
	}

	defer func() {
		if err := syscall.Unmount(mergedDir, 0); err != nil {
			fmt.Printf("[-] Warning: failed to unmount rootfs: %v\n", err)
		}
	}()

	etcDir := filepath.Join(mergedDir, "etc")
	if err := os.MkdirAll(etcDir, 0755); err != nil {
		fmt.Printf("[-] Error creating container /etc directory: %v\n", err)
		os.Exit(1)
	}

	// Alpine may ship resolv.conf as a symlink. Remove it before replacing it
	// with a regular file.[cite: 2]
	resolvPath := filepath.Join(etcDir, "resolv.conf")
	if err := os.Remove(resolvPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("[-] Error preparing resolv.conf: %v\n", err)
		os.Exit(1)
	}

	if err := copyFile("/etc/resolv.conf", resolvPath); err != nil {
		fmt.Printf("[-] Error copying resolv.conf into container: %v\n", err)
		os.Exit(1)
	}

	// Setup a cgroup if cgroup v2 is available.[cite: 2]
	cgroupDir := filepath.Join("/sys/fs/cgroup", "vps-"+clientID)
	if err := setupCgroup(cgroupDir); err != nil {
		fmt.Printf("[-] Warning: cgroup setup failed: %v\n", err)
	}

	// Linux interface names are limited to 15 characters. Also, do not use
	// "eth0" as the peer while it is still in the host namespace.
	// Use short generated names that cannot collide with a normal host eth0.
	nameSeed := uint32(time.Now().UnixNano()) ^ uint32(os.Getpid())
	vethHost := fmt.Sprintf("vh%08x", nameSeed)
	vethContainer := fmt.Sprintf("vc%08x", nameSeed)

	// Start child first so its PID identifies the new network namespace.
	cmd := exec.Command("/proc/self/exe", "init", mergedDir, vethContainer)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWNET,
	}

	if err := cmd.Start(); err != nil {
		fmt.Printf("[-] Failed to start container: %v\n", err)
		os.Exit(1)
	}

	containerPID := cmd.Process.Pid

	if err := createVethPair(vethHost, vethContainer); err != nil {
		fmt.Printf("[-] Failed to create veth pair: %v\n", err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		os.Exit(1)
	}

	defer runCmd("ip", "link", "del", vethHost)

	// Move the peer first. Once moved, it is no longer visible in the host
	// namespace and is owned by the child's network namespace.
	if err := moveVethToNamespace(vethContainer, containerPID, 30*time.Second); err != nil {
		fmt.Printf("[-] Failed to move %s into container namespace: %v\n", vethContainer, err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		os.Exit(1)
	}

	// Keep the generated interface name inside the container namespace.
	// Renaming here would race with the child, which is already waiting for
	// vethContainer. The generated name is valid on Linux (<= 15 chars).

	// Configure the host endpoint after the peer has been handed off.
	if err := runCmd("ip", "link", "set", vethHost, "master", BridgeName); err != nil {
		fmt.Printf("[-] Failed to attach %s to %s: %v\n", vethHost, BridgeName, err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		os.Exit(1)
	}

	if err := runCmd("ip", "link", "set", vethHost, "up"); err != nil {
		fmt.Printf("[-] Failed to bring %s up: %v\n", vethHost, err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		os.Exit(1)
	}

	// Move container process into the cgroup tree.[cite: 2]
	if err := addToCgroup(cgroupDir, containerPID); err != nil {
		fmt.Printf("[-] Warning: failed to add process to cgroup: %v\n", err)
	}

	if err := cmd.Wait(); err != nil {
		fmt.Printf("[-] Container exited with error: %v\n", err)
	}

	fmt.Printf("[+] VPS session %s ended. Cleaning up...\n", clientID)

	if err := os.RemoveAll(cgroupDir); err != nil {
		fmt.Printf("[-] Warning: failed to remove cgroup %s: %v\n", cgroupDir, err)
	}
}

func runContainer(mergedDir string, vethName string) {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fmt.Printf("[-] Error: failed to make mount tree private: %v\n", err)
		os.Exit(1)
	}

	// The parent process creates the veth and moves it into this namespace.
	// Keep PID 1 alive long enough for that hand-off to complete. Do NOT
	// configure networking until the peer has actually arrived.
	if err := waitForInterface(vethName, 60*time.Second); err != nil {
		fmt.Printf("[-] Error waiting for network interface %s: %v\n", vethName, err)
		os.Exit(1)
	}

	if err := setupContainerNetwork(vethName); err != nil {
		fmt.Printf("[-] Error configuring container network: %v\n", err)
		os.Exit(1)
	}

	if err := syscall.Chroot(mergedDir); err != nil {
		fmt.Printf("[-] Error changing root filesystem to %s: %v\n", mergedDir, err)
		os.Exit(1)
	}

	if err := syscall.Chdir("/"); err != nil {
		fmt.Printf("[-] Error changing working directory: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll("/proc", 0555); err != nil {
		fmt.Printf("[-] Error creating /proc: %v\n", err)
		os.Exit(1)
	}

	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fmt.Printf("[-] Warning: failed to mount procfs: %v\n", err)
	} else {
		defer syscall.Unmount("/proc", 0)
	}

	fmt.Println("[+] Container started. Type 'exit' to stop.")

	shell := exec.Command("/bin/sh")
	shell.Stdin = os.Stdin
	shell.Stdout = os.Stdout
	shell.Stderr = os.Stderr

	if err := shell.Run(); err != nil {
		fmt.Printf("[-] Shell execution error: %v\n", err)
	}
}

func setupContainerNetwork(vethName string) error {
	if err := waitForInterface(vethName, 15*time.Second); err != nil {
		return err
	}

	if err := runCmd("ip", "addr", "add", "10.100.0.50/24", "dev", vethName); err != nil {
		if !strings.Contains(err.Error(), "File exists") {
			return err
		}
	}

	if err := runCmd("ip", "link", "set", vethName, "up"); err != nil {
		return err
	}

	if err := runCmd("ip", "link", "set", "lo", "up"); err != nil {
		return err
	}

	if err := runCmd("ip", "route", "replace", "default", "via", "10.100.0.1"); err != nil {
		return err
	}

	return nil
}

func waitForInterface(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join("/sys/class/net", name)); err == nil {
			return nil
		}
		if data, err := os.ReadFile("/proc/net/dev"); err == nil &&
			strings.Contains(string(data), name+":") {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("interface %q did not appear within %s", name, timeout)
}

func moveVethToNamespace(name string, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		if err := runCmd("ip", "link", "set", name, "netns", fmt.Sprintf("%d", pid)); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("timed out moving %s to network namespace of PID %d: %w", name, pid, lastErr)
}

func ensureBridgeExists() error {
	if err := exec.Command("ip", "link", "show", BridgeName).Run(); err == nil {
		// Make sure an existing bridge has the expected address and is up.[cite: 2]
		_ = runCmd("ip", "addr", "add", "10.100.0.1/24", "dev", BridgeName)
		_ = runCmd("ip", "link", "set", BridgeName, "up")
		_ = runCmd("sysctl", "-w", "net.ipv4.ip_forward=1")
		return nil
	}

	fmt.Printf("[+] Bridge %s does not exist. Creating it...\n", BridgeName)

	if err := runCmd("ip", "link", "add", "name", BridgeName, "type", "bridge"); err != nil {
		return err
	}

	if err := runCmd("ip", "addr", "add", "10.100.0.1/24", "dev", BridgeName); err != nil {
		_ = runCmd("ip", "link", "del", BridgeName)
		return err
	}

	if err := runCmd("ip", "link", "set", BridgeName, "up"); err != nil {
		_ = runCmd("ip", "link", "del", BridgeName)
		return err
	}

	if err := runCmd("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}

	return nil
}

func createVethPair(hostName, containerName string) error {
	// Both names are generated by this program, so cleaning them is safe.
	_ = runCmd("ip", "link", "del", hostName)
	_ = runCmd("ip", "link", "del", containerName)

	return runCmd(
		"ip", "link", "add",
		hostName,
		"type", "veth",
		"peer", "name", containerName,
	)
}

func setupCgroup(cgroupDir string) error {
	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		return err
	}

	// cgroup v2 memory limit: 100 MiB.[cite: 2]
	if err := os.WriteFile(
		filepath.Join(cgroupDir, "memory.max"),
		[]byte("104857600"),
		0644,
	); err != nil {
		_ = os.Remove(cgroupDir)
		return err
	}

	return nil
}

func addToCgroup(cgroupDir string, pid int) error {
	return os.WriteFile(
		filepath.Join(cgroupDir, "cgroup.procs"),
		fmt.Appendf(nil, "%d", pid),
		0644,
	)
}

// Extract OS from tar.gz if it has not already been extracted.[cite: 2]
func extractOS(lowerDir, osPath string) error {
	if _, err := os.Stat(osPath); err != nil {
		return fmt.Errorf("OS image not found: %w", err)
	}

	marker := filepath.Join(lowerDir, ".rootfs-ready")
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	fmt.Printf("[+] Extracting OS '%v' to '%v'...\n", filepath.Base(osPath), lowerDir)

	if err := os.MkdirAll(lowerDir, 0755); err != nil {
		return err
	}

	if err := extractTarGz(osPath, lowerDir); err != nil {
		return fmt.Errorf("error extracting rootfs: %w", err)
	}

	if err := os.WriteFile(marker, []byte("ready\n"), 0644); err != nil {
		return fmt.Errorf("failed to write rootfs marker: %w", err)
	}

	return nil
}

func generateClientID() string {
	return fmt.Sprintf("vps-%04d", rand.Intn(10000))
}

func sourceRootDir() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		log.Fatalln("No caller data available")
	}

	return filepath.Dir(filename)
}

func runCmd(name string, arg ...string) error {
	cmd := exec.Command(name, arg...)
	if output, err := cmd.CombinedOutput(); err != nil {
		if len(output) > 0 {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
		}
		return err
	}
	return nil
}

func extractTarGz(tarball, targetDir string) error {
	file, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		name := filepath.Clean(header.Name)
		if name == "." || name == "" {
			continue // Safely skip root pointer headers from the tarball
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in tar archive: %q", header.Name)
		}

		target := filepath.Join(targetDir, name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)|0700); err != nil {
				return err
			}

		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}

			outFile, err := os.OpenFile(
				target,
				os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
				os.FileMode(header.Mode),
			)
			if err != nil {
				return err
			}

			_, copyErr := io.Copy(outFile, tarReader)
			closeErr := outFile.Close()

			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}

		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}

		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			linkTarget := filepath.Join(targetDir, filepath.Clean(header.Linkname))
			if err := os.Link(linkTarget, target); err != nil {
				return err
			}

		default:
			continue
		}
	}

	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
