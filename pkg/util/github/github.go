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

package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/github"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

var (
	// DefaultClient is the default GitHub client.
	DefaultClient = &defaultGitHub{
		httpClient: defaultHTTPClient(),
		urlParse:   url.Parse,
	}
)

// Repo is a GitHub repo
type Repo struct {
	Org  string
	Repo string
}

func (r Repo) String() string {
	return fmt.Sprintf("%v/%v", r.Org, r.Repo)
}

// ContentSpec represents coordinates for fetching contents from a GitHub repo
type ContentSpec struct {
	Repo
	Path    string // Path within repo to fetch
	RefSpec string // A git refspec - branch, tag, or commit sha1.
}

func (c ContentSpec) String() string {
	return fmt.Sprintf("%v/%v@%v", c.Repo, c.Path, c.RefSpec)
}

// GitHub is an interface for communicating with GitHub.
type GitHub interface {
	SetBaseURL(*url.URL)
	ValidateURL(u string) error
	CommitSHA1(ctx context.Context, repo Repo, refSpec string) (string, error)
	Contents(ctx context.Context, repo Repo, path, sha1 string) (*github.RepositoryContent, []*github.RepositoryContent, error)
}

type httpClient interface {
	Head(string) (*http.Response, error)
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
	}
}

type defaultGitHub struct {
	httpClient *http.Client
	urlParse   func(string) (*url.URL, error)
	baseURL    *url.URL
}

var _ GitHub = (*defaultGitHub)(nil)

// NewGitHub constructs a GitHub client
func NewGitHub(httpClient *http.Client) GitHub {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	return &defaultGitHub{
		httpClient: httpClient,
		urlParse:   url.Parse,
	}
}

func (dg *defaultGitHub) SetBaseURL(baseURL *url.URL) {
	if baseURL == nil {
		fmt.Printf("DEBUG!!! setting default baseURL: DEFAULT\n")
	} else {
		fmt.Printf("DEBUG!!! setting default baseURL: %s\n", baseURL.String())
	}
	dg.baseURL = baseURL
}

func (dg *defaultGitHub) ValidateURL(urlStr string) error {
	u, err := dg.urlParse(urlStr)
	if err != nil {
		return errors.Wrap(err, "parsing URL")
	}

	if u.Scheme == "" {
		u.Scheme = "https"
	}

	if !strings.HasPrefix(u.Path, "registry.yaml") {
		u.Path = u.Path + "/registry.yaml"
	}

	resp, err := dg.httpClient.Head(u.String())
	if err != nil {
		return errors.Wrapf(err, "verifying %q", u.String())
	}

	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("%q actual %d; expected %d", u.String(), resp.StatusCode, http.StatusOK)
	}

	return nil
}

func (dg *defaultGitHub) CommitSHA1(ctx context.Context, repo Repo, refSpec string) (string, error) {
	log := log.WithField("action", "defaultGitHub.CommitSHA1")
	if refSpec == "" {
		refSpec = "master"
	}

	log.Debugf("fetching SHA1 for %s@%s", repo, refSpec)
	sha, _, err := dg.client().Repositories.GetCommitSHA1(ctx, repo.Org, repo.Repo, refSpec, "")
	return sha, err
}

func (dg *defaultGitHub) Contents(ctx context.Context, repo Repo, path, ref string) (*github.RepositoryContent, []*github.RepositoryContent, error) {
	log := log.WithField("action", "defaultGitHub.Contents")
	log.Debugf("fetching contents for %s/%s@%s", repo, path, ref)
	opts := &github.RepositoryContentGetOptions{Ref: ref}

	file, dir, _, err := dg.client().Repositories.GetContents(ctx, repo.Org, repo.Repo, path, opts)
	return file, dir, err
}

func (dg *defaultGitHub) client() *github.Client {
	var httpClient = dg.httpClient

	ght := os.Getenv("GITHUB_TOKEN")
	if len(ght) > 0 {
		// TODO WithTimeout
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, dg.httpClient)
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: ght},
		)
		httpClient = oauth2.NewClient(ctx, ts)
	}


	client := github.NewClient(httpClient)
	if dg.baseURL != nil {
		fmt.Printf("DEBUG!!! using baseURL: %s\n", dg.baseURL.String())
		client.BaseURL = dg.baseURL
		client.UploadURL = nil
	} else {
		fmt.Printf("DEBUG!!! using baseURL: DEFAULT\n")
	}
	return client
}
