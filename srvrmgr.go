package main

import (
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/common/log"
)

/***************************
********* SRVRMGR **********
***************************/

type SRVRMGR struct {
	connectCmd string
	serverName string
	connected  bool
	cmdWait    func() error
	cmdIn      io.WriteCloser
	cmdOut     io.ReadCloser
	// cmdErr     io.ReadCloser
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

	shell := "bash" // @TODO: Need universal solution for Linux and Windows
	cmd := exec.Command(shell)

	cmdIn, err := cmd.StdinPipe()
	if err != nil {
		panic(err)
	}
	cmdOut, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	// cmdErr, err := cmd.StderrPipe()
	// if err != nil {
	// 	panic(err)
	// }

	if err := cmd.Start(); err != nil {
		panic(err)
	}

	srvrmgr := &SRVRMGR{
		connectCmd: connectCmd + " 2>&1",
		serverName: "",
		cmdWait:    cmd.Wait,
		cmdIn:      cmdIn,
		cmdOut:     cmdOut,
		// cmdErr:     cmdErr,
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
	return srvrmgr.executeCommand(cmd)
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
	if err := srvrmgr.exit(true); err != nil {
		return err
	}
	return nil
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
	_, err := srvrmgr.executeCommand("exit")
	srvrmgr.serverName = ""
	srvrmgr.connected = false
	if closePipes {
		// @TODO: error handling?
		srvrmgr.cmdIn.Close()
		srvrmgr.cmdOut.Close()
		// srvrmgr.cmdErr.Close()
		srvrmgr.cmdWait()
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
	log.Debugf("	'%s'", cmd)

	if _, err := io.WriteString(srvrmgr.cmdIn, cmd+"\n"); err != nil {
		log.Errorln(err)
		return "", err
	}

	// @FIXME: Fail without timeout here if got something in cmd.stdErr
	time.Sleep(50 * time.Millisecond)

	result, err := srvrmgr.readOutput()
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
	log.Debugf("	RESULT:\n%s", result)

	return result, nil
}

func (srvrmgr *SRVRMGR) readOutput() (string, error) {
	// log.Debugln("readOutput")
	// if stdErrRes, err := read(srvrmgr.cmdErr); err != nil {
	// 	log.Errorln(err)
	// 	return "", err
	// } else {
	// 	log.Debugln("stdErrRes:'", stdErrRes, "'")
	// 	if len(stdErrRes) >= 0 {
	// 		return stdErrRes, nil
	// 	} else {
	if stdOutRes, err := read(srvrmgr.cmdOut); err != nil {
		// log.Errorln(err)
		return "", err
	} else {
		// log.Debugln("stdOutRes:'", stdOutRes, "'")
		return stdOutRes, nil
	}
	// 	}
	// }
}

func read(reader io.Reader) (string, error) {
	var readError error = nil
	buf := make([]byte, *readBufferSize)
	data := []byte{}

	for {
		n, err := reader.Read(buf) // https://golang.org/src/io/io.go?s=3539:3599#L73
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if err != nil {
			if err != io.EOF {
				readError = err
			}
			break
		}
		if n < *readBufferSize {
			break
		}
	}

	return string(data), readError
}
