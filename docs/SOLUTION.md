To automatically drop users into an LXD container running a bash shell whenever they log in via SSH, you can use OpenSSH's `ForceCommand` combined with the `lxc exec` command.

Here is how to set this up cleanly:

### Step 1: Create a Wrapper Script

Create a script on the host machine (e.g., `/usr/local/bin/ssh-to-container.sh`) that determines which container the user should enter, checks if it's running, starts it if necessary, and attaches a bash session.

```bash
#!/usr/bin/env golang
//usr/bin/env go run "$0" "$@"; exit

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	// Map the incoming SSH user to a specific container name
	// (Here we use their Linux username, but you can customize this logic)
	sshUser := os.Getenv("USER")
	if sshUser == "" || sshUser == "root" {
		fmt.Println("Invalid user context for container routing.")
		os.Exit(1)
	}

	containerName := "container-" + sshUser

	// Check if the container is running, start it if it's stopped
	statusCmd := exec.Command("lxc", "info", containerName)
	output, err := statusCmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Container %s does not exist or LXD error.\n", containerName)
		os.Exit(1)
	}

	if !strings.Contains(string(output), "Status: RUNNING") {
		fmt.Printf("Starting container %s...\n", containerName)
		startCmd := exec.Command("lxc", "start", containerName)
		if err := startCmd.Run(); err != nil {
			fmt.Printf("Failed to start container: %v\n", err)
			os.Exit(1)
		}
	}

	// Exec into the container with a pseudo-terminal (pty) allocated
	// syscall.Exec replaces the current process with lxc
	args := []string{"exec", "--env", "TERM=xterm-256color", containerName, "--", "su", "-", sshUser}

	// Fallback to root bash if the specific user doesn't exist inside the container yet
	cmdPath, err := exec.LookPath("lxc")
	if err != nil {
		panic(err)
	}

	// Execute lxc exec
	fullArgs := append([]string{"lxc"}, args...)
	err = syscallExec(cmdPath, fullArgs, os.Environ())
	if err != nil {
		fmt.Printf("Error spawning session: %v\n", err)
	}
}

func syscallExec(argv0 string, argv []string, envv []string) error {
	return syscall.Exec(argv0, argv, envv)
}

```

*Note: Make sure to make the script executable:*

```bash
sudo chmod +x /usr/local/bin/ssh-to-container.sh

```

*(Remember that per your preferences, Go is used here instead of Python).*

---

### Step 2: Configure SSH Daemon (`sshd_config`)

To intercept user logins and route them through your container script, update your SSH configuration.

Open `/etc/ssh/sshd_config` and add a match block for the users or group you want to target:

```text
Match Group container-users
    ForceCommand /usr/local/bin/ssh-to-container.sh
    AllowTcpForwarding no
    X11Forwarding no

```

Restart the SSH service to apply changes:

```bash
sudo systemctl restart ssh

```

### Step 3: Ensure Proper Container Provisioning

Make sure that for every SSH user in the `container-users` group, a matching LXD container exists (e.g., if user is `bob`, container should be named `bob-container`). Inside that container, ensure the matching user account and a `bash` shell are present so they land cleanly in their environment when `su` is invoked.

To prevent users like Bob from having a standard interactive account on the host while still allowing them to log in via SSH directly into their container, you can use a **single shared system user** combined with **SSH forced key commands**.

In this setup, Bob doesn't get a host shell or a traditional host account; their SSH key is strictly mapped to spin up and drop them into their specific LXD container.

---

### Step 1: Create a Dedicated System User

Create a single non-login system user on the host to handle all incoming container SSH traffic:

```bash
sudo useradd -r -s /bin/false lxdproxy
sudo mkdir -p /home/lxdproxy/.ssh
sudo chmod 700 /home/lxdproxy/.ssh

```

---

### Step 2: Create the Go Wrapper Script

Create a script at `/usr/local/bin/ssh-to-container.sh` that accepts the container name as an argument:

```go
//usr/bin/env go run "$0" "$@"; exit

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Error: No container specified.")
		os.Exit(1)
	}

	containerName := os.Args[1]

	statusCmd := exec.Command("lxc", "info", containerName)
	output, err := statusCmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Container %s does not exist.\n", containerName)
		os.Exit(1)
	}

	if !strings.Contains(string(output), "Status: RUNNING") {
		startCmd := exec.Command("lxc", "start", containerName)
		if err := startCmd.Run(); err != nil {
			fmt.Printf("Failed to start container: %v\n", err)
			os.Exit(1)
		}
	}

	cmdPath, err := exec.LookPath("lxc")
	if err != nil {
		panic(err)
	}

	args := []string{"lxc", "exec", "--env", "TERM=xterm-256color", containerName, "--", "su", "-", containerName}
	err = syscall.Exec(cmdPath, args, os.Environ())
	if err != nil {
		fmt.Printf("Error spawning session: %v\n", err)
	}
}

```

Make the script executable:

```bash
sudo chmod +x /usr/local/bin/ssh-to-container.sh

```

---

### Step 3: Map User Keys in `authorized_keys`

In `/home/lxdproxy/.ssh/authorized_keys`, prepend each user's public key with a `command="..."` directive that explicitly passes their container name to your script.

Example `authorized_keys` file:

```text
command="/usr/local/bin/ssh-to-container.sh bob-container" ssh-ed25519 AAAAC3NzaC1... bob@laptop
command="/usr/local/bin/ssh-to-container.sh alice-container" ssh-ed25519 AAAAC3NzaC1... alice@work

```

Set correct permissions on the authorized keys file:

```bash
sudo chmod 600 /home/lxdproxy/.ssh/authorized_keys
sudo chown -R lxdproxy:lxdproxy /home/lxdproxy

```

---

### How it Works

When Bob runs `ssh lxdproxy@your-host-ip`, OpenSSH authenticates him using his public key. Because of the `command="..."` prefix, OpenSSH **bypasses any shell**, ignores standard host login options, and immediately executes the Go script targeting `bob-container`. Bob has no password, no home directory on the host, and cannot escape to a host shell.