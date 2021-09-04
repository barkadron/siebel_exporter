package srvrmgr

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/barkadron/siebel_exporter/shell"
	"github.com/prometheus/common/log"
)

type srvrMgr struct {
	connectCmd string
	serverName string
	connected  bool
	shell      shell.Shell
}

// Public interface
// https://stackoverflow.com/a/53034166
type SrvrMgr interface {
	Reconnect() error
	Disconnect() error
	IsConnected() bool
	ExecuteCommand(cmd string) (string, error)
	GetApplicationServerName() string
	PingGatewayServer() bool
}

func NewSrvrmgr(connectCmd string, shell shell.Shell) SrvrMgr {
	log.Debugln("NewSrvrmgr")

	// Redirection is no longer needed, because now we are able to correctly handle the stdErr
	// const redirectSuffix = " 2>&1"
	// connectCmd = strings.TrimSpace(connectCmd)
	// if !strings.HasSuffix(connectCmd, redirectSuffix) {
	// 	connectCmd = connectCmd + redirectSuffix
	// }

	sm := &srvrMgr{
		connectCmd: connectCmd,
		serverName: "",
		connected:  false,
		shell:      shell,
	}

	sm.connect()

	return sm
}

func (sm *srvrMgr) IsConnected() bool {
	return sm.connected
}

func (sm *srvrMgr) Reconnect() error {
	return sm.reconnect()
}

func (sm *srvrMgr) Disconnect() error {
	return sm.disconnect()
}

func (sm *srvrMgr) GetApplicationServerName() string {
	return sm.serverName
}

func (sm *srvrMgr) ExecuteCommand(cmd string) (string, error) {
	if strings.ToLower(strings.TrimSpace(cmd)) == "exit" {
		return "", sm.disconnect()
	} else {
		return sm.executeCommand(cmd)
	}
}

func (sm *srvrMgr) PingGatewayServer() bool {
	log.Debugln("Ping Siebel Gateway Server...")
	_, err := sm.executeCommand("list ent param MaxThreads show PA_VALUE")
	if err != nil {
		log.Errorln("Error pinging Siebel Gateway Server: \n", err, "\n")
		sm.exit(false)
		return false // Down
	}
	log.Debugln("Successfully pinged Siebel Gateway Server.")
	return true // Up
}

/***********************
*** internal methods ***
***********************/

func (sm *srvrMgr) connect() error {
	log.Debugln("srvrmgr.connect")
	if sm.connected {
		return nil
	}
	log.Infoln("Connecting to srvrmgr...")
	connRes, err := sm.executeCommand(sm.connectCmd)
	if err != nil {
		log.Errorln(err)
		return err
	}
	if strings.Contains(connRes, "Connected to 1 server(s)") {
		// Set the actual server name
		serverNameMatch := regexp.MustCompile("srvrmgr:([^>]+?)>").FindStringSubmatch(connRes)
		if len(serverNameMatch) >= 1 {
			sm.serverName = serverNameMatch[1]
			sm.connected = true
			log.Infoln("Successfully connected to server: ", sm.serverName)
		} else {
			return fmt.Errorf("error! siebel server name was not found: '%s'", connRes)
		}
	} else {
		return fmt.Errorf("error! connection was not established: '%s'", connRes)
	}

	// Disable footer (last string in command result like "12 rows returned.")
	sm.executeCommand("set footer false")

	return nil
}

func (sm *srvrMgr) disconnect() error {
	log.Debugln("srvrmgr.disconnect")
	return sm.exit(true)
}

func (sm *srvrMgr) reconnect() error {
	log.Debugln("srvrmgr.reconnect")
	if err := sm.exit(false); err != nil {
		return err
	}
	if err := sm.connect(); err != nil {
		return err
	}
	return nil
}

func (sm *srvrMgr) exit(closePipes bool) error {
	log.Debugln("srvrmgr.exit")
	if !sm.connected {
		return nil
	}
	log.Infoln("Close srvrmgr connection...")
	log.Debugln("closePipes: ", closePipes)
	// output, err := s.executeCommand("exit")
	// _, err := s.executeCommand("exit")
	_, err := sm.executeCommand("quit")
	sm.serverName = ""
	sm.connected = false
	if closePipes {
		sm.shell.Terminate()
	}
	if err != nil {
		log.Errorln(err)
		return err
	}
	// @FIXME: some shit happens here - output is empty if CTRL+C pressed
	// if !strings.Contains(output, "Disconnecting from server.") {
	// 	err = fmt.Errorf("error! failed to disconnect: '%s'", output)
	// 	log.Errorln(err)
	// 	return err
	// }
	log.Infoln("Connection closed.")
	return nil
}

func (sm *srvrMgr) executeCommand(cmd string) (string, error) {
	log.Debugln("srvrmgr.executeCommand")
	// log.Debugf("	'%s'", cmd)

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
	// log.Debugf("	RESULT:\n%s", result)

	return result, nil
}
