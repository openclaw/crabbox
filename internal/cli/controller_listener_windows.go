//go:build windows

package cli

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsAFInet                   = 2
	windowsTCPTableOwnerPIDListener = 3
	windowsTCPStateListen           = 2
	windowsTCPRowOwnerPIDSize       = 24
	maxWindowsTCPTableSize          = 16 << 20
)

var windowsGetExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

func controllerListenerOwnershipSupported() bool { return true }

func controllerVerifyDaemonOwnedListener(port string, expectedPID int) error {
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 || expectedPID <= 0 {
		return fmt.Errorf("invalid Windows listener ownership request")
	}
	table, err := controllerWindowsTCPListenerTable()
	if err != nil {
		return err
	}
	if len(table) < 4 {
		return fmt.Errorf("Windows TCP listener table is truncated")
	}
	count := int(binary.LittleEndian.Uint32(table[:4]))
	if count < 0 || count > (len(table)-4)/windowsTCPRowOwnerPIDSize {
		return fmt.Errorf("Windows TCP listener table row count is invalid")
	}
	found := false
	for index := 0; index < count; index++ {
		offset := 4 + index*windowsTCPRowOwnerPIDSize
		row := table[offset : offset+windowsTCPRowOwnerPIDSize]
		state := binary.LittleEndian.Uint32(row[0:4])
		address := binary.LittleEndian.Uint32(row[4:8])
		encodedPort := binary.LittleEndian.Uint32(row[8:12])
		ownerPID := int(binary.LittleEndian.Uint32(row[20:24]))
		listenerPort := int((uint16(encodedPort)<<8)&0xff00 | uint16(encodedPort)>>8)
		if state != windowsTCPStateListen || address != 0x0100007f || listenerPort != portNumber {
			continue
		}
		found = true
		if ownerPID != expectedPID {
			return fmt.Errorf("loopback listener is owned by pid %d, not tracked SSH pid %d", ownerPID, expectedPID)
		}
	}
	if !found {
		return fmt.Errorf("no tracked IPv4 loopback listener found on port %d", portNumber)
	}
	return nil
}

func controllerWindowsTCPListenerTable() ([]byte, error) {
	if err := windowsGetExtendedTCPTable.Find(); err != nil {
		return nil, fmt.Errorf("load GetExtendedTcpTable: %w", err)
	}
	var size uint32
	code, _, _ := windowsGetExtendedTCPTable.Call(
		0, uintptr(unsafe.Pointer(&size)), 0,
		windowsAFInet, windowsTCPTableOwnerPIDListener, 0,
	)
	if code != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || size < 4 || size > maxWindowsTCPTableSize {
		if code == 0 {
			return nil, fmt.Errorf("GetExtendedTcpTable returned invalid size %d", size)
		}
		return nil, fmt.Errorf("size Windows TCP listener table: %w", syscall.Errno(code))
	}
	for attempt := 0; attempt < 3; attempt++ {
		table := make([]byte, size)
		code, _, _ = windowsGetExtendedTCPTable.Call(
			uintptr(unsafe.Pointer(&table[0])), uintptr(unsafe.Pointer(&size)), 0,
			windowsAFInet, windowsTCPTableOwnerPIDListener, 0,
		)
		if code == 0 {
			return table[:size], nil
		}
		if code != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || size < 4 || size > maxWindowsTCPTableSize {
			return nil, fmt.Errorf("read Windows TCP listener table: %w", syscall.Errno(code))
		}
	}
	return nil, fmt.Errorf("Windows TCP listener table kept changing")
}
