package probe

import (
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// windowsPrimary is the zone of the zone database that stands for a Windows zone name: the one that a machine with that setting
// has in mind.
var windowsPrimary = map[string]string{
	"UTC": "UTC", "GMT Standard Time": "Europe/London", "Greenwich Standard Time": "Atlantic/Reykjavik",
	"W. Europe Standard Time": "Europe/Berlin", "Romance Standard Time": "Europe/Paris", "Central Europe Standard Time": "Europe/Prague",
	"Central European Standard Time": "Europe/Warsaw", "FLE Standard Time": "Europe/Helsinki", "GTB Standard Time": "Europe/Athens",
	"Turkey Standard Time": "Europe/Istanbul", "Russian Standard Time": "Europe/Moscow", "Egypt Standard Time": "Africa/Cairo",
	"South Africa Standard Time": "Africa/Johannesburg", "Israel Standard Time": "Asia/Jerusalem", "Arabian Standard Time": "Asia/Dubai",
	"India Standard Time": "Asia/Kolkata", "Singapore Standard Time": "Asia/Singapore", "China Standard Time": "Asia/Shanghai",
	"Tokyo Standard Time": "Asia/Tokyo", "Korea Standard Time": "Asia/Seoul", "AUS Eastern Standard Time": "Australia/Sydney",
	"E. Australia Standard Time": "Australia/Brisbane", "W. Australia Standard Time": "Australia/Perth",
	"New Zealand Standard Time": "Pacific/Auckland", "Eastern Standard Time": "America/New_York", "Central Standard Time": "America/Chicago",
	"Mountain Standard Time": "America/Denver", "US Mountain Standard Time": "America/Phoenix", "Pacific Standard Time": "America/Los_Angeles",
	"Alaskan Standard Time": "America/Anchorage", "Hawaiian Standard Time": "Pacific/Honolulu", "Atlantic Standard Time": "America/Halifax",
	"E. South America Standard Time": "America/Sao_Paulo", "Argentina Standard Time": "America/Argentina/Buenos_Aires",
	"Central Standard Time (Mexico)": "America/Mexico_City", "SA Pacific Standard Time": "America/Bogota",
}

// WindowsZoneNames lists the Windows zone names that can be turned into a zone.
func WindowsZoneNames() []string {
	out := make([]string, 0, len(windowsPrimary))
	for n := range windowsPrimary {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ZoneOfWindowsName turns a Windows zone name, which "tzutil /g" prints on a Windows machine, into a zone.
func ZoneOfWindowsName(name string) (Zone, bool) {
	iana, ok := windowsPrimary[strings.TrimSpace(name)]
	if !ok {
		return Zone{}, false
	}
	z, err := LoadZone(iana)
	return z, err == nil
}

// ZoneNameFromPath finds the name of a zone in the path of a zone file such as /usr/share/zoneinfo/Europe/Berlin, which is what
// /etc/localtime links to on Linux and macOS. It returns "" for a path that has no zoneinfo directory in it.
func ZoneNameFromPath(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	_, name, ok := strings.Cut(path, "zoneinfo/")
	if !ok {
		return ""
	}
	for _, prefix := range []string{"posix/", "right/"} {
		name = strings.TrimPrefix(name, prefix)
	}
	return name
}

// LocalZone is the time zone of the machine. The sources are, in this order: the variable TZ, the name of the zone that Go knows
// the local time by, /etc/timezone and the link /etc/localtime (Linux, macOS), and the setting of Windows ("tzutil /g"). A machine
// where none of them gives a zone that is in the zone database (a container usually is UTC) is UTC.
func LocalZone() Zone {
	try := func(name string) (Zone, bool) {
		if name = strings.TrimPrefix(strings.TrimSpace(name), ":"); name == "" {
			return Zone{}, false
		}
		z, err := LoadZone(name)
		return z, err == nil
	}
	if z, ok := try(os.Getenv("TZ")); ok {
		return z
	}
	if n := timeLocalName(); n != "" {
		if z, ok := try(n); ok {
			return z
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if z, ok := try(string(b)); ok {
			return z
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if z, ok := try(ZoneNameFromPath(target)); ok {
			return z
		}
	}
	if runtime.GOOS == "windows" {
		if z, ok := windowsMachineZone(); ok {
			return z
		}
	}
	z, _ := LoadZone("UTC")
	return z
}

var windowsZoneOnce struct {
	sync.Once
	z  Zone
	ok bool
}

// windowsMachineZone asks Windows for its zone once for the life of the program (it starts a process).
func windowsMachineZone() (Zone, bool) {
	windowsZoneOnce.Do(func() {
		if out, err := exec.Command("tzutil", "/g").Output(); err == nil {
			windowsZoneOnce.z, windowsZoneOnce.ok = ZoneOfWindowsName(string(out))
		}
	})
	return windowsZoneOnce.z, windowsZoneOnce.ok
}

// timeLocalName is the name that Go knows the local time zone by, "" when it only knows "Local".
func timeLocalName() string {
	if n := time.Local.String(); n != "" && n != "Local" {
		return n
	}
	return ""
}
