package srvrmgr

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/barkadron/siebel_exporter/shell"
	"github.com/prometheus/common/log"
)

// Status is an enumeration of srvrmgr statuses that represent a simple value.
type Status int

// Possible values for the Status enum.
const (
	_ Status = iota
	Disconnected
	Connecting
	Disconnecting
	Connected
	ConnectionError
)

type safeStatus struct {
	sync.RWMutex
	value Status
}

func (ss *safeStatus) Get() Status {
	ss.RLock()
	defer ss.RUnlock()

	return ss.value
}

func (ss *safeStatus) Set(value Status) error {
	ss.Lock()
	defer ss.Unlock()

	log.Debugf("Set srvrmgr status from '%v' to '%v'.", ss.value, value)
	ss.value = value
	return nil
}

type safeServerName struct {
	sync.RWMutex
	value string
}

func (ssn *safeServerName) Get() string {
	ssn.RLock()
	defer ssn.RUnlock()

	return ssn.value
}

func (ssn *safeServerName) Set(value string) error {
	ssn.Lock()
	defer ssn.Unlock()

	ssn.value = value
	return nil
}

type srvrMgr struct {
	connectCmd     string
	readBufferSize int
	serverName     safeServerName
	status         safeStatus
	shell          shell.Shell
	execCmdMu      sync.Mutex
	connectMu      sync.Mutex
	reconnectMu    sync.Mutex
	disconnectMu   sync.Mutex
}

// SrvrMgr is a public interface for the srvrmgr struct (https://stackoverflow.com/a/53034166).
type SrvrMgr interface {
	Connect() error
	Disconnect() error
	GetStatus() Status
	ExecuteCommand(cmd string) (string, error)
	// GetApplicationServerName() string
}

// NewSrvrmgr returns a new srvrmgr struct for provided connection command.
func NewSrvrmgr(connectCmd string, readBufferSize int) SrvrMgr {
	log.Debugln("NewSrvrmgr")

	// Redirection is no longer needed, because now we are able to correctly handle the stdErr
	// const redirectSuffix = " 2>&1"
	// connectCmd = strings.TrimSpace(connectCmd)
	// if !strings.HasSuffix(connectCmd, redirectSuffix) {
	// 	connectCmd = connectCmd + redirectSuffix
	// }

	// // Check for '/s' in connectCmd
	// if regexp.MustCompile(`(?i)(^|\s)\/s\s+[^\s]+(\s|$)`).FindStringSubmatch(connectCmd) == nil {
	// 	panic(errors.New("Error! Unable to create srvrmgr: connectCmd not contain '/s' argument.\n" + connectCmd + ""))
	// }

	// // Check for '/q' in connectCmd
	// if regexp.MustCompile(`(?i)(^|\s)\/q(\s|$)`).FindStringSubmatch(connectCmd) == nil {
	// 	connectCmd = connectCmd + " /q"
	// }

	sm := &srvrMgr{
		connectCmd:     connectCmd,
		readBufferSize: readBufferSize,
		serverName:     safeServerName{},
		status:         safeStatus{},
		shell:          nil,
		execCmdMu:      sync.Mutex{},
		connectMu:      sync.Mutex{},
		reconnectMu:    sync.Mutex{},
		disconnectMu:   sync.Mutex{},
	}

	sm.status.Set(Disconnected)

	return sm
}

func (sm *srvrMgr) Connect() error {
	return sm.reconnect()
}

func (sm *srvrMgr) Disconnect() error {
	return sm.disconnect()
}

func (sm *srvrMgr) GetStatus() Status {
	return sm.status.Get()
}

// func (sm *srvrMgr) GetApplicationServerName() string {
// 	return sm.serverName
// }

func (sm *srvrMgr) ExecuteCommand(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)
	if strings.ToLower(cmd) == "exit" || strings.ToLower(cmd) == "quit" || strings.Contains(strings.ToLower(cmd), "set server") {
		return "", errors.New("Error! Command '" + cmd + "' not allowed.")
	}
	return sm.executeCommand(cmd)
}

func (sm *srvrMgr) connect() error {
	log.Debugln("srvrmgr.connect")

	sm.connectMu.Lock()
	defer sm.connectMu.Unlock()

	status := sm.status.Get()
	if status == Connected {
		return nil
	}
	if status != Disconnected {
		return errors.New("error! unable to connect, try later")
	}

	sm.status.Set(Connecting)
	log.Debugln("Connecting to srvrmgr...")
	sm.shell = shell.NewShell(sm.readBufferSize)

	connRes, err := sm.executeCommand(sm.connectCmd)
	if err != nil {
		sm.status.Set(ConnectionError)
		defer sm.disconnect()
		log.Errorln(err)
		return err
	}

	// @FIXME: what if GW is up, but all APPs are down?
	if !strings.Contains(connRes, "Connected to 1 server(s)") {
		sm.status.Set(ConnectionError)
		defer sm.disconnect()
		err := fmt.Errorf("error! Connection established, but it looks like there are multiple servers. Did you forget to define '/s' argument in connection command?")
		log.Errorln(err)
		return err
	}

	// Set the actual server name
	serverNameMatch := regexp.MustCompile("srvrmgr:([^>]+?)>").FindStringSubmatch(connRes)
	if len(serverNameMatch) <= 1 {
		sm.status.Set(ConnectionError)
		defer sm.disconnect()
		err := fmt.Errorf("error! Connection established, but server name not found. Did you forget to define '/s' argument in connection command?")
		log.Errorln(err)
		return err
	}

	serverName := serverNameMatch[1]
	sm.serverName.Set(serverName)
	sm.status.Set(Connected)
	log.Infoln("Successfully connected to server: '" + serverName + "'.")

	// Disable footer (last string in command result like "12 rows returned.")
	sm.executeCommand("set footer false")

	return nil
}

func (sm *srvrMgr) disconnect() error {
	log.Debugln("srvrmgr.disconnect")

	sm.disconnectMu.Lock()
	defer sm.disconnectMu.Unlock()

	status := sm.status.Get()
	if status == Disconnected {
		// return errors.New("error! srvrmgr already disconnected")
		return nil
	}
	if status != Connected && status != ConnectionError {
		return errors.New("error! unable to disconnect, try later")
	}

	log.Debugln("Disconnecting from srvrmgr...")
	sm.status.Set(Disconnecting)
	output, err := sm.executeCommand("quit")

	sm.status.Set(Disconnected)
	sm.serverName.Set("")

	sm.shell.Terminate()
	sm.shell = nil

	if err != nil {
		log.Errorln(err)
		return err
	}
	if !strings.Contains(output, "Disconnecting from server.") {
		err = fmt.Errorf("error on exit: '%v'", output)
		log.Errorln(err)
		return err
	}
	log.Debugln("srvrmgr connection closed.")
	return nil
}

func (sm *srvrMgr) reconnect() error {
	log.Debugln("srvrmgr.reconnect")

	sm.reconnectMu.Lock()
	defer sm.reconnectMu.Unlock()

	if err := sm.disconnect(); err != nil {
		return err
	}

	time.Sleep(100 * time.Millisecond)

	if err := sm.connect(); err != nil {
		return err
	}
	return nil
}

func (sm *srvrMgr) executeCommand(cmd string) (string, error) {
	log.Debugln("srvrmgr.executeCommand")

	sm.execCmdMu.Lock()
	defer sm.execCmdMu.Unlock()

	status := sm.status.Get()
	if status == Disconnecting {
		if cmd != "quit" {
			return "", errors.New("error! unable to execute command '" + cmd + "', because srvrmgr is in process of disconnecting from server")
		}
	} else if status == Connecting {
		if cmd != sm.connectCmd {
			return "", errors.New("error! unable to execute command '" + cmd + "', because srvrmgr is in process of connecting to server")
		}
	} else if status != Connected {
		return "", errors.New("error! unable to execute command '" + cmd + "', because srvrmgr is not connected to server")
	}

	// Check for '/p password' substring in command and remove it from log
	cmdForLog := regexp.MustCompile(`(?i)(.*?\/p\s+)([^\s]+)(\s+.*|$)`).ReplaceAllString(cmd, "$1********$3")
	log.Debugf("	cmd: '%s'", cmdForLog)

	result, err := sm.shell.ExecuteCommand(cmd)
	if err != nil {
		log.Errorln(err)
		return "", err
	}

	// Validation for srvrmgr errors in command output
	/* Examples:
	srvrmgr> list component 123
		list component 123
		SBL-ADM-60070: Error reported on server 'sbldev' follows:
		SBL-ADM-01050: The specified component is not active on this server

	srvrmgr:sbldev> list ent param MaxThreads show PA_NAME, PA_VALUE
		list ent param MaxThreads show PA_NAME, PA_VALUE
		SBL-ADM-60070: Error reported on server 'Gateway Server' follows:
		SBL-SCM-00008: Batch read operation failed
	*/
	if regexp.MustCompile(`SBL-[^-]+-\d+: Error reported on server`).MatchString(result) {
		err := fmt.Errorf(result)
		log.Errorln(err)
		return "", err
	}

	// Remove last row with srvrmgr command prompt ('srvrmgr:sbldev>' )
	serverName := sm.serverName.Get()
	if len(serverName) > 0 {
		result = regexp.MustCompile("srvrmgr:"+serverName+">").ReplaceAllString(result, "")
	}

	result = strings.Trim(result, " \n")

	log.Debugf("	cmdResult:\n%v", result)

	return result, nil
}
