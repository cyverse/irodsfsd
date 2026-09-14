package commons

import (
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
)

func parseRawURL(rawurl string) (string, string, string, error) {
	if len(strings.TrimSpace(rawurl)) == 0 {
		return "", "", "", errors.New("empty raw url")
	}

	u, err := url.ParseRequestURI(rawurl)
	if err != nil || (u.Host == "" && u.Path == "") {
		// try adding //
		u, repErr := url.ParseRequestURI("tcp://" + rawurl)
		if repErr != nil {
			return "", "", "", errors.Wrapf(err, "could not parse raw url %q", rawurl)
		}

		return "tcp", u.Host, "", nil
	}

	if u != nil {
		scheme := strings.ToLower(u.Scheme)
		if scheme == "unix" {
			return "unix", "", u.Path, nil
		} else if scheme == "tcp" {
			return "tcp", u.Host, "", nil
		}

		return u.Scheme, u.Host, u.Path, nil
	}

	return "", "", "", errors.Newf("could not parse raw url %q", rawurl)
}

// ParseServiceEndpoint parses endpoint string
func ParseServiceEndpoint(endpoint string) (string, string, error) {
	scheme, host, p, err := parseRawURL(endpoint)
	if err != nil {
		return "", "", err
	}

	scheme = strings.ToLower(scheme)
	switch scheme {
	case "tcp":
		return "tcp", host, nil
	case "unix":
		p = path.Join("/", strings.TrimPrefix(p, "/"))
		return "unix", p, nil
	case "":
		if len(host) > 0 {
			return "tcp", host, nil
		}
		return "", "", errors.Newf("unknown host %q", host)
	default:
		return "", "", errors.Newf("unsupported protocol %q", scheme)
	}
}

// ParseManagementServiceEndpoint validates an HTTP management endpoint and
// returns the address suitable for net/http's Server.Addr. An empty endpoint
// disables the management service.
func ParseManagementServiceEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", nil
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}

	u, err := url.ParseRequestURI(endpoint)
	if err != nil {
		return "", errors.Wrapf(err, "could not parse management service endpoint %q", endpoint)
	}
	if strings.ToLower(u.Scheme) != "http" {
		return "", errors.Newf("management service endpoint %q must use http", endpoint)
	}
	if u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.Newf("management service endpoint %q must be an http host and port without a path, query, fragment, or credentials", endpoint)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return "", errors.Wrapf(err, "management service endpoint %q must include a valid host and port", endpoint)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return "", errors.Newf("management service endpoint %q must include a port from 1 through 65535", endpoint)
	}

	return u.Host, nil
}
