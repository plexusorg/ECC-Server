//go:build !linux

package main

import "errors"

// readSecret is Linux only: the login commands store the key with systemd-creds.
func readSecret(string) (string, error) {
	return "", errors.New("reading a secret is supported on Linux only")
}
