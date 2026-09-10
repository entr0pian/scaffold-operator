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
	"testing"
)

func testParams() map[string]string {
	return map[string]string{
		"componentName":  "payments",
		"repositoryName": "payments",
		"owner":          "entr0pian",
		"componentOwner": "payments-team",
	}
}

func TestRenderTemplate(t *testing.T) {
	templateDir := map[string][]byte{
		"go.mod.tpl":                      []byte("module github.com/{{ owner }}/{{ repositoryName }}\n\ngo 1.22\n"),
		"README.md.tpl":                   []byte("# {{ componentName }}\n"),
		"cmd/server/main.go.tpl":          []byte("// {{ componentName }} server\npackage main\n"),
		"chart/templates/deployment.yaml": []byte("name: {{ .Chart.Name }}\nimage: {{ .Values.image.repository }}\n"),
		"chart/templates/service.yaml":    []byte("port: {{ .Values.service.port }}\n"),
		".github/workflows/ci.yaml":       []byte("image: {{ env.IMAGE }}\nactor: {{ github.actor }}\n"),
		"internal/.gitkeep":               []byte(""),
		"Makefile":                        []byte("build:\n\tgo build ./...\n"),
		"catalog-info.yaml.tpl":           []byte("metadata:\n  name: {{ componentName }}\n  annotations:\n    github.com/project-slug: {{ owner }}/{{ repositoryName }}\nspec:\n  owner: {{ componentOwner }}\n"),
	}

	out := renderTemplate(templateDir, testParams())

	// .tpl files: suffix stripped, exact-name placeholders substituted.
	if got, want := string(out["go.mod"]), "module github.com/entr0pian/payments\n\ngo 1.22\n"; got != want {
		t.Errorf("go.mod = %q, want %q", got, want)
	}
	if _, stillTpl := out["go.mod.tpl"]; stillTpl {
		t.Error("go.mod.tpl should not appear in output; only the stripped name should")
	}
	if got, want := string(out["README.md"]), "# payments\n"; got != want {
		t.Errorf("README.md = %q, want %q", got, want)
	}
	if got, want := string(out["cmd/server/main.go"]), "// payments server\npackage main\n"; got != want {
		t.Errorf("cmd/server/main.go = %q, want %q", got, want)
	}
	if got, want := string(out["catalog-info.yaml"]), "metadata:\n  name: payments\n  annotations:\n    github.com/project-slug: entr0pian/payments\nspec:\n  owner: payments-team\n"; got != want {
		t.Errorf("catalog-info.yaml = %q, want %q", got, want)
	}

	// Non-.tpl files: copied byte-for-byte, including files that contain
	// unrelated Helm/GitHub Actions {{ }} syntax that must NOT be touched.
	if got, want := string(out["chart/templates/deployment.yaml"]), "name: {{ .Chart.Name }}\nimage: {{ .Values.image.repository }}\n"; got != want {
		t.Errorf("chart/templates/deployment.yaml was modified: got %q, want %q", got, want)
	}
	if got, want := string(out[".github/workflows/ci.yaml"]), "image: {{ env.IMAGE }}\nactor: {{ github.actor }}\n"; got != want {
		t.Errorf(".github/workflows/ci.yaml was modified: got %q, want %q", got, want)
	}
	if got, want := string(out["Makefile"]), "build:\n\tgo build ./...\n"; got != want {
		t.Errorf("Makefile was modified: got %q, want %q", got, want)
	}

	if len(out) != len(templateDir) {
		t.Errorf("output has %d files, want %d (one output file per input file)", len(out), len(templateDir))
	}
}

func TestSubstitute_UnsupportedPlaceholderLeftAlone(t *testing.T) {
	content := []byte("{{ componentName }} / {{ madeUpParam }} / {{ .Values.x }}")
	got := string(substitute(content, testParams()))
	want := "payments / {{ madeUpParam }} / {{ .Values.x }}"
	if got != want {
		t.Errorf("substitute() = %q, want %q", got, want)
	}
}

func TestValidateScaffoldContract(t *testing.T) {
	contract := []byte(`
parameters:
  componentName:
    required: true
  repositoryName:
    required: true
  owner:
    required: true
`)

	t.Run("all required params present", func(t *testing.T) {
		if err := validateScaffoldContract(contract, testParams()); err != nil {
			t.Errorf("validateScaffoldContract() = %v, want nil", err)
		}
	})

	t.Run("required param empty", func(t *testing.T) {
		params := testParams()
		params["owner"] = ""
		if err := validateScaffoldContract(contract, params); err == nil {
			t.Error("validateScaffoldContract() = nil, want error for empty required param")
		}
	})

	t.Run("scaffold.yaml requires an unsupported parameter", func(t *testing.T) {
		unsupported := []byte(`
parameters:
  componentName:
    required: true
  unsupportedThing:
    required: true
`)
		if err := validateScaffoldContract(unsupported, testParams()); err == nil {
			t.Error("validateScaffoldContract() = nil, want error for a required parameter this renderer has no value for")
		}
	})

	t.Run("optional params are not checked", func(t *testing.T) {
		withOptional := []byte(`
parameters:
  componentName:
    required: true
  description:
    required: false
`)
		if err := validateScaffoldContract(withOptional, testParams()); err != nil {
			t.Errorf("validateScaffoldContract() = %v, want nil (optional param should not be checked)", err)
		}
	})
}
