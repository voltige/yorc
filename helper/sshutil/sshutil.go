// Copyright 2018 Bull S.A.S. Atos Technologies - Bull, Rue Jean Jaures, B.P.68, 78340, Les Clayes-sous-Bois, France.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sshutil

import (
	"bytes"
	goerr "errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sethvargo/go-retry"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/net/context"

	"github.com/ystia/yorc/v4/helper/executil"
	"github.com/ystia/yorc/v4/log"
)

// Client is interface allowing running command
type Client interface {
	RunCommand(string) (string, error)
	CopyFile(source io.Reader, remotePath string, permissions string) error
}

// TODO(loicalbertin) sshSession and SSHSessionWrapper may be merged

// SSHSessionWrapper is a wrapper with a piped SSH session
type SSHSessionWrapper struct {
	session *sshSession
	Stdout  io.Reader
	Stderr  io.Reader
}

// StdinPipe returns a pipe that will be connected to the
// remote command's standard input when the command starts.
func (sw *SSHSessionWrapper) StdinPipe() (io.WriteCloser, error) {
	return sw.session.StdinPipe()
}

// Close closes the session
func (sw *SSHSessionWrapper) Close() error {
	return sw.session.Close()
}

// RequestPty requests the association of a pty with the session on the remote host.
func (sw *SSHSessionWrapper) RequestPty(term string, h, w int, termmodes ssh.TerminalModes) error {
	return sw.session.RequestPty(term, h, w, termmodes)
}

// Start runs cmd on the remote host. Typically, the remote
// server passes cmd to the shell for interpretation.
// A Session only accepts one call to Run, Start or Shell.
func (sw *SSHSessionWrapper) Start(cmd string) error {
	return sw.session.Start(cmd)
}

// SSHClient is a client SSH
type SSHClient struct {
	Config       *ssh.ClientConfig
	Host         string
	Port         int
	RetryBackoff time.Duration
	MaxRetries   uint64
}

// SSHAgent is an SSH agent
type SSHAgent struct {
	agent  agent.Agent
	conn   net.Conn
	Socket string
	pid    int
}

// Sessions Pool used to provide reusable sessions for each sshClient
var sessionsPool = &pool{}

// Utility function that make function to execute code under retry and connection timeout
func (client *SSHClient) makeRetryFunc(f func(ctx context.Context) error) func() error {
	backoffDuration := client.RetryBackoff
	if backoffDuration <= 0 {
		backoffDuration = 1
	}
	b, _ := retry.NewConstant(backoffDuration)
	b = retry.WithMaxRetries(client.MaxRetries, b)
	return func() error {
		err := retry.Do(context.Background(), b, func(ctx context.Context) error {
			if client.Config != nil && client.Config.Timeout > 0 {
				var cf context.CancelFunc
				ctx, cf = context.WithTimeout(ctx, client.Config.Timeout)
				defer cf()
			}
			return f(ctx)
		})
		// Unwrap error as we don't want to see retry.retryableError
		// not my preference but will work (see https://github.com/sethvargo/go-retry/pull/2)
		return goerr.Unwrap(err)
	}
}

// GetSessionWrapper allows to return a session wrapper in order to handle stdout/stderr for running long synchronous commands
func (client *SSHClient) GetSessionWrapper() (*SSHSessionWrapper, error) {
	var ps = &SSHSessionWrapper{}
	var err error

	retryOpenSession := client.makeRetryFunc(func(ctx context.Context) error {
		ps.session, err = client.newSession(ctx)
		return retry.RetryableError(errors.Wrap(err, "Unable to prepare SSH command"))
	})

	err = retryOpenSession()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	log.Debug("[SSHSession] Add Stderr/Stdout pipelines")
	ps.Stdout, err = ps.session.StdoutPipe()
	if err != nil {
		return nil, errors.Wrap(err, "Unable to setup stdout for session")
	}

	ps.Stderr, err = ps.session.StderrPipe()
	if err != nil {
		return nil, errors.Wrap(err, "Unable to setup stderr for session")
	}

	return ps, nil
}

// RunCommand allows to run a specified command
func (client *SSHClient) RunCommand(cmd string) (string, error) {

	var res string

	retryRunCommand := client.makeRetryFunc(func(ctx context.Context) error {
		var rerr error
		res, rerr = client.runCommand(ctx, cmd)
		if rerr == nil {
			return nil
		}
		var eerr *ssh.ExitError
		if goerr.As(rerr, &eerr) {
			return rerr
		}
		return retry.RetryableError(rerr)
	})
	err := retryRunCommand()
	return res, errors.WithStack(err)
}

func (client *SSHClient) runCommand(ctx context.Context, cmd string) (string, error) {
	session, err := client.newSession(ctx)
	if err != nil {
		return "", errors.Wrap(err, "Unable to create new session")
	}
	defer session.Close()

	log.Debugf("[SSHSession] cmd: %q", cmd)
	stdOutErrBytes, err := session.CombinedOutput(cmd)
	stdOutErrStr := strings.Trim(string(stdOutErrBytes[:]), "\x00")
	log.Debugf("[SSHSession] stdout/stderr: %q", stdOutErrStr)
	return stdOutErrStr, errors.WithStack(err)
}

func (client *SSHClient) newSession(ctx context.Context) (*sshSession, error) {
	session, err := sessionsPool.openSession(ctx, client)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to create session")
	}

	return session, nil
}

// RunCommand allows to run a specified command from a session wrapper in order to handle stdout/stderr during long synchronous commands
// stdout/stderr are retrieved asynchronously with SSHSessionWrapper.Stdout and SSHSessionWrapper.Stderr
func (sw *SSHSessionWrapper) RunCommand(ctx context.Context, cmd string) error {
	chClosed := make(chan struct{})
	defer func() {
		sw.session.Close()
		close(chClosed)
	}()
	log.Debugf("[SSHSession] running command: %q", cmd)
	go func() {
		select {
		case <-ctx.Done():
			log.Debug("[SSHSession] Cancellation has been sent: a sigkill signal is sent to remote process")
			sw.session.Signal(ssh.SIGKILL)
			sw.session.Close()
			return
		case <-chClosed:
			return
		}
	}()
	return sw.session.Run(cmd)
}

// CopyFile allows to copy a reader over SSH with defined remote path and specific permissions
// CopyFile allows to copy a reader over SSH with defined remote path and specific permissions
func (client *SSHClient) CopyFile(source io.Reader, remotePath string, permissions string) error {
	// Create the remote directory
	remoteDir := path.Dir(remotePath)
	mkdirCmd := fmt.Sprintf("mkdir -p %s", remoteDir)
	_, err := client.RunCommand(mkdirCmd)
	if err != nil {
		return errors.Wrapf(err, "Couldn't create the remote directory:%q", remoteDir)
	}

	// determine the length by reading the reader
	content, err := ioutil.ReadAll(source)
	if err != nil {
		return err
	}
	size := int64(len(content))

	// Copy the file with scp
	filename := path.Base(remotePath)
	directory := path.Dir(remotePath)

	wg := sync.WaitGroup{}
	wg.Add(2)

	errCh := make(chan error, 2)

	var session *sshSession
	retryOpenSession := client.makeRetryFunc(func(ctx context.Context) error {
		var err error
		session, err = client.newSession(ctx)
		return retry.RetryableError(err)
	})
	err = retryOpenSession()
	if err != nil {
		return err
	}

	// need to get StdinPipe before starting ssh process
	w, err := session.StdinPipe()
	if err != nil {
		return err
	}

	go func() {
		defer wg.Done()

		// close writer once file data have been written or if error occurs
		defer w.Close()
		_, err = fmt.Fprintln(w, "C"+permissions, size, filename)
		if err != nil {
			errCh <- err
			return
		}

		_, err = io.Copy(w, bytes.NewReader(content))
		if err != nil {
			errCh <- err
			return
		}

		_, err = fmt.Fprint(w, "\x00")
		if err != nil {
			errCh <- err
			return
		}
	}()

	go func() {
		defer wg.Done()
		err := session.Run(fmt.Sprintf("scp -qt %s", directory))
		if err != nil {
			errCh <- err
			return
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// NewSSHAgent allows to return a new SSH Agent
func NewSSHAgent(ctx context.Context) (*SSHAgent, error) {
	bin, err := exec.LookPath("ssh-agent")
	if err != nil {
		return nil, errors.Wrap(err, "could not find ssh-agent")
	}

	cmd := executil.Command(ctx, bin)
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.Wrap(err, "failed to run ssh-agent")
	}

	fields := bytes.Split(out, []byte(";"))
	line := bytes.SplitN(fields[0], []byte("="), 2)
	line[0] = bytes.TrimLeft(line[0], "\n")
	if string(line[0]) != "SSH_AUTH_SOCK" {
		return nil, errors.Wrapf(err, "failed to retrieve SSH_AUTH_SOCK in %q", fields[0])
	}
	socket := string(line[1])

	line = bytes.SplitN(fields[2], []byte("="), 2)
	line[0] = bytes.TrimLeft(line[0], "\n")
	if string(line[0]) != "SSH_AGENT_PID" {
		return nil, errors.Wrapf(err, "failed to retrieve SSH_AGENT_PID in %q", fields[2])
	}
	pidStr := line[1]
	pid, err := strconv.Atoi(string(pidStr))
	if err != nil {
		return nil, errors.Wrapf(err, "unexpected format for ssh-agent pid:%q", pidStr)
	}

	conn, err := net.Dial("unix", string(socket))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dial with ssh-agent")
	}

	return &SSHAgent{
		agent.NewClient(conn),
		conn,
		socket,
		pid,
	}, nil
}

func (sa *SSHAgent) addKey(privateKey []byte, lifeTime uint32) error {
	rawKey, err := ssh.ParseRawPrivateKey(privateKey)
	if err != nil {
		return errors.Wrapf(err, "failed to parse raw private key")
	}

	addedKey := &agent.AddedKey{
		PrivateKey:   rawKey,
		LifetimeSecs: lifeTime,
	}
	return sa.agent.Add(*addedKey)
}

// AddKey allows to add a key into ssh-agent keys list
func (sa *SSHAgent) AddKey(privateKey string, lifeTime uint32) error {
	log.Debugf("Add key for SSH-AGENT")
	keyContent, err := ToPrivateKeyContent(privateKey)
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve private key content")
	}
	return sa.addKey(keyContent, lifeTime)
}

// AddPrivateKey allows to add a key into ssh-agent keys list
func (sa *SSHAgent) AddPrivateKey(privateKey *PrivateKey, lifeTime uint32) error {
	log.Debugf("Add key for SSH-AGENT")
	return sa.addKey(privateKey.Content, lifeTime)
}

// RemoveKey allows to remove a key into ssh-agent keys list
func (sa *SSHAgent) RemoveKey(privateKey string) error {
	keyContent, err := ToPrivateKeyContent(privateKey)
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve private key content")
	}

	rawKey, err := ssh.ParseRawPrivateKey(keyContent)
	if err != nil {
		return errors.Wrapf(err, "failed to parse raw private key")
	}

	signer, err := ssh.NewSignerFromKey(rawKey)
	if err != nil {
		return errors.Wrapf(err, "failed create new signer from key")
	}

	return sa.agent.Remove(signer.PublicKey())
}

// RemoveAllKeys allows to remove all keys into ssh-agent keys list
func (sa *SSHAgent) RemoveAllKeys() error {
	log.Debugf("Remove all keys for SSH-AGENT")
	return sa.agent.RemoveAll()
}

// Stop allows to cleanup and stop ssh-agent process
func (sa *SSHAgent) Stop() error {
	log.Debugf("Stop SSH-AGENT")
	proc, err := os.FindProcess(sa.pid)
	if err != nil {
		return errors.Wrapf(err, "failed to find ssh-agent process")
	}
	if proc != nil {
		proc.Kill()
	}
	if sa.conn != nil {
		err = sa.conn.Close()
		if err != nil {
			return errors.Wrapf(err, "failed to close ssh-agent connection")
		}
	}
	return os.RemoveAll(filepath.Dir(sa.Socket))
}

// GetAuthMethod returns the auth method with all agent keys
func (sa *SSHAgent) GetAuthMethod() ssh.AuthMethod {
	return ssh.PublicKeysCallback(sa.agent.Signers)
}
