package version

import (
	"io"
	"net/http"
	"regexp"
	"time"

	"golang.org/x/mod/semver"
)

const VERSION_URL = "https://raw.githubusercontent.com/bezmoradi/t2/main/internal/version/version.go"

// The version check runs in the background at startup. The timeout bounds
// that goroutine on a slow or wedged network, and maxBody bounds what it
// reads: the file it fetches is a few lines long.
var httpClient = &http.Client{Timeout: 5 * time.Second}

const maxBody = 64 << 10

func CheckVersion() (bool, string) {
	res, err := httpClient.Get(VERSION_URL)
	if err != nil {
		return true, ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return true, ""
	}
	bytes, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return true, ""
	}

	newVersion := extractVersion(string(bytes))

	// An unparseable body means we could not determine the remote version - a
	// renamed file, a 404 page served as 200, or a reformatted declaration.
	// Treat that as "no answer". Comparing against the empty string here
	// would report every install as out of date, with a message naming no
	// version at all.
	if newVersion == "" {
		return true, ""
	}

	// Only a strictly newer remote version is worth reporting. Comparing with
	// != also fired when the local build was *ahead* of main, which is exactly
	// what happens while bumping the version before pushing.
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
