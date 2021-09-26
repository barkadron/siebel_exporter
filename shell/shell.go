package shell

import (
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/prometheus/common/log"
)

// status is an enumeration of shell statuses that represent a simple value.
type status int

// possible values for the 'status' enum.
const (
	_ status = iota
	waitingForCommand
	commandExecution
	termination
	terminated
)

type safeStatus struct {
	sync.RWMutex
	value status
}

func (ss *safeStatus) Get() status {
	ss.RLock()
	defer ss.RUnlock()

	return ss.value
}

func (ss *safeStatus) Set(value status) error {
	ss.Lock()
	defer ss.Unlock()

	if ss.value == terminated {
		return errors.New("this status transition not allowed")
	}
	if ss.value == termination && value != terminated {
		return errors.New("this status transition not allowed")
	}
	ss.value = value
	return nil
}

type pipeReader struct {
	pipe     *io.ReadCloser
	resultCh chan string
	errorCh  chan error
	quitCh   chan bool
	ready    bool
}

func newPipeReader(pipe *io.ReadCloser) *pipeReader {
	return &pipeReader{
		pipe:     pipe,
		resultCh: make(chan string, 1),
		errorCh:  make(chan error, 1),
		quitCh:   make(chan bool, 1),
		ready:    false,
	}
}

type shell struct {
	cmd          *exec.Cmd
	status       safeStatus
	stdInPipe    *io.WriteCloser
	stdOutReader *pipeReader
	stdErrReader *pipeReader
	execCmdMu    sync.Mutex
	terminateMu  sync.Mutex
}

// Shell is a public interface for the shell struct (https://stackoverflow.com/a/53034166).
type Shell interface {
	ExecuteCommand(cmd string) (string, error)
	Terminate()
}

// NewShell returns a new shell struct.
func NewShell(readBufferSize int) Shell {
	log.Debugln("NewShell")

	shellType, err := getShellType()
	if err != nil {
		panic(err)
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

	log.Debugln("	- init shell-object.")
	s := &shell{
		cmd:          cmd,
		stdInPipe:    &stdInPipe,
		status:       safeStatus{},
		execCmdMu:    sync.Mutex{},
		terminateMu:  sync.Mutex{},
		stdOutReader: newPipeReader(&stdOutPipe),
		stdErrReader: newPipeReader(&stdErrPipe),
	}

	var readyToReadWG sync.WaitGroup
	readyToReadWG.Add(2)

	go readFromStdErr(s, &readyToReadWG, readBufferSize)
	go readFromStdOut(s, &readyToReadWG, readBufferSize)

	readyToReadWG.Wait()

	if err := s.status.Set(waitingForCommand); err != nil {
		panic(err)
	}

	log.Debugln("	- shell-object is ready.")
	return s
}

func (s *shell) ExecuteCommand(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)
	if strings.ToLower(cmd) == "exit" {
		return "", errors.New("Error! Command '" + cmd + "' not allowed.")
	}
	return s.executeCommand(cmd)
}

func (s *shell) Terminate() {
	log.Debugln("shell.Terminate")
	s.terminate()
}

func (s *shell) executeCommand(cmd string) (string, error) {
	log.Debugln("shell.executeCommand")

	s.execCmdMu.Lock()
	defer s.execCmdMu.Unlock()

	status := s.status.Get()
	if status == terminated {
		return "", errors.New("Error! Execution of command '" + cmd + "' not allowed because shell terminated.")
	}

	if status == termination {
		if strings.ToLower(cmd) != "exit" {
			return "", errors.New("Error! Execution of command '" + cmd + "' not allowed while shell is in process of termination.")
		}
		if err := s.status.Set(terminated); err != nil {
			log.Errorln(err)
			return "", err
		}
	} else {
		if err := s.status.Set(commandExecution); err != nil {
			log.Errorln(err)
			return "", err
		}
		defer s.status.Set(waitingForCommand)
	}

	if !s.stdOutReader.ready {
		return "", errors.New("unable to execute command because stdoutreader is not ready")
	}
	if !s.stdErrReader.ready {
		return "", errors.New("unable to execute command because stderrreader is not ready")
	}

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
	case cmdResult = <-s.stdOutReader.resultCh:
		log.Debugln("Command output received from stdOut.")
	case cmdError = <-s.stdOutReader.errorCh:
		log.Debugln("Command error received from stdOutErr.")
	case err := <-s.stdErrReader.resultCh:
		cmdError = errors.New(err)
		log.Debugln("Command error received from stdErr.")
	case cmdError = <-s.stdErrReader.errorCh:
		log.Debugln("Command error received from stdErrErr.")
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

	return cmdResult, cmdError
}

func (s *shell) terminate() {
	log.Debugln("shell.terminate")

	s.terminateMu.Lock()
	defer s.terminateMu.Unlock()

	status := s.status.Get()
	if status == termination || status == terminated {
		return
	}

	if err := s.status.Set(termination); err != nil {
		log.Errorln(err)
	}

	defer close(s.stdErrReader.quitCh)
	defer close(s.stdOutReader.quitCh)

	log.Debugln("Send signal to quit channels.")
	s.stdErrReader.quitCh <- true
	s.stdOutReader.quitCh <- true

	s.executeCommand("exit")

	// time.Sleep(100 * time.Millisecond)

	log.Debugln("Close stdInPipe.")
	if err := (*s.stdInPipe).Close(); err != nil {
		log.Errorln("Error on closing stdInPipe:", err)
	}
	log.Debugln("Close stdOutPipe.")
	if err := (*s.stdOutReader.pipe).Close(); err != nil {
		log.Errorln("Error on closing stdOutPipe:", err)
	}
	log.Debugln("Close stdErrPipe.")
	if err := (*s.stdErrReader.pipe).Close(); err != nil {
		log.Errorln("Error on closing stdErrPipe:", err)
	}

	log.Debugln("Call cmd.Wait.")
	if err := s.cmd.Wait(); err != nil {
		log.Errorln("Error on cmd.Wait:", err)
	}
}

func getShellType() (string, error) {
	switch strings.ToLower(runtime.GOOS) {
	case "linux":
		return "bash", nil
	// @TODO: Need universal solution for Linux and Windows
	// case "windows":
	// 	return "cmd", nil
	default:
		return "", errors.New("unsupported os '" + runtime.GOOS + "'")
	}
}

func readFromStdErr(s *shell, wg *sync.WaitGroup, readBufferSize int) {
	log.Debugln("	- run reader for StdErr.")
	defer close(s.stdErrReader.resultCh)
	defer close(s.stdErrReader.errorCh)

	for {
		select {
		case <-s.stdErrReader.quitCh:
			s.stdErrReader.ready = false
			log.Debugln("Exit from StdErrReader.")
			return
		default:
			if !s.stdErrReader.ready {
				s.stdErrReader.ready = true
				wg.Done()
			}
			log.Debugln("StdErrReader waiting for data in StdErrPipe...")
			if stdErrRes, err := read(s.stdErrReader.pipe, readBufferSize); err != nil {
				log.Debugln("StdErrReader error.")
				// log.Debugln("	StdErrReader >>> ", err)
				s.stdErrReader.errorCh <- err
			} else {
				log.Debugln("StdErrReader received data.")
				// log.Debugln("	StdErrReader >>> ", stdErrRes)
				s.stdErrReader.resultCh <- stdErrRes
			}
		}
	}
}

func readFromStdOut(s *shell, wg *sync.WaitGroup, readBufferSize int) {
	log.Debugln("	- run reader for StdOut.")
	defer close(s.stdOutReader.resultCh)
	defer close(s.stdOutReader.errorCh)

	for {
		select {
		case <-s.stdOutReader.quitCh:
			s.stdOutReader.ready = false
			log.Debugln("Exit from StdOutReader.")
			return
		default:
			if !s.stdOutReader.ready {
				s.stdOutReader.ready = true
				wg.Done()
			}
			log.Debugln("StdOutReader waiting for data in StdOutPipe...")
			if stdOutRes, err := read(s.stdOutReader.pipe, readBufferSize); err != nil {
				log.Debugln("StdOutReader error.")
				// log.Debugln("	StdOutReader >>> ", err)
				s.stdOutReader.errorCh <- err
			} else {
				log.Debugln("StdOutReader received data.")
				// log.Debugln("	StdOutReader >>> ", stdOutRes)
				s.stdOutReader.resultCh <- stdOutRes
			}
		}
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
