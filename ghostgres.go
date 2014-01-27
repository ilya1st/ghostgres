// Copyright 2014, Surul Software Labs GmbH
// All rights reserved.

/*
Package ghostgres is a utility to start and control a PostgreSQL database.
The expected usage is in tests where it allows for easy startup and
shutdown of a database. The easiest way is to have Ghostgres build a template
from which it can clone a fresh database when you need one. In order to do this
run

	// Fetch the package
	go get -t github.com/surullabs/ghostgres

	// Run tests and create a default postgres cluster that will be used
	// as a template for future clusters.
	go test github.com/surullabs/ghostgres --ghostgres_pg_bin_dir=<path_to_your_postgres_bin_dir>

In your test code you can now use (with appropriate error checks)

	// Create a cloned cluster from the default template in a temporary directory
	cluster, err := ghostgres.FromDefault("")
	if err != nil {
		// fail
	}
	// Set a function which will be called on errors
	cluster.FailWith = t.Fatal // Or some other failure function
	// Start the postgres server
	cluster.Start()
	// Remember to stop it! This will delete the temporary directory.
	defer cluster.Stop()

	// Connect to the running postgres server through a unix socket.
	db, err := sql.Open("postgres", fmt.Sprintf("sslmode=disable dbname=postgres host=%s port=%d", cluster.SocketDir(), cluster.Port()))

Please consult the examples for other sample usage.
*/
package ghostgres

import (
	"fmt"
	surulio "github.com/surullabs/goutil/io"
	surultpl "github.com/surullabs/goutil/template"
	"io/ioutil"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"text/template"
	"time"
)

var postgresqlConfTemplate = template.Must(template.New("postgresql.conf").Parse(`# Auto Generated PostgreSQL Configuration
{{range $opt := $.Config}}
{{$opt.Key}} = {{$opt.Value}} {{if $opt.Comment}} # {{$opt.Comment}} {{end}}
{{end}}`))

// ConfigOpt represents a PostgreSQL configuration option
// It is used both to specify command line arguments as well
// as populate the postgresql.conf file.
type ConfigOpt struct {
	Key     string
	Value   string
	Comment string
}

// FailureHandler defines a function to be called when errors occur. Setting one
// makes using PostgresCluster easier in tests.
type FailureHandler func(...interface{})

// TestLogFileName is the file name to which PostgresSQL will
// log if TestConfigWithLogging is used. The path is relative to DataDir/pg_log
const TestLogFileName = "postgresql-tests.log"

// TestConfig provides some sane defaults for a cluster to be used in unit tests.
var TestConfig = []ConfigOpt{
	{"port", "5432", "Use the default port since we disable TCP listen"},
	{"listen_addresses", "''", "Do not listen on TCP. Instead use a unix domain socket for communication"},
	{"ssl", "false", "No ssl for unit tests"},
	{"shared_buffers", "10MB", "Smaller shared buffers to reduce resource usage"},
	{"fsync", "off", "Ignore system crashes, since tests will fail in that event anyway"},
	{"full_page_writes", "off", "Useless without fsync"},
}

// LoggingConfig provides useful defaults for logging in tests.
var LoggingConfig = []ConfigOpt{
	{"logging_collector", "on", "Collecting query logs can be useful to debug tests"},
	{"log_filename", TestLogFileName, "Well known file name to make log parsing easy in tests"},
	{"log_statement", "all", "Log all statements"},
}

// TestConfigWithLogging combines TestConfig and LoggingConfig
var TestConfigWithLogging = append(TestConfig, LoggingConfig...)

// PostgresCluster describes a single PostgreSQL cluster
type PostgresCluster struct {
	// Key value pairs used to create a postgresql.conf file. They are
	// written out as
	// 	key = value # comment
	Config []ConfigOpt
	// Directory in which to initialize the cluster.
	DataDir string
	// A set of options to be used when creating the cluster. These
	// will be passed directly to initdb. A example would be
	//  {"--auth", "trust", ""}, {"--nosync", "", ""} to enable easy testing.
	// For more details on the command line flags see
	// http://www.postgresql.org/docs/9.3/static/app-initdb.html
	InitOpts []ConfigOpt
	// A set of options to be used when running the postgres server.
	RunOpts []ConfigOpt
	// Directory containing postgres binaries
	BinDir string
	// The password for the super user
	Password string
	// Convenience when writing tests. All exported functions will call this
	// failure handler if it is not nil.
	FailWith FailureHandler `json:"-"`
	// The running postgres process
	proc *exec.Cmd
	// If not nil this handler is run after the database is stopped
	onStop func()
}

func makeArgs(opts []ConfigOpt) []string {
	args := make([]string, 0)
	for _, arg := range opts {
		args = append(args, arg.Key)
		if arg.Value != "" {
			args = append(args, arg.Value)
		}
	}
	return args
}

var tempDir = &surulio.SafeTempDirExecer{}

func (p *PostgresCluster) fail(err error) {
	if p.FailWith != nil && err != nil {
		p.FailWith(err)
	}
}

func (p *PostgresCluster) handleErrors(err error, panicked interface{}) error {
	// First check to see if we need to handle a panic
	err = handlePanic(err, panicked)
	// Now handle the error
	p.fail(err)
	return err
}

// Init will run initdb to create the cluster in the specified
// directory. Init will return an error if the directory contains
// an existing cluster. Use InitIfNeeded() to skip initialization
// of existing clusters.
//
// Please note that this can be time consuming and it
// is recommended that a golden version of a database is first
// initialized outside of the test system and then used as a
// source for cloning using Clone(string). A newly initialized
// cluster usually takes up about 33 MB of space. One potential
// option is to have the golden version be initialized in a location
// that will not be committed into a source repository. Use
// InitIfNeeded instead of Init and always use Clone(string) and
// only call Start() on the clone. This allows a single golden copy
// to be shared among multiple tests with fast start times.
func (p *PostgresCluster) Init() (err error) {
	defer func() { err = p.handleErrors(err, recover()) }()

	args := make([]ConfigOpt, len(p.InitOpts))
	copy(args, p.InitOpts)
	args = append(args, ConfigOpt{"--pgdata", p.DataDir, ""})

	checkErr(nil, tempDir.Exec("pg_init", func(dir string) error {
		passwordFile := filepath.Join(dir, "postgres_pass")
		checkErr(nil, ioutil.WriteFile(passwordFile, []byte(p.Password), 0600))

		args = append(args, ConfigOpt{"--pwfile", passwordFile, ""})
		initdb := exec.Command(filepath.Join(p.BinDir, "initdb"), makeArgs(args)...)
		checkCommand(initdb.CombinedOutput())
		return nil
	}))
	// Now write out the postgresql.conf
	return surultpl.WriteFile(p.configFile(), postgresqlConfTemplate, p, 0600)
}

// InitIfNeeded calls Init() if a call to Initialized returns false.
func (p *PostgresCluster) InitIfNeeded() (err error) {
	if !p.Initialized() {
		err = p.Init()
	}
	return
}

func (p *PostgresCluster) configFile() string { return filepath.Join(p.DataDir, "postgresql.conf") }

// Port attempts to parse a port from the provided config options
// and returns the parsed port or 5432 if there is a failure or no
// port is specified. If there is a failure the FailWith will
// be called.
func (p *PostgresCluster) Port() (portVal int) {
	port := "5432"
	for _, opt := range p.Config {
		if opt.Key == "port" {
			port = opt.Value
			break
		}
	}
	var err error
	if portVal, err = strconv.Atoi(port); err != nil {
		// The port is invalid, falling back to 5432
		p.fail(err)
		return 5432
	}
	return portVal
}

// SocketDir returns the location of the postgres unix socket directory
func (p *PostgresCluster) SocketDir() string { return p.DataDir }

// SocketFile returns the location of the postgres socket file
func (p *PostgresCluster) SocketFile() string {
	return filepath.Join(p.SocketDir(), fmt.Sprintf(".s.PGSQL.%d", p.Port()))
}

// Initialized checks if a cluster has been initialized in the data directory.
// It uses the existence of the postgresql.conf file as a signal that the
// cluster has been initialized.
func (p *PostgresCluster) Initialized() bool {
	if exists, err := surulio.Exists(p.configFile()); exists && err == nil {
		return true
	}
	return false
}

// WaitTillRunning waits for a duration of timeout for the postgres server to start.
// It must be called after a call to Start() and before a call to Stop() or Wait()
// It polls for the existence of the socket file every 10ms to detect if the server
// is running and accessible and will return an error if it cannot detect the
// server within timeout.
func (p *PostgresCluster) WaitTillRunning(timeout time.Duration) (err error) {
	defer func() { err = p.handleErrors(err, recover()) }()
	check(p.proc != nil, "server has not been started")
	return surulio.WaitTillExists(p.SocketFile(), 10*time.Millisecond, timeout)
}

// Running will return true if the server is running. Please note that this is still
// not very accurate as it merely checks if the server has been started.
func (p *PostgresCluster) Running() bool {
	// TODO: Run the process in a separate goroutine and make this more robust.
	return p.proc != nil
}

// Start starts the postgres database. It will add the following extra flags in addition
// to the RunOpts provided.
//
//	-D p.DataDir  // Use the specified data directory
//	-k p.DataDir  // Use the data directory as the socket directory for unix sockets.
//	-c config_file=p.DataDir/postgresql.confg // Custom config file.
//
// It does not attempt to read the config file to determine the data directory or the
// socket directory.
func (p *PostgresCluster) Start() (err error) {
	defer func() { err = p.handleErrors(err, recover()) }()
	check(p.Initialized(), "postgres cluster not initialized")
	check(!p.Running(), "postgres cluster already running")

	args := make([]ConfigOpt, len(p.RunOpts))
	copy(args, p.RunOpts)
	args = append(args, ConfigOpt{"-D", p.DataDir, ""})
	args = append(args, ConfigOpt{"-k", p.DataDir, ""})
	args = append(args, ConfigOpt{"-c", fmt.Sprintf("config_file=%s", p.configFile()), ""})
	proc := exec.Command(filepath.Join(p.BinDir, "postgres"), makeArgs(args)...)
	checkErr(nil, proc.Start())
	p.proc = proc
	return
}

// Clone clones a previous postgres database by copying the entire directory
// This currently only works on systems which have a cp command. This
// will not work if the destination directory exists.
func (p *PostgresCluster) Clone(dest string) (c *PostgresCluster, err error) {
	defer func() { err = p.handleErrors(err, recover()) }()
	check(!p.Running(), "cannot clone a running cluster")
	check(p.Initialized(), "cluster must be initialized before cloning")
	check(!checkErr(surulio.Exists(dest)).(bool), "cannot clone into an existing directory")
	checkCommand(exec.Command("cp", "-r", p.DataDir, dest).CombinedOutput())
	cloned := *p
	cloned.DataDir = dest
	return &cloned, nil
}

// Wait waits for a running postgres server to terminate. It is useful when you wish to freeze a test
// and inspect the database. Once it is frozen it can be stopped using
//
//	pg_ctl -D p.DataDir stop
//
// It will return an error if the server exits with any return code other than 0. It is an error
// to call this before calling Start.
func (p *PostgresCluster) Wait() (err error) {
	defer func() { err = p.handleErrors(err, recover()) }()
	check(p.Running(), "postgres cluster not running")
	defer func() { p.proc = nil }()
	err = p.proc.Wait()
	return
}

// Stop stops the postgres cluster if it is running by sending it a SIGTERM signal.
// This will request a slow shutdown and the postgres server will wait for all existing
// connections to close. It is an error to call this if the server is not running.
func (p *PostgresCluster) Stop() (err error) {
	defer func() { err = p.handleErrors(err, recover()) }()
	defer func() {
		if p.onStop != nil {
			p.onStop()
		}
	}()
	if !p.Running() {
		return
	}
	p.proc.Process.Signal(syscall.SIGTERM)
	if err = p.Wait(); err != nil && err.Error() == "signal: terminated" {
		err = nil
	}
	return
}
