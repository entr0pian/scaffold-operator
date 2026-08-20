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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	scaffoldv1alpha1 "github.com/entr0pian/scaffold-operator/api/v1alpha1"
)

const (
	// scaffoldsOwner/scaffoldsRepo is platform-scaffolds itself, the fixed
	// source of every template this controller renders. Not configurable —
	// a second scaffold source is a future extension, not something today's
	// contract needs.
	scaffoldsOwner = "entr0pian"
	scaffoldsRepo  = "platform-scaffolds"

	// credentialsSecretName/credentialsSecretNamespace is the existing
	// crossplane-github-credentials Secret, reused here as a documented
	// temporary tradeoff (see this repo's README) rather than provisioning a
	// second, narrower-scoped token right now.
	credentialsSecretName      = "crossplane-github-credentials"
	credentialsSecretNamespace = "crossplane-system"
)

// ScaffoldRequestReconciler reconciles a ScaffoldRequest object
type ScaffoldRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewGitHubClient constructs the GitHub API client used to execute a
	// request, given the token read from the crossplane-github-credentials
	// Secret. Defaults to a real go-github-backed client (see
	// github_client.go); overridden in tests with a stub that never makes a
	// network call.
	NewGitHubClient func(token string) githubClient
}

// +kubebuilder:rbac:groups=scaffold.taskapp.io,resources=scaffoldrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=scaffold.taskapp.io,resources=scaffoldrequests/status,verbs=get;update;patch

// Reconcile executes a ScaffoldRequest at most once: it renders the
// requested platform-scaffolds template and makes one atomic commit into the
// target repository, then marks the request Completed (or Blocked, if the
// target repository already has content this request cannot prove it
// created). Neither terminal state is ever retried automatically — see
// PLATFORM_API_ARCHITECTURE.md's CREATION EXCEPTION: ScaffoldRequest section
// and this repo's README for the full safety reasoning.
func (r *ScaffoldRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	sr := &scaffoldv1alpha1.ScaffoldRequest{}
	if err := r.Get(ctx, req.NamespacedName, sr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if isConditionTrue(sr.Status.Conditions, "Completed") || isConditionTrue(sr.Status.Conditions, "Blocked") {
		return ctrl.Result{}, nil
	}

	statusBase := sr.DeepCopy()

	gh, err := r.githubClientFor(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.execute(ctx, sr, gh); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.patchStatusIfChanged(ctx, statusBase, sr); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciled ScaffoldRequest", "scaffoldrequest", sr.Name)
	return ctrl.Result{}, nil
}

// execute runs the render-and-commit flow once, mutating sr.Status in
// place. A returned error means a transient failure (network, rate limit,
// ...) that should be retried with backoff; a terminal outcome (success or
// an unrecoverable block) is recorded as a condition and returns nil so the
// step-1 guard in Reconcile takes over on every future reconcile.
func (r *ScaffoldRequestReconciler) execute(ctx context.Context, sr *scaffoldv1alpha1.ScaffoldRequest, gh githubClient) error {
	log := logf.FromContext(ctx)

	ref := sr.Spec.Template + "/v" + sr.Spec.Version
	sha, err := gh.ResolveRevision(ctx, scaffoldsOwner, scaffoldsRepo, ref)
	if err != nil {
		return fmt.Errorf("resolving scaffold template revision %s: %w", ref, err)
	}
	// Recorded immediately, before any further step, so provenance of what
	// was fetched survives even if a later step fails or blocks.
	sr.Status.TemplateRevision = sha

	prefix := fmt.Sprintf("templates/%s/", sr.Spec.Template)
	tree, err := gh.FetchTree(ctx, scaffoldsOwner, scaffoldsRepo, sha, prefix)
	if err != nil {
		return fmt.Errorf("fetching scaffold template %s: %w", sr.Spec.Template, err)
	}

	scaffoldYAML, ok := tree[prefix+"scaffold.yaml"]
	if !ok {
		r.block(sr, "MissingScaffoldContract", fmt.Sprintf("template %q has no scaffold.yaml at revision %s", sr.Spec.Template, sha))
		return nil
	}

	params := map[string]string{
		"componentName":  sr.Spec.ComponentName,
		"repositoryName": sr.Spec.RepositoryName,
		"owner":          sr.Spec.Owner,
	}
	if err := validateScaffoldContract(scaffoldYAML, params); err != nil {
		r.block(sr, "InvalidScaffoldContract", err.Error())
		return nil
	}

	templateDir := make(map[string][]byte)
	templatePrefix := prefix + "template/"
	for path, content := range tree {
		if rel, ok := strings.CutPrefix(path, templatePrefix); ok {
			templateDir[rel] = content
		}
	}
	files := renderTemplate(templateDir, params)

	defaultBranch, commitCount, headSHA, headTreeSHA, err := gh.RepositoryState(ctx, sr.Spec.Owner, sr.Spec.RepositoryName)
	if err != nil {
		return fmt.Errorf("reading target repository %s/%s state: %w", sr.Spec.Owner, sr.Spec.RepositoryName, err)
	}

	// A recorded commitSHA that's still reachable means a prior run of this
	// exact request already committed and the status patch never landed —
	// genuine crash/status-write-loss recovery, checked before anything
	// else regardless of the repository's current commit count.
	if sr.Status.CommitSHA != "" {
		exists, err := gh.CommitExists(ctx, sr.Spec.Owner, sr.Spec.RepositoryName, sr.Status.CommitSHA)
		if err != nil {
			return fmt.Errorf("verifying recovery commit %s in %s/%s: %w", sr.Status.CommitSHA, sr.Spec.Owner, sr.Spec.RepositoryName, err)
		}
		if exists {
			r.complete(sr, "recovered: a prior run's commit is already present in the repository")
			return nil
		}
	}

	var parentSHA, baseTreeSHA string
	switch {
	case commitCount >= 2:
		// Two or more commits already exist and this request has no
		// reachable commitSHA of its own — no proof it produced any of this
		// content. Favor safe failure over repair or overwrite.
		r.block(sr, "RepositoryNotEmpty", fmt.Sprintf("%s/%s already has commits this request did not create", sr.Spec.Owner, sr.Spec.RepositoryName))
		return nil
	case commitCount == 1:
		// The expected starting state for a freshly created repository
		// (component-operator always sets autoInit:true) — build the
		// scaffold commit on top of GitHub's auto-generated commit rather
		// than requiring a genuinely empty repository, which GitHub's Git
		// Data API rejects outright (see CommitFiles).
		parentSHA, baseTreeSHA = headSHA, headTreeSHA
	}

	message := fmt.Sprintf("initial scaffold: %s@%s", sr.Spec.Template, sr.Spec.Version)
	commitSHA, err := gh.CommitFiles(ctx, sr.Spec.Owner, sr.Spec.RepositoryName, defaultBranch, message, files, parentSHA, baseTreeSHA)
	if err != nil {
		return fmt.Errorf("committing scaffold to %s/%s: %w", sr.Spec.Owner, sr.Spec.RepositoryName, err)
	}
	sr.Status.CommitSHA = commitSHA
	r.complete(sr, "scaffold committed to repository")
	log.Info("scaffold committed", "repo", sr.Spec.Owner+"/"+sr.Spec.RepositoryName, "commit", commitSHA)
	return nil
}

// githubCredentials mirrors the single "credentials" key on the
// crossplane-github-credentials Secret — a JSON blob, not separate Secret
// keys.
type githubCredentials struct {
	Token string `json:"token"`
	Owner string `json:"owner"`
}

// githubClientFor reads the shared crossplane-github-credentials Secret
// (crossplane-system namespace) via a direct client.Get — not an env/volume
// mount, since Secrets don't mount cross-namespace — and constructs a GitHub
// client from its token.
func (r *ScaffoldRequestReconciler) githubClientFor(ctx context.Context) (githubClient, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: credentialsSecretName, Namespace: credentialsSecretNamespace}, secret); err != nil {
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

	newClient := r.NewGitHubClient
	if newClient == nil {
		newClient = newGoGithubClient
	}
	return newClient(creds.Token), nil
}

func (r *ScaffoldRequestReconciler) block(sr *scaffoldv1alpha1.ScaffoldRequest, reason, message string) {
	setCondition(&sr.Status.Conditions, metav1.Condition{
		Type:               "Blocked",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: sr.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

func (r *ScaffoldRequestReconciler) complete(sr *scaffoldv1alpha1.ScaffoldRequest, message string) {
	setCondition(&sr.Status.Conditions, metav1.Condition{
		Type:               "Completed",
		Status:             metav1.ConditionTrue,
		Reason:             "Completed",
		Message:            message,
		ObservedGeneration: sr.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

func isConditionTrue(conditions []metav1.Condition, condType string) bool {
	cond := findCondition(conditions, condType)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

func setCondition(conditions *[]metav1.Condition, cond metav1.Condition) {
	existing := findCondition(*conditions, cond.Type)
	if existing == nil {
		*conditions = append(*conditions, cond)
		return
	}
	if existing.Status != cond.Status {
		existing.LastTransitionTime = metav1.Now()
	}
	existing.Status = cond.Status
	existing.Reason = cond.Reason
	existing.Message = cond.Message
	existing.ObservedGeneration = cond.ObservedGeneration
}

func (r *ScaffoldRequestReconciler) patchStatusIfChanged(ctx context.Context, statusBase, sr *scaffoldv1alpha1.ScaffoldRequest) error {
	if equality.Semantic.DeepEqual(statusBase.Status, sr.Status) {
		return nil
	}
	patch := client.MergeFrom(statusBase)
	return client.IgnoreNotFound(r.Status().Patch(ctx, sr, patch))
}

// SetupWithManager sets up the controller with the Manager.
func (r *ScaffoldRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&scaffoldv1alpha1.ScaffoldRequest{}).
		Named("scaffoldrequest").
		Complete(r)
}
