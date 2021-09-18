package shell

import (
	"errors"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/prometheus/common/log"
)

type shell struct {
	cmd                *exec.Cmd
	readBufferSize     int
	stdInPipe          *io.WriteCloser
	stdOutPipe         *io.ReadCloser
	stdErrPipe         *io.ReadCloser
	stdOutReaderReady  bool
	stdErrReaderReady  bool
	outCh              *chan string
	outErrCh           *chan error
	errCh              *chan error
	stdOutReaderQuitCh *chan bool
	stdErrReaderQuitCh *chan bool
}

// Shell is a public interface for the shell struct (https://stackoverflow.com/a/53034166).
type Shell interface {
	ExecuteCommand(cmd string) (string, error)
	Terminate()
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

	log.Debugln("	- start command.")
	if err := cmd.Start(); err != nil {
		panic(err)
	}

	log.Debugln("	- create channels.")
	outCh := make(chan string, 1)
	outErrCh := make(chan error, 1)
	errCh := make(chan error, 1)
	stdOutReaderQuitCh := make(chan bool, 1)
	stdErrReaderQuitCh := make(chan bool, 1)

	log.Debugln("	- init shell-object.")
	s := &shell{
		cmd:                cmd,
		readBufferSize:     readBufferSize,
		stdInPipe:          &stdInPipe,
		stdOutPipe:         &stdOutPipe,
		stdErrPipe:         &stdErrPipe,
		stdOutReaderReady:  false,
		stdErrReaderReady:  false,
		outCh:              &outCh,
		outErrCh:           &outErrCh,
		errCh:              &errCh,
		stdOutReaderQuitCh: &stdOutReaderQuitCh,
		stdErrReaderQuitCh: &stdErrReaderQuitCh,
	}

	var readyToReadWG sync.WaitGroup
	readyToReadWG.Add(2)

	// Reader for StdErr
	go func() {
		log.Debugln("	- run reader for StdErr.")
		defer close(errCh)
		for {
			select {
			case <-stdErrReaderQuitCh:
				s.stdErrReaderReady = false
				log.Debugln("Exit from StdErrReader.")
				return
			default:
				if !s.stdErrReaderReady {
					s.stdErrReaderReady = true
					readyToReadWG.Done()
				}
				log.Debugln("StdErrReader waiting for data in StdErrPipe...")
				if stdErrRes, err := read(s.stdErrPipe, s.readBufferSize); err != nil {
					log.Debugln("StdErrReader received error.")
					// log.Debugln("	StdErrReader >>> ", err)
					errCh <- err
				} else {
					log.Debugln("StdErrReader received data.")
					// log.Debugln("	StdErrReader >>> ", stdErrRes)
					errCh <- errors.New(stdErrRes)
				}
			}
		}
	}()

	// Reader for StdOut
	go func() {
		log.Debugln("	- run reader for StdOut.")
		defer close(outCh)
		defer close(outErrCh)
		for {
			select {
			case <-stdOutReaderQuitCh:
				s.stdOutReaderReady = false
				log.Debugln("Exit from StdOutReader.")
				return
			default:
				if !s.stdOutReaderReady {
					s.stdOutReaderReady = true
					readyToReadWG.Done()
				}
				log.Debugln("StdOutReader waiting for data in StdOutPipe...")
				if stdOutRes, err := read(s.stdOutPipe, s.readBufferSize); err != nil {
					log.Debugln("StdOutReader received error.")
					// log.Debugln("	StdOutReader >>> ", err)
					outErrCh <- err
				} else {
					log.Debugln("StdOutReader received data.")
					// log.Debugln("	StdOutReader >>> ", stdOutRes)
					outCh <- stdOutRes
				}
			}
		}
	}()

	readyToReadWG.Wait()
	log.Debugln("	- shell-object is ready.")
	return s
}

func (s *shell) ExecuteCommand(cmd string) (string, error) {
	if strings.ToLower(strings.TrimSpace(cmd)) == "exit" {
		s.terminate()
		return "", nil
	}
	return s.executeCommand(cmd)
}

func (s *shell) Terminate() {
	log.Debugln("shell.Terminate")
	s.terminate()
}

func (s *shell) executeCommand(cmd string) (string, error) {
	log.Debugln("shell.executeCommand")

	if !s.stdOutReaderReady {
		return "", errors.New("unable to execute command because stdoutreader is not ready")
	}
	if !s.stdErrReaderReady {
		return "", errors.New("unable to execute command because stderrreader is not ready")
	}

	// Check for '/p password' in command and remove it from log
	cmdForLog := regexp.MustCompile(`(?i)(.*?\/p\s+)([^\s]+)(\s+.*|$)`).ReplaceAllString(cmd, "$1********$3")
	log.Debugf("	'%s'", cmdForLog)

	// @FIXME: this is unstable, need more time...
	// timeoutCh := make(chan error, 1)
	// go func() {
	// 	defer close(timeoutCh)
	// 	if *commandTimeout <= 0 {
	// 		return
	// 	}
	// 	time.Sleep(time.Second * time.Duration(*commandTimeout))
	// 	timeoutCh <- errors.New("timeout")
	// }()

	if _, err := io.WriteString(*s.stdInPipe, cmd+"\n"); err != nil {
		log.Errorln("Error on WriteString to stdInPipe:", err)
		return "", err
	}

	var (
		cmdResult string = ""
		cmdError  error  = nil
	)

	select {
	case cmdResult = <-*s.outCh:
		log.Debugln("Command output received from outCh.")
	case cmdError = <-*s.outErrCh:
		log.Debugln("Command error received from outErrCh.")
	case cmdError = <-*s.errCh:
		log.Debugln("Command error received from errCh.")
		// case cmdError = <-timeoutCh:
		// 	log.Debugln("[timeout]")
	}

	if cmdError != nil {
		if strings.ToLower(strings.TrimSpace(cmd)) == "exit" && len(strings.Trim(cmdError.Error(), " \n")) == 0 {
			log.Debug("Hide empty error (response of 'exit' command).")
			cmdError = nil
		} else {
			log.Errorf("	cmdError:\n%v", cmdError)
		}
	}

	cmdResult = strings.Trim(cmdResult, " \n")
	log.Debugf("	cmdResult:\n%v", cmdResult)

	return cmdResult, cmdError
}

func (s *shell) terminate() {
	log.Debugln("shell.terminate")

	defer close(*s.stdErrReaderQuitCh)
	defer close(*s.stdOutReaderQuitCh)

	log.Debugln("Send signal to quit channels.")
	*s.stdErrReaderQuitCh <- true
	*s.stdOutReaderQuitCh <- true

	s.executeCommand("exit")

	// time.Sleep(100 * time.Millisecond)

	log.Debugln("Close stdInPipe.")
	if err := (*s.stdInPipe).Close(); err != nil {
		log.Errorln("Error on closing stdInPipe:", err)
	}
	log.Debugln("Close stdOutPipe.")
	if err := (*s.stdOutPipe).Close(); err != nil {
		log.Errorln("Error on closing stdOutPipe:", err)
	}
	log.Debugln("Close stdErrPipe.")
	if err := (*s.stdErrPipe).Close(); err != nil {
		log.Errorln("Error on closing stdErrPipe:", err)
	}

	log.Debugln("Call cmd.Wait.")
	if err := s.cmd.Wait(); err != nil {
		log.Errorln("Error on cmd.Wait:", err)
	}
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
