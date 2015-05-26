package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/pkg/jsonlog"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/pkg/tailfile"
	"github.com/docker/docker/pkg/timeutils"
)

type ContainerLogsConfig struct {
	Follow, Timestamps   bool
	Tail                 string
	Since                time.Time
	UseStdout, UseStderr bool
	OutStream            io.Writer
}

func (daemon *Daemon) ContainerLogs(name string, config *ContainerLogsConfig) error {
	var (
		lines  = -1
		format string
	)
	if !(config.UseStdout || config.UseStderr) {
		return fmt.Errorf("You must choose at least one stream")
	}
	if config.Timestamps {
		format = timeutils.RFC3339NanoFixed
	}
	if config.Tail == "" {
		config.Tail = "latest"
	}

	container, err := daemon.Get(name)
	if err != nil {
		return err
	}

	var (
		outStream = config.OutStream
		errStream io.Writer
	)
	if !container.Config.Tty {
		errStream = stdcopy.NewStdWriter(outStream, stdcopy.Stderr)
		outStream = stdcopy.NewStdWriter(outStream, stdcopy.Stdout)
	} else {
		errStream = outStream
	}

	if container.LogDriverType() != jsonfilelog.Name {
		return fmt.Errorf("\"logs\" endpoint is supported only for \"json-file\" logging driver")
	}
	maxFile := 1
	container.readHostConfig()
	cfg := container.getLogConfig()
	conf := cfg.Config
	if val, ok := conf["max-file"]; ok {
		var err error
		maxFile, err = strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("Error reading max-file value: %s", err)
		}
	}
	logDriver, err := container.getLogger()
	_, err = logDriver.GetReader()
	if err != nil {
		logrus.Errorf("Error reading logs: %s", err)
	} else {
		// json-file driver
		if config.Tail != "all" && config.Tail != "latest" {
			var err error
			lines, err = strconv.Atoi(config.Tail)
			if err != nil {
				logrus.Errorf("Failed to parse tail %s, error: %v, show all logs", config.Tail, err)
				lines = -1
			}
		}

		if lines != 0 {
			n := maxFile
			if config.Tail == "latest" && config.Since.IsZero() {
				n = 1
			}
			before := false
			for i := n; i > 0; i-- {
				if before {
					break
				}
				cLog, err := getReader(logDriver, i, n, lines)
				if err != nil {
					logrus.Debugf("Error reading %d log file: %v", i-1, err)
					continue
				}
				//if lines are specified, then iterate only once
				if lines > 0 {
					i = 1
				} else { // if lines are not specified, cLog is a file, It needs to be closed
					defer cLog.(*os.File).Close()
				}
				dec := json.NewDecoder(cLog)
				l := &jsonlog.JSONLog{}
				for {
					l.Reset()
					if err := dec.Decode(l); err == io.EOF {
						break
					} else if err != nil {
						logrus.Errorf("Error streaming logs: %s", err)
						break
					}
					logLine := l.Log
					if !config.Since.IsZero() && l.Created.Before(config.Since) {
						continue
					}
					if config.Timestamps {
						// format can be "" or time format, so here can't be error
						logLine, _ = l.Format(format)
					}
					if l.Stream == "stdout" && config.UseStdout {
						io.WriteString(outStream, logLine)
					}
					if l.Stream == "stderr" && config.UseStderr {
						io.WriteString(errStream, logLine)
					}
				}
			}
		}
	}

	if config.Follow && container.IsRunning() {
		chErr := make(chan error)
		var stdoutPipe, stderrPipe io.ReadCloser

		// write an empty chunk of data (this is to ensure that the
		// HTTP Response is sent immediatly, even if the container has
		// not yet produced any data)
		outStream.Write(nil)

		if config.UseStdout {
			stdoutPipe = container.StdoutLogPipe()
			go func() {
				logrus.Debug("logs: stdout stream begin")
				chErr <- jsonlog.WriteLog(stdoutPipe, outStream, format, config.Since)
				logrus.Debug("logs: stdout stream end")
			}()
		}
		if config.UseStderr {
			stderrPipe = container.StderrLogPipe()
			go func() {
				logrus.Debug("logs: stderr stream begin")
				chErr <- jsonlog.WriteLog(stderrPipe, errStream, format, config.Since)
				logrus.Debug("logs: stderr stream end")
			}()
		}

		err = <-chErr
		if stdoutPipe != nil {
			stdoutPipe.Close()
		}
		if stderrPipe != nil {
			stderrPipe.Close()
		}
		<-chErr // wait for 2nd goroutine to exit, otherwise bad things will happen

		if err != nil && err != io.EOF && err != io.ErrClosedPipe {
			if e, ok := err.(*net.OpError); ok && e.Err != syscall.EPIPE {
				logrus.Errorf("error streaming logs: %v", err)
			}
		}
	}
	return nil
}

func getReader(logDriver logger.Logger, fileIndex, maxFiles, lines int) (io.Reader, error) {
	if lines <= 0 {
		cLog, err := logDriver.GetReaderByIndex(fileIndex - 1)
		return cLog, err
	}
	buf := bytes.NewBuffer([]byte{})
	remaining := lines
	for i := 0; i < maxFiles; i++ {
		cLog, err := logDriver.GetReaderByIndex(i)
		if err != nil {
			return buf, err
		}
		f := cLog.(*os.File)
		ls, err := tailfile.TailFile(f, remaining)
		if err != nil {
			return buf, err
		}
		tmp := bytes.NewBuffer([]byte{})
		for _, l := range ls {
			fmt.Fprintf(tmp, "%s\n", l)
		}
		tmp.ReadFrom(buf)
		buf = tmp
		if len(ls) == remaining {
			return buf, nil
		}
		remaining = remaining - len(ls)
	}
	return buf, nil
}
