// Copyright 2015 Reborndb Org. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/juju/errors"
	"github.com/mitchellh/go-ps"
	log "github.com/ngaut/logging"
	"github.com/nu7hatch/gouuid"
	"github.com/reborndb/go/io/ioutils"
)

func genProcID() string {
	u, err := uuid.NewV4()
	if err != nil {
		log.Fatalf("gen uuid err: %v", err)
	}

	return strings.ToLower(hex.EncodeToString(u[0:16]))
}

type process struct {
	// uuid for a process in agent use
	ID string `json:"id"`

	// process type, like proxy, redis, qdb, dashboard
	Type string `json:"type"`

	// Current pid, every process will save it in its own pid file
	// so we don't save it in data file.
	Pid int `json:"-"`

	// for start process, use cmd and args
	Cmd  string   `json:"name"`
	Args []string `json:"args"`

	// for specail use
	Ctx map[string]string `json:"ctx"`

	postStartFunc func(p *process) error

	// if not nil, we will use this func to stop process
	stopFunc func(p *process) error
}

func newDefaultProcess(cmd string, tp string) *process {
	id := genProcID()
	p := new(process)

	p.ID = id
	p.Cmd = cmd
	p.Type = tp
	p.Ctx = make(map[string]string)

	return p
}

func loadProcess(dataPath string) (*process, error) {
	p := new(process)

	data, err := ioutil.ReadFile(dataPath)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if err = json.Unmarshal(data, &p); err != nil {
		return nil, errors.Trace(err)
	}

	if !isFileExist(p.pidPath()) {
		// pid file is not exists, we should not handle this id anymore
		os.Remove(dataPath)
		log.Infof("pid file %s is not exist, skip", p.pidPath())
		return nil, nil
	}

	data, err = ioutil.ReadFile(p.pidPath())
	if err != nil {
		return nil, errors.Trace(err)
	}

	if p.Pid, err = p.readPid(); err != nil {
		return nil, errors.Trace(err)
	}

	return p, nil
}

func (p *process) readPid() (int, error) {
	data, err := ioutil.ReadFile(p.pidPath())
	if err != nil {
		return 0, errors.Trace(err)
	}

	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func (p *process) addCmdArgs(args ...string) {
	p.Args = append(p.Args, args...)
}

func (p *process) start() error {
	c := exec.Command(p.Cmd, p.Args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	if err := c.Start(); err != nil {
		return errors.Trace(err)
	}

	go func() {
		// use another goroutine to wait process over
		// we don't handle anything here, because we will
		// check process alive in a checker totally.
		c.Wait()
	}()

	// wait some time
	log.Infof("wait 3 seonds for %s starts ok", p.Type)
	time.Sleep(3 * time.Second)

	// we must read pid from pid file
	var err error
	if p.Pid, err = p.readPid(); err != nil {
		return errors.Trace(err)
	}

	if b, err := p.checkAlive(); err != nil {
		return errors.Trace(err)
	} else if !b {
		return errors.Errorf("start %d (%s) but it's not alive", p.Pid, p.Type)
	}

	if p.postStartFunc != nil {
		if err := p.postStartFunc(p); err != nil {
			log.Errorf("post start %d (%s) err %v", p.Pid, p.Type, err)
			return errors.Trace(err)
		}
	}

	return errors.Trace(p.save())
}

func (p *process) save() error {
	// we only handle data file here, because process itself will handle pid file
	data, err := json.Marshal(p)
	if err != nil {
		return errors.Trace(err)
	}

	err = ioutils.WriteFileAtomic(p.dataPath(), data, 0644)
	return errors.Trace(err)
}

func (p *process) pidPath() string {
	return path.Join(dataDir, fmt.Sprintf("%s_%s.pid", p.Type, p.ID))
}

func (p *process) dataPath() string {
	return path.Join(dataDir, fmt.Sprintf("%s_%s.dat", p.Type, p.ID))
}

func (p *process) logPath() string {
	return path.Join(logDir, fmt.Sprintf("%s_%s.log", p.Type, p.ID))
}

func (p *process) baseName() string {
	return fmt.Sprintf("%s_%s", p.Type, p.ID)
}

func (p *process) checkAlive() (bool, error) {
	proc, err := ps.FindProcess(p.Pid)
	if err != nil {
		return false, errors.Trace(err)
	} else if proc == nil {
		// proc is not alive
		return false, nil
	} else {
		if strings.Contains(proc.Executable(), p.Cmd) {
			return true, nil
		} else {
			log.Warningf("pid %d exits, but exeutable name is %s, not %s", p.Pid, proc.Executable(), p.Cmd)
			return false, nil
		}
	}
}

func isFileExist(name string) bool {
	_, err := os.Stat(name)
	return !os.IsNotExist(err)
}

func (p *process) needRestart() bool {
	// if the process exited but the pid file exists,
	// we may think the process is closed unpredictably,
	// so we need restart it

	return isFileExist(p.pidPath())
}

func (p *process) clear() {
	os.Remove(p.pidPath())
	os.Remove(p.dataPath())
}

func (p *process) stop() error {
	b, err := p.checkAlive()
	if err != nil {
		return errors.Trace(err)
	}

	defer p.clear()

	if !b {
		return nil
	} else {
		if proc, err := os.FindProcess(p.Pid); err != nil {
			return errors.Trace(err)
		} else {
			if p.stopFunc != nil {
				if err := p.stopFunc(p); err != nil {
					log.Errorf("stop %d (%s) err %v, send kill signal", p.Pid, p.Type, err)
					proc.Signal(syscall.SIGTERM)
					proc.Signal(os.Kill)
				}
			} else {
				proc.Signal(syscall.SIGTERM)
				proc.Signal(os.Kill)
			}

			ch := make(chan struct{}, 1)
			go func(ch chan struct{}) {
				proc.Wait()
				ch <- struct{}{}
			}(ch)

			select {
			case <-ch:
			case <-time.After(5 * time.Minute):
				proc.Kill()
				log.Errorf("wait %d (%s)stopped timeout, force kill", p.Pid, p.Type)
			}

			return nil
		}
	}
}
