package supervisor

import (
	"fmt"
	"net"
	"os"
	"os/exec"
)

const workerListenersEnv = "EMBER_SUPERVISOR_LISTENERS"
const workerHTTPFD, workerTunnelFD = 3, 4

func startWithWorkerListeners(cmd *exec.Cmd) (string, string, error) {
	var addresses []string
	for _, name := range []string{"http", "grpc"} {
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return "", "", fmt.Errorf("listen for worker %s: %w", name, err)
		}
		defer listener.Close()
		file, err := listener.File()
		if err != nil {
			return "", "", fmt.Errorf("duplicate worker %s listener: %w", name, err)
		}
		defer file.Close()
		address := listener.Addr().String()
		addresses = append(addresses, address)
		cmd.Args = append(cmd.Args, "--"+name, address)
		cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	}
	cmd.Env = append(cmd.Environ(), workerListenersEnv+"=1")
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	return addresses[0], addresses[1], nil
}

func AdoptWorkerListeners(httpAddress, tunnelAddress string) (net.Listener, net.Listener, error) {
	if os.Getenv(workerListenersEnv) != "1" {
		return nil, nil, nil
	}
	_ = os.Unsetenv(workerListenersEnv)
	httpFile := os.NewFile(workerHTTPFD, "worker-http")
	defer httpFile.Close()
	tunnelFile := os.NewFile(workerTunnelFD, "worker-grpc")
	defer tunnelFile.Close()
	adopt := func(file *os.File, address string) (net.Listener, error) {
		listener, err := net.FileListener(file)
		if err != nil {
			return nil, fmt.Errorf("adopt %s listener: %w", file.Name(), err)
		}
		tcp, ok := listener.Addr().(*net.TCPAddr)
		if !ok || !tcp.IP.IsLoopback() || tcp.String() != address {
			listener.Close()
			return nil, fmt.Errorf("unexpected %s listener address %s (want %s)", file.Name(), listener.Addr(), address)
		}
		return listener, nil
	}
	httpListener, err := adopt(httpFile, httpAddress)
	if err != nil {
		return nil, nil, err
	}
	tunnelListener, err := adopt(tunnelFile, tunnelAddress)
	if err != nil {
		httpListener.Close()
		return nil, nil, err
	}
	return httpListener, tunnelListener, nil
}
