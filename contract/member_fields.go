package contract

import (
	"fmt"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

// memberEmail is a tenant member's email address as it appears on the wire.
type memberEmail string

// validate requires one bare mailbox address of at most 254 bytes.
func (e memberEmail) validate() error {
	if len(e) < 3 || len(e) > 254 {
		return fmt.Errorf("member email must be between 3 and 254 bytes")
	}
	parsed, err := mail.ParseAddress(string(e))
	if err != nil || parsed.Address != string(e) {
		return fmt.Errorf("member email must be a single mailbox address")
	}
	return nil
}

// memberSubject is the opaque Cloud user ID of a tenant member. It is also used as one URL path
// segment, so it must not contain "..".
type memberSubject PrincipalID

func (id memberSubject) validate() error {
	if !memberSubjectPattern.MatchString(string(id)) || strings.Contains(string(id), "..") {
		return fmt.Errorf("member subject must be an opaque Cloud user ID of 3 to %d bytes", maxTenantMemberKeyBytes)
	}
	return nil
}

// memberRoles accepts exactly one role in M1. Role existence and assignability are Engine checks,
// not wire rules.
type memberRoles []string

func (r memberRoles) validate() error {
	if len(r) != 1 {
		return fmt.Errorf("member must be given exactly one role")
	}
	return memberLabel{name: "member role", value: r[0]}.validate()
}

// memberLabel is a short free-text member attribute (a name or a role) with the field's display name
// for error messages.
type memberLabel struct {
	name, value string
}

func (l memberLabel) validate() error {
	if l.value == "" || len(l.value) > maxTenantMemberKeyBytes || strings.TrimSpace(l.value) != l.value || !utf8.ValidString(l.value) {
		return fmt.Errorf("%s must be 1 to %d bytes of UTF-8 without surrounding whitespace", l.name, maxTenantMemberKeyBytes)
	}
	for _, r := range l.value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", l.name)
		}
	}
	return nil
}
