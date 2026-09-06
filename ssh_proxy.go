package main

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	"golang.org/x/crypto/ssh"
)

func handleClient(conn net.Conn, config *ssh.ServerConfig) {
	fmt.Printf("[+] New client connection from %v\n", conn.RemoteAddr().String())

	sConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sConn.Close()

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}

		go handleContainerSession(requests, channel, sConn.User())
	}
}

func handleContainerSession(in <-chan *ssh.Request, channel ssh.Channel, username string) {
	if username == "" || username == "root" || username == "lxdproxy" {
		fmt.Fprintf(channel, "[-] Invalid target user name: %s\n", username)
		channel.Close()
		return
	}

	containerName := username + "-sandbox"

	for req := range in {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			go startContainerShell(channel, containerName)
		case "exec":
			req.Reply(true, nil)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func startContainerShell(channel ssh.Channel, containerName string) {
	defer channel.Close()

	statusCmd := exec.Command("lxc", "info", containerName)
	output, err := statusCmd.CombinedOutput()

	if err != nil {
		fmt.Fprintf(channel, "[*] Container '%s' not found. Provisioning on first login...\n", containerName)

		createCmd := exec.Command("lxc", "launch", "ubuntu:22.04", containerName)
		createCmd.Stdout = channel
		createCmd.Stderr = channel
		if err := createCmd.Run(); err != nil {
			fmt.Fprintf(channel, "[-] Failed to create container: %v\n", err)
			return
		}

		_ = exec.Command("lxc", "config", "device", "add", containerName, "eth0", "nic", "nictype=bridged", "parent=lxdbr0").Run()
		_ = exec.Command("lxc", "restart", containerName).Run()
	} else if !strings.Contains(string(output), "Status: RUNNING") {
		fmt.Fprintf(channel, "[*] Starting container %s...\n", containerName)
		startCmd := exec.Command("lxc", "start", containerName)
		if err := startCmd.Run(); err != nil {
			fmt.Fprintf(channel, "[-] Failed to start container: %v\n", err)
			return
		}
	}

	cmd := exec.Command("lxc", "exec", "-t", "--env", "TERM=xterm-256color", containerName, "--", "/bin/bash")

	cmd.Stdin = channel
	cmd.Stdout = channel
	cmd.Stderr = channel

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(channel, "[-] Failed to start container session: %v\n", err)
	}
}
