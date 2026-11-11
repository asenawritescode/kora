package site

import "testing"

func TestAppointmentsModulePassesMarketplaceValidation(t *testing.T) {
	requireValidPack(t, "appointments", erpModuleFiles(t, "appointments"))
}

// Appointments is installed beside People, CRM, Selling and the others in niche
// templates, so it must compose with them.
func TestAppointmentsComposesWithTheOtherModules(t *testing.T) {
	requireComposes(t, "composed-with-appointments", append([]string{"appointments", "people", "crm"}, erpModules...)...)
}
