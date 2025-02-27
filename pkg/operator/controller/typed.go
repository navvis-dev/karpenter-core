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

package controller

import (
	"context"
	"reflect"
	"strings"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/operator/injection"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
)

type TypedController[T client.Object] interface {
	Reconcile(context.Context, T) (reconcile.Result, error)
	Name() string
	Builder(context.Context, manager.Manager) Builder
}

type FinalizingTypedController[T client.Object] interface {
	TypedController[T]

	Finalize(context.Context, T) (reconcile.Result, error)
}

type typedDecorator[T client.Object] struct {
	kubeClient      client.Client
	typedController TypedController[T]
}

func Typed[T client.Object](kubeClient client.Client, typedReconciler TypedController[T]) Controller {
	return &typedDecorator[T]{
		kubeClient:      kubeClient,
		typedController: typedReconciler,
	}
}

func (t *typedDecorator[T]) Name() string {
	return t.typedController.Name()
}

func (t *typedDecorator[T]) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	obj := reflect.New(reflect.TypeOf(*new(T)).Elem()).Interface().(T) // Create a new pointer to a client.Object
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).
		Named(t.typedController.Name()).
		With(
			strings.ToLower(lo.Must(apiutil.GVKForObject(obj, scheme.Scheme)).Kind),
			lo.Ternary(req.NamespacedName.Namespace != "", req.NamespacedName.String(), req.Name),
		),
	)
	ctx = injection.WithControllerName(ctx, t.typedController.Name())

	if err := t.kubeClient.Get(ctx, req.NamespacedName, obj); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	stored := obj.DeepCopyObject().(client.Object)

	var result reconcile.Result
	var err error

	finalizingTypedController, ok := t.typedController.(FinalizingTypedController[T])
	if !obj.GetDeletionTimestamp().IsZero() && ok {
		result, err = finalizingTypedController.Finalize(ctx, obj)
	} else {
		result, err = t.typedController.Reconcile(ctx, obj)
	}

	if e := t.patch(ctx, stored, obj); e != nil {
		return reconcile.Result{}, multierr.Combine(e, err)
	}
	return result, err
}

func (t *typedDecorator[T]) Builder(ctx context.Context, m manager.Manager) Builder {
	return t.typedController.Builder(ctx, m)
}

func (t *typedDecorator[T]) patch(ctx context.Context, obj, updated client.Object) error {
	// If an updated value returns as nil from the Reconcile function, this means we shouldn't update the object
	if reflect.ValueOf(updated).IsNil() {
		return nil
	}
	// Patch Body if changed
	if !bodyEqual(obj, updated) {
		if err := t.kubeClient.Patch(ctx, updated, client.MergeFrom(obj)); err != nil {
			return err
		}
	}
	// Patch Status if changed
	if !statusEqual(obj, updated) {
		if err := t.kubeClient.Status().Patch(ctx, updated, client.MergeFrom(obj)); err != nil {
			return err
		}
	}
	return nil
}

// bodyEqual compares two objects, ignoring their status and determines if they are deeply-equal
func bodyEqual(a, b client.Object) bool {
	unstructuredA := lo.Must(runtime.DefaultUnstructuredConverter.ToUnstructured(a))
	unstructuredB := lo.Must(runtime.DefaultUnstructuredConverter.ToUnstructured(b))

	// Remove the status fields, so we are only left with non-status info
	delete(unstructuredA, "status")
	delete(unstructuredB, "status")

	return equality.Semantic.DeepEqual(unstructuredA, unstructuredB)
}

// statusEqual compares two object statuses and determines if they are deeply-equal
func statusEqual(a, b client.Object) bool {
	unstructuredA := lo.Must(runtime.DefaultUnstructuredConverter.ToUnstructured(a))
	unstructuredB := lo.Must(runtime.DefaultUnstructuredConverter.ToUnstructured(b))

	return equality.Semantic.DeepEqual(unstructuredA["status"], unstructuredB["status"])
}
