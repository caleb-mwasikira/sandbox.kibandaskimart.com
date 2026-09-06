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
	"path/filepath"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func main() {
	db = initDB()

	addUserCmd := flag.NewFlagSet("add-user", flag.ExitOnError)
	updatePassCmd := flag.NewFlagSet("update-password", flag.ExitOnError)
	updateEmailCmd := flag.NewFlagSet("update-email", flag.ExitOnError)
	deleteUserCmd := flag.NewFlagSet("delete-user", flag.ExitOnError)

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
			log.Fatal("[-] Usage: go run . add-user <username> [password]")
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
			log.Fatalf("[-] Failed to add user: %v", err)
		}
		fmt.Printf("[+] User '%s' added successfully.\n", username)

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

	case "delete-user":
		deleteUserCmd.Parse(os.Args[2:])
		args := deleteUserCmd.Args()
		if len(args) < 1 {
			log.Fatal("[-] Usage: go run . delete-user <username>")
		}
		username := args[0]

		err := store.DeleteUser(username)
		if err != nil {
			log.Fatalf("[-] Failed to delete user: %v", err)
		}
		fmt.Printf("[+] User '%s' deleted successfully.\n", username)

	case "start-server":
		serverCmd.Parse(os.Args[2:])
		runServers(host, sshPort, httpPort)

	default:
		printUsage("Unknown command")
		os.Exit(1)
	}
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
	fmt.Println("  go run . add-user <username> [password]")
	fmt.Println("  go run . update-password <username> [password]")
	fmt.Println("  go run . update-email <username> [new-email]")
	fmt.Println("  go run . delete-user <username>")
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
