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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"

	"github.com/google/go-github/v88/github"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// tokenRequest records how a stand-in installation transport was built; the
// tests never contact GitHub.
type tokenRequest struct {
	appID, installationID int64
	opts                  *github.InstallationTokenOptions
}

var _ = Describe("GitHub App authentication", func() {
	const appNS = "scaffold-operator-system"
	secretName := types.NamespacedName{Namespace: appNS, Name: "scaffold-operator-github-app"}

	var (
		requests []tokenRequest
		source   *appSource
	)

	privateKey := func() []byte {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred())
		return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	}

	writeSecret := func(data map[string][]byte) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName.Name, Namespace: secretName.Namespace}}
		err := k8sClient.Get(ctx, secretName, secret)
		if errors.IsNotFound(err) {
			secret.Data = data
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			return
		}
		Expect(err).NotTo(HaveOccurred())
		secret.Data = data
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
	}

	validSecret := func() map[string][]byte {
		return map[string][]byte{appIDKey: []byte("123"), installationIDKey: []byte("456"), privateKeyKey: privateKey()}
	}

	BeforeEach(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: appNS}})
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName.Name, Namespace: secretName.Namespace}})

		requests = nil
		source = &appSource{
			reader: k8sClient,
			secret: secretName,
			newTransport: func(appID, installationID int64, _ []byte, opts *github.InstallationTokenOptions) (http.RoundTripper, error) {
				requests = append(requests, tokenRequest{appID, installationID, opts})
				return http.DefaultTransport, nil
			},
		}
	})

	It("reads platform-scaffolds only, and writes contents and workflows only to the target repository", func() {
		writeSecret(validSecret())

		_, err := source.clientFor(ctx, "payments")
		Expect(err).NotTo(HaveOccurred())

		Expect(requests).To(HaveLen(2))
		for _, r := range requests {
			Expect(r.appID).To(Equal(int64(123)))
			Expect(r.installationID).To(Equal(int64(456)))
		}
		Expect(requests[0].opts.Repositories).To(Equal([]string{scaffoldsRepo}))
		Expect(requests[0].opts.Permissions).To(Equal(&github.InstallationPermissions{Contents: github.Ptr("read")}))
		Expect(requests[1].opts.Repositories).To(Equal([]string{"payments"}))
		Expect(requests[1].opts.Permissions).To(Equal(&github.InstallationPermissions{
			Contents: github.Ptr("write"), Workflows: github.Ptr("write"),
		}))
	})

	It("reuses the platform-scaffolds client until the Secret changes, but scopes each target separately", func() {
		writeSecret(validSecret())
		first, err := source.clientFor(ctx, "payments")
		Expect(err).NotTo(HaveOccurred())
		again, err := source.clientFor(ctx, "orders")
		Expect(err).NotTo(HaveOccurred())
		Expect(again.(*repoRoutingClient).templates).To(BeIdenticalTo(first.(*repoRoutingClient).templates))
		Expect(requests).To(HaveLen(3), "one platform-scaffolds token, then one per target")
		Expect(requests[2].opts.Repositories).To(Equal([]string{"orders"}))

		writeSecret(validSecret())
		rotated, err := source.clientFor(ctx, "payments")
		Expect(err).NotTo(HaveOccurred())
		Expect(rotated.(*repoRoutingClient).templates).NotTo(BeIdenticalTo(first.(*repoRoutingClient).templates))
		Expect(requests).To(HaveLen(5))
	})

	It("refuses a write token for platform-scaffolds itself", func() {
		writeSecret(validSecret())
		_, err := source.clientFor(ctx, scaffoldsRepo)
		Expect(err).To(MatchError(ContainSubstring("refusing a write token")))
		Expect(requests).To(BeEmpty())
	})

	It("fails, without falling back to the PAT, when the Secret is missing", func() {
		_, err := source.clientFor(ctx, "payments")
		Expect(err).To(MatchError(ContainSubstring("reading GitHub App credentials secret scaffold-operator-system/scaffold-operator-github-app")))
		Expect(requests).To(BeEmpty())
	})

	It("rejects a Secret with a missing or malformed key", func() {
		data := validSecret()
		delete(data, privateKeyKey)
		writeSecret(data)
		_, err := source.clientFor(ctx, "payments")
		Expect(err).To(MatchError(ContainSubstring(`no "privateKey" key`)))

		data = validSecret()
		data[appIDKey] = []byte("abc")
		writeSecret(data)
		_, err = source.clientFor(ctx, "payments")
		Expect(err).To(MatchError(ContainSubstring(`"appId" is not a number`)))
	})

	It("is the only source a reconciler without credentials would use", func() {
		_, err := (&ScaffoldRequestReconciler{}).githubClientFor(ctx, "payments")
		Expect(err).To(MatchError("no GitHub credentials configured"))
	})
})

var _ = Describe("repoRoutingClient", func() {
	It("sends platform-scaffolds calls to the templates client and the rest to the target client", func() {
		// Each stub fails the test on any call it wasn't set up for.
		templates := &stubGitHubClient{
			resolveRevisionFn: func(context.Context, string, string, string) (string, error) {
				return testTemplateRevision, nil
			},
		}
		target := &stubGitHubClient{
			commitFilesFn: func(context.Context, string, string, string, string, map[string][]byte, string, string) (string, error) {
				return "c0ffee", nil
			},
		}
		c := &repoRoutingClient{templates: templates, target: target}

		sha, err := c.ResolveRevision(context.Background(), "entr0pian", scaffoldsRepo, "golang-service/v0.1.0")
		Expect(err).NotTo(HaveOccurred())
		Expect(sha).To(Equal(testTemplateRevision))

		commit, err := c.CommitFiles(context.Background(), "entr0pian", "payments", "main", "m", map[string][]byte{"f": []byte("x")}, "", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(commit).To(Equal("c0ffee"))
		Expect(target.commitFilesCalls).To(Equal(1))
		Expect(templates.commitFilesCalls).To(BeZero())
	})
})
