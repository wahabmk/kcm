// Copyright 2024
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scheme

import (
	helmcontrollerv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	addoncontrollerv1beta1 "github.com/projectsveltos/addon-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	clusterapiv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	kcmv1 "github.com/K0rdent/kcm/api/v1beta1"
)

var (
	Scheme = runtime.NewScheme()

	builder = runtime.SchemeBuilder{
		corev1.AddToScheme,
		clientgoscheme.AddToScheme,
		clusterapiv1.AddToScheme,
		kcmv1.AddToScheme,
		sourcev1.AddToScheme,
		helmcontrollerv2.AddToScheme,
		addoncontrollerv1beta1.AddToScheme,
		kubevirtv1.AddToScheme,
		cdiv1.AddToScheme,
	}
)

func init() {
	utilruntime.Must(builder.AddToScheme(Scheme))
}
