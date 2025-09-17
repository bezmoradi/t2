package version

import (
	"io"
	"net/http"
	"regexp"
)

const VERSION_URL = "https://raw.githubusercontent.com/bezmoradi/t2/main/internal/version/version.go"

func CheckVersion() (bool, string) {
	res, err := http.Get(VERSION_URL)
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
	if VERSION != newVersion {
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
