package handlerfactory

/*

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	goruntime "runtime"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/handlers"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
)

// responder implements rest.Responder for assisting a connector in writing objects or errors.
type responder struct {
	scope *handlers.RequestScope
	req   *http.Request
	w     http.ResponseWriter
}

func (r *responder) Object(statusCode int, obj runtime.Object) {
	responsewriters.WriteObjectNegotiated(r.scope.Serializer, r.scope, r.scope.Kind.GroupVersion(), r.w, r.req, statusCode, obj)
}

func (r *responder) Error(err error) {
	responsewriters.ErrorNegotiated(err, r.scope.Serializer, r.scope.Kind.GroupVersion(), r.w, r.req)
}

// resultFunc is a function that returns a rest result and can be run in a goroutine
type resultFunc func() (runtime.Object, error)

// finishRequest makes a given resultFunc asynchronous and handles errors returned by the response.
// An api.Status object with status != success is considered an "error", which interrupts the normal response flow.
func finishRequest(timeout time.Duration, fn resultFunc) (result runtime.Object, err error) {
	// these channels need to be buffered to prevent the goroutine below from hanging indefinitely
	// when the select statement reads something other than the one the goroutine sends on.
	ch := make(chan runtime.Object, 1)
	errCh := make(chan error, 1)
	panicCh := make(chan interface{}, 1)
	go func() {
		// panics don't cross goroutine boundaries, so we have to handle ourselves
		defer func() {
			panicReason := recover()
			if panicReason != nil {
				// do not wrap the sentinel ErrAbortHandler panic value
				if panicReason != http.ErrAbortHandler {
					// Same as stdlib http server code. Manually allocate stack
					// trace buffer size to prevent excessively large logs
					const size = 64 << 10
					buf := make([]byte, size)
					buf = buf[:goruntime.Stack(buf, false)]
					panicReason = fmt.Sprintf("%v\n%s", panicReason, buf)
				}
				// Propagate to parent goroutine
				panicCh <- panicReason
			}
		}()

		if result, err := fn(); err != nil {
			errCh <- err
		} else {
			ch <- result
		}
	}()

	select {
	case result = <-ch:
		if status, ok := result.(*metav1.Status); ok {
			if status.Status != metav1.StatusSuccess {
				return nil, errors.FromObject(status)
			}
		}
		return result, nil
	case err = <-errCh:
		return nil, err
	case p := <-panicCh:
		panic(p)
	case <-time.After(timeout):
		return nil, errors.NewTimeoutError(fmt.Sprintf("request did not complete within requested timeout %s", timeout), 0)
	}
}

// transformDecodeError adds additional information into a bad-request api error when a decode fails.
func transformDecodeError(typer runtime.ObjectTyper, baseErr error, into runtime.Object, gvk *schema.GroupVersionKind, body []byte) error {
	objGVKs, _, err := typer.ObjectKinds(into)
	if err != nil {
		return errors.NewBadRequest(err.Error())
	}
	objGVK := objGVKs[0]
	if gvk != nil && len(gvk.Kind) > 0 {
		return errors.NewBadRequest(fmt.Sprintf("%s in version %q cannot be handled as a %s: %v", gvk.Kind, gvk.Version, objGVK.Kind, baseErr))
	}
	summary := summarizeData(body, 30)
	return errors.NewBadRequest(fmt.Sprintf("the object provided is unrecognized (must be of type %s): %v (%s)", objGVK.Kind, baseErr, summary))
}

// setSelfLink sets the self link of an object (or the child items in a list) to the base URL of the request
// plus the path and query generated by the provided linkFunc
func setSelfLink(obj runtime.Object, requestInfo *request.RequestInfo, namer handlers.ScopeNamer) error {
	// TODO: SelfLink generation should return a full URL?
	uri, err := namer.GenerateLink(requestInfo, obj)
	if err != nil {
		return nil
	}

	return namer.SetSelfLink(obj, uri)
}

func hasUID(obj runtime.Object) (bool, error) {
	if obj == nil {
		return false, nil
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return false, errors.NewInternalError(err)
	}
	if len(accessor.GetUID()) == 0 {
		return false, nil
	}
	return true, nil
}

// checkName checks the provided name against the request
func checkName(obj runtime.Object, name, namespace string, namer handlers.ScopeNamer) error {
	objNamespace, objName, err := namer.ObjectName(obj)
	if err != nil {
		return errors.NewBadRequest(fmt.Sprintf(
			"the name of the object (%s based on URL) was undeterminable: %v", name, err))
	}
	if objName != name {
		return errors.NewBadRequest(fmt.Sprintf(
			"the name of the object (%s) does not match the name on the URL (%s)", objName, name))
	}
	if len(namespace) > 0 {
		if len(objNamespace) > 0 && objNamespace != namespace {
			return errors.NewBadRequest(fmt.Sprintf(
				"the namespace of the object (%s) does not match the namespace on the request (%s)", objNamespace, namespace))
		}
	}

	return nil
}

// setObjectSelfLink sets the self link of an object as needed.
// TODO: remove the need for the namer LinkSetters by requiring objects implement either Object or List
//   interfaces
func setObjectSelfLink(ctx context.Context, obj runtime.Object, req *http.Request, namer handlers.ScopeNamer) error {
	if utilfeature.DefaultFeatureGate.Enabled(features.RemoveSelfLink) {
		return nil
	}

	// We only generate list links on objects that implement ListInterface - historically we duck typed this
	// check via reflection, but as we move away from reflection we require that you not only carry Items but
	// ListMeta into order to be identified as a list.
	if !meta.IsListType(obj) {
		requestInfo, ok := request.RequestInfoFrom(ctx)
		if !ok {
			return fmt.Errorf("missing requestInfo")
		}
		return setSelfLink(obj, requestInfo, namer)
	}

	uri, err := namer.GenerateListLink(req)
	if err != nil {
		return err
	}
	if err := namer.SetSelfLink(obj, uri); err != nil {
		klog.V(4).Infof("Unable to set self link on object: %v", err)
	}
	requestInfo, ok := request.RequestInfoFrom(ctx)
	if !ok {
		return fmt.Errorf("missing requestInfo")
	}

	count := 0
	err = meta.EachListItem(obj, func(obj runtime.Object) error {
		count++
		return setSelfLink(obj, requestInfo, namer)
	})

	if count == 0 {
		if err := meta.SetList(obj, []runtime.Object{}); err != nil {
			return err
		}
	}

	return err
}

func summarizeData(data []byte, maxLength int) string {
	switch {
	case len(data) == 0:
		return "<empty>"
	case data[0] == '{':
		if len(data) > maxLength {
			return string(data[:maxLength]) + " ..."
		}
		return string(data)
	default:
		if len(data) > maxLength {
			return hex.EncodeToString(data[:maxLength]) + " ..."
		}
		return hex.EncodeToString(data)
	}
}

func limitedReadBody(req *http.Request, limit int64) ([]byte, error) {
	defer req.Body.Close()
	if limit <= 0 {
		return ioutil.ReadAll(req.Body)
	}
	lr := &io.LimitedReader{
		R: req.Body,
		N: limit + 1,
	}
	data, err := ioutil.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if lr.N <= 0 {
		return nil, errors.NewRequestEntityTooLargeError(fmt.Sprintf("limit is %d", limit))
	}
	return data, nil
}

func parseTimeout(str string) time.Duration {
	if str != "" {
		timeout, err := time.ParseDuration(str)
		if err == nil {
			return timeout
		}
		klog.Errorf("Failed to parse %q: %v", str, err)
	}
	// 34 chose as a number close to 30 that is likely to be unique enough to jump out at me the next time I see a timeout.  Everyone chooses 30.
	return 34 * time.Second
}

func isDryRun(url *url.URL) bool {
	return len(url.Query()["dryRun"]) != 0
}

//type etcdError interface {
//	Code() grpccodes.Code
//	Error() string
//}
//
//type grpcError interface {
//	GRPCStatus() *grpcstatus.Status
//}
//
//func isTooLargeError(err error) bool {
//	if err != nil {
//		if etcdErr, ok := err.(etcdError); ok {
//			if etcdErr.Code() == grpccodes.InvalidArgument && etcdErr.Error() == "etcdserver: request is too large" {
//				return true
//			}
//		}
//		if grpcErr, ok := err.(grpcError); ok {
//			if grpcErr.GRPCStatus().Code() == grpccodes.ResourceExhausted && strings.Contains(grpcErr.GRPCStatus().Message(), "trying to send message larger than max") {
//				return true
//			}
//		}
//	}
//	return false
//}
*/