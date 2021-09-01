package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/prometheus/common/log"
)

type SRVRMGR struct {
	connectCmd string
	serverName string
	connected  bool
	shell      iShell
}

// Public interface
// https://stackoverflow.com/a/53034166
type iSRVRMGR interface {
	Reconnect() error
	Disconnect() error
	IsConnected() bool
	ExecuteCommand(cmd string) (string, error)
	GetApplicationServerName() string
	PingGatewayServer() bool
	// PingApplicationServer() bool
}

func NewSrvrmgr(connectCmd string) iSRVRMGR {
	log.Debugln("NewSrvrmgr")

	// Redirection is no longer needed, because now we are able to correctly handle the stdErr
	// const redirectSuffix = " 2>&1"
	// connectCmd = strings.TrimSpace(connectCmd)
	// if !strings.HasSuffix(connectCmd, redirectSuffix) {
	// 	connectCmd = connectCmd + redirectSuffix
	// }

	srvrmgr := &SRVRMGR{
		connectCmd: connectCmd,
		serverName: "",
		shell:      NewShell(),
	}

	srvrmgr.connect()

	return srvrmgr
}

func (srvrmgr *SRVRMGR) IsConnected() bool {
	return srvrmgr.connected
}

func (srvrmgr *SRVRMGR) Reconnect() error {
	return srvrmgr.reconnect()
}

func (srvrmgr *SRVRMGR) Disconnect() error {
	return srvrmgr.disconnect()
}

func (srvrmgr *SRVRMGR) GetApplicationServerName() string {
	return srvrmgr.serverName
}

func (srvrmgr *SRVRMGR) ExecuteCommand(cmd string) (string, error) {
	if strings.ToLower(strings.TrimSpace(cmd)) == "exit" {
		return "", srvrmgr.disconnect()
	} else {
		return srvrmgr.executeCommand(cmd)
	}
}

func (srvrmgr *SRVRMGR) PingGatewayServer() bool {
	log.Debugln("Ping Siebel Gateway Server...")
	_, err := srvrmgr.executeCommand("list ent param MaxThreads show PA_VALUE")
	if err != nil {
		log.Errorln("Error pinging Siebel Gateway Server: \n", err, "\n")
		srvrmgr.exit(false)
		return false // Down
	}
	log.Debugln("Successfully pinged Siebel Gateway Server.")
	return true // Up

	/* It is possible to check status of Gateway Server if gtwsrvr and siebsrvr installed on the same machine.
	Checking the Status of the Siebel Gateway Name Server System Service on UNIX: https://docs.oracle.com/cd/E74890_01/books/SystAdm/systadm_srvrsyssrvc5002.htm#sthref178
	Example:
	$ source /siebel/ses/gtwysrvr/siebenv.sh && start_ns
	$ started at Mon Jul  5 16:10:33 2021, pid: 17465, autostart: no
	$ stopped at Mon Jul  5 16:10:18 2021
	*/
}

/*
func (srvrmgr *SRVRMGR) PingApplicationServer() bool {
	log.Debugln("pingApplicationServer...")
	result, err := srvrmgr.executeCommand("list servers show SBLSRVR_STATE")
	if err != nil {
		log.Errorln("Error pinging Siebel Application Server.", err)
		return false // Down
	}
	if strings.Contains(result, "Running") {
		log.Debugln("Successfully pinged Siebel Application Server.")
		return true // Up
	} else {
		log.Errorln("Error pinging Siebel Application Server.", result)
		return false // Down
	}

	// https://docs.oracle.com/cd/B40099_02/books/SysDiag/SysDiagSysMonitor2.html
}
*/

/***********************
*** internal methods ***
***********************/

func (srvrmgr *SRVRMGR) connect() error {
	log.Debugln("srvrmgr.connect")
	if srvrmgr.connected {
		return nil
	}
	log.Infoln("Connecting to srvrmgr...")
	connRes, err := srvrmgr.executeCommand(srvrmgr.connectCmd)
	if err != nil {
		log.Errorln(err)
		return err
	}
	if strings.Contains(connRes, "Connected to 1 server(s)") {
		// Set the actual server name
		serverNameMatch := regexp.MustCompile("srvrmgr:([^>]+?)>").FindStringSubmatch(connRes)
		if len(serverNameMatch) >= 1 {
			srvrmgr.serverName = serverNameMatch[1]
			srvrmgr.connected = true
			log.Infoln("Successfully connected to server: ", srvrmgr.serverName)
		} else {
			return fmt.Errorf("error! siebel server name was not found: '%s'", connRes)
		}
	} else {
		return fmt.Errorf("error! connection was not established: '%s'", connRes)
	}

	// Disable footer (last string in command result like "12 rows returned.")
	srvrmgr.executeCommand("set footer false")

	return nil
}

func (srvrmgr *SRVRMGR) disconnect() error {
	log.Debugln("srvrmgr.disconnect")
	return srvrmgr.exit(true)
}

func (srvrmgr *SRVRMGR) reconnect() error {
	log.Debugln("srvrmgr.reconnect")
	if err := srvrmgr.exit(false); err != nil {
		return err
	}
	if err := srvrmgr.connect(); err != nil {
		return err
	}
	return nil
}

func (srvrmgr *SRVRMGR) exit(closePipes bool) error {
	log.Debugln("srvrmgr.exit")
	if !srvrmgr.connected {
		return nil
	}
	log.Infoln("Close srvrmgr connection...")
	log.Debugln("closePipes: ", closePipes)
	// output, err := srvrmgr.executeCommand("exit")
	// _, err := srvrmgr.executeCommand("exit")
	_, err := srvrmgr.executeCommand("quit")
	srvrmgr.serverName = ""
	srvrmgr.connected = false
	if closePipes {
		srvrmgr.shell.Terminate()
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

func (srvrmgr *SRVRMGR) executeCommand(cmd string) (string, error) {
	log.Debugln("srvrmgr.executeCommand")
	// log.Debugf("	'%s'", cmd)

	result, err := srvrmgr.shell.ExecuteCommand(cmd)
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
	if len(srvrmgr.serverName) > 0 {
		result = regexp.MustCompile("srvrmgr:"+srvrmgr.serverName+">").ReplaceAllString(result, "")
	}

	result = strings.Trim(result, " \n")
	// log.Debugf("	RESULT:\n%s", result)

	return result, nil
}
