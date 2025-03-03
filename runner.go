package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/consul-template/child"
	"github.com/hashicorp/consul-template/config"
	dep "github.com/hashicorp/consul-template/dependency"
	"github.com/hashicorp/consul-template/watch"
	"github.com/pkg/errors"
)

// InvalidRegexp is a regexp for invalid characters in keys
var InvalidRegexp = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// Runner executes a given child process with configuration
type Runner struct {
	// ErrCh and DoneCh are channels where errors and finish notifications occur.
	ErrCh  chan error
	DoneCh chan struct{}

	// ExitCh is a channel for parent processes to read exit status values from
	// the child processes.
	ExitCh chan int

	// child is the child process under management. This may be nil if not running
	// in exec mode.
	child *child.Child

	// childLock is the internal lock around the child process.
	childLock sync.RWMutex

	// config is the Config that created this Runner. It is used internally to
	// construct other objects and pass data.
	config *Config

	// configPrefixMap is a map of a dependency's hashcode back to the config
	// prefix that created it.
	configPrefixMap map[string]*PrefixConfig

	configServiceMap map[string]*ServiceConfig

	// data is the latest representation of the data from Consul.
	data map[string]interface{}

	// dependencies is the list of dependencies this runner is watching.
	dependencies []dep.Dependency

	// dependenciesLock is a lock around touching the dependencies map.
	dependenciesLock sync.Mutex

	// env is the last compiled environment.
	env map[string]string

	// once indicates the runner should get data exactly one time and then stop.
	once bool

	// outStream and errStream are the io.Writer streams where the runner will
	// write information.
	//
	// inStream is the ioReader where the runner will read information.
	outStream, errStream io.Writer
	inStream             io.Reader

	// minTimer and maxTimer are used for quiescence.
	minTimer, maxTimer <-chan time.Time

	// stopLock is the lock around checking if the runner can be stopped
	stopLock sync.Mutex

	// stopped is a boolean of whether the runner is stopped
	stopped bool

	// watcher is the watcher this runner is using.
	watcher *watch.Watcher
	// dedicated token watcher
	vaultTokenWatcher *watch.Watcher
}

// NewRunner accepts a config, command, and boolean value for once mode.
func NewRunner(config *Config, once bool) (*Runner, error) {
	namedLogger("runner").Info("creating new runner", "once:", once)

	runner := &Runner{
		config:           config,
		once:             once,
		data:             make(map[string]interface{}),
		configPrefixMap:  make(map[string]*PrefixConfig),
		configServiceMap: make(map[string]*ServiceConfig),
		inStream:         os.Stdin,
		outStream:        os.Stdout,
		errStream:        os.Stderr,
		ErrCh:            make(chan error),
		DoneCh:           make(chan struct{}),
		ExitCh:           make(chan int, 1),
	}

	// Create the clientset
	clients, err := newClientSet(config)
	if err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}

	// needs to be run early to do initial token handling
	runner.vaultTokenWatcher, err = watch.VaultTokenWatcher(
		clients, config.Vault, runner.DoneCh)
	if err != nil {
		return nil, err
	}

	if err := runner.init(clients); err != nil {
		return nil, err
	}

	return runner, nil
}

// Start creates a new runner and begins watching dependencies and quiescence
// timers. This is the main event loop and will block until finished.
func (r *Runner) Start() {
	logger := namedLogger("runner")
	logger.Info("starting")

	// Create the pid before doing anything.
	if err := r.storePid(); err != nil {
		r.ErrCh <- err
		return
	}

	// Add each dependency to the watcher
	for _, d := range r.dependencies {
		r.watcher.Add(d)
	}

	var exitCh <-chan int

	for {
		select {
		case data := <-r.watcher.DataCh():
			r.Receive(data.Dependency(), data.Data())

			// Drain all views that have data
		OUTER:
			for {
				select {
				case data = <-r.watcher.DataCh():
					r.Receive(data.Dependency(), data.Data())
				default:
					break OUTER
				}
			}

			// If we are waiting for quiescence, setup the timers
			if config.BoolVal(r.config.Wait.Enabled) {
				logger.Info("quiescence timers starting")
				r.minTimer = time.After(config.TimeDurationVal(r.config.Wait.Min))
				if r.maxTimer == nil {
					r.maxTimer = time.After(config.TimeDurationVal(r.config.Wait.Max))
				}
				continue
			}
		case <-r.minTimer:
			logger.Info("quiescence minTimer fired")
			r.minTimer, r.maxTimer = nil, nil
		case <-r.maxTimer:
			logger.Info("quiescence maxTimer fired")
			r.minTimer, r.maxTimer = nil, nil
		case err := <-r.watcher.ErrCh():
			// Intentionally do not send the error back up to the runner.
			// Eventually, once Consul API implements errwrap and multierror,
			// we can check the "type" of error and conditionally alert back.
			//
			// if err.Contains(Something) {
			//   errCh <- err
			// }
			logger.Error("watcher reported error:", err)
			if r.once {
				r.ErrCh <- err
				return
			}
		case err := <-r.vaultTokenWatcher.ErrCh():
			// follow same pattern as primary watcher
			logger.Error("vault watcher reported error:", err)
			if r.once {
				r.ErrCh <- err
				return
			}
		case code := <-exitCh:
			r.ExitCh <- code
		case <-r.DoneCh:
			logger.Info("received finish")
			return
		}

		// If we got this far, that means we got new data or one of the timers
		// fired, so attempt to re-process the environment.
		nexitCh, err := r.Run()
		if err != nil {
			r.ErrCh <- err
			return
		}

		// It's possible that we didn't start a process, in which case no exitCh
		// is returned. In this case, we should assume our current process is still
		// running and chug along. If we did get a new exitCh, that means a new
		// process is spawned, so we need to watch a new exitCh.
		if nexitCh != nil {
			exitCh = nexitCh
		}
	}
}

// Stop halts the execution of this runner and its subprocesses.
func (r *Runner) Stop() {
	r.stopLock.Lock()
	defer r.stopLock.Unlock()

	if r.stopped {
		return
	}

	logger := namedLogger("runner")
	logger.Info("stopping")
	r.stopWatchers()
	r.stopChild()

	if err := r.deletePid(); err != nil {
		logger.Warn(fmt.Sprintf("could not remove pid at %#v: %s",
			r.config.PidFile, err))
	}

	r.stopped = true

	close(r.DoneCh)
}

// Receive accepts data from and maps that data to the prefix.
func (r *Runner) Receive(d dep.Dependency, data interface{}) {
	r.dependenciesLock.Lock()
	defer r.dependenciesLock.Unlock()
	namedLogger("runner").Debug("receiving dependency", d.String())
	r.data[d.String()] = data
}

// Signal sends a signal to the child process, if it exists. Any errors that
// occur are returned.
func (r *Runner) Signal(s os.Signal) error {
	r.childLock.RLock()
	defer r.childLock.RUnlock()
	if r.child == nil {
		return nil
	}
	return r.child.Signal(s)
}

// Run executes and manages the child process with the correct environment. The
// current environment is also copied into the child process environment.
func (r *Runner) Run() (<-chan int, error) {
	logger := namedLogger("runner")
	logger.Info("running")

	env := make(map[string]string)

	// Iterate over each dependency and pull out its data. If any dependencies do
	// not have data yet, this function will immediately return because we cannot
	// safely continue until all dependencies have received data at least once.
	//
	// We iterate over the list of config prefixes so that order is maintained,
	// since order in a map is not deterministic.
	r.dependenciesLock.Lock()
	defer r.dependenciesLock.Unlock()
	for _, d := range r.dependencies {
		data, ok := r.data[d.String()]
		if !ok {
			logger.Info("missing data for", d)
			return nil, nil
		}

		switch typed := d.(type) {
		case *dep.KVListQuery:
			r.appendPrefixes(env, typed, data)
		case *dep.VaultReadQuery:
			r.appendSecrets(env, typed, data)
		case *dep.CatalogServiceQuery:
			r.appendServices(env, typed, data)
		default:
			return nil, fmt.Errorf("unknown dependency type %T", typed)
		}
	}

	// Print the final environment
	logger.Trace("Environment:")
	for k, v := range env {
		logger.Trace(fmt.Sprintf("%s=%q", k, v))
	}

	// If the resulting map is the same, do not do anything. We use a length
	// check first to get a small performance increase if something has changed
	// so we don't immediately delegate to reflect which is slow.
	if len(r.env) == len(env) && reflect.DeepEqual(r.env, env) {
		logger.Info("environment was the same")
		return nil, nil
	}

	// Update the environment
	r.env = env

	if r.child != nil {
		logger.Info("stopping existing child process")
		r.stopChild()
	}

	// Create a new environment
	newEnv := make(map[string]string)

	// If we are not pristine, copy over all values in the current env.
	if !config.BoolVal(r.config.Pristine) {
		for _, v := range os.Environ() {
			list := strings.SplitN(v, "=", 2)
			newEnv[list[0]] = list[1]
		}
	}

	// Add our custom values, overwriting any existing ones.
	for k, v := range r.env {
		newEnv[k] = v
	}

	filteredEnv := r.applyConfigEnv(newEnv)

	// Prepare the final environment. Note that it's CRUCIAL for us to
	// initialize this slice to an empty one vs. a nil one, since that's
	// how the child process class decides whether to pull in the parent's
	// environment or not, and we control that via -pristine.
	cmdEnv := make([]string, 0)
	for k, v := range filteredEnv {
		cmdEnv = append(cmdEnv, fmt.Sprintf("%s=%s", k, v))
	}

	args, subshell, err := child.CommandPrep(r.config.Exec.Command)
	if err != nil {
		return nil, errors.Wrap(err, "parsing command")
	}
	child, err := child.New(&child.NewInput{
		Stdin:        r.inStream,
		Stdout:       r.outStream,
		Stderr:       r.errStream,
		Command:      args[0],
		Args:         args[1:],
		Env:          cmdEnv,
		Timeout:      0, // Allow running indefinitely
		ReloadSignal: config.SignalVal(r.config.Exec.ReloadSignal),
		KillSignal:   config.SignalVal(r.config.Exec.KillSignal),
		KillTimeout:  config.TimeDurationVal(r.config.Exec.KillTimeout),
		Splay:        config.TimeDurationVal(r.config.Exec.Splay),
		Setpgid:      subshell, // only setpgid for 'sh -c' subshell calls
	})
	if err != nil {
		return nil, errors.Wrap(err, "spawning child")
	}
	if err := child.Start(); err != nil {
		return nil, errors.Wrap(err, "starting child")
	}
	r.child = child

	return child.ExitCh(), nil
}

func applyFormatTemplate(contents, key string) (string, error) {
	funcs := template.FuncMap{
		"key": func() (string, error) {
			return key, nil
		},
		"replaceKey": replaceKey,
	}

	tmpl, err := template.New("filter").Funcs(funcs).Parse(contents)
	if err != nil {
		return "", nil
	}

	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, nil); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func replaceKey(args ...string) string {
	if len(args) != 3 {
		return args[0]
	}
	return map[bool]string{
		true:  args[1],
		false: args[2],
	}[args[0] == args[2]]
}

func applyPathTemplate(contents string) (string, error) {
	funcs := template.FuncMap{
		"env": func(key string) (string, error) {
			envVar, exists := os.LookupEnv(key)
			if !exists {
				return "", fmt.Errorf("unable to read environment variable %q in template %q", key, contents)
			}
			return envVar, nil
		},
	}

	tmpl, err := template.New("path").Funcs(funcs).Parse(contents)
	if err != nil {
		return "", nil
	}

	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, nil); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func applyServiceTemplate(contents, service, key string) (string, error) {
	funcs := template.FuncMap{
		"service": func() (string, error) {
			return service, nil
		},
		"key": func() (string, error) {
			return key, nil
		},
	}

	tmpl, err := template.New("filter").Funcs(funcs).Parse(contents)
	if err != nil {
		return "", nil
	}

	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, nil); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func (r *Runner) appendServices(env map[string]string, d *dep.CatalogServiceQuery, data interface{}) (err error) {
	typed, ok := data.([]*dep.CatalogService)
	if !ok {
		return fmt.Errorf("error converting to service %s", d)
	}

	for _, ser := range typed {
		serKV := make(map[string]string)
		cs := r.configServiceMap[d.String()]

		keyFormat := ser.ServiceName + "/id"
		if cs != nil && config.StringPresent(cs.FormatId) {
			keyFormat, err = applyServiceTemplate(config.StringVal(cs.FormatId), ser.ServiceName, "id")
			if err != nil {
				return err
			}
		}
		serKV[keyFormat] = ser.ServiceID

		keyFormat = ser.ServiceName + "/name"
		if cs != nil && config.StringPresent(cs.FormatName) {
			keyFormat, err = applyServiceTemplate(config.StringVal(cs.FormatName), ser.ServiceName, "name")
			if err != nil {
				return err
			}
		}
		serKV[keyFormat] = ser.ServiceName

		keyFormat = ser.ServiceName + "/address"
		if cs != nil && config.StringPresent(cs.FormatAddress) {
			keyFormat, err = applyServiceTemplate(config.StringVal(cs.FormatAddress), ser.ServiceName, "address")
			if err != nil {
				return err
			}
		}
		serKV[keyFormat] = ser.ServiceAddress

		keyFormat = ser.ServiceName + "/tag"
		if cs != nil && config.StringPresent(cs.FormatTag) {
			keyFormat, err = applyServiceTemplate(config.StringVal(cs.FormatTag), ser.ServiceName, "tag")
			if err != nil {
				return err
			}
		}
		serKV[keyFormat] = strings.Join([]string(ser.ServiceTags), ",")

		keyFormat = ser.ServiceName + "/port"
		if cs != nil && config.StringPresent(cs.FormatPort) {
			keyFormat, err = applyServiceTemplate(config.StringVal(cs.FormatPort), ser.ServiceName, "port")
			if err != nil {
				return err
			}
		}
		serKV[keyFormat] = strconv.Itoa(ser.ServicePort)

		for key, value := range serKV {
			if config.BoolVal(r.config.Upcase) {
				key = strings.ToUpper(key)
			}

			if config.BoolVal(r.config.Sanitize) {
				key = InvalidRegexp.ReplaceAllString(key, "_")
			}

			env[key] = value
		}
	}

	return
}

func (r *Runner) appendPrefixes(
	env map[string]string, d *dep.KVListQuery, data interface{},
) error {
	var err error

	typed, ok := data.([]*dep.KeyPair)
	if !ok {
		return fmt.Errorf("error converting to keypair %s", d)
	}

	// Get the PrefixConfig so we can get configuration from it.
	cp := r.configPrefixMap[d.String()]

	// For each pair, update the environment hash. Subsequent runs could
	// overwrite an existing key.
	for _, pair := range typed {
		key, value := pair.Key, string(pair.Value)

		// It is not possible to have an environment variable that is blank, but
		// it is possible to have an environment variable _value_ that is blank.
		if strings.TrimSpace(key) == "" {
			continue
		}

		// NoPrefix is nil when not set in config. Default to excluding prefix for Consul keys.
		if cp.NoPrefix != nil && !config.BoolVal(cp.NoPrefix) {
			pc, ok := r.configPrefixMap[d.String()]
			if !ok {
				return fmt.Errorf("missing dependency %s", d)
			}

			// Replace the invalid path chars such as slashes with underscores
			path := InvalidRegexp.ReplaceAllString(config.StringVal(pc.Path), "_")

			// Prefix the key value with the path value.
			key = fmt.Sprintf("%s_%s", path, key)
		}

		// If the user specified a custom format, apply that here.
		if config.StringPresent(cp.Format) {
			key, err = applyFormatTemplate(config.StringVal(cp.Format), key)
			if err != nil {
				return err
			}
		}

		if config.BoolVal(r.config.Sanitize) {
			key = InvalidRegexp.ReplaceAllString(key, "_")
		}

		if config.BoolVal(r.config.Upcase) {
			key = strings.ToUpper(key)
		}

		logger := namedLogger("runner")
		if current, ok := env[key]; ok {
			logger.Debug(fmt.Sprintf("overwriting %s=%q (was %q) from %s", key, value, current, d))
			env[key] = value
		} else {
			logger.Debug(fmt.Sprintf("setting %s=%q from %s", key, value, d))
			env[key] = value
		}
	}

	return nil
}

func isVaultKv2(data map[string]interface{}) bool {
	// check for presence of "metadata.version", indicating this value came from Vault
	// kv version 2
	if data["metadata"] != nil {
		metadata := data["metadata"].(map[string]interface{})
		return metadata["version"] != nil
	}

	return false
}

func (r *Runner) appendSecrets(
	env map[string]string, d *dep.VaultReadQuery, data interface{},
) error {
	var err error
	logger := namedLogger("runner")

	typed, ok := data.(*dep.Secret)
	if !ok {
		return fmt.Errorf("error converting to secret %s", d)
	}

	// Get the PrefixConfig so we can get configuration from it.
	cp := r.configPrefixMap[d.String()]

	valueMap := typed.Data
	if isVaultKv2(valueMap) {
		// Vault Secrets KV1 and KV2 return different formats. Here we check the key
		// value, and if we've found another key called "data" that is of type
		// map[string]interface, we assume it's KV2 and use the key/value pair from
		// it, otherwise we assume it's KV1
		//
		// In KV1, the JSON looks like
		// {
		//		"secretKey1": "value1",
		//		"secretKey2", "value2"
		// }
		//
		// In KV2, the JSON looks like
		// {
		//		"data": {
		//			"secretKey1": "value1",
		//			"secretKey2", "value2"
		//		},
		//		"metadata" : {
		//			...
		// 		}
		// }
		logger.Debug("Found KV2 secret")

		if valueMap["data"] == nil {
			logger.Debug("KV2 secret is nil or was deleted")
			valueMap = nil
		} else {
			valueMap = valueMap["data"].(map[string]interface{})
		}
	}

	var applyPerKeyFormat bool
	var keyFormats map[string][]*KeyFormat

	// pre-populate key formats map here so we don't have a potential O(n^2) complexity in the loop later
	if cp.Keys != nil && !config.StringPresent(cp.Format) {
		applyPerKeyFormat = true
		keyFormats = make(map[string][]*KeyFormat)
		for _, v := range *cp.Keys {
			keyFormats[*v.Name] = append(keyFormats[*v.Name], v)
		}
	}

	for originalKey, value := range valueMap {
		// Ignore any keys that are empty (not sure if this is even possible in
		// Vault, but I play defense).
		if strings.TrimSpace(originalKey) == "" {
			continue
		}

		// Ignore any keys in which value is nil
		if value == nil {
			continue
		}

		keys := []string{originalKey}
		// Check for per-key configuration override on a very early stage
		// before the `key` is updated with prefix or become uppercase
		if applyPerKeyFormat {
			keyFormat, ok := keyFormats[originalKey]
			if !ok {
				logger.Debug(fmt.Sprintf("skipping key '%s' since it is not listed in configuration", originalKey))
				continue
			}
			appliedFormats := []string{}
			for _, format := range keyFormat {
				if config.StringPresent(format.Format) {
					key, err := applyFormatTemplate(*format.Format, originalKey)
					if err != nil {
						return err
					}
					appliedFormats = append(appliedFormats, key)
				}
			}
			// reset keys slice in case of per-key formatting
			if len(appliedFormats) > 0 {
				keys = appliedFormats
			}
		}

		for i := range keys {
			key := keys[i]
			// NoPrefix is nil when not set in config. Default to including prefix for Vault secrets.
			if cp.NoPrefix == nil || !config.BoolVal(cp.NoPrefix) {
				// Replace the path slashes with an underscore.
				pc, ok := r.configPrefixMap[d.String()]
				if !ok {
					return fmt.Errorf("missing dependency %s", d)
				}

				path, err := applyPathTemplate(config.StringVal(pc.Path))
				if err != nil {
					return err
				}
				path = InvalidRegexp.ReplaceAllString(path, "_")

				// Prefix the key value with the path value.
				key = fmt.Sprintf("%s_%s", path, key)
			}

			// If the user specified a custom format for all keys, apply that here.
			if config.StringPresent(cp.Format) {
				key, err = applyFormatTemplate(config.StringVal(cp.Format), key)
				if err != nil {
					return err
				}
			}

			if config.BoolVal(r.config.Sanitize) {
				key = InvalidRegexp.ReplaceAllString(key, "_")
			}

			if config.BoolVal(r.config.Upcase) {
				key = strings.ToUpper(key)
			}

			val, ok := value.(string)
			if !ok {
				logger.Warn(fmt.Sprintf("skipping key '%s', invalid type for value. got %v, not string", key, reflect.TypeOf(value)))
				continue
			}

			if _, ok := env[key]; ok {
				logger.Debug(fmt.Sprintf("overwriting %s from %s", key, d))
			} else {
				logger.Debug(fmt.Sprintf("setting %s from %s", key, d))
			}

			env[key] = val
		}
	}

	return nil
}

// init creates the Runner's underlying data structures and returns an error if
// any problems occur.
func (r *Runner) init(clients *dep.ClientSet) error {
	// Ensure default configuration values
	r.config = DefaultConfig().Merge(r.config)
	r.config.Finalize()

	// Print the final config for debugging
	result, err := json.Marshal(r.config)
	if err != nil {
		return err
	}
	logger := namedLogger("runner")
	logger.Debug("final config:", string(result))

	// Set's consul-template's default vault lease duration and renewal thresh
	// these will go away with hashicat as it will eliminate the setting
	dep.SetVaultDefaultLeaseDuration(config.TimeDurationVal(r.config.Vault.DefaultLeaseDuration))
	dep.SetVaultLeaseRenewalThreshold(valueFrom(r.config.Vault.LeaseRenewalThreshold))

	// Create the watcher
	r.watcher = newWatcher(r.config, clients, r.once)

	// Parse and add consul dependencies
	for _, p := range *r.config.Prefixes {
		path, err := applyPathTemplate(config.StringVal(p.Path))
		if err != nil {
			return err
		}
		d, err := dep.NewKVListQuery(path)
		if err != nil {
			return err
		}
		r.dependencies = append(r.dependencies, d)
		r.configPrefixMap[d.String()] = p
	}

	// Parse and add consul services
	for _, s := range *r.config.Services {
		d, err := dep.NewCatalogServiceQuery(config.StringVal(s.Query))
		if err != nil {
			return err
		}

		r.dependencies = append(r.dependencies, d)
		r.configServiceMap[d.String()] = s
	}

	// Parse and add vault dependencies - it is important that this come after
	// consul, because consul should never be permitted to overwrite values from
	// vault; that would expose a security hole since access to consul is
	// typically less controlled than access to vault.
	for _, s := range *r.config.Secrets {
		path, err := applyPathTemplate(config.StringVal(s.Path))
		if err != nil {
			return err
		}

		logger.Info("looking at vault", "path", path)
		d, err := dep.NewVaultReadQuery(path)
		if err != nil {
			return err
		}
		r.dependencies = append(r.dependencies, d)
		r.configPrefixMap[d.String()] = s
	}

	return nil
}

func (r *Runner) stopWatchers() {
	if r.watcher != nil {
		namedLogger("runner").Debug("stopping watcher")
		r.watcher.Stop()
	}
	if r.vaultTokenWatcher != nil {
		namedLogger("runner").Debug("stopping vault token watcher")
		r.vaultTokenWatcher.Stop()
	}
}

func (r *Runner) stopChild() {
	r.childLock.RLock()
	defer r.childLock.RUnlock()

	if r.child != nil {
		namedLogger("runner").Debug("stopping child process")
		r.child.Stop()
	}
}

// storePid is used to write out a PID file to disk.
func (r *Runner) storePid() error {
	path := config.StringVal(r.config.PidFile)
	if path == "" {
		return nil
	}

	namedLogger("runner").Info("creating pid file at", path)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
	if err != nil {
		return fmt.Errorf("runner: could not open pid file: %s", err)
	}
	defer f.Close()

	pid := os.Getpid()
	_, err = f.WriteString(fmt.Sprintf("%d", pid))
	if err != nil {
		return fmt.Errorf("runner: could not write to pid file: %s", err)
	}
	return nil
}

// deletePid is used to remove the PID on exit.
func (r *Runner) deletePid() error {
	path := config.StringVal(r.config.PidFile)
	if path == "" {
		return nil
	}

	namedLogger("runner").Debug("removing pid file at", path)

	stat, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("runner: could not remove pid file: %s", err)
	}
	if stat.IsDir() {
		return fmt.Errorf("runner: specified pid file path is directory")
	}

	err = os.Remove(path)
	if err != nil {
		return fmt.Errorf("runner: could not remove pid file: %s", err)
	}
	return nil
}

// newClientSet creates a new client set from the given config.
func newClientSet(c *Config) (*dep.ClientSet, error) {
	clients := dep.NewClientSet()

	if err := clients.CreateConsulClient(&dep.CreateConsulClientInput{
		Address:                      config.StringVal(c.Consul.Address),
		Token:                        config.StringVal(c.Consul.Token),
		AuthEnabled:                  config.BoolVal(c.Consul.Auth.Enabled),
		AuthUsername:                 config.StringVal(c.Consul.Auth.Username),
		AuthPassword:                 config.StringVal(c.Consul.Auth.Password),
		SSLEnabled:                   config.BoolVal(c.Consul.SSL.Enabled),
		SSLVerify:                    config.BoolVal(c.Consul.SSL.Verify),
		SSLCert:                      config.StringVal(c.Consul.SSL.Cert),
		SSLKey:                       config.StringVal(c.Consul.SSL.Key),
		SSLCACert:                    config.StringVal(c.Consul.SSL.CaCert),
		SSLCAPath:                    config.StringVal(c.Consul.SSL.CaPath),
		ServerName:                   config.StringVal(c.Consul.SSL.ServerName),
		TransportDialKeepAlive:       config.TimeDurationVal(c.Consul.Transport.DialKeepAlive),
		TransportDialTimeout:         config.TimeDurationVal(c.Consul.Transport.DialTimeout),
		TransportDisableKeepAlives:   config.BoolVal(c.Consul.Transport.DisableKeepAlives),
		TransportIdleConnTimeout:     config.TimeDurationVal(c.Consul.Transport.IdleConnTimeout),
		TransportMaxIdleConns:        config.IntVal(c.Consul.Transport.MaxIdleConns),
		TransportMaxIdleConnsPerHost: config.IntVal(c.Consul.Transport.MaxIdleConnsPerHost),
		TransportTLSHandshakeTimeout: config.TimeDurationVal(c.Consul.Transport.TLSHandshakeTimeout),
	}); err != nil {
		return nil, fmt.Errorf("runner: %s", err)
	}

	if err := clients.CreateVaultClient(&dep.CreateVaultClientInput{
		Address:                      config.StringVal(c.Vault.Address),
		Namespace:                    config.StringVal(c.Vault.Namespace),
		Token:                        config.StringVal(c.Vault.Token),
		UnwrapToken:                  config.BoolVal(c.Vault.UnwrapToken),
		SSLEnabled:                   config.BoolVal(c.Vault.SSL.Enabled),
		SSLVerify:                    config.BoolVal(c.Vault.SSL.Verify),
		SSLCert:                      config.StringVal(c.Vault.SSL.Cert),
		SSLKey:                       config.StringVal(c.Vault.SSL.Key),
		SSLCACert:                    config.StringVal(c.Vault.SSL.CaCert),
		SSLCAPath:                    config.StringVal(c.Vault.SSL.CaPath),
		ServerName:                   config.StringVal(c.Vault.SSL.ServerName),
		TransportDialKeepAlive:       config.TimeDurationVal(c.Vault.Transport.DialKeepAlive),
		TransportDialTimeout:         config.TimeDurationVal(c.Vault.Transport.DialTimeout),
		TransportDisableKeepAlives:   config.BoolVal(c.Vault.Transport.DisableKeepAlives),
		TransportIdleConnTimeout:     config.TimeDurationVal(c.Vault.Transport.IdleConnTimeout),
		TransportMaxIdleConns:        config.IntVal(c.Vault.Transport.MaxIdleConns),
		TransportMaxIdleConnsPerHost: config.IntVal(c.Vault.Transport.MaxIdleConnsPerHost),
		TransportTLSHandshakeTimeout: config.TimeDurationVal(c.Vault.Transport.TLSHandshakeTimeout),
		K8SAuthRoleName:              config.StringVal(c.Vault.K8SAuthRoleName),
		K8SServiceAccountTokenPath:   config.StringVal(c.Vault.K8SServiceAccountTokenPath),
		K8SServiceAccountToken:       config.StringVal(c.Vault.K8SServiceAccountToken),
		K8SServiceMountPath:          config.StringVal(c.Vault.K8SServiceMountPath),
	}); err != nil {
		return nil, fmt.Errorf("runner: %s", err)
	}

	return clients, nil
}

// newWatcher creates a new watcher.
func newWatcher(c *Config, clients *dep.ClientSet, once bool) *watch.Watcher {
	namedLogger("runner").Info("creating watcher")

	return watch.NewWatcher(&watch.NewWatcherInput{
		Clients:             clients,
		MaxStale:            config.TimeDurationVal(c.MaxStale),
		Once:                once,
		RenewVault:          config.StringPresent(c.Vault.Token) && config.BoolVal(c.Vault.RenewToken),
		VaultAgentTokenFile: config.StringVal(c.Vault.VaultAgentTokenFile),
		RetryFuncConsul:     watch.RetryFunc(c.Consul.Retry.RetryFunc()),
		// TODO: Add a sane default retry - right now this only affects "local"
		// dependencies like reading a file from disk.
		RetryFuncDefault: nil,
		RetryFuncVault:   watch.RetryFunc(c.Vault.Retry.RetryFunc()),
		VaultToken:       clients.Vault().Token(),
	})
}

// applyConfigEnv applies custom env variables and allowlist/denylist rules from config
func (r *Runner) applyConfigEnv(env map[string]string) map[string]string {
	// Parse custom environment variables
	custom := make(map[string]string, len(r.config.Exec.Env.Custom))
	for _, v := range r.config.Exec.Env.Custom {
		list := strings.SplitN(v, "=", 2)
		custom[list[0]] = list[1]
	}

	// In pristine mode, just return the custom environment. If the user did not
	// specify a custom environment, just return the empty slice to force an
	// empty environment. We cannot return nil here because the later call to
	// os/exec will think we want to inherit the parent.
	if config.BoolVal(r.config.Exec.Env.Pristine) {
		if len(custom) > 0 {
			return custom
		}
		return make(map[string]string)
	}

	keys := make(map[string]bool, len(env))
	for k := range env {
		keys[k] = true
	}

	// anyGlobMatch is a helper function which checks if any of the given globs
	// match the string.
	anyGlobMatch := func(s string, patterns []string) bool {
		for _, pattern := range patterns {
			if matched, _ := filepath.Match(pattern, s); matched {
				return true
			}
		}
		return false
	}

	// Filter to envvars that match the allowlist
	// Combining lists on each reference may be slightly inefficient but this
	// allows for out of order method calls, not requiring the config to be
	// finalized first.
	allowlist := combineLists(r.config.Exec.Env.Allowlist, r.config.Exec.Env.AllowlistDeprecated)
	if n := len(allowlist); n > 0 {
		include := make(map[string]bool, n)
		for k := range keys {
			if anyGlobMatch(k, allowlist) {
				include[k] = true
			}
		}
		keys = include
	}

	// Remove any env vars that match the denylist
	// Denylist takes precedence over allowlist
	// Combining lists on each reference may be slightly inefficient but this
	// allows for out of order method calls, not requiring the config to be
	// finalized first.
	denylist := combineLists(r.config.Exec.Env.Denylist, r.config.Exec.Env.DenylistDeprecated)
	if len(denylist) > 0 {
		for k := range keys {
			if anyGlobMatch(k, denylist) {
				delete(keys, k)
			}
		}
	}

	// Filter env to allowed keys
	for k := range env {
		if _, ok := keys[k]; !ok {
			delete(env, k)
		}
	}

	// Add custom env to final map
	// Custom variables take precedence over allowlist and denylist
	for k, v := range custom {
		env[k] = v
	}

	return env
}

// combineLists makes a new list that combines 2 lists by adding values from
// the second list without removing any duplicates from the first.
func combineLists(a, b []string) []string {
	combined := make([]string, len(a), len(a)+len(b))
	m := make(map[string]bool)
	for i, v := range a {
		m[v] = true
		combined[i] = v
	}

	for _, v := range b {
		if !m[v] {
			combined = append(combined, v)
		}
	}

	return combined
}
