//go:build !ignore_autogenerated
// +build !ignore_autogenerated

/*
Copyright The Kubernetes Authors.

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

// Code generated by deepcopy-gen. DO NOT EDIT.

package config

import (
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *DeprecatedControllerConfiguration) DeepCopyInto(out *DeprecatedControllerConfiguration) {
	*out = *in
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new DeprecatedControllerConfiguration.
func (in *DeprecatedControllerConfiguration) DeepCopy() *DeprecatedControllerConfiguration {
	if in == nil {
		return nil
	}
	out := new(DeprecatedControllerConfiguration)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *KubeControllerManagerConfiguration) DeepCopyInto(out *KubeControllerManagerConfiguration) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.Generic.DeepCopyInto(&out.Generic)
	out.KubeCloudShared = in.KubeCloudShared
	out.AttachDetachController = in.AttachDetachController
	out.CSRSigningController = in.CSRSigningController
	out.DaemonSetController = in.DaemonSetController
	out.DeploymentController = in.DeploymentController
	out.StatefulSetController = in.StatefulSetController
	out.DeprecatedController = in.DeprecatedController
	out.EndpointController = in.EndpointController
	out.EndpointSliceController = in.EndpointSliceController
	out.EndpointSliceMirroringController = in.EndpointSliceMirroringController
	out.EphemeralVolumeController = in.EphemeralVolumeController
	in.GarbageCollectorController.DeepCopyInto(&out.GarbageCollectorController)
	out.HPAController = in.HPAController
	out.JobController = in.JobController
	out.CronJobController = in.CronJobController
	out.NamespaceController = in.NamespaceController
	out.NodeIPAMController = in.NodeIPAMController
	out.NodeLifecycleController = in.NodeLifecycleController
	in.PersistentVolumeBinderController.DeepCopyInto(&out.PersistentVolumeBinderController)
	out.PodGCController = in.PodGCController
	out.ReplicaSetController = in.ReplicaSetController
	out.ReplicationController = in.ReplicationController
	out.ResourceQuotaController = in.ResourceQuotaController
	out.SAController = in.SAController
	out.ServiceController = in.ServiceController
	out.TTLAfterFinishedController = in.TTLAfterFinishedController
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new KubeControllerManagerConfiguration.
func (in *KubeControllerManagerConfiguration) DeepCopy() *KubeControllerManagerConfiguration {
	if in == nil {
		return nil
	}
	out := new(KubeControllerManagerConfiguration)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *KubeControllerManagerConfiguration) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
