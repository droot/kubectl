/*
Copyright 2018 The Kubernetes Authors.

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

package app

import (
	"fmt"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/golang/glog"

	manifest "k8s.io/kubectl/pkg/apis/manifest/v1alpha1"
	"k8s.io/kubectl/pkg/kinflate/constants"
	interror "k8s.io/kubectl/pkg/kinflate/internal/error"
	"k8s.io/kubectl/pkg/kinflate/resource"
	"k8s.io/kubectl/pkg/kinflate/transformers"
	"k8s.io/kubectl/pkg/kinflate/types"
	"k8s.io/kubectl/pkg/loader"
)

type Application interface {
	// Resources computes and returns the resources for the app.
	Resources() (resource.ResourceCollection, error)
	// RawResources computes and returns the raw resources from the manifest.
	// It contains resources from 1) untransformed resources from current manifest 2) transformed resources from sub packages
	RawResources() (resource.ResourceCollection, error)

	// Vars returns all the variables defined by the App.
	Vars() ([]manifest.Var, error)
}

var _ Application = &applicationImpl{}

// Private implementation of the Application interface
type applicationImpl struct {
	manifest *manifest.Manifest
	loader   loader.Loader
}

// NewApp parses the manifest at the path using the loader.
func New(loader loader.Loader) (Application, error) {
	// load the manifest using the loader
	manifestBytes, err := loader.Load(constants.KubeManifestFileName)
	if err != nil {
		return nil, err
	}

	var m manifest.Manifest
	err = yaml.Unmarshal(manifestBytes, &m)
	if err != nil {
		return nil, err
	}
	return &applicationImpl{manifest: &m, loader: loader}, nil
}

// Resources computes and returns the resources from the manifest.
func (a *applicationImpl) Resources() (resource.ResourceCollection, error) {
	errs := &interror.ManifestErrors{}
	raw, err := a.RawResources()
	if err != nil {
		errs.Append(err)
	}

	cms, err := resource.NewFromConfigMaps(a.loader, a.manifest.Configmaps)
	if err != nil {
		errs.Append(err)
	}
	secrets, err := resource.NewFromSecretGenerators(a.loader.Root(), a.manifest.SecretGenerators)
	if err != nil {
		errs.Append(err)
	}
	res, err := resource.Merge(cms, secrets)
	if err != nil {
		return nil, err
	}
	// Only append hash for generated configmaps and secrets.
	nht := transformers.NewNameHashTransformer()
	err = nht.Transform(res)
	if err != nil {
		return nil, err
	}

	patches, err := resource.NewFromPaths(a.loader, a.manifest.Patches)
	if err != nil {
		errs.Append(err)
	}

	if len(errs.Get()) > 0 {
		return nil, errs
	}

	// Reindex the Raw Resources (resources from sub package and resources field of this package).
	// raw is a ResourceCollection (map) from <GVK, original name> to object with the transformed name.
	// transRaw is a ResourceCollection (map) from <GVK, transformed name> to object with the transformed name.
	transRaw := reindexResourceCollection(raw)
	// allRes contains the resources that are indexed by the original names (old names).
	// allTransRes contains the resources that are indexed by the transformed names (new names).
	// allRes and allTransRes point to the same set of objects with new names.
	allTransRes, err := resource.Merge(res, transRaw)
	if err != nil {
		return nil, err
	}
	allRes, err := resource.Merge(res, raw)
	if err != nil {
		return nil, err
	}

	ot, err := transformers.NewOverlayTransformer(patches)
	if err != nil {
		return nil, err
	}
	// Overlay transformer uses the ResourceCollection indexed by the original names.
	err = ot.Transform(allRes)
	if err != nil {
		return nil, err
	}

	t, err := a.getTransformer(patches)
	if err != nil {
		return nil, err
	}
	err = t.Transform(allTransRes)
	if err != nil {
		return nil, err
	}

	refvars, err := a.resolveRefVars(allRes)
	if err != nil {
		return nil, err
	}

	glog.Infof("found all the refvars: %+v", refvars)

	varExpander, err := transformers.NewRefVarTransformer(refvars)
	if err != nil {
		return nil, err
	}
	err = varExpander.Transform(allRes)
	if err != nil {
		return nil, err
	}

	return allRes, nil
}

// RawResources computes and returns the raw resources from the manifest.
func (a *applicationImpl) RawResources() (resource.ResourceCollection, error) {
	subAppResources, errs := a.subAppResources()
	resources, err := resource.NewFromPaths(a.loader, a.manifest.Resources)
	if err != nil {
		errs.Append(err)
	}

	if len(errs.Get()) > 0 {
		return nil, errs
	}

	return resource.Merge(resources, subAppResources)
}

func (a *applicationImpl) subApps() ([]Application, error) {
	var apps []Application
	errs := &interror.ManifestErrors{}
	for _, pkgPath := range a.manifest.Packages {
		subloader, err := a.loader.New(pkgPath)
		if err != nil {
			errs.Append(err)
			continue
		}
		subapp, err := New(subloader)
		if err != nil {
			errs.Append(err)
			continue
		}
		apps = append(apps, subapp)
	}
	if len(errs.Get()) > 0 {
		return nil, errs
	}
	return apps, nil
}

func (a *applicationImpl) subAppResources() (resource.ResourceCollection, *interror.ManifestErrors) {
	sliceOfSubAppResources := []resource.ResourceCollection{}
	errs := &interror.ManifestErrors{}

	subApps, err := a.subApps()
	if err != nil {
		return nil, errs
	}

	for _, subapp := range subApps {
		// Gather all transformed resources from subpackages.
		subAppResources, err := subapp.Resources()
		if err != nil {
			errs.Append(err)
			continue
		}
		sliceOfSubAppResources = append(sliceOfSubAppResources, subAppResources)
	}
	allResources, err := resource.Merge(sliceOfSubAppResources...)
	if err != nil {
		errs.Append(err)
	}
	return allResources, errs
}

// getTransformer generates the following transformers:
// 1) name prefix
// 2) apply labels
// 3) apply annotations
// 4) update name reference
func (a *applicationImpl) getTransformer(patches resource.ResourceCollection) (transformers.Transformer, error) {
	ts := []transformers.Transformer{}

	npt, err := transformers.NewDefaultingNamePrefixTransformer(string(a.manifest.NamePrefix))
	if err != nil {
		return nil, err
	}
	ts = append(ts, npt)

	lt, err := transformers.NewDefaultingLabelsMapTransformer(a.manifest.ObjectLabels)
	if err != nil {
		return nil, err
	}
	ts = append(ts, lt)

	at, err := transformers.NewDefaultingAnnotationsMapTransformer(a.manifest.ObjectAnnotations)
	if err != nil {
		return nil, err
	}
	ts = append(ts, at)

	nrt, err := transformers.NewDefaultingNameReferenceTransformer()
	if err != nil {
		return nil, err
	}
	ts = append(ts, nrt)
	return transformers.NewMultiTransformer(ts), nil
}

// Vars returns all the variables defined at the app and subapps of the app.
func (a *applicationImpl) Vars() ([]manifest.Var, error) {
	vars := []manifest.Var{}
	errs := &interror.ManifestErrors{}

	apps, err := a.subApps()
	if err != nil {
		return nil, err
	}

	// TODO: computing vars and resources for subApps can be combined
	for _, subApp := range apps {
		subAppVars, err := subApp.Vars()
		if err != nil {
			errs.Append(err)
			continue
		}
		vars = append(vars, subAppVars...)
	}
	vars = append(vars, a.manifest.Vars...)
	if len(errs.Get()) > 0 {
		return nil, errs
	}
	return vars, nil
}

// resolveRefVars computes the values for each of the referred variables by
// looking at the transformed resources.
func (a *applicationImpl) resolveRefVars(resources resource.ResourceCollection) (map[string]string, error) {
	refvars := map[string]string{}
	vars, err := a.Vars()
	if err != nil {
		return refvars, err
	}

	for _, refvar := range vars {
		refGVKN := gvkn(refvar)
		if r, found := resources[refGVKN]; found {
			s, err := getFieldAsString(r.Data.UnstructuredContent(), strings.Split(refvar.FieldRef.FieldPath, "."))
			if err != nil {
				return nil, fmt.Errorf("failed to resolve referred var: %+v", refvar)
			}
			refvars[refvar.Name] = s
		} else {
			glog.Infof("couldn't resolve refvar: %v", refvar)
		}
	}
	return refvars, nil
}

// TODO(droot): this will be a method on the Var itself.
func gvkn(rv manifest.Var) types.GroupVersionKindName {
	return types.GroupVersionKindName{
		GVK:  rv.ObjRef.GroupVersionKind(),
		Name: rv.ObjRef.Name,
	}
}

// reindexResourceCollection returns a new instance of ResourceCollection which
// is indexed using the new name in the object.
func reindexResourceCollection(rc resource.ResourceCollection) resource.ResourceCollection {
	result := resource.ResourceCollection{}
	for gvkn, res := range rc {
		gvkn.Name = res.Data.GetName()
		result[gvkn] = res
	}
	return result
}

func getFieldAsString(m map[string]interface{}, pathToField []string) (string, error) {
	if len(pathToField) == 0 {
		return "", fmt.Errorf("field not found")
	}

	if len(pathToField) == 1 {
		if v, found := m[pathToField[0]]; found {
			if s, ok := v.(string); ok {
				return s, nil
			}
			return "", fmt.Errorf("value at fieldpath is not of string type")
		}
		return "", fmt.Errorf("field at given fieldpath does not exist")
	}

	curr, rest := pathToField[0], pathToField[1:]

	v := m[curr]
	switch typedV := v.(type) {
	case map[string]interface{}:
		return getFieldAsString(typedV, rest)
	// case []interface{}:
	// 	for i := range typedV {
	// 		item := typedV[i]
	// 		typedItem, ok := item.(map[string]interface{})
	// 		if !ok {
	// 			return fmt.Errorf("%#v is expectd to be %T", item, typedItem)
	// 		}
	// 		err := mutateField(typedItem, newPathToField, createIfNotPresent, fns...)
	// 		if err != nil {
	// 			return err
	// 		}
	// 	}
	// 	return nil
	default:
		return "", fmt.Errorf("%#v is not expected to be a primitive type", typedV)
	}
}
