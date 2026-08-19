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
	"fmt"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// scaffoldContract is the subset of platform-scaffolds' scaffold.yaml this
// controller reads: which parameters the template declares required. Every
// other field (name, description, version) is documentation for humans and
// is not consumed here.
type scaffoldContract struct {
	Parameters map[string]struct {
		Required bool `json:"required"`
	} `json:"parameters"`
}

// validateScaffoldContract confirms every parameter scaffold.yaml marks
// required has a non-empty value in paramValues, and that scaffold.yaml
// doesn't declare a required parameter this renderer has no value for at
// all. No other parameters are supported for golang-service today (or any
// template) — a scaffold.yaml requiring one this controller doesn't know
// about is a contract change that needs a corresponding code change here,
// not something the renderer improvises around.
func validateScaffoldContract(raw []byte, paramValues map[string]string) error {
	var contract scaffoldContract
	if err := yaml.Unmarshal(raw, &contract); err != nil {
		return fmt.Errorf("parsing scaffold.yaml: %w", err)
	}

	for name, param := range contract.Parameters {
		if !param.Required {
			continue
		}
		value, known := paramValues[name]
		if !known {
			return fmt.Errorf("scaffold.yaml requires parameter %q, which this scaffold-operator does not supply", name)
		}
		if value == "" {
			return fmt.Errorf("required parameter %q is empty", name)
		}
	}

	return nil
}

// placeholderPattern matches a bare {{ name }} placeholder — a single
// identifier, optionally surrounded by whitespace, and nothing else. This is
// deliberately narrow: it must never match Helm (`{{ .Values.x }}`,
// `{{- toYaml ... }}`) or GitHub Actions (`{{ github.sha }}`) template
// syntax, which is why non-.tpl files are never passed through substitute at
// all, and why even within a .tpl file this never evaluates or parses
// arbitrary `{{ ... }}` expressions — only an exact, known parameter name is
// ever substituted.
var placeholderPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// substitute replaces every {{ paramName }} placeholder in content with its
// value from params. A placeholder whose name isn't in params — a typo or an
// unsupported parameter — is left in place untouched rather than silently
// dropped or guessed at.
func substitute(content []byte, params map[string]string) []byte {
	return placeholderPattern.ReplaceAllFunc(content, func(match []byte) []byte {
		name := string(placeholderPattern.FindSubmatch(match)[1])
		if value, ok := params[name]; ok {
			return []byte(value)
		}
		return match
	})
}

// renderTemplate turns a fetched template/ directory (paths already relative
// to the template root, e.g. "go.mod.tpl", "chart/templates/deployment.yaml")
// into the final file set to commit: the .tpl suffix stripped and its
// content substituted for .tpl files, non-.tpl files copied byte-for-byte
// and never passed through substitute — per platform-scaffolds' own
// contract, only .tpl files may contain a scaffold placeholder at all.
func renderTemplate(templateDir map[string][]byte, params map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(templateDir))
	for path, content := range templateDir {
		if rendered, isTpl := strings.CutSuffix(path, ".tpl"); isTpl {
			out[rendered] = substitute(content, params)
		} else {
			out[path] = content
		}
	}
	return out
}
