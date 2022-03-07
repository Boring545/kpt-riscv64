// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"context"
	"reflect"
	"strings"
	"testing"

	kptfilev1 "github.com/GoogleContainerTools/kpt/pkg/api/kptfile/v1"
	porchapi "github.com/GoogleContainerTools/kpt/porch/api/porch/v1alpha1"
	configapi "github.com/GoogleContainerTools/kpt/porch/controllers/pkg/apis/porch/v1alpha1"
	"github.com/google/go-cmp/cmp"
	coreapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

const (
	testBlueprintsRepo = "https://github.com/platkrm/test-blueprints.git"
)

func TestE2E(t *testing.T) {
	Run(&PorchSuite{}, t)
}

func Run(suite interface{}, t *testing.T) {
	sv := reflect.ValueOf(suite)
	st := reflect.TypeOf(suite)
	ctx := context.Background()

	t.Run(st.Elem().Name(), func(t *testing.T) {
		var ts *TestSuite = sv.Elem().FieldByName("TestSuite").Addr().Interface().(*TestSuite)

		ts.T = t
		if init, ok := suite.(Initializer); ok {
			init.Initialize(ctx)
		}

		for i, max := 0, st.NumMethod(); i < max; i++ {
			m := st.Method(i)
			if strings.HasPrefix(m.Name, "Test") {
				t.Run(m.Name, func(t *testing.T) {
					ts.T = t
					m.Func.Call([]reflect.Value{sv, reflect.ValueOf(ctx)})
				})
			}
		}
	})
}

type PorchSuite struct {
	TestSuite
}

var _ Initializer = &PorchSuite{}

func (p *PorchSuite) Initialize(ctx context.Context) {
	p.TestSuite.Initialize(ctx)
}

func (t *PorchSuite) TestGitRepository(ctx context.Context) {
	config := t.CreateGitRepo()

	var secret string

	// Create auth secret if necessary
	if config.Username != "" || config.Password != "" {
		const credSecret = "git-repository-auth"
		immutable := true
		t.CreateF(ctx, &coreapi.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      credSecret,
				Namespace: t.namespace,
			},
			Immutable: &immutable,
			Data: map[string][]byte{
				"username": []byte(config.Username),
				"password": []byte(config.Password),
			},
			Type: coreapi.SecretTypeBasicAuth,
		})

		secret = credSecret

		t.Cleanup(func() {
			t.DeleteE(ctx, &coreapi.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      credSecret,
					Namespace: t.namespace,
				},
			})
		})
	}

	// Register repository
	t.CreateF(ctx, &configapi.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "git",
			Namespace: t.namespace,
		},
		Spec: configapi.RepositorySpec{
			Title:       "Porch Test Repository",
			Description: "Porch Test Repository Description",
			Type:        configapi.RepositoryTypeGit,
			Content:     configapi.RepositoryContentPackage,
			Git: &configapi.GitRepository{
				Repo:      config.Repo,
				Branch:    config.Branch,
				Directory: config.Directory,
				SecretRef: configapi.SecretRef{
					Name: secret,
				},
			},
		},
	})

	t.Cleanup(func() {
		t.DeleteE(ctx, &configapi.Repository{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "git",
				Namespace: t.namespace,
			},
		})
	})

	// Create Package Revision
	t.CreateF(ctx, &porchapi.PackageRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "git:test-bucket:v1",
			Namespace: t.namespace,
		},
		Spec: porchapi.PackageRevisionSpec{
			PackageName:    "test-bucket",
			Revision:       "v1",
			RepositoryName: "git",
			Tasks: []porchapi.Task{
				{
					Type: "clone",
					Clone: &porchapi.PackageCloneTaskSpec{
						Upstream: porchapi.UpstreamPackage{
							Type: "git",
							Git: &porchapi.GitPackage{
								Repo:      "https://github.com/GoogleCloudPlatform/blueprints.git",
								Ref:       "bucket-blueprint-v0.4.3",
								Directory: "catalog/bucket",
							},
						},
					},
				},
				{
					Type: "eval",
					Eval: &porchapi.FunctionEvalTaskSpec{
						Image: "gcr.io/kpt-fn/set-namespace:v0.2.0",
						ConfigMap: map[string]string{
							"namespace": "bucket-namespace",
						},
					},
				},
			},
		},
	})

	// Get package resources
	var resources porchapi.PackageRevisionResources
	t.GetF(ctx, client.ObjectKey{
		Namespace: t.namespace,
		Name:      "git:test-bucket:v1",
	}, &resources)

	bucket, ok := resources.Spec.Resources["bucket.yaml"]
	if !ok {
		t.Errorf("'bucket.yaml' not found among package resources")
	}
	node, err := yaml.Parse(bucket)
	if err != nil {
		t.Errorf("yaml.Parse(\"bucket.yaml\") failed: %v", err)
	}
	if got, want := node.GetNamespace(), "bucket-namespace"; got != want {
		t.Errorf("StorageBucket namespace: got %q, want %q", got, want)
	}

	// Register Upstream Repository
	t.registerGitRepositoryF(ctx, testBlueprintsRepo, "test-blueprints")

	var pr porchapi.PackageRevisionResourcesList
	t.ListE(ctx, &pr)

	// Ensure basens package exists
	const name = "test-blueprints:basens:v1"
	found := false
	for _, r := range pr.Items {
		if r.Name == name {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Repository %q doesn't contain package %q", testBlueprintsRepo, name)
	}

	// Create PackageRevision from upstream repo
	t.CreateF(ctx, &porchapi.PackageRevision{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PackageRevision",
			APIVersion: porchapi.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "git:istions:v1",
			Namespace: t.namespace,
		},
		Spec: porchapi.PackageRevisionSpec{
			PackageName:    "istions",
			Revision:       "v1",
			RepositoryName: "git",
			Tasks: []porchapi.Task{
				{
					Type: porchapi.TaskTypeClone,
					Clone: &porchapi.PackageCloneTaskSpec{
						Upstream: porchapi.UpstreamPackage{
							UpstreamRef: porchapi.PackageRevisionRef{
								Name: "test-blueprints:basens:v1", // Clone from basens/v1
							},
						},
					},
				},
			},
		},
	})

	// Get istions resources
	var istions porchapi.PackageRevisionResources
	t.GetF(ctx, client.ObjectKey{
		Namespace: t.namespace,
		Name:      "git:istions:v1",
	}, &istions)

	kptfile := t.ParseKptfileF(&istions)

	if got, want := kptfile.Name, "istions"; got != want {
		t.Errorf("istions package Kptfile.metadata.name: got %q, want %q", got, want)
	}
	if kptfile.UpstreamLock == nil {
		t.Fatalf("istions package upstreamLock is missing")
	}
	if kptfile.UpstreamLock.Git == nil {
		t.Errorf("istions package upstreamLock.git is missing")
	}
	if kptfile.UpstreamLock.Git.Commit == "" {
		t.Errorf("isions package upstreamLock.gkti.commit is missing")
	}

	// Remove commit from comparison
	got := kptfile.UpstreamLock
	got.Git.Commit = ""

	want := &kptfilev1.UpstreamLock{
		Type: kptfilev1.GitOrigin,
		Git: &kptfilev1.GitLock{
			Repo:      testBlueprintsRepo,
			Directory: "basens",
			Ref:       "v1",
		},
	}
	if !cmp.Equal(want, got) {
		t.Errorf("unexpected upstreamlock returned (-want, +got) %s", cmp.Diff(want, got))
	}

	// Check Upstream
	if got, want := kptfile.Upstream, (&kptfilev1.Upstream{
		Type: kptfilev1.GitOrigin,
		Git: &kptfilev1.Git{
			Repo:      testBlueprintsRepo,
			Directory: "basens",
			Ref:       "v1",
		},
	}); !cmp.Equal(want, got) {
		t.Errorf("unexpected upstream returned (-want, +got) %s", cmp.Diff(want, got))
	}

	{
		// Create a new package via init, no task specified
		const name = "git:empty-package:v1"
		const description = "empty-package description"
		const site = "https://kpt.dev/empty-package"

		// Create a new package (via init)
		t.CreateF(ctx, &porchapi.PackageRevision{
			TypeMeta: metav1.TypeMeta{
				Kind:       "PackageRevision",
				APIVersion: porchapi.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: t.namespace,
			},
			Spec: porchapi.PackageRevisionSpec{
				PackageName:    "empty-package",
				Revision:       "v1",
				RepositoryName: "git",
			},
		})

		// Get the package
		var newPackage porchapi.PackageRevisionResources
		t.GetF(ctx, client.ObjectKey{
			Namespace: t.namespace,
			Name:      name,
		}, &newPackage)

		kptfile = t.ParseKptfileF(&newPackage)
		if got, want := kptfile.Name, "empty-package"; got != want {
			t.Fatalf("New package name: got %q, want %q", got, want)
		}
		if got, want := kptfile.Info, (&kptfilev1.PackageInfo{
			Description: description,
		}); !cmp.Equal(want, got) {
			t.Fatalf("unexpected %s/%s package info (-want, +got) %s", newPackage.Namespace, newPackage.Name, cmp.Diff(want, got))
		}
	}

	{
		const name = "git:new-package:v1"
		const description = "New Package"
		const site = "https://kpt.dev/new-package"
		keywords := []string{"test"}

		// Create a new package (via init)
		t.CreateF(ctx, &porchapi.PackageRevision{
			TypeMeta: metav1.TypeMeta{
				Kind:       "PackageRevision",
				APIVersion: porchapi.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: t.namespace,
			},
			Spec: porchapi.PackageRevisionSpec{
				PackageName:    "new-package",
				Revision:       "v1",
				RepositoryName: "git",
				Tasks: []porchapi.Task{
					{
						Type: porchapi.TaskTypeInit,
						Init: &porchapi.PackageInitTaskSpec{
							Description: description,
							Keywords:    keywords,
							Site:        site,
						},
					},
				},
			},
		})

		// Get the package
		var newPackage porchapi.PackageRevisionResources
		t.GetF(ctx, client.ObjectKey{
			Namespace: t.namespace,
			Name:      name,
		}, &newPackage)

		kptfile = t.ParseKptfileF(&newPackage)
		if got, want := kptfile.Name, "new-package"; got != want {
			t.Fatalf("New package name: got %q, want %q", got, want)
		}
		if got, want := kptfile.Info, (&kptfilev1.PackageInfo{
			Site:        site,
			Description: description,
			Keywords:    keywords,
		}); !cmp.Equal(want, got) {
			t.Fatalf("unexpected %s/%s package info (-want, +got) %s", newPackage.Namespace, newPackage.Name, cmp.Diff(want, got))
		}
	}
}

func (t *PorchSuite) TestFunctionRepository(ctx context.Context) {
	t.CreateF(ctx, &configapi.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "function-repository",
			Namespace: t.namespace,
		},
		Spec: configapi.RepositorySpec{
			Title:       "Function Repository",
			Description: "Test Function Repository",
			Type:        configapi.RepositoryTypeOCI,
			Content:     configapi.RepositoryContentFunction,
			Oci: &configapi.OciRepository{
				Registry: "gcr.io/kpt-fn",
			},
		},
	})

	t.Cleanup(func() {
		t.DeleteL(ctx, &configapi.Repository{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "function-repository",
				Namespace: t.namespace,
			},
		})
	})

	list := &porchapi.FunctionList{}
	t.ListE(ctx, list)

	if got := len(list.Items); got == 0 {
		t.Errorf("Found no functions in gcr.io/kpt-fn repository; expected at least one")
	}
}

func (t *PorchSuite) TestPublicGitRepository(ctx context.Context) {
	t.registerGitRepositoryF(ctx, testBlueprintsRepo, "demo-blueprints")

	var list porchapi.PackageRevisionList
	t.ListE(ctx, &list)

	if got := len(list.Items); got == 0 {
		t.Errorf("Found no package revisions in %s; expected at least one", testBlueprintsRepo)
	}
}

func (t *PorchSuite) TestDevPorch(ctx context.Context) {
	t.IsUsingDevPorch()
}

func (t *PorchSuite) registerGitRepositoryF(ctx context.Context, repo, name string) {
	t.CreateF(ctx, &configapi.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: t.namespace,
		},
		Spec: configapi.RepositorySpec{
			Title:   "Public Git Repository",
			Type:    configapi.RepositoryTypeGit,
			Content: configapi.RepositoryContentPackage,
			Git: &configapi.GitRepository{
				Repo:   repo,
				Branch: "main",
			},
		},
	})

	t.Cleanup(func() {
		t.DeleteL(ctx, &configapi.Repository{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: t.namespace,
			},
		})
	})
}
