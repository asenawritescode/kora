package api

import (
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

func TestChannelPermissionContractMapsToEngineAuthorizationType(t *testing.T) {
	grant := contract.PermissionGrant{
		Doctype: "Invoice", Role: "Manager", Read: true, Write: true, Create: true,
		Delete: true, Submit: true, Cancel: true, Amend: true, Export: true,
		Import: true, Report: true, IfOwner: true,
	}
	want := []doctype.Permission{{
		Doctype: "Invoice", Role: "Manager", Read: true, Write: true, Create: true,
		Delete: true, Submit: true, Cancel: true, Amend: true, Export: true,
		Import: true, Report: true, IfOwner: true,
	}}
	if got := channelPermissionGrants([]contract.PermissionGrant{grant}); !reflect.DeepEqual(got, want) {
		t.Fatalf("channel permission mapping = %#v, want %#v", got, want)
	}
}
