package version

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/bezmoradi/t2/internal/transcription"
)

const latestReleaseURL = "https://api.github.com/repos/bezmoradi/t2/releases/latest"

const installScriptURL = "https://github.com/bezmoradi/t2/releases/latest/download/install.sh"

const goInstallPath = "github.com/bezmoradi/t2/cmd/t2"

const maxReleaseResponseBytes = 1 << 20

var updateCheckClient = &http.Client{Timeout: 5 * time.Second}

var updateCheckOptOuts = []string{"T2_NO_UPDATE_CHECK", "CI", "GITHUB_ACTIONS"}

func NotifyIfOutdated(w io.Writer) {
	if !updateCheckWanted() {
		return
	}
	latest := CheckVersion()
	if latest == "" {
		return
	}
	notice := fmt.Sprintf("\nT2 %s is available (you have %s). To update:\n  curl -fsSL %s | %ssh\nOr, with Go: go install %s@%s\nSet T2_NO_UPDATE_CHECK=1 to stop this check.\n%s\n",
		latest, VERSION, installScriptURL, installDirPrefix(), goInstallPath, latest, UpdateMessage)
	_, _ = io.WriteString(w, notice)
}

func updateCheckWanted() bool {
	for _, env := range updateCheckOptOuts {
		if os.Getenv(env) != "" {
			return false
		}
	}
	return semver.IsValid(VERSION)
}

func CheckVersion() string {
	req, err := http.NewRequest(http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "t2/"+VERSION)
	res, err := updateCheckClient.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ""
	}
	var release struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxReleaseResponseBytes)).Decode(&release); err != nil || release.Draft || release.Prerelease {
		return ""
	}
	if !isReleaseTag(release.TagName) || semver.Compare(release.TagName, VERSION) <= 0 {
		return ""
	}
	return release.TagName
}

func isReleaseTag(tag string) bool {
	return semver.IsValid(tag) && semver.Canonical(tag) == tag && semver.Prerelease(tag) == ""
}

func installDirPrefix() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	if !filepath.IsAbs(dir) || transcription.Sanitize(dir) != dir {
		return ""
	}
	return "T2_INSTALL_DIR='" + strings.ReplaceAll(dir, "'", `'\''`) + "' "
}
