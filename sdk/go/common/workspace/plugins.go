// Copyright 2016-2021, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package workspace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/cheggaaa/pb"
	"github.com/djherbis/times"
	"github.com/pkg/errors"

	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/archive"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/fsutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/httputil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
	"github.com/pulumi/pulumi/sdk/v3/go/common/version"
	"github.com/pulumi/pulumi/sdk/v3/nodejs/npm"
	"github.com/pulumi/pulumi/sdk/v3/python"
)

const (
	windowsGOOS = "windows"
)

var (
	enableLegacyPluginBehavior = os.Getenv("PULUMI_ENABLE_LEGACY_PLUGIN_SEARCH") != ""
)

// pluginDownloadURLOverrides is a variable instead of a constant so it can be set using the `-X` `ldflag` at build
// time, if necessary. When non-empty, it's parsed into `pluginDownloadURLOverridesParsed` in `init()`. The expected
// format is `regexp=URL`, and multiple pairs can be specified separated by commas, e.g. `regexp1=URL1,regexp2=URL2`.
//
// For example, when set to "^foo.*=https://foo,^bar.*=https://bar", plugin names that start with "foo" will use
// https://foo as the download URL and names that start with "bar" will use https://bar.
var pluginDownloadURLOverrides string

// pluginDownloadURLOverridesParsed is the parsed array from `pluginDownloadURLOverrides`.
var pluginDownloadURLOverridesParsed pluginDownloadOverrideArray

// pluginDownloadURLOverride represents a plugin download URL override, parsed from `pluginDownloadURLOverrides`.
type pluginDownloadURLOverride struct {
	reg *regexp.Regexp // The regex used to match against the plugin's name.
	url string         // The URL to use for the matched plugin.
}

// pluginDownloadOverrideArray represents an array of overrides.
type pluginDownloadOverrideArray []pluginDownloadURLOverride

// get returns the URL and true if name matches an override's regular expression,
// otherwise an empty string and false.
func (overrides pluginDownloadOverrideArray) get(name string) (string, bool) {
	for _, override := range overrides {
		if override.reg.MatchString(name) {
			return override.url, true
		}
	}
	return "", false
}

func init() {
	var err error
	if pluginDownloadURLOverridesParsed, err = parsePluginDownloadURLOverrides(pluginDownloadURLOverrides); err != nil {
		panic(fmt.Errorf("error parsing `pluginDownloadURLOverrides`: %w", err))
	}
}

// parsePluginDownloadURLOverrides parses an overrides string with the expected format `regexp1=URL1,regexp2=URL2`.
func parsePluginDownloadURLOverrides(overrides string) (pluginDownloadOverrideArray, error) {
	var result pluginDownloadOverrideArray
	if overrides == "" {
		return result, nil
	}
	for _, pair := range strings.Split(overrides, ",") {
		split := strings.Split(pair, "=")
		if len(split) != 2 || split[0] == "" || split[1] == "" {
			return nil, fmt.Errorf("expected format to be \"regexp1=URL1,regexp2=URL2\"; got %q", overrides)
		}
		reg, err := regexp.Compile(split[0])
		if err != nil {
			return nil, err
		}
		result = append(result, pluginDownloadURLOverride{
			reg: reg,
			url: split[1],
		})
	}
	return result, nil
}

// MissingError is returned by functions that attempt to load plugins if a plugin can't be located.
type MissingError struct {
	// Info contains information about the plugin that was not found.
	Info PluginInfo
	// includeAmbient is true if we search $PATH for this plugin
	includeAmbient bool
}

// NewMissingError allocates a new error indicating the given plugin info was not found.
func NewMissingError(info PluginInfo, includeAmbient bool) error {
	return &MissingError{
		Info:           info,
		includeAmbient: includeAmbient,
	}
}

func (err *MissingError) Error() string {
	includePath := ""
	if err.includeAmbient {
		includePath = " or on your $PATH"
	}

	if err.Info.Version != nil {
		return fmt.Sprintf("no %[1]s plugin 'pulumi-%[1]s-%[2]s' found in the workspace at version v%[3]s%[4]s, "+
			"install the plugin using `pulumi plugin install %[1]s %[2]s v%[3]s`",
			err.Info.Kind, err.Info.Name, err.Info.Version, includePath)
	}

	return fmt.Sprintf("no %[1]s plugin 'pulumi-%[1]s-%[2]s' found in the workspace%[3]s, "+
		"install the plugin using `pulumi plugin install %[1]s %[2]s`",
		err.Info.Kind, err.Info.Name, includePath)
}

// PluginSource deals with downloading a specific version of a plugin, or looking up the latest version of it.
type PluginSource interface {
	// Download fetches an io.ReadCloser for this plugin and also returns the size of the response (if known).
	Download(
		version semver.Version, opSy string, arch string,
		getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (io.ReadCloser, int64, error)
	// GetLatestVersion tries to find the latest version for this plugin. This is currently only supported for
	// plugins we can get from github releases.
	GetLatestVersion(getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (*semver.Version, error)
}

// getPulumiSource can download a plugin from get.pulumi.com
type getPulumiSource struct {
	name string
	kind PluginKind
}

func newGetPulumiSource(name string, kind PluginKind) *getPulumiSource {
	return &getPulumiSource{name: name, kind: kind}
}

func (source *getPulumiSource) GetLatestVersion(
	getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (*semver.Version, error) {
	return nil, errors.New("GetLatestVersion is not supported for plugins from get.pulumi.com")
}

func (source *getPulumiSource) Download(
	version semver.Version, opSy string, arch string,
	getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (io.ReadCloser, int64, error) {
	serverURL := "https://get.pulumi.com/releases/plugins"

	logging.V(1).Infof("%s downloading from %s", source.name, serverURL)

	serverURL = interpolateURL(serverURL, version, opSy, arch)
	serverURL = strings.TrimSuffix(serverURL, "/")

	logging.V(1).Infof("%s downloading from %s", source.name, serverURL)
	endpoint := fmt.Sprintf("%s/%s",
		serverURL,
		url.QueryEscape(fmt.Sprintf("pulumi-%s-%s-v%s-%s-%s.tar.gz", source.kind, source.name, version.String(), opSy, arch)))

	req, err := buildHTTPRequest(endpoint, "")
	if err != nil {
		return nil, -1, err
	}
	return getHTTPResponse(req)
}

// githubSource can download a plugin from github releases
type githubSource struct {
	organization string
	name         string
	kind         PluginKind

	token string
}

// Creates a new github source adding authentication data in the environment, if it exists
func newGithubSource(organization, name string, kind PluginKind) *githubSource {

	// 14-03-2022 we stopped looking at GITHUB_PERSONAL_ACCESS_TOKEN and sending basic auth for github and
	// instead just look at GITHUB_TOKEN and send in a header. Given GITHUB_PERSONAL_ACCESS_TOKEN was an
	// envvar we made up we check to see if it's set here and log a warning. This can be removed after a few
	// releases.
	if os.Getenv("GITHUB_PERSONAL_ACCESS_TOKEN") != "" {
		logging.Warningf("GITHUB_PERSONAL_ACCESS_TOKEN is no longer used for Github authentication, set GITHUB_TOKEN instead")
	}

	return &githubSource{
		organization: organization,
		name:         name,
		kind:         kind,

		token: os.Getenv("GITHUB_TOKEN"),
	}
}

func (source *githubSource) HasAuthentication() bool {
	return source.token != ""
}

func (source *githubSource) GetLatestVersion(
	getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (*semver.Version, error) {
	releaseURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/pulumi-%s/releases/latest",
		source.organization, source.name)
	logging.V(9).Infof("plugin GitHub releases url: %s", releaseURL)
	req, err := buildHTTPRequest(releaseURL, source.token)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, length, err := getHTTPResponse(req)
	if err != nil {
		return nil, err
	}
	jsonBody, err := ioutil.ReadAll(resp)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal github response len(%d): %s", length, err.Error())
	}
	release := struct {
		TagName string `json:"tag_name"`
	}{}
	err = json.Unmarshal(jsonBody, &release)
	if err != nil {
		return nil, err
	}
	parsedVersion, err := semver.ParseTolerant(release.TagName)
	if err != nil {
		return nil, fmt.Errorf("invalid plugin semver: %w", err)
	}
	return &parsedVersion, nil
}

func (source *githubSource) Download(
	version semver.Version, opSy string, arch string,
	getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (io.ReadCloser, int64, error) {
	if !source.HasAuthentication() {
		// If we're not using authentication we can just download from the release/download URL

		logging.V(1).Infof(
			"%s downloading from github.com/%s/pulumi-%s/releases",
			source.name, source.organization, source.name)

		pluginURL := fmt.Sprintf("https://github.com/%s/pulumi-%s/releases/download/v%s/%s",
			source.organization, source.name, version.String(), url.QueryEscape(fmt.Sprintf("pulumi-%s-%s-v%s-%s-%s.tar.gz",
				source.kind, source.name, version.String(), opSy, arch)))

		req, err := buildHTTPRequest(pluginURL, "")
		if err != nil {
			return nil, -1, err
		}
		return getHTTPResponse(req)
	}

	// If we are using authentication we need to lookup the asset via the github releases API
	assetName := fmt.Sprintf("pulumi-%s-%s-v%s-%s-%s.tar.gz", source.kind, source.name, version.String(), opSy, arch)

	releaseURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/pulumi-%s/releases/tags/v%s",
		source.organization, source.name, version.String())
	logging.V(9).Infof("plugin GitHub releases url: %s", releaseURL)

	req, err := buildHTTPRequest(releaseURL, source.token)
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("Accept", "application/json")
	resp, length, err := getHTTPResponse(req)
	if err != nil {
		return nil, -1, err
	}
	jsonBody, err := ioutil.ReadAll(resp)
	if err != nil {
		logging.V(9).Infof("cannot unmarshal github response len(%d): %s", length, err.Error())
		return nil, -1, err
	}
	release := struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}{}
	err = json.Unmarshal(jsonBody, &release)
	if err != nil {
		logging.V(9).Infof("github json response: %s", jsonBody)
		logging.V(9).Infof("cannot unmarshal github response: %s", err.Error())
		return nil, -1, err
	}
	assetURL := ""
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			assetURL = asset.URL
		}
	}
	if assetURL == "" {
		logging.V(9).Infof("github json response: %s", jsonBody)
		logging.V(9).Infof("plugin asset '%s' not found", assetName)
		return nil, -1, errors.Errorf("plugin asset '%s' not found", assetName)
	}

	logging.V(1).Infof("%s downloading from %s", source.name, assetURL)

	req, err = buildHTTPRequest(assetURL, source.token)
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	return getHTTPResponse(req)
}

// pluginURLSource can download a plugin from a given PluginDownloadURL, it doesn't support GetLatestVersion
type pluginURLSource struct {
	name              string
	kind              PluginKind
	pluginDownloadURL string
}

func newPluginURLSource(name string, kind PluginKind, pluginDownloadURL string) *pluginURLSource {
	return &pluginURLSource{
		name:              name,
		kind:              kind,
		pluginDownloadURL: pluginDownloadURL,
	}
}

func (source *pluginURLSource) GetLatestVersion(
	getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (*semver.Version, error) {
	return nil, errors.New("GetLatestVersion is not supported for plugins using PluginDownloadURL")
}

func (source *pluginURLSource) Download(
	version semver.Version, opSy string, arch string,
	getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (io.ReadCloser, int64, error) {
	serverURL := source.pluginDownloadURL
	logging.V(1).Infof("%s downloading from %s", source.name, serverURL)

	serverURL = interpolateURL(serverURL, version, opSy, arch)
	serverURL = strings.TrimSuffix(serverURL, "/")

	logging.V(1).Infof("%s downloading from %s", source.name, serverURL)
	endpoint := fmt.Sprintf("%s/%s",
		serverURL,
		url.QueryEscape(fmt.Sprintf("pulumi-%s-%s-v%s-%s-%s.tar.gz", source.kind, source.name, version.String(), opSy, arch)))

	req, err := buildHTTPRequest(endpoint, "")
	if err != nil {
		return nil, -1, err
	}
	return getHTTPResponse(req)
}

// fallbackSource handles our current complicated default logic of trying the pulumi public github, then maybe
// the users private github, then get.pulumi.com
type fallbackSource struct {
	name string
	kind PluginKind
}

func newFallbackSource(name string, kind PluginKind) *fallbackSource {
	return &fallbackSource{
		name: name,
		kind: kind,
	}
}

func (source *fallbackSource) GetLatestVersion(
	getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (*semver.Version, error) {
	// Try and get this package from public pulumi github
	public := newGithubSource("pulumi", source.name, source.kind)
	version, err := public.GetLatestVersion(getHTTPResponse)
	if err == nil {
		return version, nil
	}

	// Are we in experimental mode? Try a users private github release
	if _, ok := os.LookupEnv("PULUMI_EXPERIMENTAL"); ok {
		// Check if we have a repo owner set
		repoOwner := os.Getenv("GITHUB_REPOSITORY_OWNER")
		var privateErr error
		if repoOwner == "" {
			privateErr = errors.New("ENV[GITHUB_REPOSITORY_OWNER] not set")
		} else {
			private := newGithubSource(repoOwner, source.name, source.kind)
			if !private.HasAuthentication() {
				privateErr = errors.New("no GitHub authentication information provided")
			} else {
				version, privateErr = private.GetLatestVersion(getHTTPResponse)
				if privateErr == nil {
					return version, nil
				}
			}
		}

		logging.V(1).Infof("cannot find plugin %s on private GitHub releases: %s", source.name, privateErr.Error())

		return nil, fmt.Errorf(
			"error getting version from Pulumi github: %w\nand from private github: %s",
			err, privateErr.Error())
	}

	return nil, err
}

func (source *fallbackSource) Download(
	version semver.Version, opSy string, arch string,
	getHTTPResponse func(*http.Request) (io.ReadCloser, int64, error)) (io.ReadCloser, int64, error) {
	// Try and get this package from public pulumi github
	public := newGithubSource("pulumi", source.name, source.kind)
	resp, length, err := public.Download(version, opSy, arch, getHTTPResponse)
	if err == nil {
		return resp, length, nil
	}

	// Are we in experimental mode? Try a private github release
	if _, ok := os.LookupEnv("PULUMI_EXPERIMENTAL"); ok {
		// Check if we have a repo owner set
		repoOwner := os.Getenv("GITHUB_REPOSITORY_OWNER")
		if repoOwner == "" {
			err = errors.New("ENV[GITHUB_REPOSITORY_OWNER] not set")
		} else {
			private := newGithubSource(repoOwner, source.name, source.kind)
			if !private.HasAuthentication() {
				err = errors.New("no GitHub authentication information provided")
			} else {
				resp, length, err := private.Download(version, opSy, arch, getHTTPResponse)
				if err == nil {
					return resp, length, nil
				}
			}
		}

		logging.V(1).Infof("cannot find plugin %s on private GitHub releases: %s", source.name, err.Error())
	}

	// Fallback to get.pulumi.com
	pulumi := newGetPulumiSource(source.name, source.kind)
	return pulumi.Download(version, opSy, arch, getHTTPResponse)
}

// PluginInfo provides basic information about a plugin.  Each plugin gets installed into a system-wide
// location, by default `~/.pulumi/plugins/<kind>-<name>-<version>/`.  A plugin may contain multiple files,
// however the primary loadable executable must be named `pulumi-<kind>-<name>`.
type PluginInfo struct {
	Name              string          // the simple name of the plugin.
	Path              string          // the path that a plugin was loaded from.
	Kind              PluginKind      // the kind of the plugin (language, resource, etc).
	Version           *semver.Version // the plugin's semantic version, if present.
	Size              int64           // the size of the plugin, in bytes.
	InstallTime       time.Time       // the time the plugin was installed.
	LastUsedTime      time.Time       // the last time the plugin was used.
	PluginDownloadURL string          // an optional server to use when downloading this plugin.
	PluginDir         string          // if set, will be used as the root plugin dir instead of ~/.pulumi/plugins.
}

// Dir gets the expected plugin directory for this plugin.
func (info PluginInfo) Dir() string {
	dir := fmt.Sprintf("%s-%s", info.Kind, info.Name)
	if info.Version != nil {
		dir = fmt.Sprintf("%s-v%s", dir, info.Version.String())
	}
	return dir
}

// File gets the expected filename for this plugin.
func (info PluginInfo) File() string {
	return info.FilePrefix() + info.FileSuffix()
}

// FilePrefix gets the expected default file prefix for the plugin.
func (info PluginInfo) FilePrefix() string {
	return fmt.Sprintf("pulumi-%s-%s", info.Kind, info.Name)
}

// FileSuffix returns the suffix for the plugin (if any).
func (info PluginInfo) FileSuffix() string {
	if runtime.GOOS == windowsGOOS {
		return ".exe"
	}
	return ""
}

// DirPath returns the directory where this plugin should be installed.
func (info PluginInfo) DirPath() (string, error) {
	var err error
	dir := info.PluginDir
	if dir == "" {
		dir, err = GetPluginDir()
		if err != nil {
			return "", err
		}
	}

	return filepath.Join(dir, info.Dir()), nil
}

// LockFilePath returns the full path to the plugin's lock file used during installation
// to prevent concurrent installs.
func (info PluginInfo) LockFilePath() (string, error) {
	dir, err := info.DirPath()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.lock", dir), nil
}

// PartialFilePath returns the full path to the plugin's partial file used during installation
// to indicate installation of the plugin hasn't completed yet.
func (info PluginInfo) PartialFilePath() (string, error) {
	dir, err := info.DirPath()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.partial", dir), nil
}

// FilePath returns the full path where this plugin's primary executable should be installed.
func (info PluginInfo) FilePath() (string, error) {
	dir, err := info.DirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, info.File()), nil
}

// Delete removes the plugin from the cache.  It also deletes any supporting files in the cache, which includes
// any files that contain the same prefix as the plugin itself.
func (info PluginInfo) Delete() error {
	dir, err := info.DirPath()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// Attempt to delete any leftover .partial or .lock files.
	// Don't fail the operation if we can't delete these.
	contract.IgnoreError(os.Remove(fmt.Sprintf("%s.partial", dir)))
	contract.IgnoreError(os.Remove(fmt.Sprintf("%s.lock", dir)))
	return nil
}

// SetFileMetadata adds extra metadata from the given file, representing this plugin's directory.
func (info *PluginInfo) SetFileMetadata(path string) error {
	// Get the file info.
	file, err := os.Stat(path)
	if err != nil {
		return err
	}

	// Next, get the size from the directory (or, if there is none, just the file).
	size, err := getPluginSize(path)
	if err != nil {
		return errors.Wrapf(err, "getting plugin dir %s size", path)
	}
	info.Size = size

	// Next get the access times from the plugin binary itself.
	tinfo := times.Get(file)

	if tinfo.HasBirthTime() {
		info.InstallTime = tinfo.BirthTime()
	}

	info.LastUsedTime = tinfo.AccessTime()
	return nil
}

func interpolateURL(serverURL string, version semver.Version, os, arch string) string {
	replacer := strings.NewReplacer(
		"${VERSION}", url.QueryEscape(version.String()),
		"${OS}", url.QueryEscape(os),
		"${ARCH}", url.QueryEscape(arch))
	return replacer.Replace(serverURL)
}

func (info PluginInfo) GetSource() PluginSource {
	// The plugin has a set URL use that.
	if info.PluginDownloadURL != "" {
		return newPluginURLSource(info.Name, info.Kind, info.PluginDownloadURL)
	}

	// If the plugin name matches an override, download the plugin from the override URL.
	if url, ok := pluginDownloadURLOverridesParsed.get(info.Name); ok {
		return newPluginURLSource(info.Name, info.Kind, url)
	}

	// Use our default fallback behaviour of github then get.pulumi.com
	return newFallbackSource(info.Name, info.Kind)
}

// GetLatestVersion tries to find the latest version for this plugin. This is currently only supported for
// plugins we can get from github releases.
func (info PluginInfo) GetLatestVersion() (*semver.Version, error) {
	source := info.GetSource()
	return source.GetLatestVersion(getHTTPResponse)
}

// Download fetches an io.ReadCloser for this plugin and also returns the size of the response (if known).
func (info PluginInfo) Download() (io.ReadCloser, int64, error) {
	// Figure out the OS/ARCH pair for the download URL.
	var opSy string
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		opSy = runtime.GOOS
	default:
		return nil, -1, errors.Errorf("unsupported plugin OS: %s", runtime.GOOS)
	}
	var arch string
	switch runtime.GOARCH {
	case "amd64", "arm64":
		arch = runtime.GOARCH
	default:
		return nil, -1, errors.Errorf("unsupported plugin architecture: %s", runtime.GOARCH)
	}

	// The plugin version is necessary for the endpoint. If it's not present, return an error.
	if info.Version == nil {
		return nil, -1, errors.Errorf("unknown version for plugin %s", info.Name)
	}

	source := info.GetSource()
	return source.Download(*info.Version, opSy, arch, getHTTPResponse)
}

func buildHTTPRequest(pluginEndpoint string, token string) (*http.Request, error) {
	req, err := http.NewRequest("GET", pluginEndpoint, nil)
	if err != nil {
		return nil, err
	}

	userAgent := fmt.Sprintf("pulumi-cli/1 (%s; %s)", version.Version, runtime.GOOS)
	req.Header.Set("User-Agent", userAgent)

	if token != "" {
		req.Header.Set("Authorization", fmt.Sprintf("token %s", token))
	}

	return req, nil
}

func getHTTPResponse(req *http.Request) (io.ReadCloser, int64, error) {
	logging.V(9).Infof("full plugin download url: %s", req.URL)
	logging.V(9).Infof("plugin install request headers: %v", req.Header)

	resp, err := httputil.DoWithRetry(req, http.DefaultClient)
	if err != nil {
		return nil, -1, err
	}

	logging.V(9).Infof("plugin install response headers: %v", resp.Header)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {

		errmsg := "%d HTTP error fetching plugin from %s"

		if req.URL.Host == "api.github.com" && resp.StatusCode == 404 {
			errmsg += ". If this is a private GitHub repository, try " +
				"providing a token via the GITHUB_TOKEN environment variable. " +
				"See: https://github.com/settings/tokens"
		}

		return nil, -1, errors.Errorf(errmsg, resp.StatusCode, req.URL)
	}

	return resp.Body, resp.ContentLength, nil
}

// installLock acquires a file lock used to prevent concurrent installs.
func (info PluginInfo) installLock() (unlock func(), err error) {
	finalDir, err := info.DirPath()
	if err != nil {
		return nil, err
	}
	lockFilePath := fmt.Sprintf("%s.lock", finalDir)

	if err := os.MkdirAll(filepath.Dir(lockFilePath), 0700); err != nil {
		return nil, errors.Wrap(err, "creating plugin root")
	}

	mutex := fsutil.NewFileMutex(lockFilePath)
	if err := mutex.Lock(); err != nil {
		return nil, err
	}
	return func() {
		contract.IgnoreError(mutex.Unlock())
	}, nil
}

// Install installs a plugin's tarball into the cache. It validates that plugin names are in the expected format.
// Previous versions of Pulumi extracted the tarball to a temp directory first, and then renamed the temp directory
// to the final directory. The rename operation fails often enough on Windows due to aggressive virus scanners opening
// files in the temp directory. To address this, we now extract the tarball directly into the final directory, and use
// file locks to prevent concurrent installs.
// Each plugin has its own file lock, with the same name as the plugin directory, with a `.lock` suffix.
// During installation an empty file with a `.partial` suffix is created, indicating that installation is in-progress.
// The `.partial` file is deleted when installation is complete, indicating that the plugin has finished installing.
// If a failure occurs during installation, the `.partial` file will remain, indicating the plugin wasn't fully
// installed. The next time the plugin is installed, the old installation directory will be removed and replaced with
// a fresh install.
func (info PluginInfo) Install(tgz io.ReadCloser, reinstall bool) error {
	defer contract.IgnoreClose(tgz)

	// Fetch the directory into which we will expand this tarball.
	finalDir, err := info.DirPath()
	if err != nil {
		return err
	}

	// Create a file lock file at <pluginsdir>/<kind>-<name>-<version>.lock.
	unlock, err := info.installLock()
	if err != nil {
		return err
	}
	defer unlock()

	// Cleanup any temp dirs from failed installations of this plugin from previous versions of Pulumi.
	if err := cleanupTempDirs(finalDir); err != nil {
		// We don't want to fail the installation if there was an error cleaning up these old temp dirs.
		// Instead, log the error and continue on.
		logging.V(5).Infof("Install: Error cleaning up temp dirs: %s", err.Error())
	}

	// Get the partial file path (e.g. <pluginsdir>/<kind>-<name>-<version>.partial).
	partialFilePath, err := info.PartialFilePath()
	if err != nil {
		return err
	}

	// Check whether the directory exists while we were waiting on the lock.
	_, finalDirStatErr := os.Stat(finalDir)
	if finalDirStatErr == nil {
		_, partialFileStatErr := os.Stat(partialFilePath)
		if partialFileStatErr != nil {
			if !os.IsNotExist(partialFileStatErr) {
				return partialFileStatErr
			}
			if !reinstall {
				// finalDir exists, there's no partial file, and we're not reinstalling, so the plugin is already
				// installed.
				return nil
			}
		}

		// Either the partial file exists--meaning a previous attempt at installing the plugin failed--or we're
		// deliberately reinstalling the plugin. Delete finalDir so we can try installing again. There's no need to
		// delete the partial file since we'd just be recreating it again below anyway.
		if err := os.RemoveAll(finalDir); err != nil {
			return err
		}
	} else if !os.IsNotExist(finalDirStatErr) {
		return finalDirStatErr
	}

	// Create an empty partial file to indicate installation is in-progress.
	if err := ioutil.WriteFile(partialFilePath, nil, 0600); err != nil {
		return err
	}

	// Create the final directory.
	if err := os.MkdirAll(finalDir, 0700); err != nil {
		return err
	}

	// Uncompress the plugin.
	if err := archive.ExtractTGZ(tgz, finalDir); err != nil {
		return err
	}

	// Even though we deferred closing the tarball at the beginning of this function, go ahead and explicitly close
	// it now since we're finished extracting it, to prevent subsequent output from being displayed oddly with
	// the progress bar.
	contract.IgnoreClose(tgz)

	// Install dependencies, if needed.
	proj, err := LoadPluginProject(filepath.Join(finalDir, "PulumiPlugin.yaml"))
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "loading PulumiPlugin.yaml")
	}
	if proj != nil {
		runtime := strings.ToLower(proj.Runtime.Name())
		// For now, we only do this for Node.js and Python. For Go, the expectation is the binary is
		// already built. For .NET, similarly, a single self-contained binary could be used, but
		// otherwise `dotnet run` will implicitly run `dotnet restore`.
		// TODO[pulumi/pulumi#1334]: move to the language plugins so we don't have to hard code here.
		switch runtime {
		case "nodejs":
			var b bytes.Buffer
			if _, err := npm.Install(finalDir, true /* production */, &b, &b); err != nil {
				os.Stderr.Write(b.Bytes())
				return errors.Wrap(err, "installing plugin dependencies")
			}
		case "python":
			if err := python.InstallDependencies(finalDir, "venv", false /*showOutput*/); err != nil {
				return errors.Wrap(err, "installing plugin dependencies")
			}
		}
	}

	// Installation is complete. Remove the partial file.
	return os.Remove(partialFilePath)
}

// cleanupTempDirs cleans up leftover temp dirs from failed installs with previous versions of Pulumi.
func cleanupTempDirs(finalDir string) error {
	dir := filepath.Dir(finalDir)

	infos, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, info := range infos {
		// Temp dirs have a suffix of `.tmpXXXXXX` (where `XXXXXX`) is a random number,
		// from ioutil.TempFile.
		if info.IsDir() && installingPluginRegexp.MatchString(info.Name()) {
			path := filepath.Join(dir, info.Name())
			if err := os.RemoveAll(path); err != nil {
				return errors.Wrapf(err, "cleaning up temp dir %s", path)
			}
		}
	}

	return nil
}

func (info PluginInfo) String() string {
	var version string
	if v := info.Version; v != nil {
		version = fmt.Sprintf("-%s", v)
	}
	return info.Name + version
}

// PluginKind represents a kind of a plugin that may be dynamically loaded and used by Pulumi.
type PluginKind string

const (
	// AnalyzerPlugin is a plugin that can be used as a resource analyzer.
	AnalyzerPlugin PluginKind = "analyzer"
	// LanguagePlugin is a plugin that can be used as a language host.
	LanguagePlugin PluginKind = "language"
	// ResourcePlugin is a plugin that can be used as a resource provider for custom CRUD operations.
	ResourcePlugin PluginKind = "resource"
)

// IsPluginKind returns true if k is a valid plugin kind, and false otherwise.
func IsPluginKind(k string) bool {
	switch PluginKind(k) {
	case AnalyzerPlugin, LanguagePlugin, ResourcePlugin:
		return true
	default:
		return false
	}
}

// HasPlugin returns true if the given plugin exists.
func HasPlugin(plug PluginInfo) bool {
	dir, err := plug.DirPath()
	if err == nil {
		_, err := os.Stat(dir)
		if err == nil {
			partialFilePath, err := plug.PartialFilePath()
			if err == nil {
				if _, err := os.Stat(partialFilePath); os.IsNotExist(err) {
					return true
				}
			}
		}
	}
	return false
}

// HasPluginGTE returns true if the given plugin exists at the given version number or greater.
func HasPluginGTE(plug PluginInfo) (bool, error) {
	// If an exact match, return true right away.
	if HasPlugin(plug) {
		return true, nil
	}

	// Otherwise, load up the list of plugins and find one with the same name/type and >= version.
	plugs, err := GetPlugins()
	if err != nil {
		return false, err
	}

	// If we're not doing the legacy plugin behavior and we've been asked for a specific version, do the same plugin
	// search that we'd do at runtime. This ensures that `pulumi plugin install` works the same way that the runtime
	// loader does, to minimize confusion when a user has to install new plugins.
	if !enableLegacyPluginBehavior && plug.Version != nil {
		requestedVersion := semver.MustParseRange(plug.Version.String())
		_, err := SelectCompatiblePlugin(plugs, plug.Kind, plug.Name, requestedVersion)
		return err == nil, err
	}

	for _, p := range plugs {
		if p.Name == plug.Name &&
			p.Kind == plug.Kind &&
			(p.Version != nil && plug.Version != nil && p.Version.GTE(*plug.Version)) {
			return true, nil
		}
	}
	return false, nil
}

// GetPolicyDir returns the directory in which an organization's Policy Packs on the current machine are managed.
func GetPolicyDir(orgName string) (string, error) {
	return GetPulumiPath(PolicyDir, orgName)
}

// GetPolicyPath finds a PolicyPack by its name version, as well as a bool marked true if the path
// already exists and is a directory.
func GetPolicyPath(orgName, name, version string) (string, bool, error) {
	policiesDir, err := GetPolicyDir(orgName)
	if err != nil {
		return "", false, err
	}

	policyPackPath := path.Join(policiesDir, fmt.Sprintf("pulumi-analyzer-%s-v%s", name, version))

	file, err := os.Stat(policyPackPath)
	if err == nil && file.IsDir() {
		// PolicyPack exists. Return.
		return policyPackPath, true, nil
	} else if err != nil && !os.IsNotExist(err) {
		// Error trying to inspect PolicyPack FS entry. Return error.
		return "", false, err
	}

	// Not found. Return empty path.
	return policyPackPath, false, nil
}

// GetPluginDir returns the directory in which plugins on the current machine are managed.
func GetPluginDir() (string, error) {
	return GetPulumiPath(PluginDir)
}

// GetPlugins returns a list of installed plugins without size info and last accessed metadata.
// Plugin size requires recursively traversing the plugin directory, which can be extremely
// expensive with the introduction of nodejs multilang components that have
// deeply nested node_modules folders.
func GetPlugins() ([]PluginInfo, error) {
	// To get the list of plugins, simply scan the directory in the usual place.
	dir, err := GetPluginDir()
	if err != nil {
		return nil, err
	}
	return getPlugins(dir, true /* skipMetadata */)
}

// GetPluginsWithMetadata returns a list of installed plugins with metadata about size,
// and last access (POOR RUNTIME PERF). Plugin size requires recursively traversing the
// plugin directory, which can be extremely expensive with the introduction of
// nodejs multilang components that have deeply nested node_modules folders.
func GetPluginsWithMetadata() ([]PluginInfo, error) {
	// To get the list of plugins, simply scan the directory in the usual place.
	dir, err := GetPluginDir()
	if err != nil {
		return nil, err
	}
	return getPlugins(dir, false /* skipMetadata */)
}

func getPlugins(dir string, skipMetadata bool) ([]PluginInfo, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Now read the file infos and create the plugin infos.
	var plugins []PluginInfo
	for _, file := range files {
		// Skip anything that doesn't look like a plugin.
		if kind, name, version, ok := tryPlugin(file); ok {
			plugin := PluginInfo{
				Name:    name,
				Kind:    kind,
				Version: &version,
			}
			path := filepath.Join(dir, file.Name())
			if _, err := os.Stat(fmt.Sprintf("%s.partial", path)); err == nil {
				// Skip it if the partial file exists, meaning the plugin is not fully installed.
				continue
			} else if !os.IsNotExist(err) {
				return nil, err
			}
			// computing plugin sizes can be very expensive (nested node_modules)
			if !skipMetadata {
				if err = plugin.SetFileMetadata(path); err != nil {
					return nil, err
				}
			}
			plugins = append(plugins, plugin)
		}
	}
	return plugins, nil
}

// GetPluginPath finds a plugin's path by its kind, name, and optional version.  It will match the latest version that
// is >= the version specified.  If no version is supplied, the latest plugin for that given kind/name pair is loaded,
// using standard semver sorting rules.  A plugin may be overridden entirely by placing it on your $PATH, though it is
// possible to opt out of this behavior by setting PULUMI_IGNORE_AMBIENT_PLUGINS to any non-empty value.
func GetPluginPath(kind PluginKind, name string, version *semver.Version) (string, string, error) {
	var filename string

	// We currently bundle some plugins with "pulumi" and thus expect them to be next to the pulumi binary. We
	// also always allow these plugins to be picked up from PATH even if PULUMI_IGNORE_AMBIENT_PLUGINS is set.
	// Eventually we want to fix this so new plugins are true plugins in the plugin cache.
	isBundled := kind == LanguagePlugin ||
		(kind == ResourcePlugin && name == "pulumi-nodejs") ||
		(kind == ResourcePlugin && name == "pulumi-python")

	// If we have a version of the plugin on its $PATH, use it, unless we have opted out of this behavior explicitly.
	// This supports development scenarios.
	optOut, isFound := os.LookupEnv("PULUMI_IGNORE_AMBIENT_PLUGINS")
	includeAmbient := !(isFound && cmdutil.IsTruthy(optOut)) || isBundled
	if includeAmbient {
		filename = (&PluginInfo{Kind: kind, Name: name, Version: version}).FilePrefix()
		if path, err := exec.LookPath(filename); err == nil {
			logging.V(6).Infof("GetPluginPath(%s, %s, %v): found on $PATH %s", kind, name, version, path)
			return "", path, nil
		}
	}

	// At some point in the future, bundled plugins will be located in the plugin cache, just like regular
	// plugins (see pulumi/pulumi#956 for some of the reasons why this isn't the case today). For now, they
	// ship next to the `pulumi` binary. While we encourage this folder to be on the $PATH (and so the check
	// above would have found the plugin) it's possible someone is running `pulumi` with an explicit path on
	// the command line or has done symlink magic such that `pulumi` is on the path, but the bundled plugins
	// are not. So, if possible, look next to the instance of `pulumi` that is running to find this bundled
	// plugin.
	if isBundled {
		exePath, exeErr := os.Executable()
		if exeErr == nil {
			fullPath, fullErr := filepath.EvalSymlinks(exePath)
			if fullErr == nil {
				for _, ext := range getCandidateExtensions() {
					candidate := filepath.Join(filepath.Dir(fullPath), filename+ext)
					// Let's see if the file is executable. On Windows, os.Stat() returns a mode of "-rw-rw-rw" so on
					// on windows we just trust the fact that the .exe can actually be launched.
					if stat, err := os.Stat(candidate); err == nil &&
						(stat.Mode()&0100 != 0 || runtime.GOOS == windowsGOOS) {
						logging.V(6).Infof("GetPluginPath(%s, %s, %v): found next to current executable %s",
							kind, name, version, candidate)

						return "", candidate, nil
					}
				}
			}
		}
	}

	// Otherwise, check the plugin cache.
	plugins, err := GetPlugins()
	if err != nil {
		return "", "", errors.Wrapf(err, "loading plugin list")
	}

	var match *PluginInfo
	if !enableLegacyPluginBehavior && version != nil {
		logging.V(6).Infof("GetPluginPath(%s, %s, %s): enabling new plugin behavior", kind, name, version)
		candidate, err := SelectCompatiblePlugin(plugins, kind, name, semver.MustParseRange(version.String()))
		if err != nil {
			return "", "", NewMissingError(PluginInfo{
				Name:    name,
				Kind:    kind,
				Version: version,
			}, includeAmbient)
		}
		match = &candidate
	} else {
		for _, cur := range plugins {
			// Since the value of cur changes as we iterate, we can't save a pointer to it. So let's have a local that
			// we can take a pointer to if this plugin is the best match yet.
			plugin := cur
			if plugin.Kind == kind && plugin.Name == name {
				// Always pick the most recent version of the plugin available.  Even if this is an exact match, we
				// keep on searching just in case there's a newer version available.
				var m *PluginInfo
				if match == nil && version == nil {
					m = &plugin // no existing match, no version spec, take it.
				} else if match != nil &&
					(match.Version == nil || (plugin.Version != nil && plugin.Version.GT(*match.Version))) {
					m = &plugin // existing match, but this plugin is newer, prefer it.
				} else if version != nil && plugin.Version != nil && plugin.Version.GTE(*version) {
					m = &plugin // this plugin is >= the version being requested, use it.
				}

				if m != nil {
					match = m
					logging.V(6).Infof("GetPluginPath(%s, %s, %s): found candidate (#%s)",
						kind, name, version, match.Version)
				}
			}
		}
	}

	if match != nil {
		matchDir, err := match.DirPath()
		if err != nil {
			return "", "", err
		}
		matchPath, err := match.FilePath()
		if err != nil {
			return "", "", err
		}

		logging.V(6).Infof("GetPluginPath(%s, %s, %v): found in cache at %s", kind, name, version, matchPath)
		return matchDir, matchPath, nil
	}

	return "", "", NewMissingError(PluginInfo{
		Name:    name,
		Kind:    kind,
		Version: version,
	}, includeAmbient)
}

// SortedPluginInfo is a wrapper around PluginInfo that allows for sorting by version.
type SortedPluginInfo []PluginInfo

func (sp SortedPluginInfo) Len() int { return len(sp) }
func (sp SortedPluginInfo) Less(i, j int) bool {
	iVersion := sp[i].Version
	jVersion := sp[j].Version
	switch {
	case iVersion == nil && jVersion == nil:
		return false
	case iVersion == nil:
		return true
	case jVersion == nil:
		return false
	case iVersion.EQ(*jVersion):
		return iVersion.String() < jVersion.String()
	default:
		return iVersion.LT(*jVersion)
	}
}
func (sp SortedPluginInfo) Swap(i, j int) { sp[i], sp[j] = sp[j], sp[i] }

// SelectCompatiblePlugin selects a plugin from the list of plugins with the given kind and name that sastisfies the
// requested semver range. It returns the highest version plugin that satisfies the requested constraints, or an error
// if no such plugin could be found.
//
// If there exist plugins in the plugin list that don't have a version, SelectCompatiblePlugin will select them if there
// are no other compatible plugins available.
func SelectCompatiblePlugin(
	plugins []PluginInfo, kind PluginKind, name string, requested semver.Range) (PluginInfo, error) {
	logging.V(7).Infof("SelectCompatiblePlugin(..., %s): beginning", name)
	var bestMatch PluginInfo
	var hasMatch bool

	// Before iterating over the list of plugins, sort the list of plugins by version in ascending order. This ensures
	// that we can do a single pass over the plugin list, from lowest version to greatest version, and be confident that
	// the best match that we find at the end is the greatest possible compatible version for the requested plugin.
	//
	// Plugins without versions are treated as having the lowest version. Ties between plugins without versions are
	// resolved arbitrarily.
	sort.Sort(SortedPluginInfo(plugins))
	for _, plugin := range plugins {
		switch {
		case plugin.Kind != kind || plugin.Name != name:
			// Not the plugin we're looking for.
		case !hasMatch && plugin.Version == nil:
			// This is the plugin we're looking for, but it doesn't have a version. We haven't seen anything better yet,
			// so take it.
			logging.V(7).Infof(
				"SelectCompatiblePlugin(..., %s): best plugin %s: no version and no other candidates",
				name, plugin.String())
			hasMatch = true
			bestMatch = plugin
		case plugin.Version == nil:
			// This is a rare case - we've already seen a version-less plugin and we're seeing another here. Ignore this
			// one and defer to the one we previously selected.
			logging.V(7).Infof("SelectCompatiblePlugin(..., %s): skipping plugin %s: no version", name, plugin.String())
		case requested(*plugin.Version):
			// This plugin is compatible with the requested semver range. Save it as the best match and continue.
			logging.V(7).Infof("SelectCompatiblePlugin(..., %s): best plugin %s: semver match", name, plugin.String())
			hasMatch = true
			bestMatch = plugin
		default:
			logging.V(7).Infof(
				"SelectCompatiblePlugin(..., %s): skipping plugin %s: semver mismatch", name, plugin.String())
		}
	}

	if !hasMatch {
		logging.V(7).Infof("SelectCompatiblePlugin(..., %s): failed to find match", name)
		return PluginInfo{}, errors.New("failed to locate compatible plugin")
	}
	logging.V(7).Infof("SelectCompatiblePlugin(..., %s): selecting plugin '%s': best match ", name, bestMatch.String())
	return bestMatch, nil
}

// ReadCloserProgressBar displays a progress bar for the given closer and returns a wrapper closer to manipulate it.
func ReadCloserProgressBar(
	closer io.ReadCloser, size int64, message string, colorization colors.Colorization) io.ReadCloser {
	if size == -1 {
		return closer
	}

	if !cmdutil.Interactive() {
		return closer
	}

	// If we know the length of the download, show a progress bar.
	bar := pb.New(int(size))
	bar.Output = os.Stderr
	bar.Prefix(colorization.Colorize(colors.SpecUnimportant + message + ":"))
	bar.Postfix(colorization.Colorize(colors.Reset))
	bar.SetMaxWidth(80)
	bar.SetUnits(pb.U_BYTES)
	bar.Start()

	return &barCloser{
		bar:        bar,
		readCloser: bar.NewProxyReader(closer),
	}
}

// getCandidateExtensions returns a set of file extensions (including the dot seprator) which should be used when
// probing for an executable file.
func getCandidateExtensions() []string {
	if runtime.GOOS == windowsGOOS {
		return []string{".exe", ".cmd"}
	}

	return []string{""}
}

// pluginRegexp matches plugin directory names: pulumi-KIND-NAME-VERSION.
var pluginRegexp = regexp.MustCompile(
	"^(?P<Kind>[a-z]+)-" + // KIND
		"(?P<Name>[a-zA-Z0-9-]*[a-zA-Z0-9])-" + // NAME
		"v(?P<Version>.*)$") // VERSION

// installingPluginRegexp matches the name of temporary folders. Previous versions of Pulumi first extracted
// plugins to a temporary folder with a suffix of `.tmpXXXXXX` (where `XXXXXX`) is a random number, from
// ioutil.TempFile. We should ignore these folders.
var installingPluginRegexp = regexp.MustCompile(`\.tmp[0-9]+$`)

// tryPlugin returns true if a file is a plugin, and extracts information about it.
func tryPlugin(file os.FileInfo) (PluginKind, string, semver.Version, bool) {
	// Only directories contain plugins.
	if !file.IsDir() {
		logging.V(11).Infof("skipping file in plugin directory: %s", file.Name())
		return "", "", semver.Version{}, false
	}

	// Ignore plugins which are being installed
	if installingPluginRegexp.MatchString(file.Name()) {
		logging.V(11).Infof("skipping plugin %s which is being installed", file.Name())
		return "", "", semver.Version{}, false
	}

	// Filenames must match the plugin regexp.
	match := pluginRegexp.FindStringSubmatch(file.Name())
	if len(match) != len(pluginRegexp.SubexpNames()) {
		logging.V(11).Infof("skipping plugin %s with missing capture groups: expect=%d, actual=%d",
			file.Name(), len(pluginRegexp.SubexpNames()), len(match))
		return "", "", semver.Version{}, false
	}
	var kind PluginKind
	var name string
	var version *semver.Version
	for i, group := range pluginRegexp.SubexpNames() {
		v := match[i]
		switch group {
		case "Kind":
			// Skip invalid kinds.
			if IsPluginKind(v) {
				kind = PluginKind(v)
			} else {
				logging.V(11).Infof("skipping invalid plugin kind: %s", v)
			}
		case "Name":
			name = v
		case "Version":
			// Skip invalid versions.
			ver, err := semver.ParseTolerant(v)
			if err == nil {
				version = &ver
			} else {
				logging.V(11).Infof("skipping invalid plugin version: %s", v)
			}
		}
	}

	// If anything was missing or invalid, skip this plugin.
	if kind == "" || name == "" || version == nil {
		logging.V(11).Infof("skipping plugin with missing information: kind=%s, name=%s, version=%v",
			kind, name, version)
		return "", "", semver.Version{}, false
	}

	return kind, name, *version, true
}

// getPluginSize recursively computes how much space is devoted to a given plugin.
func getPluginSize(path string) (int64, error) {
	file, err := os.Stat(path)
	if err != nil {
		return 0, nil
	}

	size := int64(0)
	if file.IsDir() {
		subs, err := ioutil.ReadDir(path)
		if err != nil {
			return 0, err
		}
		for _, child := range subs {
			add, err := getPluginSize(filepath.Join(path, child.Name()))
			if err != nil {
				return 0, err
			}
			size += add
		}
	} else {
		size += file.Size()
	}
	return size, nil
}

type barCloser struct {
	bar        *pb.ProgressBar
	readCloser io.ReadCloser
}

func (bc *barCloser) Read(dest []byte) (int, error) {
	return bc.readCloser.Read(dest)
}

func (bc *barCloser) Close() error {
	bc.bar.Finish()
	return bc.readCloser.Close()
}
