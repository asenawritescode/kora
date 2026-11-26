package script

import (
	"context"
	"testing"
)

// hasDocTypeProvider answers HasDocType from a fixed set of installed record types.
type hasDocTypeProvider struct {
	NoopProvider
	installed map[string]bool
}

func (p hasDocTypeProvider) HasDocType(name string) bool { return p.installed[name] }

// plainProvider implements only the document interface, without HasDocType.
type plainProvider struct{ NoopProvider }

func TestKoraHasDocType(t *testing.T) {
	installed := hasDocTypeProvider{installed: map[string]bool{"Sale": true}}
	tests := []struct {
		name     string
		provider KoraProvider
		expr     string
		want     any
	}{
		{name: "installed type", provider: installed, expr: `kora.hasDocType("Sale")`, want: true},
		{name: "unknown type", provider: installed, expr: `kora.hasDocType("No Such Type")`, want: false},
		{name: "provider without checker", provider: plainProvider{}, expr: `kora.hasDocType("Sale")`, want: false},
		{name: "no provider", provider: nil, expr: `kora.hasDocType("Sale")`, want: false},
	}
	runner := NewEmbeddedRunner(DefaultEmbeddedConfig())
	t.Cleanup(func() { _ = runner.Close() }) // test-only runner; Close errors cannot change the assertions
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runner.Execute(context.Background(), ExecuteRequest{
				Script:   "return {result: " + tc.expr + "};",
				Provider: tc.provider,
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Result != tc.want {
				t.Fatalf("%s = %v, want %v", tc.expr, result.Result, tc.want)
			}
		})
	}
}

func TestKoraHasDocTypeRequiresArgument(t *testing.T) {
	runner := NewEmbeddedRunner(DefaultEmbeddedConfig())
	t.Cleanup(func() { _ = runner.Close() }) // test-only runner; Close errors cannot change the assertions
	if _, err := runner.Execute(context.Background(), ExecuteRequest{Script: "kora.hasDocType();"}); err == nil {
		t.Fatal("kora.hasDocType() without argument returned no error")
	}
}

func TestValidateAcceptsHasDocType(t *testing.T) {
	runner := NewEmbeddedRunner(DefaultEmbeddedConfig())
	t.Cleanup(func() { _ = runner.Close() }) // test-only runner; Close errors cannot change the assertions
	if err := runner.Validate(`if (kora.hasDocType("Sale")) { kora.log.info("ok"); }`); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
