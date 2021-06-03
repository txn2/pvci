package pvci

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

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
	InjectorHasError bool
	InjectorError    string
	InjectorState    string
	PVCHasError      bool
	PVCError         string
	PVCStatus        coreV1.PersistentVolumeClaimStatus
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
	AvgMPS               int
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

	// get injector status
	podClient := a.Cs.CoreV1().Pods(pvcRequestConfig.Namespace)

	pods, err := podClient.List(ctx, metaV1.ListOptions{
		LabelSelector: fmt.Sprintf("pvci.txn2.com/vol=%s", pvcRequestConfig.Name),
	})
	if err != nil {
		sr.InjectorHasError = true
		sr.InjectorError = err.Error()
	}

	if pods == nil || len(pods.Items) < 1 {
		sr.InjectorHasError = true
		sr.InjectorError = "no injectors found"
	}

	if pods != nil && len(pods.Items) > 0 {
		sr.InjectorState = fmt.Sprintf("%s", pods.Items[0].Status.Phase)
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
			a.Log.Warn("parsePVCRequestConfig aborted with error",
				zap.Int("code", http.StatusBadRequest),
				zap.String("reason", err.Error()))

			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "unable to read post body",
			})
			return
		}

		err = a.CreatePVC(*pvcRequestConfig)
		if err != nil {
			a.Log.Warn("CreatePVCHandler aborted with error",
				zap.Int("code", http.StatusBadRequest),
				zap.String("reason", err.Error()))

			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{})
	}
}

func (a *API) CreatePVCAsyncHandler() gin.HandlerFunc {
	return func(c *gin.Context) {

		pvcRequestConfig, err := a.parsePVCRequestConfig(c)
		if err != nil {
			a.Log.Warn("parsePVCRequestConfig aborted with error",
				zap.Int("code", http.StatusBadRequest),
				zap.String("reason", err.Error()))

			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "unable to read post body",
			})
			return
		}

		go func() {
			err = a.CreatePVC(*pvcRequestConfig)
			if err != nil {
				a.Log.Warn("CreatePVCHandler aborted with error",
					zap.Int("code", http.StatusBadRequest),
					zap.String("reason", err.Error()))
			}
		}()

		c.JSON(http.StatusOK, gin.H{})
	}
}

// CreatePVC is the core purpose of PVCI, to create PVCs and inject
// them with files. CreatePVC takes a PVCRequestConfig object and
// creates a Kubernetes PVC, followed by a Kubernetes Job used to
// populate it.
func (a *API) CreatePVC(pvcRequestConfig PVCRequestConfig) error {
	ctx := context.Background()
	api := a.Cs.CoreV1()

	// create a PersistentVolumeClaim sized for the bucket data
	pvcClient := api.PersistentVolumeClaims(pvcRequestConfig.Namespace)

	// does the PVC exist
	existingPVC, _ := pvcClient.Get(ctx, pvcRequestConfig.Name, metaV1.GetOptions{})
	if existingPVC != nil && existingPVC.Name != "" {
		a.Log.Info("Found existing PVC",
			zap.String("namespace", pvcRequestConfig.Namespace),
			zap.String("name", pvcRequestConfig.Name),
			zap.String("phase", fmt.Sprintf("%s", existingPVC.Status.Phase)),
		)

		return fmt.Errorf("found a %s PVC named %s", existingPVC.Status.Phase, pvcRequestConfig.Name)
	}

	// does the PVC exist
	existingSrcPVC, _ := pvcClient.Get(ctx, fmt.Sprintf("%s-src", pvcRequestConfig.Name), metaV1.GetOptions{})
	if existingSrcPVC != nil && existingSrcPVC.Name != "" {
		a.Log.Info("Found existing PVC",
			zap.String("namespace", pvcRequestConfig.Namespace),
			zap.String("name", existingSrcPVC.Name),
			zap.String("phase", fmt.Sprintf("%s", existingSrcPVC.Status.Phase)),
		)

		return fmt.Errorf("found a %s PVC named %s", existingSrcPVC.Status.Phase, existingSrcPVC.Name)
	}

	// get bucket size
	objCount, sz, err := a.GetSize(pvcRequestConfig)
	if err != nil {
		return err
	}

	// calculate run estimate
	runEst := sz / (int64(a.AvgMPS) * 1048576)

	// calculate timeouts at a slow 5mb/sec
	a.Log.Info("CreatePVC called",
		zap.Int64("object_count", objCount),
		zap.Int64("size", sz),
		zap.Int64("run_est", runEst),
		zap.Int("run_est_cfg_mps", a.AvgMPS),
		zap.String("name", pvcRequestConfig.Name),
		zap.String("namespace", pvcRequestConfig.Namespace),
		zap.String("bucket", pvcRequestConfig.S3Bucket),
		zap.String("prefix", pvcRequestConfig.S3Prefix),
		zap.String("s3_endpoint", pvcRequestConfig.S3Endpoint),
		zap.Any("vol_config", pvcRequestConfig.VolConfig),
	)

	volMode := coreV1.PersistentVolumeFilesystem

	// MiB/MB Conversion plus % overage for copy buffers and set
	// the copy buffer needed for moving objects.
	pctOver := 1 + (float64(a.VolumeOveragePercent) / 100)

	storageQtyBuffer := resource.Quantity{}
	storageQtyBuffer.Set(int64(math.Ceil((float64(sz) * 1.048576) * pctOver)))

	srcPVCName := fmt.Sprintf("%s-src", pvcRequestConfig.Name)

	// Create source PVC Spec
	srcPVCSpecification := coreV1.PersistentVolumeClaim{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      srcPVCName,
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
			},
			StorageClassName: &pvcRequestConfig.StorageClass,
			VolumeMode:       &volMode,
			Resources: coreV1.ResourceRequirements{
				Requests: coreV1.ResourceList{
					coreV1.ResourceStorage: storageQtyBuffer,
				},
			},
		},
	}

	a.Log.Info("Creating PVC",
		zap.String("name", srcPVCName),
		zap.String("namespace", srcPVCSpecification.Namespace))

	// Create source PVC Spec
	_, err = pvcClient.Create(ctx, &srcPVCSpecification, metaV1.CreateOptions{})
	if err != nil {
		return err
	}

	// rolling backoff check for proper PVC status
	err = a.checkPVC(pvcRequestConfig.Namespace, srcPVCName)
	if err != nil {
		a.Log.Error("checkPVC failed",
			zap.String("name", srcPVCName),
			zap.String("namespace", srcPVCSpecification.Namespace),
			zap.Error(err),
		)
		return err
	}

	// create a Job with MinIO client Pod attached to the new srcPVCSpecification
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

	jobName := fmt.Sprintf("%s-injector", pvcRequestConfig.Name)

	jobSpecification := batchV1.Job{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      jobName,
			Namespace: pvcRequestConfig.Namespace,
			Labels: map[string]string{
				"pvci.txn2.com/vol":     pvcRequestConfig.Name,
				"pvci.txn2.com/job":     "injector",
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
		Spec: batchV1.JobSpec{
			Template: coreV1.PodTemplateSpec{
				ObjectMeta: metaV1.ObjectMeta{
					Labels: map[string]string{
						"pvci.txn2.com/vol":     pvcRequestConfig.Name,
						"pvci.txn2.com/job":     "injector",
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
				Spec: coreV1.PodSpec{
					RestartPolicy: coreV1.RestartPolicyOnFailure,
					Volumes: []coreV1.Volume{
						{
							Name: "srcpvc",
							VolumeSource: coreV1.VolumeSource{
								PersistentVolumeClaim: &coreV1.PersistentVolumeClaimVolumeSource{
									ClaimName: srcPVCName,
									ReadOnly:  false,
								},
							},
						},
					},
					Containers: []coreV1.Container{
						{
							Name:  "mc",
							Image: a.MCImage,
							Command: []string{
								"mc",
								"cp",
								"-r",
								"objstore/" + objPath,
								"/srcpvc",
							},
							VolumeMounts: []coreV1.VolumeMount{
								{
									MountPath: "/srcpvc",
									Name:      "srcpvc",
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

	_, err = jobsClient.Create(ctx, &jobSpecification, metaV1.CreateOptions{})
	if err != nil {
		a.Log.Error("could not create job",
			zap.String("namespace", pvcRequestConfig.Namespace),
			zap.String("name", jobName),
			zap.Error(err),
		)

		// clean up on fail
		cleanErr := pvcClient.Delete(ctx, srcPVCName, metaV1.DeleteOptions{})
		if err != nil {
			a.Log.Error("could not delete pvc",
				zap.String("namespace", pvcRequestConfig.Namespace),
				zap.String("name", srcPVCName),
				zap.Error(cleanErr),
			)
		}

		return err
	}

	// check job status (up to 60 seconds)
	err = a.checkJob(pvcRequestConfig.Namespace, jobName, runEst)
	if err != nil {
		return err
	}

	// cleanup job
	err = jobsClient.Delete(ctx, jobName, metaV1.DeleteOptions{})
	if err != nil {
		a.Log.Error("unable to cleanup job",
			zap.String("namespace", pvcRequestConfig.Namespace),
			zap.String("name", jobName),
			zap.Error(err),
		)
	}

	// Create roxPVC from srcPVC
	pvcSpecification := coreV1.PersistentVolumeClaim{
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
			DataSource: &coreV1.TypedLocalObjectReference{
				Kind: "PersistentVolumeClaim",
				Name: srcPVCName,
			},
			AccessModes: []coreV1.PersistentVolumeAccessMode{
				"ReadOnlyMany",
			},
			StorageClassName: &pvcRequestConfig.StorageClass,
			VolumeMode:       &volMode,
			Resources: coreV1.ResourceRequirements{
				Requests: coreV1.ResourceList{
					coreV1.ResourceStorage: storageQtyBuffer,
				},
			},
		},
	}

	_, err = pvcClient.Create(ctx, &pvcSpecification, metaV1.CreateOptions{})
	if err != nil {
		// @TODO if error clean up src PVC
		a.Log.Error("unable to create PVC",
			zap.String("namespace", pvcRequestConfig.Namespace),
			zap.String("name", pvcRequestConfig.Name),
			zap.Error(err),
		)

		return err
	}

	// rolling backoff check for proper PVC status
	err = a.checkPVC(pvcRequestConfig.Namespace, srcPVCName)
	if err != nil {
		// @TODO if error clean up src PVC
		a.Log.Error("checkPVC failed",
			zap.String("name", srcPVCName),
			zap.String("namespace", srcPVCSpecification.Namespace),
			zap.Error(err),
		)

		return err
	}

	// delete srcPVC
	err = pvcClient.Delete(ctx, srcPVCName, metaV1.DeleteOptions{})
	if err != nil {
		a.Log.Error("unable to delete source PVC",
			zap.String("name", srcPVCName),
			zap.String("namespace", srcPVCSpecification.Namespace),
			zap.Error(err),
		)
	}

	// patch pvc to remove finalizers for deletion
	po := &PatchOperations{
		{
			Op:   "remove",
			Path: "/metadata/finalizers/0",
		},
	}

	poJson, _ := json.Marshal(po)

	_, err = pvcClient.Patch(ctx, srcPVCName, types.JSONPatchType, poJson, metaV1.PatchOptions{})
	if err != nil {
		a.Log.Error("unable to patch source PVC",
			zap.String("name", srcPVCName),
			zap.String("namespace", srcPVCSpecification.Namespace),
			zap.Error(err),
		)
	}

	return nil
}

const JobAttemptInterval = 5

// checkJob loops over a period for checking job status
func (a *API) checkJob(namespace string, name string, timeout int64) error {
	attempt := 0
	maxAttempts := 1

	// add 50 percent to overhead
	maxTime := float64(timeout) + (float64(timeout) * .5)
	if maxTime > JobAttemptInterval {
		maxAttempts = int(math.Ceil(maxTime / JobAttemptInterval))
	}

	if maxAttempts < 6 {
		maxAttempts = 6
	}

	for {
		time.Sleep(time.Duration(JobAttemptInterval) * time.Second)

		if attempt > maxAttempts {
			a.Log.Error("job is unable to complete in allotted time",
				zap.String("name", name),
				zap.String("namespace", namespace),
			)
			return fmt.Errorf("job is unable to complete in allotted time")
		}

		job, err := a.getJob(namespace, name)
		if err != nil {
			return err
		}

		a.Log.Info("Job status",
			zap.String("name", name),
			zap.String("namespace", namespace),
			zap.Int32("active", job.Status.Active),
			zap.Int32("succeeded", job.Status.Succeeded),
			zap.Int32("failed", job.Status.Failed),
			zap.Int("check_attempt", attempt),
			zap.Int("max_attempts", maxAttempts),
			zap.Int("attempt_interval", JobAttemptInterval),
		)

		if job.Status.Failed > 0 {
			return fmt.Errorf("job failed")
		}

		if job.Status.Succeeded > 0 {
			return nil
		}

		attempt += 1
	}
}

func (a *API) getJob(namespace string, name string) (*batchV1.Job, error) {
	ctx := context.Background()

	// check status with rolling backoff
	job, err := a.Cs.BatchV1().Jobs(namespace).Get(ctx, name, metaV1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return job, nil
}

func (a *API) checkPVC(namespace string, name string) error {
	attempt := 0
	retrySecs := []int{1, 2, 2, 4, 4, 4, 8, 8, 8, 8, 8}
	//var srcPVC *coreV1.PersistentVolumeClaim
	for {
		if attempt > len(retrySecs)-1 {
			a.Log.Error("requested PVC is unable to reach Bound phase",
				zap.String("name", name),
				zap.String("namespace", namespace),
			)
			return fmt.Errorf("requested PVC is unable to reach Bound phase")
		}

		time.Sleep(time.Duration(retrySecs[attempt]) * time.Second)

		srcPVC, err := a.getPVC(namespace, name)
		if err != nil {
			return err
		}

		a.Log.Info("PVC status phase",
			zap.String("name", name),
			zap.String("namespace", namespace),
			zap.Any("status", srcPVC.Status.Phase))
		if srcPVC.Status.Phase == coreV1.ClaimBound {
			return nil
		}

		attempt += 1
	}
}

func (a *API) getPVC(namespace string, name string) (*coreV1.PersistentVolumeClaim, error) {
	ctx := context.Background()

	// check status with rolling backoff
	srcPVC, err := a.Cs.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metaV1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return srcPVC, nil
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
