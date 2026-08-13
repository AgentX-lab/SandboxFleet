//go:build e2e

package framework

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	minioAccessKey = "minioadmin"
	minioSecretKey = "minioadmin"
	minioBucket    = "sandboxfleet-snapshots"
)

// EnsureMinIO deploys MinIO + credentials Secret in namespace and creates the snapshot bucket.
func (c *Context) EnsureMinIO(ctx context.Context, namespace string) {
	c.T.Helper()
	root, err := repoRoot()
	if err != nil {
		c.T.Fatalf("find repo root: %v", err)
	}
	c.ApplyManifest(ctx, filepath.Join(root, "test", "e2e", "testdata", "minio.yaml"), map[string]string{
		"NAMESPACE": namespace,
	})
	c.waitMinIOReady(ctx, namespace)
	c.ensureMinIOBucket(ctx, namespace)
}

func (c *Context) waitMinIOReady(ctx context.Context, namespace string) {
	c.T.Helper()
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		var dep appsv1.Deployment
		if err := c.K8s.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "minio"}, &dep); err != nil {
			return false, err
		}
		return dep.Status.ReadyReplicas >= 1, nil
	})
	if err != nil {
		c.T.Fatalf("wait MinIO ready in %s: %v", namespace, err)
	}
}

func (c *Context) ensureMinIOBucket(ctx context.Context, namespace string) {
	c.T.Helper()
	var pods corev1.PodList
	if err := c.K8s.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app": "minio"}); err != nil {
		c.T.Fatalf("list MinIO pods: %v", err)
	}
	var podName string
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			podName = pods.Items[i].Name
			break
		}
	}
	if podName == "" {
		c.T.Fatalf("no running MinIO Pod in %s", namespace)
	}

	baseURL, stop, err := portForward(ctx, c.RestConfig, namespace, podName, 9000)
	if err != nil {
		c.T.Fatalf("port-forward MinIO: %v", err)
	}
	defer stop()

	s3c := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(minioAccessKey, minioSecretKey, ""),
		BaseEndpoint: aws.String(baseURL),
		UsePathStyle: true,
	})
	_, err = s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(minioBucket)})
	if err != nil {
		_, headErr := s3c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(minioBucket)})
		if headErr != nil {
			c.T.Fatalf("create MinIO bucket %q: %v (head: %v)", minioBucket, err, headErr)
		}
	}
	c.T.Logf("MinIO bucket %q ready via %s", minioBucket, baseURL)
}

// SnapshotBucket returns the e2e MinIO bucket name.
func SnapshotBucket() string { return minioBucket }

// MinIOObjectCount lists objects under prefix (host-side via port-forward).
func (c *Context) MinIOObjectCount(ctx context.Context, namespace, prefix string) int {
	c.T.Helper()
	var pods corev1.PodList
	if err := c.K8s.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app": "minio"}); err != nil {
		c.T.Fatalf("list MinIO pods: %v", err)
	}
	var podName string
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			podName = pods.Items[i].Name
			break
		}
	}
	if podName == "" {
		c.T.Fatalf("no running MinIO Pod in %s", namespace)
	}
	baseURL, stop, err := portForward(ctx, c.RestConfig, namespace, podName, 9000)
	if err != nil {
		c.T.Fatalf("port-forward MinIO: %v", err)
	}
	defer stop()

	s3c := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(minioAccessKey, minioSecretKey, ""),
		BaseEndpoint: aws.String(baseURL),
		UsePathStyle: true,
	})
	out, err := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(minioBucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		c.T.Fatalf("list MinIO objects prefix=%q: %v", prefix, err)
	}
	return len(out.Contents)
}

// MinIOGetObject fetches one object (key relative to bucket) via port-forward.
func (c *Context) MinIOGetObject(ctx context.Context, namespace, key string) ([]byte, error) {
	c.T.Helper()
	var pods corev1.PodList
	if err := c.K8s.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app": "minio"}); err != nil {
		return nil, err
	}
	var podName string
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			podName = pods.Items[i].Name
			break
		}
	}
	if podName == "" {
		return nil, fmt.Errorf("no running MinIO Pod in %s", namespace)
	}
	baseURL, stop, err := portForward(ctx, c.RestConfig, namespace, podName, 9000)
	if err != nil {
		return nil, err
	}
	defer stop()

	s3c := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(minioAccessKey, minioSecretKey, ""),
		BaseEndpoint: aws.String(baseURL),
		UsePathStyle: true,
	})
	out, err := s3c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(minioBucket),
		Key:    aws.String(strings.TrimPrefix(key, "/")),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}
