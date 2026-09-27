// solar-battery-tester - Tests a solar battery pack using the solar-battery-tester HAT.
// Copyright (C) 2025, The Cacophony Project
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// config is the whole of config.toml, which lives at
// /etc/solar-battery-tester/config.toml unless --config says otherwise. See
// _release/config.toml.example for the file itself.
type config struct {
	Server serverSettings `toml:"server"`
}

// serverSettings is where finished runs get posted. An empty url turns
// uploading off: runs stay in the data directory until one is set.
type serverSettings struct {
	URL      string `toml:"url"`
	Port     int    `toml:"port"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// loadConfig reads path, treating a missing file as "nothing configured yet"
// rather than an error -- a tester that hasn't been pointed at a server still
// tests batteries, it just keeps the runs.
//
// A file that is there but can't be read is fatal: a typo in a key name would
// otherwise turn into uploads failing with no explanation.
func loadConfig(path string) (config, error) {
	var cfg config

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		log.Warnf("No config file at %s, so no server is configured.", path)
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("reading %s: %v", path, err)
	}

	// Strict: an unrecognised key is nearly always a misspelled one, and
	// silently ignoring it is how a tester ends up quietly not uploading.
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("reading %s: %v", path, err)
	}
	return cfg, nil
}

// serverFromConfig is where runs get posted: what the config file says, with
// any command line flag winning over it.
//
// The address is worked out and checked here, at startup, rather than when the
// first run is ready to go -- an address the tester can't post to should be an
// error you see immediately, not one that turns up hours later with a finished
// test in hand.
func serverFromConfig(cfg config, args Args) (serverConfig, error) {
	server := serverConfig{
		username: cfg.Server.Username,
		password: cfg.Server.Password,
	}
	rawURL, port := cfg.Server.URL, cfg.Server.Port
	if args.ServerURL != "" {
		rawURL = args.ServerURL
	}
	// A --server-port given against an address from the config file is someone
	// deliberately pointing this run somewhere else, so it replaces whatever
	// port that address carries. Both coming from the command line, or both
	// from the file, is a contradiction instead -- see serverURL.
	portWins := args.ServerPort != 0 && args.ServerURL == ""
	if args.ServerPort != 0 {
		port = args.ServerPort
	}
	if args.ServerUsername != "" {
		server.username = args.ServerUsername
	}
	if args.ServerPassword != "" {
		server.password = args.ServerPassword
	}

	url, err := serverURL(rawURL, port, portWins)
	if err != nil {
		return server, err
	}
	server.url = url
	return server, nil
}

// hasScheme matches an address that starts with something like "https://".
var hasScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// serverURL combines the configured address and port into the base URL runs are
// posted to.
//
// An address with no scheme, like "10.1.5.86:5000", is taken as http://: Go
// otherwise reads "10.1.5.86" as the scheme and the rest as a path, which fails
// with "first path segment in URL cannot contain colon" at the point of upload.
func serverURL(rawURL string, port int, portWins bool) (string, error) {
	// Any trailing slash is trimmed off the parsed path further down, not here:
	// trimming it from the raw text turns "http://" into "http:/", which no
	// longer looks like it has a scheme and gets another one bolted on.
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil
	}
	if port != 0 && (port < 1 || port > 65535) {
		return "", fmt.Errorf("server port %d is not a port number", port)
	}

	if !hasScheme.MatchString(rawURL) {
		log.Warnf("Server address %q doesn't say http:// or https://, assuming http://.", rawURL)
		rawURL = "http://" + rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("server address %q: %v", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server address %q: %q isn't http or https", rawURL, u.Scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("server address %q has no host in it", rawURL)
	}

	if port != 0 {
		// Two different ports from the same place, one in the address and one
		// in the port setting, is a config whose author meant one of them.
		// Which is anyone's guess, so it stops here rather than picking.
		if have := u.Port(); have != "" && have != strconv.Itoa(port) && !portWins {
			return "", fmt.Errorf("server address %q has port %s, but port is set to %d",
				rawURL, have, port)
		}
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), nil
}
