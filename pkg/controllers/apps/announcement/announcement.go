/*
Copyright 2021 The Clusternet Authors.

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

package announcement

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	appsapi "github.com/clusternet/clusternet/pkg/apis/apps/v1alpha1"
	clusternetClientSet "github.com/clusternet/clusternet/pkg/generated/clientset/versioned"
	appInformers "github.com/clusternet/clusternet/pkg/generated/informers/externalversions/apps/v1alpha1"
	appListers "github.com/clusternet/clusternet/pkg/generated/listers/apps/v1alpha1"
	"github.com/clusternet/clusternet/pkg/known"
	"github.com/clusternet/clusternet/pkg/utils"
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = appsapi.SchemeGroupVersion.WithKind("Announcement")

type SyncHandlerFunc func(announcement *appsapi.Announcement) error

// Controller is a controller that handle Announcement
type Controller struct {
	ctx context.Context

	clusternetClient clusternetClientSet.Interface

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface

	anncLister appListers.AnnouncementLister
	anncSynced cache.InformerSynced

	descLister appListers.DescriptionLister
	descSynced cache.InformerSynced

	SyncHandler SyncHandlerFunc
}

func NewController(ctx context.Context, clusternetClient clusternetClientSet.Interface,
	anncInformer appInformers.AnnouncementInformer, descInformer appInformers.DescriptionInformer,
	syncHandler SyncHandlerFunc) (*Controller, error) {
	if syncHandler == nil {
		return nil, fmt.Errorf("syncHandler must be set")
	}

	c := &Controller{
		ctx:              ctx,
		clusternetClient: clusternetClient,
		workqueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "announcement"),
		anncLister:       anncInformer.Lister(),
		anncSynced:       anncInformer.Informer().HasSynced,
		descLister:       descInformer.Lister(),
		descSynced:       descInformer.Informer().HasSynced,
		SyncHandler:      syncHandler,
	}

	// Manage the addition/update of Announcement
	anncInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addAnnouncement,
		UpdateFunc: c.updateAnnouncement,
		DeleteFunc: c.deleteAnnouncement,
	})

	descInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: c.deleteDescription,
	})

	return c, nil
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.Info("starting announcement controller...")
	defer klog.Info("shutting down announcement controller")

	// Wait for the caches to be synced before starting workers
	klog.V(5).Info("waiting for informer caches to sync")
	if !cache.WaitForCacheSync(stopCh, c.anncSynced, c.descSynced) {
		return
	}

	klog.V(5).Infof("starting %d worker threads", workers)
	// Launch workers to process Announcement resources
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
}

func (c *Controller) addAnnouncement(obj interface{}) {
	annc := obj.(*appsapi.Announcement)
	klog.V(4).Infof("adding Announcement %q", klog.KObj(annc))

	// add finalizer
	if annc.DeletionTimestamp == nil {
		if !utils.ContainsString(annc.Finalizers, known.AppFinalizer) {
			annc.Finalizers = append(annc.Finalizers, known.AppFinalizer)
		}
		_, err := c.clusternetClient.AppsV1alpha1().Announcements(annc.Namespace).Update(context.TODO(),
			annc, metav1.UpdateOptions{})
		if err == nil {
			msg := fmt.Sprintf("successfully inject finalizer %s to Announcement %s", known.AppFinalizer, klog.KObj(annc))
			klog.V(4).Info(msg)
			// todo: add recorder
		} else {
			klog.WarningDepth(4,
				fmt.Sprintf("failed to inject finalizer %s to Announcement %s: %v", known.AppFinalizer, klog.KObj(annc), err))
			c.addAnnouncement(obj)
			return
		}
	}

	c.enqueue(annc)
}

func (c *Controller) updateAnnouncement(old, cur interface{}) {
	oldAnnc := old.(*appsapi.Announcement)
	newAnnc := cur.(*appsapi.Announcement)

	// Decide whether discovery has reported a spec change.
	if reflect.DeepEqual(oldAnnc.Spec, newAnnc.Spec) {
		klog.V(4).Infof("no updates on the spec of Announcement %q, skipping syncing", oldAnnc.Name)
		return
	}

	klog.V(4).Infof("updating Announcement %q", klog.KObj(oldAnnc))
	c.enqueue(newAnnc)
}

func (c *Controller) deleteAnnouncement(obj interface{}) {
	annc, ok := obj.(*appsapi.Announcement)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		_, ok = tombstone.Obj.(*appsapi.Announcement)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Announcement %#v", obj))
			return
		}
	}

	descs, err := c.descLister.List(labels.SelectorFromSet(labels.Set{
		known.ConfigSourceKindLabel: annc.Kind,
		known.ConfigNameLabel:       annc.Name,
		known.ConfigNamespaceLabel:  annc.Namespace,
	}))
	if err == nil {
		// delete all matching Description
		var allErrors []error
		deletePropagationBackground := metav1.DeletePropagationBackground
		for _, desc := range descs {
			if desc.DeletionTimestamp != nil {
				continue
			}
			err = c.clusternetClient.AppsV1alpha1().Descriptions(desc.Namespace).Delete(context.TODO(), desc.Name, metav1.DeleteOptions{
				PropagationPolicy: &deletePropagationBackground,
			})
			if err != nil {
				allErrors = append(allErrors, err)
			}
		}

		if len(allErrors) > 0 {
			c.deleteAnnouncement(obj)
			return
		}
	} else {
		c.deleteAnnouncement(obj)
		return
	}

	klog.V(4).Infof("deleting Announcement %q", klog.KObj(annc))
	c.enqueue(annc)
}

func (c *Controller) deleteDescription(obj interface{}) {
	desc, ok := obj.(*appsapi.Description)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		_, ok = tombstone.Obj.(*appsapi.Description)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Description %#v", obj))
			return
		}
	}

	controllerRef := &metav1.OwnerReference{
		Kind: desc.Labels[known.ConfigUIDLabel],
		Name: desc.Labels[known.ConfigNameLabel],
		UID:  types.UID(desc.Labels[known.ConfigUIDLabel]),
	}
	annc := c.resolveControllerRef(desc.Labels[known.ConfigNamespaceLabel], controllerRef)
	if annc == nil {
		return
	}
	klog.V(4).Infof("deleting Description %q", klog.KObj(desc))
	c.enqueue(annc)
}

// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (c *Controller) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *appsapi.Announcement {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != controllerKind.Kind {
		return nil
	}
	annc, err := c.anncLister.Announcements(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if annc.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return annc
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Announcement resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		klog.Infof("successfully synced Announcement %q", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Announcement resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// If an error occurs during handling, we'll requeue the item so we can
	// attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.

	// Convert the namespace/name string into a distinct namespace and name
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	klog.V(4).Infof("start processing Announcement %q", key)
	// Get the Announcement resource with this name
	annc, err := c.anncLister.Announcements(ns).Get(name)
	// The Announcement resource may no longer exist, in which case we stop processing.
	if errors.IsNotFound(err) {
		klog.V(2).Infof("Announcement %q has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	return c.SyncHandler(annc)
}

func (c *Controller) UpdateAnnouncementStatus(annc *appsapi.Announcement, status *appsapi.AnnouncementStatus) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance

	klog.V(5).Infof("try to update Announcement %q status", annc.Name)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		annc.Status = *status
		_, err := c.clusternetClient.AppsV1alpha1().Announcements(annc.Namespace).UpdateStatus(c.ctx, annc, metav1.UpdateOptions{})
		if err == nil {
			//TODO
			return nil
		}

		if updated, err := c.anncLister.Announcements(annc.Namespace).Get(annc.Name); err == nil {
			// make a copy so we don't mutate the shared cache
			annc = updated.DeepCopy()
		} else {
			utilruntime.HandleError(fmt.Errorf("error getting updated Announcement %q from lister: %v", annc.Name, err))
		}
		return err
	})
}

// enqueue takes a Announcement resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Announcement.
func (c *Controller) enqueue(annc *appsapi.Announcement) {
	key, err := cache.MetaNamespaceKeyFunc(annc)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}