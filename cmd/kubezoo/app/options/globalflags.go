/*
Copyright 2022 The KubeZoo Authors.

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

package options

import (
	"github.com/spf13/pflag"
	"k8s.io/component-base/cli/globalflag"

	// ensure libs have a chance to globally register their flags
	_ "k8s.io/apiserver/pkg/admission"
)

// AddCustomGlobalFlags explicitly registers flags that internal packages register
// against the global flagsets from "flag". We do this in order to prevent
// unwanted flags from leaking into the kube-apiserver's flagset.
func AddCustomGlobalFlags(fs *pflag.FlagSet) {
	// Lookup flags in global flag set and re-register the values with our flagset.

	// The in-tree GCE cloud provider used to register cloud-provider-gce-*-src-cidrs
	// here. It was removed from kube-apiserver upstream, along with the package that
	// registered the flags, so there is nothing left to re-register.

	// Adds flags from k8s.io/apiserver/pkg/admission.
	globalflag.Register(fs, "default-not-ready-toleration-seconds")
	globalflag.Register(fs, "default-unreachable-toleration-seconds")
}
