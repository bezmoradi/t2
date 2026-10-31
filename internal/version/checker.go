package version

import (
	"io"
	"net/http"
	"regexp"
	"time"

	"golang.org/x/mod/semver"
)

const versionURL = "https://raw.githubusercontent.com/bezmoradi/t2/main/internal/version/version.go"

var versionCheckClient = &http.Client{Timeout: 5 * time.Second}

const maxVersionFileBytes = 64 << 10

func CheckVersion() (bool, string) {
	res, err := versionCheckClient.Get(versionURL)
	if err != nil {
		return true, ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return true, ""
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxVersionFileBytes))
	if err != nil {
		return true, ""
	}

	remoteVersion := extractVersion(string(body))
	remoteVersionUnknown := remoteVersion == ""
	if remoteVersionUnknown {
		return true, ""
	}

	remoteIsNewer := semver.Compare(remoteVersion, VERSION) > 0
	if remoteIsNewer {
		return false, remoteVersion
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
