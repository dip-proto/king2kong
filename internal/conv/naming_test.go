package conv

import (
	"slices"
	"testing"
)

func TestUpperWords(t *testing.T) {
	for name, want := range map[string][]string{
		"VCLSnippet":  {"VCL"},
		"useSSO":      {"SSO"},
		"HTTPMethods": {"HTTP"},
		"ServiceID":   {"ID"},
		"parseJSON":   {"JSON"},
		"VCLs":        nil,
		"IDs":         nil,
		"MAX_COUNT":   nil,
		"Parse":       nil,
	} {
		if got := upperWords(name); !slices.Equal(got, want) {
			t.Errorf("upperWords(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestIdentFromName(t *testing.T) {
	prog := &Program{}
	prog.learnInitialisms()
	prog.initialisms["VCL"] = true
	for name, want := range map[string]string{
		"tls-config":   "TLSConfig",
		"vcl-name":     "VCLName",
		"service-id":   "ServiceID",
		"dry-run":      "DryRun",
		"2fa":          "X2fa",
		"http_methods": "HTTPMethods",
	} {
		if got := prog.identFromName(name); got != want {
			t.Errorf("identFromName(%q) = %q, want %q", name, got, want)
		}
	}
}
