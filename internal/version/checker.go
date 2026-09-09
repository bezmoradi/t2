package version

import (
	"io"
	"net/http"
	"regexp"
	"time"

	"golang.org/x/mod/semver"
)

const VERSION_URL = "https://raw.githubusercontent.com/bezmoradi/t2/main/internal/version/version.go"

// The version check gates startup, so it must never be able to hang the app on
// a slow or wedged network.
var httpClient = &http.Client{Timeout: 5 * time.Second}

func CheckVersion() (bool, string) {
	res, err := httpClient.Get(VERSION_URL)
	if err != nil {
		return true, ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return true, ""
	}
	bytes, err := io.ReadAll(res.Body)
	if err != nil {
		return true, ""
	}

	newVersion := extractVersion(string(bytes))

	// An unparseable body means we could not determine the remote version - a
	// renamed file, a 404 page served as 200, or a reformatted declaration.
	// Treat that as "no answer" and let the app run. Comparing against the
	// empty string here would report every install as out of date and refuse
	// to start, with a message naming no version at all.
	if newVersion == "" {
		return true, ""
	}

	// Only a strictly newer remote version is worth reporting. Comparing with
	// != also fired when the local build was *ahead* of main, which is exactly
	// what happens while bumping the version before pushing, and refused to
	// start.
	if semver.Compare(newVersion, VERSION) > 0 {
		return false, newVersion
	}

	return true, ""
}

func extractVersion(input string) string {
	re := regexp.MustCompile(`VERSION\s*=\s*"v(\d+\.\d+\.\d+)"`)
	matches := re.FindStringSubmatch(input)
	if len(matches) < 2 {
		return ""
	}
	return "v" + matches[1]
}
