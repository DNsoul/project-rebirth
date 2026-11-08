//go:build windows

// awgtunnel is the tunnel service of Rebirth for Windows: the same service the
// AmneziaWG client for Windows runs for a tunnel (amneziawg-windows/tunnel: the
// Wintun adapter, the firewall rules, the address, routes and names, the
// protocol), without the client's window, so that the program ships with its own
// tunnel and nobody has to install another one.
//
// Rebirth runs it as an administrator:
//
//	awgtunnel install <conf>     makes the service for a configuration and starts it
//	awgtunnel uninstall <name>   stops and removes it
//	awgtunnel handshake <name>   asks a running tunnel when its server last answered
//
// and the service manager runs it as "awgtunnel /tunnelservice <conf>". The
// service is called AmneziaWGTunnel$<name> and Wintun's DLL is loaded from the
// folder this program is in.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/ipc/namedpipe"
	"github.com/amnezia-vpn/amneziawg-windows/v3/conf"
	"github.com/amnezia-vpn/amneziawg-windows/v3/ringlogger"
	"github.com/amnezia-vpn/amneziawg-windows/v3/services"
	"github.com/amnezia-vpn/amneziawg-windows/v3/tunnel"
	"github.com/amnezia-vpn/amneziawg-windows/v3/version"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// uapiPipe is where a running tunnel answers questions about itself.
const uapiPipe = `\\.\pipe\ProtectedPrefix\Administrators\AmneziaWG\`

func main() {
	cmd, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(cmd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(cmd command) error {
	switch cmd.name {
	case "/tunnelservice":
		// The tunnel keeps its log (a ring buffer, log.bin) in the folder of its
		// configuration, which only administrators and the system can open. Left
		// to itself, the library would use the Data folder of the AmneziaWG client.
		conf.PresetRootDirectory(filepath.Dir(cmd.arg))
		return tunnel.Run(cmd.arg)
	case "install":
		return install(cmd.arg)
	case "uninstall":
		return uninstall(cmd.arg)
	case "handshake":
		seconds, err := handshake(cmd.arg)
		if err != nil {
			return err
		}
		fmt.Println(seconds)
		return nil
	case "log":
		return printLog(cmd.arg)
	case "version":
		fmt.Println("awgtunnel", version.UserAgent())
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd.name)
}

// install makes the tunnel service for a configuration and starts it. A service
// of the same name that is stopped (left over from before) is replaced; one that is
// running is not touched. A service that does not start is not left behind.
func install(confPath string) error {
	name, err := conf.NameFromPath(confPath)
	if err != nil {
		return err
	}
	serviceName, err := services.ServiceNameOfTunnel(name)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	if old, err := m.OpenService(serviceName); err == nil {
		status, qerr := old.Query()
		if qerr == nil && status.State != svc.Stopped {
			old.Close()
			return errors.New("the tunnel is installed and running")
		}
		derr := old.Delete()
		old.Close()
		if derr != nil && !errors.Is(derr, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return derr
		}
		waitGone(m, serviceName)
	}

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		ServiceType: windows.SERVICE_WIN32_OWN_PROCESS,
		// Started by hand, when Rebirth asks, never by the system: after a restart
		// nothing should send names to a server that may be gone.
		StartType:    mgr.StartManual,
		ErrorControl: mgr.ErrorNormal,
		Dependencies: []string{"Nsi", "TcpIp"},
		DisplayName:  "Rebirth tunnel: " + name,
		// The tunnel's firewall rules name the service, which they can only do
		// when it has an identifier of its own.
		SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}, "/tunnelservice", confPath)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		_ = s.Delete()
		return fmt.Errorf("the tunnel did not start: %w", err)
	}
	return nil
}

// uninstall stops the tunnel service called name and removes it, whichever
// program made it. No such service is not a failure.
func uninstall(name string) error {
	serviceName, err := services.ServiceNameOfTunnel(name)
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		return err
	}
	defer s.Close()

	if status, err := s.Query(); err == nil && status.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if status, err := s.Query(); err != nil || status.State == svc.Stopped {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return err
	}
	return nil
}

// waitGone waits until the service manager has really removed a service.
func waitGone(m *mgr.Mgr, serviceName string) {
	for i := 0; i < 50; i++ {
		s, err := m.OpenService(serviceName)
		if err != nil {
			return
		}
		s.Close()
		time.Sleep(200 * time.Millisecond)
	}
}

// handshake asks a running tunnel when its server last answered.
func handshake(name string) (int64, error) {
	if _, err := services.ServiceNameOfTunnel(name); err != nil {
		return 0, err
	}
	c, err := (&namedpipe.DialConfig{}).DialTimeout(uapiPipe+name, 3*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("get=1\n\n")); err != nil {
		return 0, err
	}
	return newestHandshake(c)
}

// printLog prints what the tunnel service has written to its log in dir.
func printLog(dir string) error {
	rl, err := ringlogger.NewRinglogger(filepath.Join(dir, "log.bin"), "log")
	if err != nil {
		return err
	}
	defer rl.Close()
	_, err = rl.WriteTo(os.Stdout)
	return err
}
