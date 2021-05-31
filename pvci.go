package pvci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
	minio "github.com/minio/minio-go/v6"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// PatchOperation
// see: http://jsonpatch.com/
type PatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// PatchOperations
type PatchOperations []PatchOperation

// StatusReport
type StatusReport struct {
	JobHasError bool
	JobError    string
	JobStatus   batchv1.JobStatus
	PVCHasError bool
	PVCError    string
	PVCStatus   corev1.PersistentVolumeClaimStatus
}

// S3Config
type S3Config struct {
	S3Endpoint string `json:"s3_endpoint"`
	S3SSL      bool   `json:"s3_ssl"`
	S3Bucket   string `json:"s3_bucket"`
	S3Prefix   string `json:"s3_prefix"`
	S3Key      string `json:"s3_key"`
	S3Secret   string `json:"s3_secret"`
}

// VolConfig
type VolConfig struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	StorageClass string `json:"storage_class"`
}

// PVCRequestConfig
type PVCRequestConfig struct {
	S3Config
	VolConfig
}

// Config configures the API
type Config struct {
	Service              string
	Version              string
	VolumeOveragePercent int
	Log                  *zap.Logger
	Cs                   *kubernetes.Clientset
}

// Api
type Api struct {
	*Config
	LogErrors prometheus.Counter
}

// NewApi
func NewApi(cfg *Config) (*Api, error) {
	a := &Api{Config: cfg}

	// default logger if none specified
	if a.Log == nil {
		zapCfg := zap.NewProductionConfig()
		logger, err := zapCfg.Build()
		if err != nil {
			os.Exit(1)
		}

		a.Log = logger
	}

	return a, nil
}

// OkHandler
func (a *Api) OkHandler(version string, mode string, service string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": version, "mode": mode, "service": service})
	}
}

// DeleteHandler
func (a *Api) DeleteHandler() gin.HandlerFunc {
	return func(c *gin.Context) {

		pvcRequestConfig, err := a.parsePVCRequestConfig(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "unable to read post body",
			})
			return
		}

		err = a.Delete(*pvcRequestConfig)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{})
	}
}

// Delete
func (a *Api) Delete(pvcRequestConfig PVCRequestConfig) error {
	ctx := context.Background()

	pvcClient := a.Cs.CoreV1().PersistentVolumeClaims(pvcRequestConfig.Namespace)

	err := pvcClient.Delete(ctx, pvcRequestConfig.Name, metav1.DeleteOptions{})
	if err != nil {
		return err
	}

	return nil
}

// ModeSet
func (a *Api) ModeSet(pvcRequestConfig PVCRequestConfig, modes []corev1.PersistentVolumeAccessMode) error {
	ctx := context.Background()

	pvcClient := a.Cs.CoreV1().PersistentVolumeClaims(pvcRequestConfig.Namespace)

	pvc, err := pvcClient.Get(ctx, pvcRequestConfig.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	a.Log.Info("ModeSet", zap.String("VolumeName", pvc.Spec.VolumeName))

	po := &PatchOperations{
		{
			Op:    "add",
			Path:  "/spec/accessModes",
			Value: modes,
		},
	}

	poJson, _ := json.Marshal(po)
	_, err = a.Cs.CoreV1().PersistentVolumes().Patch(ctx, pvc.Spec.VolumeName, types.JSONPatchType, poJson, metav1.PatchOptions{})

	return err
}

// ModeRoxHandler
func (a *Api) SetModeHandler(modes []corev1.PersistentVolumeAccessMode) gin.HandlerFunc {
	return func(c *gin.Context) {
		pvcRequestConfig, err := a.parsePVCRequestConfig(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "unable to read post body",
			})
			return
		}

		err = a.ModeSet(*pvcRequestConfig, modes)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{})
	}
}

// CleanupHandler
func (a *Api) CleanupHandler() gin.HandlerFunc {
	return func(c *gin.Context) {

		pvcRequestConfig, err := a.parsePVCRequestConfig(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "unable to read post body",
			})
			return
		}

		err = a.Cleanup(*pvcRequestConfig)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{})
	}
}

// Cleanup
func (a *Api) Cleanup(pvcRequestConfig PVCRequestConfig) error {
	ctx := context.Background()

	// delete job
	jobsClient := a.Cs.BatchV1().Jobs(pvcRequestConfig.Namespace)

	err := jobsClient.Delete(ctx, pvcRequestConfig.Name, metav1.DeleteOptions{})
	if err != nil {
		a.Log.Warn("unable to delete job", zap.Error(err))
	}

	podsClient := a.Cs.CoreV1().Pods(pvcRequestConfig.Namespace)

	errMessage := ""

	// list related pods
	pl, listErr := podsClient.List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + pvcRequestConfig.Name,
	})
	if listErr != nil {
		a.Log.Warn("unable to list pods", zap.Error(listErr))
		errMessage = listErr.Error()
	}

	if pl != nil {
		// delete related docs
		for _, pod := range pl.Items {
			delErr := podsClient.Delete(ctx, pod.Name, metav1.DeleteOptions{})
			if delErr != nil {
				a.Log.Warn("unable delete pod", zap.Error(err))
				errMessage = errMessage + " " + delErr.Error()
			}
		}
	}

	if errMessage != "" {
		return errors.New(errMessage)
	}

	return nil
}

// GetStatusHandler
func (a *Api) GetStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {

		pvcRequestConfig, err := a.parsePVCRequestConfig(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "unable to read post body",
			})
			return
		}

		sr, err := a.GetStatus(*pvcRequestConfig)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, sr)
	}
}

// GetStatus
func (a *Api) GetStatus(pvcRequestConfig PVCRequestConfig) (StatusReport, error) {
	sr := StatusReport{}
	ctx := context.Background()

	jobsClient := a.Cs.BatchV1().Jobs(pvcRequestConfig.Namespace)

	job, err := jobsClient.Get(ctx, pvcRequestConfig.Name, metav1.GetOptions{})
	if err != nil {
		sr.JobHasError = true
		sr.JobError = err.Error()
	}

	if job != nil {
		sr.JobStatus = job.Status
	}

	// get pvc status
	pvcClient := a.Cs.CoreV1().PersistentVolumeClaims(pvcRequestConfig.Namespace)

	pvc, err := pvcClient.Get(ctx, pvcRequestConfig.Name, metav1.GetOptions{})
	if err != nil {
		sr.PVCHasError = true
		sr.PVCError = err.Error()
	}

	if pvc != nil {
		sr.PVCStatus = pvc.Status
	}

	return sr, nil
}

// GetSizeHandler
func (a *Api) GetSizeHandler() gin.HandlerFunc {
	return func(c *gin.Context) {

		pvcRequestConfig, err := a.parsePVCRequestConfig(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "unable to read post body",
			})
			return
		}

		cnt, sz, err := a.GetSize(*pvcRequestConfig)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{"objects": cnt, "bytes": sz})
	}
}

// GetSize
func (a *Api) GetSize(pvcRequestConfig PVCRequestConfig) (int64, int64, error) {
	objCount := int64(0)
	totalSize := int64(0)

	minioClient, err := a.getMinIOClient(pvcRequestConfig)
	if err != nil {
		return objCount, totalSize, err
	}

	// Create a done channel to control 'ListObjectsV2' go routine.
	doneCh := make(chan struct{})

	// Indicate to our routine to exit cleanly upon return.
	defer close(doneCh)

	objectCh := minioClient.ListObjectsV2(
		pvcRequestConfig.S3Bucket,
		pvcRequestConfig.S3Prefix,
		true,
		doneCh)

	for object := range objectCh {
		if object.Err != nil {
			a.Log.Warn("object error", zap.Error(object.Err))
			return objCount, totalSize, object.Err
		}
		objCount += 1
		totalSize += object.Size
	}

	return objCount, totalSize, nil
}

// CreatePVCHandler
func (a *Api) CreatePVCHandler() gin.HandlerFunc {
	return func(c *gin.Context) {

		pvcRequestConfig, err := a.parsePVCRequestConfig(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "unable to read post body",
			})
			return
		}

		err = a.CreatePVC(*pvcRequestConfig)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{})
	}
}

// CreatePVC
func (a *Api) CreatePVC(pvcRequestConfig PVCRequestConfig) error {

	// get bucket size
	objCount, sz, err := a.GetSize(pvcRequestConfig)
	if err != nil {
		return err
	}

	api := a.Cs.CoreV1()

	// create a PersistentVolumeClaim sized for the bucket data
	pvcClient := api.PersistentVolumeClaims(pvcRequestConfig.Namespace)
	volMode := corev1.PersistentVolumeFilesystem
	storageQty := resource.Quantity{}
	// MiB/MB Conversion plus % overage for copy buffers and set
	// the copy buffer needed for moving objects.
	pctOver := 1 + (float64(a.VolumeOveragePercent) / 100)

	storageQty.Set(int64(math.Ceil((float64(sz) * 1.048576) * pctOver)))

	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcRequestConfig.Name,
			Namespace: pvcRequestConfig.Namespace,
			Labels: map[string]string{
				"pvci.txn2.com/service": a.Service,
				"pvci.txn2.com/version": a.Version,
			},
			Annotations: map[string]string{
				"pvci.txn2.com/requested_size": strconv.FormatInt(sz, 10),
				"pvci.txn2.com/object_count":   strconv.FormatInt(objCount, 10),
				"pvci.txn2.com/origin": fmt.Sprintf("%s/%s/%s",
					pvcRequestConfig.S3Endpoint,
					pvcRequestConfig.S3Bucket,
					pvcRequestConfig.S3Prefix,
				),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				"ReadWriteOnce",
				"ReadOnlyMany",
			},
			StorageClassName: &pvcRequestConfig.StorageClass,
			VolumeMode:       &volMode,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageQty,
				},
			},
		},
	}

	ctx := context.Background()
	_, err = pvcClient.Create(ctx, &pvc, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// create a Job with MinIO client Pod attached to the new pvc
	jobsClient := a.Cs.BatchV1().Jobs(pvcRequestConfig.Namespace)

	objStoreEpProto := "http://"
	if pvcRequestConfig.S3SSL == true {
		objStoreEpProto = "https://"
	}

	objStoreEp := fmt.Sprintf(
		"%s%s:%s@%s",
		objStoreEpProto,
		pvcRequestConfig.S3Key,
		pvcRequestConfig.S3Secret,
		pvcRequestConfig.S3Endpoint,
	)

	objPath := fmt.Sprintf(
		"%s/%s",
		pvcRequestConfig.S3Bucket,
		pvcRequestConfig.S3Prefix,
	)

	// seconds to keep job
	ttl := int32(120)

	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcRequestConfig.Name,
			Namespace: pvcRequestConfig.Namespace,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Volumes: []corev1.Volume{
						{
							Name: "attached-pvc",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcRequestConfig.Name,
									ReadOnly:  false,
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  pvcRequestConfig.Name,
							Image: "minio/mc:RELEASE.2020-06-26T19-56-55Z",
							Command: []string{
								"mc",
								"cp",
								"-r",
								"objstore/" + objPath,
								"/data",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									MountPath: "/data",
									Name:      "attached-pvc",
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "MC_HOST_objstore",
									Value: objStoreEp,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err = jobsClient.Create(ctx, &job, metav1.CreateOptions{})
	if err != nil {
		a.Log.Error("could not create job",
			zap.String("name", pvcRequestConfig.Name),
			zap.Error(err),
		)

		// clean up pvc
		err := pvcClient.Delete(ctx, pvcRequestConfig.Name, metav1.DeleteOptions{})
		if err != nil {
			a.Log.Error("could not delete pvc",
				zap.String("name", pvcRequestConfig.Name),
				zap.Error(err),
			)
			return err
		}
		return err
	}

	// edit PVC remove ReadWriteOnce

	return nil
}

// getMinIOClient
func (a *Api) getMinIOClient(pvcRequestConfig PVCRequestConfig) (*minio.Client, error) {

	// Initialize MinIO client object.
	minioClient, err := minio.New(
		pvcRequestConfig.S3Endpoint,
		pvcRequestConfig.S3Key,
		pvcRequestConfig.S3Secret,
		pvcRequestConfig.S3SSL,
	)
	if err != nil {
		return nil, err
	}

	return minioClient, err
}

// parsePVCRequestConfig
func (a *Api) parsePVCRequestConfig(c *gin.Context) (*PVCRequestConfig, error) {
	rs, err := c.GetRawData()
	if err != nil {
		return nil, err
	}

	pvcRequestConfig := &PVCRequestConfig{}
	err = json.Unmarshal(rs, pvcRequestConfig)
	if err != nil {
		return nil, err
	}

	return pvcRequestConfig, nil
}
