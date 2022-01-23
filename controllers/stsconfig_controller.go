/*
Copyright 2021.

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

package controllers

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io/ioutil"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	stsv1alpha1 "github.com/silicomdk/sts-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// StsConfigReconciler reconciles a StsConfig object
type StsConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

type StsConfigTemplate struct {
	StsConfig         *stsv1alpha1.StsConfig
	StsOperatorConfig *stsv1alpha1.StsOperatorConfig
	NodeName          string
	EnableGPS         bool
	ServicePrefix     string
	SlavePortMask     int
	MasterPortMask    int
	SyncePortMask     int
	ProfileId         int
}

func (r *StsConfigReconciler) interfacesToBitmask(cfg *StsConfigTemplate, interfaces []stsv1alpha1.StsInterfaceSpec) {

	cfg.SlavePortMask = 0
	cfg.MasterPortMask = 0
	cfg.SyncePortMask = 0

	for _, x := range interfaces {
		if x.SyncE == 1 {
			cfg.SyncePortMask |= (1 << x.EthPort)
		}

		if x.Mode == "Master" {
			cfg.MasterPortMask |= (1 << x.EthPort)
		} else if x.Mode == "Slave" {
			cfg.MasterPortMask |= (1 << x.EthPort)
		}
	}
}

//
// Even though namespaces are mentioned here, OLM will overwrite them anyways, but we will still have a Role, not ClusterRole.
//

//+kubebuilder:rbac:groups="",resources=services;nodes;configmaps;serviceaccounts;namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings;clusterroles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=daemonsets;deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use
//+kubebuilder:rbac:groups=sts.silicom.com,resources=*,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=sts.silicom.com,resources=stsconfigs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=sts.silicom.com,resources=stsconfigs/finalizers,verbs=update
//+kubebuilder:rbac:groups=sts.silicom.com,resources=stsnodes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=sts.silicom.com,resources=stsconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=nfd.openshift.io,resources=nodefeaturediscoveries,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=sro.openshift.io,resources=specialresources,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the StsConfig object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.9.2/pkg/reconcile
func (r *StsConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	var objects []client.Object
	reqLogger := r.Log.WithValues("Request.Namespace", req.Namespace, "Request.Name", req.Name)
	reqLogger.Info("Reconciling StsConfig")

	// Fetch the StsOperatorConfig instance
	operatorCfgList := &stsv1alpha1.StsOperatorConfigList{}

	opts := (&client.ListOptions{}).ApplyOptions([]client.ListOption{client.InNamespace(req.NamespacedName.Namespace)})
	err := r.List(ctx, operatorCfgList, opts)
	if err != nil {
		reqLogger.Error(err, "Failed to get operator config")
		return ctrl.Result{}, err
	}

	if len(operatorCfgList.Items) == 0 {
		reqLogger.Info("No Operator CR found in this namespace")
		return ctrl.Result{}, nil
	}

	operatorCfg := &operatorCfgList.Items[0]

	stsConfig := &stsv1alpha1.StsConfig{}
	if err := r.Get(context.TODO(), client.ObjectKey{
		Namespace: req.NamespacedName.Namespace,
		Name:      req.NamespacedName.Name,
	}, stsConfig); err != nil {
		return ctrl.Result{}, err
	}

	funcMap := template.FuncMap{
		"inc": func(i int) int {
			return i + 1
		},
	}

	content, err := ioutil.ReadFile("/assets/sts-deployment.yaml")
	if err != nil {
		reqLogger.Error(err, "Loading sts-deployment.yaml file")
		return ctrl.Result{}, err
	}

	t, err := template.New("asset").Funcs(funcMap).Option("missingkey=error").Parse(string(content))
	if err != nil {
		return ctrl.Result{}, err
	}

	nodeList := &v1.NodeList{}
	err = r.List(ctx, nodeList, client.MatchingLabels(stsConfig.Spec.NodeSelector))
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		reqLogger.Error(err, "Can't retreive NodeList")
		return ctrl.Result{}, err
	}

	reqLogger.Info(fmt.Sprintf("Found %d sts nodes", len(nodeList.Items)))

	ownerRefs := []metav1.OwnerReference{{
		Kind:       stsConfig.Kind,
		APIVersion: stsConfig.APIVersion,
		Name:       stsConfig.Name,
		UID:        stsConfig.UID,
	}, {
		Kind:       operatorCfg.Kind,
		APIVersion: operatorCfg.APIVersion,
		Name:       operatorCfg.Name,
		UID:        operatorCfg.UID,
	}}

	cfgTemplate := &StsConfigTemplate{}
	for _, node := range nodeList.Items {

		var buff bytes.Buffer

		reqLogger.Info(fmt.Sprintf("Creating/Updating deamonset for node: %s:%s", node.Name, stsConfig.Spec.Mode))

		cfgTemplate.EnableGPS = false
		if stsConfig.Spec.Mode == "T-GM.8275.1" {
			cfgTemplate.ProfileId = 2
			cfgTemplate.EnableGPS = true
		} else if stsConfig.Spec.Mode == "T-BC-8275.1" {
			cfgTemplate.ProfileId = 1
		} else if stsConfig.Spec.Mode == "T-TSC.8275.1" {
			cfgTemplate.ProfileId = 3
		}

		cfgTemplate.StsConfig = stsConfig
		cfgTemplate.StsOperatorConfig = operatorCfg
		cfgTemplate.NodeName = node.Name
		cfgTemplate.ServicePrefix = node.Name

		r.interfacesToBitmask(cfgTemplate, stsConfig.Spec.Interfaces)

		err = t.Execute(&buff, cfgTemplate)
		if err != nil {
			reqLogger.Error(err, "Template execute failure")
			return ctrl.Result{}, err
		}

		rx := regexp.MustCompile("\n-{3}")
		objectsDefs := rx.Split(buff.String(), -1)

		for _, objectDef := range objectsDefs {
			obj := unstructured.Unstructured{}
			r := strings.NewReader(objectDef)
			decoder := yaml.NewYAMLOrJSONDecoder(r, 4096)
			err := decoder.Decode(&obj)
			if err != nil {
				reqLogger.Error(err, "Decoding YAML failure")
				return ctrl.Result{}, err
			}

			obj.SetOwnerReferences(ownerRefs)
			objects = append(objects, &obj)
		}

		for _, obj := range objects {
			gvk := obj.GetObjectKind().GroupVersionKind()
			old := &unstructured.Unstructured{}
			old.SetGroupVersionKind(gvk)
			key := client.ObjectKeyFromObject(obj)
			if err := r.Get(ctx, key, old); err != nil {
				if err := r.Create(ctx, obj); err != nil {
					reqLogger.Error(err, "Create failed", "key", key, "GroupVersionKind", gvk)
					return ctrl.Result{}, err
				}
				reqLogger.Info("Object Created", "key", key, "GroupVersionKind", gvk)
			} else {
				if !equality.Semantic.DeepDerivative(obj, old) {
					obj.SetResourceVersion(old.GetResourceVersion())
					if err := r.Update(ctx, obj); err != nil {
						reqLogger.Error(err, "Update failed", "key", key, "GroupVersionKind", gvk)
						return ctrl.Result{}, err
					}
					reqLogger.Info("Object updated", "key", key, "GroupVersionKind", gvk)
				} else {
					reqLogger.Info("Object has not changed", "key", key, "GroupVersionKind", gvk)
				}
			}
		}
	}

	return ctrl.Result{}, nil
}

// syncStsConfig synchronizes StsConfig CR
func (r *StsConfigReconciler) syncStsConfig(ctx context.Context, StsConfigList *stsv1alpha1.StsConfigList, nodeList *v1.NodeList) error {
	reqLogger := r.Log.WithValues("Request.Namespace--->")
	reqLogger.Info("---->Syncing: stsConfig")

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StsConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctrl.NewControllerManagedBy(mgr). // Create the Controller
						For(&stsv1alpha1.StsConfig{}). // StsConfig is the Application API
						Owns(&appsv1.DaemonSet{}).     // StsConfig owns Daemonsets created by it
						Complete(r)
	return nil
}
