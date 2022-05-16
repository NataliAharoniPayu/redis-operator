/*


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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/PayU/redis-operator/controllers/view"

	"github.com/go-logr/logr"
	"github.com/labstack/echo/v4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	dbv1 "github.com/PayU/redis-operator/api/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/PayU/redis-operator/controllers/rediscli"
	clusterData "github.com/PayU/redis-operator/data"
)

const (
	// NotExists: the RedisCluster custom resource has just been created
	NotExists RedisClusterState = "NotExists"

	// InitializingCluster: ConfigMap, Service resources are created; the leader
	// pods are created and clusterized

	Reset RedisClusterState = "Reset"

	// Ready: cluster is up & running as expected
	Ready RedisClusterState = "Ready"

	// Recovering: one ore note nodes are in fail state and are being recreated
	Recovering RedisClusterState = "Recovering"

	// Updating: the cluster is in the middle of a rolling update
	Updating RedisClusterState = "Updating"

	Scale RedisClusterState = "Scale"
)

type RedisClusterState string

type RedisClusterReconciler struct {
	client.Client
	Cache                 cache.Cache
	Log                   logr.Logger
	Scheme                *runtime.Scheme
	RedisCLI              *rediscli.RedisCLI
	Config                *OperatorConfig
	State                 RedisClusterState
	RedisClusterStateView *view.RedisClusterStateView
}

var reconciler *RedisClusterReconciler
var cluster *dbv1.RedisCluster
var ClusterReset bool = false

// +kubebuilder:rbac:groups=db.payu.com,resources=redisclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db.payu.com,resources=redisclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=*,resources=pods;services;configmaps,verbs=create;update;patch;get;list;watch;delete

func (r *RedisClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	println("Reconcile call")
	reconciler = r
	r.Status()

	var redisCluster dbv1.RedisCluster
	var err error

	if err = r.Get(context.Background(), req.NamespacedName, &redisCluster); err != nil {
		r.Log.Info("Unable to fetch RedisCluster resource")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	r.State = RedisClusterState(redisCluster.Status.ClusterState)
	if len(redisCluster.Status.ClusterState) == 0 {
		r.State = NotExists
	}

	cluster = &redisCluster

	err = r.getClusterStateView(&redisCluster)
	if r.State == NotExists {
		r.RedisClusterStateView.CreateStateView(redisCluster.Spec.LeaderCount, redisCluster.Spec.LeaderFollowersCount)
	} else if err != nil {
		r.Log.Error(err, "Could not perform reconcile loop")
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	switch r.State {
	case NotExists:
		err = r.handleInitializingCluster(&redisCluster)
		break
	case Reset:
		err = r.handleInitializingCluster(&redisCluster)
		ClusterReset = false
		break
	case Ready:
		if !ClusterReset {
			err = r.handleReadyState(&redisCluster)
		}
		break
	case Recovering:
		if !ClusterReset {
			err = r.handleRecoveringState(&redisCluster)
		}
		break
	case Updating:
		if !ClusterReset {
			err = r.handleUpdatingState(&redisCluster)
		}
		break
	case Scale:
		if !ClusterReset {
			err = r.handleScaleState(&redisCluster)
		}
	}
	if err != nil {
		r.Log.Error(err, "Handling error")
	}
	defer r.updateClusterView(&redisCluster)
	defer r.updateClusterState(&redisCluster)
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *RedisClusterReconciler) updateClusterState(redisCluster *dbv1.RedisCluster) {
	r.Status().Update(context.Background(), redisCluster)
	clusterState := redisCluster.Status.ClusterState
	r.Client.Status()
	r.Log.Info(fmt.Sprintf("Updated state to: [%s]", clusterState))
}

func (r *RedisClusterReconciler) updateClusterView(redisCluster *dbv1.RedisCluster) {
	view, err := r.NewRedisClusterView(redisCluster)
	if err != nil {
		r.Log.Info("[Warn] Could not get view for api view update, Error: %v", err.Error())
		return
	}
	data, _ := json.MarshalIndent(view, "", "")
	clusterData.SaveRedisClusterView(data)
	clusterData.SaveRedisClusterState(redisCluster.Status.ClusterState)

	err = r.updateClusterStateView(redisCluster)
	if err != nil {
		err = r.createClusterStateView(redisCluster)
		if err != nil {
			r.Log.Error(err, "Could not update cluster state view")
		}
	}
}

func (r *RedisClusterReconciler) handleInitializingCluster(redisCluster *dbv1.RedisCluster) error {
	r.updateClusterState(redisCluster)
	r.Log.Info("Clear all cluster pods...")
	e := r.deleteAllRedisClusterPods()
	if e != nil {
		return e
	}
	r.Log.Info("Clear cluster state map...")
	r.deleteClusterStateView(redisCluster)
	r.RedisClusterStateView.CreateStateView(redisCluster.Spec.LeaderCount, redisCluster.Spec.LeaderFollowersCount)
	r.Log.Info("Handling initializing leaders...")
	if err := r.createNewRedisCluster(redisCluster); err != nil {
		redisCluster.Status.ClusterState = string(Reset)
		ClusterReset = true
		return err
	}
	r.Log.Info("Handling initializing followers...")
	if err := r.initializeFollowers(redisCluster); err != nil {
		redisCluster.Status.ClusterState = string(Reset)
		ClusterReset = true
		return err
	}
	redisCluster.Status.ClusterState = string(Ready)
	r.updateClusterState(redisCluster)
	return nil
}

func (r *RedisClusterReconciler) handleReadyState(redisCluster *dbv1.RedisCluster) error {
	complete, err := r.isClusterComplete(redisCluster)
	if err != nil {
		r.Log.Info("Could not check if cluster is complete")
		return err
	}
	if !complete {
		redisCluster.Status.ClusterState = string(Recovering)
		return nil
	}
	uptodate, err := r.isClusterUpToDate(redisCluster)
	if err != nil {
		r.Log.Info("Could not check if cluster is updated")
		redisCluster.Status.ClusterState = string(Recovering)
		return err
	}
	if !uptodate {
		redisCluster.Status.ClusterState = string(Updating)
		return nil
	}
	defer r.forgetLostNodes(redisCluster)
	r.Log.Info("Cluster is healthy")
	scale, scaleType := r.isScaleRequired(redisCluster)
	if scale {
		r.Log.Info(fmt.Sprintf("Scale is required, scale type: [%v]", scaleType.String()))
		redisCluster.Status.ClusterState = string(Scale)
	}
	return nil
}

func (r *RedisClusterReconciler) handleScaleState(redisCluster *dbv1.RedisCluster) error {
	e := r.scaleCluster(redisCluster)
	if e != nil {
		r.Log.Error(e, "Could not perform cluster scale")
	}
	defer r.forgetLostNodes(redisCluster)
	redisCluster.Status.ClusterState = string(Ready)
	return nil
}

func (r *RedisClusterReconciler) handleRecoveringState(redisCluster *dbv1.RedisCluster) error {
	r.Log.Info("Handling cluster recovery...")
	e := r.recoverCluster(redisCluster)
	if e != nil {
		return e
	}
	defer r.forgetLostNodes(redisCluster)
	redisCluster.Status.ClusterState = string(Ready)
	return nil
}

func (r *RedisClusterReconciler) handleUpdatingState(redisCluster *dbv1.RedisCluster) error {
	var err error = nil
	r.Log.Info("Handling rolling update...")
	if err = r.updateCluster(redisCluster); err != nil {
		r.Log.Info("Rolling update failed")
	}
	redisCluster.Status.ClusterState = string(Recovering)
	return err
}

func (r *RedisClusterReconciler) validateStateUpdated(redisCluster *dbv1.RedisCluster) (ctrl.Result, error) {
	clusterState := RedisClusterState(redisCluster.Status.ClusterState)
	if len(redisCluster.Status.ClusterState) == 0 {
		clusterState = NotExists
	}
	if clusterState != r.State {
		err := r.Status().Update(context.Background(), redisCluster)
		if err != nil && !apierrors.IsConflict(err) {
			r.Log.Info("Failed to update state to " + string(clusterState))
			return ctrl.Result{}, err
		}
		if apierrors.IsConflict(err) {
			r.Log.Info("Conflict when updating state to " + string(clusterState))
		}

		r.Client.Status()
		r.State = clusterState
		r.Log.Info(fmt.Sprintf("Updated state to: [%s]", clusterState))
	}
	return ctrl.Result{}, nil
}

func (r *RedisClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "status.podIP", func(rawObj runtime.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{pod.Status.PodIP}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1.RedisCluster{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func DoResetCluster(c echo.Context) error {
	cluster.Status.ClusterState = string(Reset)
	ClusterReset = true
	reconciler.updateClusterState(cluster)
	return c.String(http.StatusOK, "Set cluster state to reset mode")
}

func DeleteAllPods(c echo.Context) error {
	reconciler.deleteAllRedisClusterPods()
	return c.String(http.StatusOK, "All pods deleted")
}

func AddNewLeaders(c echo.Context) error {
	v, e := reconciler.NewRedisClusterView(cluster)
	if e != nil {
		return c.String(http.StatusOK, "Could not retrieve cluster view")
	}
	healthyServerName := reconciler.findHealthyLeader(v)
	if len(healthyServerName) == 0 {
		return c.String(http.StatusOK, "Could not find healthy reachable leader to serve the add leaders request")
	}
	healthyServerIp := v.Nodes[healthyServerName].Ip
	e = reconciler.addLeaders(cluster, healthyServerIp, []string{"redis-node-8", "redis-node-9"})
	if e != nil {
		return c.String(http.StatusOK, "Could not add requested leaders properly")
	}
	return c.String(http.StatusOK, "New leader node added successfully")
}

func DeleteNewLeaders(c echo.Context) error {
	v, e := reconciler.NewRedisClusterView(cluster)
	if e != nil {
		return c.String(http.StatusOK, "Could not retrieve cluster view")
	}
	healthyServerName := reconciler.findHealthyLeader(v)
	if len(healthyServerName) == 0 {
		return c.String(http.StatusOK, "Could not find healthy server to serve the fix request")
	}
	healthyServerIp := v.Nodes[healthyServerName].Ip
	e = reconciler.deleteNodes(cluster, v, healthyServerIp)
	if e != nil {
		return c.String(http.StatusOK, "Could not delete requested leaders properly")
	}
	return c.String(http.StatusOK, "New leader node deleted successfully")
}

func ClusterRebalance(c echo.Context) error {
	v, e := reconciler.NewRedisClusterView(cluster)
	if e != nil {
		return c.String(http.StatusOK, "Could not retrieve cluster view")
	}
	healthyServerName := reconciler.findHealthyLeader(v)
	if len(healthyServerName) == 0 {
		return c.String(http.StatusOK, "Could not find healthy server to serve the rebalance request")
	}
	healthyServerIp := v.Nodes[healthyServerName].Ip
	successful, _, err := reconciler.RedisCLI.ClusterRebalance(healthyServerIp, true)
	println("Successful: " + fmt.Sprint(successful))
	if err != nil {
		println(err.Error())
	}

	return c.String(http.StatusOK, "Cluster rebalance attempt executed")
}

func ClusterFix(c echo.Context) error {
	v, e := reconciler.NewRedisClusterView(cluster)
	if e != nil {
		return c.String(http.StatusOK, "Could not retrieve cluster view")
	}
	healthyServerName := reconciler.findHealthyLeader(v)
	if len(healthyServerName) == 0 {
		return c.String(http.StatusOK, "Could not find healthy server to serve the fix request")
	}
	healthyServerIp := v.Nodes[healthyServerName].Ip
	successful, _, err := reconciler.RedisCLI.ClusterFix(healthyServerIp)
	println("Successful: " + fmt.Sprint(successful))
	if err != nil {
		println(err.Error())
	}
	return c.String(http.StatusOK, "Cluster fix attempt executed")
}

func PrintClusterStateView(c echo.Context) error {
	reconciler.RedisClusterStateView.CreateStateView(cluster.Spec.LeaderCount, cluster.Spec.LeaderFollowersCount)
	reconciler.RedisClusterStateView.ClusterState = view.ClusterOK
	reconciler.RedisClusterStateView.Nodes["redis-node-0-1"].NodeState = view.NodeOK
	fmt.Printf("%+v\n", reconciler.RedisClusterStateView)
	bytes, err := json.Marshal(reconciler.RedisClusterStateView)
	if err != nil {
		fmt.Printf("error: " + err.Error())
	} else {
		fmt.Printf(string(bytes) + "\n")
		e := reconciler.updateClusterStateView(cluster)
		if e != nil {
			fmt.Printf(e.Error() + "\n")
			e := reconciler.createClusterStateView(cluster)
			if e != nil {
				fmt.Printf(e.Error() + "\n")
			}
		}
	}
	reconciler.getClusterStateView(cluster)
	fmt.Printf("%+v\n", reconciler.RedisClusterStateView)
	bytes, err = json.Marshal(reconciler.RedisClusterStateView)
	if err != nil {
		fmt.Printf("error: " + err.Error())
	} else {
		fmt.Printf(string(bytes) + "\n")
	}
	return c.String(http.StatusOK, "Cluster state view printed to screen")
}
