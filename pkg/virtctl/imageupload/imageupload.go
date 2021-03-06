/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

package imageupload

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	pb "gopkg.in/cheggaaa/pb.v1"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"kubevirt.io/client-go/kubecli"
	cdiv1 "kubevirt.io/containerized-data-importer/pkg/apis/core/v1alpha1"
	uploadcdiv1 "kubevirt.io/containerized-data-importer/pkg/apis/upload/v1alpha1"
	cdiClientset "kubevirt.io/containerized-data-importer/pkg/client/clientset/versioned"
	"kubevirt.io/kubevirt/pkg/virtctl/templates"
)

const (
	// PodPhaseAnnotation is the annotation on a PVC containing the upload pod phase
	PodPhaseAnnotation = "cdi.kubevirt.io/storage.pod.phase"

	// PodReadyAnnotation tells whether the uploadserver pod is ready
	PodReadyAnnotation = "cdi.kubevirt.io/storage.pod.ready"

	uploadRequestAnnotation = "cdi.kubevirt.io/storage.upload.target"

	uploadReadyWaitInterval = 2 * time.Second

	//UploadProxyURI is a URI of the upoad proxy
	UploadProxyURI = "/v1alpha1/upload"

	configName = "config"
)

var (
	insecure       bool
	uploadProxyURL string
	name           string
	size           string
	pvcSize        string
	storageClass   string
	imagePath      string
	accessMode     string

	uploadPodWaitSecs uint
	blockVolume       bool
	noCreate          bool
)

// HTTPClientCreator is a function that creates http clients
type HTTPClientCreator func(bool) *http.Client

var httpClientCreatorFunc HTTPClientCreator

// SetHTTPClientCreator allows overriding the default http client
// useful for unit tests
func SetHTTPClientCreator(f HTTPClientCreator) {
	httpClientCreatorFunc = f
}

// SetDefaultHTTPClientCreator sets the http client creator back to default
func SetDefaultHTTPClientCreator() {
	httpClientCreatorFunc = getHTTPClient
}

func init() {
	SetDefaultHTTPClientCreator()
}

// NewImageUploadCommand returns a cobra.Command for handling the the uploading of VM images
func NewImageUploadCommand(clientConfig clientcmd.ClientConfig) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "image-upload",
		Short:   "Upload a VM image to a PersistentVolumeClaim.",
		Example: usage(),
		Args:    cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			v := command{clientConfig: clientConfig}
			return v.run(cmd, args)
		},
	}
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Allow insecure server connections when using HTTPS.")
	cmd.Flags().StringVar(&uploadProxyURL, "uploadproxy-url", "", "The URL of the cdi-upload proxy service.")
	cmd.Flags().StringVar(&name, "pvc-name", "", "DEPRECATED - The destination DataVolume/PVC name.")
	cmd.Flags().StringVar(&pvcSize, "pvc-size", "", "DEPRECATED - The size of the PVC to create (ex. 10Gi, 500Mi).")
	cmd.Flags().StringVar(&size, "size", "", "The size of the DataVolume to create (ex. 10Gi, 500Mi).")
	cmd.Flags().StringVar(&storageClass, "storage-class", "", "The storage class for the PVC.")
	cmd.Flags().StringVar(&accessMode, "access-mode", "ReadWriteOnce", "The access mode for the PVC.")
	cmd.Flags().BoolVar(&blockVolume, "block-volume", false, "Create a PVC with VolumeMode=Block (default Filesystem).")
	cmd.Flags().StringVar(&imagePath, "image-path", "", "Path to the local VM image.")
	cmd.MarkFlagRequired("image-path")
	cmd.Flags().BoolVar(&noCreate, "no-create", false, "Don't attempt to create a new DataVolume/PVC.")
	cmd.Flags().UintVar(&uploadPodWaitSecs, "wait-secs", 60, "Seconds to wait for upload pod to start.")
	cmd.SetUsageTemplate(templates.UsageTemplate())
	return cmd
}

func usage() string {
	usage := `  # Upload a local disk image to a newly created DataVolume:
  {{ProgramName}} image-upload dv dv-name --size=10Gi --image-path=/images/fedora30.qcow2

  # Upload a local disk image to an exististing DataVolume
  {{ProgramName}} image-upload dv dv-name --no-create --image-path=/images/fedora30.qcow2

  # Upload a local disk image to an exististing PersistentVolumeClaim
  {{ProgramName}} image-upload pvc pvc-name --image-path=/images/fedora30.qcow2

  # Upload to a DataVolume with explicit URL to CDI Upload Proxy
  {{ProgramName}} image-upload dv dv-name --uploadproxy-url=https://cdi-uploadproxy.mycluster.com --image-path=/images/fedora30.qcow2`
	return usage
}

type command struct {
	clientConfig clientcmd.ClientConfig
}

func parseArgs(args []string) error {
	if len(size) > 0 && len(pvcSize) > 0 && size != pvcSize {
		return fmt.Errorf("--pvc-size deprecated, use --size")
	}

	if len(pvcSize) > 0 {
		size = pvcSize
	}

	// check deprecated invocation
	if name != "" {
		if len(args) != 0 {
			return fmt.Errorf("cannot use --pvc-name and args")
		}

		return nil
	}

	if len(args) != 2 {
		return fmt.Errorf("expecting two args")
	}

	switch strings.ToLower(args[0]) {
	case "dv":
	case "pvc":
		noCreate = true
	default:
		return fmt.Errorf("invalid resource type %s", args[0])
	}

	name = args[1]

	return nil
}

func (c *command) run(cmd *cobra.Command, args []string) error {
	if err := parseArgs(args); err != nil {
		return err
	}

	file, err := os.Open(imagePath)
	if err != nil {
		return err
	}
	defer file.Close()

	namespace, _, err := c.clientConfig.Namespace()
	if err != nil {
		return err
	}

	virtClient, err := kubecli.GetKubevirtClientFromClientConfig(c.clientConfig)
	if err != nil {
		return fmt.Errorf("cannot obtain KubeVirt client: %v", err)
	}

	pvc, err := getAndValidateUploadPVC(virtClient, namespace, name, noCreate)
	if err != nil {
		if !(k8serrors.IsNotFound(err) && !noCreate) {
			return err
		}

		if !noCreate && len(size) == 0 {
			return fmt.Errorf("when creating DataVolume, the size must be specified")
		}

		dv, err := createUploadDataVolume(virtClient, namespace, name, size, storageClass, accessMode, blockVolume)
		if err != nil {
			return err
		}

		fmt.Printf("DataVolume %s/%s created\n", dv.Namespace, dv.Name)
	} else {
		pvc, err = ensurePVCSupportsUpload(virtClient, pvc)
		if err != nil {
			return err
		}

		fmt.Printf("Using existing PVC %s/%s\n", namespace, pvc.Name)
	}

	err = waitUploadServerReady(virtClient, namespace, name, uploadReadyWaitInterval, time.Duration(uploadPodWaitSecs)*time.Second)
	if err != nil {
		return err
	}

	if uploadProxyURL == "" {
		uploadProxyURL, err = getUploadProxyURL(virtClient.CdiClient())
		if err != nil {
			return err
		}
		if uploadProxyURL == "" {
			return fmt.Errorf("uploadproxy URL not found")
		}
	}

	u, err := url.Parse(uploadProxyURL)
	if err != nil {
		return err
	}

	if u.Scheme == "" {
		uploadProxyURL = fmt.Sprintf("https://%s", uploadProxyURL)
	}

	fmt.Printf("Uploading data to %s\n", uploadProxyURL)

	token, err := getUploadToken(virtClient.CdiClient(), namespace, name)
	if err != nil {
		return err
	}

	err = uploadData(uploadProxyURL, token, file, insecure)
	if err != nil {
		return err
	}

	fmt.Printf("Uploading %s completed successfully\n", imagePath)

	return nil
}

func getHTTPClient(insecure bool) *http.Client {
	client := &http.Client{}

	if insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	return client
}

//ConstructUploadProxyPath - receives uploadproxy adress and concatenates to it URI
func ConstructUploadProxyPath(uploadProxyURL string) (string, error) {
	u, err := url.Parse(uploadProxyURL)

	if err != nil {
		return "", err
	}

	if !strings.Contains(uploadProxyURL, UploadProxyURI) {
		u.Path = path.Join(u.Path, UploadProxyURI)
	}

	return u.String(), nil
}

func uploadData(uploadProxyURL, token string, file *os.File, insecure bool) error {
	url, err := ConstructUploadProxyPath(uploadProxyURL)
	if err != nil {
		return err
	}

	fi, err := file.Stat()
	if err != nil {
		return err
	}

	bar := pb.New64(fi.Size()).SetUnits(pb.U_BYTES)
	reader := bar.NewProxyReader(file)

	client := httpClientCreatorFunc(insecure)
	req, _ := http.NewRequest("POST", url, reader)

	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Content-Type", "application/octet-stream")
	req.ContentLength = fi.Size()

	fmt.Println()
	bar.Start()

	resp, err := client.Do(req)

	bar.Finish()
	fmt.Println()

	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected return value %d", resp.StatusCode)
	}

	return nil
}

func getUploadToken(client cdiClientset.Interface, namespace, name string) (string, error) {
	request := &uploadcdiv1.UploadTokenRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: "token-for-virtctl",
		},
		Spec: uploadcdiv1.UploadTokenRequestSpec{
			PvcName: name,
		},
	}

	response, err := client.UploadV1alpha1().UploadTokenRequests(namespace).Create(request)
	if err != nil {
		return "", err
	}

	return response.Status.Token, nil
}

func waitUploadServerReady(client kubernetes.Interface, namespace, name string, interval, timeout time.Duration) error {
	loggedStatus := false

	err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			// DataVolume controller may not have created the PVC yet
			if k8serrors.IsNotFound(err) {
				return false, nil
			}

			return false, err
		}

		// upload controler sets this to true when uploadserver pod is ready to receive data
		podReady := pvc.Annotations[PodReadyAnnotation]
		done, _ := strconv.ParseBool(podReady)

		if !done && !loggedStatus {
			fmt.Printf("Waiting for PVC %s upload pod to be ready...\n", name)
			loggedStatus = true
		}

		if done && loggedStatus {
			fmt.Printf("Pod now ready\n")
		}

		return done, nil
	})

	return err
}

func createUploadDataVolume(client kubecli.KubevirtClient, namespace, name, size, storageClass, accessMode string, blockVolume bool) (*cdiv1.DataVolume, error) {
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("validation failed for size=%s: %s", size, err)
	}

	dv := &cdiv1.DataVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: cdiv1.DataVolumeSpec{
			Source: cdiv1.DataVolumeSource{
				Upload: &cdiv1.DataVolumeSourceUpload{},
			},
			PVC: &v1.PersistentVolumeClaimSpec{
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceStorage: quantity,
					},
				},
			},
		},
	}

	if storageClass != "" {
		dv.Spec.PVC.StorageClassName = &storageClass
	}

	if accessMode != "" {
		dv.Spec.PVC.AccessModes = []v1.PersistentVolumeAccessMode{v1.PersistentVolumeAccessMode(accessMode)}
	}

	if blockVolume {
		volMode := v1.PersistentVolumeBlock
		dv.Spec.PVC.VolumeMode = &volMode
	}

	dv, err = client.CdiClient().CdiV1alpha1().DataVolumes(namespace).Create(dv)
	if err != nil {
		return nil, err
	}

	return dv, nil
}

func ensurePVCSupportsUpload(client kubernetes.Interface, pvc *v1.PersistentVolumeClaim) (*v1.PersistentVolumeClaim, error) {
	var err error
	_, hasAnnotation := pvc.Annotations[uploadRequestAnnotation]

	if !hasAnnotation {
		pvc.Annotations[uploadRequestAnnotation] = ""
		pvc, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Update(pvc)
		if err != nil {
			return nil, err
		}
	}

	return pvc, nil
}

func getAndValidateUploadPVC(client kubernetes.Interface, namespace, name string, shouldExist bool) (*v1.PersistentVolumeClaim, error) {
	pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// for PVCs that exist, we ony want to use them if
	// 1. They have not already been used AND EITHER
	//   a. shouldExist is true
	//   b. shouldExist is false AND the upload annotation exists

	_, isUploadPVC := pvc.Annotations[uploadRequestAnnotation]
	podPhase := pvc.Annotations[PodPhaseAnnotation]

	if podPhase == string(v1.PodSucceeded) {
		return nil, fmt.Errorf("PVC %s already successfully imported/cloned/updated", name)
	}

	if !shouldExist && !isUploadPVC {
		return nil, fmt.Errorf("PVC %s not available for upload", name)
	}

	return pvc, nil
}

func getUploadProxyURL(client cdiClientset.Interface) (string, error) {
	cdiConfig, err := client.CdiV1alpha1().CDIConfigs().Get(configName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if cdiConfig.Spec.UploadProxyURLOverride != nil {
		return *cdiConfig.Spec.UploadProxyURLOverride, nil
	}
	if cdiConfig.Status.UploadProxyURL != nil {
		return *cdiConfig.Status.UploadProxyURL, nil
	}
	return "", nil
}
