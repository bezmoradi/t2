package version

import "testing"

// A remote body we cannot parse must never block startup. Reporting the empty
// string as "the newest version" made every install refuse to run.
func TestUnparseableRemoteDoesNotBlockStartup(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"404 page served as 200", `<html>404: Not Found</html>`},
		{"missing v prefix", "package version\nconst VERSION = \"1.0.7\""},
		{"declared as var", "package version\nvar VERSION string = \"v1.0.7\""},
		{"empty body", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractVersion(tc.body); got != "" {
				t.Fatalf("extractVersion() = %q, want empty", got)
			}
		})
	}
}

func TestExtractVersion(t *testing.T) {
	body := "package version\n\nconst VERSION = \"v1.2.3\"\n"
	if got, want := extractVersion(body), "v1.2.3"; got != want {
		t.Errorf("extractVersion() = %q, want %q", got, want)
	}
}
