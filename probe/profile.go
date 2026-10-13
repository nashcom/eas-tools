package probe

import (
	"sort"
	"strings"

	"github.com/hstern/go-activesync/eas"
)

// Version is the version of eas-tools: every program of the toolbox has this one, and it is also part of the User-Agent marker. push-release.sh
// reads it from here, and the tag vX.Y.Z of a release has to be this version (the release workflow checks that).
const Version = "0.9.2"

// DefaultProfile imitates an iPhone, so the probe sees the same code paths
// and device-type rules as real users; the "-easprobe" marker in its
// User-Agent still lets admins tell it from real phones. The "easprobe"
// profile identifies itself plainly instead.
const DefaultProfile = "iphone"

// Profile describes how the probe presents itself to the server: the
// DeviceType in the request URL, the HTTP User-Agent and, optionally, the
// DeviceInformation a real phone sends right after Provision.
//
// Traveler classifies devices by pattern matching on this data (for Apple
// devices the pattern accepts "Apple-", "iPhone", "iPod" and "iPad" and
// rejects BlackBerry, Windows Phone, Boxer and WorxMail). The iOS profiles
// therefore start with the Apple string and append our own marker, so they
// are recognized as iOS and can still be told apart from real devices.
type Profile struct {
	Name       string
	DeviceType string
	UserAgent  string
	Info       *eas.DeviceInformation
}

// marker is appended to the User-Agent of every imitating profile. It must
// not contain any word Traveler's device patterns exclude.
const marker = "easprobe"

// Traveler takes the Device Name from the User-Agent text before the "/", and
// shows the whole User-Agent as OS Type. A real iPhone looks like
// "Apple-iPhone13C3/2307.90" (Traveler adds the "(OS 26)" itself, apparently
// from the OS in DeviceInformation); the marker goes behind the device
// part with a dash so the device is still recognized as Apple but can be told
// apart from real phones: "Apple-iPhone13C3-easprobe/2307.90". The
// FriendlyName in DeviceInformation must be the same text: Traveler takes the
// name from either source, so two different texts make the name flip.
const (
	iphoneUA = "Apple-iPhone13C3-" + marker + "/2307.90"
	ipadUA   = "Apple-iPad13C1-" + marker + "/2307.90"
)

var profiles = map[string]Profile{
	"easprobe": {
		Name:       "easprobe",
		DeviceType: "easprobe",
		UserAgent:  marker + "/" + Version,
	},
	"iphone": {
		Name:       "iphone",
		DeviceType: "iPhone",
		UserAgent:  iphoneUA,
		Info: &eas.DeviceInformation{
			Model:        "iPhone13,3",
			FriendlyName: "Apple-iPhone13C3-" + marker,
			OS:           "iOS 26.0 23A344",
			OSLanguage:   "en-us",
			UserAgent:    iphoneUA,
		},
	},
	"ipad": {
		Name:       "ipad",
		DeviceType: "iPad",
		UserAgent:  ipadUA,
		Info: &eas.DeviceInformation{
			Model:        "iPad13,1",
			FriendlyName: "Apple-iPad13C1-" + marker,
			OS:           "iOS 26.0 23A344",
			OSLanguage:   "en-us",
			UserAgent:    ipadUA,
		},
	},
}

// LookupProfile returns the named profile (case-insensitive).
func LookupProfile(name string) (Profile, bool) {
	if name == "" {
		name = DefaultProfile
	}
	p, ok := profiles[strings.ToLower(name)]
	return p, ok
}

// ProfileNames lists the available profile names.
func ProfileNames() []string {
	names := make([]string, 0, len(profiles))
	for n := range profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
