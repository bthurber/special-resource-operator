package filter

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	"github.com/openshift-psap/special-resource-operator/pkg/color"
	"github.com/openshift-psap/special-resource-operator/pkg/exit"
	"github.com/openshift-psap/special-resource-operator/pkg/hash"
	"github.com/openshift-psap/special-resource-operator/pkg/lifecycle"
	"github.com/openshift-psap/special-resource-operator/pkg/storage"
	"github.com/openshift-psap/special-resource-operator/pkg/warn"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

var (
	Mode   string
	sroGVK string
	owned  string
	log    logr.Logger
)

func init() {
	sroGVK = "SpecialResource"
	owned = "specialresource.openshift.io/owned"
}

func init() {
	log = zap.New(zap.UseDevMode(true)).WithName(color.Print("filter", color.Purple))
}

func SetLabel(obj *unstructured.Unstructured) {

	var labels map[string]string

	if labels = obj.GetLabels(); labels == nil {
		labels = make(map[string]string)
	}

	labels[owned] = "true"
	obj.SetLabels(labels)

	SetSubResourceLabel(obj)
}

func SetSubResourceLabel(obj *unstructured.Unstructured) {

	if obj.GetKind() == "DaemonSet" || obj.GetKind() == "Deployment" ||
		obj.GetKind() == "StatefulSet" {

		labels, found, err := unstructured.NestedMap(obj.Object, "spec", "template", "metadata", "labels")
		exit.OnErrorOrNotFound(found, err)

		labels[owned] = "true"
		err = unstructured.SetNestedMap(obj.Object, labels, "spec", "template", "metadata", "labels")
		exit.OnError(err)
	}

	if obj.GetKind() == "BuildConfig" {
		log.Info("TODO: how to set label ownership for Builds and related Pods")
		/*
			output, found, err := unstructured.NestedMap(obj.Object, "spec", "output")
			exit.OnErrorOrNotFound(found, err)

			label := make(map[string]interface{})
			label["name"] = owned
			label["value"] = "true"
			imageLabels := append(make([]interface{}, 0), label)

			if _, found := output["imageLabels"]; !found {
				err := unstructured.SetNestedSlice(obj.Object, imageLabels, "spec", "output", "imageLabels")
				exit.OnError(err)
			}
		*/
	}
}

func IsSpecialResource(obj client.Object) bool {

	kind := obj.GetObjectKind().GroupVersionKind().Kind

	if kind == sroGVK {
		log.Info(Mode+" IsSpecialResource (sroGVK)", "Name", obj.GetName(), "Type", reflect.TypeOf(obj).String())
		return true
	}

	t := reflect.TypeOf(obj).String()

	if strings.Contains(t, sroGVK) {
		log.Info(Mode+" IsSpecialResource (reflect)", "Name", obj.GetName(), "Type", reflect.TypeOf(obj).String())
		return true

	}

	// If SRO owns the resource than it cannot be a SpecialResource
	if Owned(obj) {
		return false
	}

	// We need this because a newly created SpecialResource will not yet
	// have a GVK
	selfLink := obj.GetSelfLink()
	if strings.Contains(selfLink, "/apis/sro.openshift.io/v") {
		log.Info(Mode+" IsSpecialResource (selflink)", "Name", obj.GetName(), "Type", reflect.TypeOf(obj).String())
		return true
	}
	if kind == "" {
		objstr := fmt.Sprintf("%+v", obj)
		if strings.Contains(objstr, "sro.openshift.io/v") {
			log.Info(Mode+" IsSpecialResource (contains)", "Name", obj.GetName(), "Type", reflect.TypeOf(obj).String())
			return true
		}
	}

	return false
}

func Owned(obj client.Object) bool {

	for _, owner := range obj.GetOwnerReferences() {
		if owner.Kind == sroGVK {
			log.Info(Mode+" Owned (sroGVK)", "Name", obj.GetName(),
				"Type", reflect.TypeOf(obj).String())
			return true
		}
	}

	var labels map[string]string

	if labels = obj.GetLabels(); labels != nil {
		if _, found := labels[owned]; found {
			log.Info(Mode+" Owned (label)", "Name", obj.GetName(),
				"Type", reflect.TypeOf(obj).String())
			return true
		}
	}
	return false
}

func Predicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {

			Mode = "CREATE"
			// If a specialresource dependency is deleted we
			/* want to recreate it so handle the delete event */
			obj := e.Object

			if IsSpecialResource(obj) {
				return true
			}

			if Owned(obj) {
				return true
			}

			return false
		},

		UpdateFunc: func(e event.UpdateEvent) bool {
			// Ignore updates if the resourceVersion does not change
			// resourceVersion is updated when the object is modified

			/* UPDATING THE STATUS WILL INCREASE THE RESOURCEVERSION DISABLING
			 * BUT KEEPING FOR REFERENCE
			if e.MetaOld.GetResourceVersion() == e.MetaNew.GetResourceVersion() {
				return false
			}*/
			Mode = "UPDATE"

			e.ObjectOld.GetGeneration()
			e.ObjectOld.GetOwnerReferences()

			// Ignore updates to CR status in which case metadata.Generation does not change
			if e.ObjectOld.GetGeneration() == e.ObjectNew.GetGeneration() {
				return false
			}
			// Some objects will increase generation on Update SRO sets the
			// resourceversion New = Old so we can filter on those even if an
			// update does not change anything see e.g. Deployment or SCC
			if e.ObjectOld.GetResourceVersion() == e.ObjectNew.GetResourceVersion() {
				return false
			}

			// If a specialresource dependency is updated we
			// want to reconcile it, handle the update event
			obj := e.ObjectNew

			if IsSpecialResource(obj) {
				log.Info(Mode+" IsSpecialResource GenerationChanged",
					"Name", obj.GetName(), "Type", reflect.TypeOf(obj).String())
				return true
			}

			// If we do not own the object, do not care
			if Owned(obj) {

				log.Info(Mode+" Owned GenerationChanged",
					"Name", obj.GetName(), "Type", reflect.TypeOf(obj).String())

				if reflect.TypeOf(obj).String() == "*v1.DaemonSet" {
					err := lifecycle.UpdateDaemonSetPods(obj)
					warn.OnError(err)
				}

				return true
			}

			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {

			Mode = "DELETE"
			// If a specialresource dependency is deleted we
			/* want to recreate it so handle the delete event */
			obj := e.Object
			if IsSpecialResource(obj) {
				return true
			}

			// If we do not own the object, do not care
			if Owned(obj) {

				ins := types.NamespacedName{
					Namespace: os.Getenv("OPERATOR_NAMESPACE"),
					Name:      "special-resource-lifecycle",
				}
				key := hash.FNV64a(obj.GetNamespace() + obj.GetName())
				err := storage.DeleteConfigMapEntry(key, ins)
				warn.OnError(err)

				return true
			}
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {

			Mode = "GENERIC"

			// If a specialresource dependency is updated we
			// want to reconcile it, handle the update event
			obj := e.Object
			if IsSpecialResource(obj) {
				return true
			}
			// If we do not own the object, do not care
			if Owned(obj) {
				return true
			}
			return false

		},
	}
}
