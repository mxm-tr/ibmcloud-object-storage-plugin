/*******************************************************************************
 * IBM Confidential
 * OCO Source Materials
 * IBM Cloud Container Service, 5737-D43
 * (C) Copyright IBM Corp. 2017, 2018 All Rights Reserved.
 * The source code for this program is not  published or otherwise divested of
 * its trade secrets, irrespective of what has been deposited with
 * the U.S. Copyright Office.
 ******************************************************************************/

package provisioner

import (
	"errors"
	"fmt"
	"github.com/IBM/ibmcloud-object-storage-plugin/driver"
	"github.com/IBM/ibmcloud-object-storage-plugin/utils/backend"
	"github.com/IBM/ibmcloud-object-storage-plugin/utils/logger"
	"github.com/IBM/ibmcloud-object-storage-plugin/utils/parser"
	"github.com/IBM/ibmcloud-object-storage-plugin/utils/uuid"
	"github.com/kubernetes-incubator/external-storage/lib/controller"
	"go.uber.org/zap"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"os"
	"strconv"
	"strings"
)

// PVC annotations
type pvcAnnotations struct {
	AutoCreateBucket       bool   `json:"ibm.io/auto-create-bucket,string"`
	AutoDeleteBucket       bool   `json:"ibm.io/auto-delete-bucket,string"`
	Bucket                 string `json:"ibm.io/bucket"`
	ObjectPath             string `json:"ibm.io/object-path,omitempty"`
	Endpoint               string `json:"ibm.io/endpoint,omitempty"` //Will be deprecated
	Region                 string `json:"ibm.io/region,omitempty"`   //Will be deprecated
	SecretName             string `json:"ibm.io/secret-name"`
	ChunkSizeMB            string `json:"ibm.io/chunk-size-mb,omitempty"`
	ParallelCount          string `json:"ibm.io/parallel-count,omitempty"`
	MultiReqMax            string `json:"ibm.io/multireq-max,omitempty"`
	StatCacheSize          string `json:"ibm.io/stat-cache-size,omitempty"`
	S3FSFUSERetryCount     string `json:"ibm.io/s3fs-fuse-retry-count,omitempty"`
	StatCacheExpireSeconds string `json:"ibm.io/stat-cache-expire-seconds,omitempty"`
	IAMEndpoint            string `json:"ibm.io/iam-endpoint,omitempty"`
	ValidateBucket         string `json:"ibm.io/validate-bucket,omitempty"`
}

// PV annotations
type pvAnnotations struct {
	pvcAnnotations
	SecretNamespace string `json:"ibm.io/secret-namespace"`
}

// Storage Class options
type scOptions struct {
	ChunkSizeMB        int    `json:"ibm.io/chunk-size-mb,string"`
	ParallelCount      int    `json:"ibm.io/parallel-count,string"`
	MultiReqMax        int    `json:"ibm.io/multireq-max,string"`
	StatCacheSize      int    `json:"ibm.io/stat-cache-size,string"`
	TLSCipherSuite     string `json:"ibm.io/tls-cipher-suite,omitempty"`
	DebugLevel         string `json:"ibm.io/debug-level"`
	CurlDebug          bool   `json:"ibm.io/curl-debug,string,omitempty"`
	KernelCache        bool   `json:"ibm.io/kernel-cache,string,omitempty"`
	S3FSFUSERetryCount int    `json:"ibm.io/s3fs-fuse-retry-count,string,omitempty"`
	IAMEndpoint        string `json:"ibm.io/iam-endpoint,omitempty"`
	OSEndpoint         string `json:"ibm.io/object-store-endpoint,omitempty"`
	OSStorageClass     string `json:"ibm.io/object-store-storage-class,omitempty"`
}

const (
	driverName           = "ibm/ibmc-s3fs"
	autoBucketNamePrefix = "tmp-s3fs-"
	fsType               = ""
)

// IBMS3fsProvisioner is a dynamic provisioner of persistent volumes backed by Object Storage via s3fs
type IBMS3fsProvisioner struct {
	// Backend is the object store session factory
	Backend backend.ObjectStorageSessionFactory
	// Logger will be used for logging
	Logger *zap.Logger
	// Client is the Kubernetes Go-Client that will be used to fetch user credentials
	Client kubernetes.Interface
	// UUIDGenerator is a UUID generator that will be used to generate bucket names
	UUIDGenerator uuid.Generator
}

var _ controller.Provisioner = &IBMS3fsProvisioner{}

func parseSecret(secret *v1.Secret, keyName string) (string, error) {
	bytesVal, ok := secret.Data[keyName]
	if !ok {
		return "", fmt.Errorf("%s secret missing", keyName)
	}

	return string(bytesVal), nil
}

func (p *IBMS3fsProvisioner) getCredentials(secretName, secretNamespace string) (*backend.ObjectStorageCredentials, error) {
	secrets, err := p.Client.Core().Secrets(secretNamespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("cannot get secret %s: %v", secretName, err)
	}

	var accessKey, secretKey, apiKey, serviceInstanceID string

	apiKey, err = parseSecret(secrets, driver.SecretAPIKey)
	if err != nil {
		accessKey, err = parseSecret(secrets, driver.SecretAccessKey)
		if err != nil {
			return nil, err
		}

		secretKey, err = parseSecret(secrets, driver.SecretSecretKey)
		if err != nil {
			return nil, err
		}
	} else {
		serviceInstanceID, err = parseSecret(secrets, driver.SecretServiceInstanceID)
	}

	return &backend.ObjectStorageCredentials{
		AccessKey:         accessKey,
		SecretKey:         secretKey,
		APIKey:            apiKey,
		ServiceInstanceID: serviceInstanceID,
	}, nil

}

// Provision provisions a new persistent volume
func (p *IBMS3fsProvisioner) Provision(options controller.VolumeOptions) (*v1.PersistentVolume, error) {
	var pvc pvcAnnotations
	var sc scOptions
	var pvcName = options.PVC.Name
	var clusterID = os.Getenv("CLUSTER_ID")
	var msg string
	var valBucket = true
	var creds *backend.ObjectStorageCredentials
	var sess backend.ObjectStorageSession

	contextLogger, _ := logger.GetZapDefaultContextLogger()
	contextLogger.Info(pvcName + ":" + clusterID + ":Provisioning storage with these spec")
	contextLogger.Info(pvcName+":"+clusterID+":PVC Details: ", zap.String("pvc", options.PVName))

	err := parser.UnmarshalMap(&options.PVC.Annotations, &pvc)
	if err != nil {
		return nil, fmt.Errorf(pvcName+":"+clusterID+":cannot unmarshal PVC annotations: %v", err)
	}
	err = parser.UnmarshalMap(&options.Parameters, &sc)
	if err != nil {
		return nil, fmt.Errorf(pvcName+":"+clusterID+":cannot unmarshal storage class parameters: %v", err)
	}

	//Override value of EndPoint defined in storageclass
	// EndPoint should be defined in storage class.
	if pvc.Endpoint != "" {
		sc.OSEndpoint = pvc.Endpoint
	}

	//Override value of OSStorageClass defined in storageclass.
	// pvc Region will be deprecated.
	if pvc.Region != "" {
		sc.OSStorageClass = pvc.Region
	}

	if !(strings.HasPrefix(sc.OSEndpoint, "https://") || strings.HasPrefix(sc.OSEndpoint, "http://")) {
		return nil, fmt.Errorf(pvcName+":"+clusterID+
			":Bad value for ibm.io/object-store-endpoint \"%v\": scheme is missing. "+
			"Must be of the form http://<hostname> or https://<hostname>",
			sc.OSEndpoint)
	}

	if pvc.IAMEndpoint != "" {
		sc.IAMEndpoint = pvc.IAMEndpoint
	}

	if !(strings.HasPrefix(sc.IAMEndpoint, "https://") || strings.HasPrefix(sc.IAMEndpoint, "http://")) {
		return nil, fmt.Errorf(pvcName+":"+clusterID+
			":Bad value for ibm.io/iam-endpoint \"%v\":"+
			" Must be of the form https://<hostname> or http://<hostname>",
			sc.IAMEndpoint)
	}

	//Override value of s3fs-fuse-retry-count defined in storageclass
	if pvc.S3FSFUSERetryCount != "" {
		if sc.S3FSFUSERetryCount, err = strconv.Atoi(pvc.S3FSFUSERetryCount); err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":Cannot convert value of s3fs-fuse-retry-count into integer: %v", err)
		}
	}

	//Override value of chunk-size-mb defined in storageclass
	if pvc.ChunkSizeMB != "" {
		if sc.ChunkSizeMB, err = strconv.Atoi(pvc.ChunkSizeMB); err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":Cannot convert value of chunk-size-mb into integer: %v", err)
		}
	}

	//Override value of parallel-count defined in storageclass
	if pvc.ParallelCount != "" {
		if sc.ParallelCount, err = strconv.Atoi(pvc.ParallelCount); err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":Cannot convert value of parallel-count into integer: %v", err)
		}
	}

	//Override value of multireq-max defined in storageclass
	if pvc.MultiReqMax != "" {
		if sc.MultiReqMax, err = strconv.Atoi(pvc.MultiReqMax); err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":Cannot convert value of multireq-max into integer: %v", err)
		}
	}

	//Override value of stat-cache-size defined in storageclass
	if pvc.StatCacheSize != "" {
		if sc.StatCacheSize, err = strconv.Atoi(pvc.StatCacheSize); err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":Cannot convert value of stat-cache-size into integer: %v", err)
		}
	}

	//Check if value of stat-cache-expire-seconds parameter can be converted to integer
	if pvc.StatCacheExpireSeconds != "" {
		cacheExpireSeconds, err := strconv.Atoi(pvc.StatCacheExpireSeconds)
		if err != nil {
			return nil, fmt.Errorf(pvcName+":Cannot convert value of stat-cache-expire-seconds into integer: %v", err)
		} else if cacheExpireSeconds < 0 {
			return nil, fmt.Errorf(pvcName + ":value of stat-cache-expire-seconds should be >= 0")
		}
	}

	if pvc.AutoCreateBucket && pvc.ObjectPath != "" {
		return nil, fmt.Errorf(pvcName+":"+clusterID+":object-path cannot be set when auto-create is enabled, got: %s", pvc.ObjectPath)
	}

	if pvc.AutoDeleteBucket {
		if !pvc.AutoCreateBucket {
			return nil, errors.New(pvcName + ":" + clusterID + ":bucket auto-create must be enabled when bucket auto-delete is enabled")
		}

		if pvc.Bucket != "" {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":bucket cannot be set when auto-delete is enabled, got: %s", pvc.Bucket)
		}

		id, err := p.UUIDGenerator.New()
		if err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":cannot create UUID for bucket name: %v", err)
		}

		pvc.Bucket = autoBucketNamePrefix + id
	} else if pvc.Bucket == "" {
		return nil, errors.New(pvcName + ":" + clusterID + ":bucket name not specified")
	}

	if pvc.ValidateBucket == "no" && !pvc.AutoCreateBucket {
		valBucket = false
	} else {
		valBucket = true
	}

	//var err_msg error
	if valBucket {
		creds, err = p.getCredentials(pvc.SecretName, options.PVC.Namespace)
		if err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":cannot get credentials: %v", err)
		}

		creds.IAMEndpoint = sc.IAMEndpoint
		sess = p.Backend.NewObjectStorageSession(sc.OSEndpoint, sc.OSStorageClass, creds, p.Logger)
	}

	if pvc.AutoCreateBucket {
		if creds.APIKey != "" && creds.ServiceInstanceID == "" {
			return nil, errors.New(pvcName + ":" + clusterID + ":cannot create bucket using API key without service-instance-id")
		}
		msg, err = sess.CreateBucket(pvc.Bucket)
		if msg != "" {
			contextLogger.Info(pvcName + ":" + clusterID + ":" + msg)
		}
		if err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":cannot create bucket %s: %v", pvc.Bucket, err)
		}
	}

	if valBucket {
		err = sess.CheckBucketAccess(pvc.Bucket)
		if err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":cannot access bucket %s: %v", pvc.Bucket, err)
		}
	}

	if pvc.ObjectPath != "" {
		exist, err := sess.CheckObjectPathExistence(pvc.Bucket, pvc.ObjectPath)
		if err != nil {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":cannot access object-path \"%s\" inside bucket %s: %v", pvc.ObjectPath, pvc.Bucket, err)
		} else if !exist {
			return nil, fmt.Errorf(pvcName+":"+clusterID+":object-path \"%s\" not found inside bucket %s", pvc.ObjectPath, pvc.Bucket)
		}
	}

	driverOptions, err := parser.MarshalToMap(&driver.Options{
		ChunkSizeMB:            sc.ChunkSizeMB,
		ParallelCount:          sc.ParallelCount,
		MultiReqMax:            sc.MultiReqMax,
		StatCacheSize:          sc.StatCacheSize,
		TLSCipherSuite:         sc.TLSCipherSuite,
		CurlDebug:              sc.CurlDebug,
		KernelCache:            sc.KernelCache,
		DebugLevel:             sc.DebugLevel,
		S3FSFUSERetryCount:     strconv.Itoa(sc.S3FSFUSERetryCount),
		StatCacheExpireSeconds: pvc.StatCacheExpireSeconds,
		IAMEndpoint:            sc.IAMEndpoint,
		OSEndpoint:             sc.OSEndpoint,
		OSStorageClass:         sc.OSStorageClass,
		Bucket:                 pvc.Bucket,
		ObjectPath:             pvc.ObjectPath,
	})
	if err != nil {
		return nil, fmt.Errorf(pvcName+":"+clusterID+":cannot marshal driver options: %v", err)
	}

	pvAnnots, err := parser.MarshalToMap(&pvAnnotations{
		pvcAnnotations:  pvc,
		SecretNamespace: options.PVC.Namespace,
	})
	if err != nil {
		return nil, fmt.Errorf(pvcName+":"+clusterID+":cannot marshal pv options: %v", err)
	}

	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        options.PVName,
			Annotations: pvAnnots,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: options.PersistentVolumeReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceStorage: options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				FlexVolume: &v1.FlexPersistentVolumeSource{
					Driver:    driverName,
					FSType:    fsType,
					SecretRef: &v1.SecretReference{Name: pvc.SecretName},
					ReadOnly:  false,
					Options:   driverOptions,
				},
			},
		},
	}, nil
}

// Delete deletes a persistent volume
func (p *IBMS3fsProvisioner) Delete(pv *v1.PersistentVolume) error {
	var pvAnnots pvAnnotations

	contextLogger, _ := logger.GetZapDefaultContextLogger()
	contextLogger.Info("Deleting the pvc..")

	endpointValue := pv.Spec.PersistentVolumeSource.FlexVolume.Options["object-store-endpoint"]
	regionValue := pv.Spec.PersistentVolumeSource.FlexVolume.Options["object-store-storage-class"]
	iamEndpoint := pv.Spec.PersistentVolumeSource.FlexVolume.Options["iam-endpoint"]

	err := parser.UnmarshalMap(&pv.Annotations, &pvAnnots)
	if err != nil {
		return fmt.Errorf("cannot unmarshal PV annotations: %v", err)
	}

	if pvAnnots.AutoDeleteBucket {
		err = p.deleteBucket(&pvAnnots, endpointValue, regionValue, iamEndpoint)
		if err != nil {
			return fmt.Errorf("cannot delete bucket: %v", err)
		}
	}

	return nil
}

func (p *IBMS3fsProvisioner) deleteBucket(pvAnnots *pvAnnotations, endpointValue, regionValue, iamEndpoint string) error {
	creds, err := p.getCredentials(pvAnnots.SecretName, pvAnnots.SecretNamespace)
	if err != nil {
		return fmt.Errorf("cannot get credentials: %v", err)
	}
	creds.IAMEndpoint = iamEndpoint
	sess := p.Backend.NewObjectStorageSession(endpointValue, regionValue, creds, p.Logger)

	return sess.DeleteBucket(pvAnnots.Bucket)
}