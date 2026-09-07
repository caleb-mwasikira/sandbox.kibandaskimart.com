package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func main() {
	db = initDB()

	addUserCmd := flag.NewFlagSet("add-user", flag.ExitOnError)
	var (
		imageFlag   string
		memoryFlag  string
		storageFlag string
		cpusFlag    string
	)
	addUserCmd.StringVar(&imageFlag, "image", "ubuntu:22.04", "OS image for container")
	addUserCmd.StringVar(&memoryFlag, "memory", "", "Memory limit (e.g., 512MB, 2GB)")
	addUserCmd.StringVar(&storageFlag, "storage", "", "Storage size limit (e.g., 10GB, 50GB)")
	addUserCmd.StringVar(&cpusFlag, "cpus", "", "CPU limit (e.g., 2, 0.5)")

	updatePassCmd := flag.NewFlagSet("update-password", flag.ExitOnError)
	updateEmailCmd := flag.NewFlagSet("update-email", flag.ExitOnError)

	updateLimitsCmd := flag.NewFlagSet("update-limits", flag.ExitOnError)
	var (
		updateMemoryFlag  string
		updateStorageFlag string
		updateCpusFlag    string
	)
	updateLimitsCmd.StringVar(&updateMemoryFlag, "memory", "", "Memory limit (e.g., 512MB, 2GB)")
	updateLimitsCmd.StringVar(&updateStorageFlag, "storage", "", "Storage size limit (e.g., 10GB, 50GB)")
	updateLimitsCmd.StringVar(&updateCpusFlag, "cpus", "", "CPU limit (e.g., 2, 0.5)")

	deleteUserCmd := flag.NewFlagSet("delete-user", flag.ExitOnError)
	listUsersCmd := flag.NewFlagSet("list-users", flag.ExitOnError)
	listImagesCmd := flag.NewFlagSet("list-images", flag.ExitOnError)

	var (
		host     string
		sshPort  uint
		httpPort uint
	)
	serverCmd := flag.NewFlagSet("start-server", flag.ExitOnError)
	serverCmd.StringVar(&host, "host", "0.0.0.0", "Host to run SSH server")
	serverCmd.UintVar(&sshPort, "port", 2022, "Port to run SSH server")
	serverCmd.UintVar(&httpPort, "http-port", 8080, "Port to run HTTP user management server")

	if len(os.Args) < 2 {
		printUsage("Undefined command")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "add-user":
		addUserCmd.Parse(os.Args[2:])
		args := addUserCmd.Args()
		if len(args) < 1 {
			log.Fatal("[-] Usage: go run . add-user [flags] <username> [password]")
		}
		username := args[0]
		var password string
		if len(args) >= 2 {
			password = args[1]
		} else {
			password = promptPassword("Enter password: ")
		}

		var email string
		fmt.Print("Enter email (optional): ")
		fmt.Scanln(&email)

		err := store.AddUser(User{Username: username, Password: password, Email: email})
		if err != nil {
			log.Fatalf("[-] Failed to add user to database: %v", err)
		}

		fmt.Printf("[*] Provisioning container '%s-sandbox' with image '%s'...\n", username, imageFlag)
		if err := provisionUserContainer(username, imageFlag, memoryFlag, storageFlag, cpusFlag); err != nil {
			log.Fatalf("[-] Failed to create container for user: %v", err)
		}

		fmt.Printf("[+] User '%s' and container '%s-sandbox' created successfully.\n", username, username)

	case "update-password":
		updatePassCmd.Parse(os.Args[2:])
		args := updatePassCmd.Args()
		if len(args) < 1 {
			log.Fatal("[-] Usage: go run . update-password <username> [password]")
		}
		username := args[0]
		var password string
		if len(args) >= 2 {
			password = args[1]
		} else {
			password = promptPassword("Enter new password: ")
		}

		err := store.UpdateUser(username, "", password)
		if err != nil {
			log.Fatalf("[-] Failed to update password: %v", err)
		}
		fmt.Printf("[+] Password updated successfully for '%s'.\n", username)

	case "update-email":
		updateEmailCmd.Parse(os.Args[2:])
		args := updateEmailCmd.Args()
		if len(args) < 1 {
			log.Fatal("[-] Usage: go run . update-email <username> [new-email]")
		}
		username := args[0]
		var email string
		if len(args) >= 2 {
			email = args[1]
		} else {
			fmt.Print("Enter new email: ")
			fmt.Scanln(&email)
		}

		err := store.UpdateUser(username, email, "")
		if err != nil {
			log.Fatalf("[-] Failed to update email: %v", err)
		}
		fmt.Printf("[+] Email updated successfully for '%s'.\n", username)

	case "update-limits":
		updateLimitsCmd.Parse(os.Args[2:])
		args := updateLimitsCmd.Args()
		if len(args) < 1 {
			log.Fatal("[-] Usage: go run . update-limits [flags] <username>")
		}
		username := args[0]
		containerName := getContainerName(username)

		statusCmd := exec.Command("lxc", "info", containerName)
		if err := statusCmd.Run(); err != nil {
			log.Fatalf("[-] Container '%s' does not exist.", containerName)
		}

		updated := false
		if updateMemoryFlag != "" {
			cmd := exec.Command("lxc", "config", "set", containerName, "limits.memory", updateMemoryFlag)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("[-] Failed to update memory limit: %s", strings.TrimSpace(string(output)))
			} else {
				fmt.Printf("[+] Memory limit updated to %s\n", updateMemoryFlag)
				updated = true
			}
		}
		if updateStorageFlag != "" {
			cmd := exec.Command("lxc", "config", "device", "set", containerName, "root", "size", updateStorageFlag)
			output, err := cmd.CombinedOutput()
			if err != nil {
				cmdAdd := exec.Command("lxc", "config", "device", "add", containerName, "root", "disk", "pool=default", fmt.Sprintf("size=%s", updateStorageFlag))
				output, err = cmdAdd.CombinedOutput()
			}

			if err != nil {
				log.Printf("[-] Failed to update storage limit: %s", strings.TrimSpace(string(output)))
			} else {
				fmt.Printf("[+] Storage limit updated to %s\n", updateStorageFlag)
				updated = true
			}
		}
		if updateCpusFlag != "" {
			cmd := exec.Command("lxc", "config", "set", containerName, "limits.cpu", updateCpusFlag)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("[-] Failed to update CPU limit: %s", strings.TrimSpace(string(output)))
			} else {
				fmt.Printf("[+] CPU limit updated to %s\n", updateCpusFlag)
				updated = true
			}
		}

		if !updated {
			fmt.Println("[-] No limits specified to update. Use --memory, --storage, or --cpus.")
		}

	case "delete-user":
		deleteUserCmd.Parse(os.Args[2:])
		args := deleteUserCmd.Args()
		if len(args) < 1 {
			log.Fatal("[-] Usage: go run . delete-user <username>")
		}
		username := args[0]
		containerName := getContainerName(username)

		// Stop and delete container instance
		_ = exec.Command("lxc", "stop", containerName, "--force").Run()
		if err := exec.Command("lxc", "delete", containerName).Run(); err != nil {
			log.Printf("[-] Warning: Failed to delete container '%s': %v", containerName, err)
		}

		err := store.DeleteUser(username)
		if err != nil {
			log.Fatalf("[-] Failed to delete user from DB: %v", err)
		}
		fmt.Printf("[+] User '%s' and container '%s' deleted successfully.\n", username, containerName)

	case "list-users":
		listUsersCmd.Parse(os.Args[2:])
		users, err := store.ListUsers()
		if err != nil {
			log.Fatalf("[-] Failed to list users: %v", err)
		}

		if len(users) == 0 {
			fmt.Println("[-] No container users found.")
			return
		}

		fmt.Println("\n----------------------------------------")
		fmt.Println("Registered Container Users:")
		fmt.Println("----------------------------------------")
		for _, u := range users {
			emailDisplay := u.Email
			if emailDisplay == "" {
				emailDisplay = "N/A"
			}
			fmt.Printf("Username:  %s\n", u.Username)
			fmt.Printf("Container: %s-sandbox\n", u.Username)
			fmt.Printf("Email:     %s\n", emailDisplay)
			fmt.Println("----------------------------------------")
		}

	case "list-images":
		listImagesCmd.Parse(os.Args[2:])
		args := listImagesCmd.Args()

		lxcArgs := []string{"image", "list", "images:"}
		if len(args) > 0 {
			lxcArgs = append(lxcArgs, args[0])
		}

		lxcCmd := exec.Command("lxc", lxcArgs...)
		pagerCmd := exec.Command("less", "-R")

		pipe, err := lxcCmd.StdoutPipe()
		if err != nil {
			log.Fatalf("[-] Failed to create pipe: %v", err)
		}
		pagerCmd.Stdin = pipe
		pagerCmd.Stdout = os.Stdout
		pagerCmd.Stderr = os.Stderr

		if err := pagerCmd.Start(); err != nil {
			lxcCmd.Stdout = os.Stdout
			lxcCmd.Stderr = os.Stderr
			_ = lxcCmd.Run()
			return
		}

		if err := lxcCmd.Run(); err != nil {
			log.Fatalf("[-] Failed to list images: %v", err)
		}

		_ = pagerCmd.Wait()

	case "start-server":
		serverCmd.Parse(os.Args[2:])
		runServers(host, sshPort, httpPort)

	default:
		printUsage("Unknown command")
		os.Exit(1)
	}
}

func getContainerName(username string) string {
	return username + "-sandbox"
}

func provisionUserContainer(username, imageName, memory, storage, cpus string) error {
	containerName := getContainerName(username)
	statusCmd := exec.Command("lxc", "info", containerName)
	if err := statusCmd.Run(); err == nil {
		fmt.Printf("[*] Container '%s' already exists.\n", containerName)
		return nil
	}

	createCmd := exec.Command("lxc", "launch", imageName, containerName)
	createCmd.Stdout = os.Stdout
	createCmd.Stderr = os.Stderr
	if err := createCmd.Run(); err != nil {
		return err
	}

	_ = exec.Command("lxc", "config", "device", "add", containerName, "eth0", "nic", "nictype=bridged", "parent=lxdbr0").Run()

	if memory != "" {
		cmd := exec.Command("lxc", "config", "set", containerName, "limits.memory", memory)
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("[-] Warning: Failed to set memory limit: %s\n", strings.TrimSpace(string(output)))
		}
	}
	if storage != "" {
		cmd := exec.Command("lxc", "config", "device", "add", containerName, "root", "disk", "pool=default", fmt.Sprintf("size=%s", storage))
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("[-] Warning: Failed to set storage limit: %s\n", strings.TrimSpace(string(output)))
		}
	}
	if cpus != "" {
		cmd := exec.Command("lxc", "config", "set", containerName, "limits.cpu", cpus)
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("[-] Warning: Failed to set CPU limit: %s\n", strings.TrimSpace(string(output)))
		}
	}

	_ = exec.Command("lxc", "restart", containerName).Run()

	return nil
}

func promptPassword(prompt string) string {
	fmt.Print(prompt)
	bytePassword, err := term.ReadPassword(int(syscall.Stdin))
	if err != nil {
		log.Fatalf("[-] Failed to read password: %v", err)
	}
	fmt.Println()
	return string(bytePassword)
}

func printUsage(errMsg string) {
	fmt.Printf("%v. Usage:\n", errMsg)
	fmt.Println("  go run . add-user [--image <image>] [--memory <limit>] [--storage <size>] [--cpus <limit>] <username> [password]")
	fmt.Println("  go run . update-password <username> [password]")
	fmt.Println("  go run . update-email <username> [new-email]")
	fmt.Println("  go run . update-limits [--memory <limit>] [--storage <size>] [--cpus <limit>] <username>")
	fmt.Println("  go run . delete-user <username>")
	fmt.Println("  go run . list-users")
	fmt.Println("  go run . list-images [search-term]")
	fmt.Println("  go run . start-server [--host <host>] [--port <port>] [--http-port <http-port>]")
}

func runServers(host string, port uint, httpPort uint) {
	keyPath := "./ssh-keys/id_rsa"
	config, err := initSSHConfig(keyPath)
	if err != nil {
		fmt.Printf("[-] Error initializing ssh config: %v\n", err)
		os.Exit(1)
	}

	address := fmt.Sprintf("%v:%v", host, port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		fmt.Printf("[-] Failed to listen on port %v: %v\n", port, err)
		os.Exit(1)
	}
	defer listener.Close()

	fmt.Printf("[+] Starting SSH container proxy server on %v...\n", address)

	go startHTTPServer(host, httpPort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("[-] Error accepting client connection: %v\n", err)
			continue
		}

		go handleClient(conn, config)
	}
}

func generatePrivateKey(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	fmt.Println("[*] Generating new RSA private key...")
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	privateKeyPEM := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := pem.Encode(file, privateKeyPEM); err != nil {
		return err
	}

	fmt.Println("[+] Host private key generated successfully.")
	return nil
}

func initSSHConfig(keyPath string) (*ssh.ServerConfig, error) {
	if err := generatePrivateKey(keyPath); err != nil {
		return nil, fmt.Errorf("error generating private key: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if store.ValidateUser(conn.User(), string(password)) {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected for %q", conn.User())
		},
	}

	privateBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("error loading private key: %v", err)
	}

	privateKey, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		return nil, fmt.Errorf("error parsing private key: %v", err)
	}

	config.AddHostKey(privateKey)
	return config, nil
}

func startHTTPServer(host string, port uint) {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Hello world :)"))
	})
	r.Post("/users", AddUserHandler)
	r.Put("/users/{username}", UpdateUserHandler)
	r.Delete("/users/{username}", DeleteUserHandler)

	address := fmt.Sprintf("%v:%v", host, port)
	fmt.Printf("[+] Starting HTTP server on %s...\n", address)

	if err := http.ListenAndServe(address, r); err != nil {
		fmt.Printf("[-] Error running HTTP server: %v\n", err)
		os.Exit(1)
	}
}