// Copyright 2018 The ksonnet authors
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package actions

import (
	"strings"

	"github.com/ksonnet/ksonnet/pkg/app"
	"github.com/ksonnet/ksonnet/pkg/component"
	"github.com/ksonnet/ksonnet/pkg/env"
	"github.com/pkg/errors"
)

type getModuleFn func(ksApp app.App, moduleName string) (component.Module, error)
type deleteEnvFn func(ksApp app.App, envName, componentName, paramName string) error
type deleteEnvGlobalFn func(a app.App, envName, paramName string) error

// RunParamDelete runs `param set`
func RunParamDelete(m map[string]interface{}) error {
	pd, err := NewParamDelete(m)
	if err != nil {
		return err
	}

	return pd.Run()
}

// ParamDelete sets a parameter for a component.
type ParamDelete struct {
	app     app.App
	name    string
	rawPath string
	global  bool
	envName string

	deleteEnvFn       deleteEnvFn
	deleteEnvGlobalFn deleteEnvGlobalFn
	getModuleFn       getModuleFn
	resolvePathFn     func(a app.App, path string) (component.Module, component.Component, error)
}

// NewParamDelete creates an instance of ParamDelete.
func NewParamDelete(m map[string]interface{}) (*ParamDelete, error) {
	ol := newOptionLoader(m)

	pd := &ParamDelete{
		app:     ol.LoadApp(),
		name:    ol.LoadOptionalString(OptionName),
		rawPath: ol.LoadString(OptionPath),
		global:  ol.LoadOptionalBool(OptionGlobal),
		envName: ol.LoadOptionalString(OptionEnvName),

		deleteEnvFn:       env.DeleteParam,
		deleteEnvGlobalFn: env.UnsetGlobalParams,
		resolvePathFn:     component.ResolvePath,
		getModuleFn:       component.GetModule,
	}

	if ol.err != nil {
		return nil, ol.err
	}

	if pd.envName != "" && pd.global {
		return nil, errors.New("unable to delete global param for environments")
	}

	return pd, nil
}

// Run runs the action.
func (pd *ParamDelete) Run() error {
	if pd.envName != "" {
		if pd.name != "" {
			return pd.deleteEnvFn(pd.app, pd.envName, pd.name, pd.rawPath)
		}
		return pd.deleteEnvGlobalFn(pd.app, pd.envName, pd.rawPath)
	}

	path := strings.Split(pd.rawPath, ".")

	if pd.global {
		return pd.deleteGlobal(path)
	}

	return pd.deleteLocal(path)
}

func (pd *ParamDelete) deleteGlobal(path []string) error {
	module, err := pd.getModuleFn(pd.app, pd.name)
	if err != nil {
		return errors.Wrap(err, "retrieve module")
	}

	if err := module.DeleteParam(path); err != nil {
		return errors.Wrap(err, "delete global param")
	}

	return nil
}

func (pd *ParamDelete) deleteLocal(path []string) error {
	_, c, err := pd.resolvePathFn(pd.app, pd.name)
	if err != nil {
		return errors.Wrap(err, "could not find component")
	}

	if c == nil {
		return errors.New("invalid component or param key")
	}

	if err := c.DeleteParam(path); err != nil {
		return errors.Wrap(err, "delete param")
	}

	return nil
}
