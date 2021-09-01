package main

import (
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/prometheus/common/log"
)

type Shell struct {
	cmd            *exec.Cmd
	readBufferSize *int
	stdInPipe      *io.WriteCloser
	stdOutPipe     *io.ReadCloser
	stdErrPipe     *io.ReadCloser
	outCh          *chan string
	outErrCh       *chan error
	errCh          *chan error
	quitCh         *chan bool
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

	stdInPipe, err := cmd.StdinPipe()
	if err != nil {
		panic(err)
	}
	stdOutPipe, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	stdErrPipe, err := cmd.StderrPipe()
	if err != nil {
		panic(err)
	}

	log.Debugln("	- start command")
	if err := cmd.Start(); err != nil {
		panic(err)
	}

	log.Debugln("	- create channels")
	outCh := make(chan string, 1)
	outErrCh := make(chan error, 1)
	errCh := make(chan error, 1)
	quitCh := make(chan bool, 1)

	log.Debugln("	- init shell struct")
	shell := &Shell{
		cmd:            cmd,
		stdInPipe:      &stdInPipe,
		stdOutPipe:     &stdOutPipe,
		stdErrPipe:     &stdErrPipe,
		readBufferSize: readBufferSize,
		outCh:          &outCh,
		outErrCh:       &outErrCh,
		errCh:          &errCh,
		quitCh:         &quitCh,
	}

	log.Debugln("	- run goroutine for read from stderr")
	// Read stderr
	go func() {
		defer close(errCh)
		for {
			select {
			case <-quitCh:
				log.Debugln("[stderr quitCh]")
				return
			default:
				log.Debugln("[stderr default]")
				if shell != nil && shell.stdErrPipe != nil {
					if stdErrRes, err := read(shell.stdErrPipe, shell.readBufferSize); err != nil {
						log.Debugln("	- stderr (err != nil)")
						log.Debugln("		>>> '", err, "'")
						errCh <- err
					} else {
						log.Debugln("	- stderr (err == nil)")
						log.Debugln("		>>> '", stdErrRes, "'")
						errCh <- errors.New(stdErrRes)
					}
				}
			}
		}
	}()

	log.Debugln("	- run goroutine for read from stdout")
	// Read stdout
	go func() {
		defer close(outCh)
		defer close(outErrCh)
		for {
			select {
			case <-quitCh:
				log.Debugln("[stdout quitCh]")
				return
			default:
				log.Debugln("[stdout default]")
				if shell != nil && shell.stdOutPipe != nil {
					if stdOutRes, err := read(shell.stdOutPipe, shell.readBufferSize); err != nil {
						log.Debugln("	- stdout (err != nil)")
						log.Debugln("		>>> '", err, "'")
						errCh <- err
					} else {
						log.Debugln("	- stdout (err == nil)")
						outCh <- stdOutRes
					}
				}
				// return
			}
		}
	}()

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

	var (
		cmdResult string = ""
		cmdError  error  = nil
	)

	// @FIXME: this is unstable, need more time...
	// timeoutCh := make(chan error, 1)
	// go func() {
	// 	defer close(timeoutCh)
	// 	if *queryTimeout <= 0 {
	// 		return
	// 	}
	// 	time.Sleep(time.Second * time.Duration(*queryTimeout))
	// 	timeoutCh <- errors.New("timeout")
	// }()

	if _, err := io.WriteString(*shell.stdInPipe, cmd+"\n"); err != nil {
		log.Errorln(err)
		return "", err
	}

	select {
	case cmdResult = <-*shell.outCh:
		log.Debugln("[chanOut]")
		break
	case cmdError = <-*shell.outErrCh:
		log.Debugln("[chanOutErr]")
		break
	case cmdError = <-*shell.errCh:
		log.Debugln("[chanErr]")
		break
		// case cmdError = <-timeoutCh:
		// 	log.Debugln("[timeout]")
		// 	break
	}

	if cmdError != nil {
		log.Errorln(cmdError)
	} else {
		cmdResult = strings.Trim(cmdResult, " \n")
		log.Debugf("	RESULT:\n%s", cmdResult)
	}

	return cmdResult, cmdError
}

func (shell *Shell) terminate() error {
	log.Debugln("shell.terminate")

	defer close(*shell.quitCh)
	*shell.quitCh <- true

	_, err := shell.executeCommand("exit")

	if err := (*shell.stdInPipe).Close(); err != nil {
		log.Errorln(err)
	}
	if err := (*shell.stdOutPipe).Close(); err != nil {
		log.Errorln(err)
	}
	if err := (*shell.stdErrPipe).Close(); err != nil {
		log.Errorln(err)
	}

	if err := shell.cmd.Wait(); err != nil {
		log.Errorln(err)
	}

	return err
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
