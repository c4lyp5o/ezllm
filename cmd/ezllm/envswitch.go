package main

import (
	"fmt"
	"strings"
)

// envSwitch reads an on/off environment switch, the same way for every one.
// Unset or empty: set is false. on/true/1 and off/false/0 are accepted in any
// case (t/f too, for strconv.ParseBool habits); anything else is a startup
// error. A switch that read every unknown value as "on" turned features on for
// an operator who wrote =false.
func envSwitch(name, raw string) (on, set bool, err error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "":
		return false, false, nil
	case "on", "true", "1", "t":
		return true, true, nil
	case "off", "false", "0", "f":
		return false, true, nil
	}
	return false, true, fmt.Errorf("%s=%q: want on or off (also true/false, 1/0)", name, raw)
}

// bridgeEnabled is envSwitch for an EZLLM_*_BRIDGE switch: unset means off.
func bridgeEnabled(name string, getenv func(string) string) (bool, error) {
	on, _, err := envSwitch(name, getenv(name))
	return on, err
}
