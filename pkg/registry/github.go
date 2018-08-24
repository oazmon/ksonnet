// Copyright 2018 The ksonnet authors
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package registry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ksonnet/ksonnet/pkg/app"
	"github.com/ksonnet/ksonnet/pkg/parts"
	"github.com/ksonnet/ksonnet/pkg/util/github"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/afero"
)

const (
	rawGitHubRoot       = "https://raw.githubusercontent.com"
	defaultGitHubBranch = "master"
)

var (
	// errInvalidURI is an invalid github uri error.
	errInvalidURI = fmt.Errorf("Invalid GitHub URI: try navigating in GitHub to the URI of the folder containing the 'yaml', and using that URI instead. Generally, this URI should be of the form 'github.com/{organization}/{repository}/tree/{branch}/[path-to-directory]'")

	githubFactory = func(a app.App, spec *app.RegistryConfig, opts ...GitHubOpt) (*GitHub, error) {
		return NewGitHub(a, spec, opts...)
	}
)

type ghFactoryFn func(a app.App, spec *app.RegistryConfig, opts ...GitHubOpt) (*GitHub, error)

// GitHubClient is an option for the setting a github client.
func GitHubClient(c github.GitHub) GitHubOpt {
	return func(gh *GitHub) {
		gh.ghClient = c
	}
}

// GitHubOpt is an option for configuring GitHub.
type GitHubOpt func(*GitHub)

// GitHub is a Github Registry
type GitHub struct {
	app      app.App
	name     string
	hd       *hubDescriptor
	ghClient github.GitHub
	spec     *app.RegistryConfig
}

// NewGitHub creates an instance of GitHub.
func NewGitHub(a app.App, registryRef *app.RegistryConfig, opts ...GitHubOpt) (*GitHub, error) {
	if registryRef == nil {
		return nil, errors.New("registry ref is nil")
	}

	gh := &GitHub{
		app:      a,
		name:     registryRef.Name,
		spec:     registryRef,
		ghClient: github.DefaultClient,
	}

	// Apply functional options
	for _, opt := range opts {
		opt(gh)
	}

	hd, err := parseGitHubURI(gh.URI())
	if err != nil {
		return nil, err
	}
	gh.hd = hd
	gh.SetBaseURL(hd.baseURL)

	return gh, nil
}

// IsOverride is true if this registry an an override.
func (gh *GitHub) IsOverride() bool {
	return gh.spec.IsOverride()
}

// Name is the registry name.
func (gh *GitHub) Name() string {
	return gh.name
}

// Protocol is the registry protocol.
func (gh *GitHub) Protocol() Protocol {
	return Protocol(gh.spec.Protocol)
}

// URI is the registry URI.
func (gh *GitHub) URI() string {
	return gh.spec.URI
}

// RegistrySpecDir is the registry directory.
func (gh *GitHub) RegistrySpecDir() string {
	return gh.Name()
}

// RegistrySpecFilePath is the path for the registry.yaml
func (gh *GitHub) RegistrySpecFilePath() string {
	return path.Join(gh.Name(), registryYAMLFile)
}

// resolveLatestSHA fetches the SHA currently pointed to by configured RefSpec from remote
func (gh *GitHub) resolveLatestSHA() (string, error) {
	log := log.WithField("action", "GitHub.resolveLatestSHA")

	if gh == nil {
		return "", errors.Errorf("nil receiver")
	}
	// Generally hubDescriptor is parsed in NewGitHub - this is just a backup.
	if gh.hd == nil {
		hd, err := parseGitHubURI(gh.URI())
		if err != nil {
			return "", errors.Wrapf(err, "unable to parse URI: %v", gh.URI())
		}
		gh.hd = hd
	}

	log.Debugf("resolving SHA for URI: %v", gh.URI())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sha, err := gh.ghClient.CommitSHA1(ctx, gh.hd.Repo(), gh.hd.refSpec)
	if err != nil {
		return "", errors.Wrapf(err, "unable to find SHA1 for URI: %v", gh.URI())
	}

	return sha, nil
}

// updateLibVersions updates the libraries in a registry spec to present the provided version.
func updateLibVersions(spec *Spec, version string) {
	if spec == nil {
		return
	}

	for _, lib := range spec.Libraries {
		lib.Version = version
	}
}

// FetchRegistrySpec fetches the registry spec (registry.yaml, inventory of packages)
// This inventory may have been previously cached on disk. If the cache is not stale,
// it will be used. Otherwise, the spec is fetched from the remote repository.
func (gh *GitHub) FetchRegistrySpec() (*Spec, error) {
	log := log.WithField("action", "GitHub.FetchRegistrySpec")

	// Check local disk cache.
	registrySpecFile := registrySpecFilePath(gh.app, gh)

	log.Debugf("checking for registry cache: %v", registrySpecFile)
	registrySpec, exists, err := load(gh.app, registrySpecFile)
	if err != nil {
		log.Warnf("error loading cache for %v (%v), trying to refresh instead", gh.spec.Name, err)
		exists = false
	}

	var cachedVersion string
	if registrySpec != nil {
		cachedVersion = registrySpec.Version
	}

	// Get the latest matching commit to determine staleness of cache
	sha, err := gh.resolveLatestSHA()
	if err != nil || sha == "" {
		errMsg := errors.Wrapf(err, "unable to resolve commit for refspec: %v", gh.hd.refSpec)
		if registrySpec == nil || cachedVersion == "" {
			// In this case, we failed both the cache and to fetch from remote
			return nil, errMsg
		}

		log.Warnf("%v", errMsg)
		log.Warnf("falling back to cached version (%v)", cachedVersion)
		updateLibVersions(registrySpec, gh.hd.refSpec)
		return registrySpec, nil
	}

	// Check if cache is still current
	if exists && cachedVersion == sha {
		log.Debugf("using cache @%v", sha)
		updateLibVersions(registrySpec, sha)
		return registrySpec, nil
	}

	if exists {
		log.Debugf("cache is stale, updating to %v", sha)
	} else {
		log.Debugf("cache not found, fetching remote for %v", gh.spec.Name)
	}

	// Abandoning cache - fetch from remote

	cs := github.ContentSpec{
		Repo:    gh.hd.Repo(),
		Path:    gh.hd.regSpecRepoPath,
		RefSpec: sha,
	}

	registrySpec, err = gh.fetchRemoteSpec(cs)
	if err != nil {
		return nil, err
	}
	updateLibVersions(registrySpec, sha)

	var registrySpecBytes []byte
	registrySpecBytes, err = registrySpec.Marshal()
	if err != nil {
		return nil, err
	}

	// NOTE: We call mkdir after getting the registry spec, since a
	// network call might fail and leave this half-initialized empty
	// directory.
	registrySpecDir := filepath.Join(registryCacheRoot(gh.app), gh.RegistrySpecDir())
	err = gh.app.Fs().MkdirAll(registrySpecDir, app.DefaultFolderPermissions)
	if err != nil {
		return nil, err
	}

	err = afero.WriteFile(gh.app.Fs(), registrySpecFile, registrySpecBytes, app.DefaultFilePermissions)
	if err != nil {
		return nil, err
	}

	return registrySpec, nil
}

// fetchRemoteSpec fetches a ksonnet registry spec (registry.yaml) from a remote GitHub repository.
// repo describes the remote repo (org/repo)
// path is the file path within the repo (represents the registry.yaml file)
// sha1 is the commit to pull the contents from
func (gh *GitHub) fetchRemoteSpec(cs github.ContentSpec) (*Spec, error) {
	log := log.WithField("action", "GitHub.fetchRemoteSpec")
	ctx := context.Background()

	log.Debugf("fetching %v", cs)
	file, _, err := gh.ghClient.Contents(ctx, cs.Repo, cs.Path,
		cs.RefSpec)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("Could not find valid registry with coordinates: %v", cs)
	}

	registrySpecText, err := file.GetContent()
	if err != nil {
		return nil, err
	}

	// Deserialize, return.
	registrySpec, err := Unmarshal([]byte(registrySpecText))
	if err != nil {
		return nil, err
	}

	// Version will persisted in registry.yaml cache.
	// This allows us to check whether the cache is stale.
	registrySpec.Version = cs.RefSpec

	return registrySpec, nil
}

// MakeRegistryConfig returns an app registry ref spec.
func (gh *GitHub) MakeRegistryConfig() *app.RegistryConfig {
	return gh.spec
}

// ResolveLibrarySpec returns a resolved spec for a part.
func (gh *GitHub) ResolveLibrarySpec(partName, libRefSpec string) (*parts.Spec, error) {
	ctx := context.Background()
	resolvedSHA, err := gh.ghClient.CommitSHA1(ctx, gh.hd.Repo(), libRefSpec)
	if err != nil {
		return nil, err
	}

	// Resolve app spec.
	appSpecPath := strings.Join([]string{gh.hd.regRepoPath, partName, partsYAMLFile}, "/")

	file, directory, err := gh.ghClient.Contents(ctx, gh.hd.Repo(), appSpecPath, resolvedSHA)
	if err != nil {
		return nil, err
	} else if directory != nil {
		return nil, fmt.Errorf("Can't download library specification; resource '%s' points at a file", gh.registrySpecRawURL())
	}

	partsSpecText, err := file.GetContent()
	if err != nil {
		return nil, err
	}

	parts, err := parts.Unmarshal([]byte(partsSpecText))
	if err != nil {
		return nil, err
	}

	// For GitHub repositories, the SHA is the correct version, not what is written in the spec file.
	parts.Version = resolvedSHA

	return parts, nil
}

// chrootOnFile is a ResolveFile decorator that rebases paths to be relative to the registry root
// (as opposed to the repo root).
// Example:
//   uri: github.com/ksonnet/parts/tree/master/nested/registry/incubator
//   relPath: nested/registry/incubator/registry.yaml
//   chrootedPath: registry.yaml
func (gh *GitHub) chrootOnFile(onFile ResolveFile) ResolveFile {
	return func(relPath string, contents []byte) error {
		chrootedPath, err := gh.rebaseToRoot(relPath)
		if err != nil {
			return errors.Wrapf(err, "chrooting path %v relative to registry root %v", relPath, gh.URI)
		}
		return onFile(chrootedPath, contents)
	}
}

// chrootOnDir is a ResolveDirectory decorator that rebases paths to be relative to the registry root
// (as opposed to the repo root).
// Example:
//   uri: github.com/ksonnet/parts/tree/master/nested/registry/incubator
//   relPath: nested/registry/incubator/dir
//   chrootedPath: dir
func (gh *GitHub) chrootOnDir(onDir ResolveDirectory) ResolveDirectory {
	return func(relPath string) error {
		chrootedPath, err := gh.rebaseToRoot(relPath)
		if err != nil {
			return errors.Wrapf(err, "chrooting path %v relative to registry root %v", relPath, gh.URI)
		}
		return onDir(chrootedPath)
	}
}

// ResolveLibrary fetches the part and creates a parts spec and library ref spec.
func (gh *GitHub) ResolveLibrary(partName, partAlias, libRefSpec string, onFile ResolveFile, onDir ResolveDirectory) (*parts.Spec, *app.LibraryConfig, error) {
	//log := log.WithField("action", "GitHub.ResolveLibrary")
	if gh == nil {
		return nil, nil, errors.Errorf("nil receiver")
	}

	var err error
	var resolvedSHA string
	ctx := context.Background()

	if libRefSpec == "" {
		// Resolve the commit based on the registry uri
		resolvedSHA, err = gh.resolveLatestSHA()
		if err != nil || resolvedSHA == "" {
			return nil, nil, errors.Wrapf(err, "unable to resolve commit for refspec: %v", gh.hd.refSpec)
		}
	} else {
		// Resolve `version` (a git refspec) to a specific SHA.
		// TODO if it is already a SHA, don't resolve again
		resolvedSHA, err = gh.ghClient.CommitSHA1(ctx, gh.hd.Repo(), libRefSpec)
		if err != nil {
			return nil, nil, err
		}
	}

	// Resolve directories and files.
	path := strings.Join([]string{gh.hd.regRepoPath, partName}, "/")
	err = gh.resolveDir(partName, path, resolvedSHA, gh.chrootOnFile(onFile), gh.chrootOnDir(onDir))
	if err != nil {
		return nil, nil, err
	}

	// Resolve app spec.
	// TODO we just downloaded this above - why download again?
	appSpecPath := strings.Join([]string{path, partsYAMLFile}, "/")
	file, directory, err := gh.ghClient.Contents(ctx, gh.hd.Repo(), appSpecPath, resolvedSHA)

	if err != nil {
		return nil, nil, err
	} else if directory != nil {
		return nil, nil, fmt.Errorf("Can't download library specification; resource '%s' points at a file", gh.registrySpecRawURL())
	}

	partsSpecText, err := file.GetContent()
	if err != nil {
		return nil, nil, err
	}

	parts, err := parts.Unmarshal([]byte(partsSpecText))
	if err != nil {
		return nil, nil, err
	}

	if partAlias == "" {
		partAlias = partName
	}

	refSpec := app.LibraryConfig{
		Name:     partAlias,
		Registry: gh.Name(),
		Version:  resolvedSHA,
	}

	return parts, &refSpec, nil
}

func (gh *GitHub) resolveDir(libID, path, version string, onFile ResolveFile, onDir ResolveDirectory) error {
	ctx := context.Background()

	file, directory, err := gh.ghClient.Contents(ctx, gh.hd.Repo(), path, version)
	if err != nil {
		return err
	} else if file != nil {
		return fmt.Errorf("Lib ID %q resolves to a file in registry %q", libID, gh.Name())
	}

	for _, item := range directory {
		switch item.GetType() {
		case "file":
			itemPath := item.GetPath()
			file, directory, err := gh.ghClient.Contents(ctx, gh.hd.Repo(), itemPath, version)
			if err != nil {
				return err
			} else if directory != nil {
				return fmt.Errorf("INTERNAL ERROR: GitHub API reported resource %q of type file, but returned type dir", itemPath)
			}
			contents, err := file.GetContent()
			if err != nil {
				return err
			}
			if err := onFile(itemPath, []byte(contents)); err != nil {
				return err
			}
		case "dir":
			itemPath := item.GetPath()
			if err := onDir(itemPath); err != nil {
				return err
			}
			if err := gh.resolveDir(libID, itemPath, version, onFile, onDir); err != nil {
				return err
			}
		case "symlink":
		case "submodule":
			return fmt.Errorf("Invalid library %q; ksonnet doesn't support libraries with symlinks or submodules", libID)
		}
	}

	return nil
}

func (gh *GitHub) registrySpecRawURL() string {
	return strings.Join([]string{
		rawGitHubRoot,
		gh.hd.org,
		gh.hd.repo,
		gh.hd.refSpec,
		gh.hd.regSpecRepoPath}, "/")
}

type hubDescriptor struct {
	baseURL         *url.URL
	org             string
	repo            string
	refSpec         string
	regRepoPath     string
	regSpecRepoPath string
}

func (hd *hubDescriptor) Repo() github.Repo {
	return github.Repo{Org: hd.org, Repo: hd.repo}
}

// func parseGitHubURI(uri string) (org, repo, refSpec, regRepoPath, regSpecRepoPath string, err error) {
func parseGitHubURI(uri string) (hd *hubDescriptor, err error) {
	// Normalize URI.
	uri = strings.TrimSpace(uri)
	if strings.HasPrefix(uri, "http://github.") || strings.HasPrefix(uri, "https://github.") || strings.HasPrefix(uri, "http://www.github.") || strings.HasPrefix(uri, "https://www.github.") {
		// Do nothing.
	} else if strings.HasPrefix(uri, "github.") || strings.HasPrefix(uri, "www.github.") {
		uri = "http://" + uri
	} else {
		return nil, errors.Errorf("Registries using protocol 'github' must provide URIs beginning with 'github' (optionally prefaced with 'http', 'https', 'www', and so on")
	}

	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	components := strings.Split(parsed.Path, "/")
	fmt.Printf("DEBUG: path: %s\n", parsed.Path)

	hd = &hubDescriptor{}
	fmt.Printf("DEBUG: host: %s\n", parsed.Host)
	isEnterprise := !strings.HasSuffix(parsed.Host, "github.com")
	fmt.Printf("DEBUG: isEnterprise: %t\n", isEnterprise)
	baseIndex := -1
	if isEnterprise {
		for i, n := range components {
			if "repos" == n {
				baseIndex = i
				break
			}
		}
		if baseIndex == -1 {
			return nil, errors.Errorf("Enterprise GitHub URI must point at a repository's V3 API 'repos' endpoint:\n%s", uri)
		}
		hd.baseURL,_ = url.Parse(
		parsed.Scheme + "://" + parsed.Host + strings.Join(components[:baseIndex], "/") + "/")

		queries := parsed.Query()
		fmt.Printf("DEBUG: queries: %s\n", queries)
		switch len(queries) {
		case 0:
			hd.refSpec = ""
		case 1:
			refSpecs, ok := queries["ref"]
			if ok && len(refSpecs) == 1 {
				hd.refSpec = refSpecs[0]
				break
			}
			fallthrough
		default:
			return nil, errors.Errorf("Only 'ref' query strings allowed in enterprise registry URI:\n%s", uri)
		}
		fmt.Printf("DEBUG: hd.refSpec: %s\n", hd.refSpec)
	} else {
		if len(parsed.Query()) != 0 {
			return nil, errors.Errorf("No query strings allowed in registry URI:\n%s", uri)
		}

		hd.baseURL = nil
		baseIndex = 0
	}
	fmt.Printf("DEBUG: baseURL: %d\n", hd.baseURL.String())
	fmt.Printf("DEBUG: baseIndex: %d\n", baseIndex)

	if len(components) < baseIndex+3 {
		return nil, errors.Errorf("GitHub URI must point at a repository:\n%s", uri)
	}

	// NOTE: The first component is always blank, because the path
	// begins like: '/whatever'.
	hd.org = components[baseIndex+1]
	fmt.Printf("DEBUG: hd.org: %s\n", hd.org)
	hd.repo = components[baseIndex+2]
	fmt.Printf("DEBUG: hd.repo: %s\n", hd.repo)

	//
	// Parse out `regSpecRepoPath`. There are a few cases:
	//   * URI points at a directory inside the respoitory, e.g.,
	//     'http://github.com/ksonnet/parts/tree/master/incubator'
	//   * URI points at an 'app.yaml', e.g.,
	//     'http://github.com/ksonnet/parts/blob/master/yaml'
	//   * URI points at a repository root, e.g.,
	//     'http://github.com/ksonnet/parts'
	//
	//   Enterprise:
	//   * URI points at a directory inside the enterprise respoitory, e.g.,
	//     'https://github.my-company.com/api/v3/repos/my-org/ksonnet-parts/contents/registry?ref=master'
	//   * URI points at an 'app.yaml' inside an enterprise repository, e.g.,
	//     'https://github.my-company.com/api/v3/repos/my-org/ksonnet-parts/contents/registry/app.yaml?ref=master'
	//   * URI points at a enterprise repository root, e.g.,
	//     'https://github.my-company.com/api/v3/repos/my-org/ksonnet-parts?ref=master'
	//

	// See note above about first component being blank.
	if isEnterprise {
		if len := len(components); len > baseIndex+4 {
			// If we have a trailing '/' character, last component will be blank. Make
			// sure that `regRepoPath` does not contain a trailing `/`.
			if components[len-1] == "" {
				hd.regRepoPath = strings.Join(components[baseIndex+4:len-1], "/")
				fmt.Printf("DEBUG: hd.regRepoPath: %s\n", hd.regRepoPath)
				components[len-1] = registryYAMLFile
			} else {
				hd.regRepoPath = strings.Join(components[baseIndex+4:], "/")
				fmt.Printf("DEBUG: hd.regRepoPath: %s\n", hd.regRepoPath)
				components = append(components, registryYAMLFile)
			}
			hd.regSpecRepoPath = strings.Join(components[baseIndex+4:], "/")
			fmt.Printf("DEBUG: hd.regSpecRepoPath: %s\n", hd.regSpecRepoPath)
			return
		} else {
			// Else, URI should point at repository root.
			hd.refSpec = defaultGitHubBranch
			hd.regRepoPath = ""
			hd.regSpecRepoPath = registryYAMLFile
			return
		}
	} else {
		hd.refSpec = components[baseIndex+4]
		fmt.Printf("DEBUG: hd.refSpec: %s\n", hd.refSpec)

		if len := len(components); len > baseIndex+4 {
			//
			// Case where we're pointing at either a directory inside a GitHub
			// URL, or an 'app.yaml' inside a GitHub URL.
			//
			if components[baseIndex+3] == "tree" {
				// If we have a trailing '/' character, last component will be blank. Make
				// sure that `regRepoPath` does not contain a trailing `/`.
				if components[len-1] == "" {
					hd.regRepoPath = strings.Join(components[baseIndex+5:len-1], "/")
					components[len-1] = registryYAMLFile
				} else {
					hd.regRepoPath = strings.Join(components[baseIndex+5:], "/")
					components = append(components, registryYAMLFile)
				}
				hd.regSpecRepoPath = strings.Join(components[baseIndex+5:], "/")
				return
			} else if components[baseIndex+3] == "blob" && components[len-1] == registryYAMLFile {
				hd.regRepoPath = strings.Join(components[baseIndex+5:len-1], "/")
				// Path to the `yaml` (may or may not exist).
				hd.regSpecRepoPath = strings.Join(components[baseIndex+5:], "/")
				return
			} else {
				return nil, errInvalidURI
			}
		} else {
			// Else, URI should point at repository root.
			hd.refSpec = defaultGitHubBranch
			hd.regRepoPath = ""
			hd.regSpecRepoPath = registryYAMLFile
			return
		}
	}
}

// Rebase a path to *registry* root (not repo root)
// Example:
//  uri:    github.com/ksonnet/parts/tree/master/long/path/incubator
//  path:   long/path/incubator/parts.yaml
//  output: parts.yaml
func (gh *GitHub) rebaseToRoot(path string) (string, error) {
	if gh == nil {
		return "", errors.Errorf("nil receiver")
	}
	if gh.hd == nil {
		return "", errors.Errorf("registry %v not correctly initialized - missing hubDescriptor", gh.name)
	}

	root := gh.hd.regRepoPath
	rebasedAbs := strings.TrimPrefix(strings.TrimPrefix(path, "/"), root)
	rebased := strings.TrimPrefix(rebasedAbs, "/")

	return rebased, nil
}

// CacheRoot returns the root for caching - it removes any leading path segments
// from a provided path, leaving just the relative path under the registry name.
// Example:
//  uri:    github.com/ksonnet/parts/tree/master/long/path/incubator
//  path:   long/path/incubator/parts.yaml
//  output: incubator/parts.yaml
func (gh *GitHub) CacheRoot(name, path string) (string, error) {
	if gh == nil {
		return "", errors.Errorf("nil receiver")
	}
	if gh.hd == nil {
		return "", errors.Errorf("registry %v not correctly initialized - missing hubDescriptor", gh.name)
	}

	root := gh.hd.regRepoPath
	rebasedAbs := strings.TrimPrefix(strings.TrimPrefix(path, "/"), root)
	rebased := strings.TrimPrefix(rebasedAbs, "/")
	return filepath.Join(name, rebased), nil
}

func (gh *GitHub) fetchRemoteAndSave(cs github.ContentSpec, w io.Writer) error {
	if gh == nil {
		return errors.Errorf("nil receiver")
	}

	if gh.app == nil {
		return errors.Errorf("application is required")
	}

	if w == nil {
		return errors.Errorf("writer is required")
	}

	// If failed, use the protocol to try to retrieve app specification.
	registrySpec, err := gh.fetchRemoteSpec(cs)
	if err != nil || registrySpec == nil {
		return err
	}

	var registrySpecBytes []byte
	registrySpecBytes, err = registrySpec.Marshal()
	if err != nil {
		return err
	}

	r := bytes.NewReader(registrySpecBytes)
	_, err = io.Copy(w, r)
	if err != nil {
		return errors.Wrap(err, "failed writing registry spec")

	}

	return nil
}

// SetURI implements registry.Setter. It sets the URI for the registry.
func (gh *GitHub) SetURI(uri string) error {
	if gh == nil {
		return errors.Errorf("nil receiver")
	}
	if gh.spec == nil {
		return errors.Errorf("nil spec")
	}

	// 1. Verify URI
	hd, err := parseGitHubURI(uri)
	if err != nil {
		return err
	}
	if ok, err := gh.ValidateURI(uri); err != nil || !ok {
		return errors.Wrap(err, "validating uri")
	}

	// 3. Set URI
	gh.hd = hd
	gh.spec.URI = uri

	// TODO: Call FetchRegistrySpec here or from our caller?
	return nil
}

// ValidateURI implements registry.Validator. A URI is valid if:
//   * It is a valid URI (RFC 3986)
//   * It points to GitHub (Enterprise not supported at this time)
//   * It points to a valid tree in a GitHub repository
//   * That tree contains a `registry.yaml` file
//   * It currently exists (a HEAD request is sent over the network)
func (gh *GitHub) ValidateURI(uri string) (bool, error) {
	if gh == nil {
		return false, errors.Errorf("nil receiver")
	}
	if err := gh.ghClient.ValidateURL(uri); err != nil {
		return false, errors.Wrap(err, "validating GitHub registry URL")
	}

	if _, err := parseGitHubURI(uri); err != nil {
		return false, errors.Wrap(err, "parsing GitHub registry URL")
	}

	return true, nil
}

func (gh *GitHub) SetBaseURL(baseURL *url.URL) {
	if baseURL == nil {
		fmt.Printf("DEBUG!!! setting registry baseURL: DEFAULT\n")
	} else {
		fmt.Printf("DEBUG!!! setting registry baseURL: %s\n", baseURL.String())
	} 
	gh.ghClient.SetBaseURL(baseURL)
}
