package print

import "testing"

func TestEncodeDecodeRoundTrip(t *testing.T) {
	f, err := Load([]byte("name: r\nfor: {operation: a~b~c}\npaper: thermal_58\ntext: \"{{ .doc.x }}\"\nhtml: h\n"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	g, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != f.Name || g.For != f.For || g.Paper != f.Paper || g.Text != f.Text || g.HTML != f.HTML {
		t.Fatalf("mismatch %+v vs %+v", g, f)
	}
	if _, err := Decode([]byte(`{"name":"x"}`)); err == nil {
		t.Fatal("expected invalid")
	}
}
