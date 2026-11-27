// Package tzloc resolves the time zones a rule may name: an IANA name such as
// Europe/Berlin, or a fixed offset such as +08:00. time.LoadLocation knows
// only the first, so an offset the API accepted used to fall back to UTC
// without a word.
package tzloc

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var offsetRE = regexp.MustCompile(`^([+-])([0-9]{2}):([0-9]{2})$`)

// Load returns the location for name. Empty means UTC.
func Load(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if m := offsetRE.FindStringSubmatch(name); m != nil {
		h, _ := strconv.Atoi(m[2])
		min, _ := strconv.Atoi(m[3])
		if h > 14 || min > 59 {
			return nil, fmt.Errorf("offset %q is out of range", name)
		}
		secs := h*3600 + min*60
		if m[1] == "-" {
			secs = -secs
		}
		return time.FixedZone("UTC"+name, secs), nil
	}
	return time.LoadLocation(name)
}
