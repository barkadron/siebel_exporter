package srvrmgr

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/barkadron/siebel_exporter/shell"
	"github.com/prometheus/common/log"
)

// Status is an enumeration of srvrmgr statuses that represent a simple value.
type Status int

// Possible values for the Status enum.
const (
	_ Status = iota
	Disconnect
	Connecting
	Disconnecting
	Connected
)

type srvrMgr struct {
	connectCmd string
	serverName string
	status     Status
	shell      shell.Shell
}

// Public interface
// https://stackoverflow.com/a/53034166
type SrvrMgr interface {
	Connect() error
	Disconnect() error
	GetStatus() Status
	ExecuteCommand(cmd string) (string, error)
	GetApplicationServerName() string
	PingGatewayServer() bool
}

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
		connectCmd: connectCmd,
		serverName: "",
		status:     Disconnect,
		shell:      shell.NewShell(readBufferSize),
	}

	return sm
}

func (sm *srvrMgr) GetStatus() Status {
	return sm.status
}

func (sm *srvrMgr) Connect() error {
	return sm.reconnect()
}

func (sm *srvrMgr) Disconnect() error {
	return sm.disconnect(true)
}

func (sm *srvrMgr) GetApplicationServerName() string {
	return sm.serverName
}

func (sm *srvrMgr) ExecuteCommand(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)
	if strings.ToLower(cmd) == "exit" || strings.ToLower(cmd) == "quit" {
		return "", sm.disconnect(false)
	} else {
		return sm.executeCommand(cmd)
	}
}

func (sm *srvrMgr) PingGatewayServer() bool {
	log.Debugln("Ping Siebel Gateway Server...")
	_, err := sm.executeCommand("list ent param MaxThreads show PA_VALUE")
	if err != nil {
		log.Errorln("Error pinging Siebel Gateway Server: \n", err, "\n")
		sm.disconnect(false)
		return false // Down
	}
	log.Debugln("Successfully pinged Siebel Gateway Server.")
	return true // Up
}

func (sm *srvrMgr) connect() error {
	log.Debugln("srvrmgr.connect")
	if sm.status == Connected || sm.status == Connecting {
		return nil
	}

	log.Debugln("Connecting to srvrmgr...")
	sm.status = Connecting
	connRes, err := sm.executeCommand(sm.connectCmd)
	if err != nil {
		sm.status = Disconnect
		log.Errorln(err)
		return err
	}
	if strings.Contains(connRes, "Connected to 1 server(s)") {
		// Set the actual server name
		serverNameMatch := regexp.MustCompile("srvrmgr:([^>]+?)>").FindStringSubmatch(connRes)
		if len(serverNameMatch) >= 1 {
			sm.serverName = serverNameMatch[1]
			sm.status = Connected
			log.Infoln("Successfully connected to server: '" + sm.serverName + "'.")
		} else {
			defer sm.disconnect(false)
			err := fmt.Errorf("error! Connection established, but server name not found. Did you forget to define '/s' argument in connection command?")
			log.Errorln(err)
			return err
		}
	} else {
		defer sm.disconnect(false)
		err := fmt.Errorf("error! Connection established, but it looks like there are multiple servers. Did you forget to define '/s' argument in connection command?")
		log.Errorln(err)
		return err
	}

	// Disable footer (last string in command result like "12 rows returned.")
	sm.executeCommand("set footer false")

	return nil
}

func (sm *srvrMgr) reconnect() error {
	log.Debugln("srvrmgr.reconnect")
	if err := sm.disconnect(false); err != nil {
		return err
	}
	if err := sm.connect(); err != nil {
		return err
	}
	return nil
}

func (sm *srvrMgr) disconnect(closeShell bool) error { //@TODO: closeShell... mb just close shell all the time???
	log.Debugln("srvrmgr.disconnect")
	if sm.status == Disconnect || sm.status == Disconnecting {
		return nil
	}

	log.Debugln("Disconnecting from srvrmgr...")
	sm.status = Disconnecting
	output, err := sm.executeCommand("quit")
	sm.status = Disconnect
	sm.serverName = ""
	if closeShell {
		sm.shell.Terminate()
	}
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

func (sm *srvrMgr) executeCommand(cmd string) (string, error) {
	log.Debugln("srvrmgr.executeCommand")

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
		srvrmgrError := fmt.Errorf(result)
		log.Errorln(srvrmgrError)
		return "", srvrmgrError
	}

	// Remove last row with srvrmgr command prompt ('srvrmgr:sbldev>' )
	if len(sm.serverName) > 0 {
		result = regexp.MustCompile("srvrmgr:"+sm.serverName+">").ReplaceAllString(result, "")
	}

	result = strings.Trim(result, " \n")

	return result, nil
}
