package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/consul-template/config"
	"github.com/hashicorp/consul-template/signals"
	"github.com/hashicorp/hcl"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
)

const (
	// DefaultLogLevel is the default logging level.
	DefaultLogLevel = "WARN"

	// DefaultMaxStale is the default staleness permitted. This enables stale
	// queries by default for performance reasons.
	DefaultMaxStale = 2 * time.Second

	// DefaultReloadSignal is the default signal for reload.
	DefaultReloadSignal = syscall.SIGHUP

	// DefaultKillSignal is the default signal for termination.
	DefaultKillSignal = syscall.SIGINT
)

// Config is used to configure Consul ENV
type Config struct {
	// Consul is the configuration for connecting to a Consul cluster.
	Consul *config.ConsulConfig `mapstructure:"consul"`

	// Exec is the configuration for exec/supervise mode.
	Exec *config.ExecConfig `mapstructure:"exec"`

	// KillSignal is the signal to listen for a graceful terminate event.
	KillSignal *os.Signal `mapstructure:"kill_signal"`

	// LogLevel is the level with which to log for this config.
	LogLevel *string `mapstructure:"log_level"`

	// MaxStale is the maximum amount of time for staleness from Consul as given
	// by LastContact.
	MaxStale *time.Duration `mapstructure:"max_stale"`

	// PidFile is the path on disk where a PID file should be written containing
	// this processes PID.
	PidFile *string `mapstructure:"pid_file"`

	// Prefixes is the list of all prefix dependencies (consul)
	// in merge order.
	Prefixes *PrefixConfigs `mapstructure:"prefix"`

	// Pristine indicates that we want a clean environment only
	// composed of consul config variables, not inheriting from exising
	// environment
	Pristine *bool `mapstructure:"pristine"`

	// ReloadSignal is the signal to listen for a reload event.
	ReloadSignal *os.Signal `mapstructure:"reload_signal"`

	// Sanitize converts any "bad" characters in key values to underscores
	Sanitize *bool `mapstructure:"sanitize"`

	// Secrets is the list of all secret dependencies (vault)
	Secrets *PrefixConfigs `mapstructure:"secret"`

	Services *ServiceConfigs `mapstructure:"service"`

	// Syslog is the configuration for syslog.
	Syslog *config.SyslogConfig `mapstructure:"syslog"`

	// Upcase converts environment variables to uppercase
	Upcase *bool `mapstructure:"upcase"`

	// Vault is the configuration for connecting to a vault server.
	Vault *config.VaultConfig `mapstructure:"vault"`

	// Wait is the quiescence timers.
	Wait *config.WaitConfig `mapstructure:"wait"`
}

// Copy returns a deep copy of the current configuration. This is useful because
// the nested data structures may be shared.
func (c *Config) Copy() *Config {
	var o Config

	if c.Consul != nil {
		o.Consul = c.Consul.Copy()
	}

	if c.Exec != nil {
		o.Exec = c.Exec.Copy()
	}

	o.KillSignal = c.KillSignal

	o.LogLevel = c.LogLevel

	o.MaxStale = c.MaxStale

	o.PidFile = c.PidFile

	o.ReloadSignal = c.ReloadSignal

	if c.Prefixes != nil {
		o.Prefixes = c.Prefixes.Copy()
	}

	o.Services = c.Services

	o.Pristine = c.Pristine

	o.Sanitize = c.Sanitize

	if c.Secrets != nil {
		o.Secrets = c.Secrets.Copy()
	}

	if c.Syslog != nil {
		o.Syslog = c.Syslog.Copy()
	}

	o.Upcase = c.Upcase

	if c.Vault != nil {
		o.Vault = c.Vault.Copy()
	}

	if c.Wait != nil {
		o.Wait = c.Wait.Copy()
	}

	return &o
}

func (c *Config) Merge(o *Config) *Config {
	if c == nil {
		if o == nil {
			return nil
		}
		return o.Copy()
	}

	if o == nil {
		return c.Copy()
	}

	r := c.Copy()

	if o.Consul != nil {
		r.Consul = r.Consul.Merge(o.Consul)
	}

	if o.Exec != nil {
		r.Exec = r.Exec.Merge(o.Exec)
	}

	if o.KillSignal != nil {
		r.KillSignal = o.KillSignal
	}

	if o.LogLevel != nil {
		r.LogLevel = o.LogLevel
	}

	if o.MaxStale != nil {
		r.MaxStale = o.MaxStale
	}

	if o.PidFile != nil {
		r.PidFile = o.PidFile
	}

	if o.ReloadSignal != nil {
		r.ReloadSignal = o.ReloadSignal
	}

	if o.Prefixes != nil {
		r.Prefixes = r.Prefixes.Merge(o.Prefixes)
	}

	if o.Services != nil {
		r.Services = r.Services.Merge(o.Services)
	}

	if o.Pristine != nil {
		r.Pristine = o.Pristine
	}

	if o.Sanitize != nil {
		r.Sanitize = o.Sanitize
	}

	if o.Secrets != nil {
		r.Secrets = r.Secrets.Merge(o.Secrets)
	}

	if o.Syslog != nil {
		r.Syslog = r.Syslog.Merge(o.Syslog)
	}

	if o.Upcase != nil {
		r.Upcase = o.Upcase
	}

	if o.Vault != nil {
		r.Vault = r.Vault.Merge(o.Vault)
	}

	if o.Wait != nil {
		r.Wait = r.Wait.Merge(o.Wait)
	}

	return r
}

// Parse parses the given string contents as a config
func Parse(s string) (*Config, error) {
	logger := namedLogger("parse")
	var shadow interface{}
	if err := hcl.Decode(&shadow, s); err != nil {
		return nil, errors.Wrap(err, "error decoding config")
	}

	// Convert to a map and flatten the keys we want to flatten
	parsed, ok := shadow.(map[string]interface{})
	if !ok {
		return nil, errors.New("error converting config")
	}

	flattenKeys(parsed, []string{
		"consul",
		"consul.auth",
		"consul.retry",
		"consul.ssl",
		"consul.transport",
		"exec",
		"exec.env",
		"syslog",
		"vault",
		"vault.retry",
		"vault.ssl",
		"vault.transport",
		"wait",
	})

	// Deprecations
	// TODO remove in 0.8.0
	flattenKeys(parsed, []string{
		"auth",
		"ssl",
	})
	if auth, ok := parsed["auth"]; ok {
		logger.Warn("auth is now a child stanza inside consul instead of a " +
			"top-level stanza. Update your configuration files and change " +
			"auth {} to consul { auth { ... } } instead.")
		consul, ok := parsed["consul"].(map[string]interface{})
		if !ok {
			consul = map[string]interface{}{}
		}
		consul["auth"] = auth
		parsed["consul"] = consul
		delete(parsed, "auth")
	}
	if _, ok := parsed["path"]; ok {
		logger.Warn("path is no longer a key in the configuration. Please " +
			"remove it and use the CLI option instead.")
		delete(parsed, "path")
	}
	if splay, ok := parsed["splay"]; ok {
		logger.Warn(fmt.Sprintf("splay is now a child stanza for exec instead "+
			"of a top-level key. Update your configuration files and change "+
			"splay = \"%s\" to exec { splay = \"%s\" } instead.", splay, splay))
		exec, ok := parsed["exec"].(map[string]interface{})
		if !ok {
			exec = map[string]interface{}{}
		}
		exec["splay"] = splay
		parsed["exec"] = exec
		delete(parsed, "splay")
	}
	if retry, ok := parsed["retry"]; ok {
		logger.Warn("retry is now a child stanza for both consul and vault " +
			"instead of a top-level stanza. Update your configuration files " +
			"and change retry {} to consul { retry { ... } } and " +
			"vault { retry { ... } } instead.")

		consul, ok := parsed["consul"].(map[string]interface{})
		if !ok {
			consul = map[string]interface{}{}
		}

		vault, ok := parsed["vault"].(map[string]interface{})
		if !ok {
			vault = map[string]interface{}{}
		}

		r := map[string]interface{}{
			"backoff":     retry,
			"max_backoff": retry,
		}

		consul["retry"] = r
		parsed["consul"] = consul

		vault["retry"] = r
		parsed["vault"] = vault

		delete(parsed, "retry")
	}
	if ssl, ok := parsed["ssl"]; ok {
		logger.Warn("ssl is now a child stanza for both consul and vault " +
			"instead of a top-level stanza. Update your configuration files " +
			"and change ssl {} to consul { ssl { ... } } and " +
			"vault { ssl { ... } } instead.")

		consul, ok := parsed["consul"].(map[string]interface{})
		if !ok {
			consul = map[string]interface{}{}
		}

		vault, ok := parsed["vault"].(map[string]interface{})
		if !ok {
			vault = map[string]interface{}{}
		}

		consul["ssl"] = ssl
		parsed["consul"] = consul

		vault["ssl"] = ssl
		parsed["vault"] = vault

		delete(parsed, "ssl")
	}
	if timeout, ok := parsed["timeout"]; ok {
		logger.Warn(fmt.Sprintf("timeout is now a child stanza for exec instead"+
			"of a top-level key. Update your configuration files and change "+
			"timeout = \"%s\" to exec { kill_timeout = \"%s\" } instead.",
			timeout, timeout))
		exec, ok := parsed["exec"].(map[string]interface{})
		if !ok {
			exec = map[string]interface{}{}
		}
		exec["kill_timeout"] = timeout
		parsed["exec"] = exec
		delete(parsed, "timeout")
	}
	if token, ok := parsed["token"]; ok {
		logger.Warn("token is now a child stanza inside consul instead of a " +
			"top-level key. Update your configuration files and change " +
			"token = \"...\" to consul { token = \"...\" } instead.")
		consul, ok := parsed["consul"].(map[string]interface{})
		if !ok {
			consul = map[string]interface{}{}
		}
		consul["token"] = token
		parsed["consul"] = consul
		delete(parsed, "token")
	}

	// Create a new, empty config
	var c Config

	// Use mapstructure to populate the basic config fields
	var md mapstructure.Metadata
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			config.ConsulStringToStructFunc(),
			config.StringToFileModeFunc(),
			signals.StringToSignalFunc(),
			config.StringToWaitDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
			mapstructure.StringToTimeDurationHookFunc(),
		),
		ErrorUnused: true,
		Metadata:    &md,
		Result:      &c,
	})
	if err != nil {
		logger.Debug(fmt.Sprintf("%#v", parsed))
		return nil, errors.Wrap(err, "mapstructure decoder creation failed")
	}
	if err := decoder.Decode(parsed); err != nil {
		logger.Debug(fmt.Sprintf("%#v", parsed))
		return nil, errors.Wrap(err, "mapstructure decode failed")
	}

	return &c, nil
}

// Must returns a config object that must compile. If there are any errors, this
// function will panic. This is most useful in testing or constants.
func Must(s string) *Config {
	c, err := Parse(s)
	if err != nil {
		namedLogger("parse").Error(err.Error())
	}
	return c
}

// TestConfig returuns a default, finalized config, with the provided
// configuration taking precedence.
func TestConfig(c *Config) *Config {
	d := DefaultConfig().Merge(c)
	d.Finalize()
	return d
}

// FromFile reads the configuration file at the given path and returns a new
// Config struct with the data populated.
func FromFile(path string) (*Config, error) {
	c, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "from file: "+path)
	}

	config, err := Parse(string(c))
	if err != nil {
		return nil, errors.Wrap(err, "from file: "+path)
	}
	return config, nil
}

// FromPath iterates and merges all configuration files in a given
// directory, returning the resulting config.
func FromPath(path string) (*Config, error) {
	// Ensure the given filepath exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, errors.Wrap(err, "missing file/folder: "+path)
	}

	// Check if a file was given or a path to a directory
	stat, err := os.Stat(path)
	if err != nil {
		return nil, errors.Wrap(err, "failed stating file: "+path)
	}

	// Recursively parse directories, single load files
	if stat.Mode().IsDir() {
		// Ensure the given filepath has at least one config file
		_, err := ioutil.ReadDir(path)
		if err != nil {
			return nil, errors.Wrap(err, "failed listing dir: "+path)
		}

		// Create a blank config to merge off of
		var c *Config

		// Potential bug: Walk does not follow symlinks!
		err = filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
			// If WalkFunc had an error, just return it
			if err != nil {
				return err
			}

			// Do nothing for directories
			if info.IsDir() {
				return nil
			}

			// Parse and merge the config
			newConfig, err := FromFile(path)
			if err != nil {
				return err
			}
			c = c.Merge(newConfig)

			return nil
		})

		if err != nil {
			return nil, errors.Wrap(err, "walk error")
		}

		return c, nil
	} else if stat.Mode().IsRegular() {
		return FromFile(path)
	}

	return nil, fmt.Errorf("unknown filetype: %q", stat.Mode().String())
}

// GoString defines the printable version of this struct.
func (c *Config) GoString() string {
	if c == nil {
		return "(*Config)(nil)"
	}

	return fmt.Sprintf("&Config{"+
		"Consul:%s, "+
		"Exec:%s, "+
		"KillSignal:%s, "+
		"LogLevel:%s, "+
		"MaxStale:%s, "+
		"PidFile:%s, "+
		"Prefixes:%s, "+
		"Pristine:%s, "+
		"ReloadSignal:%s, "+
		"Sanitize:%s, "+
		"Secrets:%s, "+
		"Services:%s, "+
		"Syslog:%s, "+
		"Upcase:%s, "+
		"Vault:%s, "+
		"Wait:%s"+
		"}",
		c.Consul.GoString(),
		c.Exec.GoString(),
		config.SignalGoString(c.KillSignal),
		config.StringGoString(c.LogLevel),
		config.TimeDurationGoString(c.MaxStale),
		config.StringGoString(c.PidFile),
		c.Prefixes.GoString(),
		config.BoolGoString(c.Pristine),
		config.SignalGoString(c.ReloadSignal),
		config.BoolGoString(c.Sanitize),
		c.Secrets.GoString(),
		c.Services.GoString(),
		c.Syslog.GoString(),
		config.BoolGoString(c.Upcase),
		c.Vault.GoString(),
		c.Wait.GoString(),
	)
}

// DefaultConfig returns the default configuration struct. Certain environment
// variables may be set which control the values for the default configuration.
func DefaultConfig() *Config {
	return &Config{
		Consul:   config.DefaultConsulConfig(),
		Exec:     config.DefaultExecConfig(),
		Prefixes: DefaultPrefixConfigs(),
		Secrets:  DefaultPrefixConfigs(),
		Services: DefaultServiceConfigs(),
		Syslog:   config.DefaultSyslogConfig(),
		Vault:    config.DefaultVaultConfig(),
		Wait:     config.DefaultWaitConfig(),
	}
}

// Finalize ensures all configuration options have the default values, so it
// is safe to dereference the pointers later down the line. It also
// intelligently tries to activate stanzas that should be "enabled" because
// data was given, but the user did not explicitly add "Enabled: true" to the
// configuration.
func (c *Config) Finalize() {
	if c.Consul == nil {
		c.Consul = config.DefaultConsulConfig()
	}
	c.Consul.Finalize()

	if c.Exec == nil {
		c.Exec = config.DefaultExecConfig()
	}
	c.Exec.Finalize()

	if c.KillSignal == nil {
		c.KillSignal = config.Signal(DefaultKillSignal)
	}

	if c.LogLevel == nil {
		c.LogLevel = stringFromEnv([]string{
			"CT_LOG",
			"ENVCONSUL",
		}, DefaultLogLevel)
	}

	if c.MaxStale == nil {
		c.MaxStale = config.TimeDuration(DefaultMaxStale)
	}

	if c.Prefixes == nil {
		c.Prefixes = DefaultPrefixConfigs()
	}
	c.Prefixes.Finalize()

	if c.PidFile == nil {
		c.PidFile = config.String("")
	}

	if c.Pristine == nil {
		c.Pristine = config.Bool(false)
	}

	if c.ReloadSignal == nil {
		c.ReloadSignal = config.Signal(DefaultReloadSignal)
	}

	if c.Sanitize == nil {
		c.Sanitize = config.Bool(false)
	}

	if c.Secrets == nil {
		c.Secrets = DefaultPrefixConfigs()
	}
	c.Secrets.Finalize()

	if c.Services == nil {
		c.Services = DefaultServiceConfigs()
	}
	c.Services.Finalize()

	if c.Syslog == nil {
		c.Syslog = config.DefaultSyslogConfig()
	}
	c.Syslog.Finalize()

	if c.Upcase == nil {
		c.Upcase = config.Bool(false)
	}

	if c.Vault == nil {
		c.Vault = config.DefaultVaultConfig()
	}
	c.Vault.Finalize()

	if c.Wait == nil {
		c.Wait = config.DefaultWaitConfig()
	}
	c.Wait.Finalize()
}

func stringFromEnv(list []string, def string) *string {
	for _, s := range list {
		if v := os.Getenv(s); v != "" {
			return config.String(strings.TrimSpace(v))
		}
	}
	return config.String(def)
}

func stringFromFile(list []string, def string) *string {
	for _, s := range list {
		c, err := ioutil.ReadFile(s)
		if err == nil {
			return config.String(strings.TrimSpace(string(c)))
		}
	}
	return config.String(def)
}

func antiboolFromEnv(list []string, def bool) *bool {
	for _, s := range list {
		if v := os.Getenv(s); v != "" {
			b, err := strconv.ParseBool(v)
			if err == nil {
				return config.Bool(!b)
			}
		}
	}
	return config.Bool(def)
}

func boolFromEnv(list []string, def bool) *bool {
	for _, s := range list {
		if v := os.Getenv(s); v != "" {
			b, err := strconv.ParseBool(v)
			if err == nil {
				return config.Bool(b)
			}
		}
	}
	return config.Bool(def)
}

// flattenKeys is a function that takes a map[string]interface{} and recursively
// flattens any keys that are a []map[string]interface{} where the key is in the
// given list of keys.
func flattenKeys(m map[string]interface{}, keys []string) {
	keyMap := make(map[string]struct{})
	for _, key := range keys {
		keyMap[key] = struct{}{}
	}

	var flatten func(map[string]interface{}, string)
	flatten = func(m map[string]interface{}, parent string) {
		for k, v := range m {
			// Calculate the map key, since it could include a parent.
			mapKey := k
			if parent != "" {
				mapKey = parent + "." + k
			}

			if _, ok := keyMap[mapKey]; !ok {
				continue
			}

			switch typed := v.(type) {
			case []map[string]interface{}:
				if len(typed) > 0 {
					last := typed[len(typed)-1]
					flatten(last, mapKey)
					m[k] = last
				} else {
					m[k] = nil
				}
			case map[string]interface{}:
				flatten(typed, mapKey)
				m[k] = typed
			default:
				m[k] = v
			}
		}
	}

	flatten(m, "")
}
