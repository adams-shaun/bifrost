package fixtures

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// portForwardService starts `kubectl port-forward` for a Service in the
// background and returns the local port it bound to plus a stop function.
//
// Passing localPort=0 binds an ephemeral port chosen by the OS (we discover it
// from kubectl's stdout: "Forwarding from 127.0.0.1:<port> -> <target>").
func portForwardService(ctx context.Context, c *KindCluster, namespace, target string, targetPort int) (int, func(), error) {
	localPort, err := freePort()
	if err != nil {
		return 0, nil, fmt.Errorf("alloc port: %w", err)
	}

	cmd := exec.Command("kubectl",
		"port-forward",
		"-n", namespace,
		target,
		fmt.Sprintf("%d:%d", localPort, targetPort),
		"--address=127.0.0.1",
	)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)
	// Run in its own process group so we can kill the whole tree cleanly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, nil, err
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("kubectl port-forward: %w", err)
	}

	ready := make(chan int, 1)
	failure := make(chan error, 2)

	go scanPortForward(stdout, ready, failure)
	go scanPortForward(stderr, ready, failure)

	select {
	case p := <-ready:
		if p != 0 {
			localPort = p
		}
	case err := <-failure:
		_ = killProcessGroup(cmd)
		return 0, nil, fmt.Errorf("port-forward: %w", err)
	case <-time.After(15 * time.Second):
		_ = killProcessGroup(cmd)
		return 0, nil, fmt.Errorf("timed out waiting for port-forward ready")
	case <-ctx.Done():
		_ = killProcessGroup(cmd)
		return 0, nil, ctx.Err()
	}

	stop := func() {
		_ = killProcessGroup(cmd)
		_ = cmd.Wait()
	}
	return localPort, stop, nil
}

var portForwardRE = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

func scanPortForward(r interface {
	Read(p []byte) (n int, err error)
}, ready chan<- int, failure chan<- error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if m := portForwardRE.FindStringSubmatch(line); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil {
				select {
				case ready <- p:
				default:
				}
			}
		}
		if strings.Contains(line, "error") || strings.Contains(line, "unable to") {
			select {
			case failure <- fmt.Errorf("%s", line):
			default:
			}
		}
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return cmd.Process.Kill()
	}
	return syscall.Kill(-pgid, syscall.SIGTERM)
}
