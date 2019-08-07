package webhook

import (
	"encoding/json"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
)

var (
	runtimeScheme    = runtime.NewScheme()
	codecs           = serializer.NewCodecFactory(runtimeScheme)
	deserializer     = codecs.UniversalDeserializer()
	systemNameSpaces = []string{
		metav1.NamespaceSystem,
		metav1.NamespacePublic,
	}
)

const (
	sideCarNameSpace                 = "shawarma.centeredge.io/"
	injectAnnotation                 = "service-name"
	imageAnnotation                  = "image"
	statusAnnotation                 = "status"
	sideCarInjectionAnnotation       = sideCarNameSpace + injectAnnotation
	sideCarInjectionStatusAnnotation = sideCarNameSpace + statusAnnotation
	sideCarInjectionImageAnnotation  = sideCarNameSpace + imageAnnotation
	injectedValue                    = "injected"
	sideCarName                      = "shawarma"
)

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

/*SideCar is the template of the sidecar to be implemented*/
type SideCar struct {
	Containers       []corev1.Container            `yaml:"containers"`
	Volumes          []corev1.Volume               `yaml:"volumes"`
	ImagePullSecrets []corev1.LocalObjectReference `yaml:"imagePullSecrets"`
}

/*Mutator is the interface for mutating webhook*/
type Mutator struct {
	SideCars                map[string]*SideCar
	ShawarmaImage           string
	ShawarmaSecretTokenName string
}

/*Mutate function performs the actual mutation of pod spec*/
func (mutator Mutator) Mutate(req []byte) ([]byte, error) {
	admissionReviewResp := v1beta1.AdmissionReview{}
	admissionReviewReq := v1beta1.AdmissionReview{}
	var admissionResponse *v1beta1.AdmissionResponse

	_, _, err := deserializer.Decode(req, nil, &admissionReviewReq)

	if err == nil && admissionReviewReq.Request != nil {
		admissionResponse = mutate(&admissionReviewReq, &mutator)
	} else {
		message := "Failed to decode request"

		if err != nil {
			message = fmt.Sprintf("message: %s err: %v", message, err)
		}

		admissionResponse = &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: message,
			},
		}
	}

	admissionReviewResp.Response = admissionResponse
	return json.Marshal(admissionReviewResp)
}

func mutate(ar *v1beta1.AdmissionReview, mutator *Mutator) *v1beta1.AdmissionResponse {
	req := ar.Request

	log.Infof("AdmissionReview for Kind=%v, Namespace=%v Name=%v UID=%v patchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, req.UID, req.Operation, req.UserInfo)

	pod, err := unMarshall(req)
	if err != nil {
		return errorResponse(ar.Request.UID, err)
	}

	if sideCarNames, ok := shouldMutate(systemNameSpaces, &pod.ObjectMeta); ok {
		annotations := map[string]string{sideCarInjectionStatusAnnotation: injectedValue}
		patchBytes, err := createPatch(&pod, sideCarNames, mutator, annotations)
		if err != nil {
			return errorResponse(req.UID, err)
		}

		log.Infof("AdmissionResponse: Patch: %v\n", string(patchBytes))
		pt := v1beta1.PatchTypeJSONPatch
		return &v1beta1.AdmissionResponse{
			UID:       req.UID,
			Allowed:   true,
			Patch:     patchBytes,
			PatchType: &pt,
		}
	}

	return &v1beta1.AdmissionResponse{
		Allowed: true,
	}
}

func errorResponse(uid types.UID, err error) *v1beta1.AdmissionResponse {
	log.Errorf("AdmissionReview failed : [%v] %s", uid, err)
	return &v1beta1.AdmissionResponse{
		Result: &metav1.Status{
			Message: err.Error(),
		},
	}
}

func unMarshall(req *v1beta1.AdmissionRequest) (corev1.Pod, error) {
	var pod corev1.Pod
	err := json.Unmarshal(req.Object.Raw, &pod)
	return pod, err
}

func shouldMutate(ignoredList []string, metadata *metav1.ObjectMeta) ([]string, bool) {
	for _, namespace := range ignoredList {
		if metadata.Namespace == namespace {
			log.Infof("Skipping mutation for [%v] in special namespace: [%v]", metadata.Name, metadata.Namespace)
			return nil, false
		}
	}

	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	if status, ok := annotations[sideCarInjectionStatusAnnotation]; ok && strings.ToLower(status) == injectedValue {
		log.Infof("Skipping mutation for [%v]. Has been mutated already", metadata.Name)
		return nil, false
	}

	if serviceName, ok := annotations[sideCarInjectionAnnotation]; ok {
		if len(serviceName) > 0 {
			log.Infof("shawarma injection for %v/%v: service-name: %v", metadata.Namespace, metadata.Name, serviceName)
			return []string{sideCarName}, true
		}
	}

	log.Infof("Skipping mutation for [%v]. No action required", metadata.Name)
	return nil, false
}

func createPatch(pod *corev1.Pod, sideCarNames []string, mutator *Mutator, annotations map[string]string) ([]byte, error) {

	var patch []patchOperation
	var containers []corev1.Container
	var volumes []corev1.Volume
	var imagePullSecrets []corev1.LocalObjectReference
	count := 0

	// Check for image override in annotations
	shawarmaImage := mutator.ShawarmaImage
	existingAnnotations := pod.ObjectMeta.GetAnnotations()
	if existingAnnotations != nil {
		if image, ok := existingAnnotations[sideCarInjectionImageAnnotation]; ok {
			log.Infof("Overriding Shawarma image, using %v", image)
			shawarmaImage = image
		}
	}

	for _, name := range sideCarNames {
		if sideCar, ok := mutator.SideCars[name]; ok {
			sideCarCopy := *sideCar

			sideCarCopy.Containers = make([]corev1.Container, len(sideCar.Containers))
			sideCarCopy.Volumes = make([]corev1.Volume, len(sideCar.Volumes))

			// Apply the configured image
			for i := range sideCar.Containers {
				containerCopy := sideCar.Containers[i]
				containerCopy.Image = strings.Replace(containerCopy.Image, "|SHAWARMA_IMAGE|", shawarmaImage, -1)

				sideCarCopy.Containers[i] = containerCopy
			}

			// Apply the configured volumes
			for i := range sideCar.Volumes {
				volumeCopy := sideCar.Volumes[i]

				if volumeCopy.Secret != nil {
					volumeCopy.Secret.SecretName = strings.Replace(volumeCopy.Secret.SecretName, "|SHAWARMA_TOKEN_NAME|", mutator.ShawarmaSecretTokenName, -1)
				}

				sideCarCopy.Volumes[i] = volumeCopy
			}

			containers = append(containers, sideCarCopy.Containers...)
			volumes = append(volumes, sideCarCopy.Volumes...)
			imagePullSecrets = append(imagePullSecrets, sideCarCopy.ImagePullSecrets...)

			count++
		}
	}

	if len(sideCarNames) == count {
		patch = append(patch, addContainer(pod.Spec.Containers, containers, "/spec/containers")...)
		patch = append(patch, addVolume(pod.Spec.Volumes, volumes, "/spec/volumes")...)
		patch = append(patch, addImagePullSecrets(pod.Spec.ImagePullSecrets, imagePullSecrets, "/spec/imagePullSecrets")...)
		patch = append(patch, updateAnnotation(pod.Annotations, annotations)...)

		return json.Marshal(patch)
	}

	return nil, fmt.Errorf("Did not find one or more sidecars to inject %v", sideCarNames)
}

func addContainer(target, added []corev1.Container, basePath string) []patchOperation {
	var patch []patchOperation
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Container{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func addVolume(target, added []corev1.Volume, basePath string) []patchOperation {
	var patch []patchOperation
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Volume{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func addImagePullSecrets(target, added []corev1.LocalObjectReference, basePath string) []patchOperation {
	var patch []patchOperation
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.LocalObjectReference{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func updateAnnotation(target map[string]string, added map[string]string) []patchOperation {
	var patch []patchOperation
	if target == nil {
		target = map[string]string{}
	}
	for key, value := range added {
		keyEscaped := strings.Replace(key, "/", "~1", -1)

		_, ok := target[key]
		if ok {
			patch = append(patch, patchOperation{
				Op:    "replace",
				Path:  "/metadata/annotations/" + keyEscaped,
				Value: value,
			})
		} else {
			patch = append(patch, patchOperation{
				Op:    "add",
				Path:  "/metadata/annotations/" + keyEscaped,
				Value: value,
			})
		}
	}
	return patch
}
