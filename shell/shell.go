package shell

import (
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/prometheus/common/log"
)

type shell struct {
	cmd            *exec.Cmd
	readBufferSize int
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
type Shell interface {
	ExecuteCommand(cmd string) (string, error)
	Terminate() error
}

func NewShell(readBufferSize int) Shell {
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
	s := &shell{
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
				if s != nil && s.stdErrPipe != nil {
					if stdErrRes, err := read(s.stdErrPipe, s.readBufferSize); err != nil {
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
				if s != nil && s.stdOutPipe != nil {
					if stdOutRes, err := read(s.stdOutPipe, s.readBufferSize); err != nil {
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

	return s
}

func (s *shell) ExecuteCommand(cmd string) (string, error) {
	if strings.ToLower(strings.TrimSpace(cmd)) == "exit" {
		return "", s.terminate()
	} else {
		return s.executeCommand(cmd)
	}
}

func (s *shell) Terminate() error {
	log.Debugln("shell.Terminate")
	return s.terminate()
}

/***********************
*** internal methods ***
***********************/

func (s *shell) executeCommand(cmd string) (string, error) {
	log.Debugln("shell.executeCommand")

	// @TODO: check for '/p password' in command and replace with **** for log
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

	if _, err := io.WriteString(*s.stdInPipe, cmd+"\n"); err != nil {
		log.Errorln(err)
		return "", err
	}

	select {
	case cmdResult = <-*s.outCh:
		log.Debugln("[chanOut]")
		break
	case cmdError = <-*s.outErrCh:
		log.Debugln("[chanOutErr]")
		break
	case cmdError = <-*s.errCh:
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

func (s *shell) terminate() error {
	log.Debugln("shell.terminate")

	defer close(*s.quitCh)
	*s.quitCh <- true

	_, err := s.executeCommand("exit")

	if err := (*s.stdInPipe).Close(); err != nil {
		log.Errorln(err)
	}
	if err := (*s.stdOutPipe).Close(); err != nil {
		log.Errorln(err)
	}
	if err := (*s.stdErrPipe).Close(); err != nil {
		log.Errorln(err)
	}

	if err := s.cmd.Wait(); err != nil {
		log.Errorln(err)
	}

	return err
}

func read(reader *io.ReadCloser, bufferSize int) (string, error) {
	var readError error = nil
	buf := make([]byte, bufferSize)
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
		if n < bufferSize {
			break
		}
	}

	return string(data), readError
}
