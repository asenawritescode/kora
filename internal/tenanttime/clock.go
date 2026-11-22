// Package tenanttime holds a tenant's business clock: the IANA time zone and
// the business-day cutoff that decide which calendar day a moment belongs to.
package tenanttime

import (
	"fmt"
	"time"
	_ "time/tzdata" // zones resolve in minimal containers without system zoneinfo
)

// DefaultZone is the time zone of a tenant that has not stored its own.
const DefaultZone = "Africa/Nairobi"

var defaultLocation = mustLocation(DefaultZone)

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf("tenanttime: load %s: %v", name, err))
	}
	return loc
}

// Clock maps instants to tenant business days. The zero value uses DefaultZone
// and a 00:00 cutoff.
type Clock struct {
	Location *time.Location
	// Cutoff is how long after local midnight a business day starts. With a
	// 03:00 cutoff, 02:30 local still belongs to the previous day.
	Cutoff time.Duration
}

// Default returns the clock used when a tenant stores no settings.
func Default() Clock {
	return Clock{Location: defaultLocation}
}

func (c Clock) location() *time.Location {
	if c.Location == nil {
		return defaultLocation
	}
	return c.Location
}

// ZoneName is the IANA name of the clock's zone.
func (c Clock) ZoneName() string {
	return c.location().String()
}

// BusinessDate returns the business day that contains t.
func (c Clock) BusinessDate(t time.Time) Date {
	year, month, day := t.In(c.location()).Add(-c.Cutoff).Date()
	return Date{Year: year, Month: month, Day: day}
}

// Date is a calendar day with no time zone attached.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// String formats d as YYYY-MM-DD.
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
}
