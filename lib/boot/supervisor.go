// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package boot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"git.arvados.org/arvados.git/lib/service"
	"git.arvados.org/arvados.git/sdk/go/arvados"
	"git.arvados.org/arvados.git/sdk/go/ctxlog"
	"git.arvados.org/arvados.git/sdk/go/health"
	"github.com/sirupsen/logrus"
)

type Supervisor struct {
	SourcePath           string // e.g., /home/username/src/arvados
	SourceVersion        string // e.g., acbd1324...
	ClusterType          string // e.g., production
	ListenHost           string // e.g., localhost
	ControllerAddr       string // e.g., 127.0.0.1:8000
	OwnTemporaryDatabase bool
	Stderr               io.Writer

	logger  logrus.FieldLogger
	cluster *arvados.Cluster

	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	healthChecker *health.Aggregator
	tasksReady    map[string]chan bool
	waitShutdown  sync.WaitGroup

	tempdir    string
	configfile string
	environ    []string // for child processes
}

func (super *Supervisor) Start(ctx context.Context, cfg *arvados.Config) {
	super.ctx, super.cancel = context.WithCancel(ctx)
	super.done = make(chan struct{})

	go func() {
		sigch := make(chan os.Signal)
		signal.Notify(sigch, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigch)
		go func() {
			for sig := range sigch {
				super.logger.WithField("signal", sig).Info("caught signal")
				super.cancel()
			}
		}()

		err := super.run(cfg)
		if err != nil {
			super.logger.WithError(err).Warn("supervisor shut down")
		}
		close(super.done)
	}()
}

func (super *Supervisor) run(cfg *arvados.Config) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(super.SourcePath, "/") {
		super.SourcePath = filepath.Join(cwd, super.SourcePath)
	}
	super.SourcePath, err = filepath.EvalSymlinks(super.SourcePath)
	if err != nil {
		return err
	}

	super.tempdir, err = ioutil.TempDir("", "arvados-server-boot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(super.tempdir)
	if err := os.Mkdir(filepath.Join(super.tempdir, "bin"), 0755); err != nil {
		return err
	}

	// Fill in any missing config keys, and write the resulting
	// config in the temp dir for child services to use.
	err = super.autofillConfig(cfg)
	if err != nil {
		return err
	}
	conffile, err := os.OpenFile(filepath.Join(super.tempdir, "config.yml"), os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer conffile.Close()
	err = json.NewEncoder(conffile).Encode(cfg)
	if err != nil {
		return err
	}
	err = conffile.Close()
	if err != nil {
		return err
	}
	super.configfile = conffile.Name()

	super.environ = os.Environ()
	super.cleanEnv([]string{"ARVADOS_"})
	super.setEnv("ARVADOS_CONFIG", super.configfile)
	super.setEnv("RAILS_ENV", super.ClusterType)
	super.setEnv("TMPDIR", super.tempdir)
	super.prependEnv("PATH", filepath.Join(super.tempdir, "bin")+":")

	super.cluster, err = cfg.GetCluster("")
	if err != nil {
		return err
	}
	// Now that we have the config, replace the bootstrap logger
	// with a new one according to the logging config.
	loglevel := super.cluster.SystemLogs.LogLevel
	if s := os.Getenv("ARVADOS_DEBUG"); s != "" && s != "0" {
		loglevel = "debug"
	}
	super.logger = ctxlog.New(super.Stderr, super.cluster.SystemLogs.Format, loglevel).WithFields(logrus.Fields{
		"PID": os.Getpid(),
	})

	if super.SourceVersion == "" {
		// Find current source tree version.
		var buf bytes.Buffer
		err = super.RunProgram(super.ctx, ".", &buf, nil, "git", "diff", "--shortstat")
		if err != nil {
			return err
		}
		dirty := buf.Len() > 0
		buf.Reset()
		err = super.RunProgram(super.ctx, ".", &buf, nil, "git", "log", "-n1", "--format=%H")
		if err != nil {
			return err
		}
		super.SourceVersion = strings.TrimSpace(buf.String())
		if dirty {
			super.SourceVersion += "+uncommitted"
		}
	} else {
		return errors.New("specifying a version to run is not yet supported")
	}

	_, err = super.installGoProgram(super.ctx, "cmd/arvados-server")
	if err != nil {
		return err
	}
	err = super.setupRubyEnv()
	if err != nil {
		return err
	}

	tasks := []supervisedTask{
		createCertificates{},
		runPostgreSQL{},
		runNginx{},
		runServiceCommand{name: "controller", svc: super.cluster.Services.Controller, depends: []supervisedTask{runPostgreSQL{}}},
		runGoProgram{src: "services/arv-git-httpd", svc: super.cluster.Services.GitHTTP},
		runGoProgram{src: "services/health", svc: super.cluster.Services.Health},
		runGoProgram{src: "services/keepproxy", svc: super.cluster.Services.Keepproxy, depends: []supervisedTask{runPassenger{src: "services/api"}}},
		runGoProgram{src: "services/keepstore", svc: super.cluster.Services.Keepstore},
		runGoProgram{src: "services/keep-web", svc: super.cluster.Services.WebDAV},
		runServiceCommand{name: "ws", svc: super.cluster.Services.Websocket, depends: []supervisedTask{runPostgreSQL{}}},
		installPassenger{src: "services/api"},
		runPassenger{src: "services/api", svc: super.cluster.Services.RailsAPI, depends: []supervisedTask{createCertificates{}, runPostgreSQL{}, installPassenger{src: "services/api"}}},
		installPassenger{src: "apps/workbench", depends: []supervisedTask{installPassenger{src: "services/api"}}}, // dependency ensures workbench doesn't delay api startup
		runPassenger{src: "apps/workbench", svc: super.cluster.Services.Workbench1, depends: []supervisedTask{installPassenger{src: "apps/workbench"}}},
		seedDatabase{},
	}
	if super.ClusterType != "test" {
		tasks = append(tasks,
			runServiceCommand{name: "dispatch-cloud", svc: super.cluster.Services.Controller},
			runGoProgram{src: "services/keep-balance"},
		)
	}
	super.tasksReady = map[string]chan bool{}
	for _, task := range tasks {
		super.tasksReady[task.String()] = make(chan bool)
	}
	for _, task := range tasks {
		task := task
		fail := func(err error) {
			if super.ctx.Err() != nil {
				return
			}
			super.cancel()
			super.logger.WithField("task", task.String()).WithError(err).Error("task failed")
		}
		go func() {
			super.logger.WithField("task", task.String()).Info("starting")
			err := task.Run(super.ctx, fail, super)
			if err != nil {
				fail(err)
				return
			}
			close(super.tasksReady[task.String()])
		}()
	}
	err = super.wait(super.ctx, tasks...)
	if err != nil {
		return err
	}
	super.logger.Info("all startup tasks are complete; starting health checks")
	super.healthChecker = &health.Aggregator{Cluster: super.cluster}
	<-super.ctx.Done()
	super.logger.Info("shutting down")
	super.waitShutdown.Wait()
	return super.ctx.Err()
}

func (super *Supervisor) wait(ctx context.Context, tasks ...supervisedTask) error {
	for _, task := range tasks {
		ch, ok := super.tasksReady[task.String()]
		if !ok {
			return fmt.Errorf("no such task: %s", task)
		}
		super.logger.WithField("task", task.String()).Info("waiting")
		select {
		case <-ch:
			super.logger.WithField("task", task.String()).Info("ready")
		case <-ctx.Done():
			super.logger.WithField("task", task.String()).Info("task was never ready")
			return ctx.Err()
		}
	}
	return nil
}

func (super *Supervisor) Stop() {
	super.cancel()
	<-super.done
}

func (super *Supervisor) WaitReady() (*arvados.URL, bool) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for waiting := "all"; waiting != ""; {
		select {
		case <-ticker.C:
		case <-super.ctx.Done():
			return nil, false
		}
		if super.healthChecker == nil {
			// not set up yet
			continue
		}
		resp := super.healthChecker.ClusterHealth()
		// The overall health check (resp.Health=="OK") might
		// never pass due to missing components (like
		// arvados-dispatch-cloud in a test cluster), so
		// instead we wait for all configured components to
		// pass.
		waiting = ""
		for target, check := range resp.Checks {
			if check.Health != "OK" {
				waiting += " " + target
			}
		}
		if waiting != "" {
			super.logger.WithField("targets", waiting[1:]).Info("waiting")
		}
	}
	u := super.cluster.Services.Controller.ExternalURL
	return &u, true
}

func (super *Supervisor) prependEnv(key, prepend string) {
	for i, s := range super.environ {
		if strings.HasPrefix(s, key+"=") {
			super.environ[i] = key + "=" + prepend + s[len(key)+1:]
			return
		}
	}
	super.environ = append(super.environ, key+"="+prepend)
}

func (super *Supervisor) cleanEnv(prefixes []string) {
	var cleaned []string
	for _, s := range super.environ {
		drop := false
		for _, p := range prefixes {
			if strings.HasPrefix(s, p) {
				drop = true
				break
			}
		}
		if !drop {
			cleaned = append(cleaned, s)
		}
	}
	super.environ = cleaned
}

func (super *Supervisor) setEnv(key, val string) {
	for i, s := range super.environ {
		if strings.HasPrefix(s, key+"=") {
			super.environ[i] = key + "=" + val
			return
		}
	}
	super.environ = append(super.environ, key+"="+val)
}

// Remove all but the first occurrence of each env var.
func dedupEnv(in []string) []string {
	saw := map[string]bool{}
	var out []string
	for _, kv := range in {
		if split := strings.Index(kv, "="); split < 1 {
			panic("invalid environment var: " + kv)
		} else if saw[kv[:split]] {
			continue
		} else {
			saw[kv[:split]] = true
			out = append(out, kv)
		}
	}
	return out
}

func (super *Supervisor) installGoProgram(ctx context.Context, srcpath string) (string, error) {
	_, basename := filepath.Split(srcpath)
	bindir := filepath.Join(super.tempdir, "bin")
	binfile := filepath.Join(bindir, basename)
	err := super.RunProgram(ctx, filepath.Join(super.SourcePath, srcpath), nil, []string{"GOBIN=" + bindir}, "go", "install", "-ldflags", "-X git.arvados.org/arvados.git/lib/cmd.version="+super.SourceVersion+" -X main.version="+super.SourceVersion)
	return binfile, err
}

func (super *Supervisor) usingRVM() bool {
	return os.Getenv("rvm_path") != ""
}

func (super *Supervisor) setupRubyEnv() error {
	if !super.usingRVM() {
		// (If rvm is in use, assume the caller has everything
		// set up as desired)
		super.cleanEnv([]string{
			"GEM_HOME=",
			"GEM_PATH=",
		})
		cmd := exec.Command("gem", "env", "gempath")
		cmd.Env = super.environ
		buf, err := cmd.Output() // /var/lib/arvados/.gem/ruby/2.5.0/bin:...
		if err != nil || len(buf) == 0 {
			return fmt.Errorf("gem env gempath: %v", err)
		}
		gempath := string(bytes.Split(buf, []byte{':'})[0])
		super.prependEnv("PATH", gempath+"/bin:")
		super.setEnv("GEM_HOME", gempath)
		super.setEnv("GEM_PATH", gempath)
	}
	// Passenger install doesn't work unless $HOME is ~user
	u, err := user.Current()
	if err != nil {
		return err
	}
	super.setEnv("HOME", u.HomeDir)
	return nil
}

func (super *Supervisor) lookPath(prog string) string {
	for _, val := range super.environ {
		if strings.HasPrefix(val, "PATH=") {
			for _, dir := range filepath.SplitList(val[5:]) {
				path := filepath.Join(dir, prog)
				if fi, err := os.Stat(path); err == nil && fi.Mode()&0111 != 0 {
					return path
				}
			}
		}
	}
	return prog
}

// Run prog with args, using dir as working directory. If ctx is
// cancelled while the child is running, RunProgram terminates the
// child, waits for it to exit, then returns.
//
// Child's environment will have our env vars, plus any given in env.
//
// Child's stdout will be written to output if non-nil, otherwise the
// boot command's stderr.
func (super *Supervisor) RunProgram(ctx context.Context, dir string, output io.Writer, env []string, prog string, args ...string) error {
	cmdline := fmt.Sprintf("%s", append([]string{prog}, args...))
	super.logger.WithField("command", cmdline).WithField("dir", dir).Info("executing")

	logprefix := strings.TrimPrefix(prog, super.tempdir+"/bin/")
	if logprefix == "bundle" && len(args) > 2 && args[0] == "exec" {
		logprefix = args[1]
	} else if logprefix == "arvados-server" && len(args) > 1 {
		logprefix = args[0]
	}
	if !strings.HasPrefix(dir, "/") {
		logprefix = dir + ": " + logprefix
	}

	cmd := exec.Command(super.lookPath(prog), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	logwriter := &service.LogPrefixer{Writer: super.Stderr, Prefix: []byte("[" + logprefix + "] ")}
	var copiers sync.WaitGroup
	copiers.Add(1)
	go func() {
		io.Copy(logwriter, stderr)
		copiers.Done()
	}()
	copiers.Add(1)
	go func() {
		if output == nil {
			io.Copy(logwriter, stdout)
		} else {
			io.Copy(output, stdout)
		}
		copiers.Done()
	}()

	if strings.HasPrefix(dir, "/") {
		cmd.Dir = dir
	} else {
		cmd.Dir = filepath.Join(super.SourcePath, dir)
	}
	env = append([]string(nil), env...)
	env = append(env, super.environ...)
	cmd.Env = dedupEnv(env)

	exited := false
	defer func() { exited = true }()
	go func() {
		<-ctx.Done()
		log := ctxlog.FromContext(ctx).WithFields(logrus.Fields{"dir": dir, "cmdline": cmdline})
		for !exited {
			if cmd.Process == nil {
				log.Debug("waiting for child process to start")
				time.Sleep(time.Second / 2)
			} else {
				log.WithField("PID", cmd.Process.Pid).Debug("sending SIGTERM")
				cmd.Process.Signal(syscall.SIGTERM)
				time.Sleep(5 * time.Second)
				if !exited {
					stdout.Close()
					stderr.Close()
					log.WithField("PID", cmd.Process.Pid).Warn("still waiting for child process to exit 5s after SIGTERM")
				}
			}
		}
	}()

	err = cmd.Start()
	if err != nil {
		return err
	}
	copiers.Wait()
	err = cmd.Wait()
	if ctx.Err() != nil {
		// Return "context canceled", instead of the "killed"
		// error that was probably caused by the context being
		// canceled.
		return ctx.Err()
	} else if err != nil {
		return fmt.Errorf("%s: error: %v", cmdline, err)
	}
	return nil
}

func (super *Supervisor) autofillConfig(cfg *arvados.Config) error {
	cluster, err := cfg.GetCluster("")
	if err != nil {
		return err
	}
	usedPort := map[string]bool{}
	nextPort := func(host string) string {
		for {
			port, err := availablePort(host)
			if err != nil {
				panic(err)
			}
			if usedPort[port] {
				continue
			}
			usedPort[port] = true
			return port
		}
	}
	if cluster.Services.Controller.ExternalURL.Host == "" {
		h, p, err := net.SplitHostPort(super.ControllerAddr)
		if err != nil {
			return err
		}
		if h == "" {
			h = super.ListenHost
		}
		if p == "0" {
			p = nextPort(h)
		}
		cluster.Services.Controller.ExternalURL = arvados.URL{Scheme: "https", Host: net.JoinHostPort(h, p)}
	}
	for _, svc := range []*arvados.Service{
		&cluster.Services.Controller,
		&cluster.Services.DispatchCloud,
		&cluster.Services.GitHTTP,
		&cluster.Services.Health,
		&cluster.Services.Keepproxy,
		&cluster.Services.Keepstore,
		&cluster.Services.RailsAPI,
		&cluster.Services.WebDAV,
		&cluster.Services.WebDAVDownload,
		&cluster.Services.Websocket,
		&cluster.Services.Workbench1,
	} {
		if svc == &cluster.Services.DispatchCloud && super.ClusterType == "test" {
			continue
		}
		if svc.ExternalURL.Host == "" {
			if svc == &cluster.Services.Controller ||
				svc == &cluster.Services.GitHTTP ||
				svc == &cluster.Services.Keepproxy ||
				svc == &cluster.Services.WebDAV ||
				svc == &cluster.Services.WebDAVDownload ||
				svc == &cluster.Services.Workbench1 {
				svc.ExternalURL = arvados.URL{Scheme: "https", Host: fmt.Sprintf("%s:%s", super.ListenHost, nextPort(super.ListenHost))}
			} else if svc == &cluster.Services.Websocket {
				svc.ExternalURL = arvados.URL{Scheme: "wss", Host: fmt.Sprintf("%s:%s", super.ListenHost, nextPort(super.ListenHost))}
			}
		}
		if len(svc.InternalURLs) == 0 {
			svc.InternalURLs = map[arvados.URL]arvados.ServiceInstance{
				arvados.URL{Scheme: "http", Host: fmt.Sprintf("%s:%s", super.ListenHost, nextPort(super.ListenHost))}: arvados.ServiceInstance{},
			}
		}
	}
	if cluster.SystemRootToken == "" {
		cluster.SystemRootToken = randomHexString(64)
	}
	if cluster.ManagementToken == "" {
		cluster.ManagementToken = randomHexString(64)
	}
	if cluster.API.RailsSessionSecretToken == "" {
		cluster.API.RailsSessionSecretToken = randomHexString(64)
	}
	if cluster.Collections.BlobSigningKey == "" {
		cluster.Collections.BlobSigningKey = randomHexString(64)
	}
	if super.ClusterType != "production" && cluster.Containers.DispatchPrivateKey == "" {
		buf, err := ioutil.ReadFile(filepath.Join(super.SourcePath, "lib", "dispatchcloud", "test", "sshkey_dispatch"))
		if err != nil {
			return err
		}
		cluster.Containers.DispatchPrivateKey = string(buf)
	}
	if super.ClusterType != "production" {
		cluster.TLS.Insecure = true
	}
	if super.ClusterType == "test" {
		// Add a second keepstore process.
		cluster.Services.Keepstore.InternalURLs[arvados.URL{Scheme: "http", Host: fmt.Sprintf("%s:%s", super.ListenHost, nextPort(super.ListenHost))}] = arvados.ServiceInstance{}

		// Create a directory-backed volume for each keepstore
		// process.
		cluster.Volumes = map[string]arvados.Volume{}
		for url := range cluster.Services.Keepstore.InternalURLs {
			volnum := len(cluster.Volumes)
			datadir := fmt.Sprintf("%s/keep%d.data", super.tempdir, volnum)
			if _, err = os.Stat(datadir + "/."); err == nil {
			} else if !os.IsNotExist(err) {
				return err
			} else if err = os.Mkdir(datadir, 0755); err != nil {
				return err
			}
			cluster.Volumes[fmt.Sprintf(cluster.ClusterID+"-nyw5e-%015d", volnum)] = arvados.Volume{
				Driver:           "Directory",
				DriverParameters: json.RawMessage(fmt.Sprintf(`{"Root":%q}`, datadir)),
				AccessViaHosts: map[arvados.URL]arvados.VolumeAccess{
					url: {},
				},
			}
		}
	}
	if super.OwnTemporaryDatabase {
		cluster.PostgreSQL.Connection = arvados.PostgreSQLConnection{
			"client_encoding": "utf8",
			"host":            "localhost",
			"port":            nextPort(super.ListenHost),
			"dbname":          "arvados_test",
			"user":            "arvados",
			"password":        "insecure_arvados_test",
		}
	}

	cfg.Clusters[cluster.ClusterID] = *cluster
	return nil
}

func addrIsLocal(addr string) (bool, error) {
	return true, nil
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		listener.Close()
		return true, nil
	} else if strings.Contains(err.Error(), "cannot assign requested address") {
		return false, nil
	} else {
		return false, err
	}
}

func randomHexString(chars int) string {
	b := make([]byte, chars/2)
	_, err := rand.Read(b)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

func internalPort(svc arvados.Service) (string, error) {
	if len(svc.InternalURLs) > 1 {
		return "", errors.New("internalPort() doesn't work with multiple InternalURLs")
	}
	for u := range svc.InternalURLs {
		if _, p, err := net.SplitHostPort(u.Host); err != nil {
			return "", err
		} else if p != "" {
			return p, nil
		} else if u.Scheme == "https" {
			return "443", nil
		} else {
			return "80", nil
		}
	}
	return "", fmt.Errorf("service has no InternalURLs")
}

func externalPort(svc arvados.Service) (string, error) {
	if _, p, err := net.SplitHostPort(svc.ExternalURL.Host); err != nil {
		return "", err
	} else if p != "" {
		return p, nil
	} else if svc.ExternalURL.Scheme == "https" {
		return "443", nil
	} else {
		return "80", nil
	}
}

func availablePort(host string) (string, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return "", err
	}
	return port, nil
}

// Try to connect to addr until it works, then close ch. Give up if
// ctx cancels.
func waitForConnect(ctx context.Context, addr string) error {
	dialer := net.Dialer{Timeout: time.Second}
	for ctx.Err() == nil {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			time.Sleep(time.Second / 10)
			continue
		}
		conn.Close()
		return nil
	}
	return ctx.Err()
}
