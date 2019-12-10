package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/apis/core/v1"
)

var (
	NamespaceNodeSelectors = []string{"scheduler.alpha.kubernetes.io/node-selector"}

	ignoredNamespaces = []string{
		metav1.NamespaceSystem,
		metav1.NamespacePublic,
	}

	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()

	// (https://github.com/kubernetes/kubernetes/issues/57982)
	defaulter = runtime.ObjectDefaulter(runtimeScheme)
)

type WebhookServer struct {
	server               *http.Server
	client               kubernetes.Interface
	namespaceLister      corev1listers.NamespaceLister
	clusterNodeSelectors map[string]string
}

// Webhook Server parameters
type WhSvrParameters struct {
	port      int    // webhook server port
	certFile  string // path to the x509 certificate for https
	keyFile   string // path to the x509 private key matching `CertFile`
	inCluster bool
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)
	_ = v1.AddToScheme(runtimeScheme)
}

func admissionRequired(ignoredList []string, metadata *metav1.ObjectMeta) bool {
	// skip special kubernetes system namespaces
	for _, namespace := range ignoredList {
		if metadata.Namespace == namespace {
			klog.Infof("Skip validation for %v for it's in special namespace:%v", metadata.Name, metadata.Namespace)
			return false
		}
	}

	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	return true
}

func mutationRequired(ignoredList []string, metadata *metav1.ObjectMeta) bool {
	required := admissionRequired(ignoredList, metadata)
	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	klog.Infof("Mutation policy for %v/%v: required:%v", metadata.Namespace, metadata.Name, required)
	return required
}

func validationRequired(ignoredList []string, metadata *metav1.ObjectMeta) bool {
	required := admissionRequired(ignoredList, metadata)
	klog.Infof("Validation policy for %v/%v: required:%v", metadata.Namespace, metadata.Name, required)
	return required
}

func createPatch(nodeSelectors map[string]string) ([]byte, error) {
	var patch []patchOperation

	patch = append(patch, patchOperation{
		Op:    "replace",
		Path:  "/spec/nodeSelector",
		Value: nodeSelectors,
	})

	return json.Marshal(patch)
}

func (whsvr *WebhookServer) SetExternalKubeClientSet(client kubernetes.Interface) {
	whsvr.client = client

	factory := informers.NewSharedInformerFactory(client, 0)
	namespaceInformer := factory.Core().V1().Namespaces()
	whsvr.namespaceLister = namespaceInformer.Lister()
}

func (whsvr *WebhookServer) ValidateInitialization() error {
	if whsvr.namespaceLister == nil {
		return fmt.Errorf("missing namespaceLister")
	}
	if whsvr.client == nil {
		return fmt.Errorf("missing client")
	}
	return nil
}

func (whsvr *WebhookServer) getNamespaceNodeSelectorMap(namespaceName string) (labels.Set, error) {
	namespace, err := whsvr.namespaceLister.Get(namespaceName)
	if errors.IsNotFound(err) {
		namespace, err = whsvr.defaultGetNamespace(namespaceName)
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, err
			}
			return nil, errors.NewInternalError(err)
		}
	} else if err != nil {
		return nil, errors.NewInternalError(err)
	}

	return whsvr.getNodeSelectorMap(namespace)
}

func (whsvr *WebhookServer) defaultGetNamespace(name string) (*corev1.Namespace, error) {
	namespace, err := whsvr.client.CoreV1().Namespaces().Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("namespace %s does not exist", name)
	}
	return namespace, nil
}

func (whsvr *WebhookServer) getNodeSelectorMap(namespace *corev1.Namespace) (labels.Set, error) {
	selector := labels.Set{}
	labelsMap := labels.Set{}
	var err error
	found := false
	if len(namespace.ObjectMeta.Annotations) > 0 {
		for _, annotation := range NamespaceNodeSelectors {
			if ns, ok := namespace.ObjectMeta.Annotations[annotation]; ok {
				labelsMap, err = labels.ConvertSelectorToLabelsMap(ns)
				if err != nil {
					return labels.Set{}, err
				}

				if labels.Conflicts(selector, labelsMap) {
					nsName := namespace.ObjectMeta.Name
					return labels.Set{}, fmt.Errorf("%s annotations' node label selectors conflict", nsName)
				}
				selector = labels.Merge(selector, labelsMap)
				found = true
			}
		}
	}
	if !found {
		selector, err = labels.ConvertSelectorToLabelsMap(whsvr.clusterNodeSelectors["clusterDefaultNodeSelector"])
		if err != nil {
			return labels.Set{}, err
		}
	}
	return selector, nil
}

// validate deployments and services
func (whsvr *WebhookServer) validate(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request
	var (
		availableLabels                 map[string]string
		objectMeta                      *metav1.ObjectMeta
		resourceNamespace, resourceName string
	)

	klog.Infof("AdmissionReview for Kind=%v, Namespace=%v Name=%v (%v) UID=%v patchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, resourceName, req.UID, req.Operation, req.UserInfo)

	switch req.Kind.Kind {
	case "Pod":
		var pod corev1.Pod
		if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
			klog.Errorf("Could not unmarshal raw object: %v", err)
			return &v1beta1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}
		resourceName, resourceNamespace, objectMeta = pod.Name, pod.Namespace, &pod.ObjectMeta
		availableLabels = pod.Labels

	}

	if !validationRequired(ignoredNamespaces, objectMeta) {
		klog.Infof("Skipping validation for %s/%s due to policy check", resourceNamespace, resourceName)
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	allowed := true
	var result *metav1.Status
	klog.Info("available labels:", availableLabels)

	return &v1beta1.AdmissionResponse{
		Allowed: allowed,
		Result:  result,
	}
}

// main mutation process
func (whsvr *WebhookServer) mutate(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request
	var (
		objectMeta                      *metav1.ObjectMeta
		resourceNamespace, resourceName string
	)

	klog.Infof("AdmissionReview for Kind=%v, Namespace=%v Name=%v (%v) UID=%v patchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, resourceName, req.UID, req.Operation, req.UserInfo)

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		klog.Info("Skipping resource")
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	klog.Infof("pod: %+v", pod)

	resourceName, resourceNamespace, objectMeta = pod.Name, req.Namespace, &pod.ObjectMeta

	if !mutationRequired(ignoredNamespaces, objectMeta) {
		klog.Infof("Skipping validation for %s/%s due to policy check", resourceNamespace, resourceName)
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	namespaceNodeSelector, err := whsvr.getNamespaceNodeSelectorMap(resourceNamespace)

	if err != nil {
		klog.Errorf("Error getting namespace selector: %v", err)
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	if labels.Conflicts(namespaceNodeSelector, labels.Set(pod.Spec.NodeSelector)) {
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: "pod node label selector conflicts with its namespace node label selector",
			},
		}
	}

	podNodeSelectorLabels := labels.Merge(namespaceNodeSelector, pod.Spec.NodeSelector)

	klog.Infof("node selectors: %+v", podNodeSelectorLabels)

	nodeSelectors := map[string]string(podNodeSelectorLabels)
	patchBytes, err := createPatch(nodeSelectors)
	if err != nil {
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	klog.Infof("AdmissionResponse: patch=%v\n", string(patchBytes))
	return &v1beta1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

// Serve method for webhook server
func (whsvr *WebhookServer) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		klog.Error("empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		klog.Errorf("Content-Type=%s, expect application/json", contentType)
		http.Error(w, "invalid Content-Type, expect `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *v1beta1.AdmissionResponse
	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		klog.Errorf("Can't decode body: %v", err)
		admissionResponse = &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		if r.URL.Path == "/mutate" {
			admissionResponse = whsvr.mutate(&ar)
		} else if r.URL.Path == "/validate" {
			admissionResponse = whsvr.validate(&ar)
		}
	}

	admissionReview := v1beta1.AdmissionReview{}
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}

	resp, err := json.Marshal(admissionReview)
	if err != nil {
		klog.Errorf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	klog.Infof("Ready to write reponse ...")

	w.Header().Set("Content-Type", "application/json")

	if _, err := w.Write(resp); err != nil {
		klog.Errorf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}
