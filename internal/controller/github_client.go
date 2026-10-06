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
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v88/github"
)

// githubClient is the minimal GitHub surface the ScaffoldRequest controller
// needs. goGithubClient below is the real implementation, backed by
// google/go-github; tests use a stub that never makes a network call.
//
// The Git Data API (trees/blobs/commits/refs) is used throughout instead of
// higher-level convenience endpoints, for two reasons: it's the only way to
// produce one atomic commit from many files (the thing a Crossplane
// RepositoryFile-per-file approach couldn't give us — see
// scaffold-operator-plan.md), and it needs no local git binary and no
// special auth beyond a plain token (crossplane-compositions/platform-scaffolds
// are public repos).
type githubClient interface {
	// ResolveRevision resolves ref (a tag, branch, or SHA) in owner/repo to
	// its commit SHA.
	ResolveRevision(ctx context.Context, owner, repo, ref string) (string, error)

	// FetchTree returns path -> file content for every blob under prefix at
	// the given commit sha. Paths are as they appear in the repository
	// (i.e. still prefixed).
	FetchTree(ctx context.Context, owner, repo, sha, prefix string) (map[string][]byte, error)

	// RepositoryState returns owner/repo's default branch and its commit
	// history shape. CommitCount is capped at 2 ("2 or more") — the caller
	// only ever needs to distinguish 0 / 1 / many. HeadSHA and HeadTreeSHA
	// are populated whenever CommitCount is 1: with autoInit:true (see
	// component-operator), a freshly created repository's expected starting
	// state is exactly one commit (GitHub's auto-generated README), and the
	// scaffold commit is built on top of it as parent, using its tree as
	// base_tree, rather than requiring a genuinely empty repository (which
	// GitHub's Git Data API rejects outright — see CommitFiles).
	RepositoryState(ctx context.Context, owner, repo string) (defaultBranch string, commitCount int, headSHA string, headTreeSHA string, err error)

	// CommitExists reports whether sha is a real, reachable commit in
	// owner/repo.
	CommitExists(ctx context.Context, owner, repo, sha string) (bool, error)

	// CommitFiles creates one commit containing files (path -> content) on
	// branch. If parentSHA is empty, the commit is parent-less and branch
	// must not yet exist (RepositoryState reported 0 commits). Otherwise the
	// commit is built on top of parentSHA, using baseTreeSHA as the tree's
	// base_tree, and the existing branch ref is updated to point at it.
	// Returns the new commit's SHA.
	CommitFiles(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error)
}

// goGithubClient is githubClient backed by the real GitHub API.
type goGithubClient struct {
	gh *github.Client
}

// githubTimeout bounds every GitHub API call, so a hung request can't stall
// a reconcile worker.
const githubTimeout = 30 * time.Second

// newGoGithubClient authenticates with a static token (the PAT).
func newGoGithubClient(token string) (githubClient, error) {
	gh, err := github.NewClient(github.WithAuthToken(token), github.WithTimeout(githubTimeout))
	if err != nil {
		return nil, err
	}
	return &goGithubClient{gh: gh}, nil
}

// newGoGithubClientWithTransport authenticates through transport (a GitHub
// App installation transport that mints its own tokens).
func newGoGithubClientWithTransport(transport http.RoundTripper) (githubClient, error) {
	gh, err := github.NewClient(github.WithTransport(transport), github.WithTimeout(githubTimeout))
	if err != nil {
		return nil, err
	}
	return &goGithubClient{gh: gh}, nil
}

func (c *goGithubClient) ResolveRevision(ctx context.Context, owner, repo, ref string) (string, error) {
	commit, _, err := c.gh.Repositories.GetCommit(ctx, owner, repo, ref, nil)
	if err != nil {
		return "", err
	}
	return commit.GetSHA(), nil
}

func (c *goGithubClient) FetchTree(ctx context.Context, owner, repo, sha, prefix string) (map[string][]byte, error) {
	tree, _, err := c.gh.Git.GetTree(ctx, owner, repo, sha, true)
	if err != nil {
		return nil, fmt.Errorf("getting tree %s: %w", sha, err)
	}

	out := make(map[string][]byte)
	for _, entry := range tree.Entries {
		if entry.GetType() != "blob" || !strings.HasPrefix(entry.GetPath(), prefix) {
			continue
		}
		blob, _, err := c.gh.Git.GetBlobRaw(ctx, owner, repo, entry.GetSHA())
		if err != nil {
			return nil, fmt.Errorf("fetching blob %s: %w", entry.GetPath(), err)
		}
		out[entry.GetPath()] = blob
	}
	return out, nil
}

func (c *goGithubClient) RepositoryState(ctx context.Context, owner, repo string) (string, int, string, string, error) {
	r, _, err := c.gh.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", 0, "", "", err
	}
	defaultBranch := r.GetDefaultBranch()

	ref, _, err := c.gh.Git.GetRef(ctx, owner, repo, "heads/"+defaultBranch)
	if err != nil {
		// GitHub does not 404 a missing ref on a repository that exists but
		// has no commit history yet -- it returns 409 Conflict ("Git
		// Repository is empty") instead. isNotFound alone (404) never
		// matches a genuinely empty repo; a nonexistent repo would already
		// have failed above on Repositories.Get.
		if isEmptyRepository(err) {
			return defaultBranch, 0, "", "", nil
		}
		return "", 0, "", "", err
	}
	headSHA := ref.GetObject().GetSHA()

	// PerPage:2 is enough to tell "exactly one" from "more than one" without
	// paging through full history.
	commits, _, err := c.gh.Repositories.ListCommits(ctx, owner, repo, &github.CommitsListOptions{
		SHA:         defaultBranch,
		ListOptions: github.ListOptions{PerPage: 2},
	})
	if err != nil {
		return "", 0, "", "", fmt.Errorf("listing commits: %w", err)
	}
	if len(commits) >= 2 {
		return defaultBranch, 2, headSHA, "", nil
	}

	headCommit, _, err := c.gh.Git.GetCommit(ctx, owner, repo, headSHA)
	if err != nil {
		return "", 0, "", "", fmt.Errorf("getting head commit %s: %w", headSHA, err)
	}
	return defaultBranch, 1, headSHA, headCommit.GetTree().GetSHA(), nil
}

func (c *goGithubClient) CommitExists(ctx context.Context, owner, repo, sha string) (bool, error) {
	_, _, err := c.gh.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *goGithubClient) CommitFiles(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error) {
	entries := make([]*github.TreeEntry, 0, len(files))
	for path, content := range files {
		blob, _, err := c.gh.Git.CreateBlob(ctx, owner, repo, github.Blob{
			Content:  github.Ptr(base64.StdEncoding.EncodeToString(content)),
			Encoding: github.Ptr("base64"),
		})
		if err != nil {
			return "", fmt.Errorf("creating blob for %s: %w", path, err)
		}
		entries = append(entries, &github.TreeEntry{
			Path: github.Ptr(path),
			Mode: github.Ptr("100644"),
			Type: github.Ptr("blob"),
			SHA:  blob.SHA,
		})
	}

	tree, _, err := c.gh.Git.CreateTree(ctx, owner, repo, baseTreeSHA, entries)
	if err != nil {
		return "", fmt.Errorf("creating tree: %w", err)
	}

	commitInput := github.Commit{
		Message: github.Ptr(message),
		Tree:    tree,
	}
	if parentSHA != "" {
		commitInput.Parents = []*github.Commit{{SHA: github.Ptr(parentSHA)}}
	}
	commit, _, err := c.gh.Git.CreateCommit(ctx, owner, repo, commitInput, nil)
	if err != nil {
		return "", fmt.Errorf("creating commit: %w", err)
	}

	ref := "refs/heads/" + branch
	if parentSHA == "" {
		if _, _, err := c.gh.Git.CreateRef(ctx, owner, repo, github.CreateRef{
			Ref: ref,
			SHA: commit.GetSHA(),
		}); err != nil {
			return "", fmt.Errorf("creating ref %s: %w", ref, err)
		}
	} else {
		if _, _, err := c.gh.Git.UpdateRef(ctx, owner, repo, ref, github.UpdateRef{SHA: commit.GetSHA()}); err != nil {
			return "", fmt.Errorf("updating ref %s: %w", ref, err)
		}
	}

	return commit.GetSHA(), nil
}

func isNotFound(err error) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

// isEmptyRepository reports whether err is GitHub's documented response for
// a git-data read against a repository with no commit history: a 409
// Conflict ("Git Repository is empty"), not a 404.
func isEmptyRepository(err error) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusConflict
}
