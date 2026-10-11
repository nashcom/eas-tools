package probe_test

import (
	"testing"

	"github.com/nashcom/eas-tools/probe"
)

func TestZoneOfTheMachineFollowsTZ(t *testing.T) {
	t.Setenv("TZ", "Europe/Berlin")
	if z := probe.LocalZone(); z.Name != "Europe/Berlin" || z.IsUTC() {
		t.Errorf("TZ=Europe/Berlin gives %+v", z)
	}
	t.Setenv("TZ", ":America/New_York") // the form with a colon that some systems use
	if z := probe.LocalZone(); z.Name != "America/New_York" {
		t.Errorf("TZ=:America/New_York gives %+v", z)
	}
	t.Setenv("TZ", "UTC")
	if z := probe.LocalZone(); !z.IsUTC() {
		t.Errorf("TZ=UTC gives %+v", z)
	}
	// a name that is not one is not an error of the program: the next source decides, and the end is a zone
	t.Setenv("TZ", "not/a/zone")
	if z := probe.LocalZone(); z.Loc == nil || z.Name == "" {
		t.Errorf("a wrong TZ gives %+v, want some zone", z)
	}
}

func TestZoneFromTheNameOfTheSystem(t *testing.T) {
	for link, want := range map[string]string{
		"/usr/share/zoneinfo/Europe/Berlin":                    "Europe/Berlin",
		"../usr/share/zoneinfo/America/New_York":               "America/New_York",
		"/var/db/timezone/zoneinfo/Asia/Tokyo":                 "Asia/Tokyo",
		"/usr/share/zoneinfo/posix/Europe/Vienna":              "Europe/Vienna",
		"/usr/share/zoneinfo/right/Europe/London":              "Europe/London",
		"/usr/share/zoneinfo/Etc/UTC":                          "Etc/UTC",
		"/etc/localtime-is-a-file-without-zoneinfo-in-the-way": "",
	} {
		if got := probe.ZoneNameFromPath(link); got != want {
			t.Errorf("%q: %q, want %q", link, got, want)
		}
	}
}

func TestWindowsNameOfTheMachineBecomesAZone(t *testing.T) {
	for name, want := range map[string]string{
		"W. Europe Standard Time": "Europe/Berlin", "GMT Standard Time": "Europe/London", "Eastern Standard Time": "America/New_York",
		"UTC": "UTC", "India Standard Time": "Asia/Kolkata",
	} {
		z, ok := probe.ZoneOfWindowsName(name)
		if !ok || (z.Name != want && !(want == "UTC" && z.IsUTC())) {
			t.Errorf("%q: %+v, %v; want %s", name, z, ok, want)
		}
	}
	if _, ok := probe.ZoneOfWindowsName("Nothing Standard Time"); ok {
		t.Error("an unknown Windows name was accepted")
	}
}

// Every Windows name that the machine zone can turn into a zone gives the same name back as the TZID of that zone.
func TestWindowsNamesGoBackAndForth(t *testing.T) {
	for _, name := range probe.WindowsZoneNames() {
		z, ok := probe.ZoneOfWindowsName(name)
		if !ok {
			t.Errorf("%q: no zone", name)
			continue
		}
		if got := z.TZID(); got != name {
			t.Errorf("%q gives the zone %s, whose TZID is %q", name, z.Name, got)
		}
	}
}
