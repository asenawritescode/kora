package site

import "testing"

func TestRoomsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "rooms-bookings", packFiles(t, "rooms-bookings"))
}

// Rooms is installed beside People, CRM and the other modules in niche
// templates, so it must compose with them.
func TestRoomsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-rooms", append([]string{"rooms-bookings", "people", "crm"}, baseModules...)...)
}
