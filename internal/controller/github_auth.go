/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// GitHubAuthApp authenticates as the platform's scaffolding GitHub App
	// (taskapp-platform-scaffolder) with short-lived installation tokens.
	GitHubAuthApp = "app"
	// GitHubAuthPAT authenticates with the shared crossplane-github-credentials
	// personal access token. Kept for clusters without the App; never used as
	// a fallback when the App's credentials are missing.
	GitHubAuthPAT = "pat"

	// credentialsSecretName/credentialsSecretNamespace is the shared
	// crossplane-github-credentials Secret, read only in PAT mode.
	credentialsSecretName      = "crossplane-github-credentials"
	credentialsSecretNamespace = "crossplane-system"

	// Keys of the GitHub App credentials Secret, as the chart's
	// ExternalSecret writes them from taskapp/platform/scaffolder-github-app.
	appIDKey          = "appId"
	installationIDKey = "installationId"
	privateKeyKey     = "privateKey"
)

// GitHubClientSource hands the reconciler a GitHub client for one
// ScaffoldRequest, whose target repository is repo. It's resolved on every
// reconcile, so rotated credentials are picked up without a restart.
type GitHubClientSource interface {
	clientFor(ctx context.Context, repo string) (githubClient, error)
}

// NewGitHubAppSource authenticates as a GitHub App installation whose App
// ID, installation ID and private key are in secret.
func NewGitHubAppSource(reader client.Reader, secret types.NamespacedName) GitHubClientSource {
	return &appSource{reader: reader, secret: secret, newTransport: newInstallationTransport}
}

// NewGitHubPATSource authenticates with the token in the shared
// crossplane-system/crossplane-github-credentials Secret.
func NewGitHubPATSource(reader client.Reader) GitHubClientSource {
	return &patSource{reader: reader, newClient: newGoGithubClient}
}

// patSource reads the shared crossplane-github-credentials Secret via a
// direct client.Get (Secrets don't mount cross-namespace).
type patSource struct {
	reader    client.Reader
	newClient func(token string) (githubClient, error)
}

// githubCredentials mirrors the single "credentials" key on the
// crossplane-github-credentials Secret — a JSON blob, not separate Secret
// keys.
type githubCredentials struct {
	Token string `json:"token"`
	Owner string `json:"owner"`
}

func (s *patSource) clientFor(ctx context.Context, _ string) (githubClient, error) {
	secret := &corev1.Secret{}
	if err := s.reader.Get(ctx, types.NamespacedName{Name: credentialsSecretName, Namespace: credentialsSecretNamespace}, secret); err != nil {
		return nil, fmt.Errorf("reading %s/%s credentials secret: %w", credentialsSecretNamespace, credentialsSecretName, err)
	}

	raw, ok := secret.Data["credentials"]
	if !ok {
		return nil, fmt.Errorf("%s/%s secret has no \"credentials\" key", credentialsSecretNamespace, credentialsSecretName)
	}

	var creds githubCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, fmt.Errorf("parsing %s/%s credentials: %w", credentialsSecretNamespace, credentialsSecretName, err)
	}
	gh, err := s.newClient(creds.Token)
	if err != nil {
		return nil, fmt.Errorf("%s/%s credentials: %w", credentialsSecretNamespace, credentialsSecretName, err)
	}
	return gh, nil
}

// appSource builds clients from the App credentials Secret. The
// platform-scaffolds client is cached and only rebuilt when the Secret
// changes, since its transport mints and refreshes its own tokens. The
// target repository's client is built per request: its token is scoped to
// that one repository.
type appSource struct {
	reader       client.Reader
	secret       types.NamespacedName
	newTransport func(appID, installationID int64, privateKey []byte, opts *github.InstallationTokenOptions) (http.RoundTripper, error)

	mu              sync.Mutex
	resourceVersion string
	appID           int64
	installationID  int64
	privateKey      []byte
	templates       githubClient
}

func (s *appSource) clientFor(ctx context.Context, repo string) (githubClient, error) {
	if repo == "" || repo == scaffoldsRepo {
		return nil, fmt.Errorf("refusing a write token for target repository %q", repo)
	}

	secret := &corev1.Secret{}
	if err := s.reader.Get(ctx, s.secret, secret); err != nil {
		return nil, fmt.Errorf("reading GitHub App credentials secret %s: %w", s.secret, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.templates == nil || s.resourceVersion != secret.ResourceVersion {
		if err := s.load(secret); err != nil {
			return nil, err
		}
	}

	// The scaffold commit writes .github/workflows/*, which GitHub only
	// accepts from an App token that also carries workflows:write.
	target, err := s.client(&github.InstallationTokenOptions{
		Repositories: []string{repo},
		Permissions:  &github.InstallationPermissions{Contents: github.Ptr("write"), Workflows: github.Ptr("write")},
	})
	if err != nil {
		return nil, err
	}
	return &repoRoutingClient{templates: s.templates, target: target}, nil
}

// load parses secret and rebuilds the cached platform-scaffolds client.
func (s *appSource) load(secret *corev1.Secret) error {
	appID, err := secretInt(secret, appIDKey)
	if err != nil {
		return err
	}
	installationID, err := secretInt(secret, installationIDKey)
	if err != nil {
		return err
	}
	privateKey := secret.Data[privateKeyKey]
	if len(privateKey) == 0 {
		return fmt.Errorf("GitHub App credentials secret %s has no %q key", s.secret, privateKeyKey)
	}
	s.appID, s.installationID, s.privateKey = appID, installationID, privateKey

	templates, err := s.client(&github.InstallationTokenOptions{
		Repositories: []string{scaffoldsRepo},
		Permissions:  &github.InstallationPermissions{Contents: github.Ptr("read")},
	})
	if err != nil {
		s.templates = nil
		return err
	}
	s.templates, s.resourceVersion = templates, secret.ResourceVersion
	return nil
}

func (s *appSource) client(opts *github.InstallationTokenOptions) (githubClient, error) {
	tr, err := s.newTransport(s.appID, s.installationID, s.privateKey, opts)
	if err != nil {
		return nil, fmt.Errorf("GitHub App credentials secret %s: %w", s.secret, err)
	}
	return newGoGithubClientWithTransport(tr)
}

func secretInt(secret *corev1.Secret, key string) (int64, error) {
	raw, ok := secret.Data[key]
	if !ok {
		return 0, fmt.Errorf("GitHub App credentials secret %s/%s has no %q key", secret.Namespace, secret.Name, key)
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("GitHub App credentials secret %s/%s: %q is not a number", secret.Namespace, secret.Name, key)
	}
	return n, nil
}

func newInstallationTransport(appID, installationID int64, privateKey []byte, opts *github.InstallationTokenOptions) (http.RoundTripper, error) {
	tr, err := ghinstallation.New(http.DefaultTransport, appID, installationID, privateKey)
	if err != nil {
		return nil, err
	}
	tr.InstallationTokenOptions = opts
	return tr, nil
}

// repoRoutingClient sends calls on platform-scaffolds to a read-only client
// and every other call to the client scoped to the target repository.
type repoRoutingClient struct {
	templates githubClient
	target    githubClient
}

func (c *repoRoutingClient) forRepo(repo string) githubClient {
	if repo == scaffoldsRepo {
		return c.templates
	}
	return c.target
}

func (c *repoRoutingClient) ResolveRevision(ctx context.Context, owner, repo, ref string) (string, error) {
	return c.forRepo(repo).ResolveRevision(ctx, owner, repo, ref)
}

func (c *repoRoutingClient) FetchTree(ctx context.Context, owner, repo, sha, prefix string) (map[string][]byte, error) {
	return c.forRepo(repo).FetchTree(ctx, owner, repo, sha, prefix)
}

func (c *repoRoutingClient) RepositoryState(ctx context.Context, owner, repo string) (string, int, string, string, error) {
	return c.forRepo(repo).RepositoryState(ctx, owner, repo)
}

func (c *repoRoutingClient) CommitExists(ctx context.Context, owner, repo, sha string) (bool, error) {
	return c.forRepo(repo).CommitExists(ctx, owner, repo, sha)
}

func (c *repoRoutingClient) CommitFiles(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error) {
	return c.forRepo(repo).CommitFiles(ctx, owner, repo, branch, message, files, parentSHA, baseTreeSHA)
}
