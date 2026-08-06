package controller

import (
	"context"
	"fmt"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	secretAccessKeyID     = "accessKeyID"
	secretSecretAccessKey = "secretAccessKey"
)

// snapshotStorageConfig turns Pool.snapshotStorage + Secret into Worker storage credentials.
func snapshotStorageConfig(ctx context.Context, c client.Client, pool *sandboxv1alpha1.SandboxPool) (worker.ObjectStorageConfig, error) {
	if pool.Spec.SnapshotStorage == nil || pool.Spec.SnapshotStorage.S3 == nil {
		return worker.ObjectStorageConfig{}, fmt.Errorf("SandboxPool %q has no snapshotStorage.s3", pool.Name)
	}
	s3 := pool.Spec.SnapshotStorage.S3
	if s3.Bucket == "" || s3.CredentialsSecretRef.Name == "" {
		return worker.ObjectStorageConfig{}, fmt.Errorf("SandboxPool %q snapshotStorage.s3 needs bucket and credentialsSecretRef", pool.Name)
	}

	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: s3.CredentialsSecretRef.Name}, &secret); err != nil {
		return worker.ObjectStorageConfig{}, fmt.Errorf("get snapshot credentials secret %q: %w", s3.CredentialsSecretRef.Name, err)
	}
	accessKey := string(secret.Data[secretAccessKeyID])
	secretKey := string(secret.Data[secretSecretAccessKey])
	if accessKey == "" || secretKey == "" {
		return worker.ObjectStorageConfig{}, fmt.Errorf("secret %q must contain %s and %s", s3.CredentialsSecretRef.Name, secretAccessKeyID, secretSecretAccessKey)
	}

	return worker.ObjectStorageConfig{
		Endpoint:        s3.Endpoint,
		Bucket:          s3.Bucket,
		Region:          s3.Region,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		UsePathStyle:    s3.Endpoint != "",
	}, nil
}

func poolRuntime(pool *sandboxv1alpha1.SandboxPool) string {
	if pool.Spec.Runtime.CRI != nil {
		return pool.Spec.Runtime.CRI.RuntimeHandler
	}
	return ""
}
