//go:build !linux

package model

import "errors"

// RuntimeDir is where the Debian package installs OpenVINO on a Linux host; the models run on Linux hosts only.
const RuntimeDir = "/usr/lib/ecc-server/openvino"

var errNotLinux = errors.New("the models run on Linux hosts only (OpenVINO is loaded from " + RuntimeDir + ")")

type ovCore struct{}

func newCore() (*ovCore, error) { return nil, errNotLinux }

func (c *ovCore) close() {}

func runtimeVersion() (string, error) { return "", errNotLinux }

type graph struct{}

func (c *ovCore) load(string, string, []string, map[string]string) (*graph, error) {
	return nil, errNotLinux
}

func (g *graph) close() {}

func (g *graph) run([]tensor) ([]float32, error) { return nil, errNotLinux }
