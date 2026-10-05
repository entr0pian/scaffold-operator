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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	scaffoldv1alpha1 "github.com/entr0pian/scaffold-operator/api/v1alpha1"
)

// stubGitHubClient is a githubClient that never makes a network call. Every
// method fails the running test if invoked without a corresponding function
// set — this is what lets the "already terminal, zero further calls" tests
// prove no GitHub API call happens at all, not just that none of them wrote
// anything.
type stubGitHubClient struct {
	resolveRevisionFn func(ctx context.Context, owner, repo, ref string) (string, error)
	fetchTreeFn       func(ctx context.Context, owner, repo, sha, prefix string) (map[string][]byte, error)
	repositoryStateFn func(ctx context.Context, owner, repo string) (string, int, string, string, error)
	commitExistsFn    func(ctx context.Context, owner, repo, sha string) (bool, error)
	commitFilesFn     func(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error)

	commitFilesCalls int
}

func (s *stubGitHubClient) ResolveRevision(ctx context.Context, owner, repo, ref string) (string, error) {
	if s.resolveRevisionFn == nil {
		Fail("unexpected ResolveRevision call")
	}
	return s.resolveRevisionFn(ctx, owner, repo, ref)
}

func (s *stubGitHubClient) FetchTree(ctx context.Context, owner, repo, sha, prefix string) (map[string][]byte, error) {
	if s.fetchTreeFn == nil {
		Fail("unexpected FetchTree call")
	}
	return s.fetchTreeFn(ctx, owner, repo, sha, prefix)
}

func (s *stubGitHubClient) RepositoryState(ctx context.Context, owner, repo string) (string, int, string, string, error) {
	if s.repositoryStateFn == nil {
		Fail("unexpected RepositoryState call")
	}
	return s.repositoryStateFn(ctx, owner, repo)
}

func (s *stubGitHubClient) CommitExists(ctx context.Context, owner, repo, sha string) (bool, error) {
	if s.commitExistsFn == nil {
		Fail("unexpected CommitExists call")
	}
	return s.commitExistsFn(ctx, owner, repo, sha)
}

func (s *stubGitHubClient) CommitFiles(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error) {
	s.commitFilesCalls++
	if s.commitFilesFn == nil {
		Fail("unexpected CommitFiles call")
	}
	return s.commitFilesFn(ctx, owner, repo, branch, message, files, parentSHA, baseTreeSHA)
}

// scaffoldTreeFixture is a minimal fake templates/golang-service/ tree, keyed
// exactly as FetchTree would return it (paths still carrying the
// templates/<template>/ prefix).
func scaffoldTreeFixture(prefix string) map[string][]byte {
	return map[string][]byte{
		prefix + "scaffold.yaml": []byte(`
parameters:
  componentName:
    required: true
  repositoryName:
    required: true
  owner:
    required: true
`),
		prefix + "template/go.mod.tpl":    []byte("module github.com/{{ owner }}/{{ repositoryName }}\n"),
		prefix + "template/README.md.tpl": []byte("# {{ componentName }}\n"),
		prefix + "template/Makefile":      []byte("build:\n\tgo build ./...\n"),
	}
}

func newScaffoldRequest(name string) *scaffoldv1alpha1.ScaffoldRequest {
	return &scaffoldv1alpha1.ScaffoldRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: scaffoldv1alpha1.ScaffoldRequestSpec{
			ComponentRef:   scaffoldv1alpha1.ComponentReference{Name: name},
			ComponentName:  name,
			RepositoryName: name,
			Owner:          "entr0pian",
			ComponentOwner: "team-payments",
			Template:       "golang-service",
			Version:        "0.1.0",
		},
	}
}

func ensureCredentialsSecret(ctx context.Context) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: credentialsSecretNamespace}}
	if err := k8sClient.Create(ctx, ns); err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentialsSecretName,
			Namespace: credentialsSecretNamespace,
		},
		Data: map[string][]byte{
			"credentials": []byte(`{"token":"fake-token","owner":"entr0pian"}`),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// Fixture values shared by the reconcile tests below.
const (
	testTemplatePrefix   = "templates/golang-service/"
	testTemplateRevision = "deadbeef"
	testDefaultBranch    = "main"
)

var _ = Describe("ScaffoldRequest Controller", func() {
	ctx := context.Background()

	reconcileWith := func(name string, gh githubClient) (reconcile.Result, error) {
		r := &ScaffoldRequestReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewGitHubClient: func(token string) githubClient {
				return gh
			},
		}
		return r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		})
	}

	getScaffoldRequest := func(name string) *scaffoldv1alpha1.ScaffoldRequest {
		sr := &scaffoldv1alpha1.ScaffoldRequest{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, sr)).To(Succeed())
		return sr
	}

	BeforeEach(func() {
		ensureCredentialsSecret(ctx)
	})

	Context("empty repository", func() {
		It("resolves the template revision, commits, and completes", func() {
			const name = "payments-empty"
			Expect(k8sClient.Create(ctx, newScaffoldRequest(name))).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, newScaffoldRequest(name))).To(Succeed())
			})

			prefix := testTemplatePrefix
			stub := &stubGitHubClient{
				resolveRevisionFn: func(ctx context.Context, owner, repo, ref string) (string, error) {
					Expect(owner).To(Equal(scaffoldsOwner))
					Expect(repo).To(Equal(scaffoldsRepo))
					Expect(ref).To(Equal("golang-service/v0.1.0"))
					return testTemplateRevision, nil
				},
				fetchTreeFn: func(ctx context.Context, owner, repo, sha, gotPrefix string) (map[string][]byte, error) {
					Expect(sha).To(Equal(testTemplateRevision))
					Expect(gotPrefix).To(Equal(prefix))
					return scaffoldTreeFixture(prefix), nil
				},
				repositoryStateFn: func(ctx context.Context, owner, repo string) (string, int, string, string, error) {
					Expect(owner).To(Equal("entr0pian"))
					Expect(repo).To(Equal(name))
					return "trunk", 0, "", "", nil // not "main" -- must not be hardcoded
				},
				commitFilesFn: func(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error) {
					Expect(branch).To(Equal("trunk"))
					Expect(parentSHA).To(BeEmpty())
					Expect(baseTreeSHA).To(BeEmpty())
					Expect(files).To(HaveKey("go.mod"))
					Expect(files).NotTo(HaveKey("go.mod.tpl"))
					Expect(string(files["go.mod"])).To(Equal(fmt.Sprintf("module github.com/entr0pian/%s\n", name)))
					return "commitsha123", nil
				},
			}

			_, err := reconcileWith(name, stub)
			Expect(err).NotTo(HaveOccurred())
			Expect(stub.commitFilesCalls).To(Equal(1))

			sr := getScaffoldRequest(name)
			Expect(sr.Status.TemplateRevision).To(Equal(testTemplateRevision))
			Expect(sr.Status.CommitSHA).To(Equal("commitsha123"))

			completed := findCondition(sr.Status.Conditions, "Completed")
			Expect(completed).NotTo(BeNil())
			Expect(completed.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("repository with a single existing commit (autoInit:true baseline)", func() {
		It("builds the scaffold commit on top of it and completes", func() {
			const name = "payments-autoinit"
			Expect(k8sClient.Create(ctx, newScaffoldRequest(name))).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, newScaffoldRequest(name))).To(Succeed())
			})

			prefix := testTemplatePrefix
			stub := &stubGitHubClient{
				resolveRevisionFn: func(ctx context.Context, owner, repo, ref string) (string, error) { return testTemplateRevision, nil },
				fetchTreeFn: func(ctx context.Context, owner, repo, sha, gotPrefix string) (map[string][]byte, error) {
					return scaffoldTreeFixture(prefix), nil
				},
				repositoryStateFn: func(ctx context.Context, owner, repo string) (string, int, string, string, error) {
					return testDefaultBranch, 1, "autoinitsha", "autoinittree", nil
				},
				commitFilesFn: func(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error) {
					Expect(parentSHA).To(Equal("autoinitsha"))
					Expect(baseTreeSHA).To(Equal("autoinittree"))
					return "commitsha456", nil
				},
			}

			_, err := reconcileWith(name, stub)
			Expect(err).NotTo(HaveOccurred())
			Expect(stub.commitFilesCalls).To(Equal(1))

			sr := getScaffoldRequest(name)
			Expect(sr.Status.CommitSHA).To(Equal("commitsha456"))
			completed := findCondition(sr.Status.Conditions, "Completed")
			Expect(completed).NotTo(BeNil())
			Expect(completed.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("non-empty repository", func() {
		It("recovers to Completed without writing when the recorded commitSHA is still reachable", func() {
			const name = "payments-recover"
			sr := newScaffoldRequest(name)
			Expect(k8sClient.Create(ctx, sr)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, newScaffoldRequest(name))).To(Succeed())
			})

			// Simulate a prior run that committed but never got to persist
			// status: patch commitSHA directly, without a Completed condition.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, sr)).To(Succeed())
			sr.Status.CommitSHA = "priorcommit"
			Expect(k8sClient.Status().Update(ctx, sr)).To(Succeed())

			prefix := testTemplatePrefix
			stub := &stubGitHubClient{
				resolveRevisionFn: func(ctx context.Context, owner, repo, ref string) (string, error) { return testTemplateRevision, nil },
				fetchTreeFn: func(ctx context.Context, owner, repo, sha, gotPrefix string) (map[string][]byte, error) {
					return scaffoldTreeFixture(prefix), nil
				},
				repositoryStateFn: func(ctx context.Context, owner, repo string) (string, int, string, string, error) {
					return testDefaultBranch, 2, "someothersha", "someothertree", nil
				},
				commitExistsFn: func(ctx context.Context, owner, repo, sha string) (bool, error) {
					Expect(sha).To(Equal("priorcommit"))
					return true, nil
				},
			}

			_, err := reconcileWith(name, stub)
			Expect(err).NotTo(HaveOccurred())
			Expect(stub.commitFilesCalls).To(Equal(0))

			result := getScaffoldRequest(name)
			completed := findCondition(result.Status.Conditions, "Completed")
			Expect(completed).NotTo(BeNil())
			Expect(completed.Status).To(Equal(metav1.ConditionTrue))
			Expect(result.Status.CommitSHA).To(Equal("priorcommit"))
		})

		It("blocks without writing when there is no verifiable prior commit", func() {
			const name = "payments-blocked"
			Expect(k8sClient.Create(ctx, newScaffoldRequest(name))).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, newScaffoldRequest(name))).To(Succeed())
			})

			prefix := testTemplatePrefix
			stub := &stubGitHubClient{
				resolveRevisionFn: func(ctx context.Context, owner, repo, ref string) (string, error) { return testTemplateRevision, nil },
				fetchTreeFn: func(ctx context.Context, owner, repo, sha, gotPrefix string) (map[string][]byte, error) {
					return scaffoldTreeFixture(prefix), nil
				},
				repositoryStateFn: func(ctx context.Context, owner, repo string) (string, int, string, string, error) {
					return testDefaultBranch, 2, "someothersha", "someothertree", nil // non-empty, and status.commitSHA is unset
				},
			}

			_, err := reconcileWith(name, stub)
			Expect(err).NotTo(HaveOccurred())
			Expect(stub.commitFilesCalls).To(Equal(0))

			sr := getScaffoldRequest(name)
			blocked := findCondition(sr.Status.Conditions, "Blocked")
			Expect(blocked).NotTo(BeNil())
			Expect(blocked.Status).To(Equal(metav1.ConditionTrue))
			Expect(blocked.Reason).To(Equal("RepositoryNotEmpty"))
		})
	})

	Context("terminal guard", func() {
		It("makes zero GitHub calls on a second reconcile once Completed", func() {
			const name = "payments-guard-completed"
			sr := newScaffoldRequest(name)
			Expect(k8sClient.Create(ctx, sr)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, newScaffoldRequest(name))).To(Succeed())
			})

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, sr)).To(Succeed())
			setCondition(&sr.Status.Conditions, metav1.Condition{
				Type: "Completed", Status: metav1.ConditionTrue, Reason: "Completed", Message: "done",
				ObservedGeneration: sr.Generation, LastTransitionTime: metav1.Now(),
			})
			Expect(k8sClient.Status().Update(ctx, sr)).To(Succeed())

			// No functions set on the stub: any call fails the test.
			_, err := reconcileWith(name, &stubGitHubClient{})
			Expect(err).NotTo(HaveOccurred())
		})

		It("makes zero GitHub calls on a second reconcile once Blocked", func() {
			const name = "payments-guard-blocked"
			sr := newScaffoldRequest(name)
			Expect(k8sClient.Create(ctx, sr)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, newScaffoldRequest(name))).To(Succeed())
			})

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, sr)).To(Succeed())
			setCondition(&sr.Status.Conditions, metav1.Condition{
				Type: "Blocked", Status: metav1.ConditionTrue, Reason: "RepositoryNotEmpty", Message: "blocked",
				ObservedGeneration: sr.Generation, LastTransitionTime: metav1.Now(),
			})
			Expect(k8sClient.Status().Update(ctx, sr)).To(Succeed())

			_, err := reconcileWith(name, &stubGitHubClient{})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
