package main

import (
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/prometheus/common/log"
)

type Shell struct {
	cmd *exec.Cmd
	// cmdWait        func() error
	cmdIn          *io.WriteCloser
	cmdOut         *io.ReadCloser
	cmdErr         *io.ReadCloser
	readBufferSize int
}

// Public interface
// https://stackoverflow.com/a/53034166
type iShell interface {
	ExecuteCommand(cmd string) (string, error)
	Terminate() error
}

func NewShell() iShell {
	log.Debugln("NewShell")

	var shellType string

	switch strings.ToLower(runtime.GOOS) {
	case "linux":
		shellType = "bash"
	// @TODO: Need universal solution for Linux and Windows
	// case "windows":
	// 	shellType = "cmd"
	default:
		panic("Error! Unsupported OS '" + runtime.GOOS + "'.")
	}

	cmd := exec.Command(shellType)

	cmdIn, err := cmd.StdinPipe()
	if err != nil {
		panic(err)
	}
	cmdOut, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	cmdErr, err := cmd.StderrPipe()
	if err != nil {
		panic(err)
	}

	if err := cmd.Start(); err != nil {
		panic(err)
	}

	shell := &Shell{
		cmd: cmd,
		// cmdWait:        cmd.Wait,
		cmdIn:          &cmdIn,
		cmdOut:         &cmdOut,
		cmdErr:         &cmdErr,
		readBufferSize: *readBufferSize,
	}

	return shell
}

func (shell *Shell) ExecuteCommand(cmd string) (string, error) {
	if strings.ToLower(strings.TrimSpace(cmd)) == "exit" {
		return "", shell.terminate()
	} else {
		return shell.executeCommand(cmd)
	}
}

func (shell *Shell) Terminate() error {
	log.Debugln("shell.Terminate")
	return shell.terminate()
}

/***********************
*** internal methods ***
***********************/

func (shell *Shell) executeCommand(cmd string) (string, error) {
	log.Debugln("shell.executeCommand")
	log.Debugf("	'%s'", cmd)

	if _, err := io.WriteString(*shell.cmdIn, cmd+"\n"); err != nil {
		log.Errorln(err)
		return "", err
	}

	// @FIXME: Fail without timeout here if got something in cmd.stdErr
	time.Sleep(50 * time.Millisecond)

	result, err := shell.readOutput()

	if err != nil {
		log.Errorln(err)
		return "", err
	}

	result = strings.Trim(result, " \n")
	log.Debugf("	RESULT:\n%s", result)

	return result, nil
}

func (shell *Shell) terminate() error {
	log.Debugln("shell.terminate")
	_, err := shell.executeCommand("exit")

	if err := (*shell.cmdIn).Close(); err != nil {
		log.Errorln(err)
	}
	if err := (*shell.cmdOut).Close(); err != nil {
		log.Errorln(err)
	}
	if err := (*shell.cmdErr).Close(); err != nil {
		log.Errorln(err)
	}

	if err := shell.cmd.Wait(); err != nil {
		log.Errorln(err)
	}

	return err
}

func (shell *Shell) readOutput() (string, error) {
	log.Debugln("readOutput")

	var readResult string = ""
	var readError error = nil

	outCh := make(chan string)
	outErrCh := make(chan error)
	errCh := make(chan error)
	// quitCh := make(chan bool)

	// defer close(quitCh)

	// Read stderr
	go func() {
		defer close(errCh)
		// for {
		// 	select {
		// 	case <-quitCh:
		// 		log.Debugln("[stderr quitCh]")
		// 		return
		// 	default:
		// log.Debugln("[stderr default]")
		if stdErrRes, err := read(shell.cmdErr, &shell.readBufferSize); err != nil {
			log.Debugln("	- stderr (err != nil)")
			errCh <- err
		} else {
			log.Debugln("	- stderr (err == nil)")
			log.Debugln("		>>> '", stdErrRes, "'")
			errCh <- errors.New(stdErrRes)
		}
		// return
		// 	}
		// }
	}()

	// Read stdout
	go func() {
		defer close(outCh)
		defer close(outErrCh)
		// for {
		// 	select {
		// 	case <-quitCh:
		// 		log.Debugln("[stdout quitCh]")
		// 		return
		// 	default:
		// log.Debugln("[stdout default]")
		if stdOutRes, err := read(shell.cmdOut, &shell.readBufferSize); err != nil {
			log.Debugln("	- stdout (err != nil)")
			log.Debugln("		>>> '", err, "'")
			errCh <- err
		} else {
			log.Debugln("	- stdout (err == nil)")
			outCh <- stdOutRes
		}
		// 		return
		// 	}
		// }
	}()

	select {
	case msg := <-outCh:
		log.Debugln("[chanOut]")
		readResult = string(msg)
	case msg := <-outErrCh:
		log.Debugln("[chanOutErr]")
		readError = msg
	case msg := <-errCh:
		log.Debugln("[chanErr]")
		readError = msg
	case msg := <-time.After(time.Second * time.Duration(*queryTimeout)):
		log.Debugln("[timeout]")
		readError = errors.New("timeout (" + msg.String() + ")")
	}

	// log.Debugln("	readResult:\n", readResult)
	// log.Debugln("	readError:\n", readError)

	// quitCh <- true

	// log.Debugln("return")

	// leaks ((((
	log.Warnf("Number of hanging goroutines: %d", runtime.NumGoroutine()-1)

	return readResult, readError
}

func read(reader *io.ReadCloser, bufferSize *int) (string, error) {
	var readError error = nil
	buf := make([]byte, *bufferSize)
	data := []byte{}

	for {
		n, err := (*reader).Read(buf) // https://golang.org/src/io/io.go?s=3539:3599#L73
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if err != nil {
			if err != io.EOF {
				readError = err
			}
			break
		}
		if n < *bufferSize {
			break
		}
	}

	return string(data), readError
}
