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
	"github.com/minio/minio-go/v6"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	batchV1 "k8s.io/api/batch/v1"
	coreV1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

type PatchOperations []PatchOperation

// StatusReport structures data returned by the /status endpoint using
// the GetStatusHandler() and implementing the GetStatus() method in this package.
type StatusReport struct {
	JobHasError bool
	JobError    string
	JobStatus   batchV1.JobStatus
	PVCHasError bool
	PVCError    string
	PVCStatus   coreV1.PersistentVolumeClaimStatus
}

// S3Config structures authentication, bucket and prefix
// configuration used to pull objects from an S3/MinIO object cluster.
type S3Config struct {
	S3Endpoint string `json:"s3_endpoint"`
	S3SSL      bool   `json:"s3_ssl"`
	S3Bucket   string `json:"s3_bucket"`
	S3Prefix   string `json:"s3_prefix"`
	S3Key      string `json:"s3_key"`
	S3Secret   string `json:"s3_secret"`
}

// VolConfig is part of the PVCRequestConfig and used to specify
// the name of the volume to create the the Kubernetes storage class.
// run `kubectl get StorageClass` to see a list of available storage
// classed for a cluster.
type VolConfig struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	StorageClass string `json:"storage_class"`
}

// PVCRequestConfig is the primary configuration structure for describing
// the S3/MinIO cluster to pull objects from and kubernetes pvc to create
// and place the objects in.
type PVCRequestConfig struct {
	S3Config
	VolConfig
}

// Config configures the API
type Config struct {
	Service              string
	Version              string
	VolumeOveragePercent int
	MCImage              string
	Log                  *zap.Logger
	Cs                   *kubernetes.Clientset
}

// API is primary object implementing the core API methods
// and HTTP handlers
type API struct {
	*Config
	LogErrors prometheus.Counter
}

// NewApi constructs an API object and populates it with
// configuration along with setting defaults where required.
func NewApi(cfg *Config) (*API, error) {
	a := &API{Config: cfg}

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

// OkHandler is provided for created a default slash route for the
// HTTP API and returns basic version, node and service name.
func (a *API) OkHandler(version string, mode string, service string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": version, "mode": mode, "service": service})
	}
}

// DeleteHandler used for the /delete HTTP endpoint to delete a PVC
func (a *API) DeleteHandler() gin.HandlerFunc {
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

// Delete a PVC. @TODO limit to pvc created by PCI by looking at labels
func (a *API) Delete(pvcRequestConfig PVCRequestConfig) error {
	ctx := context.Background()

	pvcClient := a.Cs.CoreV1().PersistentVolumeClaims(pvcRequestConfig.Namespace)

	err := pvcClient.Delete(ctx, pvcRequestConfig.Name, metaV1.DeleteOptions{})
	if err != nil {
		return err
	}

	return nil
}

// ModeSet set a mode on a PVC to one more more of ReadWriteOnce, ReadOnlyMany,
// ReadWriteMany where supported by the storage driver associated with the
// storage class of the PVC.
func (a *API) ModeSet(pvcRequestConfig PVCRequestConfig, modes []coreV1.PersistentVolumeAccessMode) error {
	ctx := context.Background()

	pvcClient := a.Cs.CoreV1().PersistentVolumeClaims(pvcRequestConfig.Namespace)

	pvc, err := pvcClient.Get(ctx, pvcRequestConfig.Name, metaV1.GetOptions{})
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
	_, err = a.Cs.CoreV1().PersistentVolumes().Patch(ctx, pvc.Spec.VolumeName, types.JSONPatchType, poJson, metaV1.PatchOptions{})

	return err
}

// SetModeHandler is ued by the /mode/rox (read-only many) and /mode/rwo (read-write once)
// HTTP POST endpoints.
func (a *API) SetModeHandler(modes []coreV1.PersistentVolumeAccessMode) gin.HandlerFunc {
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

// CleanupHandler used by the HTTP POST /cleanup endpoint to
// remove completed jobs.
func (a *API) CleanupHandler() gin.HandlerFunc {
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

// Cleanup removes jobs created by PVCI.
func (a *API) Cleanup(pvcRequestConfig PVCRequestConfig) error {
	ctx := context.Background()

	// delete job
	jobsClient := a.Cs.BatchV1().Jobs(pvcRequestConfig.Namespace)

	err := jobsClient.Delete(ctx, pvcRequestConfig.Name, metaV1.DeleteOptions{})
	if err != nil {
		a.Log.Error("unable to delete job", zap.Error(err))
	}

	podsClient := a.Cs.CoreV1().Pods(pvcRequestConfig.Namespace)

	errMessage := ""

	// list related pods
	pl, listErr := podsClient.List(ctx, metaV1.ListOptions{
		LabelSelector: "job-name=" + pvcRequestConfig.Name,
	})
	if listErr != nil {
		a.Log.Warn("unable to list pods", zap.Error(listErr))
		errMessage = listErr.Error()
	}

	if pl != nil {
		// delete related docs
		for _, pod := range pl.Items {
			delErr := podsClient.Delete(ctx, pod.Name, metaV1.DeleteOptions{})
			if delErr != nil {
				a.Log.Error("unable delete pod", zap.Error(err))
				errMessage = errMessage + " " + delErr.Error()
			}
		}
	}

	if errMessage != "" {
		return errors.New(errMessage)
	}

	return nil
}

// GetStatusHandler is used by the HTTP POST /status endpoint
// and returns a StatusReport object as JSON.
func (a *API) GetStatusHandler() gin.HandlerFunc {
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

// GetStatus returns a StatusReport representing the state of PVCI created
// Jobs and PVCs.
func (a *API) GetStatus(pvcRequestConfig PVCRequestConfig) (StatusReport, error) {
	sr := StatusReport{}
	ctx := context.Background()

	jobsClient := a.Cs.BatchV1().Jobs(pvcRequestConfig.Namespace)

	job, err := jobsClient.Get(ctx, pvcRequestConfig.Name, metaV1.GetOptions{})
	if err != nil {
		sr.JobHasError = true
		sr.JobError = err.Error()
	}

	if job != nil {
		sr.JobStatus = job.Status
	}

	// get pvc status
	pvcClient := a.Cs.CoreV1().PersistentVolumeClaims(pvcRequestConfig.Namespace)

	pvc, err := pvcClient.Get(ctx, pvcRequestConfig.Name, metaV1.GetOptions{})
	if err != nil {
		sr.PVCHasError = true
		sr.PVCError = err.Error()
	}

	if pvc != nil {
		sr.PVCStatus = pvc.Status
	}

	return sr, nil
}

// GetSizeHandler used by the HTTP POST endpoint /size to get the
// size of a list of S3/MinIO objects (files) based on bucket and prefix.
func (a *API) GetSizeHandler() gin.HandlerFunc {
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

// GetSize gets the size of a list of S3/MinIO objects (files) based on
// bucket and prefix specified in a PVCRequestConfig object.
func (a *API) GetSize(pvcRequestConfig PVCRequestConfig) (int64, int64, error) {
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

// CreatePVCHandler used by the HTTP POST /create endpoint. CreatePVCHandler is
// the core purpose of PVCI, to create PVCs and inject them with files. This
// handler expects a JSON object representing a PVCRequestConfig.
func (a *API) CreatePVCHandler() gin.HandlerFunc {
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

// CreatePVC is the core purpose of PVCI, to create PVCs and inject
// them with files. CreatePVC takes a PVCRequestConfig object and
// creates a Kubernetes PVC, followed by a Kubernetes Job used to
// populate it.
func (a *API) CreatePVC(pvcRequestConfig PVCRequestConfig) error {

	// get bucket size
	objCount, sz, err := a.GetSize(pvcRequestConfig)
	if err != nil {
		return err
	}

	api := a.Cs.CoreV1()

	// create a PersistentVolumeClaim sized for the bucket data
	pvcClient := api.PersistentVolumeClaims(pvcRequestConfig.Namespace)
	volMode := coreV1.PersistentVolumeFilesystem
	storageQty := resource.Quantity{}
	// MiB/MB Conversion plus % overage for copy buffers and set
	// the copy buffer needed for moving objects.
	pctOver := 1 + (float64(a.VolumeOveragePercent) / 100)

	storageQty.Set(int64(math.Ceil((float64(sz) * 1.048576) * pctOver)))

	pvc := coreV1.PersistentVolumeClaim{
		ObjectMeta: metaV1.ObjectMeta{
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
		Spec: coreV1.PersistentVolumeClaimSpec{
			AccessModes: []coreV1.PersistentVolumeAccessMode{
				"ReadWriteOnce",
				"ReadOnlyMany",
			},
			StorageClassName: &pvcRequestConfig.StorageClass,
			VolumeMode:       &volMode,
			Resources: coreV1.ResourceRequirements{
				Requests: coreV1.ResourceList{
					coreV1.ResourceStorage: storageQty,
				},
			},
		},
	}

	ctx := context.Background()
	_, err = pvcClient.Create(ctx, &pvc, metaV1.CreateOptions{})
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

	job := batchV1.Job{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      pvcRequestConfig.Name,
			Namespace: pvcRequestConfig.Namespace,
		},
		Spec: batchV1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: coreV1.PodTemplateSpec{
				Spec: coreV1.PodSpec{
					RestartPolicy: coreV1.RestartPolicyOnFailure,
					Volumes: []coreV1.Volume{
						{
							Name: "attached-pvc",
							VolumeSource: coreV1.VolumeSource{
								PersistentVolumeClaim: &coreV1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcRequestConfig.Name,
									ReadOnly:  false,
								},
							},
						},
					},
					Containers: []coreV1.Container{
						{
							Name:  pvcRequestConfig.Name,
							Image: a.MCImage,
							Command: []string{
								"mc",
								"cp",
								"-r",
								"objstore/" + objPath,
								"/data",
							},
							VolumeMounts: []coreV1.VolumeMount{
								{
									MountPath: "/data",
									Name:      "attached-pvc",
								},
							},
							Env: []coreV1.EnvVar{
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

	_, err = jobsClient.Create(ctx, &job, metaV1.CreateOptions{})
	if err != nil {
		a.Log.Error("could not create job",
			zap.String("name", pvcRequestConfig.Name),
			zap.Error(err),
		)

		// clean up pvc
		err := pvcClient.Delete(ctx, pvcRequestConfig.Name, metaV1.DeleteOptions{})
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

// getMinIOClient constructs a MinIO client used for interacting with
// MinIO or S3. See: https://docs.min.io/docs/golang-client-api-reference
func (a *API) getMinIOClient(pvcRequestConfig PVCRequestConfig) (*minio.Client, error) {

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

// parsePVCRequestConfig is used to Unmarshal JSON representing the PVCRequestConfig
// sent in on POST from most inbound API calls.
func (a *API) parsePVCRequestConfig(c *gin.Context) (*PVCRequestConfig, error) {
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
