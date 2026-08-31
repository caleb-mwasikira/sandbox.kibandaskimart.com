package main

import (
	"archive/tar"
	"compress/gzip"
	cryptorand "crypto/rand"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const (
	BaseStorageDir = "/var/lib/funnybunny"
	BridgeName     = "br0"
	ContainerIP    = "10.100.0.50"
	DBFileName     = "funnybunny.db"
)

func main() {
	osFlag := flag.String("os", "alpine", "OS image name to use (e.g. alpine, ubuntu)")
	flag.Parse()

	args := flag.Args()
	if len(args) > 0 && (args[0] == "add-os" || args[0] == "add-iso") {
		if len(args) < 3 {
			fmt.Printf("[-] Usage: go run main.go %s <name> <path/to/file>\n", args[0])
			os.Exit(1)
		}
		registerOSImage(args[1], args[2])
		return
	}

	if len(args) > 0 && args[0] == "delete-os" {
		if len(args) < 2 {
			fmt.Println("[-] Usage: go run main.go delete-os <name>")
			os.Exit(1)
		}
		deleteOSImage(args[1])
		return
	}

	if len(args) > 0 && args[0] == "list-os" {
		listOSImages()
		return
	}

	if len(args) > 1 && args[0] == "init" {
		runContainer(args[1], args[2])
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

	db := initDB()
	defer db.Close()

	osRecord, err := getOSImage(db, *osFlag)
	if err != nil {
		fmt.Printf("[-] OS image '%s' not found in database. Register it using: go run main.go add-os <name> <path>\n", *osFlag)
		os.Exit(1)
	}

	if err := ensureBridgeExists(); err != nil {
		fmt.Printf("[-] Failed to configure bridge %s: %v\n", BridgeName, err)
		os.Exit(1)
	}

	var clientID string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		clientID = args[0]
	} else {
		clientID = generateClientID()
	}
	fmt.Printf("[+] Using Client ID: %s (OS: %s)\n", clientID, osRecord.Name)

	clientDir := filepath.Join(BaseStorageDir, clientID)
	lowerDir := filepath.Join(clientDir, "lower")
	upperDir := filepath.Join(clientDir, "upper")
	workDir := filepath.Join(clientDir, "work")
	mergedDir := filepath.Join(clientDir, "merged")

	for _, dir := range []string{lowerDir, upperDir, workDir, mergedDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Printf("[-] Failed to create %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	if err := extractOS(lowerDir, osRecord.Path); err != nil {
		fmt.Printf("[-] Error preparing OS '%v': %v\n", filepath.Base(osRecord.Path), err)
		os.Exit(1)
	}

	defer func() {
		_ = syscall.Unmount(lowerDir, 0)
	}()

	overlayOpts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerDir, upperDir, workDir)
	if err := syscall.Mount("overlay", mergedDir, "overlay", 0, overlayOpts); err != nil {
		fmt.Printf("[-] Failed to mount OverlayFS: %v\n", err)
		os.Exit(1)
	}

	defer func() {
		if err := syscall.Unmount(mergedDir, 0); err != nil {
			fmt.Printf("[-] Warning: failed to unmount rootfs: %v\n", err)
		}
	}()

	sshPassFile := filepath.Join(clientDir, ".ssh_passwd")
	var sshPassword string
	if data, err := os.ReadFile(sshPassFile); err == nil {
		sshPassword = strings.TrimSpace(string(data))
	} else {
		sshPassword = generateRandomPassword(10)
		_ = os.WriteFile(sshPassFile, []byte(sshPassword+"\n"), 0600)
		configureSSHUser(mergedDir, clientID, sshPassword)
	}

	fmt.Println("\n========================================")
	fmt.Printf("[+] SSH Access Enabled for Container!\n")
	fmt.Printf("[+] IP Address: %s\n", ContainerIP)
	fmt.Printf("[+] Username:   %s\n", clientID)
	fmt.Printf("[+] Password:   %s\n", sshPassword)
	fmt.Printf("[+] Command:    ssh %s@%s\n", clientID, ContainerIP)
	fmt.Println("========================================\n")

	cgroupDir := filepath.Join("/sys/fs/cgroup", clientID)
	if err := setupCgroup(cgroupDir); err != nil {
		fmt.Printf("[-] Warning: cgroup setup failed: %v\n", err)
	}

	nameSeed := uint32(time.Now().UnixNano()) ^ uint32(os.Getpid())
	vethHost := fmt.Sprintf("vh%08x", nameSeed)
	vethContainer := fmt.Sprintf("vc%08x", nameSeed)

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

	if err := moveVethToNamespace(vethContainer, containerPID, 30*time.Second); err != nil {
		fmt.Printf("[-] Failed to move %s into container namespace: %v\n", vethContainer, err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		os.Exit(1)
	}

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

	if err := addToCgroup(cgroupDir, containerPID); err != nil {
		fmt.Printf("[-] Warning: failed to add process to cgroup: %v\n", err)
	}

	if err := cmd.Wait(); err != nil {
		fmt.Printf("[-] Container exited with error: %v\n", err)
	}

	fmt.Printf("[+] VPS session %s ended. State preserved in %s\n", clientID, upperDir)

	if err := os.RemoveAll(cgroupDir); err != nil {
		fmt.Printf("[-] Warning: failed to remove cgroup %s: %v\n", cgroupDir, err)
	}
}

type OSImageRecord struct {
	Name string
	Path string
}

func initDB() *sql.DB {
	dbPath := filepath.Join(BaseStorageDir, DBFileName)
	_ = os.MkdirAll(BaseStorageDir, 0755)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("[-] Failed to open sqlite database: %v", err)
	}

	query := `CREATE TABLE IF NOT EXISTS os_images (
		name TEXT PRIMARY KEY,
		path TEXT NOT NULL
	);`
	if _, err := db.Exec(query); err != nil {
		log.Fatalf("[-] Failed to create table: %v", err)
	}

	return db
}

func registerOSImage(name, imagePath string) {
	absPath, err := filepath.Abs(imagePath)
	if err != nil {
		log.Fatalf("[-] Invalid path: %v", err)
	}

	db := initDB()
	defer db.Close()

	_, err = db.Exec("INSERT OR REPLACE INTO os_images (name, path) VALUES (?, ?)", name, absPath)
	if err != nil {
		log.Fatalf("[-] Failed to add OS/ISO image in database: %v", err)
	}

	fmt.Printf("[+] Successfully registered image '%s' -> %s\n", name, absPath)
}

func deleteOSImage(name string) {
	db := initDB()
	defer db.Close()

	result, err := db.Exec("DELETE FROM os_images WHERE name = ?", name)
	if err != nil {
		log.Fatalf("[-] Failed to delete OS image from database: %v", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Fatalf("[-] Failed to check deletion result: %v", err)
	}

	if rowsAffected == 0 {
		fmt.Printf("[-] OS image '%s' not found in database.\n", name)
		return
	}

	fmt.Printf("[+] Successfully deleted OS image '%s' from database.\n", name)
}

func listOSImages() {
	db := initDB()
	defer db.Close()

	rows, err := db.Query("SELECT name, path FROM os_images")
	if err != nil {
		log.Fatalf("[-] Failed to query database: %v", err)
	}
	defer rows.Close()

	fmt.Println("Available OS/ISO Images:")
	fmt.Println("----------------------------------------")
	found := false
	for rows.Next() {
		var name, path string
		if err := rows.Scan(&name, &path); err != nil {
			continue
		}
		fmt.Printf("Name: %s\nPath: %s\n\n", name, path)
		found = true
	}

	if err := rows.Err(); err != nil {
		log.Printf("[-] Error during rows iteration: %v", err)
	}

	if !found {
		fmt.Println("No OS images registered yet.")
	}
}

func getOSImage(db *sql.DB, name string) (OSImageRecord, error) {
	var record OSImageRecord
	row := db.QueryRow("SELECT name, path FROM os_images WHERE name = ?", name)
	err := row.Scan(&record.Name, &record.Path)
	if err != nil {
		return record, err
	}
	return record, nil
}

func configureSSHUser(mergedDir, username, password string) {
	_ = os.MkdirAll(filepath.Join(mergedDir, "etc", "ssh"), 0755)
	_ = os.MkdirAll(filepath.Join(mergedDir, "var", "empty"), 0755)

	_ = os.WriteFile(filepath.Join(mergedDir, "etc", "resolv.conf"), []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0644)

	sshdConfig := "ListenAddress 10.100.0.50\nPermitRootLogin yes\nPasswordAuthentication yes\nPermitEmptyPasswords no\n"
	_ = os.WriteFile(filepath.Join(mergedDir, "etc", "ssh", "sshd_config"), []byte(sshdConfig), 0644)

	addCmd := fmt.Sprintf(
		"export DEBIAN_FRONTEND=noninteractive && "+
			"if command -v apt-get >/dev/null 2>&1; then "+
			"  apt-get update && apt-get install -y --no-install-recommends bash zsh openssh-server passwd shadow; "+
			"elif command -v apk >/dev/null 2>&1; then "+
			"  apk update && apk add bash zsh openssh shadow; "+
			"elif command -v dnf >/dev/null 2>&1; then "+
			"  dnf install -y bash zsh openssh-server passwd shadow-utils; "+
			"elif command -v yum >/dev/null 2>&1; then "+
			"  yum install -y bash zsh openssh-server passwd shadow-utils; "+
			"else "+
			"  echo '[-] Error: No supported package manager found in container rootfs!' >&2; "+
			"  exit 1; "+
			"fi && "+
			"id -u %s >/dev/null 2>&1 || useradd -m -s /bin/bash %s || adduser -D -s /bin/bash %s || true && "+
			"grep -qxF '/bin/bash' /etc/shells || echo '/bin/bash' >> /etc/shells",
		username, username, username,
	)
	chrootCmd := exec.Command("chroot", mergedDir, "/bin/sh", "-c", addCmd)
	if out, err := chrootCmd.CombinedOutput(); err != nil {
		fmt.Printf("[-] SSH installation failed/warn: %v\n%s\n", err, out)
	}

	passCmd := fmt.Sprintf("echo '%s:%s' | chpasswd && echo 'root:%s' | chpasswd", username, password, password)
	chrootPassCmd := exec.Command("chroot", mergedDir, "/bin/sh", "-c", passCmd)
	if out, err := chrootPassCmd.CombinedOutput(); err != nil {
		fmt.Printf("[-] Password setup failed: %v\n%s\n", err, out)
	}

	configureShellRc(mergedDir, username)
	configureShellRc(mergedDir, "root")
}

func configureShellRc(mergedDir, username string) {
	var userHome string
	if username == "root" {
		userHome = filepath.Join(mergedDir, "root")
	} else {
		userHome = filepath.Join(mergedDir, "home", username)
	}
	_ = os.MkdirAll(userHome, 0755)

	bashProfileContent := "# Automatically source .bashrc on login\nif [ -f ~/.bashrc ]; then\n    . ~/.bashrc\nfi\n"
	zshProfileContent := "# Automatically source .zshrc on login\nif [ -f ~/.zshrc ]; then\n    . ~/.zshrc\nfi\n"

	_ = os.WriteFile(filepath.Join(userHome, ".bash_profile"), []byte(bashProfileContent), 0644)
	_ = os.WriteFile(filepath.Join(userHome, ".profile"), []byte(bashProfileContent), 0644)
	_ = os.WriteFile(filepath.Join(userHome, ".zprofile"), []byte(zshProfileContent), 0644)

	bashRcPath := filepath.Join(userHome, ".bashrc")
	if _, err := os.Stat(bashRcPath); os.IsNotExist(err) {
		_ = os.WriteFile(bashRcPath, []byte("export PATH=$PATH:/usr/local/bin\nalias ll='ls -la'\n"), 0644)
	}

	zshRcPath := filepath.Join(userHome, ".zshrc")
	if _, err := os.Stat(zshRcPath); os.IsNotExist(err) {
		_ = os.WriteFile(zshRcPath, []byte("export PATH=$PATH:/usr/local/bin\nalias ll='ls -la'\n"), 0644)
	}
}

func runContainer(mergedDir string, vethName string) {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fmt.Printf("[-] Error: failed to make mount tree private: %v\n", err)
		os.Exit(1)
	}

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

	if err := os.MkdirAll("/proc", 0555); err == nil {
		_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	}

	// Mount an isolated tmpfs for container /dev to protect host /dev
	if err := os.MkdirAll("/dev", 0755); err == nil {
		_ = syscall.Mount("tmpfs", "/dev", "tmpfs", syscall.MS_NOSUID|syscall.MS_STRICTATIME, "mode=0755")
	}

	// Helper to safely create essential isolated device nodes
	createDev := func(path string, mode uint32, major, minor int) {
		dev := (major << 8) | minor
		_ = syscall.Mknod(path, mode|syscall.S_IFCHR, dev)
		_ = os.Chmod(path, os.FileMode(mode))
	}

	createDev("/dev/null", 0666, 1, 3)
	createDev("/dev/zero", 0666, 1, 5)
	createDev("/dev/full", 0666, 1, 7)
	createDev("/dev/random", 0666, 1, 8)
	createDev("/dev/urandom", 0666, 1, 9)
	createDev("/dev/tty", 0666, 5, 0)

	// Mount private isolated devpts instance
	if err := os.MkdirAll("/dev/pts", 0755); err == nil {
		_ = syscall.Mount("devpts", "/dev/pts", "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620")
		_ = os.Symlink("pts/ptmx", "/dev/ptmx")
	}

	if err := os.MkdirAll("/etc", 0755); err == nil {
		_ = os.WriteFile("/etc/resolv.conf", []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0644)
	}

	var sshdPath string
	for _, p := range []string{"/usr/sbin/sshd", "/usr/bin/sshd", "/sbin/sshd", "/bin/sshd"} {
		if _, err := os.Stat(p); err == nil {
			sshdPath = p
			break
		}
	}

	if sshdPath == "" {
		fmt.Println("[+] Package manager or sshd missing. Bootstrapping inside container...")
		installCmd := "export DEBIAN_FRONTEND=noninteractive && " +
			"if command -v apt-get >/dev/null 2>&1; then " +
			"  apt-get update && apt-get install -y --no-install-recommends openssh-server; " +
			"elif command -v apk >/dev/null 2>&1; then " +
			"  apk update && apk add openssh; " +
			"elif command -v dnf >/dev/null 2>&1; then " +
			"  dnf install -y openssh-server; " +
			"elif command -v yum >/dev/null 2>&1; then " +
			"  yum install -y openssh-server; " +
			"elif [ -f /etc/alpine-release ]; then " +
			"  apk add --allow-untrusted openssh || true; " +
			"elif [ -f /etc/debian_version ] || [ -f /etc/ubuntu-version ]; then " +
			"  apt-get update || true; apt-get install -y --no-install-recommends apt dpkg openssh-server || true; " +
			"fi"
		_ = exec.Command("/bin/sh", "-c", installCmd).Run()

		for _, p := range []string{"/usr/sbin/sshd", "/usr/bin/sshd", "/sbin/sshd", "/bin/sshd"} {
			if _, err := os.Stat(p); err == nil {
				sshdPath = p
				break
			}
		}
	}

	_ = os.MkdirAll("/var/empty", 0755)
	_ = exec.Command("/bin/sh", "-c", "id -u sshd >/dev/null 2>&1 || (groupadd -r sshd 2>/dev/null || addgroup -g 74 sshd 2>/dev/null; useradd -M -r -g sshd -c 'Privilege-separated SSH' -d /var/empty -s /sbin/nologin sshd 2>/dev/null || adduser -D -H -h /var/empty -s /sbin/nologin -G sshd sshd 2>/dev/null)").Run()

	_ = exec.Command("ssh-keygen", "-A").Run()

	if sshdPath == "" {
		fmt.Println("[-] Error: Failed to find or install sshd binary.")
	} else {
		sshCmd := exec.Command(sshdPath, "-D", "-e")
		sshCmd.Stderr = os.Stderr
		sshCmd.Stdout = os.Stdout
		go func() {
			if err := sshCmd.Run(); err != nil {
				fmt.Printf("[-] SSH Daemon exited: %v\n", err)
			}
		}()
	}

	fmt.Println("[+] Container started with SSH service running. Type 'exit' to stop.")

	shellPath := "/bin/bash"
	if _, err := os.Stat("/bin/bash"); err != nil {
		shellPath = "/bin/sh"
	}

	shell := exec.Command(shellPath, "-l")
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
	if err := runCmd("ip", "addr", "add", ContainerIP+"/24", "dev", vethName); err != nil {
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
		if data, err := os.ReadFile("/proc/net/dev"); err == nil && strings.Contains(string(data), name+":") {
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

	if hostInterface, err := getDefaultRouteInterface(); err == nil && hostInterface != "" {
		fmt.Printf("[+] Setting up NAT masquerade on interface: %s\n", hostInterface)
		_ = runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "10.100.0.0/24", "!", "-o", BridgeName, "-j", "MASQUERADE")
		_ = runCmd("iptables", "-A", "FORWARD", "-i", BridgeName, "-o", hostInterface, "-j", "ACCEPT")
		_ = runCmd("iptables", "-A", "FORWARD", "-i", hostInterface, "-o", BridgeName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")
	}

	return nil
}

func getDefaultRouteInterface() (string, error) {
	cmd := exec.Command("ip", "route", "show", "default")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	parts := strings.Fields(string(output))
	for i, p := range parts {
		if p == "dev" && i+1 < len(parts) {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("default interface not found")
}

func createVethPair(hostName, containerName string) error {
	_ = runCmd("ip", "link", "del", hostName)
	_ = runCmd("ip", "link", "del", containerName)
	return runCmd("ip", "link", "add", hostName, "type", "veth", "peer", "name", containerName)
}

func setupCgroup(cgroupDir string) error {
	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "memory.max"), []byte("104857600"), 0644); err != nil {
		_ = os.Remove(cgroupDir)
		return err
	}
	return nil
}

func addToCgroup(cgroupDir string, pid int) error {
	return os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), fmt.Appendf(nil, "%d", pid), 0644)
}

func extractOS(lowerDir, imagePath string) error {
	if _, err := os.Stat(imagePath); err != nil {
		return fmt.Errorf("image path not found: %w", err)
	}

	if err := os.MkdirAll(lowerDir, 0755); err != nil {
		return err
	}

	if strings.HasSuffix(strings.ToLower(imagePath), ".iso") {
		fmt.Printf("[+] Mounting and inspecting ISO '%v'...\n", filepath.Base(imagePath))

		tempMountDir, err := os.MkdirTemp("", "iso-mount-*")
		if err != nil {
			return fmt.Errorf("failed to create temp dir for ISO: %w", err)
		}
		defer os.RemoveAll(tempMountDir)

		if err := syscall.Mount(imagePath, tempMountDir, "iso9660", syscall.MS_RDONLY, "loop"); err != nil {
			if err := runCmd("mount", "-o", "loop,ro", imagePath, tempMountDir); err != nil {
				return fmt.Errorf("failed to mount ISO image: %w", err)
			}
		}

		defer func() {
			_ = syscall.Unmount(tempMountDir, 0)
		}()

		var squashfsPath string
		_ = filepath.WalkDir(tempMountDir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && (strings.HasSuffix(strings.ToLower(d.Name()), ".squashfs") || d.Name() == "filesystem.squashfs") {
				squashfsPath = path
				return filepath.SkipAll
			}
			return nil
		})

		if squashfsPath != "" {
			fmt.Printf("[+] Found SquashFS filesystem payload at '%s'. Mounting directly...\n", filepath.Base(squashfsPath))
			if err := syscall.Mount(squashfsPath, lowerDir, "squashfs", syscall.MS_RDONLY, "loop"); err != nil {
				if err := runCmd("mount", "-o", "loop,ro", squashfsPath, lowerDir); err != nil {
					return fmt.Errorf("failed to mount squashfs image directly: %w", err)
				}
			}
		} else {
			fmt.Println("[-] No squashfs found inside ISO. Copying raw ISO contents...")
			if err := copyDirContents(tempMountDir, lowerDir); err != nil {
				return fmt.Errorf("failed to copy ISO contents: %w", err)
			}
		}
	} else if strings.HasSuffix(strings.ToLower(imagePath), ".squashfs") {
		fmt.Printf("[+] Found SquashFS payload at '%s'. Mounting directly...\n", filepath.Base(imagePath))
		if err := syscall.Mount(imagePath, lowerDir, "squashfs", syscall.MS_RDONLY, "loop"); err != nil {
			if err := runCmd("mount", "-o", "loop,ro", imagePath, lowerDir); err != nil {
				return fmt.Errorf("failed to mount squashfs image directly: %w", err)
			}
		}
	} else {
		fmt.Printf("[+] Extracting Tarball OS '%v' to '%v'...\n", filepath.Base(imagePath), lowerDir)
		if err := extractTarGz(imagePath, lowerDir); err != nil {
			return fmt.Errorf("error extracting rootfs: %w", err)
		}
	}

	return nil
}

func copyDirContents(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		targetPath := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(targetPath, 0755)
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			linkDest, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(targetPath)
			return os.Symlink(linkDest, targetPath)
		}

		return copyFile(path, targetPath, info.Mode())
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	buf := make([]byte, 32*1024)
	for {
		n, readErr := srcFile.Read(buf)
		if n > 0 {
			if _, writeErr := dstFile.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}
	return nil
}

func generateClientID() string {
	return fmt.Sprintf("srv%d-funnybunny-cloud", 100000+mathrand.Intn(900000))
}

func generateRandomPassword(length int) string {
	bytes := make([]byte, length)
	_, _ = cryptorand.Read(bytes)
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for i, b := range bytes {
		bytes[i] = letters[b%byte(len(letters))]
	}
	return string(bytes)
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
			continue
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
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
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
