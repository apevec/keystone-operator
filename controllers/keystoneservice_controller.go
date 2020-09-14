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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	gophercloud "github.com/gophercloud/gophercloud"
	openstack "github.com/gophercloud/gophercloud/openstack"
	endpoints "github.com/gophercloud/gophercloud/openstack/identity/v3/endpoints"
	services "github.com/gophercloud/gophercloud/openstack/identity/v3/services"
	keystonev1beta1 "github.com/openstack-k8s-operators/keystone-operator/api/v1beta1"
	keystone "github.com/openstack-k8s-operators/keystone-operator/pkg/keystone"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KeystoneServiceReconciler reconciles a KeystoneService object
type KeystoneServiceReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// Reconcile keystone service requests
// +kubebuilder:rbac:groups=keystone.openstack.org,resources=keystoneservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.openstack.org,resources=keystoneservices/status,verbs=get;update;patch
func (r *KeystoneServiceReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("keystoneservice", req.NamespacedName)

	// your logic here

	keystoneAPI := keystone.API(req.Namespace, "keystone")
	objectKey, err := client.ObjectKeyFromObject(keystoneAPI)
	err = r.Client.Get(context.TODO(), objectKey, keystoneAPI)
	if err != nil {
		if errors.IsNotFound(err) {
			// No KeystoneAPI instance running, return error
			r.Log.Error(err, "KeystoneAPI instance not found")
			return ctrl.Result{}, err
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	if keystoneAPI.Status.BootstrapHash == "" {
		r.Log.Info("KeystoneAPI bootstrap not complete.", "BootstrapHash", keystoneAPI.Status.BootstrapHash)
		return ctrl.Result{RequeueAfter: time.Second * 5}, err
	}
	r.Log.Info("KeystoneAPI bootstrap complete.", "BootstrapHash", keystoneAPI.Status.BootstrapHash)

	// Fetch the KeystoneService instance
	instance := &keystonev1beta1.KeystoneService{}
	err = r.Client.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	opts := gophercloud.AuthOptions{
		IdentityEndpoint: instance.Spec.AuthURL,
		Username:         instance.Spec.Username,
		Password:         instance.Spec.Password,
		TenantName:       instance.Spec.Project,
		DomainName:       instance.Spec.DomainName,
	}

	provider, err := openstack.AuthenticatedClient(opts)
	if err != nil {
		return ctrl.Result{}, err
	}
	endpointOpts := gophercloud.EndpointOpts{Type: "identity", Region: instance.Spec.Region}
	identityClient, err := openstack.NewIdentityV3(provider, endpointOpts)

	// Create new service if ServiceID is not already set
	if instance.Status.ServiceID == "" {
		createOpts := services.CreateOpts{
			Type:    instance.Spec.ServiceType,
			Enabled: &instance.Spec.Enabled,
			Extra: map[string]interface{}{
				"name":        instance.Spec.ServiceName,
				"description": instance.Spec.ServiceDescription,
			},
		}

		service, err := services.Create(identityClient, createOpts).Extract()
		if err != nil {
			r.Log.Error(err, "error")
			return ctrl.Result{}, err
		}

		// Set ServiceID in the status
		r.Log.Info("instance.Status.ServiceID", "ServiceID", instance.Status.ServiceID)
		r.Log.Info("service.ID", "service.ID", service.ID)
		if instance.Status.ServiceID != service.ID {
			instance.Status.ServiceID = service.ID
			if err := r.Client.Status().Update(context.TODO(), instance); err != nil {
				r.Log.Error(err, "error")
				return ctrl.Result{}, err
			}
		}
	} else {
		// ServiceID is already set, update the service
		updateOpts := services.UpdateOpts{
			Type:    instance.Spec.ServiceType,
			Enabled: &instance.Spec.Enabled,
			Extra: map[string]interface{}{
				"name":        instance.Spec.ServiceName,
				"description": instance.Spec.ServiceDescription,
			},
		}
		_, err := services.Update(identityClient, instance.Status.ServiceID, updateOpts).Extract()
		if err != nil {
			r.Log.Error(err, "error")
			return ctrl.Result{}, err
		}
	}

	serviceID := instance.Status.ServiceID
	reconcileEndpoint(identityClient, serviceID, instance.Spec.ServiceName, instance.Spec.Region, "admin", instance.Spec.AdminURL)
	reconcileEndpoint(identityClient, serviceID, instance.Spec.ServiceName, instance.Spec.Region, "internal", instance.Spec.InternalURL)
	reconcileEndpoint(identityClient, serviceID, instance.Spec.ServiceName, instance.Spec.Region, "public", instance.Spec.PublicURL)
	return ctrl.Result{}, nil
}

// SetupWithManager x
func (r *KeystoneServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1beta1.KeystoneService{}).
		Complete(r)
}

func reconcileEndpoint(client *gophercloud.ServiceClient, serviceID string, serviceName string, region string, endpointInterface string, url string) error {
	// Return if url is empty, likely wasn't specified in the request
	if url == "" {
		return nil
	}

	var availability gophercloud.Availability
	if endpointInterface == "admin" {
		availability = gophercloud.AvailabilityAdmin
	} else if endpointInterface == "internal" {
		availability = gophercloud.AvailabilityInternal
	} else if endpointInterface == "public" {
		availability = gophercloud.AvailabilityPublic
	} else {
		return fmt.Errorf("Endpoint interface %s not known", endpointInterface)
	}

	// Fetch existing endpoint and check it's value if it exists
	listOpts := endpoints.ListOpts{
		ServiceID:    serviceID,
		Availability: availability,
		RegionID:     region,
	}
	allPages, err := endpoints.List(client, listOpts).AllPages()
	if err != nil {
		return err
	}
	allEndpoints, err := endpoints.ExtractEndpoints(allPages)
	if err != nil {
		return err
	}
	if len(allEndpoints) == 1 {
		endpoint := allEndpoints[0]
		if url != endpoint.URL {
			// Update the endpoint
			updateOpts := endpoints.UpdateOpts{
				Availability: availability,
				Name:         serviceName,
				Region:       region,
				ServiceID:    serviceID,
				URL:          url,
			}
			_, err := endpoints.Update(client, endpoint.ID, updateOpts).Extract()
			if err != nil {
				return err
			}
		}
	} else {
		// Create the endpoint
		createOpts := endpoints.CreateOpts{
			Availability: availability,
			Name:         serviceName,
			Region:       region,
			ServiceID:    serviceID,
			URL:          url,
		}
		_, err := endpoints.Create(client, createOpts).Extract()
		if err != nil {
			return err
		}
	}

	return nil

}
