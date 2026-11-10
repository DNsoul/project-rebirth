//go:build android

package main

/*
#cgo LDFLAGS: -llog
#include <android/log.h>
#include <stdlib.h>

static void awg_log(int priority, const char *message) {
	__android_log_write(priority, "awg", message);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"awgrotator/internal/awglib"
)

// Android's log priorities.
const (
	logDebug = 3
	logError = 6
)

func logcat(priority int, message string) {
	cs := C.CString(message)
	defer C.free(unsafe.Pointer(cs))
	C.awg_log(C.int(priority), cs)
}

var (
	mu      sync.Mutex
	nextID  int32 = 1
	tunnels       = map[int32]*awglib.Tunnel{}
	lastErr string
)

func fail(code C.int, err error) C.int {
	mu.Lock()
	lastErr = err.Error()
	mu.Unlock()
	logcat(logError, err.Error())
	return code
}

// awgTurnOn starts a client from a configuration file's text on the TUN
// descriptor the system handed over, which the library then owns. It returns
// a handle (1 or more), or a negative number, and awgLastError says why.
//
//export awgTurnOn
func awgTurnOn(conf *C.char, tunFd C.int) C.int {
	settings, err := awglib.ParseConf(C.GoString(conf))
	if err != nil {
		// The descriptor was handed over, so it is closed here: left open, the
		// interface would stay up with nothing serving it.
		_ = unix.Close(int(tunFd))
		return fail(-1, fmt.Errorf("configuration: %w", err))
	}
	// Not the monitored kind: the monitor asks the kernel about network
	// interfaces over netlink, which Android refuses to apps.
	dev, _, err := tun.CreateUnmonitoredTUNFromFD(int(tunFd))
	if err != nil {
		_ = unix.Close(int(tunFd))
		return fail(-2, fmt.Errorf("tun device: %w", err))
	}
	t, err := awglib.Up(dev, settings, &awglib.Logger{
		Verbosef: func(f string, a ...any) { logcat(logDebug, fmt.Sprintf(f, a...)) },
		Errorf:   func(f string, a ...any) { logcat(logError, fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return fail(-3, err)
	}

	mu.Lock()
	defer mu.Unlock()
	id := nextID
	nextID++
	tunnels[id] = t
	return C.int(id)
}

// awgTurnOff stops a client and closes its TUN descriptor. A handle that is
// not running is ignored.
//
//export awgTurnOff
func awgTurnOff(handle C.int) {
	mu.Lock()
	t := tunnels[int32(handle)]
	delete(tunnels, int32(handle))
	mu.Unlock()
	if t != nil {
		t.Close()
	}
}

// awgTurnOffAll stops every client this library is running and returns how many
// there were. The app calls it before it opens a tunnel it has no record of
// owning: a tunnel that outlived the program that started it has nobody to stop
// it, and goes on talking to the server with the same key as the new one, which
// makes the server answer to whichever of the two spoke last.
//
//export awgTurnOffAll
func awgTurnOffAll() C.int {
	mu.Lock()
	all := make([]*awglib.Tunnel, 0, len(tunnels))
	for id, t := range tunnels {
		all = append(all, t)
		delete(tunnels, id)
	}
	mu.Unlock()
	for _, t := range all {
		t.Close()
	}
	return C.int(len(all))
}

// awgRebind is for a change of network: it opens the client's sockets afresh
// and starts a new handshake at once. It returns 0, -1 for a handle that is not
// running, or -2 when the sockets could not be opened (awgLastError says why).
// The sockets have new descriptors afterwards (awgSocketV4, awgSocketV6).
//
//export awgRebind
func awgRebind(handle C.int) C.int {
	t := lookup(handle)
	if t == nil {
		return -1
	}
	if err := t.Rebind(); err != nil {
		return fail(-2, fmt.Errorf("network changed: %w", err))
	}
	return 0
}

func lookup(handle C.int) *awglib.Tunnel {
	mu.Lock()
	defer mu.Unlock()
	return tunnels[int32(handle)]
}

// awgLatestHandshake is the Unix time of the last handshake, 0 when there has
// not been one, and -1 for a handle that is not running.
//
//export awgLatestHandshake
func awgLatestHandshake(handle C.int) C.longlong {
	t := lookup(handle)
	if t == nil {
		return -1
	}
	if when, ok := t.LatestHandshake(); ok {
		return C.longlong(when.Unix())
	}
	return 0
}

// awgSocketV4 and awgSocketV6 are the descriptors of the client's UDP sockets,
// or -1. The system has to be told to keep them out of the tunnel.
//
//export awgSocketV4
func awgSocketV4(handle C.int) C.int {
	t := lookup(handle)
	if t == nil {
		return -1
	}
	v4, _ := t.SocketFDs()
	return C.int(v4)
}

//export awgSocketV6
func awgSocketV6(handle C.int) C.int {
	t := lookup(handle)
	if t == nil {
		return -1
	}
	_, v6 := t.SocketFDs()
	return C.int(v6)
}

// awgLastError is the reason the last call failed. The caller frees the text
// with awgFree.
//
//export awgLastError
func awgLastError() *C.char {
	mu.Lock()
	defer mu.Unlock()
	return C.CString(lastErr)
}

// awgFree releases text this library returned.
//
//export awgFree
func awgFree(p *C.char) { C.free(unsafe.Pointer(p)) }

// awgVersion says which library this is, so the app can tell it loaded the one
// it was built with.
//
//export awgVersion
func awgVersion() *C.char { return C.CString("awglib 2") }
