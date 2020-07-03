package pvci

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	minio "github.com/minio/minio-go/v6"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PVCRequestConfig
type PVCRequestConfig struct {
	S3Endpoint string `json:"s3_endpoint"`
	S3SSL      bool   `json:"s3_ssl"`
	S3Bucket   string `json:"s3_bucket"`
	S3Prefix   string `json:"s3_prefix"`
	S3Key      string `json:"s3_key"`
	S3Secret   string `json:"s3_secret"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

// Config configures the score API
type Config struct {
	Log *zap.Logger
	Cs  *kubernetes.Clientset
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

// create pv
func (a *Api) CreatePVC(pvcRequestConfig PVCRequestConfig) error {

	// get bucket size
	_, sz, err := a.GetSize(pvcRequestConfig)
	if err != nil {
		return err
	}

	api := a.Cs.CoreV1()

	// create a PersistentVolumeClaim sized for the bucket data
	pvcClient := api.PersistentVolumeClaims(pvcRequestConfig.Namespace)
	volMode := v1.PersistentVolumeFilesystem
	storageQty := resource.Quantity{}
	storageQty.Set(sz)

	storageClass := "rook-ceph-block"

	pvc := v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcRequestConfig.Name,
			Namespace: pvcRequestConfig.Namespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{
				"ReadWriteOnce",
			},
			StorageClassName: &storageClass,
			VolumeMode:       &volMode,
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: storageQty,
				},
			},
		},
	}

	_, err = pvcClient.Create(&pvc)
	if err != nil {
		return err
	}

	// create a Pod attached to the new pvc
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
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					Volumes: []v1.Volume{
						{
							Name: "attached-pvc",
							VolumeSource: v1.VolumeSource{
								PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcRequestConfig.Name,
									ReadOnly:  false,
								},
							},
						},
					},
					Containers: []v1.Container{
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
							VolumeMounts: []v1.VolumeMount{
								{
									MountPath: "/data",
									Name:      "attached-pvc",
								},
							},
							Env: []v1.EnvVar{
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

	_, err = jobsClient.Create(&job)
	if err != nil {
		a.Log.Warn("cloud not create job",
			zap.String("name", pvcRequestConfig.Name),
			zap.Error(err),
		)

		// clean up pvc
		err := pvcClient.Delete(pvcRequestConfig.Name, &metav1.DeleteOptions{})
		if err != nil {
			a.Log.Warn("cloud not delete pvc",
				zap.String("name", pvcRequestConfig.Name),
				zap.Error(err),
			)
			return err
		}
		return err
	}

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
