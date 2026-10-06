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
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-github/v88/github"
)

func githubError(statusCode int) error {
	return &github.ErrorResponse{
		Response: &http.Response{StatusCode: statusCode},
		Message:  "synthetic test error",
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(githubError(http.StatusNotFound)) {
		t.Error("isNotFound(404) = false, want true")
	}
	if isNotFound(githubError(http.StatusConflict)) {
		t.Error("isNotFound(409) = true, want false")
	}
	if isNotFound(errors.New("not a github error")) {
		t.Error("isNotFound(non-github error) = true, want false")
	}
}

func TestIsEmptyRepository(t *testing.T) {
	// This is the actual bug this test guards against: GitHub returns 409
	// Conflict ("Git Repository is empty"), not 404, when reading git data
	// (e.g. a branch ref) from a repository with no commit history yet.
	// RepositoryState must recognize this as "empty", not as a hard error.
	if !isEmptyRepository(githubError(http.StatusConflict)) {
		t.Error("isEmptyRepository(409) = false, want true")
	}
	if isEmptyRepository(githubError(http.StatusNotFound)) {
		t.Error("isEmptyRepository(404) = true, want false")
	}
	if isEmptyRepository(errors.New("not a github error")) {
		t.Error("isEmptyRepository(non-github error) = true, want false")
	}
}
