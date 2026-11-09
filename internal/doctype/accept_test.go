package doctype

import "testing"

func TestValidateAccept(t *testing.T) {
	valid := []string{
		".pdf",
		".pdf,.docx",
		"image/*",
		"application/pdf",
		".pdf\nimage/*",
	}
	for _, a := range valid {
		if err := validateAccept(a); err != nil {
			t.Errorf("validateAccept(%q) = %v, want nil", a, err)
		}
	}

	invalid := []string{"pdf", "image", ".", "pdf,docx"}
	for _, a := range invalid {
		if err := validateAccept(a); err == nil {
			t.Errorf("validateAccept(%q) = nil, want error", a)
		}
	}
}

func TestIsAttachType(t *testing.T) {
	for _, ft := range []string{"Attach", "Attach Image", "Attach Audio"} {
		if !isAttachType(ft) {
			t.Errorf("isAttachType(%q) = false, want true", ft)
		}
	}
	if isAttachType("Data") {
		t.Error("isAttachType(Data) = true, want false")
	}
}

func TestValidateRejectsAcceptOnNonAttachField(t *testing.T) {
	dt := &DocType{
		Name:   "Invoice",
		Module: "Core",
		Fields: []Field{{Fieldname: "total", Fieldtype: "Currency", Accept: ".pdf"}},
	}
	if err := dt.Validate(); err == nil {
		t.Fatal("expected accept on a non-attach field to be rejected")
	}
}

func TestValidateAllowsAcceptOnAttachField(t *testing.T) {
	dt := &DocType{
		Name:   "Invoice",
		Module: "Core",
		Fields: []Field{{Fieldname: "receipt", Fieldtype: "Attach", Accept: ".pdf,.docx"}},
	}
	if err := dt.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}
