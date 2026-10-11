package probe

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // the zone rules are part of the program: a Windows host has no zone database

	"github.com/hstern/go-activesync/eas"
)

// Zone is a time zone of the user: the one that the times of an event are given in, and that the event and its invitation say.
// The zero value, and "UTC", is UTC.
type Zone struct {
	Name string // the name of the zone in the zone database, such as Europe/Berlin
	Loc  *time.Location
}

// LoadZone finds a zone by its name in the zone database (Europe/Berlin); an empty name is UTC.
func LoadZone(name string) (Zone, error) {
	n := strings.TrimSpace(name)
	if n == "" || strings.EqualFold(n, "utc") {
		return Zone{Name: "UTC", Loc: time.UTC}, nil
	}
	loc, err := time.LoadLocation(n)
	if err != nil {
		return Zone{}, fmt.Errorf("time zone %q: %w (use a name of the zone database, such as Europe/Berlin)", name, err)
	}
	return Zone{Name: n, Loc: loc}, nil
}

// IsUTC reports whether the zone is UTC.
func (z Zone) IsUTC() bool { return z.Loc == nil || z.Loc == time.UTC }

func (z Zone) location() *time.Location {
	if z.Loc == nil {
		return time.UTC
	}
	return z.Loc
}

// windowsZones are the names that Windows, Outlook and Notes give to zones. Notes writes them as the TZID of an invitation, so a
// zone gets its Windows name when it has one here.
var windowsZones = map[string]string{
	"UTC":                            "UTC",
	"Europe/London":                  "GMT Standard Time",
	"Europe/Dublin":                  "GMT Standard Time",
	"Europe/Lisbon":                  "GMT Standard Time",
	"Atlantic/Reykjavik":             "Greenwich Standard Time",
	"Europe/Berlin":                  "W. Europe Standard Time",
	"Europe/Vienna":                  "W. Europe Standard Time",
	"Europe/Zurich":                  "W. Europe Standard Time",
	"Europe/Amsterdam":               "W. Europe Standard Time",
	"Europe/Rome":                    "W. Europe Standard Time",
	"Europe/Stockholm":               "W. Europe Standard Time",
	"Europe/Oslo":                    "W. Europe Standard Time",
	"Europe/Luxembourg":              "W. Europe Standard Time",
	"Europe/Paris":                   "Romance Standard Time",
	"Europe/Madrid":                  "Romance Standard Time",
	"Europe/Brussels":                "Romance Standard Time",
	"Europe/Copenhagen":              "Romance Standard Time",
	"Europe/Prague":                  "Central Europe Standard Time",
	"Europe/Budapest":                "Central Europe Standard Time",
	"Europe/Warsaw":                  "Central European Standard Time",
	"Europe/Belgrade":                "Central Europe Standard Time",
	"Europe/Helsinki":                "FLE Standard Time",
	"Europe/Kiev":                    "FLE Standard Time",
	"Europe/Kyiv":                    "FLE Standard Time",
	"Europe/Riga":                    "FLE Standard Time",
	"Europe/Athens":                  "GTB Standard Time",
	"Europe/Bucharest":               "GTB Standard Time",
	"Europe/Istanbul":                "Turkey Standard Time",
	"Europe/Moscow":                  "Russian Standard Time",
	"Africa/Cairo":                   "Egypt Standard Time",
	"Africa/Johannesburg":            "South Africa Standard Time",
	"Asia/Jerusalem":                 "Israel Standard Time",
	"Asia/Dubai":                     "Arabian Standard Time",
	"Asia/Kolkata":                   "India Standard Time",
	"Asia/Calcutta":                  "India Standard Time",
	"Asia/Singapore":                 "Singapore Standard Time",
	"Asia/Shanghai":                  "China Standard Time",
	"Asia/Hong_Kong":                 "China Standard Time",
	"Asia/Tokyo":                     "Tokyo Standard Time",
	"Asia/Seoul":                     "Korea Standard Time",
	"Australia/Sydney":               "AUS Eastern Standard Time",
	"Australia/Melbourne":            "AUS Eastern Standard Time",
	"Australia/Brisbane":             "E. Australia Standard Time",
	"Australia/Perth":                "W. Australia Standard Time",
	"Pacific/Auckland":               "New Zealand Standard Time",
	"America/New_York":               "Eastern Standard Time",
	"America/Toronto":                "Eastern Standard Time",
	"America/Chicago":                "Central Standard Time",
	"America/Denver":                 "Mountain Standard Time",
	"America/Phoenix":                "US Mountain Standard Time",
	"America/Los_Angeles":            "Pacific Standard Time",
	"America/Vancouver":              "Pacific Standard Time",
	"America/Anchorage":              "Alaskan Standard Time",
	"Pacific/Honolulu":               "Hawaiian Standard Time",
	"America/Halifax":                "Atlantic Standard Time",
	"America/Sao_Paulo":              "E. South America Standard Time",
	"America/Argentina/Buenos_Aires": "Argentina Standard Time",
	"America/Mexico_City":            "Central Standard Time (Mexico)",
	"America/Bogota":                 "SA Pacific Standard Time",
}

// TZID is the name that the zone has in an invitation: its Windows name when it has one, else its own name.
func (z Zone) TZID() string {
	if z.IsUTC() {
		return "UTC"
	}
	if w, ok := windowsZones[z.Name]; ok {
		return w
	}
	return z.Name
}

// zoneChange is a switch of the offset of a zone.
type zoneChange struct {
	at       time.Time
	from, to int // the offsets before and after, in seconds east of UTC
}

// changes returns the switches of the offset that the zone has in a year.
func (z Zone) changes(year int) []zoneChange {
	loc := z.location()
	t := time.Date(year, 1, 1, 0, 0, 0, 0, loc)
	var out []zoneChange
	for i := 0; i < 16; i++ {
		_, end := t.ZoneBounds()
		if end.IsZero() || end.In(loc).Year() > year {
			break
		}
		_, from := t.Zone()
		_, to := end.In(loc).Zone()
		if from != to { // the zone data has boundaries where the offset stays the same
			out = append(out, zoneChange{end, from, to})
		}
		t = end
	}
	return out
}

// rule is the yearly rule of one of the two switches: the wall time before it, as Windows and iCalendar write it.
type rule struct {
	month, weekday, week int // week: 1 to 5, 5 is the last
	hour, minute, second int
	wall                 time.Time // the wall time of the switch in this year
	from, to             int       // offsets in seconds
}

func ruleOf(c zoneChange) rule {
	wall := c.at.Add(time.Duration(c.from) * time.Second).UTC()
	days := time.Date(wall.Year(), wall.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	week := (wall.Day()-1)/7 + 1
	if wall.Day()+7 > days {
		week = 5
	}
	return rule{month: int(wall.Month()), weekday: int(wall.Weekday()), week: week, hour: wall.Hour(), minute: wall.Minute(), second: wall.Second(),
		wall: wall, from: c.from, to: c.to}
}

// switches returns the rule that starts daylight saving time and the one that ends it in a year; ok is false for a zone without.
func (z Zone) switches(year int) (toDST, toStd rule, ok bool) {
	ch := z.changes(year)
	if len(ch) != 2 {
		return rule{}, rule{}, false
	}
	a, b := ruleOf(ch[0]), ruleOf(ch[1])
	switch {
	case a.to > a.from && b.to < b.from:
		return a, b, true
	case a.to < a.from && b.to > b.from:
		return b, a, true
	}
	return rule{}, rule{}, false
}

// standardOffset is the offset of the zone without daylight saving time, in seconds east of UTC.
func (z Zone) standardOffset(year int) int {
	if toDST, _, ok := z.switches(year); ok {
		return toDST.from
	}
	_, off := time.Date(year, 7, 1, 12, 0, 0, 0, z.location()).Zone()
	return off
}

// EASBlob is the TimeZone of a calendar entry of ActiveSync: the TIME_ZONE_INFORMATION of Windows, base64 coded, with the rules of
// daylight saving time of a year. UTC is all zero.
func (z Zone) EASBlob(year int) string {
	if z.IsUTC() {
		return eas.EncodeTimeZone(time.UTC)
	}
	tz := eas.EASTimeZone{BiasMinutes: int32(-z.standardOffset(year) / 60), StandardName: z.TZID(), DaylightName: strings.Replace(z.TZID(), "Standard", "Daylight", 1)}
	if toDST, toStd, ok := z.switches(year); ok {
		tz.DaylightBias = int32(-(toDST.to - toDST.from) / 60)
		tz.DaylightDate = eas.SystemTime{Month: uint16(toDST.month), DayOfWeek: uint16(toDST.weekday), Day: uint16(toDST.week), Hour: uint16(toDST.hour), Minute: uint16(toDST.minute), Second: uint16(toDST.second)}
		tz.StandardDate = eas.SystemTime{Month: uint16(toStd.month), DayOfWeek: uint16(toStd.weekday), Day: uint16(toStd.week), Hour: uint16(toStd.hour), Minute: uint16(toStd.minute), Second: uint16(toStd.second)}
	}
	return tz.Encode()
}

func icalOffset(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign, seconds = "-", -seconds
	}
	return fmt.Sprintf("%s%02d%02d", sign, seconds/3600, seconds%3600/60)
}

// firstWeekday is the day of the month of the nth (1 to 5; 5 is the last) weekday of a month.
func firstWeekday(year, month, weekday, week int) int {
	if week == 5 {
		last := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC)
		return last.Day() - (int(last.Weekday())-weekday+7)%7
	}
	first := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return 1 + (weekday-int(first.Weekday())+7)%7 + 7*(week-1)
}

// VTimezone is the time zone block (VTIMEZONE) of an invitation, written as Notes writes it: the Windows name of the zone and the
// two rules of daylight saving time. The rules start in 1970 and repeat every year. UTC has none.
func (z Zone) VTimezone(year int) []byte {
	if z.IsUTC() {
		return nil
	}
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\r\n") }
	line("BEGIN:VTIMEZONE")
	line("TZID:" + z.TZID())
	part := func(kind string, r rule) {
		byday := "-1"
		if r.week != 5 {
			byday = fmt.Sprint(r.week)
		}
		day := firstWeekday(1970, r.month, r.weekday, r.week)
		line("BEGIN:" + kind)
		line(fmt.Sprintf("DTSTART:1970%02d%02dT%02d%02d%02d", r.month, day, r.hour, r.minute, r.second))
		line("TZOFFSETFROM:" + icalOffset(r.from))
		line("TZOFFSETTO:" + icalOffset(r.to))
		line(fmt.Sprintf("RRULE:FREQ=YEARLY;BYDAY=%s%s;BYMONTH=%d", byday, []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}[r.weekday], r.month))
		line("END:" + kind)
	}
	if toDST, toStd, ok := z.switches(year); ok {
		part("STANDARD", toStd)
		part("DAYLIGHT", toDST)
	} else {
		off := icalOffset(z.standardOffset(year))
		line("BEGIN:STANDARD")
		line("DTSTART:19700101T000000")
		line("TZOFFSETFROM:" + off)
		line("TZOFFSETTO:" + off)
		line("END:STANDARD")
	}
	line("END:VTIMEZONE")
	return []byte(b.String())
}

// Location is the *time.Location of the zone, UTC for the zero value.
func (z Zone) Location() *time.Location { return z.location() }
