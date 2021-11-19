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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	stsv1alpha1 "github.com/silicomdk/sts-operator/api/v1alpha1"
	pb "github.com/silicomdk/sts-operator/grpc/tsynctl"
	grpc "google.golang.org/grpc"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
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
	*stsv1alpha1.StsConfig
	NodeName       string
	EnableGPS      bool
	ServicePrefix  string
	SlavePortMask  int
	MasterPortMask int
	SyncePortMask  int
	ProfileId      int
	GpsPort        int
	TsyncPort      int
}

type GPSStatusRsp struct {
	Tpvs []TPV `json:"tpv"`
}

type TPV struct {
	Time string  `json:"time"`
	Lat  float32 `json:"lat"`
	Lon  float32 `json:"lon"`
}

const (
	BC             = 1
	GM             = 2
	SC             = 3
	STATUS_NORMAL  = 1
	STATUS_INIT    = 2
	STATUS_BUSY    = 3
	STATUS_INVALID = 4
)

func printMode(s int) string {
	switch s {
	case GM:
		return "T-GM.8275.1"
	case BC:
		return "T-BC-8275.1"
	case SC:
		return "T-TSC.8275.1"
	}
	return fmt.Sprintf("unknown: %d", s)
}

func printStatus(s int) string {
	switch s {
	case STATUS_NORMAL:
		return "Normal"
	case STATUS_INIT:
		return "Initializing"
	case STATUS_BUSY:
		return "Busy"
	case STATUS_INVALID:
		return "Invalid"
	}
	return fmt.Sprintf("unknown: %d", s)
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

//+kubebuilder:rbac:groups="",resources=services;nodes;configmaps;serviceaccounts;namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use
//+kubebuilder:rbac:groups=sts.silicom.com,resources=*,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=sts.silicom.com,resources=stsconfigs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=sts.silicom.com,resources=stsconfigs/finalizers,verbs=update

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

	stsConfigList := &stsv1alpha1.StsConfigList{}
	err := r.List(ctx, stsConfigList)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		reqLogger.Error(err, "Can't retrieve StsConfigList")
		return ctrl.Result{}, err
	}

	content, err := ioutil.ReadFile("/assets/sts-deployment.yaml")
	if err != nil {
		reqLogger.Error(err, "Loading sts-deployment.yaml file")
		return ctrl.Result{}, err
	}

	t, err := template.New("asset").Option("missingkey=error").Parse(string(content))
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, stsConfig := range stsConfigList.Items {

		nodeList := &v1.NodeList{}
		err := r.List(ctx, nodeList, client.MatchingLabels(stsConfig.Spec.NodeSelector))
		if err != nil {
			if errors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			reqLogger.Error(err, "Can't retreive NodeList")
			return ctrl.Result{}, err
		}

		reqLogger.Info(fmt.Sprintf("Found %d sts nodes", len(nodeList.Items)))

		statusList := stsv1alpha1.StsConfigStatus{}
		cfgTemplate := &StsConfigTemplate{}
		for _, node := range nodeList.Items {

			var buff bytes.Buffer

			reqLogger.Info(fmt.Sprintf("Creating deamonset for node: %s:%s", node.Name, stsConfig.Spec.Mode))

			cfgTemplate.EnableGPS = false
			if stsConfig.Spec.Mode == "T-GM.8275.1" {
				cfgTemplate.ProfileId = 2
				cfgTemplate.EnableGPS = true
			} else if stsConfig.Spec.Mode == "T-BC-8275.1" {
				cfgTemplate.ProfileId = 1
			} else if stsConfig.Spec.Mode == "T-TSC.8275.1" {
				cfgTemplate.ProfileId = 3
			}

			cfgTemplate.GpsPort = 2947
			if len(os.Getenv("GPS_PORT")) > 1 {
				i2, err := strconv.Atoi(os.Getenv("GPS_PORT"))
				if err == nil {
					fmt.Println(i2)
				}
				cfgTemplate.GpsPort = i2
			}

			cfgTemplate.TsyncPort = 50051
			if len(os.Getenv("TSYNC_PORT")) > 1 {
				i2, err := strconv.Atoi(os.Getenv("TSYNC_PORT"))
				if err == nil {
					fmt.Println(i2)
				}
				cfgTemplate.TsyncPort = i2
			}

			cfgTemplate.NodeName = node.Name
			cfgTemplate.StsConfig = &stsConfig
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
					reqLogger.Info("Object created")
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

			nodeStatus := stsv1alpha1.STSNodeStatus{
				Name: node.Name,
				TsyncStatus: stsv1alpha1.TsyncStatus{
					Status: "unknown",
					Mode:   stsConfig.Spec.Mode,
				},
				GpsStatus: stsv1alpha1.GPSStatus{
					Time: "unknown",
					Lon:  0,
					Lat:  0,
				},
			}
			statusList.NodeStatus = append(statusList.NodeStatus, nodeStatus)

			go r.query_tsyncd(fmt.Sprintf("%s-tsyncd:%d", cfgTemplate.ServicePrefix, cfgTemplate.TsyncPort))

			if cfgTemplate.EnableGPS {
				go r.query_gpsd(fmt.Sprintf("%s-gpsd:%d", cfgTemplate.ServicePrefix, cfgTemplate.GpsPort))
			}
		}

		statusList.DeepCopyInto(&stsConfig.Status)
		if err := r.Status().Update(ctx, &stsConfig); err != nil {
			reqLogger.Error(err, "Update failed: stsConfig")
			return ctrl.Result{}, err
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

func (r *StsConfigReconciler) query_tsyncd(svc_str string) {
	time.Sleep(30 * time.Second)

	conn, err := grpc.Dial(svc_str, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		fmt.Println(fmt.Sprintf("Could not connect: %v", err))
	}
	defer conn.Close()

	fmt.Println(fmt.Sprintf("Connected to: %s", svc_str))
	c := pb.NewTsynctlGrpcClient(conn)

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		r, err := c.GetStatus(ctx, &pb.Empty{})
		if err != nil {
			fmt.Println(fmt.Sprintf("could not get status: %v", err))
		}
		fmt.Println(fmt.Sprintf("Status: %s", r.Message))

		r2, err := c.GetMode(ctx, &pb.Empty{})
		if err != nil {
			fmt.Println(fmt.Sprintf("could not get mode: %v", err))
		}

		fmt.Println(fmt.Sprintf("Mode: %s", r2.Message))

		r3, err := c.GetTime(ctx, &pb.Empty{})
		if err != nil {
			fmt.Println(fmt.Sprintf("could not get time: %v", err))
		}
		fmt.Println(fmt.Sprintf("Time: %s", r3.Message))

		cancel()

		time.Sleep(30 * time.Second)
	}
}

func (r *StsConfigReconciler) query_gpsd(svc_str string) {
	var conn net.Conn
	var err error

	for {
		conn, err = net.Dial("tcp", svc_str)
		if err != nil {
			fmt.Println(fmt.Sprintf("Dial failed: %s: %v", svc_str, err))
		} else {
			break
		}
		time.Sleep(5 * time.Second)
	}

	fmt.Println(fmt.Sprintf("Connected to: %s", svc_str))

	for {
		fmt.Fprintf(conn, "?POLL;")
		rsp, _ := bufio.NewReader(conn).ReadString('\n')

		if len(rsp) < 1 {
			fmt.Printf("Bad GPS Read: %s\n", rsp)
		} else {
			var status GPSStatusRsp
			err := json.Unmarshal([]byte(rsp), &status)
			if err != nil {
				fmt.Println("Error occured during unmarshaling.")
			}
			fmt.Printf("Status Struct: %#v\n", status.Tpvs)
		}
		time.Sleep(30 * time.Second)
	}
}
