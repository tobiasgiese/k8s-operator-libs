/*
Copyright 2024 NVIDIA CORPORATION & AFFILIATES

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

package crdutil

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	v1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
)

var (
	filenames []string
	recursive bool
)

func initFlags() {
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.StringSliceVarP(&filenames, "filename", "f", filenames, "The files that contain the configurations to apply.")
	pflag.BoolVarP(&recursive, "recursive", "R", false, "Process the directory used in -f, --filename recursively.")
	pflag.Parse()

	if len(filenames) == 0 {
		log.Fatalf("CRDs directory or single CRDs are required")
	}

	for _, crdDir := range filenames {
		if _, err := os.Stat(crdDir); os.IsNotExist(err) {
			log.Fatalf("CRDs directory %s does not exist", filenames)
		}
	}
}

// EnsureCRDsCmd reads each YAML file in the directory, splits it into documents, and applies each CRD to the cluster.
// The parameter --crds-dir is required and should point to the directory containing the CRD manifests.
// TODO: add unit test for this command.
func EnsureCRDsCmd() {
	ctx := context.Background()

	initFlags()

	config, err := ctrl.GetConfig()
	if err != nil {
		log.Fatalf("Failed to get Kubernetes config: %v", err)
	}

	client, err := clientset.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create API extensions client: %v", err)
	}

	dirsToApply, err := walkCRDs(recursive, filenames)
	if err != nil {
		log.Fatalf("Failed to walk through CRDs: %v", err)
	}

	for _, dir := range dirsToApply {
		log.Printf("Apply CRDs from file: %s", dir)
		if err := applyCRDs(ctx, client.ApiextensionsV1().CustomResourceDefinitions(), dir); err != nil {
			log.Fatalf("Failed to apply CRDs: %v", err)
		}
	}
}

// walkCRDs walks the CRDs directory and applies each YAML file.
// TODO: add unit test  for this function.
func walkCRDs(recursive bool, crdDirs []string) ([]string, error) {
	var dirs []string
	for _, crdDir := range crdDirs {
		// We need the parent directory to check if we are in the top-level directory.
		// This is necessary for the recursive logic.
		// We can skip the errors as it has been checked in initFlags.
		parentDir, _ := os.Stat(crdDir)
		// Walk the directory recursively and apply each YAML file.
		err := filepath.Walk(crdDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			// If this is a directory, skip it.
			// filepath.Walk() is also called for directories, but we only want to apply CRDs from files.
			if info.IsDir() {
				return nil
			}
			if filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml" {
				return nil
			}
			// If not recursive we want to only apply the CRDs in the top-level directory.
			// filepath.Dir() does not add a trailing slash, thus we need to trim it in the crdDir.
			if !recursive && parentDir.IsDir() && filepath.Dir(path) != strings.TrimRight(crdDir, "/") {
				return nil
			}

			dirs = append(dirs, path)
			return nil
		})
		if err != nil {
			return []string{}, fmt.Errorf("walk the path %s: %w", crdDirs, err)
		}
	}
	return dirs, nil
}

// applyCRDs reads a YAML file, splits it into documents, and applies each CRD to the cluster.
func applyCRDs(ctx context.Context, crdClient v1.CustomResourceDefinitionInterface, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file %q: %w", filePath, err)
	}
	defer file.Close()

	// Create a decoder that reads multiple YAML documents.
	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	var crdsToApply []*apiextensionsv1.CustomResourceDefinition
	for {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := decoder.Decode(crd); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("decode YAML: %w", err)
		}
		if crd.GetObjectKind().GroupVersionKind().Kind != "CustomResourceDefinition" {
			log.Printf("Skipping non-CRD object %s", crd.GetName())
			continue
		}
		crdsToApply = append(crdsToApply, crd)
	}

	// Apply each CRD separately.
	for _, crd := range crdsToApply {
		err := wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(context.Context) (bool, error) {
			if err := applyCRD(ctx, crdClient, crd); err != nil {
				log.Printf("Failed to apply CRD %s: %v", crd.Name, err)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("apply CRD %s: %w", crd.Name, err)
		}
	}
	return nil
}

// applyCRD creates or updates the CRD.
func applyCRD(
	ctx context.Context,
	crdClient v1.CustomResourceDefinitionInterface,
	crd *apiextensionsv1.CustomResourceDefinition,
) error {
	// Check if CRD already exists in cluster and create if not found.
	curCRD, err := crdClient.Get(ctx, crd.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		log.Printf("Create CRD %s", crd.Name)
		_, err = crdClient.Create(ctx, crd, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create CRD %s: %w", crd.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get CRD %s: %w", crd.Name, err)
	}

	log.Printf("Update CRD %s", crd.Name)
	// Set resource version to update an existing CRD.
	crd.SetResourceVersion(curCRD.GetResourceVersion())
	_, err = crdClient.Update(ctx, crd, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update CRD %s: %w", crd.Name, err)
	}

	return nil
}
