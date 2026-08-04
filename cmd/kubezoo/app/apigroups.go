package app

import (
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsapiv1 "k8s.io/api/apps/v1"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	appsv1beta2 "k8s.io/api/apps/v1beta2"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchapiv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	coreapiv1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	discoveryv1beta1 "k8s.io/api/discovery/v1beta1"
	eventsv1 "k8s.io/api/events/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	rbacv1 "k8s.io/api/rbac/v1"
	resourcev1 "k8s.io/api/resource/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/kubernetes/pkg/apis/admissionregistration"
	"k8s.io/kubernetes/pkg/apis/apps"
	"k8s.io/kubernetes/pkg/apis/authentication"
	"k8s.io/kubernetes/pkg/apis/authorization"
	"k8s.io/kubernetes/pkg/apis/autoscaling"
	"k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/pkg/apis/coordination"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/discovery"
	"k8s.io/kubernetes/pkg/apis/networking"
	"k8s.io/kubernetes/pkg/apis/node"
	"k8s.io/kubernetes/pkg/apis/policy"
	"k8s.io/kubernetes/pkg/apis/rbac"
	resourceapi "k8s.io/kubernetes/pkg/apis/resource"
	"k8s.io/kubernetes/pkg/apis/storage"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
)

var legacyGroup = apiconfig.APIGroupConfig{
	Group: coreapiv1.GroupName,
	StorageConfigs: map[string]map[string]*apiconfig.StorageConfig{
		"v1": {
			"pods": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Pod"),
				Resource:        "pods",
				ShortNames:      []string{"po"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Pod{}
				},
				NewListFunc: func() runtime.Object {
					return &core.PodList{}
				},
			},
			// attach and portforward are connections, not objects. Registered as
			// ordinary object proxies they returned an empty body where the
			// apiserver's upgrade handler expects the Pod, and both failed with
			// "the object provided is unrecognized (must be of type Pod) ...
			// unexpected end of JSON input" -- measured, while the same commands
			// worked against upstream. exec and log were already connecters,
			// which is why only these two were broken.
			"pods/attach": {
				IsConnecter: true,
			},
			"pods/status": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Pod"),
				Resource:        "pods",
				Subresource:     "status",
				ShortNames:      []string{"po"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Pod{}
				},
			},
			// In-place pod resize, added upstream after the fork point.
			"pods/resize": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Pod"),
				Resource:        "pods",
				Subresource:     "resize",
				ShortNames:      []string{"po"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Pod{}
				},
			},
			"pods/log": {
				IsConnecter: true,
			},
			"pods/exec": {
				IsConnecter: true,
			},
			// The body of an eviction is a policy/v1 Eviction, not the Pod it
			// names. Decoding it as a Pod fails outright, which took graceful
			// eviction -- and so PodDisruptionBudgets -- away from tenants.
			"pods/eviction": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Pod"),
				Resource:        "pods",
				Subresource:     "eviction",
				ShortNames:      []string{"po"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &policy.Eviction{}
				},
				GroupVersionKindFunc: func(schema.GroupVersion) schema.GroupVersionKind {
					return policyv1.SchemeGroupVersion.WithKind("Eviction")
				},
			},
			"pods/portforward": {
				IsConnecter: true,
			},
			"pods/proxy": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Pod"),
				Resource:        "pods",
				Subresource:     "proxy",
				ShortNames:      []string{"po"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Pod{}
				},
			},
			// Likewise a binding's body is a Binding.
			"pods/binding": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Pod"),
				Resource:        "pods",
				Subresource:     "binding",
				ShortNames:      []string{"po"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Binding{}
				},
				GroupVersionKindFunc: func(schema.GroupVersion) schema.GroupVersionKind {
					return coreapiv1.SchemeGroupVersion.WithKind("Binding")
				},
			},
			"pods/ephemeralcontainers": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Pod"),
				Resource:        "pods",
				Subresource:     "ephemeralcontainers",
				ShortNames:      []string{"po"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Pod{}
				},
			},
			"bindings": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Binding"),
				Resource:        "bindings",
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Binding{}
				},
			},
			"podtemplates": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("PodTemplate"),
				Resource:        "podtemplates",
				ShortNames:      []string{},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.PodTemplate{}
				},
				NewListFunc: func() runtime.Object {
					return &core.PodTemplateList{}
				},
			},

			"replicationcontrollers": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("ReplicationController"),
				Resource:        "replicationcontrollers",
				ShortNames:      []string{"rc"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.ReplicationController{}
				},
				NewListFunc: func() runtime.Object {
					return &core.ReplicationControllerList{}
				},
			},
			"replicationcontrollers/status": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("ReplicationController"),
				Resource:        "replicationcontrollers",
				Subresource:     "status",
				ShortNames:      []string{"rc"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.ReplicationController{}
				},
			},
			"replicationcontrollers/scale": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("ReplicationController"),
				Resource:        "replicationcontrollers",
				Subresource:     "scale",
				ShortNames:      []string{"rc"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &autoscaling.Scale{}
				},
				GroupVersionKindFunc: groupVersionKindForScale,
			},

			"services": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Service"),
				Resource:        "services",
				ShortNames:      []string{"svc"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Service{}
				},
				NewListFunc: func() runtime.Object {
					return &core.ServiceList{}
				},
			},
			"services/proxy": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Service"),
				Resource:        "services",
				Subresource:     "proxy",
				ShortNames:      []string{"svc"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Service{}
				},
			},
			"services/status": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Service"),
				Resource:        "services",
				Subresource:     "status",
				ShortNames:      []string{"svc"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Service{}
				},
			},

			"endpoints": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Endpoints"),
				Resource:        "endpoints",
				ShortNames:      []string{"ep"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Endpoints{}
				},
				NewListFunc: func() runtime.Object {
					return &core.EndpointsList{}
				},
			},

			"nodes": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Node"),
				Resource:        "nodes",
				ShortNames:      []string{"no"},
				NamespaceScoped: false,
				NewFunc: func() runtime.Object {
					return &core.Node{}
				},
				NewListFunc: func() runtime.Object {
					return &core.NodeList{}
				},
			},
			"nodes/status": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Node"),
				Resource:        "nodes",
				Subresource:     "status",
				ShortNames:      []string{"no"},
				NamespaceScoped: false,
				NewFunc: func() runtime.Object {
					return &core.Node{}
				},
			},
			"nodes/proxy": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Node"),
				Resource:        "nodes",
				Subresource:     "proxy",
				ShortNames:      []string{"no"},
				NamespaceScoped: false,
				NewFunc: func() runtime.Object {
					return &core.Node{}
				},
			},

			"events": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Event"),
				Resource:        "events",
				ShortNames:      []string{"ev"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Event{}
				},
				NewListFunc: func() runtime.Object {
					return &core.EventList{}
				},
			},

			"limitranges": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("LimitRange"),
				Resource:        "limitranges",
				ShortNames:      []string{"limits"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.LimitRange{}
				},
				NewListFunc: func() runtime.Object {
					return &core.LimitRangeList{}
				},
				TableConvertor: rest.NewDefaultTableConvertor(core.Resource("limitranges")),
			},

			"resourcequotas": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("ResourceQuota"),
				Resource:        "resourcequotas",
				ShortNames:      []string{"quota"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.ResourceQuota{}
				},
				NewListFunc: func() runtime.Object {
					return &core.ResourceQuotaList{}
				},
			},
			"resourcequotas/status": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("ResourceQuota"),
				Resource:        "resourcequotas",
				Subresource:     "status",
				ShortNames:      []string{"quota"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.ResourceQuota{}
				},
			},

			"namespaces": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Namespace"),
				Resource:        "namespaces",
				ShortNames:      []string{"ns"},
				NamespaceScoped: false,
				NewFunc: func() runtime.Object {
					return &core.Namespace{}
				},
				NewListFunc: func() runtime.Object {
					return &core.NamespaceList{}
				},
			},

			"namespaces/status": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Namespace"),
				Resource:        "namespaces",
				Subresource:     "status",
				ShortNames:      []string{"ns"},
				NamespaceScoped: false,
				NewFunc: func() runtime.Object {
					return &core.Namespace{}
				},
			},

			"namespaces/finalize": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Namespace"),
				Resource:        "namespaces",
				Subresource:     "finalize",
				ShortNames:      []string{"ns"},
				NamespaceScoped: false,
				NewFunc: func() runtime.Object {
					return &core.Namespace{}
				},
			},

			"secrets": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("Secret"),
				Resource:        "secrets",
				ShortNames:      []string{},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.Secret{}
				},
				NewListFunc: func() runtime.Object {
					return &core.SecretList{}
				},
			},
			"serviceaccounts": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("ServiceAccount"),
				Resource:        "serviceaccounts",
				ShortNames:      []string{"sa"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.ServiceAccount{}
				},
				NewListFunc: func() runtime.Object {
					return &core.ServiceAccountList{}
				},
			},

			"persistentvolumes": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("PersistentVolume"),
				Resource:        "persistentvolumes",
				ShortNames:      []string{"pv"},
				NamespaceScoped: false,
				NewFunc: func() runtime.Object {
					return &core.PersistentVolume{}
				},
				NewListFunc: func() runtime.Object {
					return &core.PersistentVolumeList{}
				},
			},
			"persistentvolumes/status": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("PersistentVolume"),
				Resource:        "persistentvolumes",
				Subresource:     "status",
				ShortNames:      []string{"pv"},
				NamespaceScoped: false,
				NewFunc: func() runtime.Object {
					return &core.PersistentVolume{}
				},
			},
			"persistentvolumeclaims": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("PersistentVolumeClaim"),
				Resource:        "persistentvolumeclaims",
				ShortNames:      []string{"pvc"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.PersistentVolumeClaim{}
				},
				NewListFunc: func() runtime.Object {
					return &core.PersistentVolumeClaimList{}
				},
			},
			// The status subresource was missing here while every other core
			// resource kubezoo exposes has one.
			"persistentvolumeclaims/status": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("PersistentVolumeClaim"),
				Resource:        "persistentvolumeclaims",
				Subresource:     "status",
				ShortNames:      []string{"pvc"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.PersistentVolumeClaim{}
				},
			},
			"configmaps": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("ConfigMap"),
				Resource:        "configmaps",
				ShortNames:      []string{"cm"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &core.ConfigMap{}
				},
				NewListFunc: func() runtime.Object {
					return &core.ConfigMapList{}
				},
			},
			// And a token request's body is an authentication.k8s.io TokenRequest.
			// Since 1.24 this subresource is the only way to obtain a service
			// account token, so decoding it as a ServiceAccount made
			// `kubectl create token` impossible for tenants.
			"serviceaccounts/token": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("ServiceAccount"),
				Resource:        "serviceaccounts",
				Subresource:     "token",
				ShortNames:      []string{"sa"},
				NamespaceScoped: true,
				NewFunc: func() runtime.Object {
					return &authentication.TokenRequest{}
				},
				GroupVersionKindFunc: func(schema.GroupVersion) schema.GroupVersionKind {
					return authenticationv1.SchemeGroupVersion.WithKind("TokenRequest")
				},
			},

			"componentstatuses": {
				Kind: coreapiv1.
					SchemeGroupVersion.WithKind("ComponentStatus"),
				Resource:        "componentstatuses",
				ShortNames:      []string{"cs"},
				NamespaceScoped: false,
				NewFunc: func() runtime.Object {
					return &core.ComponentStatus{}
				},
				NewListFunc: func() runtime.Object {
					return &core.ComponentStatusList{}
				},
			},
		},
	},
}

var nonLegacyGroups = []apiconfig.APIGroupConfig{
	{
		// group: apps
		// ref: k8s.io/kubernetes/pkg/registry/apps/rest/storage_apps.go
		appsapiv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				// deployments
				"deployments": {
					Kind:            appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
					Resource:        "deployments",
					ShortNames:      []string{"deploy"},
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &apps.Deployment{} },
					NewListFunc:     func() runtime.Object { return &apps.DeploymentList{} },
				},
				"deployments/status": {
					Kind:            appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
					Resource:        "deployments",
					Subresource:     "status",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &apps.Deployment{} },
				},
				"deployments/scale": {
					Kind:                 appsapiv1.SchemeGroupVersion.WithKind("Deployment"),
					Resource:             "deployments",
					Subresource:          "scale",
					NamespaceScoped:      true,
					NewFunc:              func() runtime.Object { return &autoscaling.Scale{} },
					GroupVersionKindFunc: groupVersionKindForScale,
				},

				// statefulsets
				"statefulsets": {
					Kind:            appsapiv1.SchemeGroupVersion.WithKind("StatefulSet"),
					Resource:        "statefulsets",
					ShortNames:      []string{"sts"},
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &apps.StatefulSet{} },
					NewListFunc:     func() runtime.Object { return &apps.StatefulSetList{} },
				},
				"statefulsets/status": {
					Kind:            appsapiv1.SchemeGroupVersion.WithKind("StatefulSet"),
					Resource:        "statefulsets",
					Subresource:     "status",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &apps.StatefulSet{} },
				},
				"statefulsets/scale": {
					Kind:                 appsapiv1.SchemeGroupVersion.WithKind("StatefulSet"),
					Resource:             "statefulsets",
					Subresource:          "scale",
					NamespaceScoped:      true,
					NewFunc:              func() runtime.Object { return &autoscaling.Scale{} },
					GroupVersionKindFunc: groupVersionKindForScale,
				},

				// daemonsets
				"daemonsets": {
					Kind:            appsapiv1.SchemeGroupVersion.WithKind("DaemonSet"),
					Resource:        "daemonsets",
					ShortNames:      []string{"ds"},
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &apps.DaemonSet{} },
					NewListFunc:     func() runtime.Object { return &apps.DaemonSetList{} },
				},
				"daemonsets/status": {
					Kind:            appsapiv1.SchemeGroupVersion.WithKind("DaemonSet"),
					Resource:        "daemonsets",
					ShortNames:      []string{"ds"},
					Subresource:     "status",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &apps.DaemonSet{} },
				},

				// replicasets
				"replicasets": {
					Kind:            appsapiv1.SchemeGroupVersion.WithKind("ReplicaSet"),
					Resource:        "replicasets",
					ShortNames:      []string{"rs"},
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &apps.ReplicaSet{} },
					NewListFunc:     func() runtime.Object { return &apps.ReplicaSetList{} },
				},
				"replicasets/status": {
					Kind:            appsapiv1.SchemeGroupVersion.WithKind("ReplicaSet"),
					Resource:        "replicasets",
					Subresource:     "status",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &apps.ReplicaSet{} },
				},
				"replicasets/scale": {
					Kind:                 appsapiv1.SchemeGroupVersion.WithKind("ReplicaSet"),
					Resource:             "replicasets",
					Subresource:          "scale",
					NamespaceScoped:      true,
					NewFunc:              func() runtime.Object { return &autoscaling.Scale{} },
					GroupVersionKindFunc: groupVersionKindForScale,
				},

				// controllerrevisions
				"controllerrevisions": {
					Kind:            appsapiv1.SchemeGroupVersion.WithKind("ControllerRevision"),
					Resource:        "controllerrevisions",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &apps.ControllerRevision{} },
					NewListFunc:     func() runtime.Object { return &apps.ControllerRevisionList{} },
				},
			},
		},
	},

	{
		authenticationv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"tokenreviews": {
					Kind:            authenticationv1.SchemeGroupVersion.WithKind("TokenReview"),
					Resource:        "tokenreviews",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &authentication.TokenReview{} },
				},
			},
		},
	},

	{
		batchapiv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"jobs": {
					Kind:            batchapiv1.SchemeGroupVersion.WithKind("Job"),
					Resource:        "jobs",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &batch.Job{} },
					NewListFunc:     func() runtime.Object { return &batch.JobList{} },
				},
				"jobs/status": {
					Kind:            batchapiv1.SchemeGroupVersion.WithKind("Job"),
					Resource:        "jobs",
					Subresource:     "status",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &batch.Job{} },
				},
				"cronjobs": {
					Kind:            batchapiv1.SchemeGroupVersion.WithKind("CronJob"),
					Resource:        "cronjobs",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &batch.CronJob{} },
					NewListFunc:     func() runtime.Object { return &batch.CronJobList{} },
				},
				"cronjobs/status": {
					Kind:            batchapiv1.SchemeGroupVersion.WithKind("CronJob"),
					Resource:        "cronjobs",
					Subresource:     "status",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &batch.CronJob{} },
				},
			},
		},
	},

	{
		admissionregistrationv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"validatingwebhookconfigurations": {
					Kind:            admissionregistrationv1.SchemeGroupVersion.WithKind("ValidatingWebhookConfiguration"),
					Resource:        "validatingwebhookconfigurations",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &admissionregistration.ValidatingWebhookConfiguration{} },
					NewListFunc:     func() runtime.Object { return &admissionregistration.ValidatingWebhookConfigurationList{} },
				},
				"mutatingwebhookconfigurations": {
					Kind:            admissionregistrationv1.SchemeGroupVersion.WithKind("MutatingWebhookConfiguration"),
					Resource:        "mutatingwebhookconfigurations",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &admissionregistration.MutatingWebhookConfiguration{} },
					NewListFunc:     func() runtime.Object { return &admissionregistration.MutatingWebhookConfigurationList{} },
				},
			},
		},
	},

	{
		eventsv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"events": {
					Kind:            eventsv1.SchemeGroupVersion.WithKind("Event"),
					Resource:        "events",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &eventsv1.Event{} },
					NewListFunc:     func() runtime.Object { return &eventsv1.EventList{} },
				},
			},
		},
	},

	{
		rbacv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"roles": {
					Kind:            rbacv1.SchemeGroupVersion.WithKind("Role"),
					Resource:        "roles",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &rbac.Role{} },
					NewListFunc:     func() runtime.Object { return &rbac.RoleList{} },
					TableConvertor:  rest.NewDefaultTableConvertor(rbac.Resource("roles")),
				},
				"rolebindings": {
					Kind:            rbacv1.SchemeGroupVersion.WithKind("RoleBinding"),
					Resource:        "rolebindings",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &rbac.RoleBinding{} },
					NewListFunc:     func() runtime.Object { return &rbac.RoleBindingList{} },
				},
				"clusterroles": {
					Kind:            rbacv1.SchemeGroupVersion.WithKind("ClusterRole"),
					Resource:        "clusterroles",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &rbac.ClusterRole{} },
					NewListFunc:     func() runtime.Object { return &rbac.ClusterRoleList{} },
					TableConvertor:  rest.NewDefaultTableConvertor(rbac.Resource("clusterroles")),
				},
				"clusterrolebindings": {
					Kind:            rbacv1.SchemeGroupVersion.WithKind("ClusterRoleBinding"),
					Resource:        "clusterrolebindings",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &rbac.ClusterRoleBinding{} },
					NewListFunc:     func() runtime.Object { return &rbac.ClusterRoleBindingList{} },
				},
			},
		},
	},

	{
		policyv1beta1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"poddisruptionbudgets": {
					Kind:            policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
					Resource:        "poddisruptionbudgets",
					NamespaceScoped: true,
					ShortNames:      []string{"pdb"},
					NewFunc:         func() runtime.Object { return &policy.PodDisruptionBudget{} },
					NewListFunc:     func() runtime.Object { return &policy.PodDisruptionBudgetList{} },
				},
				"poddisruptionbudgets/status": {
					Kind:            policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
					Resource:        "poddisruptionbudgets",
					Subresource:     "status",
					NamespaceScoped: true,
					ShortNames:      []string{"pdb"},
					NewFunc:         func() runtime.Object { return &policy.PodDisruptionBudget{} },
				},
			},
		},
	},

	{
		networkingv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"networkpolicies": {
					Kind:            networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
					Resource:        "networkpolicies",
					NamespaceScoped: true,
					ShortNames:      []string{"netpol"},
					NewFunc:         func() runtime.Object { return &networking.NetworkPolicy{} },
					NewListFunc:     func() runtime.Object { return &networking.NetworkPolicyList{} },
				},
				"ingresses": {
					Kind:            networkingv1.SchemeGroupVersion.WithKind("Ingress"),
					Resource:        "ingresses",
					NamespaceScoped: true,
					ShortNames:      []string{"ing"},
					NewFunc:         func() runtime.Object { return &networking.Ingress{} },
					NewListFunc:     func() runtime.Object { return &networking.IngressList{} },
				},
				"ingresses/status": {
					Kind:            networkingv1.SchemeGroupVersion.WithKind("Ingress"),
					Resource:        "ingresses",
					Subresource:     "status",
					NamespaceScoped: true,
					ShortNames:      []string{"ing"},
					NewFunc:         func() runtime.Object { return &networking.Ingress{} },
				},
				"ingressclasses": {
					Kind:            networkingv1.SchemeGroupVersion.WithKind("IngressClass"),
					Resource:        "ingressclasses",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &networking.IngressClass{} },
					NewListFunc:     func() runtime.Object { return &networking.IngressClassList{} },
				},
			},
		},
	},

	{
		discoveryv1beta1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"endpointslices": {
					Kind:            discoveryv1.SchemeGroupVersion.WithKind("EndpointSlice"),
					Resource:        "endpointslices",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &discovery.EndpointSlice{} },
					NewListFunc:     func() runtime.Object { return &discovery.EndpointSliceList{} },
				},
			},
		},
	},

	{
		coordinationv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"leases": {
					Kind:            coordinationv1.SchemeGroupVersion.WithKind("Lease"),
					Resource:        "leases",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &coordination.Lease{} },
					NewListFunc:     func() runtime.Object { return &coordination.LeaseList{} },
				},
			},
		},
	},

	{
		autoscalingv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"horizontalpodautoscalers": {
					Kind:            autoscalingv1.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
					Resource:        "horizontalpodautoscalers",
					NamespaceScoped: true,
					ShortNames:      []string{"hpa"},
					NewFunc:         func() runtime.Object { return &autoscaling.HorizontalPodAutoscaler{} },
					NewListFunc:     func() runtime.Object { return &autoscaling.HorizontalPodAutoscalerList{} },
				},
				"horizontalpodautoscalers/status": {
					Kind:            autoscalingv1.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
					Resource:        "horizontalpodautoscalers",
					Subresource:     "status",
					NamespaceScoped: true,
					ShortNames:      []string{"hpa"},
					NewFunc:         func() runtime.Object { return &autoscaling.HorizontalPodAutoscaler{} },
				},
			},
			// autoscaling/v2beta1 and v2beta2 were removed from Kubernetes in 1.26.
			// v2 is the successor and is served below.
			"v2": {
				"horizontalpodautoscalers": {
					Kind:            autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
					Resource:        "horizontalpodautoscalers",
					NamespaceScoped: true,
					ShortNames:      []string{"hpa"},
					NewFunc:         func() runtime.Object { return &autoscaling.HorizontalPodAutoscaler{} },
					NewListFunc:     func() runtime.Object { return &autoscaling.HorizontalPodAutoscalerList{} },
				},
				"horizontalpodautoscalers/status": {
					Kind:            autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
					Resource:        "horizontalpodautoscalers",
					Subresource:     "status",
					NamespaceScoped: true,
					ShortNames:      []string{"hpa"},
					NewFunc:         func() runtime.Object { return &autoscaling.HorizontalPodAutoscaler{} },
				},
			},
		},
	},

	{
		authorizationv1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"subjectaccessreviews": {
					Kind:            authorizationv1.SchemeGroupVersion.WithKind("SubjectAccessReview"),
					Resource:        "subjectaccessreviews",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &authorization.SubjectAccessReview{} },
				},
				"selfsubjectaccessreviews": {
					Kind:            authorizationv1.SchemeGroupVersion.WithKind("SelfSubjectAccessReview"),
					Resource:        "selfsubjectaccessreviews",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &authorization.SelfSubjectAccessReview{} },
				},
				"localsubjectaccessreviews": {
					Kind:            authorizationv1.SchemeGroupVersion.WithKind("LocalSubjectAccessReview"),
					Resource:        "localsubjectaccessreviews",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &authorization.LocalSubjectAccessReview{} },
				},
				"selfsubjectrulesreviews": {
					Kind:            authorizationv1.SchemeGroupVersion.WithKind("SelfSubjectRulesReview"),
					Resource:        "selfsubjectrulesreviews",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &authorization.SelfSubjectRulesReview{} },
				},
			},
		},
	},

	{
		nodev1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				"runtimeclasses": {
					Kind:            nodev1.SchemeGroupVersion.WithKind("RuntimeClass"),
					Resource:        "runtimeclasses",
					NamespaceScoped: false,
					NewFunc: func() runtime.Object {
						return &node.RuntimeClass{}
					},
					NewListFunc: func() runtime.Object {
						return &node.RuntimeClassList{}
					},
				},
			},
		},
	},

	{
		storagev1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				// ⭐ Read-only, and only the classes the platform published --
				// which it does by labelling them, so that offering one more does
				// not mean restarting the gateway and interrupting every tenant.
				// A StorageClass is the PLATFORM's object: a tenant
				// already references one successfully -- pkg/convert/pvc.go passes
				// spec.storageClassName through untranslated -- but before this it
				// had no way to discover which names exist, because this whole
				// group was unserved. pkg/convert/pv.go even refuses a volume
				// source and tells the tenant to "Use a StorageClass", naming a
				// resource it could not enumerate.
				//
				// Not the tenant proxy: no name prefixing (the tenant must see the
				// name that works in a PVC), visibility by allowlist rather than by
				// tenant prefix, and no write verbs at all. See publicclass.go for
				// why that is a separate storage rather than three conditionals.
				"storageclasses": {
					Kind:            storagev1.SchemeGroupVersion.WithKind("StorageClass"),
					Resource:        "storageclasses",
					ShortNames:      []string{"sc"},
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &storage.StorageClass{} },
					NewListFunc:     func() runtime.Object { return &storage.StorageClassList{} },
				},
				// Same storage, same reasoning, different default: nothing is
				// published unless an operator labels it, and naming an
				// unpublished one is refused rather than merely undiscoverable.
				// A VolumeAttributesClass carries the CSI driver's IOPS and
				// throughput parameters, so it is a performance tier a platform
				// sells rather than a choice a tenant makes freely.
				"volumeattributesclasses": {
					Kind:            storagev1.SchemeGroupVersion.WithKind("VolumeAttributesClass"),
					Resource:        "volumeattributesclasses",
					ShortNames:      []string{"vac"},
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &storage.VolumeAttributesClass{} },
					NewListFunc:     func() runtime.Object { return &storage.VolumeAttributesClassList{} },
				},
			},
		},
	},

	// ⭐ DRA (dynamic resource allocation). GA and locked on since 1.35, so the
	// upstream cluster serves it whether or not tenants do; what decides is
	// whether kubezoo declares it here.
	//
	// ⛔ THREE OF THE FOUR RESOURCES, and the one left out is the point.
	// ResourceSlice carries spec.nodeName, spec.nodeSelector and the device
	// inventory of each machine -- the platform's hardware, per node, for every
	// tenant to read. That is the same thing Nodes were withdrawn for: a
	// description of the infrastructure a tenant shares with everyone else, and
	// a list of what to look up when something there has a CVE. A tenant does
	// not need it: it asks for a DEVICE CLASS and the scheduler finds the
	// hardware.
	{
		resourcev1.GroupName,
		map[string]map[string]*apiconfig.StorageConfig{
			"v1": {
				// The tenant's own objects, namespaced like everything else.
				"resourceclaims": {
					Kind:            resourcev1.SchemeGroupVersion.WithKind("ResourceClaim"),
					Resource:        "resourceclaims",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &resourceapi.ResourceClaim{} },
					NewListFunc:     func() runtime.Object { return &resourceapi.ResourceClaimList{} },
				},
				"resourceclaims/status": {
					Kind:            resourcev1.SchemeGroupVersion.WithKind("ResourceClaim"),
					Resource:        "resourceclaims",
					Subresource:     "status",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &resourceapi.ResourceClaim{} },
				},
				"resourceclaimtemplates": {
					Kind:            resourcev1.SchemeGroupVersion.WithKind("ResourceClaimTemplate"),
					Resource:        "resourceclaimtemplates",
					NamespaceScoped: true,
					NewFunc:         func() runtime.Object { return &resourceapi.ResourceClaimTemplate{} },
					NewListFunc:     func() runtime.Object { return &resourceapi.ResourceClaimTemplateList{} },
				},
				// ⚠️ Same shape as StorageClass and VolumeAttributesClass: the
				// PLATFORM's object, discoverable only where it has been
				// published, and read-only. A DeviceClass is a hardware tier a
				// platform sells -- naming one is asking for that hardware -- so
				// it defaults to publishing NOTHING, like VolumeAttributesClass
				// and unlike StorageClass, which had existing behaviour to keep.
				"deviceclasses": {
					Kind:            resourcev1.SchemeGroupVersion.WithKind("DeviceClass"),
					Resource:        "deviceclasses",
					NamespaceScoped: false,
					NewFunc:         func() runtime.Object { return &resourceapi.DeviceClass{} },
					NewListFunc:     func() runtime.Object { return &resourceapi.DeviceClassList{} },
				},
			},
		},
	},

	// the following kinds should not be available to serverless kubernetes users, so the api configs are skipped.
	// group: storage.k8s.io
	// kinds: CSIDriver, CSINode, VolumeAttachment
	//
	// StorageClass and VolumeAttributesClass are served above, read-only and
	// narrowed to what the platform published.

	// group: scheduling.k8s.io
	// kinds: PriorityClass

	// group: node.k8s.io
	// kinds: RuntimeClass
}

func groupVersionKindForScale(containingGV schema.GroupVersion) schema.GroupVersionKind {
	switch containingGV {
	case extensionsv1beta1.SchemeGroupVersion:
		return extensionsv1beta1.SchemeGroupVersion.WithKind("Scale")
	case appsv1beta1.SchemeGroupVersion:
		return appsv1beta1.SchemeGroupVersion.WithKind("Scale")
	case appsv1beta2.SchemeGroupVersion:
		return appsv1beta2.SchemeGroupVersion.WithKind("Scale")
	default:
		return autoscalingv1.SchemeGroupVersion.WithKind("Scale")
	}
}

// ServedAPIGroups names the API groups kubezoo actually installs storage for.
//
// Discovery used to advertise every native group the scheme knows about, which
// is a much larger set: the scheme is Kubernetes', while the storage installed
// here is this file. A tenant therefore saw resources it could not use --
// certificatesigningrequests was advertised by `kubectl api-resources` and
// answered "Unable to list" on every request. It failed closed, so it was not a
// hole, but a resource that appears and then does not work is worse than one
// that never appears.
//
// Derived from the same values that build the storage, so the two cannot drift
// apart again.
func ServedAPIGroups() map[string]bool {
	served := map[string]bool{legacyGroup.Group: true}
	for i := range nonLegacyGroups {
		served[nonLegacyGroups[i].Group] = true
	}
	// Groups kubezoo serves through a path other than the storage configs
	// above, and which have to be listed by hand because they are installed
	// elsewhere. Leaving apiextensions out of the first version of this
	// function stopped tenants managing CRDs at all -- `kubectl get crd` came
	// back with "the server doesn't have a resource type crd" -- which is worth
	// more than a comment: this set is now the thing that decides what a tenant
	// can see, so anything added to the served surface has to be added here.
	for _, elsewhere := range []string{
		// CustomResourceDefinitions, installed by the delegated
		// apiextensions-apiserver (cmd/kubezoo/app/apiextensions.go).
		"apiextensions.k8s.io",
		// kubezoo's own APIs.
		"tenant.kubezoo.io",
		"quota.kubezoo.io",
	} {
		served[elsewhere] = true
	}
	return served
}
